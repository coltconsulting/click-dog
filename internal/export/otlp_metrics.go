package export

import (
	"context"
	"errors"
	"fmt"
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
		logAndDropMetricExporter{Exporter: metricExp},
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

type logAndDropMetricExporter struct {
	sdkmetric.Exporter
}

func (e logAndDropMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if err := e.Exporter.Export(ctx, rm); err != nil {
		clicklog.Error("OTLP self-metrics export failed; dropping batch: %v", err)
	}
	return nil
}
