package export

import (
	"context"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// attrValue returns the string value of key in an attribute slice, and whether
// the key was present at all. It is used on both Resource and span attribute
// slices, which is the point: the tests below assert the two are distinct
// places.
func attrValue(attrs []*commonpb.KeyValue, key string) (string, bool) {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.GetStringValue(), true
		}
	}
	return "", false
}

func TestOTELExporter_ServerVersionOnResource(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		wantValue   string
		wantStamped bool
	}{
		{name: "unset omits the attribute", version: "", wantStamped: false},
		{name: "set stamps the resource", version: "25.3.2.1", wantValue: "25.3.2.1", wantStamped: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockTraceClient{}
			exp := newTestExporter(mock)
			exp.SetServerVersion(tc.version)

			if err := exp.exportOTLPSpans(context.Background(), makeSpans(3, 10)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(mock.calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(mock.calls))
			}

			got, ok := attrValue(mock.calls[0].ResourceSpans[0].Resource.Attributes, "clickhouse.version")
			if ok != tc.wantStamped {
				t.Fatalf("clickhouse.version present = %v, want %v", ok, tc.wantStamped)
			}
			if ok && got != tc.wantValue {
				t.Fatalf("clickhouse.version = %q, want %q", got, tc.wantValue)
			}

			// service.name must survive regardless.
			if _, ok := attrValue(mock.calls[0].ResourceSpans[0].Resource.Attributes, "service.name"); !ok {
				t.Fatal("service.name missing from resource")
			}
		})
	}
}

// The version belongs to the batch, not to individual spans. A per-span copy
// would multiply the payload by the span count.
func TestOTELExporter_ServerVersionIsNotPerSpan(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)
	exp.SetServerVersion("25.3.2.1")

	if err := exp.exportOTLPSpans(context.Background(), makeSpans(4, 10)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, span := range mock.calls[0].ResourceSpans[0].ScopeSpans[0].Spans {
		if _, ok := attrValue(span.Attributes, "clickhouse.version"); ok {
			t.Fatal("clickhouse.version must not be stamped on individual spans")
		}
	}
}

// A ClickHouse upgrade under a running click-dog must reach the next export.
func TestOTELExporter_ServerVersionChangeReachesNextExport(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	exp.SetServerVersion("24.8.4.13")
	if err := exp.exportOTLPSpans(context.Background(), makeSpans(1, 10)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exp.SetServerVersion("25.3.2.1")
	if err := exp.exportOTLPSpans(context.Background(), makeSpans(1, 10)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(mock.calls))
	}
	for i, want := range []string{"24.8.4.13", "25.3.2.1"} {
		got, ok := attrValue(mock.calls[i].ResourceSpans[0].Resource.Attributes, "clickhouse.version")
		if !ok || got != want {
			t.Fatalf("call %d clickhouse.version = %q (present %v), want %q", i, got, ok, want)
		}
	}
}

// A cleared version must not leave the previous value behind: a stale version
// silently attributes new telemetry to the wrong build.
func TestOTELExporter_ClearedServerVersionDropsAttribute(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	exp.SetServerVersion("25.3.2.1")
	exp.SetServerVersion("")
	if err := exp.exportOTLPSpans(context.Background(), makeSpans(1, 10)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := attrValue(mock.calls[0].ResourceSpans[0].Resource.Attributes, "clickhouse.version"); ok {
		t.Fatal("cleared version must remove the attribute, not retain the old value")
	}
}

// The cached wrapper size bounds gRPC chunking. If it were measured from a
// different Resource than the one sent, chunks would be undersized by exactly
// the version attribute and could exceed the limit.
func TestOTELExporter_WrapperBytesTracksResource(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	for _, version := range []string{"", "25.3.2.1", "24.8.4.13-with-a-much-longer-build-suffix", ""} {
		exp.SetServerVersion(version)
		st := exp.currentResource()

		if st.serverVersion != version {
			t.Fatalf("state version = %q, want %q", st.serverVersion, version)
		}
		want := proto.Size(exp.buildExportRequest(st, nil))
		if st.wrapperBytes != want {
			t.Fatalf("version %q: wrapperBytes = %d, want %d", version, st.wrapperBytes, want)
		}
	}
}

// Every chunk actually sent must fit the limit the estimate was policing.
func TestOTELExporter_ChunksStayBoundedWithVersion(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)
	exp.SetServerVersion("25.3.2.1-a-deliberately-long-version-string-for-size")

	spans := makeSpans(4000, 1000)
	if err := exp.exportOTLPSpans(context.Background(), spans); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.calls) < 2 {
		t.Fatalf("expected the batch to chunk, got %d calls", len(mock.calls))
	}

	var total int
	for i, call := range mock.calls {
		if size := proto.Size(call); size > grpcMaxBytes {
			t.Fatalf("chunk %d is %d bytes, over the %d limit", i, size, grpcMaxBytes)
		}
		total += len(call.ResourceSpans[0].ScopeSpans[0].Spans)
	}
	if total != len(spans) {
		t.Fatalf("sent %d spans across chunks, want %d", total, len(spans))
	}
}

// Repeated identical sets must not churn the state pointer; the fast path
// exists so a per-cycle refresh is free when nothing changed.
func TestOTELExporter_RepeatedSetServerVersionKeepsState(t *testing.T) {
	exp := newTestExporter(&mockTraceClient{})
	exp.SetServerVersion("25.3.2.1")
	first := exp.currentResource()
	exp.SetServerVersion("25.3.2.1")
	if exp.currentResource() != first {
		t.Fatal("setting the same version must not rebuild the resource state")
	}
}
