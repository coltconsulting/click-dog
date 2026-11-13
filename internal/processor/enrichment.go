package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
)

// maxLogCommentSize caps the size of a single log_comment value processed by
// ExtractLogComment. Above this size the value is dropped (no JSON parse, no
// attribute write) to bound per-span memory in the face of a malformed or
// hostile log_comment from a misbehaving ClickHouse client.
const maxLogCommentSize = 64 * 1024

// ExtractLogComment finds ClickHouse log_comment values and promotes JSON
// keys to top-level span attributes prefixed with "log_comment.".
//
// Two sources are checked, in order:
//
//  1. Direct attributes whose key is "log_comment" or ends with ".log_comment"
//     (e.g. "clickhouse.setting.log_comment" on query-kind spans). The value
//     is expected to be JSON or a plain string.
//
//  2. URI attributes (http.url, http.target, then any other attribute
//     filter.IsURIAttribute recognizes) on HTTP-kind spans. The log_comment
//     query parameter is URL-decoded and then treated the same as case (1).
//     Statement attributes are never scanned: a SQL literal containing
//     "log_comment=" must not become exported metadata.
//
// Non-JSON values are set as a single "log_comment" attribute.
// A copy of the Attributes map is made to avoid mutating the original.
func ExtractLogComment(span *model.OpenTelemetrySpan) {
	var raw string

	// 1. Direct attribute check (preferred — no URL decoding needed).
	// Preferred keys are checked in explicit order to keep behavior
	// deterministic when multiple are present.
	for _, key := range []string{"log_comment", "clickhouse.setting.log_comment"} {
		if v, ok := span.Attributes[key]; ok {
			raw = v
			break
		}
	}
	// Fall back to any other *.log_comment attribute (map order is
	// non-deterministic, but this is the rare case).
	if raw == "" {
		for k, v := range span.Attributes {
			if strings.HasSuffix(k, ".log_comment") {
				raw = v
				break
			}
		}
	}

	// 2. URI-encoded fallback (for HTTP-kind spans without direct attribute)
	if raw == "" {
		for _, key := range []string{"http.url", "http.target"} {
			if v, ok := span.Attributes[key]; ok {
				if lc := extractLogCommentFromURI(v); lc != "" {
					raw = lc
					break
				}
			}
		}
	}

	// 3. Last resort: any other URI attribute containing log_comment=. Only
	// URI attributes are scanned; db.statement and clickhouse.query hold SQL,
	// and a literal there is query text, not a log_comment.
	if raw == "" {
		for k, v := range span.Attributes {
			if filter.IsURIAttribute(k) && strings.Contains(v, "log_comment=") {
				if lc := extractLogCommentFromURI(v); lc != "" {
					raw = lc
					break
				}
			}
		}
	}

	if raw == "" {
		return
	}

	// Bound per-span memory: a hostile or buggy ClickHouse client could
	// stuff arbitrarily large/deeply-nested JSON into log_comment. Skip
	// enrichment entirely above the cap rather than spending memory on
	// either Unmarshal or storing the raw string as an attribute.
	if len(raw) > maxLogCommentSize {
		return
	}

	// Copy attributes to avoid mutating shared map
	newAttrs := make(map[string]string, len(span.Attributes)+4)
	for k, v := range span.Attributes {
		newAttrs[k] = v
	}

	// Try to parse as JSON and promote keys
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		for k, v := range obj {
			switch val := v.(type) {
			case string:
				newAttrs["log_comment."+k] = val
			case bool, float64:
				newAttrs["log_comment."+k] = fmt.Sprintf("%v", val)
			default:
				if b, err := json.Marshal(val); err == nil {
					newAttrs["log_comment."+k] = string(b)
				}
			}
		}
	} else {
		newAttrs["log_comment"] = raw
	}

	span.Attributes = newAttrs
}

// extractLogCommentFromURI extracts and URL-decodes the log_comment parameter from a URI string.
func extractLogCommentFromURI(uri string) string {
	// Find log_comment= in the query string
	idx := strings.Index(uri, "log_comment=")
	if idx < 0 {
		return ""
	}

	// Parse just the query portion to properly URL-decode
	qMark := strings.LastIndex(uri[:idx], "?")
	queryStr := uri
	if qMark >= 0 {
		queryStr = uri[qMark+1:]
	}

	vals, err := url.ParseQuery(queryStr)
	if err != nil {
		return ""
	}
	return vals.Get("log_comment")
}

// OriginatingAddress returns the address of the client that started the
// query a query_log row belongs to. A distributed query fans out to other
// servers as secondary queries whose own `address` is the initiating server,
// not the client; `initial_address` names the client on every row of the
// query, so it is the value client-scoped decisions must use. Rows that carry
// no initial address (an unset or zero value) fall back to their own address.
func OriginatingAddress(ql model.QueryLog) string {
	return originatingAddress(ql.InitialAddress, ql.ClientAddress)
}

func originatingAddress(initial, own string) string {
	switch initial {
	case "", "::", "0.0.0.0", "::ffff:0.0.0.0":
		return own
	}
	return initial
}

// TraceDecision is FilteredTraces' verdict on a span page. The zero value
// withholds nothing.
type TraceDecision struct {
	// Dropped holds the traces none of whose spans in the page are exported.
	Dropped map[uuid.UUID]bool
	// Unattributed holds the spans of the other traces that run under no
	// query span read: their query has not finished, so it cannot be judged.
	Unattributed map[model.SpanKey]bool
}

// Withholds reports whether span is not exported.
func (d TraceDecision) Withholds(span *model.OpenTelemetrySpan) bool {
	return d.Dropped[span.TraceID] || d.Unattributed[model.KeyOf(*span)]
}

// FilteredTraces decides the trace-scoped filters for a live span page:
// whitelist_ips, whitelist_users, blacklist_users, and blacklist_queries. Each
// needs a query's client, user, or statement, which only a trace's query
// spans carry, so a per-span check would export or drop a query's child spans
// regardless of the query.
//
// Every trace is decided from the query spans the span log records for it,
// whichever page each landed in, and from each query's query_log origin, read
// without the user whitelist splice the enrichment SQL carries. Both logs are
// read from shortly before the earliest start of the page's spans, which
// holds the query span each of those spans runs under and that query's
// query_log rows. A trace is dropped when any of its queries matches
// blacklist_queries, comes from a client outside whitelist_ips, or fails
// AllowsQueryUsers. It fails closed: a trace with no query span yet is
// dropped, and a query with no query_log row drops its trace too.
//
// A span of a trace that is kept is exported only when it is attributed: its
// parent chain reaches a `query` span read, or it is the connection span a
// query span read runs directly under. The spans of a query that has not
// finished have no query span yet, so they are withheld rather than exported
// on the verdict of the trace's other queries. ClickHouse logs the query span
// when the query finishes, which moves the trace past the live walk's
// watermark, so it is read and decided again then. A lookup error returns err
// and no decision, so the caller can withhold the page and fail the cycle.
func FilteredTraces(
	ctx context.Context,
	reader TraceQueryReader,
	spans []model.OpenTelemetrySpan,
	f *filter.QueryFilter,
	lookbackDays int,
) (TraceDecision, error) {
	if !f.HasTraceScopedFilter() {
		return TraceDecision{}, nil
	}
	dropped := make(map[uuid.UUID]bool)
	var order []uuid.UUID
	seen := make(map[uuid.UUID]bool)
	for i := range spans {
		id := spans[i].TraceID
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
		// A blacklisted user stamped on the span or the page's own statement
		// can decide the trace early. Only the blacklist applies here: from
		// 25.8 a distributed query's work on another server stamps the
		// interserver account, and the whitelist judges the client's account,
		// which only query_log carries.
		if u := spans[i].Attributes["clickhouse.user"]; u != "" && f.IsBlacklistedUser(u) {
			dropped[id] = true
		}
		if f.IsBlacklistedQuery(spans[i].Attributes["db.statement"]) {
			dropped[id] = true
		}
	}
	var traceIDs []uuid.UUID
	for _, id := range order {
		if !dropped[id] {
			traceIDs = append(traceIDs, id)
		}
	}
	if len(traceIDs) == 0 {
		return TraceDecision{Dropped: dropped}, nil
	}

	// Attribute what the page can on its own, and ask the span log for the
	// ancestors of the rest.
	tree := newSpanTree(spans)
	earliestStartUs := EarliestStartUs(spans, traceIDs)
	byTrace, err := reader.FetchTraceNodes(ctx, traceIDs, lookbackDays, earliestStartUs, tree.ancestorRead(spans, dropped))
	if err != nil {
		return TraceDecision{}, fmt.Errorf("reading the traces' queries: %w", err)
	}
	tree.add(byTrace)

	// The page's own query spans are judged too: in cluster mode the span log
	// read can land on another replica than the page did, and a query span
	// that anchors spans must never go unjudged.
	type traceQuery struct{ id, statement string }
	queriesOf := make(map[uuid.UUID][]traceQuery, len(traceIDs))
	distinct := make(map[uuid.UUID]map[traceQuery]bool, len(traceIDs))
	addQuery := func(trace uuid.UUID, q traceQuery) {
		if q.id == "" || dropped[trace] {
			return
		}
		if distinct[trace] == nil {
			distinct[trace] = make(map[traceQuery]bool)
		}
		if !distinct[trace][q] {
			distinct[trace][q] = true
			queriesOf[trace] = append(queriesOf[trace], q)
		}
	}
	for i := range spans {
		addQuery(spans[i].TraceID, traceQuery{spans[i].Attributes["clickhouse.query_id"], spans[i].Attributes["db.statement"]})
	}
	for id, nodes := range byTrace {
		for _, n := range nodes {
			addQuery(id, traceQuery{n.QueryID, n.Statement})
		}
	}

	needOrigins := f.HasIPWhitelist() || f.HasUserFilter()
	var origins map[string]model.QueryOrigin
	if needOrigins {
		var queryIDs []string
		asked := make(map[string]bool)
		for _, queries := range queriesOf {
			for _, q := range queries {
				if !asked[q.id] {
					asked[q.id] = true
					queryIDs = append(queryIDs, q.id)
				}
			}
		}
		if len(queryIDs) > 0 {
			origins, err = reader.FetchQueryOrigins(ctx, queryIDs, lookbackDays, earliestStartUs)
			if err != nil {
				return TraceDecision{}, fmt.Errorf("reading the queries' users and clients: %w", err)
			}
		}
	}

	for _, id := range traceIDs {
		queries := queriesOf[id]
		if len(queries) == 0 {
			dropped[id] = true
			continue
		}
		for _, q := range queries {
			if f.IsBlacklistedQuery(q.statement) {
				dropped[id] = true
				break
			}
			if !needOrigins {
				continue
			}
			o, ok := origins[q.id]
			if !ok ||
				!f.AllowsQueryUsers(o.User, o.InitialUser) ||
				!f.AllowsClient(originatingAddress(o.InitialAddress, o.Address)) {
				dropped[id] = true
				break
			}
		}
	}

	decision := TraceDecision{Dropped: dropped}
	for i := range spans {
		if dropped[spans[i].TraceID] || tree.attributed(spans[i].TraceID, spans[i].SpanID, spans[i].OperationName) {
			continue
		}
		if decision.Unattributed == nil {
			decision.Unattributed = make(map[model.SpanKey]bool)
		}
		decision.Unattributed[model.KeyOf(spans[i])] = true
	}
	return decision, nil
}

// connectionOperations name the spans ClickHouse opens for a connection or a
// distributed DDL task before it starts the query span under them. Such a span
// belongs to the query it runs. A span of another kind that a query span runs
// under is inside some other query, such as one that loads a dictionary, and
// is attributed only through its own chain: it may belong to a query that has
// not finished yet.
var connectionOperations = []string{"TCPHandler", "HTTPHandler", "DDLWorker::processTask"}

func isConnectionOperation(name string) bool {
	for _, op := range connectionOperations {
		if strings.Contains(name, op) {
			return true
		}
	}
	return false
}

// spanTree links the spans of a page's traces, from the page itself and from
// the span log, so each span can be traced to the query span it runs under.
// Span IDs are unique within a trace, so a trace's spans from every server
// share one index.
type spanTree struct {
	parent map[model.SpanKey]uint64
	query  map[model.SpanKey]bool
	// queryParent holds the spans a query span runs directly under.
	queryParent map[model.SpanKey]bool
	memo        map[model.SpanKey]bool
}

func newSpanTree(spans []model.OpenTelemetrySpan) *spanTree {
	t := &spanTree{
		parent:      make(map[model.SpanKey]uint64, len(spans)),
		query:       make(map[model.SpanKey]bool),
		queryParent: make(map[model.SpanKey]bool),
		memo:        make(map[model.SpanKey]bool),
	}
	for i := range spans {
		s := &spans[i]
		t.link(s.TraceID, s.SpanID, s.ParentSpanID, s.OperationName == "query" && s.Attributes["clickhouse.query_id"] != "")
	}
	return t
}

// link records a span's parent. query marks a `query` span with a query ID,
// the only kind FilteredTraces judges and so the only kind that anchors.
func (t *spanTree) link(trace uuid.UUID, span, parent uint64, query bool) {
	key := model.SpanKey{TraceID: trace, SpanID: span}
	t.parent[key] = parent
	if query {
		t.query[key] = true
		t.queryParent[model.SpanKey{TraceID: trace, SpanID: parent}] = true
	}
}

func (t *spanTree) add(byTrace map[uuid.UUID][]model.TraceNode) {
	for trace, nodes := range byTrace {
		for _, n := range nodes {
			t.link(trace, n.SpanID, n.ParentSpanID, n.Query && n.QueryID != "")
		}
	}
	clear(t.memo)
}

// attributed reports whether the span is the connection span a query span
// runs under, or its parent chain, itself included, reaches a query span.
func (t *spanTree) attributed(trace uuid.UUID, span uint64, operation string) bool {
	if isConnectionOperation(operation) && t.queryParent[model.SpanKey{TraceID: trace, SpanID: span}] {
		return true
	}
	var chain []model.SpanKey
	result := false
	for key := (model.SpanKey{TraceID: trace, SpanID: span}); ; {
		if v, ok := t.memo[key]; ok {
			result = v
			break
		}
		if t.query[key] {
			result = true
			break
		}
		parent, ok := t.parent[key]
		chain = append(chain, key)
		if !ok || len(chain) > len(t.parent) {
			break
		}
		key = model.SpanKey{TraceID: trace, SpanID: parent}
	}
	for _, k := range chain {
		t.memo[k] = result
	}
	return result
}

// ancestorRead asks for the ancestors of the page spans the page cannot
// attribute, in traces not already dropped: every one of them started no
// later than the latest such span.
func (t *spanTree) ancestorRead(spans []model.OpenTelemetrySpan, dropped map[uuid.UUID]bool) model.AncestorRead {
	var read model.AncestorRead
	asked := make(map[uuid.UUID]bool)
	for i := range spans {
		s := &spans[i]
		if dropped[s.TraceID] || t.attributed(s.TraceID, s.SpanID, s.OperationName) {
			continue
		}
		if !asked[s.TraceID] {
			asked[s.TraceID] = true
			read.Traces = append(read.Traces, s.TraceID)
		}
		read.StartedByUs = max(read.StartedByUs, s.StartTimeUs)
	}
	return read
}

// EarliestStartUs returns the earliest start_time_us among the spans of the
// given traces, or 0 when spans holds none of them.
func EarliestStartUs(spans []model.OpenTelemetrySpan, traceIDs []uuid.UUID) uint64 {
	want := make(map[uuid.UUID]bool, len(traceIDs))
	for _, id := range traceIDs {
		want[id] = true
	}
	return earliestStartWhere(spans, func(s *model.OpenTelemetrySpan) bool { return want[s.TraceID] })
}

// earliestStartWhere returns the earliest start_time_us among the spans keep
// accepts, or 0 when it accepts none.
func earliestStartWhere(spans []model.OpenTelemetrySpan, keep func(*model.OpenTelemetrySpan) bool) uint64 {
	var earliest uint64
	found := false
	for i := range spans {
		if keep(&spans[i]) && (!found || spans[i].StartTimeUs < earliest) {
			earliest, found = spans[i].StartTimeUs, true
		}
	}
	return earliest
}

// AllowedSpans returns the spans scheduled export would keep: those
// FilteredTraces does not withhold and whose operation passes
// whitelist_operations.
// Readers that sample the span log outside the live walk, such as analyze,
// use it so their output describes only what the filters admit. It fails
// closed: a lookup error returns err and no spans.
func AllowedSpans(
	ctx context.Context,
	reader TraceQueryReader,
	spans []model.OpenTelemetrySpan,
	f *filter.QueryFilter,
	lookbackDays int,
) ([]model.OpenTelemetrySpan, error) {
	decision, err := FilteredTraces(ctx, reader, spans, f, lookbackDays)
	if err != nil {
		return nil, err
	}
	allowed := make([]model.OpenTelemetrySpan, 0, len(spans))
	for i := range spans {
		if decision.Withholds(&spans[i]) || !f.AllowsOperation(spans[i].OperationName) {
			continue
		}
		allowed = append(allowed, spans[i])
	}
	return allowed, nil
}

// UniqueQueryIDs returns the deduplicated set of ClickHouse query IDs
// from a slice of spans. Query IDs come from the clickhouse.query_id
// attribute; spans without this attribute are skipped.
func UniqueQueryIDs(spans []model.OpenTelemetrySpan) []string {
	seen := make(map[string]struct{}, len(spans))
	var ids []string
	for i := range spans {
		qid := spans[i].Attributes["clickhouse.query_id"]
		if qid == "" {
			continue
		}
		if _, ok := seen[qid]; !ok {
			seen[qid] = struct{}{}
			ids = append(ids, qid)
		}
	}
	return ids
}

// EarliestQueryStartUs returns the earliest start_time_us among the spans
// that carry a clickhouse.query_id, the spans UniqueQueryIDs reads, or 0 when
// none does. FetchQueryLogByQueryIDs reads query_log from shortly before it.
func EarliestQueryStartUs(spans []model.OpenTelemetrySpan) uint64 {
	return earliestStartWhere(spans, func(s *model.OpenTelemetrySpan) bool { return s.Attributes["clickhouse.query_id"] != "" })
}

// EnrichSpanFromQueryLog adds query_log metadata as span attributes.
// Makes a copy of the Attributes map to avoid mutating the original.
func EnrichSpanFromQueryLog(span *model.OpenTelemetrySpan, ql model.QueryLog, maxQueryLength int) {
	newAttrs := make(map[string]string, len(span.Attributes)+18)
	for k, v := range span.Attributes {
		newAttrs[k] = v
	}
	newStringSliceAttrs := copyStringSliceAttributes(span.StringSliceAttributes)

	newAttrs["query_log.query_id"] = ql.QueryID
	newAttrs["query_log.query_duration_ms"] = strconv.FormatUint(ql.QueryDurationMs, 10)
	newAttrs["query_log.read_rows"] = strconv.FormatUint(ql.ReadRows, 10)
	newAttrs["query_log.read_bytes"] = strconv.FormatUint(ql.ReadBytes, 10)
	newAttrs["query_log.written_rows"] = strconv.FormatUint(ql.WrittenRows, 10)
	newAttrs["query_log.written_bytes"] = strconv.FormatUint(ql.WrittenBytes, 10)
	newAttrs["query_log.result_rows"] = strconv.FormatUint(ql.ResultRows, 10)
	newAttrs["query_log.result_bytes"] = strconv.FormatUint(ql.ResultBytes, 10)
	newAttrs["query_log.memory_usage"] = strconv.FormatUint(ql.MemoryUsage, 10)
	if operation := normalizeQueryOperation(ql.QueryOperation); operation != "" {
		newAttrs["query_log.operation"] = operation
		newAttrs["query_log.access_type"] = classifyQueryAccess(operation)
	}

	if ql.NormalizedQueryHash != 0 {
		newAttrs["query_log.normalized_query_hash"] = strconv.FormatUint(ql.NormalizedQueryHash, 10)
	}
	if ql.NormalizedQuery != "" {
		newAttrs["query_log.normalized_query"] = truncateNormalizedQuery(ql.NormalizedQuery, maxQueryLength)
	}
	if ql.User != "" {
		newAttrs["query_log.user"] = ql.User
	}
	if ql.ClientName != "" {
		newAttrs["query_log.client_name"] = ql.ClientName
	}
	if ql.ClientHostname != "" {
		newAttrs["query_log.client_hostname"] = ql.ClientHostname
	}
	if ql.ClientAddress != "" {
		newAttrs["query_log.client_address"] = ql.ClientAddress
	}
	if ql.ExceptionCode != 0 {
		newAttrs["query_log.exception_code"] = strconv.FormatInt(int64(ql.ExceptionCode), 10)
	}
	if len(ql.DatabasesVisited) > 0 {
		setStringSliceAttribute(&newStringSliceAttrs, "query_log.databases", ql.DatabasesVisited)
	}
	if len(ql.TablesVisited) > 0 {
		setStringSliceAttribute(&newStringSliceAttrs, "query_log.tables", ql.TablesVisited)
	}

	span.Attributes = newAttrs
	span.StringSliceAttributes = newStringSliceAttrs
}

func normalizeQueryOperation(operation string) string {
	return strings.ToLower(strings.TrimSpace(operation))
}

func classifyQueryAccess(operation string) string {
	// Current ClickHouse folds TRUNCATE/DETACH into Drop, ATTACH into Create,
	// and KILL MUTATION into KillQuery. Retain their direct spellings as
	// defensive compatibility aliases for older or vendor-modified emitters.
	switch normalizeQueryOperation(operation) {
	case "select", "show", "describe", "exists", "explain", "check":
		return "read"
	case "insert", "asyncinsertflush", "delete", "update", "optimize":
		return "write"
	case "create", "alter", "drop", "undrop", "rename", "externalddl",
		"truncate", "attach", "detach":
		return "ddl"
	case "system", "killquery", "killmutation", "grant", "revoke", "move", "set", "use",
		"backup", "restore", "begin", "commit", "rollback", "settransactionsnapshot", "snapshot":
		return "admin"
	default:
		// None/empty, ParallelWithQuery, and Copy intentionally remain other:
		// the wrapper or COPY direction does not identify one stable access type.
		return "other"
	}
}

func copyStringSliceAttributes(attrs map[string][]string) map[string][]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string][]string, len(attrs))
	for k, values := range attrs {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func setStringSliceAttribute(attrs *map[string][]string, key string, values []string) {
	if len(values) == 0 {
		return
	}
	if *attrs == nil {
		*attrs = make(map[string][]string, 1)
	}
	(*attrs)[key] = append([]string(nil), values...)
}

func truncateNormalizedQuery(query string, maxLen int) string {
	return export.TruncateQuery(strings.TrimSpace(query), maxLen)
}
