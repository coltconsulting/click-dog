package analysis

import "time"

// TraceDrilldownSchemaVersion versions the trace-drilldown JSON contract. It is
// separate from ReportSchemaVersion because the trace drilldown answers the
// inverse question ("show me the trace for this query") and has its own shape.
const TraceDrilldownSchemaVersion = "analysis.trace_drilldown.v1"

// TraceDrilldownReport is the full result of one `click-dog analyze trace` run.
//
// It is an operational artifact: like the analysis report, it must never emit
// raw system.query_log.query or unbounded db.statement text. Optional
// sub-objects are pointers so an absent section is omitted from JSON rather
// than rendered as an empty struct. Findings carries the `-fanout findings`
// view: the analysis registry's findings filtered to the selected family, and
// is omitted when the fan-out is off or nothing matched.
type TraceDrilldownReport struct {
	SchemaVersion     string           `json:"schema_version"`
	GeneratedAt       time.Time        `json:"generated_at"`
	Window            AnalysisWindow   `json:"window"`
	Source            string           `json:"source"`
	SuppliedIdentity  *TraceIdentity   `json:"supplied_identity,omitempty"`
	SelectedCandidate *QueryCandidate  `json:"selected_candidate,omitempty"`
	Trace             *TraceSummary    `json:"trace,omitempty"`
	QueryLog          *QueryLogSummary `json:"query_log,omitempty"`
	Family            *FamilySummary   `json:"family,omitempty"`
	Findings          []Finding        `json:"findings,omitempty"`
	Warnings          []string         `json:"warnings,omitempty"`
}

// TraceIdentity records the identities supplied on the command line and any
// identities discovered during lookup. query_id and trace_id are direct paths;
// normalized_query_hash is an exact-identity path resolved against the recent
// candidate search. When more than one is supplied the command prefers the most
// precise path: trace_id, then query_id, then normalized_query_hash.
type TraceIdentity struct {
	QueryID             string `json:"query_id,omitempty"`
	TraceID             string `json:"trace_id,omitempty"`
	NormalizedQueryHash string `json:"normalized_query_hash,omitempty"`
}

// QueryCandidate is the selected query and the metadata available for it. In
// Phase 1 the candidate is built directly from the supplied identity and any
// query-log row found for it; later phases populate it from candidate search.
type QueryCandidate struct {
	Source          string    `json:"source"`
	QueryID         string    `json:"query_id,omitempty"`
	TraceIDs        []string  `json:"trace_ids,omitempty"`
	EventTime       time.Time `json:"event_time,omitempty"`
	QueryDurationMs uint64    `json:"query_duration_ms,omitempty"`
	// ElapsedMs is the wall-clock runtime so far of a still-running `current`
	// candidate (from system.processes), in milliseconds. It is zero for
	// finished `recent`/`other` candidates, which carry QueryDurationMs instead.
	ElapsedMs           uint64 `json:"elapsed_ms,omitempty"`
	QueryKind           string `json:"query_kind,omitempty"`
	User                string `json:"user,omitempty"`
	ClientName          string `json:"client_name,omitempty"`
	ClientHostname      string `json:"client_hostname,omitempty"`
	ClientAddress       string `json:"client_address,omitempty"`
	NormalizedQueryHash uint64 `json:"normalized_query_hash,omitempty"`
	NormalizedQuery     string `json:"normalized_query,omitempty"`
}

// TraceSummary is the bounded view of the native trace spans fetched for the
// selected trace IDs. Spans is capped by the command span limit; SpanCount is
// the number of spans actually fetched. SlowestSpans is the top-N spans by
// duration, descending, for quick human triage.
type TraceSummary struct {
	TraceIDs      []string         `json:"trace_ids"`
	SpanCount     int              `json:"span_count"`
	EarliestStart time.Time        `json:"earliest_start,omitempty"`
	LatestFinish  time.Time        `json:"latest_finish,omitempty"`
	RootOperation string           `json:"root_operation,omitempty"`
	SlowestSpans  []TraceSpanBrief `json:"slowest_spans,omitempty"`
}

// TraceSpanBrief is one bounded span row for the trace summary. It carries
// identity, timing, and operation metadata only — never raw query text. SpanID
// and ParentSpanID are rendered as decimal strings to keep the JSON stable and
// avoid uint64 precision loss in JSON consumers.
type TraceSpanBrief struct {
	TraceID       string `json:"trace_id"`
	SpanID        string `json:"span_id"`
	ParentSpanID  string `json:"parent_span_id,omitempty"`
	OperationName string `json:"operation_name"`
	Kind          string `json:"kind,omitempty"`
	DurationMs    uint64 `json:"duration_ms"`
	Hostname      string `json:"hostname,omitempty"`
}

// QueryLogSummary is the bounded query-log enrichment for the selected query.
// It mirrors the fields the analyze surface already exposes and never includes
// raw query text. Tables/Databases are bounded facets.
type QueryLogSummary struct {
	QueryID             string    `json:"query_id"`
	QueryKind           string    `json:"query_kind,omitempty"`
	EventTime           time.Time `json:"event_time,omitempty"`
	QueryDurationMs     uint64    `json:"query_duration_ms,omitempty"`
	ReadRows            uint64    `json:"read_rows,omitempty"`
	ReadBytes           uint64    `json:"read_bytes,omitempty"`
	WrittenRows         uint64    `json:"written_rows,omitempty"`
	WrittenBytes        uint64    `json:"written_bytes,omitempty"`
	ResultRows          uint64    `json:"result_rows,omitempty"`
	ResultBytes         uint64    `json:"result_bytes,omitempty"`
	MemoryUsage         uint64    `json:"memory_usage,omitempty"`
	ExceptionCode       int32     `json:"exception_code,omitempty"`
	User                string    `json:"user,omitempty"`
	ClientName          string    `json:"client_name,omitempty"`
	ClientHostname      string    `json:"client_hostname,omitempty"`
	ClientAddress       string    `json:"client_address,omitempty"`
	Databases           []string  `json:"databases,omitempty"`
	Tables              []string  `json:"tables,omitempty"`
	NormalizedQueryHash uint64    `json:"normalized_query_hash,omitempty"`
	NormalizedQuery     string    `json:"normalized_query,omitempty"`
}

// FamilySummary is the `similar` fan-out: the query-family rollup that contains
// the selected normalized query hash. FamilyID and member hashes are the
// deterministic rollup identity; RepresentativeQuery is the bounded normalized
// preview for the family. When rollups are unsupported, or the selected query
// has no normalized hash, the command emits a warning and leaves Family nil.
type FamilySummary struct {
	FamilyID            string   `json:"family_id,omitempty"`
	RepresentativeQuery string   `json:"representative_query,omitempty"`
	MemberHashes        []string `json:"member_hashes,omitempty"`
}
