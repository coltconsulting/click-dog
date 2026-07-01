package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// ObserverConfig configures the OTLP observer view of click-dog's in-memory
// self-metrics.
type ObserverConfig struct {
	// SinkNames maps Prometheus/raw exporter status names to stable OTLP sink
	// attribute values such as otel_0 or splunk_hec_0.
	SinkNames map[string]string
	// Rename is an optional canonical-key to emitted-name override.
	Rename map[string]string
}

type otlpInstruments struct {
	counters    map[string]otelmetric.Int64ObservableCounter
	intGauges   map[string]otelmetric.Int64ObservableGauge
	floatGauges map[string]otelmetric.Float64ObservableGauge
}

type otlpSnapshot struct {
	spansExported int64
	spansFiltered int64
	spansDupes    int64

	exportSinks map[string]exportSinkCounters

	cyclesSuccess int64
	cyclesError   int64
	cyclesSkipped int64

	circuitBreakerState string
	backoffIntervalSecs float64
	lastSuccessUnix     int64
	leader              int
	upSince             time.Time
	lastCycle           CycleSnapshot

	spanLogLastPollUnix      int64
	spanLogNewestRowTimeUnix int64
	spanLogRowsLastCycle     int64
	queryLogEnrichAttempts   int64
	queryLogEnrichSuccesses  int64
	queryLogEnrichFailures   int64
	queryLogEnrichMatchRatio float64
	spansWithQueryIDRatio    float64
	normalizedQuerySupported int
	topologyWarnings         map[string]bool
}

// RegisterObservers registers OTEL observable instruments that read the
// existing raw metric state. It is additive to the Prometheus endpoint: callers
// keep recording through the same Metrics methods. The observations are
// cumulative; the SDK's delta temporality selector converts them at export.
func (m *Metrics) RegisterObservers(meter otelmetric.Meter, cfg ObserverConfig) (otelmetric.Registration, error) {
	instruments, observables, err := makeOTLPInstruments(meter, cfg.Rename)
	if err != nil {
		return nil, err
	}

	return meter.RegisterCallback(func(ctx context.Context, observer otelmetric.Observer) error {
		_ = ctx
		m.observeOTLP(observer, instruments, cfg)
		return nil
	}, observables...)
}

func makeOTLPInstruments(meter otelmetric.Meter, rename map[string]string) (otlpInstruments, []otelmetric.Observable, error) {
	out := otlpInstruments{
		counters:    make(map[string]otelmetric.Int64ObservableCounter),
		intGauges:   make(map[string]otelmetric.Int64ObservableGauge),
		floatGauges: make(map[string]otelmetric.Float64ObservableGauge),
	}
	var observables []otelmetric.Observable

	for _, d := range CanonicalDescriptors() {
		name := OTLPName(d, rename)
		switch d.Kind {
		case MetricKindCounter:
			if d.ValueType != MetricValueTypeInt64 {
				return out, nil, fmt.Errorf("counter %s has unsupported value type %q", d.Key, d.ValueType)
			}
			inst, err := meter.Int64ObservableCounter(name, intCounterOptions(d)...)
			if err != nil {
				return out, nil, fmt.Errorf("create OTLP counter %s: %w", d.Key, err)
			}
			out.counters[d.Key] = inst
			observables = append(observables, inst)
		case MetricKindGauge:
			switch d.ValueType {
			case MetricValueTypeFloat64:
				inst, err := meter.Float64ObservableGauge(name, floatGaugeOptions(d)...)
				if err != nil {
					return out, nil, fmt.Errorf("create OTLP gauge %s: %w", d.Key, err)
				}
				out.floatGauges[d.Key] = inst
				observables = append(observables, inst)
			case MetricValueTypeInt64:
				inst, err := meter.Int64ObservableGauge(name, intGaugeOptions(d)...)
				if err != nil {
					return out, nil, fmt.Errorf("create OTLP gauge %s: %w", d.Key, err)
				}
				out.intGauges[d.Key] = inst
				observables = append(observables, inst)
			default:
				return out, nil, fmt.Errorf("gauge %s has unsupported value type %q", d.Key, d.ValueType)
			}
		default:
			return out, nil, fmt.Errorf("unknown metric kind %q for %s", d.Kind, d.Key)
		}
	}

	return out, observables, nil
}

func intCounterOptions(d MetricDescriptor) []otelmetric.Int64ObservableCounterOption {
	opts := []otelmetric.Int64ObservableCounterOption{otelmetric.WithDescription(d.Help)}
	if d.Unit != "" {
		opts = append(opts, otelmetric.WithUnit(d.Unit))
	}
	return opts
}

func intGaugeOptions(d MetricDescriptor) []otelmetric.Int64ObservableGaugeOption {
	opts := []otelmetric.Int64ObservableGaugeOption{otelmetric.WithDescription(d.Help)}
	if d.Unit != "" {
		opts = append(opts, otelmetric.WithUnit(d.Unit))
	}
	return opts
}

func floatGaugeOptions(d MetricDescriptor) []otelmetric.Float64ObservableGaugeOption {
	opts := []otelmetric.Float64ObservableGaugeOption{otelmetric.WithDescription(d.Help)}
	if d.Unit != "" {
		opts = append(opts, otelmetric.WithUnit(d.Unit))
	}
	return opts
}

func (m *Metrics) observeOTLP(observer otelmetric.Observer, instruments otlpInstruments, cfg ObserverConfig) {
	s := m.otlpSnapshot()
	now := time.Now()

	observer.ObserveInt64(instruments.counters[MetricSpansExported], s.spansExported)
	observer.ObserveInt64(instruments.counters[MetricSpansFiltered], s.spansFiltered)
	observer.ObserveInt64(instruments.counters[MetricSpansDuplicates], s.spansDupes)

	for _, sink := range sortedExportSinkNames(s.exportSinks) {
		attrs := otelmetric.WithAttributes(attribute.String("sink", normalizeOTLPSinkName(sink, cfg.SinkNames)))
		c := s.exportSinks[sink]
		observer.ObserveInt64(instruments.counters[MetricExportAttempts], c.sent, attrs)
		observer.ObserveInt64(instruments.counters[MetricExportAccepted], c.accepted, attrs)
		observer.ObserveInt64(instruments.counters[MetricExportErrors], c.errors, attrs)
	}

	observer.ObserveInt64(instruments.counters[MetricCycleResults], s.cyclesSuccess, otelmetric.WithAttributes(attribute.String("result", string(cycleResultSuccess))))
	observer.ObserveInt64(instruments.counters[MetricCycleResults], s.cyclesError, otelmetric.WithAttributes(attribute.String("result", string(cycleResultError))))
	observer.ObserveInt64(instruments.counters[MetricCycleResults], s.cyclesSkipped, otelmetric.WithAttributes(attribute.String("result", string(cycleResultSkipped))))

	observer.ObserveInt64(instruments.intGauges[MetricCircuitBreakerState], int64(circuitBreakerStateValue(s.circuitBreakerState)))
	observer.ObserveInt64(instruments.intGauges[MetricLeader], int64(s.leader))
	for _, reason := range KnownTopologyReasons {
		v := int64(0)
		if s.topologyWarnings[reason] {
			v = 1
		}
		observer.ObserveInt64(instruments.intGauges[MetricTopologyWarning], v, otelmetric.WithAttributes(attribute.String("reason", reason)))
	}

	observer.ObserveFloat64(instruments.floatGauges[MetricBackoffIntervalSeconds], s.backoffIntervalSecs)
	// Unlike Prometheus compatibility output, OTLP intentionally omits this
	// point until a success exists so fresh starts do not look like 1970.
	if s.lastSuccessUnix > 0 {
		role := "active"
		if s.leader == 0 {
			role = "standby"
		}
		observer.ObserveInt64(
			instruments.intGauges[MetricLastSuccessTimestampSeconds],
			s.lastSuccessUnix,
			otelmetric.WithAttributes(attribute.String("role", role)),
		)
	}
	observer.ObserveFloat64(instruments.floatGauges[MetricUptimeSeconds], now.Sub(s.upSince).Seconds())
	observer.ObserveFloat64(instruments.floatGauges[MetricLastCycleDurationSeconds], float64(s.lastCycle.DurationMs)/1000.0)
	observer.ObserveInt64(instruments.intGauges[MetricLastCycleExportedSpans], int64(s.lastCycle.Exported))
	observer.ObserveInt64(instruments.intGauges[MetricLastCycleFilteredSpans], int64(s.lastCycle.Filtered))
	observer.ObserveInt64(instruments.intGauges[MetricLastCycleDuplicateSpans], int64(s.lastCycle.Duplicates))

	observer.ObserveInt64(instruments.intGauges[MetricSpanLogLastPollTimestamp], s.spanLogLastPollUnix)
	observer.ObserveFloat64(instruments.floatGauges[MetricSpanLogNewestRowAgeSeconds], newestRowAgeSeconds(s.spanLogNewestRowTimeUnix, now))
	observer.ObserveInt64(instruments.intGauges[MetricSpanLogRowsLastCycle], s.spanLogRowsLastCycle)

	observer.ObserveInt64(instruments.counters[MetricQueryLogEnrichmentAttempts], s.queryLogEnrichAttempts)
	observer.ObserveInt64(instruments.counters[MetricQueryLogEnrichmentSuccesses], s.queryLogEnrichSuccesses)
	observer.ObserveInt64(instruments.counters[MetricQueryLogEnrichmentFailures], s.queryLogEnrichFailures)
	observer.ObserveFloat64(instruments.floatGauges[MetricQueryLogEnrichmentMatchRatio], s.queryLogEnrichMatchRatio)
	observer.ObserveFloat64(instruments.floatGauges[MetricSpansWithQueryIDRatio], s.spansWithQueryIDRatio)
	observer.ObserveInt64(instruments.intGauges[MetricNormalizedQuerySupported], int64(s.normalizedQuerySupported))
}

func (m *Metrics) otlpSnapshot() otlpSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	exportSinks := make(map[string]exportSinkCounters, len(m.exportSinks))
	for k, v := range m.exportSinks {
		exportSinks[k] = v
	}
	topologyWarnings := make(map[string]bool, len(m.topologyWarnings))
	for k, v := range m.topologyWarnings {
		topologyWarnings[k] = v
	}

	return otlpSnapshot{
		spansExported:            m.spansExported,
		spansFiltered:            m.spansFiltered,
		spansDupes:               m.spansDupes,
		exportSinks:              exportSinks,
		cyclesSuccess:            m.cyclesSuccess,
		cyclesError:              m.cyclesError,
		cyclesSkipped:            m.cyclesSkipped,
		circuitBreakerState:      m.circuitBreakerState,
		backoffIntervalSecs:      m.backoffIntervalSecs,
		lastSuccessUnix:          m.lastSuccessUnix,
		leader:                   m.leader,
		upSince:                  m.upSince,
		lastCycle:                m.lastCycle,
		spanLogLastPollUnix:      m.spanLogLastPollUnix,
		spanLogNewestRowTimeUnix: m.spanLogNewestRowTimeUnix,
		spanLogRowsLastCycle:     m.spanLogRowsLastCycle,
		queryLogEnrichAttempts:   m.queryLogEnrichAttempts,
		queryLogEnrichSuccesses:  m.queryLogEnrichSuccesses,
		queryLogEnrichFailures:   m.queryLogEnrichFailures,
		queryLogEnrichMatchRatio: m.queryLogEnrichMatchRatio,
		spansWithQueryIDRatio:    m.spansWithQueryIDRatio,
		normalizedQuerySupported: m.normalizedQuerySupported,
		topologyWarnings:         topologyWarnings,
	}
}

func normalizeOTLPSinkName(raw string, configured map[string]string) string {
	if configured != nil {
		if name := configured[raw]; name != "" {
			return name
		}
	}
	if raw == "dry_run" {
		return raw
	}
	return NormalizeExportSinkName(raw)
}

func circuitBreakerStateValue(state string) int {
	switch state {
	case "half_open", "half-open":
		return 1
	case "open":
		return 2
	default:
		return 0
	}
}

func newestRowAgeSeconds(newestUnix int64, now time.Time) float64 {
	if newestUnix <= 0 {
		return 0
	}
	age := float64(now.Unix() - newestUnix)
	if age < 0 {
		return 0
	}
	return age
}
