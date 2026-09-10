package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	clickmetrics "github.com/coltconsulting/click-dog/internal/metrics"
)

// envVarPattern matches ${VAR} or $VAR patterns for environment variable expansion
var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// safeIdentifierPattern matches only characters safe for use in SQL identifiers.
// This prevents SQL injection via config values that are interpolated into queries
// (e.g. cluster names used in cluster('name', table) calls).
var safeIdentifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// DefaultExportTimeoutS is the per-call exporter deadline used when
// monitor.export_timeout_s is omitted.
const DefaultExportTimeoutS = 30

// MaxSpansPerCycleCeiling is the hard upper bound on monitor.max_spans_per_cycle.
// The spans query would otherwise silently cap its per-cycle SQL LIMIT at this
// value, so config validation rejects any larger setting at load instead of
// starting on a value that can't take effect. clickhouse.MaxSpanQueryLimit
// mirrors this constant.
const MaxSpansPerCycleCeiling = 100_000

// validateClusterAddr returns "" when addr is a usable host:port for the
// /clusterz peer fanout, or a short reason string otherwise. The fanout
// builds peer URLs as `http://<addr>/readyz`, so a scheme prefix or path
// in the input would produce a malformed URL and a cryptic dial error
// at runtime; surfacing the problem at config load is much friendlier.
//
// SplitHostPort alone is insufficient: it accepts "http://node-0:8686"
// (parsing it as host="http://node-0", port="8686") because it splits
// on the last colon. The "://" check catches that case explicitly.
func validateClusterAddr(addr string) string {
	if strings.Contains(addr, "://") {
		return "must not include a scheme (drop the http:// prefix)"
	}
	if strings.ContainsAny(addr, "/?#") {
		return "must not include a path or query (just host:port)"
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return err.Error()
	}
	return ""
}

func validateWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return err.Error()
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "scheme must be http or https"
	}
	if u.Host == "" {
		return "host is required"
	}
	return ""
}

// ValidateDatadogSite accepts a strict DNS hostname without encoding
// Datadog's evolving site catalogue. The site is trusted operator
// configuration; structural validation prevents URL-authority smuggling
// through userinfo, ports, paths, IP literals, or encoded delimiters while
// preserving the existing dashboard-provisioning support for future and
// custom Datadog sites.
func ValidateDatadogSite(raw string) (string, error) {
	site := strings.ToLower(strings.TrimSpace(raw))
	if site == "" || len(site) > 253 || strings.HasSuffix(site, ".") || !strings.Contains(site, ".") || net.ParseIP(site) != nil {
		return "", errors.New("must be a Datadog hostname")
	}
	for _, label := range strings.Split(site, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("must be a valid DNS hostname")
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return "", errors.New("must be a valid DNS hostname")
			}
		}
	}
	return site, nil
}

var datadogTagValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.:/-]+$`)

// ValidateDatadogTagValue returns a safe validation message for an explicitly
// configured low-cardinality Datadog event tag. It is exported so the direct
// Events client constructor enforces the same rules as Config.Validate.
func ValidateDatadogTagValue(field, value string) string {
	if strings.TrimSpace(value) == "" {
		return field + " is required when datadog_events is enabled"
	}
	if len(value) > 200 || !datadogTagValuePattern.MatchString(value) {
		return field + " must be at most 200 characters using only letters, numbers, dot, underscore, colon, slash, or hyphen"
	}
	return ""
}

// expandEnvVars replaces ${VAR} or $VAR patterns with environment variable values
// If the environment variable is not set, it returns an empty string
func expandEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		var varName string
		if len(match) > 1 && match[1] == '{' {
			// ${VAR} format
			varName = match[2 : len(match)-1]
		} else {
			// $VAR format
			varName = match[1:]
		}
		return os.Getenv(varName)
	})
}

// resolveSecret returns the effective secret value given an inline value (already
// env-expanded) and the RAW (un-expanded) *_file field. Precedence and failure
// modes:
//
//   - file field unset (raw "")        -> return inline unchanged.
//   - file field set but expands to "" -> error (fail closed). A non-empty
//     *_file key whose ${ENV} template resolves to an empty path (the var is
//     unset) is a misconfiguration, not "no file" — returning inline/empty
//     would silently disable file-based loading and defer the failure to an
//     opaque auth error (or connect with an empty credential).
//   - both set (inline non-empty AND file field non-empty) -> error (ambiguous).
//   - file set -> read it, trim a single trailing newline, return contents.
//     An unreadable file is an error (fail closed; never fall back to inline/empty).
//     A file that resolves to an empty string is an error too — an empty secret
//     file is never intentional, and silently returning "" would defer the
//     failure to an opaque auth error at connect time.
//
// Expanding the path here (rather than at the call site) is what lets the
// unset-template case fail closed: the caller passes the raw field so resolveSecret
// can tell an absent key from one that expanded to "".
//
// The label (e.g. "clickhouse.password") only appears in error messages; the
// secret value itself is never logged.
func resolveSecret(label, inline, fileField string) (string, error) {
	if fileField == "" {
		return inline, nil
	}
	filePath := expandEnvVars(fileField)
	if filePath == "" {
		return "", fmt.Errorf("%s_file is set (%q) but expands to an empty path; set the referenced environment variable or remove the key", label, fileField)
	}
	if inline != "" {
		return "", fmt.Errorf("%s: set either the inline value or %s_file, not both", label, label)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("%s_file: %w", label, err)
	}
	// Trim exactly one trailing line ending — CRLF as a unit, else a bare LF —
	// so an editor-added newline doesn't become part of the credential. Any
	// other trailing whitespace (including a lone CR, or a second newline) is
	// left intact, so a password ending in whitespace is preserved.
	s := string(data)
	if rest, ok := strings.CutSuffix(s, "\r\n"); ok {
		s = rest
	} else {
		s = strings.TrimSuffix(s, "\n")
	}
	if s == "" {
		// Also fires for a file containing only a trailing newline (trimmed
		// above) — call that out so "but it's not empty!" is less confusing.
		// An intentionally-empty password is still expressible inline (omit the
		// _file key); a _file pointed at an empty file is treated as a mistake.
		return "", fmt.Errorf("%s_file %q is empty (or only a trailing newline); write the secret to it, or for an empty password use the inline %s key", label, filePath, label)
	}
	return s, nil
}

// unresolvedEnvRefs scans raw config bytes for ${VAR} references whose
// environment variable is unset, so a typo'd or missing ${CLICKHOUSE_PASSWORD}
// surfaces as a load warning instead of silently expanding to "" (which would
// connect with an empty credential, or fail later with an opaque auth error).
//
// Scoped to the braced ${VAR} form only — the bare $VAR form, while still
// expanded, is too easily a literal '$' inside a password or filter pattern to
// warn on safely. Comment lines are skipped so example snippets don't trip it.
func unresolvedEnvRefs(data []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range envVarPattern.FindAllStringSubmatch(line, -1) {
			name := m[1] // group 1 = ${VAR} body; empty for the bare $VAR form
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if _, ok := os.LookupEnv(name); !ok {
				out = append(out, fmt.Sprintf(
					"environment variable %q is referenced in the config but not set; it expands to an empty string", name))
			}
		}
	}
	return out
}

type Config struct {
	ClickHouse    ClickHouseConfig    `yaml:"clickhouse"`
	Exporters     ExportersConfig     `yaml:"exporters"` // OTEL gRPC + Splunk HEC sinks
	Monitor       MonitorConfig       `yaml:"monitor"`
	Filters       FiltersConfig       `yaml:"filters"`
	HA            HAConfig            `yaml:"ha"`
	LogLevel      string              `yaml:"log_level"`      // debug, info, warn, error (default: info)
	LogFile       string              `yaml:"log_file"`       // Optional log file path (if empty, logs to stderr only)
	LogFormat     string              `yaml:"log_format"`     // text or json (default: text)
	LogRotation   LogRotationConfig   `yaml:"log_rotation"`   // Log rotation settings; validated even when log_file is empty
	Metrics       MetricsConfig       `yaml:"metrics"`        // Prometheus metrics endpoint
	Health        HealthConfig        `yaml:"health"`         // HTTP health endpoints (/healthz, /readyz, /status)
	Webhook       WebhookConfig       `yaml:"webhook"`        // Webhook notifications for selected operational and analysis events
	DatadogEvents DatadogEventsConfig `yaml:"datadog_events"` // Optional Datadog Event Management destination for analysis findings

	// DeprecationWarnings holds warnings for deprecated config fields found
	// during loading. Not part of the YAML schema — populated by LoadConfig.
	DeprecationWarnings []string `yaml:"-"`

	// EnvWarnings holds warnings for ${VAR} references whose environment
	// variable is unset (kept separate from DeprecationWarnings so callers can
	// assert on each independently). Not part of the YAML schema.
	EnvWarnings []string `yaml:"-"`

	// ValidationWarnings holds non-fatal advisories about knob combinations
	// that load and run but quietly degrade behavior (e.g. a topology-audit
	// cadence that defeats its own detection window). Unlike Validate's hard
	// errors these don't block startup. Not part of the YAML schema —
	// populated by LoadConfig.
	ValidationWarnings []string `yaml:"-"`
}

// LogRotationConfig configures size-based log file rotation.
type LogRotationConfig struct {
	MaxSizeMB int `yaml:"max_size_mb"` // Max size per log file in MB (default: 100)
	MaxFiles  int `yaml:"max_files"`   // Max rotated files to keep (default: 3)
}

// ExportersConfig holds configuration for one or more export backends.
type ExportersConfig struct {
	OTEL      []OTELConfig      `yaml:"otel"`
	SplunkHEC []SplunkHECConfig `yaml:"splunk_hec"`
}

// HAConfig configures high availability via ClickHouse Keeper leader election.
// Leadership is driven solely by the presence of Keeper hosts: configure
// ha.keeper.hosts and election runs; leave it empty and it doesn't. There is
// no separate enable toggle — the coordination endpoints ARE the signal that
// the operator wants coordinated leadership, so only the leader exports and
// multiple instances reading overlapping data don't duplicate. The elected
// leader additionally handles coordination duties (peer health, backfill).
type HAConfig struct {
	Keeper LeaderElectionConfig `yaml:"keeper"` // Keeper connection settings; presence of hosts enables election
}

// Active reports whether leader election should run. True exactly when Keeper
// coordination endpoints are configured — see HAConfig.
func (h HAConfig) Active() bool {
	return len(h.Keeper.Hosts) > 0
}

type ClickHouseConfig struct {
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	Database           string `yaml:"database"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`             // Inline / ${ENV} password. Mutually exclusive with password_file.
	PasswordFile       string `yaml:"password_file"`        // Path to a file holding the password (read at load; one trailing newline trimmed). Keeps the secret out of the process environment.
	Secure             bool   `yaml:"secure"`               // Enable TLS connection (default: false)
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"` // Skip TLS certificate verification (default: false, INSECURE - use only for testing)
	CACert             string `yaml:"ca_cert"`              // Path to CA certificate file for TLS verification
	Cluster            string `yaml:"cluster"`              // Optional cluster name for cluster queries
	UseClusterQueries  bool   `yaml:"use_cluster_queries"`  // Use cluster() function for distributed queries (default: false)
	MaxOpenConns       int    `yaml:"max_open_conns"`       // Max open connections (default: 2)
	MaxIdleConns       int    `yaml:"max_idle_conns"`       // Max idle connections (default: 1)
	QueryTimeoutS      int    `yaml:"query_timeout_s"`      // Query timeout in seconds (default: 30)
	MaxMemoryUsage     int64  `yaml:"max_memory_usage"`     // Expert: per-query memory limit in bytes (default: 104857600 = 100MB)
}

type OTELConfig struct {
	CollectorAddress   string `yaml:"collector_address"`
	ServiceName        string `yaml:"service_name"`
	MaxQueryLength     int    `yaml:"max_query_length"`     // Max query length to export (default: 100000)
	Secure             bool   `yaml:"secure"`               // Enable TLS for gRPC connection (default: false)
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"` // Skip TLS certificate verification (default: false, INSECURE)
	CACert             string `yaml:"ca_cert"`              // Path to CA certificate file for TLS verification
	ClientCert         string `yaml:"client_cert"`          // Path to client certificate for mTLS
	ClientKey          string `yaml:"client_key"`           // Path to client key for mTLS
}

// CanaryConfig configures lightweight canary queries during degraded mode.
// When enabled, a cheap COUNT query runs while the circuit breaker is open
// or backoff is elevated, providing visibility into whether long queries
// are still occurring without the cost of the full two-step span fetch.
type CanaryConfig struct {
	Enabled             bool `yaml:"enabled"`               // Enable canary queries in degraded mode (default: false)
	ThresholdDurationMs int  `yaml:"threshold_duration_ms"` // Count queries exceeding this duration in ms (default: 60000)
}

// MetricsConfig configures the Prometheus metrics HTTP endpoint.
//
// Enabled gates BOTH listeners (scrape + admin) — see docs/observability.md
// for the rationale and deployment recipes.
type MetricsConfig struct {
	Enabled            bool              `yaml:"enabled"`              // Enable both listeners (default: false)
	ListenAddress      string            `yaml:"listen_address"`       // Scrape: /metrics + /health (default: ":9090")
	AdminListenAddress string            `yaml:"admin_listen_address"` // Admin: POST /flush (default: "127.0.0.1:9091")
	OTLP               OTLPMetricsConfig `yaml:"otlp"`                 // Optional OTLP push path for self-metrics
}

// OTLPMetricsConfig configures the optional OTLP push path for click-dog
// self-metrics. It is additive to the Prometheus scrape endpoint.
type OTLPMetricsConfig struct {
	Enabled               bool   `yaml:"enabled"`
	InheritOTELConnection bool   `yaml:"inherit_otel_connection"`
	CollectorAddress      string `yaml:"collector_address"`
	// Secure enables TLS for standalone metrics connections. For CA or mTLS
	// client certificates, use inherit_otel_connection with exporters.otel[0].
	Secure          bool   `yaml:"secure"`
	Host            string `yaml:"host"`
	ServiceName     string `yaml:"service_name"`
	IntervalSeconds int    `yaml:"interval_seconds"`
	// Rename maps canonical metric keys to emitted OTLP names. Keys are
	// validated even when OTLP metrics are disabled so typos fail early.
	Rename map[string]string `yaml:"rename"`
}

// HealthConfig configures the HTTP health endpoints (/healthz, /readyz, /status).
// Separate from MetricsConfig so operators can enable probes without exposing
// Prometheus metrics (and vice versa). When Enabled is false, no listener,
// goroutines, or handlers are created.
type HealthConfig struct {
	Enabled       bool                `yaml:"enabled"`        // Enable health endpoints (default: false)
	ListenAddress string              `yaml:"listen_address"` // Address to listen on (default: ":8686")
	Cluster       HealthClusterConfig `yaml:"cluster"`        // Aggregate /clusterz endpoint (phase 1 of #19)
}

// HealthClusterConfig configures the leader-only /clusterz aggregate health
// endpoint. Phase 1 (#19) ships with a static peer list; Keeper-based peer
// discovery is deferred to phase 2.
//
// Peers must be reachable from the leader at the same path/port that serves
// /readyz on each node — typically a routable host:port (not the bind-only
// ":8686"). Self is THIS node's entry in that list — it's the key under
// which the leader's in-process readyz appears in the /clusterz response,
// and it's also how the handler decides which peer to skip during HTTP
// fanout. listen_address is a bind expression (often ":8686") and cannot
// double as Self because it isn't routable.
type HealthClusterConfig struct {
	Enabled       bool     `yaml:"enabled"`         // Enable /clusterz on the leader (default: false)
	Self          string   `yaml:"self"`            // Routable host:port for THIS node, e.g. "node-0:8686" (required when enabled)
	PeerTimeoutMs int      `yaml:"peer_timeout_ms"` // Per-peer fanout deadline in ms (default: 3000)
	Peers         []string `yaml:"peers"`           // Static peer list, e.g. ["node-0:8686", "node-1:8686"]
}

// WebhookConfig configures outbound webhook notifications on selected events.
// Compatible with Slack incoming webhooks — sends JSON with a "text" field.
type WebhookConfig struct {
	Enabled  bool     `yaml:"enabled"`   // Enable webhook notifications (default: false)
	URL      string   `yaml:"url"`       // Webhook URL (e.g. Slack incoming webhook)
	TimeoutS int      `yaml:"timeout_s"` // HTTP timeout in seconds (default: 10)
	Events   []string `yaml:"events"`    // Events to notify on (default: all)
}

const (
	DefaultDatadogEventsTimeoutS = 10
	MaxDatadogEventsTimeoutS     = 60
)

// DatadogEventsConfig configures the separate Datadog Events API v2
// destination used only by explicit analysis notification runs. These
// credentials are independent of OTLP exporter configuration.
type DatadogEventsConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Site               string `yaml:"site"`
	APIKey             string `yaml:"api_key"`
	APIKeyFile         string `yaml:"api_key_file"`
	ApplicationKey     string `yaml:"application_key"`
	ApplicationKeyFile string `yaml:"application_key_file"`
	TimeoutS           int    `yaml:"timeout_s"`
	Environment        string `yaml:"environment"`
	Service            string `yaml:"service"`
}

type MonitorConfig struct {
	MinTraceDurationMs int  `yaml:"min_trace_duration_ms"` // Find traces with at least one span >= this duration (required)
	MinSpanDurationMs  int  `yaml:"min_span_duration_ms"`  // Only export spans >= this duration from those traces (0 = export all)
	MaxTraceDurationMs int  `yaml:"max_trace_duration_ms"` // Skip traces/queries with spans > this duration (0 = no limit)
	MaxSpanDurationMs  int  `yaml:"max_span_duration_ms"`  // Skip individual spans > this duration (0 = no limit)
	MaxQueryLength     int  `yaml:"max_query_length"`      // Skip queries with SQL text > this many characters (0/omitted loads as default 100000)
	CheckIntervalS     int  `yaml:"check_interval_s"`      // Scheduled poll interval in seconds (default: 30; must be > 0 when enabled)
	LookbackS          int  `yaml:"lookback_s"`            // How far back to look for queries (default: check_interval_s + lookback_buffer_s)
	LookbackBufferS    int  `yaml:"lookback_buffer_s"`     // Extra seconds added to check_interval when computing default lookback (default: 10)
	Enabled            bool `yaml:"enabled"`               // Enable scheduled monitoring (default: true)
	MaxSpansPerCycle   int  `yaml:"max_spans_per_cycle"`   // SQL LIMIT on the spans query per cycle (default: 1000; ceil: 100000). Partial traces continue next cycle via lookback overlap + dedup cache
	BatchSize          int  `yaml:"batch_size"`            // Process spans in batches of this size (0 = no batching)
	BatchDelayMs       int  `yaml:"batch_delay_ms"`        // Delay between batches in milliseconds (0 = no delay)
	ExportTimeoutS     int  `yaml:"export_timeout_s"`      // Per-call exporter timeout in seconds (default: 30s; 0 disables)
	DedupCacheSize     int  `yaml:"dedup_cache_size"`      // Size of LRU cache for deduplication (default: 10000)

	// Circuit breaker configuration - protects ClickHouse from being overwhelmed
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`

	// Backoff configuration - adaptive polling when errors occur
	Backoff BackoffConfig `yaml:"backoff"`

	// Canary configuration - lightweight queries during degraded mode
	Canary CanaryConfig `yaml:"canary"`

	// TopologyAudit configures the in-process self-audit that detects the
	// sidecar-fleet + use_cluster_queries anti-pattern (N× duplicate exports).
	// Observability-only: a hard no-op unless clickhouse.use_cluster_queries is
	// true. See docs/development/specs/topology-self-audit.md.
	TopologyAudit TopologyAuditConfig `yaml:"topology_audit"`

	// ExtractLogComment promotes JSON keys from ClickHouse log_comment
	// (found in http.url, http.target, or other URI-like attributes)
	// to top-level span attributes prefixed with "log_comment.". Defaults to true.
	ExtractLogComment *bool `yaml:"extract_log_comment"`

	// EnrichFromQueryLog queries system.query_log by query_id to enrich
	// exported spans with query metadata (user, client, tables, stats).
	// Only spans carrying the clickhouse.query_id attribute are enriched
	// (typically root query spans; child spans within a trace are not).
	// Defaults to true. Non-fatal: logs a warning if the query fails.
	EnrichFromQueryLog *bool `yaml:"enrich_from_query_log"`
}

// BackoffConfig configures adaptive backoff behavior
type BackoffConfig struct {
	Enabled       bool    `yaml:"enabled"`        // Enable adaptive backoff (set by installer / production profile configs)
	MaxIntervalS  int     `yaml:"max_interval_s"` // Maximum interval between retries in seconds (default: 300 = 5 min)
	BackoffFactor float64 `yaml:"backoff_factor"` // Multiply interval by this on each failure (default: 2.0)
}

// TopologyAuditConfig configures the topology self-audit (the sidecar +
// use_cluster_queries backstop detector). On by default, but the auditor only
// starts when clickhouse.use_cluster_queries is true — zero cost otherwise.
type TopologyAuditConfig struct {
	Enabled                 bool `yaml:"enabled"`                    // Enable the self-audit (default: true)
	IntervalS               int  `yaml:"interval_s"`                 // Audit tick interval in seconds (default: 300)
	DebounceCount           int  `yaml:"debounce_count"`             // Consecutive positive ticks before flipping to detected (default: 2)
	QueryLogLookbackMinutes int  `yaml:"query_log_lookback_minutes"` // Coarse query_log scan window in minutes (default: 15). The recency trigger — whether a host still counts as a live reader — is a shorter active window the auditor derives from check_interval_s, not this.
}

// CircuitBreakerConfig holds configuration for the circuit breaker
type CircuitBreakerConfig struct {
	Enabled          bool `yaml:"enabled"`
	FailureThreshold int  `yaml:"failure_threshold"` // Failures before opening (default: 3)
	SuccessThreshold int  `yaml:"success_threshold"` // Successes to close from half-open (default: 1)
	ResetTimeoutS    int  `yaml:"reset_timeout_s"`   // Seconds before trying again (default: 60)
}

// LeaderElectionConfig holds configuration for connecting to ClickHouse Keeper.
type LeaderElectionConfig struct {
	Hosts            []string `yaml:"hosts"`              // Keeper endpoints, e.g. ["localhost:9181"]
	Secure           bool     `yaml:"secure"`             // Enable TLS for Keeper's ZooKeeper-compatible connection (default: false)
	SessionTimeout   int      `yaml:"session_timeout_s"`  // Session timeout in seconds (default: 10)
	BasePath         string   `yaml:"base_path"`          // Znode path prefix (default: /click-dog/election)
	AuthUser         string   `yaml:"auth_user"`          // Optional digest auth username
	AuthPassword     string   `yaml:"auth_password"`      // Inline / ${ENV} digest auth password. Mutually exclusive with auth_password_file.
	AuthPasswordFile string   `yaml:"auth_password_file"` // Path to a file holding the digest auth password (read at load; one trailing newline trimmed).
}

// SplunkHECConfig holds configuration for a Splunk HEC exporter.
type SplunkHECConfig struct {
	Endpoint           string `yaml:"endpoint"`             // e.g. https://splunk:8088
	Token              string `yaml:"token"`                // Inline / ${ENV} HEC token. Mutually exclusive with token_file.
	TokenFile          string `yaml:"token_file"`           // Path to a file holding the HEC token (read at load; one trailing newline trimmed).
	Index              string `yaml:"index"`                // Target index (optional)
	Source             string `yaml:"source"`               // Event source (default: click-dog)
	SourceType         string `yaml:"source_type"`          // Event source type (default: _json)
	AllowInsecureHTTP  bool   `yaml:"allow_insecure_http"`  // Allow http:// HEC endpoint for local/dev/test environments
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"` // Skip TLS verification
	MaxQueryLength     int    `yaml:"max_query_length"`     // Truncate SQL in events (default: 100000)
}

// RedactionRule defines a regex-based redaction rule for SQL query text.
// Matched portions are replaced before export.
type RedactionRule struct {
	Pattern     string `yaml:"pattern"`     // Regex pattern to match (required)
	Replacement string `yaml:"replacement"` // Replacement text (default: "[REDACTED]")
}

// QueryTextMode controls which representation of SQL query text may cross the
// final export boundary. The empty value is treated as raw for Config values
// constructed directly in tests; LoadConfig always resolves it explicitly.
type QueryTextMode string

const (
	QueryTextModeRaw            QueryTextMode = "raw"
	QueryTextModeRedacted       QueryTextMode = "redacted"
	QueryTextModeNormalizedOnly QueryTextMode = "normalized_only"
	QueryTextModeNone           QueryTextMode = "none"
)

type FiltersConfig struct {
	QueryTextMode       QueryTextMode   `yaml:"query_text_mode"`      // Exported SQL representation: raw, redacted, normalized_only, or none (default: raw)
	WhitelistOperations []string        `yaml:"whitelist_operations"` // Operation names to include, supports * wildcard (e.g., "DB::Interpreter*::execute()")
	BlacklistOperations []string        `yaml:"blacklist_operations"` // Operation name substrings to exclude at SQL level (e.g., "MergeTreeIndex", "VFSWrite"). Uses LIKE '%value%' matching.
	WhitelistIPs        []string        `yaml:"whitelist_ips"`        // IP addresses to include (if set, only these are allowed)
	WhitelistUsers      []string        `yaml:"whitelist_users"`      // ClickHouse users to include (if set, only spans from these users are exported). Matched against query_log.user on enriched spans; spans without a determinable user are dropped when a whitelist is active.
	BlacklistUsers      []string        `yaml:"blacklist_users"`      // ClickHouse users to exclude. Defense-in-depth alongside CH user-level grants — see docs/filtering.md.
	BlacklistQueries    []string        `yaml:"blacklist_queries"`    // Regex patterns to exclude from query text
	RedactQueries       []RedactionRule `yaml:"redact_queries"`       // Regex-based redaction rules applied to query text before export
}

// EffectiveQueryTextMode returns the privacy mode after applying the
// compatibility default used by directly constructed Config values.
func (f FiltersConfig) EffectiveQueryTextMode() QueryTextMode {
	if f.QueryTextMode == "" {
		if len(f.RedactQueries) > 0 {
			return QueryTextModeRedacted
		}
		return QueryTextModeRaw
	}
	return f.QueryTextMode
}

// DefaultConfigPath is the conventional system-install config location.
const DefaultConfigPath = "/etc/click-dog/click-dog.yaml"

// DefaultConfigFlag is the sentinel flag default used by all subcommands.
// ResolveConfigPath uses this to detect "user didn't pass --config".
const DefaultConfigFlag = "click-dog.yaml"

// ResolveConfigPath returns the config path to use. If the user passed a
// non-default value, it's used as-is. Otherwise it falls back to the system
// install path, then the local file.
func ResolveConfigPath(userPath string) (string, error) {
	return resolveConfigPath(userPath, DefaultConfigPath, DefaultConfigFlag)
}

// resolveConfigPath is the testable core of ResolveConfigPath; systemPath and
// localPath are parameters so the fallback scan can be exercised without
// touching /etc.
//
// A stat that fails with anything other than "does not exist" — most commonly
// permission denied, when /etc/click-dog is locked to the service user
// (drwxr-x--- click-dog) and the command is run as someone else — is reported
// distinctly. Collapsing it into "no config file found" sends operators
// hunting for a missing file when the real fix is to run with sudo or as the
// service user.
func resolveConfigPath(userPath, systemPath, localPath string) (string, error) {
	// User explicitly set something other than the default — use it as-is
	if userPath != "" && userPath != DefaultConfigFlag {
		if _, err := os.Stat(userPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("config file not found: %s", userPath)
			}
			return "", configAccessError(userPath, err)
		}
		return userPath, nil
	}

	// Default flag value: try system path first, then local. A readable
	// candidate always wins; we only surface a permission error if nothing is
	// usable, so a locked /etc/click-dog doesn't mask a readable ./click-dog.yaml.
	var accessErr error
	for _, candidate := range []string{systemPath, localPath} {
		_, err := os.Stat(candidate)
		if err == nil {
			return candidate, nil
		}
		if accessErr == nil && !errors.Is(err, os.ErrNotExist) {
			// The path is there but unreadable (e.g. /etc/click-dog locked to
			// the service user) — say so rather than claim it's missing.
			accessErr = configAccessError(candidate, err)
		}
	}
	if accessErr != nil {
		return "", accessErr
	}

	return "", fmt.Errorf("no config file found at %s or ./%s — pass --config <path> to specify", systemPath, localPath)
}

// configAccessError describes a config path that exists but can't be read,
// with the same remedy hint for both the explicit-path and default-scan cases.
func configAccessError(path string, err error) error {
	return fmt.Errorf("cannot access config at %s: %w — it exists but isn't readable as this user; run with sudo or as the click-dog service user (e.g. sudo -u click-dog click-dog ...)", path, err)
}

// LoadConfig loads configuration from the given path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// First pass: unmarshal into raw map to detect deprecated fields.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	deprecationWarnings := checkDeprecations(raw)
	envWarnings := unresolvedEnvRefs(data)

	// ha.enabled was removed outright (deployment-topology spec): election is
	// driven solely by the presence of ha.keeper.hosts. The strict decoder would
	// reject the key with a generic "field enabled not found", which reads like
	// a typo — catch it here with an actionable upgrade message instead.
	if lookupPath(raw, "ha.enabled") {
		return nil, fmt.Errorf("ha.enabled has been removed: leader election is enabled by setting ha.keeper.hosts — delete the ha.enabled key")
	}

	// Track explicit-set log_rotation keys so the defaults below only fire
	// when the user omitted the key entirely. Without this, a user-set
	// `max_size_mb: 0` would be silently coerced to 100 and bypass the
	// `> 0` validator below — leaving the writer's `maxBytes > 0` rotation
	// guard reachable as zero only via direct InitLogger calls.
	logRotationMaxSizeSet := lookupPath(raw, "log_rotation.max_size_mb")
	logRotationMaxFilesSet := lookupPath(raw, "log_rotation.max_files")
	exportTimeoutSet := lookupPath(raw, "monitor.export_timeout_s")
	monitorEnabledSet := lookupPath(raw, "monitor.enabled")
	checkIntervalSet := lookupPath(raw, "monitor.check_interval_s")
	clickHousePortSet := lookupPath(raw, "clickhouse.port")
	topologyAuditEnabledSet := lookupPath(raw, "monitor.topology_audit.enabled")
	otlpMetricsInheritSet := lookupPath(raw, "metrics.otlp.inherit_otel_connection")
	otlpMetricsIntervalSet := lookupPath(raw, "metrics.otlp.interval_seconds")
	queryTextModeSet := lookupPath(raw, "filters.query_text_mode")

	// Strict decode: reject unknown keys so a typo (e.g. check_intervla_s) — or
	// the removed legacy top-level otel: key — fails fast at load instead of
	// being silently ignored.
	var config Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	if queryTextModeSet && strings.TrimSpace(string(config.Filters.QueryTextMode)) == "" {
		return nil, errors.New("filters.query_text_mode must be one of: raw, redacted, normalized_only, none; got an empty value")
	}
	config.DeprecationWarnings = deprecationWarnings
	config.EnvWarnings = envWarnings

	// Expand environment variables in sensitive fields
	// Supports ${VAR} and $VAR syntax in config values
	config.ClickHouse.Password = expandEnvVars(config.ClickHouse.Password)
	config.ClickHouse.Username = expandEnvVars(config.ClickHouse.Username)
	config.ClickHouse.Host = expandEnvVars(config.ClickHouse.Host)
	config.ClickHouse.CACert = expandEnvVars(config.ClickHouse.CACert)
	config.HA.Keeper.AuthUser = expandEnvVars(config.HA.Keeper.AuthUser)
	config.HA.Keeper.AuthPassword = expandEnvVars(config.HA.Keeper.AuthPassword)

	// Expand env vars in exporters config
	for i := range config.Exporters.OTEL {
		config.Exporters.OTEL[i].CollectorAddress = expandEnvVars(config.Exporters.OTEL[i].CollectorAddress)
		config.Exporters.OTEL[i].ServiceName = expandEnvVars(config.Exporters.OTEL[i].ServiceName)
		config.Exporters.OTEL[i].CACert = expandEnvVars(config.Exporters.OTEL[i].CACert)
		config.Exporters.OTEL[i].ClientCert = expandEnvVars(config.Exporters.OTEL[i].ClientCert)
		config.Exporters.OTEL[i].ClientKey = expandEnvVars(config.Exporters.OTEL[i].ClientKey)
	}
	for i := range config.Exporters.SplunkHEC {
		config.Exporters.SplunkHEC[i].Endpoint = expandEnvVars(config.Exporters.SplunkHEC[i].Endpoint)
		config.Exporters.SplunkHEC[i].Token = expandEnvVars(config.Exporters.SplunkHEC[i].Token)
	}
	config.Metrics.OTLP.CollectorAddress = expandEnvVars(config.Metrics.OTLP.CollectorAddress)
	config.Metrics.OTLP.Host = expandEnvVars(config.Metrics.OTLP.Host)
	config.Metrics.OTLP.ServiceName = expandEnvVars(config.Metrics.OTLP.ServiceName)
	config.DatadogEvents.Site = expandEnvVars(config.DatadogEvents.Site)
	config.DatadogEvents.APIKey = expandEnvVars(config.DatadogEvents.APIKey)
	config.DatadogEvents.ApplicationKey = expandEnvVars(config.DatadogEvents.ApplicationKey)
	config.DatadogEvents.Environment = expandEnvVars(config.DatadogEvents.Environment)
	config.DatadogEvents.Service = expandEnvVars(config.DatadogEvents.Service)

	// Resolve *_file secrets after inline env expansion. resolveSecret receives
	// the RAW *_file field and expands the path itself (the path may be
	// ${ENV}-templated), so a non-empty key that expands to "" fails closed
	// rather than being mistaken for an absent file. A file value wins over an
	// (empty) inline default. Any error here is fatal — fail closed rather than
	// start with a missing or ambiguous credential.
	if config.ClickHouse.Password, err = resolveSecret("clickhouse.password",
		config.ClickHouse.Password, config.ClickHouse.PasswordFile); err != nil {
		return nil, err
	}
	if config.HA.Keeper.AuthPassword, err = resolveSecret("ha.keeper.auth_password",
		config.HA.Keeper.AuthPassword, config.HA.Keeper.AuthPasswordFile); err != nil {
		return nil, err
	}
	for i := range config.Exporters.SplunkHEC {
		if config.Exporters.SplunkHEC[i].Token, err = resolveSecret(
			fmt.Sprintf("exporters.splunk_hec[%d].token", i),
			config.Exporters.SplunkHEC[i].Token,
			config.Exporters.SplunkHEC[i].TokenFile); err != nil {
			return nil, err
		}
	}
	if config.DatadogEvents.APIKey, err = resolveSecret("datadog_events.api_key",
		config.DatadogEvents.APIKey, config.DatadogEvents.APIKeyFile); err != nil {
		return nil, err
	}
	if config.DatadogEvents.ApplicationKey, err = resolveSecret("datadog_events.application_key",
		config.DatadogEvents.ApplicationKey, config.DatadogEvents.ApplicationKeyFile); err != nil {
		return nil, err
	}

	// Set defaults
	// monitor.enabled defaults to true — scheduled monitoring is the primary
	// mode. The bool's zero value is false, so without this an omitted key
	// would silently disable scheduled mode despite the documented default.
	// An explicit `enabled: false` is preserved (backfill-only deployments).
	if !monitorEnabledSet {
		config.Monitor.Enabled = true
	}
	// check_interval_s drives the scheduled-mode ticker. Default it when the
	// key is omitted so a bare config doesn't leave a zero interval, which
	// time.NewTicker rejects with a panic. An explicit `check_interval_s: 0`
	// is preserved and rejected by Validate when scheduled mode is enabled.
	if !checkIntervalSet {
		config.Monitor.CheckIntervalS = 30
	}
	if config.Monitor.LookbackBufferS == 0 {
		config.Monitor.LookbackBufferS = 10 // 10s overlap absorbs clock skew & long poll cycles
	}
	if config.Monitor.LookbackS == 0 {
		config.Monitor.LookbackS = config.Monitor.CheckIntervalS + config.Monitor.LookbackBufferS
	}
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}
	if config.LogFormat == "" {
		config.LogFormat = "text"
	}
	if !clickHousePortSet {
		config.ClickHouse.Port = 9000
	}
	if config.ClickHouse.MaxOpenConns == 0 {
		config.ClickHouse.MaxOpenConns = 2
	}
	if config.ClickHouse.MaxIdleConns == 0 {
		config.ClickHouse.MaxIdleConns = 1
	}
	if config.ClickHouse.QueryTimeoutS == 0 {
		config.ClickHouse.QueryTimeoutS = 30
	}
	if config.ClickHouse.MaxMemoryUsage == 0 {
		config.ClickHouse.MaxMemoryUsage = 104857600 // 100MB
	}
	if config.Monitor.MaxQueryLength == 0 {
		config.Monitor.MaxQueryLength = 100000
	}
	if config.Monitor.ExportTimeoutS == 0 && !exportTimeoutSet {
		config.Monitor.ExportTimeoutS = DefaultExportTimeoutS
	}
	// Migrate legacy redact_queries configs without risking a silent privacy
	// regression during upgrade. New configs default to raw; an older config
	// that has redaction rules but no query_text_mode is promoted to the new,
	// fail-closed redacted mode. Unlike the legacy implementation, unmatched
	// statements are omitted, so the warning calls out that stricter behavior.
	if !queryTextModeSet {
		if len(config.Filters.RedactQueries) > 0 {
			config.Filters.QueryTextMode = QueryTextModeRedacted
			config.ValidationWarnings = append(config.ValidationWarnings,
				"filters.redact_queries is configured without filters.query_text_mode; selecting fail-closed query_text_mode: redacted for migration safety. Unlike legacy redaction, statements that match no rule are now omitted. Set the mode explicitly to complete the migration")
		} else {
			config.Filters.QueryTextMode = QueryTextModeRaw
		}
	}

	// Set defaults for exporters
	for i := range config.Exporters.OTEL {
		if config.Exporters.OTEL[i].ServiceName == "" {
			config.Exporters.OTEL[i].ServiceName = "click-dog-monitor"
		}
		if config.Exporters.OTEL[i].MaxQueryLength == 0 {
			config.Exporters.OTEL[i].MaxQueryLength = 100000
		}
	}
	for i := range config.Exporters.SplunkHEC {
		if config.Exporters.SplunkHEC[i].MaxQueryLength == 0 {
			config.Exporters.SplunkHEC[i].MaxQueryLength = 100000
		}
	}

	if config.Metrics.OTLP.Enabled && !otlpMetricsInheritSet {
		config.Metrics.OTLP.InheritOTELConnection = true
	}
	if !otlpMetricsIntervalSet {
		config.Metrics.OTLP.IntervalSeconds = 10
	}
	if config.Metrics.OTLP.Enabled && config.Metrics.OTLP.Host == "" {
		if host, hostErr := os.Hostname(); hostErr == nil {
			config.Metrics.OTLP.Host = host
		}
	}
	if config.Metrics.OTLP.ServiceName == "" {
		if len(config.Exporters.OTEL) > 0 && config.Exporters.OTEL[0].ServiceName != "" {
			config.Metrics.OTLP.ServiceName = config.Exporters.OTEL[0].ServiceName
		} else {
			config.Metrics.OTLP.ServiceName = "click-dog-monitor"
		}
	}

	// Safer default for max spans per cycle - protects backend from cost explosions.
	// The maximum is MaxSpansPerCycleCeiling; the spans query caps the SQL LIMIT there.
	if config.Monitor.MaxSpansPerCycle == 0 {
		config.Monitor.MaxSpansPerCycle = 1000
	}

	// Default deduplication cache size
	if config.Monitor.DedupCacheSize == 0 {
		config.Monitor.DedupCacheSize = 10000
	}

	// Circuit breaker defaults
	if config.Monitor.CircuitBreaker.FailureThreshold == 0 {
		config.Monitor.CircuitBreaker.FailureThreshold = 3
	}
	if config.Monitor.CircuitBreaker.SuccessThreshold == 0 {
		config.Monitor.CircuitBreaker.SuccessThreshold = 1
	}
	if config.Monitor.CircuitBreaker.ResetTimeoutS == 0 {
		config.Monitor.CircuitBreaker.ResetTimeoutS = 60
	}

	// Backoff defaults for interval and growth factor. Enabled is explicit.
	if config.Monitor.Backoff.MaxIntervalS == 0 {
		config.Monitor.Backoff.MaxIntervalS = 300 // 5 minutes max
	}
	if config.Monitor.Backoff.BackoffFactor == 0 {
		config.Monitor.Backoff.BackoffFactor = 2.0
	}

	// Canary defaults
	if config.Monitor.Canary.ThresholdDurationMs == 0 {
		config.Monitor.Canary.ThresholdDurationMs = 60000 // 60s
	}

	// Topology audit defaults. Enabled defaults to true — the bool's zero
	// value is false, so without the lookupPath check an omitted key would
	// silently disable the backstop. An explicit `enabled: false` is preserved.
	// The auditor is still a no-op unless use_cluster_queries is true.
	if !topologyAuditEnabledSet {
		config.Monitor.TopologyAudit.Enabled = true
	}
	if config.Monitor.TopologyAudit.IntervalS == 0 {
		config.Monitor.TopologyAudit.IntervalS = 300 // 5 min
	}
	if config.Monitor.TopologyAudit.DebounceCount == 0 {
		config.Monitor.TopologyAudit.DebounceCount = 2
	}
	if config.Monitor.TopologyAudit.QueryLogLookbackMinutes == 0 {
		config.Monitor.TopologyAudit.QueryLogLookbackMinutes = 15
	}

	// Log rotation defaults — only fire when the key is absent from YAML.
	// An explicit `max_size_mb: 0` (or `max_files: 0`) falls through to
	// validation, which rejects it as a config error.
	if !logRotationMaxSizeSet {
		config.LogRotation.MaxSizeMB = 100
	}
	if !logRotationMaxFilesSet {
		config.LogRotation.MaxFiles = 3
	}

	// Metrics defaults
	if config.Metrics.ListenAddress == "" {
		config.Metrics.ListenAddress = ":9090"
	}
	if config.Metrics.AdminListenAddress == "" {
		config.Metrics.AdminListenAddress = "127.0.0.1:9091"
	}

	// Health defaults — only applied when the server is enabled so that
	// -validate output (and any future struct dump) doesn't print a port
	// that will never be opened.
	if config.Health.Enabled && config.Health.ListenAddress == "" {
		config.Health.ListenAddress = ":8686"
	}
	// Cluster fanout defaults — only meaningful when health.cluster.enabled. The
	// 3s default matches the stricter end of typical Kubernetes per-probe
	// budgets while still tolerating one or two slow peers per fanout.
	if config.Health.Cluster.Enabled && config.Health.Cluster.PeerTimeoutMs == 0 {
		config.Health.Cluster.PeerTimeoutMs = 3000
	}
	// Normalize cluster addresses: drop surrounding whitespace so the
	// runtime self-skip (`p == cs.self`) matches even when the YAML value
	// was written as "  node-0:8686  ". Validation already rejects
	// whitespace-only entries; this canonicalises the populated ones so
	// the trimmed form is what propagates into Validate(), Source.Peers(),
	// and the fanout. Run unconditionally — the cost is negligible and
	// it future-proofs against `health.cluster.enabled` toggling at runtime.
	config.Health.Cluster.Self = strings.TrimSpace(config.Health.Cluster.Self)
	for i, p := range config.Health.Cluster.Peers {
		config.Health.Cluster.Peers[i] = strings.TrimSpace(p)
	}

	// Webhook defaults
	config.Webhook.URL = expandEnvVars(config.Webhook.URL)
	if config.Webhook.TimeoutS == 0 {
		config.Webhook.TimeoutS = 10
	}
	config.DatadogEvents.Site = strings.ToLower(strings.TrimSpace(config.DatadogEvents.Site))
	config.DatadogEvents.Environment = strings.TrimSpace(config.DatadogEvents.Environment)
	config.DatadogEvents.Service = strings.TrimSpace(config.DatadogEvents.Service)
	if config.DatadogEvents.TimeoutS == 0 {
		config.DatadogEvents.TimeoutS = DefaultDatadogEventsTimeoutS
	}

	// HA defaults — applied when Keeper hosts are configured (which is what
	// turns election on; see HAConfig.Active).
	if config.HA.Active() {
		if config.HA.Keeper.SessionTimeout == 0 {
			config.HA.Keeper.SessionTimeout = 10
		}
		if config.HA.Keeper.BasePath == "" {
			config.HA.Keeper.BasePath = "/click-dog/election"
		}
	}

	// Non-fatal advisories computed once defaults are settled (they read the
	// resolved config). Surfaced as WARN by main/analyze.
	config.ValidationWarnings = append(config.ValidationWarnings, config.validationWarnings()...)

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := config.validateTLSFiles(); err != nil {
		return nil, err
	}

	return &config, nil
}

// topologyAuditWarnings returns non-fatal advisories about topology-audit knob
// combinations that load and run but quietly degrade detection. Unlike the hard
// errors in Validate, these don't block startup — main/analyze surface them as
// WARN at load so an operator can see (and fix) a self-defeating config.
func (c *Config) topologyAuditWarnings() []string {
	// The auditor only starts when use_cluster_queries is on (no-op otherwise),
	// so a cadence advisory is pure noise on the common single-instance setup.
	if !c.Monitor.TopologyAudit.Enabled || !c.ClickHouse.UseClusterQueries {
		return nil
	}
	check, interval := c.Monitor.CheckIntervalS, c.Monitor.TopologyAudit.IntervalS
	if check <= 0 || interval <= 0 {
		return nil // the hard validators (or missing inputs) cover these
	}
	// The auditor caps its recency window — how fresh a host's last whole-cluster
	// read must be to still count it a live reader — at interval_s/2. Once that
	// cap falls below the poll cadence (check_interval_s > interval_s/2), a
	// genuinely-concurrent reader, which only reads every check_interval_s, can
	// sit outside the window between audit ticks: missed detection, or a warning
	// that flaps as reads drift in and out. interval_s ≥ 6×check_interval_s keeps
	// the full 3×-cadence window the auditor was designed around.
	if 2*check > interval {
		return []string{fmt.Sprintf(
			"monitor.topology_audit.interval_s=%d with monitor.check_interval_s=%d caps the detection window at interval_s/2 (%ds), below the %ds poll cadence — a concurrent cross-cluster reader may go undetected or the topology warning may flap; set interval_s ≥ %d (6×check_interval_s)",
			interval, check, interval/2, check, 6*check)}
	}
	return nil
}

func (c *Config) validationWarnings() []string {
	var warnings []string
	warnings = append(warnings, c.topologyAuditWarnings()...)
	warnings = append(warnings, c.keeperSecurityWarnings()...)
	warnings = append(warnings, c.tlsMaterialWarnings()...)
	warnings = append(warnings, c.queryTextModeWarnings()...)
	return warnings
}

func (c *Config) queryTextModeWarnings() []string {
	mode := c.Filters.EffectiveQueryTextMode()
	var warnings []string
	if mode == QueryTextModeNormalizedOnly && !c.Monitor.ShouldEnrichFromQueryLog() {
		warnings = append(warnings, "filters.query_text_mode is normalized_only while monitor.enrich_from_query_log is false; scheduled/native spans cannot receive query_log normalized previews and will omit query text unless another explicitly normalized span attribute is present")
	}
	if (mode == QueryTextModeNormalizedOnly || mode == QueryTextModeNone) && len(c.Filters.RedactQueries) > 0 {
		warnings = append(warnings, fmt.Sprintf("filters.redact_queries is ignored when filters.query_text_mode is %s; remove the inert rules after confirming the stricter query-text policy", mode))
	}
	return warnings
}

// tlsMaterialWarnings reports certificate settings that are accepted but have
// no effect because TLS is disabled. An incomplete OTEL client certificate
// pair is deliberately omitted here because Validate reports that incoherent
// configuration as a hard error instead.
func (c *Config) tlsMaterialWarnings() []string {
	var warnings []string
	if !c.ClickHouse.Secure && c.ClickHouse.CACert != "" {
		warnings = append(warnings, "clickhouse.ca_cert is configured but clickhouse.secure is false; the CA certificate will be ignored. Set clickhouse.secure: true to use it, or remove clickhouse.ca_cert")
	}
	for i, o := range c.Exporters.OTEL {
		if o.Secure {
			continue
		}
		if o.CACert != "" {
			warnings = append(warnings, fmt.Sprintf("exporters.otel[%d].ca_cert is configured but exporters.otel[%d].secure is false; the CA certificate will be ignored. Set secure: true for this exporter to use it, or remove ca_cert", i, i))
		}
		if o.ClientCert != "" && o.ClientKey != "" {
			warnings = append(warnings, fmt.Sprintf("exporters.otel[%d].client_cert and client_key are configured but exporters.otel[%d].secure is false; the mTLS client certificate will be ignored. Set secure: true for this exporter to use it, or remove client_cert and client_key", i, i))
		}
	}
	return warnings
}

// validateTLSFiles verifies that active TLS file references can be read during
// config loading, including offline -validate mode. Keep this separate from
// Validate so callers can validate an in-memory Config without depending on
// the local filesystem. Certificate parsing remains the responsibility of the
// TLS constructors, which already provide format-specific errors.
func (c *Config) validateTLSFiles() error {
	var errs []string
	check := func(label, path string) {
		if path == "" {
			return
		}
		if _, err := os.ReadFile(path); err != nil {
			errs = append(errs, fmt.Sprintf("%s %q cannot be read: %v; fix the path or file permissions", label, path, err))
		}
	}

	if c.ClickHouse.Secure {
		check("clickhouse.ca_cert", c.ClickHouse.CACert)
	}
	for i, o := range c.Exporters.OTEL {
		if !o.Secure {
			continue
		}
		check(fmt.Sprintf("exporters.otel[%d].ca_cert", i), o.CACert)
		check(fmt.Sprintf("exporters.otel[%d].client_cert", i), o.ClientCert)
		check(fmt.Sprintf("exporters.otel[%d].client_key", i), o.ClientKey)
	}

	if len(errs) > 0 {
		return fmt.Errorf("configuration TLS file validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func (c *Config) keeperSecurityWarnings() []string {
	if !c.HA.Active() {
		return nil
	}
	if c.HA.Keeper.AuthUser == "" {
		return []string{
			"ha.keeper.hosts is configured without ha.keeper.auth_user; Keeper election znodes will use world ACLs. Configure Keeper digest auth so only authenticated click-dog instances can create/delete election nodes.",
		}
	}
	if !c.HA.Keeper.Secure {
		return []string{
			"ha.keeper.auth_user is configured but ha.keeper.secure is false; Keeper digest auth is sent over a plaintext connection. Set ha.keeper.secure: true when Keeper supports TLS.",
		}
	}
	return nil
}

// Validate checks configuration values for validity
// Returns an error if any configuration is invalid
func (c *Config) Validate() error {
	var errs []string

	// ClickHouse validation
	if c.ClickHouse.Port <= 0 || c.ClickHouse.Port > 65535 {
		errs = append(errs, fmt.Sprintf("clickhouse.port must be between 1 and 65535, got %d", c.ClickHouse.Port))
	}
	if c.ClickHouse.MaxOpenConns < 0 {
		errs = append(errs, fmt.Sprintf("clickhouse.max_open_conns cannot be negative, got %d", c.ClickHouse.MaxOpenConns))
	}
	if c.ClickHouse.MaxIdleConns < 0 {
		errs = append(errs, fmt.Sprintf("clickhouse.max_idle_conns cannot be negative, got %d", c.ClickHouse.MaxIdleConns))
	}
	if c.ClickHouse.QueryTimeoutS < 0 {
		errs = append(errs, fmt.Sprintf("clickhouse.query_timeout_s cannot be negative, got %d", c.ClickHouse.QueryTimeoutS))
	}
	if c.ClickHouse.MaxMemoryUsage < 0 {
		errs = append(errs, fmt.Sprintf("clickhouse.max_memory_usage cannot be negative, got %d", c.ClickHouse.MaxMemoryUsage))
	}
	// Cluster name is interpolated into SQL (cluster('name', table)); restrict
	// to safe identifier characters to structurally prevent SQL injection.
	if c.ClickHouse.Cluster != "" && !safeIdentifierPattern.MatchString(c.ClickHouse.Cluster) {
		errs = append(errs, fmt.Sprintf("clickhouse.cluster must start with an ASCII letter and then contain only letters, digits, hyphens, or underscores, got %q", c.ClickHouse.Cluster))
	}
	// use_cluster_queries fans reads across the cluster via cluster('name', …);
	// without a cluster name buildTableRef silently falls back to the local
	// table, so a leader-gated deployment would export only its own node and
	// drop every standby node's spans. Require the name explicitly.
	if c.ClickHouse.UseClusterQueries && c.ClickHouse.Cluster == "" {
		errs = append(errs, "clickhouse.cluster is required when use_cluster_queries is true")
	}

	// Monitor validation
	if c.Monitor.MinTraceDurationMs < 0 {
		errs = append(errs, fmt.Sprintf("monitor.min_trace_duration_ms cannot be negative, got %d", c.Monitor.MinTraceDurationMs))
	}
	if c.Monitor.MinSpanDurationMs < 0 {
		errs = append(errs, fmt.Sprintf("monitor.min_span_duration_ms cannot be negative, got %d", c.Monitor.MinSpanDurationMs))
	}
	if c.Monitor.MaxTraceDurationMs < 0 {
		errs = append(errs, fmt.Sprintf("monitor.max_trace_duration_ms cannot be negative, got %d", c.Monitor.MaxTraceDurationMs))
	}
	if c.Monitor.MaxSpanDurationMs < 0 {
		errs = append(errs, fmt.Sprintf("monitor.max_span_duration_ms cannot be negative, got %d", c.Monitor.MaxSpanDurationMs))
	}
	if c.Monitor.CheckIntervalS < 0 {
		errs = append(errs, fmt.Sprintf("monitor.check_interval_s cannot be negative, got %d", c.Monitor.CheckIntervalS))
	}
	// Scheduled mode feeds check_interval_s into time.NewTicker, which panics
	// on a non-positive duration. A zero value is only reachable via an
	// explicit `check_interval_s: 0` (omission defaults to 30 in LoadConfig),
	// so reject it up front rather than crash on the first poll.
	if c.Monitor.Enabled && c.Monitor.CheckIntervalS == 0 {
		errs = append(errs, "monitor.check_interval_s must be greater than 0 when monitor.enabled is true (it is the scheduled poll interval)")
	}
	// min_trace_duration_ms is the primary trace filter and is documented as
	// required. A zero value (omitted, or explicit 0) matches every trace,
	// which is a large and usually unintended ingest-cost jump — so require a
	// positive value when scheduled mode is enabled. Backfill runs bound their
	// own volume via the explicit time range, so this is gated on enabled.
	if c.Monitor.Enabled && c.Monitor.MinTraceDurationMs == 0 {
		errs = append(errs, "monitor.min_trace_duration_ms must be greater than 0 when monitor.enabled is true (it is the trace duration filter; 0 would match every trace)")
	}
	if c.Monitor.LookbackS < 0 {
		errs = append(errs, fmt.Sprintf("monitor.lookback_s cannot be negative, got %d", c.Monitor.LookbackS))
	}
	if c.Monitor.LookbackBufferS < 0 {
		errs = append(errs, fmt.Sprintf("monitor.lookback_buffer_s cannot be negative, got %d", c.Monitor.LookbackBufferS))
	}
	if c.Monitor.MaxSpansPerCycle < 0 {
		errs = append(errs, fmt.Sprintf("monitor.max_spans_per_cycle cannot be negative, got %d", c.Monitor.MaxSpansPerCycle))
	}
	if c.Monitor.MaxSpansPerCycle > MaxSpansPerCycleCeiling {
		errs = append(errs, fmt.Sprintf("monitor.max_spans_per_cycle cannot exceed %d (the spans query caps the per-cycle SQL LIMIT at this value), got %d", MaxSpansPerCycleCeiling, c.Monitor.MaxSpansPerCycle))
	}
	if c.Monitor.BatchSize < 0 {
		errs = append(errs, fmt.Sprintf("monitor.batch_size cannot be negative, got %d", c.Monitor.BatchSize))
	}
	if c.Monitor.BatchDelayMs < 0 {
		errs = append(errs, fmt.Sprintf("monitor.batch_delay_ms cannot be negative, got %d", c.Monitor.BatchDelayMs))
	}
	if c.Monitor.ExportTimeoutS < 0 {
		errs = append(errs, fmt.Sprintf("monitor.export_timeout_s cannot be negative, got %d", c.Monitor.ExportTimeoutS))
	}
	if c.Monitor.DedupCacheSize < 0 {
		errs = append(errs, fmt.Sprintf("monitor.dedup_cache_size cannot be negative, got %d", c.Monitor.DedupCacheSize))
	}

	if c.Metrics.OTLP.Enabled {
		if c.Metrics.OTLP.IntervalSeconds <= 0 {
			errs = append(errs, fmt.Sprintf("metrics.otlp.interval_seconds must be > 0 when OTLP metrics are enabled, got %d", c.Metrics.OTLP.IntervalSeconds))
		}
		if c.Metrics.OTLP.InheritOTELConnection && len(c.Exporters.OTEL) == 0 {
			errs = append(errs, "metrics.otlp.inherit_otel_connection requires at least one exporters.otel entry")
		}
		if !c.Metrics.OTLP.InheritOTELConnection && strings.TrimSpace(c.Metrics.OTLP.CollectorAddress) == "" {
			errs = append(errs, "metrics.otlp.collector_address is required when inherit_otel_connection is false")
		}
	}
	renameTargets := make(map[string]string, len(c.Metrics.OTLP.Rename))
	knownMetricKeys := knownOTLPMetricKeys()
	for key, target := range c.Metrics.OTLP.Rename {
		if _, ok := knownMetricKeys[key]; !ok {
			errs = append(errs, fmt.Sprintf("metrics.otlp.rename.%s: unknown canonical metric key", key))
			continue
		}
		target = strings.TrimSpace(target)
		if target == "" {
			errs = append(errs, fmt.Sprintf("metrics.otlp.rename.%s must not be empty", key))
			continue
		}
		if prior, dup := renameTargets[target]; dup {
			errs = append(errs, fmt.Sprintf("metrics.otlp.rename maps both %s and %s to %q", prior, key, target))
			continue
		}
		renameTargets[target] = key
	}

	// Circuit breaker validation
	if c.Monitor.CircuitBreaker.FailureThreshold < 0 {
		errs = append(errs, fmt.Sprintf("monitor.circuit_breaker.failure_threshold cannot be negative, got %d", c.Monitor.CircuitBreaker.FailureThreshold))
	}
	if c.Monitor.CircuitBreaker.SuccessThreshold < 0 {
		errs = append(errs, fmt.Sprintf("monitor.circuit_breaker.success_threshold cannot be negative, got %d", c.Monitor.CircuitBreaker.SuccessThreshold))
	}
	if c.Monitor.CircuitBreaker.ResetTimeoutS < 0 {
		errs = append(errs, fmt.Sprintf("monitor.circuit_breaker.reset_timeout_s cannot be negative, got %d", c.Monitor.CircuitBreaker.ResetTimeoutS))
	}

	// Backoff validation
	if c.Monitor.Backoff.MaxIntervalS < 0 {
		errs = append(errs, fmt.Sprintf("monitor.backoff.max_interval_s cannot be negative, got %d", c.Monitor.Backoff.MaxIntervalS))
	}
	// A factor of exactly 0 means "unset" and is replaced by the 2.0 default
	// before validation runs; any explicit value in (0, 1] cannot back off
	// (interval never grows) and was previously coerced to 2.0 behind the
	// operator's back. Reject it so the config means what it says.
	if c.Monitor.Backoff.BackoffFactor < 0 {
		errs = append(errs, fmt.Sprintf("monitor.backoff.backoff_factor cannot be negative, got %f", c.Monitor.Backoff.BackoffFactor))
	} else if c.Monitor.Backoff.BackoffFactor > 0 && c.Monitor.Backoff.BackoffFactor <= 1 {
		errs = append(errs, fmt.Sprintf("monitor.backoff.backoff_factor must be greater than 1 to back off, got %g (omit for the 2.0 default)", c.Monitor.Backoff.BackoffFactor))
	}

	// Topology audit validation. Defaults are applied in LoadConfig before
	// this runs, so a zero here means an explicit zero in YAML. interval_s and
	// debounce_count must be > 0 when the audit is enabled (a zero interval
	// panics time.NewTicker; a zero debounce would trip on the first tick).
	if c.Monitor.TopologyAudit.IntervalS < 0 {
		errs = append(errs, fmt.Sprintf("monitor.topology_audit.interval_s cannot be negative, got %d", c.Monitor.TopologyAudit.IntervalS))
	} else if c.Monitor.TopologyAudit.Enabled && c.Monitor.TopologyAudit.IntervalS == 0 {
		errs = append(errs, "monitor.topology_audit.interval_s must be > 0 when the audit is enabled")
	}
	if c.Monitor.TopologyAudit.DebounceCount < 0 {
		errs = append(errs, fmt.Sprintf("monitor.topology_audit.debounce_count cannot be negative, got %d", c.Monitor.TopologyAudit.DebounceCount))
	} else if c.Monitor.TopologyAudit.Enabled && c.Monitor.TopologyAudit.DebounceCount == 0 {
		errs = append(errs, "monitor.topology_audit.debounce_count must be > 0 when the audit is enabled")
	}
	if c.Monitor.TopologyAudit.QueryLogLookbackMinutes < 0 {
		errs = append(errs, fmt.Sprintf("monitor.topology_audit.query_log_lookback_minutes cannot be negative, got %d", c.Monitor.TopologyAudit.QueryLogLookbackMinutes))
	} else if c.Monitor.TopologyAudit.Enabled && c.Monitor.TopologyAudit.QueryLogLookbackMinutes == 0 {
		// Symmetric with interval_s/debounce_count above. Unreachable via
		// LoadConfig (the default fills 0→15 before Validate runs), but guards a
		// Config built directly with an explicit zero lookback.
		errs = append(errs, "monitor.topology_audit.query_log_lookback_minutes must be > 0 when the audit is enabled")
	}

	// insecure_skip_verify only makes sense with secure: true
	if c.ClickHouse.InsecureSkipVerify && !c.ClickHouse.Secure {
		errs = append(errs, "clickhouse.insecure_skip_verify requires secure: true")
	}
	for i, o := range c.Exporters.OTEL {
		if o.InsecureSkipVerify && !o.Secure {
			errs = append(errs, fmt.Sprintf("exporters.otel[%d].insecure_skip_verify requires secure: true", i))
		}
		if (o.ClientCert != "") != (o.ClientKey != "") {
			errs = append(errs, fmt.Sprintf("exporters.otel[%d].client_cert and exporters.otel[%d].client_key must be provided together for mTLS; set both fields or remove the configured one", i, i))
		}
		if o.MaxQueryLength < 0 {
			errs = append(errs, fmt.Sprintf("exporters.otel[%d].max_query_length cannot be negative, got %d", i, o.MaxQueryLength))
		}
	}

	// Exporters validation: at least one exporter must be configured
	if len(c.Exporters.OTEL) == 0 && len(c.Exporters.SplunkHEC) == 0 {
		errs = append(errs, "at least one exporter must be configured under exporters.otel or exporters.splunk_hec")
	}

	// Every OTEL exporter needs a destination. A list entry with an empty
	// collector_address otherwise passes the "at least one exporter" check
	// above but fails at dial time with an opaque gRPC error.
	for i, o := range c.Exporters.OTEL {
		if strings.TrimSpace(o.CollectorAddress) == "" {
			errs = append(errs, fmt.Sprintf("exporters.otel[%d].collector_address is required", i))
		}
	}

	// Validate each Splunk HEC exporter entry
	for i, sh := range c.Exporters.SplunkHEC {
		if sh.Endpoint == "" {
			errs = append(errs, fmt.Sprintf("exporters.splunk_hec[%d].endpoint is required", i))
		} else if strings.HasPrefix(sh.Endpoint, "http://") && !sh.AllowInsecureHTTP {
			errs = append(errs, fmt.Sprintf("exporters.splunk_hec[%d].endpoint uses http://; set allow_insecure_http: true only for local/dev/test HEC endpoints", i))
		} else if !strings.HasPrefix(sh.Endpoint, "https://") && !strings.HasPrefix(sh.Endpoint, "http://") {
			errs = append(errs, fmt.Sprintf("exporters.splunk_hec[%d].endpoint must start with https:// (or http:// with allow_insecure_http: true)", i))
		}
		if sh.Token == "" {
			errs = append(errs, fmt.Sprintf("exporters.splunk_hec[%d].token is required", i))
		}
		if sh.InsecureSkipVerify && !strings.HasPrefix(sh.Endpoint, "https://") {
			errs = append(errs, fmt.Sprintf("exporters.splunk_hec[%d].insecure_skip_verify requires an https:// endpoint", i))
		}
	}

	// HA validation — only when election is active (Keeper hosts present).
	if c.HA.Active() {
		if c.HA.Keeper.SessionTimeout < 0 {
			errs = append(errs, fmt.Sprintf("ha.keeper.session_timeout_s cannot be negative, got %d", c.HA.Keeper.SessionTimeout))
		}
		if c.HA.Keeper.SessionTimeout > 30 {
			errs = append(errs, fmt.Sprintf("ha.keeper.session_timeout_s must be <= 30 to limit session hold time, got %d", c.HA.Keeper.SessionTimeout))
		}
		if c.HA.Keeper.SessionTimeout > 0 && c.HA.Keeper.SessionTimeout < 5 {
			errs = append(errs, fmt.Sprintf("ha.keeper.session_timeout_s must be >= 5 to avoid election flapping, got %d", c.HA.Keeper.SessionTimeout))
		}
		// Path confinement: base_path must live under /click-dog/ and must not
		// overlap with ClickHouse's own Keeper paths (/clickhouse/) to prevent
		// accidental interference with replication, DDL, or other cluster state.
		bp := c.HA.Keeper.BasePath
		if !strings.HasPrefix(bp, "/click-dog/") && bp != "/click-dog" {
			errs = append(errs, fmt.Sprintf("ha.keeper.base_path must start with /click-dog/ to prevent collisions with other Keeper users, got %q", bp))
		}
		if strings.HasPrefix(bp, "/clickhouse") {
			errs = append(errs, fmt.Sprintf("ha.keeper.base_path must not overlap with ClickHouse's own Keeper paths (/clickhouse/...), got %q", bp))
		}
	}

	// Monitor query length validation
	if c.Monitor.MaxQueryLength < 0 {
		errs = append(errs, fmt.Sprintf("monitor.max_query_length cannot be negative, got %d", c.Monitor.MaxQueryLength))
	}

	// Canary validation
	if c.Monitor.Canary.ThresholdDurationMs < 0 {
		errs = append(errs, fmt.Sprintf("monitor.canary.threshold_duration_ms cannot be negative, got %d", c.Monitor.Canary.ThresholdDurationMs))
	}

	// Log rotation validation. `max_size_mb: 0` would disable rotation
	// (the writer's `maxBytes > 0` guard) and produce unbounded growth, so
	// require a positive value rather than silently substituting the
	// default. `max_files: 0` is similarly a footgun: rotation deletes the
	// rotated file outright. Omit the keys to take the defaults (100, 3).
	if c.LogRotation.MaxSizeMB < 1 {
		errs = append(errs, fmt.Sprintf("log_rotation.max_size_mb must be >= 1, got %d (omit the key to use default 100)", c.LogRotation.MaxSizeMB))
	}
	if c.LogRotation.MaxFiles < 1 {
		errs = append(errs, fmt.Sprintf("log_rotation.max_files must be >= 1, got %d (omit the key to use default 3)", c.LogRotation.MaxFiles))
	}

	// Filter user-list validation: empty entries would silently match "" and
	// either drop all unknown-user spans (whitelist) or pass every such span
	// through a blacklist — both are footguns, so reject at load time.
	for i, u := range c.Filters.WhitelistUsers {
		if strings.TrimSpace(u) == "" {
			errs = append(errs, fmt.Sprintf("filters.whitelist_users[%d] must not be empty", i))
		}
	}
	for i, u := range c.Filters.BlacklistUsers {
		if strings.TrimSpace(u) == "" {
			errs = append(errs, fmt.Sprintf("filters.blacklist_users[%d] must not be empty", i))
		}
	}
	// User filtering depends on query_log enrichment to resolve the CH user
	// per span (system.opentelemetry_span_log has no user attribute). With
	// enrichment disabled, every span resolves to "" — a whitelist would
	// drop all spans, a blacklist would silently pass everyone. Reject the
	// combination rather than ship a broken filter.
	if (len(c.Filters.WhitelistUsers) > 0 || len(c.Filters.BlacklistUsers) > 0) && !c.Monitor.ShouldEnrichFromQueryLog() {
		errs = append(errs, "filters.whitelist_users / filters.blacklist_users require query_log enrichment (set monitor.enrich_from_query_log: true or remove the user filter)")
	}

	// The IP whitelist has the same dependency for the same reason: span-log
	// rows carry no client address, so scheduled mode resolves it through
	// query_log. Without enrichment every span resolves to "" and the
	// whitelist drops the whole cycle.
	if len(c.Filters.WhitelistIPs) > 0 && !c.Monitor.ShouldEnrichFromQueryLog() {
		errs = append(errs, "filters.whitelist_ips requires query_log enrichment (set monitor.enrich_from_query_log: true or remove the IP whitelist)")
	}

	// Filter IP whitelist validation: each entry must parse either as a plain
	// IP (e.g. "10.0.0.1") or as a CIDR (e.g. "10.0.0.0/8"). The filter package
	// rejects bad CIDRs at construction time, but plain-IP entries are
	// stuffed into a lookup map untouched — so a typo'd "10.0.0..1" silently
	// matches nothing instead of erroring. Validate at config load so
	// `click-dog check` surfaces it before the runtime ever sees it.
	for i, entry := range c.Filters.WhitelistIPs {
		if strings.Contains(entry, "/") {
			if _, _, err := net.ParseCIDR(entry); err != nil {
				errs = append(errs, fmt.Sprintf("filters.whitelist_ips[%d] is not a valid CIDR: %q (%v)", i, entry, err))
			}
			continue
		}
		if net.ParseIP(entry) == nil {
			errs = append(errs, fmt.Sprintf("filters.whitelist_ips[%d] is not a valid IP address: %q", i, entry))
		}
	}

	// Query-text privacy mode and redaction rule validation. normalized is
	// intentionally not accepted: normalized_only names the guarantee that raw
	// statement fields are removed, rather than merely enabling enrichment.
	queryTextMode := c.Filters.EffectiveQueryTextMode()
	switch queryTextMode {
	case QueryTextModeRaw, QueryTextModeRedacted, QueryTextModeNormalizedOnly, QueryTextModeNone:
	default:
		errs = append(errs, fmt.Sprintf("filters.query_text_mode must be one of: raw, redacted, normalized_only, none; got %q", queryTextMode))
	}

	// Redaction-rule invariants are also enforced by NewQueryFilter so directly
	// constructed configs cannot bypass the final privacy boundary. Only the
	// redacted mode consumes and therefore compiles rules. normalized_only/none
	// ignore stale rules with a warning; raw rejects them because it would
	// otherwise look redacted while exporting the original statement.
	if queryTextMode == QueryTextModeRedacted {
		for i, rule := range c.Filters.RedactQueries {
			pattern := strings.TrimSpace(rule.Pattern)
			if pattern == "" {
				errs = append(errs, fmt.Sprintf("filters.redact_queries[%d].pattern must not be empty", i))
				continue
			}
			if _, err := regexp.Compile(pattern); err != nil {
				errs = append(errs, fmt.Sprintf("filters.redact_queries[%d].pattern is invalid regex: %v", i, err))
			}
		}
	}
	if queryTextMode == QueryTextModeRedacted && len(c.Filters.RedactQueries) == 0 {
		errs = append(errs, "filters.query_text_mode redacted requires at least one valid filters.redact_queries rule; click-dog will not fall back to raw query text")
	}
	if queryTextMode == QueryTextModeRaw && len(c.Filters.RedactQueries) > 0 {
		errs = append(errs, fmt.Sprintf("filters.redact_queries is only used when filters.query_text_mode is redacted; remove the rules or set query_text_mode: redacted (currently %s)", queryTextMode))
	}

	// Log level validation
	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		errs = append(errs, fmt.Sprintf("log_level must be one of: debug, info, warn, error; got %q", c.LogLevel))
	}

	// Log format validation (empty defaults to "text" at load time)
	if c.LogFormat != "" {
		validLogFormats := map[string]bool{"text": true, "json": true}
		if !validLogFormats[c.LogFormat] {
			errs = append(errs, fmt.Sprintf("log_format must be one of: text, json; got %q", c.LogFormat))
		}
	}

	// Health cluster validation
	if c.Health.Cluster.Enabled {
		// /clusterz is mounted on the health server's mux — without it, the
		// endpoint has nowhere to live. Reject the misconfiguration at load
		// time rather than silently producing a deployment that returns 404.
		if !c.Health.Enabled {
			errs = append(errs, "health.cluster.enabled requires health.enabled: true (the cluster endpoint mounts on the health server)")
		}
		// `self` is the response key for the leader's in-process /readyz
		// AND the matcher for skipping HTTP self-loops during fanout.
		// Falling back to listen_address would break both: a bind-only
		// address like ":8686" never matches a routable peer entry like
		// "node-0:8686", so the leader would HTTP-fetch itself AND show
		// up twice in the response.
		//
		// LoadConfig already trims whitespace from Self/Peers; the
		// TrimSpace here is defensive — Validate() may run against a
		// directly-constructed Config (production tests do this) where
		// the LoadConfig pre-pass never executed.
		self := strings.TrimSpace(c.Health.Cluster.Self)
		if self == "" {
			errs = append(errs, "health.cluster.self is required when cluster is enabled (e.g. \"node-0:8686\" — the routable host:port for this instance)")
		} else if reason := validateClusterAddr(self); reason != "" {
			errs = append(errs, fmt.Sprintf("health.cluster.self %q is not a valid host:port: %s", c.Health.Cluster.Self, reason))
		}
		if c.Health.Cluster.PeerTimeoutMs < 0 {
			errs = append(errs, fmt.Sprintf("health.cluster.peer_timeout_ms cannot be negative, got %d", c.Health.Cluster.PeerTimeoutMs))
		}
		// Duplicate peers silently collapse at runtime (the fanout writes
		// to nodes[peer] from two goroutines; the second write wins but
		// summary.Total is built from the map, so the response shows
		// one entry with no hint of the duplicate). No legitimate config
		// has duplicates — surface them at load time.
		seen := make(map[string]int, len(c.Health.Cluster.Peers))
		for i, p := range c.Health.Cluster.Peers {
			trimmed := strings.TrimSpace(p)
			if trimmed == "" {
				errs = append(errs, fmt.Sprintf("health.cluster.peers[%d] must not be empty", i))
				continue
			}
			if reason := validateClusterAddr(trimmed); reason != "" {
				errs = append(errs, fmt.Sprintf("health.cluster.peers[%d] %q is not a valid host:port: %s", i, p, reason))
				continue
			}
			if first, dup := seen[trimmed]; dup {
				errs = append(errs, fmt.Sprintf("health.cluster.peers[%d] %q duplicates peers[%d]", i, p, first))
				continue
			}
			seen[trimmed] = i
		}
	}

	// Webhook validation
	if c.Webhook.Enabled {
		if c.Webhook.URL == "" {
			errs = append(errs, "webhook.url is required when webhook is enabled")
		} else if reason := validateWebhookURL(c.Webhook.URL); reason != "" {
			errs = append(errs, fmt.Sprintf("webhook.url is invalid: %s", reason))
		}
		if c.Webhook.TimeoutS < 0 {
			errs = append(errs, fmt.Sprintf("webhook.timeout_s cannot be negative, got %d", c.Webhook.TimeoutS))
		}
		validEvents := map[string]bool{
			"analysis_findings":      true,
			"circuit_breaker_opened": true,
			"circuit_breaker_closed": true,
			"backfill_complete":      true,
			"backfill_failed":        true,
			"error_spike":            true,
			"startup":                true,
			"shutdown":               true,
		}
		for _, event := range c.Webhook.Events {
			if !validEvents[event] {
				errs = append(errs, fmt.Sprintf("webhook.events contains unknown event %q", event))
			}
		}
	}

	// Datadog Event Management validation. The credentials are deliberately
	// separate from exporters.otel: an OTLP collector or Agent connection does
	// not authorize the Events API.
	if c.DatadogEvents.Enabled {
		if c.DatadogEvents.Site == "" {
			errs = append(errs, "datadog_events.site is required when datadog_events is enabled")
		} else if _, err := ValidateDatadogSite(c.DatadogEvents.Site); err != nil {
			errs = append(errs, fmt.Sprintf("datadog_events.site %q is invalid: %v", c.DatadogEvents.Site, err))
		}
		if strings.TrimSpace(c.DatadogEvents.APIKey) == "" {
			errs = append(errs, "datadog_events.api_key or api_key_file is required when datadog_events is enabled")
		}
		if strings.TrimSpace(c.DatadogEvents.ApplicationKey) == "" {
			errs = append(errs, "datadog_events.application_key or application_key_file is required when datadog_events is enabled")
		}
		if c.DatadogEvents.TimeoutS <= 0 || c.DatadogEvents.TimeoutS > MaxDatadogEventsTimeoutS {
			errs = append(errs, fmt.Sprintf("datadog_events.timeout_s must be between 1 and %d when enabled, got %d", MaxDatadogEventsTimeoutS, c.DatadogEvents.TimeoutS))
		}
		if reason := ValidateDatadogTagValue("datadog_events.environment", c.DatadogEvents.Environment); reason != "" {
			errs = append(errs, reason)
		}
		if reason := ValidateDatadogTagValue("datadog_events.service", c.DatadogEvents.Service); reason != "" {
			errs = append(errs, reason)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("configuration validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return nil
}

func knownOTLPMetricKeys() map[string]struct{} {
	out := make(map[string]struct{}, len(clickmetrics.CanonicalDescriptors()))
	for _, d := range clickmetrics.CanonicalDescriptors() {
		out[d.Key] = struct{}{}
	}
	return out
}

func (c *Config) GetCheckInterval() time.Duration {
	return time.Duration(c.Monitor.CheckIntervalS) * time.Second
}

func (c *Config) GetLookback() time.Duration {
	return time.Duration(c.Monitor.LookbackS) * time.Second
}

// ShouldExtractLogComment returns whether log_comment JSON extraction is enabled.
// Defaults to true if not explicitly set.
func (m *MonitorConfig) ShouldExtractLogComment() bool {
	if m.ExtractLogComment == nil {
		return true
	}
	return *m.ExtractLogComment
}

// ShouldEnrichFromQueryLog returns whether query_log enrichment is enabled.
// Defaults to true if not explicitly set.
func (m *MonitorConfig) ShouldEnrichFromQueryLog() bool {
	if m.EnrichFromQueryLog == nil {
		return true
	}
	return *m.EnrichFromQueryLog
}
