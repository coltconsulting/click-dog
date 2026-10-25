package export

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// Compile-time checks that *OTELExporter satisfies model.SpanExporter and
// model.ConnectivityChecker. Package convention: an exporter that can probe
// its backend without exporting implements ConnectivityChecker so
// `click-dog check` picks it up; exporters with no meaningful probe simply
// omit it.
var (
	_ model.SpanExporter         = (*OTELExporter)(nil)
	_ model.ConnectivityChecker  = (*OTELExporter)(nil)
	_ model.ServerVersionStamper = (*OTELExporter)(nil)
)

type OTELExporter struct {
	conn           *grpc.ClientConn
	grpcClient     collectorpb.TraceServiceClient
	serviceName    string
	maxQueryLength int
	// resource holds the current Resource contents and the wrapper size that
	// matches them. The two travel together in one atomic value because the
	// size estimate is only correct for the Resource it was measured from:
	// SetServerVersion swaps both at once, and each export snapshots the pair
	// so its chunk accounting and its emitted envelope cannot disagree.
	resource atomic.Pointer[resourceState]
}

// resourceState is one immutable (Resource contents, wrapper size) pair.
type resourceState struct {
	// serverVersion is the connected ClickHouse server's version(), or empty
	// when it could not be resolved. Empty omits the attribute entirely rather
	// than emitting a placeholder.
	serverVersion string

	// wrapperBytes is the proto-encoded size of an empty
	// ExportTraceServiceRequest (ResourceSpans/ScopeSpans envelope with no
	// Spans) built from this state. Cached so streamChunks can estimate
	// per-chunk size without rebuilding the envelope each span.
	wrapperBytes int
}

func NewOTELExporter(cfg config.OTELConfig) (*OTELExporter, error) {
	conn, err := NewOTELGRPCConn(cfg)
	if err != nil {
		return nil, err
	}

	grpcClient := collectorpb.NewTraceServiceClient(conn)

	clicklog.Info("OTEL exporter initialized: %s (connection is lazy; first export will establish it)", cfg.CollectorAddress)

	exp := &OTELExporter{
		conn:           conn,
		grpcClient:     grpcClient,
		serviceName:    cfg.ServiceName,
		maxQueryLength: cfg.MaxQueryLength,
	}
	exp.resource.Store(exp.newResourceState(""))
	return exp, nil
}

// newResourceState measures the wrapper size for a given server version so the
// pair is always constructed together.
func (o *OTELExporter) newResourceState(serverVersion string) *resourceState {
	st := &resourceState{serverVersion: serverVersion}
	st.wrapperBytes = proto.Size(o.buildExportRequest(st, nil))
	return st
}

// SetServerVersion updates the ClickHouse server version stamped on the OTLP
// Resource. An empty version clears the attribute.
//
// It is deliberately a whole-state swap. streamChunks snapshots the pair once
// and uses it for every chunk it sends, so within one pass a batch's size
// accounting can never describe a different Resource than the one it sends.
// A ResourceExhausted bisection re-enters streamChunks and takes a fresh
// snapshot, which is equally consistent: that pass re-measures against
// whatever state it loaded.
func (o *OTELExporter) SetServerVersion(serverVersion string) {
	if current := o.resource.Load(); current != nil && current.serverVersion == serverVersion {
		return
	}
	o.resource.Store(o.newResourceState(serverVersion))
}

// currentResource snapshots the Resource state for one export.
//
// Both constructors seed the state, so a nil load means a future construction
// path forgot to. Seeding here rather than returning a zero value is
// deliberate: a zero wrapperBytes would silently under-count every chunk by the
// whole envelope, and the only symptom would be ResourceExhausted retries in
// sendOrSplit quietly doubling the export round trips.
func (o *OTELExporter) currentResource() *resourceState {
	if st := o.resource.Load(); st != nil {
		return st
	}
	seeded := o.newResourceState("")
	if o.resource.CompareAndSwap(nil, seeded) {
		return seeded
	}
	return o.resource.Load()
}

// NewOTELGRPCConn creates the shared OTLP gRPC connection used by span and
// metrics exporters. The returned connection is lazy; the first RPC establishes
// the network connection.
//
// Plaintext chosen automatically (secure omitted, loopback collector) is held
// to the local host: it ignores proxy settings and dials only loopback
// addresses. An explicit secure: false dials as configured.
func NewOTELGRPCConn(cfg config.OTELConfig) (*grpc.ClientConn, error) {
	name := cfg.Name
	if name == "" {
		name = "exporters.otel"
	}
	transport := config.TransportSummary(cfg.CollectorAddress, cfg.Secure, cfg.SecureSource)
	opts := []grpc.DialOption{}

	if cfg.Secure {
		tlsConfig, err := newOTELTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))

		if cfg.InsecureSkipVerify {
			clicklog.Warn("%s → %s: TLS (%s) with certificate verification disabled - this is insecure and should only be used for testing", name, cfg.CollectorAddress, cfg.SecureSource)
		} else {
			clicklog.Info("%s → %s: transport %s", name, cfg.CollectorAddress, transport)
		}
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		target := config.ParseOTLPTarget(cfg.CollectorAddress)
		switch {
		case cfg.SecureSource.IsExplicit() && !target.Loopback():
			clicklog.Warn("%s → %s: transport plaintext (%s.secure: false) — span payloads and query text travel unencrypted to a collector that is not on this host; remove secure: false to use TLS", name, cfg.CollectorAddress, name)
		case cfg.SecureSource.IsExplicit():
			clicklog.Info("%s → %s: transport %s", name, cfg.CollectorAddress, transport)
		default:
			// A context dialer also turns off grpc's proxy resolver;
			// WithNoProxy says so outright. Sockets keep grpc's own dialer.
			opts = append(opts, grpc.WithNoProxy())
			if !target.Socket() {
				opts = append(opts, grpc.WithContextDialer(loopbackOnlyDialer(name, net.DefaultResolver)))
			}
			clicklog.Info("%s → %s: transport %s (loopback collector; stays on this host)", name, cfg.CollectorAddress, transport)
		}
	}

	conn, err := grpc.NewClient(cfg.CollectorAddress, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
	}

	return conn, nil
}

// ipResolver is the slice of *net.Resolver the loopback dialer needs; tests
// substitute a stub.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// loopbackOnlyDialer dials automatic plaintext only to loopback addresses.
// Under the dns resolver grpc hands it a resolved ip:port; under passthrough
// it gets the configured host:port, which it resolves itself so that only a
// verified loopback IP is dialed. A name that resolves to any non-loopback
// address is refused rather than dialed.
func loopbackOnlyDialer(name string, resolver ipResolver) func(context.Context, string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%s: plaintext dial of %q: %w", name, addr, err)
		}
		if host == "" {
			host = "localhost"
		}
		var ips []netip.Addr
		if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
			ips = []netip.Addr{ip}
		} else {
			found, lookupErr := resolver.LookupIPAddr(ctx, host)
			if lookupErr != nil {
				return nil, fmt.Errorf("%s: resolve %q for plaintext dial: %w", name, host, lookupErr)
			}
			for _, a := range found {
				if ip, ok := netip.AddrFromSlice(a.IP); ok {
					ips = append(ips, ip)
				}
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%s: %q resolved to no addresses", name, host)
		}
		for _, ip := range ips {
			if !ip.Unmap().IsLoopback() {
				return nil, fmt.Errorf("%s: refusing plaintext to %s: %s is not a loopback address, and plaintext is automatic only for a collector on this host; set %s.secure: true for TLS, or secure: false to send plaintext over the network", name, addr, ip, name)
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

// newOTELTLSConfig builds the client TLS config for a secure exporter. Every
// configured file must load now, so a bad path or pair fails construction.
// ProbeOTELTLS builds its handshake from the same config, so `click-dog test
// tls` and the installer judge a receiver by exactly what export will use.
func newOTELTLSConfig(cfg config.OTELConfig) (*tls.Config, error) {
	// Validate mTLS configuration - both cert and key must be provided together
	if (cfg.ClientCert != "") != (cfg.ClientKey != "") {
		return nil, fmt.Errorf("mTLS requires both client_cert and client_key to be specified, got only one")
	}

	// MinVersion is Go's current client default, stated explicitly so the
	// floor is a property of this config rather than of the toolchain, and
	// so it matches internal/leader's Keeper dialer. Not a behavior change.
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	// Load CA certificate if provided. It is read once: crypto/tls has no
	// client-side hook for swapping RootCAs short of disabling its
	// verification and redoing it here, and the collector's CA changes far
	// less often than a short-lived client certificate.
	if cfg.CACert != "" {
		caCert, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("failed to read OTEL CA certificate: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse OTEL CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
	}

	// Load client certificate for mTLS if provided. The pair must load now;
	// later handshakes re-read it, so a renewed certificate takes effect
	// without a restart.
	if cfg.ClientCert != "" && cfg.ClientKey != "" {
		reloader, err := newClientCertReloader(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("failed to load OTEL client certificate: %w", err)
		}
		tlsConfig.GetClientCertificate = reloader.GetClientCertificate
	}

	return tlsConfig, nil
}

// Conn returns the exporter-owned OTLP gRPC connection. Callers may pass it to
// other OTLP service clients; OTELExporter remains responsible for closing it.
func (o *OTELExporter) Conn() *grpc.ClientConn {
	return o.conn
}

// CheckConnectivity forces the lazy gRPC connection and verifies it reaches
// a Ready or Idle state before the context deadline.
func (o *OTELExporter) CheckConnectivity(ctx context.Context) error {
	o.conn.Connect()
	state := o.conn.GetState()
	for state == connectivity.Connecting || state == connectivity.Idle {
		if !o.conn.WaitForStateChange(ctx, state) {
			return fmt.Errorf("timeout waiting for gRPC connection (last state: %s)", state)
		}
		state = o.conn.GetState()
	}
	if state == connectivity.Ready {
		return nil
	}
	return fmt.Errorf("gRPC connection in unexpected state: %s", state)
}

// Close shuts down the gRPC connection. The context is used as a deadline:
// if ctx expires before conn.Close() returns, Close returns the context error.
// gRPC's ClientConn.Close() itself does not accept a context, so we run it
// in a goroutine to honor the caller's deadline.
func (o *OTELExporter) Close(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- o.conn.Close() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ExportQuery exports a single query_log row as an OTEL span. Used only in
// backfill mode. The span carries click_dog.source="query_log" so downstream
// consumers can distinguish it from live spans (click_dog.source="span_log").
//
// Trace/span IDs are derived deterministically from the row's identity
// (query_id + event_time + query_kind) so that a re-export — a MultiExporter
// partial-failure retry, or a re-run backfill window — produces identical IDs.
// That makes repeats identifiable; whether a collector/backend collapses,
// stores, or rejects them is backend-specific.
//
// The provided ctx controls the gRPC call deadline. Callers should set a
// timeout (e.g. context.WithTimeout) to avoid blocking indefinitely if the
// collector is unreachable.
func (o *OTELExporter) ExportQuery(ctx context.Context, log model.QueryLog) (model.ExportResult, error) {
	traceID, spanID := deterministicQueryIDs(log)

	// Calculate timing
	spanStartTime := log.EventTime.Add(-time.Duration(log.QueryDurationMs) * time.Millisecond)
	startTimeNano := uint64(spanStartTime.UnixNano())
	endTimeNano := uint64(log.EventTime.UnixNano())

	// Build attributes
	attrs := []*commonpb.KeyValue{
		StringAttr("click_dog.source", "query_log"),
		StringAttr("db.system", "clickhouse"),
		StringAttr("db.user", log.User),
		StringAttr("db.query_id", log.QueryID),
		StringAttr("db.query_kind", log.QueryKind),
		IntAttr("db.query_duration_ms", int64(log.QueryDurationMs)),
		StringAttr("client.name", log.ClientName),
		StringAttr("client.hostname", log.ClientHostname),
		StringAttr("client.address", log.ClientAddress),
		IntAttr("db.read_rows", int64(log.ReadRows)),
		IntAttr("db.read_bytes", int64(log.ReadBytes)),
		IntAttr("db.written_rows", int64(log.WrittenRows)),
		IntAttr("db.written_bytes", int64(log.WrittenBytes)),
		IntAttr("db.result_rows", int64(log.ResultRows)),
		IntAttr("db.result_bytes", int64(log.ResultBytes)),
		IntAttr("db.memory_usage", int64(log.MemoryUsage)),
	}
	if statement := TruncateQuery(log.Query, o.maxQueryLength); statement != "" {
		attrs = append(attrs, StringAttr("db.statement", statement))
	}

	if len(log.DatabasesVisited) > 0 {
		attrs = append(attrs, StringSliceAttr("db.databases", log.DatabasesVisited))
	}
	if len(log.TablesVisited) > 0 {
		attrs = append(attrs, StringSliceAttr("db.tables", log.TablesVisited))
	}
	if log.NormalizedQueryHash != 0 {
		attrs = append(attrs, StringAttr("db.normalized_query_hash", strconv.FormatUint(log.NormalizedQueryHash, 10)))
	}
	if log.NormalizedQuery != "" {
		attrs = append(attrs, StringAttr("db.normalized_query", TruncateQuery(log.NormalizedQuery, o.maxQueryLength)))
	}

	// Mark span as error if query failed
	statusCode := tracepb.Status_STATUS_CODE_OK
	if log.ExceptionCode != 0 {
		attrs = append(attrs, BoolAttr("error", true))
		attrs = append(attrs, IntAttr("db.exception_code", int64(log.ExceptionCode)))
		statusCode = tracepb.Status_STATUS_CODE_ERROR
	}

	span := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            spanID,
		Name:              "clickhouse.query",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: startTimeNano,
		EndTimeUnixNano:   endTimeNano,
		Attributes:        attrs,
		Status:            &tracepb.Status{Code: statusCode},
	}

	if err := o.exportOTLPSpans(ctx, []*tracepb.Span{span}); err != nil {
		return failedResult("otel", 1, err), err
	}
	return acceptedCountResult("otel", 1, 1), nil
}

// deterministicQueryIDs derives a stable 16-byte trace ID and 8-byte span ID
// from a query_log row's identity, so re-exporting the same row (a
// MultiExporter partial-failure retry, or a re-run backfill window) yields
// identical OTLP IDs so retries carry a stable, identifiable span identity.
//
// The key is (query_id, event_time, query_kind) — NOT query_id alone.
// ClickHouse query_id is a caller-supplied value that is only guaranteed unique
// among concurrently running queries (see replace_running_query), so two
// distinct historical executions can reuse the same query_id. Folding in the
// row's event_time (microsecond precision) keeps distinct executions distinct
// while staying stable across reruns of the same row, so backfill never collapses
// two real rows onto one (trace_id, span_id).
func deterministicQueryIDs(log model.QueryLog) (traceID, spanID []byte) {
	key := fmt.Sprintf("click-dog/query_log\x00%s\x00%d\x00%s",
		log.QueryID, log.EventTime.UnixNano(), log.QueryKind)
	sum := sha256.Sum256([]byte(key))
	traceID = make([]byte, 16)
	spanID = make([]byte, 8)
	copy(traceID, sum[0:16])
	copy(spanID, sum[16:24])
	return traceID, spanID
}

// convertToOTLPSpan converts an OpenTelemetrySpan to an OTLP protobuf Span.
// The trace ID is passed through as 16 raw bytes (ClickHouse's native UUID
// column is already the right shape); the span/parent-span IDs are UInt64
// in ClickHouse and encode to 8 big-endian bytes per the OTLP spec.
func (o *OTELExporter) convertToOTLPSpan(span model.OpenTelemetrySpan) *tracepb.Span {
	// Copy TraceID into a fresh slice: the uuid.UUID value lives on the span
	// struct, but tracepb.Span.TraceId is []byte and we don't want the proto
	// to share backing storage with the caller's span.
	traceIDBytes := make([]byte, 16)
	copy(traceIDBytes, span.TraceID[:])

	// Convert span ID to bytes (8 bytes, big endian)
	spanIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(spanIDBytes, span.SpanID)

	// Convert parent span ID to bytes
	var parentSpanIDBytes []byte
	if span.ParentSpanID != 0 {
		parentSpanIDBytes = make([]byte, 8)
		binary.BigEndian.PutUint64(parentSpanIDBytes, span.ParentSpanID)
	}

	// Convert timestamps from microseconds to nanoseconds
	startTimeNano := uint64(span.StartTimeUs * 1000)
	endTimeNano := uint64(span.FinishTimeUs * 1000)

	// Calculate duration in milliseconds for easy querying
	durationMs := int64((span.FinishTimeUs - span.StartTimeUs) / 1000)

	// Convert attributes map to OTLP attributes. The source attribute is emitted
	// exactly once and stays first for consistency with ExportQuery. A source
	// supplied by the span (for example, test-span) is authoritative.
	attrs := make([]*commonpb.KeyValue, 0, len(span.Attributes)+len(span.StringSliceAttributes)+3)
	attrs = append(attrs, StringAttr(liveSpanSourceKey, liveSpanSource(span.Attributes)))
	attrs = append(attrs, StringAttr("hostname", span.Hostname))
	attrs = append(attrs, IntAttr("duration_ms", durationMs))
	for k, v := range span.Attributes {
		if k == liveSpanSourceKey {
			continue
		}
		if _, ok := span.StringSliceAttributes[k]; ok {
			continue
		}
		if k == liveSpanQueryKey {
			v = TruncateQuery(v, o.maxQueryLength)
		}
		attrs = append(attrs, typedSpanAttr(k, v))
	}
	for k, values := range span.StringSliceAttributes {
		if k == liveSpanSourceKey {
			continue
		}
		attrs = append(attrs, StringSliceAttr(k, values))
	}

	// Map span kind string to OTLP SpanKind
	var spanKind tracepb.Span_SpanKind
	switch span.Kind {
	case "INTERNAL":
		spanKind = tracepb.Span_SPAN_KIND_INTERNAL
	case "SERVER":
		spanKind = tracepb.Span_SPAN_KIND_SERVER
	case "CLIENT":
		spanKind = tracepb.Span_SPAN_KIND_CLIENT
	case "PRODUCER":
		spanKind = tracepb.Span_SPAN_KIND_PRODUCER
	case "CONSUMER":
		spanKind = tracepb.Span_SPAN_KIND_CONSUMER
	default:
		spanKind = tracepb.Span_SPAN_KIND_UNSPECIFIED
	}

	// STATUS_CODE_OK is intentional: ClickHouse's opentelemetry_span_log does
	// not expose per-span error state (no exception_code column). Only the
	// query_log path (ExportQuery) can set STATUS_CODE_ERROR because it has
	// access to ExceptionCode.
	return &tracepb.Span{
		TraceId:           traceIDBytes,
		SpanId:            spanIDBytes,
		ParentSpanId:      parentSpanIDBytes,
		Name:              span.OperationName,
		Kind:              spanKind,
		StartTimeUnixNano: startTimeNano,
		EndTimeUnixNano:   endTimeNano,
		Attributes:        attrs,
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
	}
}

// typedSpanAttr promotes the known numeric query_log attributes to OTLP
// IntValue; every other key stays a StringValue. It is called for all of
// span.Attributes, but only the keys in isLiveQueryLogIntAttr are promoted.
func typedSpanAttr(key, value string) *commonpb.KeyValue {
	if isLiveQueryLogIntAttr(key) {
		// ParseInt (not ParseUint) intentionally: OTLP integers are int64.
		// Malformed or out-of-range (> MaxInt64) values fall back to string
		// rather than truncating or wrapping into a misleading number.
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return IntAttr(key, parsed)
		}
	}
	return StringAttr(key, value)
}

func isLiveQueryLogIntAttr(key string) bool {
	switch key {
	case "query_log.query_duration_ms",
		"query_log.read_rows",
		"query_log.read_bytes",
		"query_log.written_rows",
		"query_log.written_bytes",
		"query_log.result_rows",
		"query_log.result_bytes",
		"query_log.memory_usage",
		// exception_code is only ever present when non-zero — the processor
		// gates it in enrichment.go (if ql.ExceptionCode != 0), so a "0" never
		// reaches this promoter. normalized_query_hash is deliberately absent:
		// it's a grouping dimension, kept a string even though it parses.
		"query_log.exception_code":
		return true
	default:
		return false
	}
}

// ExportSpans exports multiple OpenTelemetry spans, chunking incrementally
// so the entire OTLP batch is never materialised in memory at once. The
// provided ctx controls the gRPC call deadline.
//
// On full success, returns an ExportResult containing the composite
// (trace_id, span_id) keys of every span in the same order as the input
// slice — callers (e.g. dedup tracking in processor.go) rely on positional
// correspondence with the original spans.
//
// On the first send error, returns no accepted keys and the error: chunks
// already sent to the collector are not reported, preserving the previous
// all-or-nothing retry contract.
func (o *OTELExporter) ExportSpans(ctx context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	if len(spans) == 0 {
		return model.ExportResult{}, nil
	}

	keys := make([]model.SpanKey, len(spans))
	err := o.streamChunks(ctx, len(spans), func(i int) *tracepb.Span {
		keys[i] = model.KeyOf(spans[i])
		return o.convertToOTLPSpan(spans[i])
	})
	if err != nil {
		return failedResult("otel", len(spans), err), err
	}
	return acceptedSpansResult("otel", len(spans), keys), nil
}

// grpcMaxBytes is the target max message size per gRPC call.
// The default gRPC limit is 4MB; we target 3.5MB to leave headroom.
const grpcMaxBytes = 3_500_000

// spanFieldOverheadBytes accounts for the per-span proto field tag plus a
// length-varint inside ScopeSpans.Spans. Conservative upper bound: 1-byte
// field tag + up to 4-byte length varint, which covers any span up to ~256MB
// (well past anything that would fit in a single gRPC message anyway).
const spanFieldOverheadBytes = 5

// buildExportRequest wraps spans in an ExportTraceServiceRequest using the
// given Resource state. The state is passed in rather than loaded here so one
// export's envelope and its size estimate are measured from the same snapshot.
//
// clickhouse.version describes the server click-dog reads from, not the node
// that produced each span: under cluster queries a batch can carry rows from
// replicas on other versions during a rolling upgrade. It is a Resource
// attribute because it is constant for the batch.
func (o *OTELExporter) buildExportRequest(st *resourceState, spans []*tracepb.Span) *collectorpb.ExportTraceServiceRequest {
	attrs := []*commonpb.KeyValue{
		StringAttr("service.name", o.serviceName),
	}
	if st.serverVersion != "" {
		attrs = append(attrs, StringAttr("clickhouse.version", st.serverVersion))
	}
	return &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{
					Attributes: attrs,
				},
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Scope: &commonpb.InstrumentationScope{
							Name: "clickhouse",
						},
						Spans: spans,
					},
				},
			},
		},
	}
}

// streamChunks pulls spans one at a time from produce, accumulating into a
// rolling chunk whose proto-encoded size is estimated incrementally. Each
// chunk is flushed via sendOrSplit when adding the next span would exceed
// grpcMaxBytes. produce is called exactly once per index in order, so
// callers can perform conversion (and any side effects like recording keys)
// lazily inside the closure.
func (o *OTELExporter) streamChunks(ctx context.Context, count int, produce func(i int) *tracepb.Span) error {
	if count == 0 {
		return nil
	}
	st := o.currentResource()
	chunk := make([]*tracepb.Span, 0, 64)
	chunkBytes := st.wrapperBytes
	for i := 0; i < count; i++ {
		s := produce(i)
		spanBytes := proto.Size(s) + spanFieldOverheadBytes
		if len(chunk) > 0 && chunkBytes+spanBytes > grpcMaxBytes {
			if err := o.sendOrSplit(ctx, st, chunk, chunkBytes); err != nil {
				return err
			}
			// Drop references so the converted spans from the flushed chunk
			// can be GC'd before we accumulate the next one.
			for j := range chunk {
				chunk[j] = nil
			}
			chunk = chunk[:0]
			chunkBytes = st.wrapperBytes
		}
		chunk = append(chunk, s)
		chunkBytes += spanBytes
	}
	return o.sendOrSplit(ctx, st, chunk, chunkBytes)
}

// exportOTLPSpans sends pre-built OTLP spans via the gRPC client. Used by
// ExportQuery (single span) and by sendOrSplit's ResourceExhausted bisection
// recursion. Delegates to streamChunks so multi-span inputs are still
// size-bounded.
func (o *OTELExporter) exportOTLPSpans(ctx context.Context, spans []*tracepb.Span) error {
	return o.streamChunks(ctx, len(spans), func(i int) *tracepb.Span { return spans[i] })
}

// sendOrSplit attempts to send spans. On ResourceExhausted, it bisects and retries.
func (o *OTELExporter) sendOrSplit(ctx context.Context, st *resourceState, spans []*tracepb.Span, size int) error {
	req := o.buildExportRequest(st, spans)
	_, err := o.grpcClient.Export(ctx, req)
	if err == nil {
		return nil
	}

	if st, ok := status.FromError(err); ok && st.Code() == codes.ResourceExhausted && len(spans) > 1 {
		mid := len(spans) / 2
		clicklog.Info("Chunk still too large (%d bytes, %d spans), bisecting", size, len(spans))

		if err := o.exportOTLPSpans(ctx, spans[:mid]); err != nil {
			return err
		}
		return o.exportOTLPSpans(ctx, spans[mid:])
	}

	return fmt.Errorf("failed to export %d spans via gRPC: %w", len(spans), err)
}
