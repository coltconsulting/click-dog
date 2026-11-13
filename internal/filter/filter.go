package filter

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	rawStatementAttribute = "db.statement"
	// remoteStatementAttribute is the statement ClickHouse sends to each shard
	// or replica, recorded on its Connection::sendQuery span. It is SQL as
	// written, literals included.
	remoteStatementAttribute         = "clickhouse.query"
	normalizedStatementAttribute     = "db.normalized_query"
	enrichedNormalizedQueryAttribute = "query_log.normalized_query"
)

// rawStatementAttributes are the span attributes that hold SQL as written.
var rawStatementAttributes = []string{rawStatementAttribute, remoteStatementAttribute}

// redactionEntry holds a compiled regex and its replacement string.
type redactionEntry struct {
	re          *regexp.Regexp
	replacement string
}

type QueryFilter struct {
	queryTextMode          config.QueryTextMode
	operationPatterns      []*regexp.Regexp
	queryBlacklistPatterns []*regexp.Regexp
	redactionRules         []redactionEntry
	clientPrefixes         []netip.Prefix // whitelist_ips entries, normalized by config.ClientPrefix
	whitelistUsers         map[string]bool
	blacklistUsers         map[string]bool
	hasOperationWhitelist  bool
	hasIPWhitelist         bool
	hasUserWhitelist       bool
	hasUserBlacklist       bool
}

func NewQueryFilter(cfg config.FiltersConfig) (*QueryFilter, error) {
	mode := cfg.EffectiveQueryTextMode()
	switch mode {
	case config.QueryTextModeRaw, config.QueryTextModeRedacted, config.QueryTextModeNormalizedOnly, config.QueryTextModeNone:
	default:
		return nil, fmt.Errorf("invalid query_text_mode %q", mode)
	}
	if mode == config.QueryTextModeRedacted && len(cfg.RedactQueries) == 0 {
		return nil, fmt.Errorf("query_text_mode redacted requires at least one redact_queries rule")
	}
	// This repeats Config.Validate deliberately: QueryFilter is also built from
	// directly-constructed configs in tests and integrations, so the final
	// privacy boundary must enforce its own invariants. Only redacted mode
	// consumes rules; under raw they would look redacted while exporting the
	// original statement.
	if mode != config.QueryTextModeRedacted && len(cfg.RedactQueries) > 0 {
		return nil, fmt.Errorf("redact_queries rules require query_text_mode redacted (currently %s)", mode)
	}

	qf := &QueryFilter{
		queryTextMode:          mode,
		operationPatterns:      make([]*regexp.Regexp, 0, len(cfg.WhitelistOperations)),
		queryBlacklistPatterns: make([]*regexp.Regexp, 0, len(cfg.BlacklistQueries)),
		whitelistUsers:         make(map[string]bool, len(cfg.WhitelistUsers)),
		blacklistUsers:         make(map[string]bool, len(cfg.BlacklistUsers)),
		hasOperationWhitelist:  len(cfg.WhitelistOperations) > 0,
		hasIPWhitelist:         len(cfg.WhitelistIPs) > 0,
		hasUserWhitelist:       len(cfg.WhitelistUsers) > 0,
		hasUserBlacklist:       len(cfg.BlacklistUsers) > 0,
	}

	// Convert the user's glob-style * wildcard into a regex .* (operation
	// whitelist patterns are anchored, literal except for *).
	for _, pattern := range cfg.WhitelistOperations {
		// Escape special regex characters except *
		regexPattern := regexp.QuoteMeta(pattern)
		// Replace escaped \* with .* for wildcard matching
		regexPattern = strings.ReplaceAll(regexPattern, "\\*", ".*")

		re, err := regexp.Compile("^" + regexPattern + "$")
		if err != nil {
			return nil, err
		}
		qf.operationPatterns = append(qf.operationPatterns, re)
	}

	// Compile regex patterns for query blacklist. ClickHouse's match(), which
	// applies the same patterns on the analyze and backfill reads, lets . match
	// a line break; (?s) gives Go the same semantics, so a multi-line statement
	// is judged alike on every path. A pattern can still opt out with (?-s).
	for _, pattern := range cfg.BlacklistQueries {
		re, err := regexp.Compile("(?s)" + pattern)
		if err != nil {
			return nil, err
		}
		qf.queryBlacklistPatterns = append(qf.queryBlacklistPatterns, re)
	}

	// Compile redaction rules; the check above admits them only in redacted mode.
	for i, rule := range cfg.RedactQueries {
		pattern := strings.TrimSpace(rule.Pattern)
		if pattern == "" {
			return nil, fmt.Errorf("invalid redact_queries[%d] regex: pattern must not be empty", i)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid redact_queries[%d] regex %q: %w", i, pattern, err)
		}
		replacement := rule.Replacement
		if replacement == "" {
			replacement = "[REDACTED]"
		}
		qf.redactionRules = append(qf.redactionRules, redactionEntry{re: re, replacement: replacement})
	}

	// Build user whitelist / blacklist. Empty entries are rejected in config
	// validation — here we just populate the lookup sets.
	for _, u := range cfg.WhitelistUsers {
		qf.whitelistUsers[u] = true
	}
	for _, u := range cfg.BlacklistUsers {
		qf.blacklistUsers[u] = true
	}

	// Build IP whitelist — exact IPs become single-address prefixes.
	for _, entry := range cfg.WhitelistIPs {
		prefix, err := config.ClientPrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid whitelist_ips entry %q: %w", entry, err)
		}
		qf.clientPrefixes = append(qf.clientPrefixes, prefix)
	}

	// Log active filter configuration so operators know what's armed
	if qf.queryTextMode != config.QueryTextModeRaw || qf.hasOperationWhitelist || qf.hasIPWhitelist || qf.hasUserWhitelist || qf.hasUserBlacklist ||
		len(qf.queryBlacklistPatterns) > 0 || len(qf.redactionRules) > 0 {
		clicklog.Info("Filters active: query_text_mode=%s, operation_whitelist=%d, ip_whitelist=%d, user_whitelist=%d, user_blacklist=%d, query_blacklist=%d, redaction_rules=%d",
			qf.queryTextMode,
			len(qf.operationPatterns), len(qf.clientPrefixes),
			len(qf.whitelistUsers), len(qf.blacklistUsers),
			len(qf.queryBlacklistPatterns), len(qf.redactionRules))
	}

	return qf, nil
}

// isIPWhitelisted checks if the given IP matches any exact IP or CIDR range.
// Addresses compare as parsed values, not strings: ClickHouse stores every
// client as IPv6 and reports an IPv4 client as ::ffff:a.b.c.d, and IPv6 has
// several spellings of one address.
func (qf *QueryFilter) isIPWhitelisted(clientIP string) bool {
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range qf.clientPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// HasUserFilter reports whether any user-based filtering is configured.
// Callers use this to avoid resolving the user field when it isn't needed.
func (qf *QueryFilter) HasUserFilter() bool {
	return qf.hasUserWhitelist || qf.hasUserBlacklist
}

// HasUserWhitelist reports whether a user whitelist is configured. Unlike the
// blacklist, it drops traces whose user cannot be resolved.
func (qf *QueryFilter) HasUserWhitelist() bool {
	return qf.hasUserWhitelist
}

// HasUserBlacklist reports whether a user blacklist is configured.
func (qf *QueryFilter) HasUserBlacklist() bool {
	return qf.hasUserBlacklist
}

// IsBlacklistedUser reports whether user appears in blacklist_users, ignoring
// the whitelist. Case-sensitive exact match.
func (qf *QueryFilter) IsBlacklistedUser(user string) bool {
	return qf.hasUserBlacklist && qf.blacklistUsers[user]
}

// HasQueryBlacklist reports whether blacklist_queries is configured.
func (qf *QueryFilter) HasQueryBlacklist() bool {
	return len(qf.queryBlacklistPatterns) > 0
}

// HasTraceScopedFilter reports whether any filter is configured whose input
// only some spans of a trace carry — a query's user, client, or statement —
// so the live path must decide it per trace.
func (qf *QueryFilter) HasTraceScopedFilter() bool {
	return qf.hasIPWhitelist || qf.hasUserWhitelist || qf.hasUserBlacklist || len(qf.queryBlacklistPatterns) > 0
}

// IsBlacklistedQuery reports whether query text matches a blacklist_queries
// pattern.
func (qf *QueryFilter) IsBlacklistedQuery(text string) bool {
	for _, pattern := range qf.queryBlacklistPatterns {
		if pattern.MatchString(text) {
			clicklog.Debug("Filtering query matching blacklist pattern: %s", pattern.String())
			return true
		}
	}
	return false
}

// AllowsOperation reports whether whitelist_operations admits an operation
// name; with no whitelist every operation is allowed.
func (qf *QueryFilter) AllowsOperation(operationName string) bool {
	if !qf.hasOperationWhitelist {
		return true
	}
	for _, pattern := range qf.operationPatterns {
		if pattern.MatchString(operationName) {
			return true
		}
	}
	clicklog.Debug("Filtering operation not in whitelist: %s", operationName)
	return false
}

// AllowsClient reports whether whitelist_ips admits a client address; with no
// whitelist every client is allowed.
func (qf *QueryFilter) AllowsClient(clientIP string) bool {
	if !qf.hasIPWhitelist || qf.isIPWhitelisted(clientIP) {
		return true
	}
	clicklog.Debug("Filtering span from IP not in whitelist: %s", clientIP)
	return false
}

// AllowsQueryUsers applies both user lists to one query: the whitelist judges
// the account of the client that started it (initialUser, or user when
// ClickHouse left it empty), and the blacklist rejects the query when either
// account is listed, so a blacklisted client's distributed secondary queries,
// which run as the interserver account, are rejected too.
func (qf *QueryFilter) AllowsQueryUsers(user, initialUser string) bool {
	if qf.IsBlacklistedUser(user) || qf.IsBlacklistedUser(initialUser) {
		clicklog.Debug("Filtering query from blacklisted user: %q (initial user %q)", user, initialUser)
		return false
	}
	originating := initialUser
	if originating == "" {
		originating = user
	}
	if qf.hasUserWhitelist && !qf.whitelistUsers[originating] {
		clicklog.Debug("Filtering query from user not in whitelist: %q", originating)
		return false
	}
	return true
}

// ShouldFilterQueryLog applies the filters that apply to a query_log row on
// the backfill path: blacklist_queries on its text, whitelist_ips on
// originatingAddr, and both user lists via AllowsQueryUsers. The operation
// filters describe native span-log operations and do not apply to query_log
// rows.
func (qf *QueryFilter) ShouldFilterQueryLog(query model.QueryLog, originatingAddr string) bool {
	return qf.IsBlacklistedQuery(query.Query) ||
		!qf.AllowsClient(originatingAddr) ||
		!qf.AllowsQueryUsers(query.User, query.InitialUser)
}

// HasIPWhitelist reports whether an IP whitelist is configured. Callers use
// this to avoid the query_log map lookup that resolves a span's client
// address when no IP filtering is active.
func (qf *QueryFilter) HasIPWhitelist() bool {
	return qf.hasIPWhitelist
}

// redactQueryForExport applies every configured rule and reports whether at
// least one rule matched the original or an intermediate representation. A
// redacted-mode query that matches no rule is omitted at the export boundary;
// it is never allowed to fall back to the raw statement.
func (qf *QueryFilter) redactQueryForExport(queryText string) (string, bool) {
	matched := false
	for _, entry := range qf.redactionRules {
		if entry.re.MatchString(queryText) {
			matched = true
		}
		queryText = entry.re.ReplaceAllString(queryText, entry.replacement)
	}
	return queryText, matched
}

// ShapeSpansForExport returns mode-compliant copies of spans immediately
// before they cross the exporter boundary. Filtering has already inspected
// the raw statement by this point. The caller's slice and attribute maps are
// never mutated, which is important for retries and multi-sink fan-out.
func (qf *QueryFilter) ShapeSpansForExport(spans []model.OpenTelemetrySpan) []model.OpenTelemetrySpan {
	if qf.queryTextMode == config.QueryTextModeRaw || len(spans) == 0 {
		return spans
	}

	shaped := make([]model.OpenTelemetrySpan, len(spans))
	for i, span := range spans {
		shaped[i] = qf.ShapeSpanForExport(span)
	}
	return shaped
}

// ShapeSpanForExport applies query_text_mode to one native span. Normalized
// text retains its explicit db.normalized_query/query_log.normalized_query key;
// it is never relabeled as db.statement.
func (qf *QueryFilter) ShapeSpanForExport(span model.OpenTelemetrySpan) model.OpenTelemetrySpan {
	if qf.queryTextMode == config.QueryTextModeRaw {
		return span
	}

	attrs := cloneStringMap(span.Attributes)
	slices := cloneStringSliceMap(span.StringSliceAttributes)
	removeURIAttributes(attrs, slices)
	removeSettingAttributes(attrs, slices)
	removeExceptionTextAttributes(attrs, slices)

	switch qf.queryTextMode {
	case config.QueryTextModeRedacted:
		for _, key := range rawStatementAttributes {
			if raw, ok := attrs[key]; ok {
				if redacted, matched := qf.redactQueryForExport(raw); matched {
					attrs[key] = redacted
				} else {
					delete(attrs, key)
				}
			}
			// A string-array statement cannot be safely interpreted as SQL text.
			delete(slices, key)
		}
	case config.QueryTextModeNormalizedOnly:
		for _, key := range rawStatementAttributes {
			delete(attrs, key)
			delete(slices, key)
		}
	case config.QueryTextModeNone:
		for _, key := range append([]string{normalizedStatementAttribute, enrichedNormalizedQueryAttribute}, rawStatementAttributes...) {
			delete(attrs, key)
			delete(slices, key)
		}
	}

	span.Attributes = attrs
	span.StringSliceAttributes = slices
	return span
}

// ShapeQueryForExport applies query_text_mode to a query_log record. Hashes,
// query identity, tables, timings, status, and resource metadata are retained.
func (qf *QueryFilter) ShapeQueryForExport(query model.QueryLog) model.QueryLog {
	switch qf.queryTextMode {
	case config.QueryTextModeRaw:
		return query
	case config.QueryTextModeRedacted:
		if redacted, matched := qf.redactQueryForExport(query.Query); matched {
			query.Query = redacted
		} else {
			query.Query = ""
		}
	case config.QueryTextModeNormalizedOnly:
		query.Query = ""
	case config.QueryTextModeNone:
		query.Query = ""
		query.NormalizedQuery = ""
	}
	return query
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneStringSliceMap(src map[string][]string) map[string][]string {
	if src == nil {
		return nil
	}
	dst := make(map[string][]string, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

// removeURIAttributes drops every URI-bearing and referer pass-through
// attribute. ClickHouse's HTTP interface takes the statement (query=), bound
// values (param_<name>=), and settings whose values are SQL
// (additional_table_filters=) as URL parameters, a Referer repeats them, and a
// predefined HTTP handler turns parts of the path itself into bound values, so
// no part of a request URI is safe to keep. Attribute names are checked, not
// values, so SQL string literals and ordinary metadata are not mistaken for
// request URIs.
func removeURIAttributes(attrs map[string]string, slices map[string][]string) {
	for key := range attrs {
		if IsURIAttribute(key) {
			delete(attrs, key)
		}
	}
	for key := range slices {
		if IsURIAttribute(key) {
			delete(slices, key)
		}
	}
}

const (
	// settingAttributePrefix is the prefix ClickHouse gives each changed
	// setting it records on a query span.
	settingAttributePrefix = "clickhouse.setting."
	// logCommentSetting is operator metadata, which query-text mode does not
	// govern.
	logCommentSetting = "log_comment"
)

// removeSettingAttributes drops every recorded setting except log_comment.
// ClickHouse records each setting a query changed, custom settings included,
// and several hold SQL or bound values: additional_table_filters,
// additional_result_filter and parallel_replicas_custom_key hold expressions
// with their literals, and a custom setting read through getSetting() is a
// bound value. A new setting must not reach the backend just because it is
// not on a list.
func removeSettingAttributes(attrs map[string]string, slices map[string][]string) {
	for key := range attrs {
		if isSettingAttribute(key) {
			delete(attrs, key)
		}
	}
	for key := range slices {
		if isSettingAttribute(key) {
			delete(slices, key)
		}
	}
}

func isSettingAttribute(key string) bool {
	name, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(key)), settingAttributePrefix)
	return ok && name != logCommentSetting
}

// removeExceptionTextAttributes drops exception message attributes, keeping
// exception codes. ClickHouse writes the exception message onto the query span
// as clickhouse.exception, and syntax and analyzer errors quote the statement
// with its literals, so the message is query text under another key. Any key
// naming an exception is dropped unless it is a code: a new message key must
// not reach the backend just because it is not on a list.
func removeExceptionTextAttributes(attrs map[string]string, slices map[string][]string) {
	for key := range attrs {
		if isExceptionTextAttribute(key) {
			delete(attrs, key)
		}
	}
	for key := range slices {
		if isExceptionTextAttribute(key) {
			delete(slices, key)
		}
	}
}

func isExceptionTextAttribute(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "exception") && !strings.HasSuffix(key, "exception_code")
}

// IsURIAttribute recognizes standard OpenTelemetry URL attributes, common
// vendor-specific URL/URI names, and referers, which repeat a page's URL with
// its parameters. Promoted log_comment.* metadata is excluded
// explicitly: query-text mode does not govern operator-supplied log comments.
func IsURIAttribute(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "log_comment" || strings.HasPrefix(key, "log_comment.") {
		return false
	}

	switch key {
	case "http.url", "http.urls", "http.target", "http.targets",
		"url.full", "url.query", "http.query", "http.query_string",
		"request.target", "request.uri", "request.url", "request.query_string":
		return true
	}

	parts := strings.FieldsFunc(key, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '/'
	})
	if len(parts) == 0 {
		return false
	}
	switch parts[len(parts)-1] {
	case "url", "urls", "uri", "uris", "referer", "referrer":
		return true
	default:
		return false
	}
}
