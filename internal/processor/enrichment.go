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
//  2. URI-encoded attributes (http.url, http.target, or any value containing
//     "log_comment=") on HTTP-kind spans. The log_comment query parameter
//     is URL-decoded and then treated the same as case (1).
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

	// 3. Last resort: scan all attributes for a URI containing log_comment=
	if raw == "" {
		for _, v := range span.Attributes {
			if strings.Contains(v, "log_comment=") {
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

// ShouldFilterSpanByUser reports whether the user filter excludes this span.
// HasUserFilter short-circuits the ResolveSpanUser map lookup in the hot
// per-span loop when no user filter is configured — keep the guard.
func ShouldFilterSpanByUser(span model.OpenTelemetrySpan, f *filter.QueryFilter, queryLogMap map[string]model.QueryLog) bool {
	return f.HasUserFilter() && f.ShouldFilterUser(ResolveSpanUser(span, queryLogMap))
}

// ResolveSpanUser returns the originating ClickHouse user for a span, or "" if
// it cannot be determined. The clickhouse.user attribute is checked first
// (forward-compat: CH does not currently emit it on opentelemetry_span_log,
// but a future version may); otherwise the user is resolved through the
// query_log enrichment map by clickhouse.query_id. Spans without query_id —
// typically internal child spans — return "" and are dropped by an active
// whitelist, matching the strict semantics of the IP whitelist.
func ResolveSpanUser(span model.OpenTelemetrySpan, queryLogMap map[string]model.QueryLog) string {
	if u := span.Attributes["clickhouse.user"]; u != "" {
		return u
	}
	if qid := span.Attributes["clickhouse.query_id"]; qid != "" {
		if ql, ok := queryLogMap[qid]; ok {
			return ql.User
		}
	}
	return ""
}

// OriginatingAddress returns the address of the client that started the
// query a query_log row belongs to. A distributed query fans out to other
// servers as secondary queries whose own `address` is the initiating server,
// not the client; `initial_address` names the client on every row of the
// query, so it is the value client-scoped decisions must use. Rows that carry
// no initial address (an unset or zero value) fall back to their own address.
func OriginatingAddress(ql model.QueryLog) string {
	switch ql.InitialAddress {
	case "", "::", "0.0.0.0", "::ffff:0.0.0.0":
		return ql.ClientAddress
	}
	return ql.InitialAddress
}

// TraceClientAddresses maps each trace to its originating client address. A
// trace has exactly one client, but a distributed trace carries several query
// spans — the initial query and one secondary query per remote server — and
// only query spans reach the query log, so the map is built once per cycle
// over every span and applied to the whole trace: resolving per span would
// admit roots and drop children, or admit or drop a trace's shards by which
// server's address happened to be seen first.
//
// A query-log row wins over a span attribute for the same trace whatever the
// row order: every row of a query names the same originating client, whereas
// a client.address attribute (forward-compat — ClickHouse does not emit it on
// span-log rows today) would be per server.
func TraceClientAddresses(spans []model.OpenTelemetrySpan, queryLogMap map[string]model.QueryLog) map[uuid.UUID]string {
	addrs := make(map[uuid.UUID]string)
	for i := range spans {
		if _, ok := addrs[spans[i].TraceID]; ok {
			continue
		}
		if addr := queryLogClientAddress(spans[i], queryLogMap); addr != "" {
			addrs[spans[i].TraceID] = addr
		}
	}
	for i := range spans {
		if _, ok := addrs[spans[i].TraceID]; ok {
			continue
		}
		if addr := spans[i].Attributes["client.address"]; addr != "" {
			addrs[spans[i].TraceID] = addr
		}
	}
	return addrs
}

// UnresolvedTraceIDs lists, in first-seen order, the traces in spans that the
// address map did not resolve — typically because the page holds children
// whose query root sits in another page.
func UnresolvedTraceIDs(spans []model.OpenTelemetrySpan, traceAddrs map[uuid.UUID]string) []uuid.UUID {
	var missing []uuid.UUID
	seen := make(map[uuid.UUID]bool)
	for i := range spans {
		id := spans[i].TraceID
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := traceAddrs[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// ResolveTraceAddressesFromSpanLog resolves the originating client of traces
// the current span page could not: it reads each trace's query IDs from the
// span log, fetches those query_log rows, and takes the originating address of
// the first row found. Traces with no query span inside lookbackDays, or whose
// rows are missing from query_log, stay unresolved.
func ResolveTraceAddressesFromSpanLog(ctx context.Context, reader LiveReader, traceIDs []uuid.UUID, lookbackDays int) (map[uuid.UUID]string, error) {
	byTrace, err := reader.FetchTraceQueryIDs(ctx, traceIDs, lookbackDays)
	if err != nil {
		return nil, err
	}
	var queryIDs []string
	seen := make(map[string]bool)
	for _, qids := range byTrace {
		for _, qid := range qids {
			if !seen[qid] {
				seen[qid] = true
				queryIDs = append(queryIDs, qid)
			}
		}
	}
	resolved := make(map[uuid.UUID]string)
	if len(queryIDs) == 0 {
		return resolved, nil
	}
	rows, err := reader.FetchQueryLogByQueryIDs(ctx, queryIDs, lookbackDays)
	if err != nil {
		return nil, err
	}
	for _, id := range traceIDs {
		for _, qid := range byTrace[id] {
			if ql, ok := rows[qid]; ok {
				if addr := OriginatingAddress(ql); addr != "" {
					resolved[id] = addr
					break
				}
			}
		}
	}
	return resolved, nil
}

// ResolveSpanClientAddress returns the originating client address for a span,
// or "" if it cannot be determined. The trace-level decision from
// TraceClientAddresses is authoritative so every span of a trace gets the
// same answer; a span is resolved on its own only when no trace decision
// exists. A trace whose address cannot be resolved at all returns "" and is
// dropped by an active whitelist, matching the strict semantics of the user
// whitelist.
func ResolveSpanClientAddress(span model.OpenTelemetrySpan, queryLogMap map[string]model.QueryLog, traceAddrs map[uuid.UUID]string) string {
	if addr, ok := traceAddrs[span.TraceID]; ok {
		return addr
	}
	if addr := queryLogClientAddress(span, queryLogMap); addr != "" {
		return addr
	}
	return span.Attributes["client.address"]
}

// queryLogClientAddress resolves one span through the query_log enrichment
// map by clickhouse.query_id, which is the source the IP whitelist is
// documented to use, returning the originating client of that query.
func queryLogClientAddress(span model.OpenTelemetrySpan, queryLogMap map[string]model.QueryLog) string {
	if qid := span.Attributes["clickhouse.query_id"]; qid != "" {
		if ql, ok := queryLogMap[qid]; ok {
			return OriginatingAddress(ql)
		}
	}
	return ""
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
		newAttrs["query_log.databases_csv"] = strings.Join(ql.DatabasesVisited, ",")
	}
	if len(ql.TablesVisited) > 0 {
		setStringSliceAttribute(&newStringSliceAttrs, "query_log.tables", ql.TablesVisited)
		newAttrs["query_log.tables_csv"] = strings.Join(ql.TablesVisited, ",")
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
