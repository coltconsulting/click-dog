package filter

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
)

// redactionEntry holds a compiled regex and its replacement string.
type redactionEntry struct {
	re          *regexp.Regexp
	replacement string
}

type QueryFilter struct {
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
	qf := &QueryFilter{
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

	// Compile redaction rules
	for i, rule := range cfg.RedactQueries {
		pattern := strings.TrimSpace(rule.Pattern)
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
	if qf.hasOperationWhitelist || qf.hasIPWhitelist || qf.hasUserWhitelist || qf.hasUserBlacklist ||
		len(qf.queryBlacklistPatterns) > 0 || len(qf.redactionRules) > 0 {
		clicklog.Info("Filters active: operation_whitelist=%d, ip_whitelist=%d (exact=%d, cidr=%d), user_whitelist=%d, user_blacklist=%d, query_blacklist=%d, redaction_rules=%d",
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

// RedactQuery applies all configured redaction rules to queryText.
// Returns the input unchanged when no rules are configured or none match.
func (qf *QueryFilter) RedactQuery(queryText string) string {
	for _, entry := range qf.redactionRules {
		queryText = entry.re.ReplaceAllString(queryText, entry.replacement)
	}
	return queryText
}
