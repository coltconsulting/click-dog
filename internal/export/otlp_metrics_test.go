package export

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type failingMetricExporter struct {
	err error
}

func (f failingMetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DeltaTemporalitySelector(k)
}

func (f failingMetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

func (f failingMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return f.err
}

func (f failingMetricExporter) ForceFlush(context.Context) error {
	return nil
}

func (f failingMetricExporter) Shutdown(context.Context) error {
	return nil
}

func TestOTLPMetricsExporter_ExportFailureIsDropped(t *testing.T) {
	wrapped := logAndDropMetricExporter{Exporter: failingMetricExporter{err: errors.New("backend down")}}
	if err := wrapped.Export(context.Background(), &metricdata.ResourceMetrics{}); err != nil {
		t.Fatalf("Export returned %v, want nil", err)
	}
}
