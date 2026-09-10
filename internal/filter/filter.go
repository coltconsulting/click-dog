package filter

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	rawStatementAttribute            = "db.statement"
	normalizedStatementAttribute     = "db.normalized_query"
	enrichedNormalizedQueryAttribute = "query_log.normalized_query"
)

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
	whitelistIPs           map[string]bool // exact IP matches
	whitelistCIDRs         []*net.IPNet    // CIDR range matches
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
	// privacy boundary must enforce its own invariants. Rules under the two
	// stricter modes are safely inert and ignored; raw plus rules is rejected
	// because it would look redacted while exporting the original statement.
	if mode == config.QueryTextModeRaw && len(cfg.RedactQueries) > 0 {
		return nil, fmt.Errorf("redact_queries rules require query_text_mode redacted")
	}

	qf := &QueryFilter{
		queryTextMode:          mode,
		operationPatterns:      make([]*regexp.Regexp, 0, len(cfg.WhitelistOperations)),
		queryBlacklistPatterns: make([]*regexp.Regexp, 0, len(cfg.BlacklistQueries)),
		whitelistIPs:           make(map[string]bool),
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

	// Compile regex patterns for query blacklist
	for _, pattern := range cfg.BlacklistQueries {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		qf.queryBlacklistPatterns = append(qf.queryBlacklistPatterns, re)
	}

	// Compile redaction rules only when the mode consumes them. Stale rules in
	// normalized_only/none are ignored with a load-time warning rather than
	// turning a privacy-tightening configuration edit into an outage.
	if mode == config.QueryTextModeRedacted {
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
	}

	// Build user whitelist / blacklist. Empty entries are rejected in config
	// validation — here we just populate the lookup sets.
	for _, u := range cfg.WhitelistUsers {
		qf.whitelistUsers[u] = true
	}
	for _, u := range cfg.BlacklistUsers {
		qf.blacklistUsers[u] = true
	}

	// Build IP whitelist — support both exact IPs and CIDR notation
	for _, entry := range cfg.WhitelistIPs {
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR in whitelist_ips %q: %w", entry, err)
			}
			qf.whitelistCIDRs = append(qf.whitelistCIDRs, cidr)
		} else {
			qf.whitelistIPs[entry] = true
		}
	}

	// Log active filter configuration so operators know what's armed
	if qf.queryTextMode != config.QueryTextModeRaw || qf.hasOperationWhitelist || qf.hasIPWhitelist || qf.hasUserWhitelist || qf.hasUserBlacklist ||
		len(qf.queryBlacklistPatterns) > 0 || len(qf.redactionRules) > 0 {
		clicklog.Info("Filters active: query_text_mode=%s, operation_whitelist=%d, ip_whitelist=%d (exact=%d, cidr=%d), user_whitelist=%d, user_blacklist=%d, query_blacklist=%d, redaction_rules=%d",
			qf.queryTextMode,
			len(qf.operationPatterns), len(cfg.WhitelistIPs), len(qf.whitelistIPs), len(qf.whitelistCIDRs),
			len(qf.whitelistUsers), len(qf.blacklistUsers),
			len(qf.queryBlacklistPatterns), len(qf.redactionRules))
	}

	return qf, nil
}

// isIPWhitelisted checks if the given IP matches any exact IP or CIDR range.
func (qf *QueryFilter) isIPWhitelisted(clientIP string) bool {
	// Fast path: exact match
	if qf.whitelistIPs[clientIP] {
		return true
	}

	// Slow path: CIDR matching
	if len(qf.whitelistCIDRs) > 0 {
		ip := net.ParseIP(clientIP)
		if ip != nil {
			for _, cidr := range qf.whitelistCIDRs {
				if cidr.Contains(ip) {
					return true
				}
			}
		}
	}

	return false
}

func (qf *QueryFilter) ShouldFilter(operationName string, queryText string, clientIP string) bool {
	// 1. Check IP whitelist first (if configured)
	if qf.hasIPWhitelist {
		if !qf.isIPWhitelisted(clientIP) {
			clicklog.Debug("Filtering span from IP not in whitelist: %s", clientIP)
			return true
		}
	}

	// 2. Check operation whitelist (if configured)
	if qf.hasOperationWhitelist {
		matched := false
		for _, pattern := range qf.operationPatterns {
			if pattern.MatchString(operationName) {
				matched = true
				break
			}
		}
		if !matched {
			clicklog.Debug("Filtering operation not in whitelist: %s", operationName)
			return true
		}
	}

	// 3. Check query blacklist patterns
	for _, pattern := range qf.queryBlacklistPatterns {
		if pattern.MatchString(queryText) {
			clicklog.Debug("Filtering query matching blacklist pattern: %s", pattern.String())
			return true
		}
	}

	return false
}

// ShouldFilterUser reports whether the given ClickHouse user is excluded by
// the configured whitelist/blacklist. Strict — an empty (unknown) user is
// dropped when a whitelist is active. When both lists are set a user must
// appear in the whitelist AND not appear in the blacklist to pass.
// Case-sensitive exact match. See docs/filtering.md for rationale.
func (qf *QueryFilter) ShouldFilterUser(user string) bool {
	if qf.hasUserWhitelist && !qf.whitelistUsers[user] {
		clicklog.Debug("Filtering span from user not in whitelist: %q", user)
		return true
	}
	if qf.hasUserBlacklist && qf.blacklistUsers[user] {
		clicklog.Debug("Filtering span from blacklisted user: %q", user)
		return true
	}
	return false
}

// HasUserFilter reports whether any user-based filtering is configured.
// Callers use this to avoid resolving the user field when it isn't needed.
func (qf *QueryFilter) HasUserFilter() bool {
	return qf.hasUserWhitelist || qf.hasUserBlacklist
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
	removeRawQueryURIAttributes(attrs, slices)

	switch qf.queryTextMode {
	case config.QueryTextModeRedacted:
		if raw, ok := attrs[rawStatementAttribute]; ok {
			if redacted, matched := qf.redactQueryForExport(raw); matched {
				attrs[rawStatementAttribute] = redacted
			} else {
				delete(attrs, rawStatementAttribute)
			}
		}
		// A string-array statement cannot be safely interpreted as SQL text.
		delete(slices, rawStatementAttribute)
	case config.QueryTextModeNormalizedOnly:
		delete(attrs, rawStatementAttribute)
		delete(slices, rawStatementAttribute)
	case config.QueryTextModeNone:
		delete(attrs, rawStatementAttribute)
		delete(attrs, normalizedStatementAttribute)
		delete(attrs, enrichedNormalizedQueryAttribute)
		delete(slices, rawStatementAttribute)
		delete(slices, normalizedStatementAttribute)
		delete(slices, enrichedNormalizedQueryAttribute)
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

// removeRawQueryURIAttributes drops URI-bearing pass-through attribute values
// that carry a ClickHouse HTTP query parameter. HTTP GET requests can embed the
// full SQL in ?query= even when db.statement is removed. Attribute names are
// checked before their values so SQL string literals and ordinary metadata are
// not mistaken for request URIs. Dropping the entire unsafe URI is intentionally
// fail closed: trying to preserve and re-encode it could retain malformed or
// ambiguously delimited SQL fragments.
func removeRawQueryURIAttributes(attrs map[string]string, slices map[string][]string) {
	for key, value := range attrs {
		if isURIAttribute(key) && containsRawQueryParameter(key, value) {
			delete(attrs, key)
		}
	}
	for key, values := range slices {
		if !isURIAttribute(key) {
			continue
		}
		// cloneStringSliceMap allocated this backing array, so compacting it in
		// place cannot mutate the caller's StringSliceAttributes.
		kept := values[:0]
		for _, value := range values {
			if !containsRawQueryParameter(key, value) {
				kept = append(kept, value)
			}
		}
		if len(kept) == 0 {
			delete(slices, key)
			continue
		}
		slices[key] = kept
	}
}

// isURIAttribute recognizes standard OpenTelemetry URL attributes and common
// vendor-specific URL/URI names. Promoted log_comment.* metadata is excluded
// explicitly: query-text mode does not govern operator-supplied log comments.
func isURIAttribute(key string) bool {
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
	case "url", "urls", "uri", "uris":
		return true
	default:
		return false
	}
}

// containsRawQueryParameter recognizes query components in full URLs and
// request targets. Bare query strings are accepted only for attributes whose
// contract is the query component itself (for example url.query), avoiding
// false positives in URI attributes that merely contain "query=..." as text.
// Parameter names are URL-decoded and matched case-insensitively so encoded
// spellings such as %71uery cannot bypass the boundary.
func containsRawQueryParameter(key, value string) bool {
	query := value
	if isURIQueryComponentAttribute(key) {
		// The whole value is normally already the query component, and a
		// literal '?' is valid inside a parameter value. Strip a leading '?'
		// or misplaced request target only when the prefix cannot be a
		// parameter, so a literal cannot truncate the component.
		if question := strings.IndexByte(query, '?'); question >= 0 &&
			!strings.ContainsAny(query[:question], "=&;") {
			query = query[question+1:]
		}
	} else if question := strings.IndexByte(query, '?'); question >= 0 {
		query = query[question+1:]
	} else {
		return false
	}
	if fragment := strings.IndexByte(query, '#'); fragment >= 0 {
		query = query[:fragment]
	}
	for _, field := range strings.FieldsFunc(query, func(r rune) bool {
		return r == '&' || r == ';'
	}) {
		name, _, _ := strings.Cut(field, "=")
		decoded, err := url.QueryUnescape(name)
		if err == nil && strings.EqualFold(strings.TrimSpace(decoded), "query") {
			return true
		}
	}
	return false
}

func isURIQueryComponentAttribute(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "url.query", "http.query", "http.query_string", "request.query_string":
		return true
	default:
		return false
	}
}
