package model

import (
	"context"
	"time"

	"github.com/google/uuid"
)

const (
	// CycleSkipReasonCircuitOpen marks a cycle skipped because the circuit
	// breaker was open and no canary recovery cycle ran.
	CycleSkipReasonCircuitOpen = "circuit_open"

	// CycleSkipReasonLeaderStandby marks a healthy HA follower that skipped
	// work because another instance currently holds the leader gate.
	CycleSkipReasonLeaderStandby = "leader_standby"
)

// QueryLog represents a row from ClickHouse's system.query_log table.
// Used in two contexts:
//
//   - Backfill mode: ExportQuery() creates synthetic single-span traces from
//     query_log rows (no native trace context — IDs are generated).
//   - Live monitoring enrichment: FetchQueryLogByQueryIDs() joins query_log
//     to opentelemetry_span_log by query_id (the clickhouse.query_id span
//     attribute) to attach query metadata (user, client, tables, stats).
//
// Downstream consumers can distinguish backfill spans via the click_dog.source
// span attribute: "query_log" vs "span_log".
type QueryLog struct {
	QueryID   string
	QueryKind string
	// QueryOperation is ClickHouse's statement classification from
	// system.query_log.query_kind (for example Select, Insert, or Alter).
	// QueryKind above intentionally remains the query-log lifecycle event
	// (QueryFinish / ExceptionWhileProcessing) for compatibility with the
	// existing backfill identity and exported db.query_kind contract.
	QueryOperation  string
	EventTime       time.Time
	QueryDurationMs uint64
	// ElapsedMs is the wall-clock runtime so far of a still-running query, in
	// milliseconds. It is populated only for `current` trace-drilldown
	// candidates read from system.processes (which exposes `elapsed` seconds but
	// has no finished query_duration_ms); finished query_log rows leave it zero.
	ElapsedMs           uint64
	Query               string
	NormalizedQueryHash uint64
	NormalizedQuery     string
	User                string
	ClientName          string
	ClientHostname      string
	ClientAddress       string
	// InitialAddress is query_log.initial_address: the address of the client
	// that started the initial query. For the initial query it equals
	// ClientAddress; for the secondary queries a distributed query fans out to
	// other servers it names the originating client, where ClientAddress names
	// the initiating server. Client-scoped decisions (the IP whitelist) use it.
	InitialAddress   string
	DatabasesVisited []string
	TablesVisited    []string
	ExceptionCode    int32
	ReadRows         uint64
	ReadBytes        uint64
	WrittenRows      uint64
	WrittenBytes     uint64
	ResultRows       uint64
	ResultBytes      uint64
	MemoryUsage      uint64
}

// QueryFamilyExactGroup is the level-zero aggregate for query-family rollups.
// One exact group is one ClickHouse normalized_query_hash, with a bounded
// human-readable normalizeQuery(query) preview and aggregate stats from
// system.query_log. Higher-level rollups must start from these groups rather
// than from individual query executions.
type QueryFamilyExactGroup struct {
	NormalizedQueryHash uint64
	NormalizedQuery     string
	ExecutionCount      uint64
	SuccessfulCount     uint64
	FailedCount         uint64
	P95DurationMs       float64
	P99DurationMs       float64
	TopExceptions       []QueryExceptionCount
	MaxMemoryUsage      uint64
	P95ReadRows         float64
	P95ReadBytes        float64
	TopUsers            []string
	TopClients          []string
	TopTables           []string
	FirstSeen           time.Time
	LastSeen            time.Time
}

// QueryFamilyStats contains family-level stats derived from one or more exact
// normalized-query groups. Percentile-like fields are conservative rollups of
// the member exact-group percentiles, not recomputed from raw executions.
type QueryFamilyStats struct {
	ExecutionCount  uint64
	SuccessfulCount uint64
	FailedCount     uint64
	FailureRate     float64
	P95DurationMs   float64
	P99DurationMs   float64
	TopExceptions   []QueryExceptionCount
	MaxMemoryUsage  uint64
	P95ReadRows     float64
	P95ReadBytes    float64
	TopUsers        []string
	TopClients      []string
	TopTables       []string
	FirstSeen       time.Time
	LastSeen        time.Time
}

// QueryFamilyMember preserves the exact normalized-query group membership of
// a higher-level rollup family.
type QueryFamilyMember struct {
	NormalizedQueryHash uint64
	NormalizedQuery     string
	ExecutionCount      uint64
	SuccessfulCount     uint64
	FailedCount         uint64
	FailureRate         float64
	P95DurationMs       float64
	P99DurationMs       float64
	TopExceptions       []QueryExceptionCount
}

// QueryExceptionCount is one bounded exception-code frequency for an exact
// normalized-query group or a rollup family. Entries are ordered by count
// descending, then code ascending.
type QueryExceptionCount struct {
	Code  int32
	Count uint64
}

// QueryFamilyMergeReason explains why two exact normalized-query groups were
// allowed into the same higher-level family.
type QueryFamilyMergeReason struct {
	LeftHash          uint64
	RightHash         uint64
	Similarity        float64
	TokenSimilarity   float64
	ShingleSimilarity float64
	ClauseSimilarity  float64
	Reasons           []string
}

// QueryFamilyRollup is the dashboard/analysis layer above exact normalized
// hashes. It groups one or more exact groups that are structurally similar
// enough to treat as a broader query family while preserving the member hashes
// and merge explanations. MergeReasons is empty for singleton families because
// no exact groups were merged.
type QueryFamilyRollup struct {
	// FamilyID is stable across restarts because it is derived from
	// MemberHashesSorted.
	FamilyID            string
	RepresentativeQuery string
	// MemberHashesSorted is sorted by hash for deterministic family identity.
	// Members keeps execution-count order for display.
	MemberHashesSorted []uint64
	Members            []QueryFamilyMember
	Stats              QueryFamilyStats
	MergeReasons       []QueryFamilyMergeReason
}

// OpenTelemetrySpan represents a row from ClickHouse's system.opentelemetry_span_log.
// Used in scheduled (live) monitoring mode.  Carries native trace/span IDs.
//
// TraceID is the native 16-byte UUID from ClickHouse's UUID column; clickhouse-go
// scans into uuid.UUID directly. Exporters pass the raw bytes to OTLP; log sites
// format with uuid.UUID.String() when a human-readable form is needed.
type OpenTelemetrySpan struct {
	Hostname      string
	TraceID       uuid.UUID
	SpanID        uint64
	ParentSpanID  uint64
	OperationName string
	Kind          string
	StartTimeUs   uint64
	FinishTimeUs  uint64
	FinishDate    time.Time
	Attributes    map[string]string
	// StringSliceAttributes carries OTLP string-array attributes that cannot be
	// represented in ClickHouse's span attribute map. Live query_log enrichment
	// uses this for table/database facets.
	StringSliceAttributes map[string][]string
}

// CanaryResult holds the result of a lightweight canary query.
type CanaryResult struct {
	Count            int64
	LongQueriesExist bool // always Count > 0; convenience for readable attribute values
}

// SpanKey is the composite OTLP span identity (trace_id, span_id).
// OTLP span IDs are only unique within a trace, so dedup must key off the
// pair — keying off SpanID alone would drop a sibling span in a different
// trace that happened to collide on the 64-bit ID.
type SpanKey struct {
	TraceID uuid.UUID
	SpanID  uint64
}

// KeyOf returns the dedup key for a span.
func KeyOf(s OpenTelemetrySpan) SpanKey {
	return SpanKey{TraceID: s.TraceID, SpanID: s.SpanID}
}

// ExportSinkStatus reports the outcome for one configured export sink.
type ExportSinkStatus struct {
	// Name is the sink label used in logs and metrics. Standalone exporters use
	// stable built-in labels; MultiExporter emits one status per configured sink
	// using its configured name.
	Name string

	// Sent is the number of input items attempted for this sink. It is not the
	// number of network writes or OTEL chunks flushed to the wire.
	Sent int

	// Accepted is the number of input items this sink accepted. For
	// MultiExporter, per-sink Accepted may differ from the top-level
	// ExportResult.TotalAccepted: PolicyAllRequired reports the intersection
	// accepted by every sink, while PolicyAnySuccess reports the union accepted
	// by any sink.
	Accepted int

	// Error is nil when the sink accepted its attempted payload.
	Error error
}

// ExportResult reports what an exporter attempted and what was accepted.
//
// Accepted contains the accepted span keys for ExportSpans calls. It is empty
// for ExportQuery calls because query_log exports synthesize their trace/span IDs
// inside the exporter. For MultiExporter span exports, Accepted is the set of
// keys accepted under the configured fan-out policy.
//
// Empty-input and no-op exports return the zero value. Consumers should use
// len(Accepted) and len(Sinks), rather than nil checks, when they need to know
// how many spans were accepted or whether a per-sink attempt was made.
type ExportResult struct {
	Accepted      []SpanKey
	TotalSent     int
	TotalAccepted int
	Sinks         []ExportSinkStatus
}

// SpanExporter is the interface that all export backends must implement.
// OTELExporter and SplunkHECExporter satisfy this interface.
type SpanExporter interface {
	// ExportSpans exports multiple spans. The result carries the composite keys
	// (trace_id, span_id) of successfully exported spans so the caller can mark
	// only those as seen for live-mode dedup. Returns a non-nil error if the
	// export attempt failed.
	ExportSpans(ctx context.Context, spans []OpenTelemetrySpan) (ExportResult, error)

	// ExportQuery exports a single query_log row. Used in backfill mode. The
	// result reports count and per-sink status, but has no accepted span keys.
	ExportQuery(ctx context.Context, log QueryLog) (ExportResult, error)

	// Close shuts down the exporter, flushing any pending data.
	Close(ctx context.Context) error
}

// ConnectivityChecker is the optional companion to SpanExporter for backends
// that can actively verify reachability without exporting anything.
// `click-dog check` type-asserts each constructed exporter to this interface
// and probes the ones that implement it; exporters with no meaningful probe
// simply don't implement it and pass construction only. It is deliberately
// separate from SpanExporter so non-network exporters (e.g. the dry-run sink)
// need no stub implementation.
type ConnectivityChecker interface {
	// CheckConnectivity verifies the backend is reachable, honoring ctx for
	// cancellation and deadline. It must not export data.
	CheckConnectivity(ctx context.Context) error
}

// CanaryQuerier is the interface for running lightweight canary queries.
// Extracted from *ClickHouseReader to allow unit testing without a real connection.
type CanaryQuerier interface {
	RunCanaryQuery(ctx context.Context, thresholdMs int) (CanaryResult, error)
}
