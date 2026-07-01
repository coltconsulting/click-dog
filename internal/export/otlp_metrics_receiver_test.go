package export

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type recordingMetricsReceiver struct {
	colmetricpb.UnimplementedMetricsServiceServer
	requests []*colmetricpb.ExportMetricsServiceRequest
}

func (r *recordingMetricsReceiver) Export(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	r.requests = append(r.requests, req)
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

func TestOTLPMetricsExporter_ExportsCanonicalMetrics(t *testing.T) {
	tests := []struct {
		name      string
		rawSink   string
		sinkNames map[string]string
		wantSink  string
	}{
		{
			name:      "single otel sink",
			rawSink:   "otel",
			sinkNames: map[string]string{"otel": "otel_0"},
			wantSink:  "otel_0",
		},
		{
			name:      "multi exporter raw sink",
			rawSink:   "otel[0]:localhost:4317",
			sinkNames: map[string]string{"otel[0]:localhost:4317": "otel_0"},
			wantSink:  "otel_0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receiver, conn, cleanup := startMetricsReceiver(t)
			defer cleanup()

			m := metrics.NewMetrics()
			m.RecordCycle(5, 2, 1, 125, nil)
			m.RecordSkippedCycle(7)
			m.RecordExportResult(model.ExportResult{Sinks: []model.ExportSinkStatus{{
				Name:     tt.rawSink,
				Sent:     5,
				Accepted: 5,
			}}})
			m.SetBackoffInterval(30 * time.Second)
			m.SetTopologyWarning(metrics.TopologyReasonSidecar, true)
			m.RecordSpanLogPoll(8, time.Now().Add(-10*time.Second))
			m.RecordQueryLogEnrichmentCycle(3, 6, nil)
			m.RecordSpansWithQueryIDRatio(4, 8)
			m.SetNormalizedQuerySupported(true)

			exp, err := NewOTLPMetricsExporter(context.Background(), OTLPMetricsOptions{
				Metrics:        m,
				Conn:           conn,
				Interval:       time.Hour,
				ExportTimeout:  5 * time.Second,
				Host:           "host-a",
				ServiceName:    "svc-a",
				ServiceVersion: "v.test",
				SinkNames:      tt.sinkNames,
			})
			if err != nil {
				t.Fatalf("NewOTLPMetricsExporter: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := exp.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}

			if len(receiver.requests) != 1 {
				t.Fatalf("receiver got %d requests, want 1", len(receiver.requests))
			}
			req := receiver.requests[0]
			resourceAttrs := resourceAttrs(req)
			for key, want := range map[string]string{
				"host.name":       "host-a",
				"service.name":    "svc-a",
				"service.version": "v.test",
			} {
				if got := resourceAttrs[key]; got != want {
					t.Fatalf("resource attr %s = %q, want %q", key, got, want)
				}
			}

			byName := receiverMetricMap(req)
			for _, d := range metrics.CanonicalDescriptors() {
				name := metrics.OTLPName(d, nil)
				metric, ok := byName[name]
				if !ok {
					t.Fatalf("missing emitted metric %s", name)
				}
				assertOTLPMetricShape(t, metric, d)
			}

			for _, metricKey := range []string{
				metrics.MetricExportAttempts,
				metrics.MetricExportAccepted,
				metrics.MetricExportErrors,
			} {
				points := byName["click_dog."+metricKey].GetSum().GetDataPoints()
				got, ok := intPointValueOK(points, map[string]string{"sink": tt.wantSink})
				if !ok {
					t.Fatalf("missing %s{sink=%s} point", metricKey, tt.wantSink)
				}
				if got == 0 && metricKey != metrics.MetricExportErrors {
					t.Fatalf("%s{sink=%s} = %d, want non-zero point", metricKey, tt.wantSink, got)
				}
			}

			cyclePoints := byName["click_dog."+metrics.MetricCycleResults].GetSum().GetDataPoints()
			for result, want := range map[string]int64{"success": 1, "error": 0, "skipped": 1} {
				got, ok := intPointValueOK(cyclePoints, map[string]string{"result": result})
				if !ok {
					t.Fatalf("missing cycle_results{%s} point", result)
				}
				if got != want {
					t.Fatalf("cycle_results{%s} = %d, want %d", result, got, want)
				}
			}

			topologyPoints := byName["click_dog."+metrics.MetricTopologyWarning].GetGauge().GetDataPoints()
			for reason, want := range map[string]int64{metrics.TopologyReasonSidecar: 1, metrics.TopologyReasonMultiInstance: 0} {
				got, ok := intPointValueOK(topologyPoints, map[string]string{"reason": reason})
				if !ok {
					t.Fatalf("missing topology_warning{%s} point", reason)
				}
				if got != want {
					t.Fatalf("topology_warning{%s} = %d, want %d", reason, got, want)
				}
			}
		})
	}
}

func assertOTLPMetricShape(t *testing.T, metric *metricspb.Metric, d metrics.MetricDescriptor) {
	t.Helper()
	if got := metric.GetUnit(); got != d.Unit {
		t.Fatalf("%s unit = %q, want %q", d.Key, got, d.Unit)
	}
	switch d.Kind {
	case metrics.MetricKindCounter:
		sum := metric.GetSum()
		if sum == nil {
			t.Fatalf("%s emitted as %T, want sum", d.Key, metric.GetData())
		}
		if sum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
			t.Fatalf("%s temporality = %v, want delta", d.Key, sum.GetAggregationTemporality())
		}
		if !sum.GetIsMonotonic() {
			t.Fatalf("%s sum is not monotonic", d.Key)
		}
	case metrics.MetricKindGauge:
		if metric.GetGauge() == nil {
			t.Fatalf("%s emitted as %T, want gauge", d.Key, metric.GetData())
		}
	default:
		t.Fatalf("unknown metric kind %q for %s", d.Kind, d.Key)
	}
}

func startMetricsReceiver(t *testing.T) (*recordingMetricsReceiver, *grpc.ClientConn, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	receiver := &recordingMetricsReceiver{}
	colmetricpb.RegisterMetricsServiceServer(srv, receiver)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		_ = lis.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
	return receiver, conn, cleanup
}

func resourceAttrs(req *colmetricpb.ExportMetricsServiceRequest) map[string]string {
	for _, rm := range req.GetResourceMetrics() {
		return attrsFromResource(rm.GetResource())
	}
	return nil
}

func attrsFromResource(resource *resourcepb.Resource) map[string]string {
	out := make(map[string]string)
	for _, attr := range resource.GetAttributes() {
		out[attr.GetKey()] = attrString(attr.GetValue())
	}
	return out
}

func receiverMetricMap(req *colmetricpb.ExportMetricsServiceRequest) map[string]*metricspb.Metric {
	out := make(map[string]*metricspb.Metric)
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, metric := range sm.GetMetrics() {
				out[metric.GetName()] = metric
			}
		}
	}
	return out
}

func intPointValueOK(points []*metricspb.NumberDataPoint, attrs map[string]string) (int64, bool) {
	for _, point := range points {
		if attrsMatch(point.GetAttributes(), attrs) {
			return point.GetAsInt(), true
		}
	}
	return 0, false
}

func attrsMatch(attrs []*commonpb.KeyValue, want map[string]string) bool {
	if len(attrs) != len(want) {
		return false
	}
	got := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		got[attr.GetKey()] = attrString(attr.GetValue())
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func attrString(v *commonpb.AnyValue) string {
	return v.GetStringValue()
}
