package metrics

import (
	"fmt"
	"strings"
)

// MetricKind is the canonical instrument kind for a click-dog self-metric.
type MetricKind string

const (
	MetricKindCounter MetricKind = "counter"
	MetricKindGauge   MetricKind = "gauge"
)

// MetricValueType is the OTLP number representation used by a descriptor.
type MetricValueType string

const (
	MetricValueTypeInt64   MetricValueType = "int64"
	MetricValueTypeFloat64 MetricValueType = "float64"
)

// MetricDescriptor defines one canonical click-dog self-metric. The Key is the
// stable snake_case name without the click_dog prefix or Prometheus _total
// suffix.
type MetricDescriptor struct {
	Key       string
	Kind      MetricKind
	ValueType MetricValueType
	Unit      string
	Labels    []string
	Help      string
}

const (
	MetricSpansExported                = "spans_exported"
	MetricSpansFiltered                = "spans_filtered"
	MetricSpansDuplicates              = "spans_duplicates"
	MetricExportAttempts               = "export_attempts"
	MetricExportAccepted               = "export_accepted"
	MetricExportErrors                 = "export_errors"
	MetricCycleResults                 = "cycle_results"
	MetricCircuitBreakerState          = "circuit_breaker_state"
	MetricLeader                       = "leader"
	MetricTopologyWarning              = "topology_warning"
	MetricBackoffIntervalSeconds       = "backoff_interval_seconds"
	MetricLastSuccessTimestampSeconds  = "last_success_timestamp_seconds"
	MetricUptimeSeconds                = "uptime_seconds"
	MetricLastCycleDurationSeconds     = "last_cycle_duration_seconds"
	MetricLastCycleExportedSpans       = "last_cycle_exported_spans"
	MetricLastCycleFilteredSpans       = "last_cycle_filtered_spans"
	MetricLastCycleDuplicateSpans      = "last_cycle_duplicate_spans"
	MetricSpanLogLastPollTimestamp     = "span_log_last_poll_timestamp_seconds"
	MetricSpanLogNewestRowAgeSeconds   = "span_log_newest_row_age_seconds"
	MetricSpanLogRowsLastCycle         = "span_log_rows_last_cycle"
	MetricQueryLogEnrichmentAttempts   = "query_log_enrichment_attempts"
	MetricQueryLogEnrichmentSuccesses  = "query_log_enrichment_successes"
	MetricQueryLogEnrichmentFailures   = "query_log_enrichment_failures"
	MetricQueryLogEnrichmentMatchRatio = "query_log_enrichment_match_ratio"
	MetricSpansWithQueryIDRatio        = "spans_with_query_id_ratio"
	MetricNormalizedQuerySupported     = "normalized_query_supported"
)

var metricDescriptors = []MetricDescriptor{
	{Key: MetricSpansExported, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Help: "Total spans exported to backends."},
	{Key: MetricSpansFiltered, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Help: "Total spans filtered out."},
	{Key: MetricSpansDuplicates, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Help: "Total duplicate spans skipped."},
	{Key: MetricExportAttempts, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Labels: []string{"sink"}, Help: "Total export items submitted by sink, including retries."},
	{Key: MetricExportAccepted, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Labels: []string{"sink"}, Help: "Total export items accepted by sink."},
	{Key: MetricExportErrors, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Labels: []string{"sink"}, Help: "Total export attempts that returned an error by sink."},
	{Key: MetricCycleResults, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Labels: []string{"result"}, Help: "Total processing cycles by outcome (success, error, skipped)."},
	{Key: MetricCircuitBreakerState, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Help: "Circuit breaker state (0=closed, 1=half_open, 2=open)."},
	{Key: MetricLeader, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Help: "Whether this instance is the active exporter/leader (1) or a leader-gated standby (0)."},
	{Key: MetricTopologyWarning, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Labels: []string{"reason"}, Help: "Topology self-audit warning (1=detected, 0=clean) by reason: sidecar_cluster_queries (per-node sidecars each going cluster-wide) | multi_instance_cluster_queries (separate readers not sharing an election)."},
	{Key: MetricBackoffIntervalSeconds, Kind: MetricKindGauge, ValueType: MetricValueTypeFloat64, Unit: "s", Help: "Current polling interval in seconds."},
	{Key: MetricLastSuccessTimestampSeconds, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Unit: "s", Help: "Unix timestamp of last successful cycle. The bare series is retained for compatibility; role labels identify active exporters vs leader-gated standbys."},
	{Key: MetricUptimeSeconds, Kind: MetricKindGauge, ValueType: MetricValueTypeFloat64, Unit: "s", Help: "Seconds since click-dog started."},
	{Key: MetricLastCycleDurationSeconds, Kind: MetricKindGauge, ValueType: MetricValueTypeFloat64, Unit: "s", Help: "Duration of the most recent processing cycle in seconds."},
	{Key: MetricLastCycleExportedSpans, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Help: "Spans exported in the most recent cycle."},
	{Key: MetricLastCycleFilteredSpans, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Help: "Spans filtered in the most recent cycle."},
	{Key: MetricLastCycleDuplicateSpans, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Help: "Duplicate spans skipped in the most recent cycle."},
	{Key: MetricSpanLogLastPollTimestamp, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Unit: "s", Help: "Unix timestamp of the last span-log fetch that completed without error (zero rows still counts)."},
	{Key: MetricSpanLogNewestRowAgeSeconds, Kind: MetricKindGauge, ValueType: MetricValueTypeFloat64, Unit: "s", Help: "Age in seconds of the newest span-log row observed in the most recent non-empty fetch. 0 is ambiguous (no observation yet OR age rounded below 1s); alert on > threshold."},
	{Key: MetricSpanLogRowsLastCycle, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Help: "Raw row count returned by the last span-log fetch, before filter/dedup."},
	{Key: MetricQueryLogEnrichmentAttempts, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Help: "Total cycles that attempted query_log enrichment (had at least one query_id to look up)."},
	{Key: MetricQueryLogEnrichmentSuccesses, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Help: "Total enrichment attempts that returned without error."},
	{Key: MetricQueryLogEnrichmentFailures, Kind: MetricKindCounter, ValueType: MetricValueTypeInt64, Help: "Total enrichment attempts that returned an error."},
	{Key: MetricQueryLogEnrichmentMatchRatio, Kind: MetricKindGauge, ValueType: MetricValueTypeFloat64, Help: "Last cycle's match ratio (matched query_ids / requested query_ids); 0 before first successful enrichment."},
	{Key: MetricSpansWithQueryIDRatio, Kind: MetricKindGauge, ValueType: MetricValueTypeFloat64, Help: "Last cycle's ratio of fetched spans carrying clickhouse.query_id (0 before first observation)."},
	{Key: MetricNormalizedQuerySupported, Kind: MetricKindGauge, ValueType: MetricValueTypeInt64, Help: "Whether system.query_log.normalized_query_hash is available and in use (1=yes, 0=no). In cluster query mode this requires all cluster replicas to support it. Set once at startup."},
}

var descriptorByKey = func() map[string]MetricDescriptor {
	out := make(map[string]MetricDescriptor, len(metricDescriptors))
	for _, d := range metricDescriptors {
		out[d.Key] = d
	}
	return out
}()

// CanonicalDescriptors returns the self-metric registry in stable render order.
func CanonicalDescriptors() []MetricDescriptor {
	out := make([]MetricDescriptor, len(metricDescriptors))
	copy(out, metricDescriptors)
	return out
}

func mustDescriptor(key string) MetricDescriptor {
	d, ok := descriptorByKey[key]
	if !ok {
		panic(fmt.Sprintf("unknown metric descriptor %q", key))
	}
	return d
}

// PrometheusName renders a descriptor with the Prometheus profile.
func PrometheusName(d MetricDescriptor) string {
	name := "click_dog_" + d.Key
	if d.Kind == MetricKindCounter {
		name += "_total"
	}
	return name
}

// OTLPName renders a descriptor with the default OTLP profile plus optional
// per-metric rename overrides.
func OTLPName(d MetricDescriptor, rename map[string]string) string {
	if rename != nil {
		if override := strings.TrimSpace(rename[d.Key]); override != "" {
			return override
		}
	}
	return "click_dog." + d.Key
}
