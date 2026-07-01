package processor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

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
