package export

import (
	"context"
	"errors"
	"fmt"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/model"
)

// Compile-time check that *MultiExporter satisfies model.SpanExporter.
var _ model.SpanExporter = (*MultiExporter)(nil)

// MultiExporterPolicy controls how MultiExporter interprets live span fan-out
// results when some sinks fail or accept different span subsets.
type MultiExporterPolicy int

const (
	// PolicyAllRequired preserves the strict default behavior: every sink must
	// accept a span before MultiExporter reports it as accepted, and any sink
	// error makes the whole span export attempt fail.
	PolicyAllRequired MultiExporterPolicy = iota

	// PolicyAnySuccess reports spans accepted by any successful sink and only
	// fails the span export attempt when no sink accepted any spans.
	PolicyAnySuccess
)

// MultiExporterOption configures a MultiExporter.
type MultiExporterOption func(*MultiExporter)

// WithMultiExporterPolicy sets the live span fan-out policy. ExportQuery stays
// all-required regardless of this option because backfill correctness depends
// on every configured sink receiving each query row.
func WithMultiExporterPolicy(policy MultiExporterPolicy) MultiExporterOption {
	return func(m *MultiExporter) {
		m.spanPolicy = policy
	}
}

func (p MultiExporterPolicy) valid() bool {
	return p == PolicyAllRequired || p == PolicyAnySuccess
}

// MultiExporter fans out exports to multiple SpanExporter backends.
//
// Delivery contract: PolicyAllRequired is the default. Under that policy, any
// sink failure makes ExportSpans return a non-nil error joining the per-sink
// failures so the caller (processor, metrics, /readyz) treats the cycle as a
// real export error — feeding the circuit breaker, adaptive backoff, error
// counters, and last-cycle status. ExportQuery always uses all-required
// semantics for backfill correctness. The returned ExportResult still carries
// per-sink counts and errors for observability. On retry the batch is
// re-delivered to every sink, so sinks that previously succeeded will see the
// same span keys again. That is accepted by design: OTEL collectors and trace
// backends can deduplicate by trace_id/span_id, Splunk HEC receives stable IDs
// for downstream dedup/search, and at-least-once delivery is preferred to
// silently dropping data from a partially-down fan-out.
type MultiExporter struct {
	exporters  []model.SpanExporter
	names      []string
	spanPolicy MultiExporterPolicy
}

// NewMultiExporter creates a MultiExporter wrapping the given exporters.
// names is used for log messages and error context. If names is shorter than
// exporters, fallback labels are generated; extra names are ignored.
func NewMultiExporter(exporters []model.SpanExporter, names []string, opts ...MultiExporterOption) *MultiExporter {
	if len(names) != len(exporters) {
		clicklog.Warn("MultiExporter: got %d exporter names for %d exporters; using fallback names for missing entries and ignoring extras",
			len(names), len(exporters))
	}
	m := &MultiExporter{
		exporters:  append([]model.SpanExporter(nil), exporters...),
		names:      normalizeExporterNames(len(exporters), names),
		spanPolicy: PolicyAllRequired,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	if !m.spanPolicy.valid() {
		clicklog.Warn("MultiExporter: invalid span policy %d; using PolicyAllRequired", m.spanPolicy)
		m.spanPolicy = PolicyAllRequired
	}
	return m
}

func normalizeExporterNames(exporterCount int, names []string) []string {
	normalized := make([]string, exporterCount)
	copy(normalized, names)
	for i := len(names); i < exporterCount; i++ {
		normalized[i] = fmt.Sprintf("exporter_%d", i+1)
	}
	return normalized
}

// ExportSpans calls ExportSpans on every backend sequentially.
//
// Under PolicyAllRequired, on full success, returns the intersection of
// (trace_id, span_id) keys each sink reported as exported (a span is "seen"
// only after every sink accepted it).
//
// Under PolicyAnySuccess, returns the union of keys accepted by successful
// sinks. Sink failures still appear in ExportResult.Sinks but do not make the
// top-level call fail unless no sink accepted any spans.
func (m *MultiExporter) ExportSpans(ctx context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	if len(spans) == 0 || len(m.exporters) == 0 {
		return model.ExportResult{}, nil
	}

	var sinkErrs []error
	statuses := make([]model.ExportSinkStatus, 0, len(m.exporters))
	// intersection starts nil to distinguish "no successful sink yet"
	// from "all sinks accepted an empty set".
	var intersection map[model.SpanKey]struct{}
	var union map[model.SpanKey]struct{}
	if m.spanPolicy == PolicyAnySuccess {
		union = make(map[model.SpanKey]struct{})
	}

	for i, exp := range m.exporters {
		result, err := exp.ExportSpans(ctx, spans)
		// MultiExporter reports one top-level status per configured sink name.
		// Inner result.Sinks are intentionally not merged so fan-out metrics/logs
		// remain keyed to the configured sink labels.
		statuses = append(statuses, model.ExportSinkStatus{
			Name:     m.names[i],
			Sent:     len(spans),
			Accepted: result.TotalAccepted,
			Error:    err,
		})
		if err != nil {
			clicklog.Error("MultiExporter: %s ExportSpans failed: %v", m.names[i], err)
			sinkErrs = append(sinkErrs, fmt.Errorf("%s: %w", m.names[i], err))
			continue
		}

		keySet := make(map[model.SpanKey]struct{}, len(result.Accepted))
		for _, k := range result.Accepted {
			keySet[k] = struct{}{}
			if union != nil {
				union[k] = struct{}{}
			}
		}

		if intersection == nil {
			intersection = keySet
		} else {
			for k := range intersection {
				if _, ok := keySet[k]; !ok {
					delete(intersection, k)
				}
			}
		}
	}

	if m.spanPolicy == PolicyAnySuccess {
		accepted := spanKeysFromSet(union)
		if len(sinkErrs) > 0 && len(accepted) == 0 {
			err := fmt.Errorf("multi-exporter span export failed (%d/%d sinks): %w",
				len(sinkErrs), len(m.exporters), errors.Join(sinkErrs...))
			return model.ExportResult{
				TotalSent: len(spans),
				Sinks:     statuses,
			}, err
		}
		return model.ExportResult{
			Accepted:      accepted,
			TotalSent:     len(spans),
			TotalAccepted: len(accepted),
			Sinks:         statuses,
		}, nil
	}

	if len(sinkErrs) > 0 {
		// Return no accepted keys so the caller cannot accidentally mark the
		// intersection of successful sinks as "seen" — under the
		// all-required contract a partial failure must trigger a full
		// retry on the next cycle.
		err := fmt.Errorf("multi-exporter span export failed (%d/%d sinks): %w",
			len(sinkErrs), len(m.exporters), errors.Join(sinkErrs...))
		return model.ExportResult{
			TotalSent: len(spans),
			Sinks:     statuses,
		}, err
	}

	result := spanKeysFromSet(intersection)
	return model.ExportResult{
		Accepted:      result,
		TotalSent:     len(spans),
		TotalAccepted: len(result),
		Sinks:         statuses,
	}, nil
}

func spanKeysFromSet(set map[model.SpanKey]struct{}) []model.SpanKey {
	keys := make([]model.SpanKey, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	return keys
}

// ExportQuery calls ExportQuery on every backend.
//
// All sinks are required: if any sink fails, returns a non-nil error
// joining the per-sink failures. Backfill mode counts the query as
// failed (BatchResult.Failed++) and the run exits non-zero — even
// though some sinks may have already accepted the row. Re-running the
// backfill window re-delivers to every sink; this is dedup-safe because each
// row maps to a stable identity (Splunk emits query_id directly; the OTEL sink
// derives a deterministic trace_id/span_id from query_id + event_time).
func (m *MultiExporter) ExportQuery(ctx context.Context, log model.QueryLog) (model.ExportResult, error) {
	if len(m.exporters) == 0 {
		return model.ExportResult{}, nil
	}

	var sinkErrs []error
	statuses := make([]model.ExportSinkStatus, 0, len(m.exporters))

	for i, exp := range m.exporters {
		result, err := exp.ExportQuery(ctx, log)
		// MultiExporter reports one top-level status per configured sink name.
		// Inner result.Sinks are intentionally not merged so fan-out metrics/logs
		// remain keyed to the configured sink labels.
		statuses = append(statuses, model.ExportSinkStatus{
			Name:     m.names[i],
			Sent:     1,
			Accepted: result.TotalAccepted,
			Error:    err,
		})
		if err != nil {
			clicklog.Error("MultiExporter: %s ExportQuery failed: %v", m.names[i], err)
			sinkErrs = append(sinkErrs, fmt.Errorf("%s: %w", m.names[i], err))
		}
	}

	if len(sinkErrs) > 0 {
		err := fmt.Errorf("multi-exporter query export failed (%d/%d sinks): %w",
			len(sinkErrs), len(m.exporters), errors.Join(sinkErrs...))
		return model.ExportResult{
			TotalSent: 1,
			Sinks:     statuses,
		}, err
	}
	return model.ExportResult{
		TotalSent:     1,
		TotalAccepted: 1,
		Sinks:         statuses,
	}, nil
}

// Close calls Close on every backend and collects errors.
func (m *MultiExporter) Close(ctx context.Context) error {
	var sinkErrs []error

	for i, exp := range m.exporters {
		if err := exp.Close(ctx); err != nil {
			sinkErrs = append(sinkErrs, fmt.Errorf("%s: %w", m.names[i], err))
		}
	}

	if len(sinkErrs) > 0 {
		return fmt.Errorf("errors closing exporters: %w", errors.Join(sinkErrs...))
	}
	return nil
}
