package export

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/grpc"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	clickmetrics "github.com/coltconsulting/click-dog/internal/metrics"
)

// OTLPMetricsOptions configures the OTLP self-metrics exporter.
type OTLPMetricsOptions struct {
	Metrics        *clickmetrics.Metrics
	Conn           *grpc.ClientConn
	Interval       time.Duration
	ExportTimeout  time.Duration
	Host           string
	ServiceName    string
	ServiceVersion string
	SinkNames      map[string]string
	Rename         map[string]string
}

// OTLPMetricsExporter owns the OTEL MeterProvider for click-dog self-metrics.
type OTLPMetricsExporter struct {
	provider *sdkmetric.MeterProvider
}

func NewOTLPMetricsExporter(ctx context.Context, opts OTLPMetricsOptions) (*OTLPMetricsExporter, error) {
	if opts.Metrics == nil {
		return nil, errors.New("OTLP metrics exporter requires Metrics")
	}
	if opts.Conn == nil {
		return nil, errors.New("OTLP metrics exporter requires gRPC connection")
	}
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("OTLP metrics interval must be > 0, got %s", opts.Interval)
	}
	if opts.ExportTimeout <= 0 {
		opts.ExportTimeout = 10 * time.Second
	}

	metricExp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithGRPCConn(opts.Conn),
		otlpmetricgrpc.WithTemporalitySelector(sdkmetric.DeltaTemporalitySelector),
		otlpmetricgrpc.WithTimeout(opts.ExportTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metrics exporter: %w", err)
	}

	reader := sdkmetric.NewPeriodicReader(
		&logAndDropMetricExporter{Exporter: metricExp},
		sdkmetric.WithInterval(opts.Interval),
		sdkmetric.WithTimeout(opts.ExportTimeout),
	)
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(resource.NewSchemaless(
			attribute.String("service.name", opts.ServiceName),
			attribute.String("host.name", opts.Host),
			attribute.String("service.version", opts.ServiceVersion),
		)),
	)

	if _, err := opts.Metrics.RegisterObservers(provider.Meter("github.com/coltconsulting/click-dog/self-metrics"), clickmetrics.ObserverConfig{
		SinkNames: opts.SinkNames,
		Rename:    opts.Rename,
	}); err != nil {
		_ = provider.Shutdown(ctx)
		return nil, fmt.Errorf("register OTLP self-metrics observers: %w", err)
	}

	// The caller (main) logs the operator-facing startup line via
	// selfMetricsStatusLine — a superset of interval/service/host that also
	// carries the resolved endpoint and connection source — so this no longer
	// logs on success to avoid a redundant back-to-back Info at startup.
	return &OTLPMetricsExporter{provider: provider}, nil
}

// Shutdown flushes and stops the metrics MeterProvider.
func (o *OTLPMetricsExporter) Shutdown(ctx context.Context) error {
	if o == nil || o.provider == nil {
		return nil
	}
	return o.provider.Shutdown(ctx)
}

// logAndDropMetricExporter bounds the log volume of a self-metrics outage: the
// usual failure never resolves on its own, and at a 10s push interval logging
// every batch buries real errors. Distinct errors and recovery log at once; an
// unchanged error repeats at most once per selfMetricsErrorRepeat.
type logAndDropMetricExporter struct {
	sdkmetric.Exporter

	mu         sync.Mutex
	lastErr    string
	lastLogged time.Time
	// Two counters because they answer different questions: dropped is the
	// whole outage (reported on recovery), sinceLogged only what the last
	// message did not already account for (reported on a repeat).
	dropped     int
	sinceLogged int
}

// selfMetricsErrorRepeat bounds how often an unchanged self-metrics error
// repeats. Long enough that a persistent misconfiguration costs a handful of
// lines an hour, short enough that an operator tailing the log still sees it.
const selfMetricsErrorRepeat = 5 * time.Minute

func (e *logAndDropMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)

	e.mu.Lock()
	defer e.mu.Unlock()

	if err == nil {
		if e.lastErr != "" {
			clicklog.Info("OTLP self-metrics export recovered after %d dropped batch(es)", e.dropped)
			e.lastErr = ""
			e.dropped = 0
			e.sinceLogged = 0
		}
		return nil
	}

	msg := err.Error()
	now := time.Now()
	e.dropped++
	if msg != e.lastErr {
		clicklog.Error("OTLP self-metrics export failed; dropping batch: %v", err)
		e.lastErr = msg
		e.lastLogged = now
		e.sinceLogged = 0
		return nil
	}

	e.sinceLogged++
	if now.Sub(e.lastLogged) >= selfMetricsErrorRepeat {
		clicklog.Error("OTLP self-metrics export still failing; %d batch(es) dropped since last message: %v", e.sinceLogged, err)
		e.lastLogged = now
		e.sinceLogged = 0
	}
	return nil
}
