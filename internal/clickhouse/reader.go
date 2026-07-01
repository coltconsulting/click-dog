package clickhouse

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/queryfamily"
)

const (
	// MaxSpanQueryLimit is the absolute maximum number of spans that can be fetched in a single query.
	// This prevents OOM conditions even if a very large limit is passed (e.g. from a
	// backfill call that bypasses config validation). It mirrors config.MaxSpansPerCycleCeiling,
	// the config-load ceiling on monitor.max_spans_per_cycle.
	MaxSpanQueryLimit = config.MaxSpansPerCycleCeiling

	// healthCheckTimeout is the context timeout for connection health pings.
	healthCheckTimeout = 5 * time.Second

	// canaryQueryTimeout is the context timeout for lightweight canary queries.
	canaryQueryTimeout = 5 * time.Second

	defaultQueryFamilyLookback        = 24 * time.Hour
	defaultQueryFamilyExactGroupLimit = 200
	// Bounds complete-link rollup clustering to roughly 500k pairwise
	// comparisons before token/shingle set costs.
	maxQueryFamilyExactGroupLimit = 1000
)

// ErrQueryFamilyRollupsUnsupported is returned when ClickHouse does not expose
// normalized_query_hash safely enough for higher-level family rollups.
var ErrQueryFamilyRollupsUnsupported = errors.New("query family rollups require system.query_log.normalized_query_hash support")

// ---------------------------------------------------------------------------
// SQL templates
//
// Table references use named placeholders resolved once at reader init via
// strings.ReplaceAll — no fmt.Sprintf touches SQL. All runtime values flow
// through ? parameterised queries.
// ---------------------------------------------------------------------------

const tblQueryLog = "{TABLE_QUERY_LOG}"
const tblSpanLog = "{TABLE_SPAN_LOG}"
const tblProcesses = "{TABLE_PROCESSES}"
const queryLogNormalizedColumns = "{QUERY_LOG_NORMALIZED_COLUMNS}"
const processesNormalizedColumn = "{PROCESSES_NORMALIZED_COLUMN}"

// enrichSelectAnchor is a dedicated marker setUserFilter replaces with the
// whitelist clause (or strips entirely). Kept distinct from any functional
// SQL token (e.g. ORDER BY event_time) so a future template edit can't
// accidentally collide.
const enrichSelectAnchor = "-- {USER_FILTER_SPLICE}"

const queryLogNormalizedColumnsSQL = `
				normalized_query_hash,
				normalizeQuery(query) AS normalized_query,`

const normalizedQueryHashProbeSQL = `
			SELECT count()
			FROM system.columns
			WHERE database = 'system'
			  AND table = 'query_log'
			  AND name = 'normalized_query_hash'
		`

// normalizedQueryHashClusterProbeSQL uses the same placeholder style as the
// other reader SQL constants so table-reference substitution stays centralized.
const normalizedQueryHashClusterProbeSQL = `
			SELECT count() AS replicas, countIf(has_normalized) AS supported
			FROM (
				SELECT
					hostName() AS host,
					countIf(name = 'normalized_query_hash') > 0 AS has_normalized
				FROM {TABLE_SYSTEM_COLUMNS}
				WHERE database = 'system'
				  AND table = 'query_log'
				GROUP BY host
			)
		`

const queryLogSelectSQL = `
			SELECT
				query_id,
				type as query_kind,
				event_time,
				query_duration_ms,
				query,
				` + queryLogNormalizedColumns + `
				user,
				client_name,
				client_hostname,
				IPv6NumToString(address) as client_address,
				databases,
				tables,
				exception_code,
				read_rows,
				read_bytes,
				written_rows,
				written_bytes,
				result_rows,
				result_bytes,
				memory_usage
			FROM ` + tblQueryLog + `
			WHERE `

const traceIDSelectSQL = `
			SELECT trace_id, max(finish_time) as max_finish_time
			FROM (
				SELECT
					trace_id,
					finish_time_us as finish_time,
					start_time_us as start_time,
					finish_date as fdate
				FROM ` + tblSpanLog + `
				WHERE finish_date >= today() - INTERVAL ? DAY
				  AND finish_time_us >= toUnixTimestamp64Micro(now64() - INTERVAL ? SECOND)
				  AND (finish_time_us - start_time_us) >= ? * 1000`

const spanSelectSQL = `
			SELECT
				hostname,
				trace_id,
				span_id,
				parent_span_id,
				operation_name,
				kind,
				start_time_us,
				finish_time_us,
				finish_date,
				attribute
			FROM ` + tblSpanLog + `
			WHERE trace_id IN ?
			  AND finish_date >= today() - INTERVAL ? DAY
		`

const canarySelectSQL = `
			SELECT count()
			FROM ` + tblSpanLog + `
			WHERE finish_date = today()
			  AND (finish_time_us - start_time_us) >= ? * 1000
			LIMIT 1
		`

// traceIDByQueryIDSelectSQL resolves the trace IDs that carry a given
// ClickHouse query_id span attribute. This is the trace-drilldown bridge:
// system.query_log has no trace_id column, so the clickhouse.query_id span
// attribute is the only link from a query identity to native trace spans.
// The finish_date partition bound is rounded up to whole days from the
// requested lookback (floored at 1) by the caller, matching the
// query-log-by-id enrichment lookback rule; the query_id still bounds the
// logical subject of the lookup.
const traceIDByQueryIDSelectSQL = `
			SELECT DISTINCT trace_id
			FROM ` + tblSpanLog + `
			WHERE finish_date >= today() - INTERVAL ? DAY
			  AND attribute['clickhouse.query_id'] = ?
			LIMIT ?
		`

// queryLogEnrichSelectSQL fetches query_log rows for enrichment by query_id.
// Query text is intentionally omitted — it can be large and sensitive, and
// db.statement already carries the SQL from the span attributes.
//
// Note: system.query_log does not have a trace_id column (as of CH 24.1+).
// The join key is query_id, which appears in both query_log (primary column)
// and opentelemetry_span_log (via the clickhouse.query_id span attribute).
const queryLogEnrichSelectSQL = `
			SELECT
				query_id,
				type as query_kind,
				event_time,
				query_duration_ms,
				` + queryLogNormalizedColumns + `
				user,
				client_name,
				client_hostname,
				IPv6NumToString(address) as client_address,
				databases,
				tables,
				exception_code,
				read_rows,
				read_bytes,
				written_rows,
				written_bytes,
				result_rows,
				result_bytes,
				memory_usage
			FROM ` + tblQueryLog + `
			WHERE query_id IN ?
			  AND type IN ('QueryFinish', 'ExceptionWhileProcessing')
			  AND event_date >= today() - ?
			-- {USER_FILTER_SPLICE}
			ORDER BY event_time
			LIMIT 2 BY query_id
		`

// queryFamilyExactGroupsSQL is generated from queryfamily.TopKLimit so the
// ClickHouse exact-group query and in-memory family rollup keep one top-K cap.
var queryFamilyExactGroupsSQL = fmt.Sprintf(`
			SELECT
				normalized_query_hash,
				min(normalizeQuery(query)) AS normalized_query,
				count() AS execution_count,
				quantileTDigest(0.95)(query_duration_ms) AS p95_duration_ms,
				quantileTDigest(0.99)(query_duration_ms) AS p99_duration_ms,
				max(memory_usage) AS max_memory_usage,
				quantileTDigest(0.95)(read_rows) AS p95_read_rows,
				quantileTDigest(0.95)(read_bytes) AS p95_read_bytes,
				topK(%[1]d)(user) AS top_users,
				topK(%[1]d)(client_name) AS top_clients,
				arrayReduce('topK(%[1]d)', arrayFlatten(groupArray(tables))) AS top_tables,
				min(event_time) AS first_seen,
				max(event_time) AS last_seen
			FROM `+tblQueryLog+`
			WHERE event_time >= ?
			  AND event_time <= ?
			  AND type IN ('QueryFinish', 'ExceptionWhileProcessing')
			  AND normalized_query_hash != 0
		`, queryfamily.TopKLimit)

// processesNormalizedColumnSQL is spliced into currentQueryCandidatesSelectSQL
// only when ClickHouse exposes normalized-query support (the same capability
// probe as query_log). It is the ONLY place a running query's text is touched,
// and only via normalizeQuery() — the literal-stripped, bounded preview. Raw
// system.processes.query is NEVER selected: when normalized support is absent
// the column is dropped entirely and the candidate carries no preview.
const processesNormalizedColumnSQL = `
				normalizeQuery(query) AS normalized_query,`

// currentQueryCandidatesSelectSQL reads in-flight queries from system.processes
// for the trace-drilldown `current` source. system.processes has no
// query_duration_ms / normalized_query_hash / event_time — a running query has
// not produced a query_log row yet — so the command surfaces the running
// metadata it does have (query_id, elapsed runtime, user/client/host/address)
// and degrades the absent fields to warnings.
//
// `elapsed` is a Float64 of seconds; it is converted to whole milliseconds in
// SQL so the candidate carries the same elapsed_ms unit the report renders.
// The normalized preview column is spliced in only when supported (see
// processesNormalizedColumnSQL); raw query text is never selected. The empty
// query_id rows that system.processes can carry for housekeeping are excluded.
const currentQueryCandidatesSelectSQL = `
			SELECT
				query_id,
				toUInt64(elapsed * 1000) AS elapsed_ms,
				` + processesNormalizedColumn + `
				user,
				client_name,
				client_hostname,
				IPv6NumToString(address) AS client_address
			FROM ` + tblProcesses + `
			WHERE query_id != ''`

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

type ClickHouseReader struct {
	conn         driver.Conn
	queryTimeout time.Duration

	// Precomputed SQL templates with table references resolved at init.
	queryLogSelect     string
	traceIDSelect      string
	traceIDByQueryID   string
	spanSelect         string
	canarySelect       string
	enrichSelect       string
	queryFamilySelect  string
	currentQuerySelect string
	queryLogNormalized bool

	// Resolved table references (cluster-aware) reused by the readiness
	// doctor to build its diagnostic SQL without re-deriving cluster mode.
	spanLogRef        string
	queryLogRef       string
	useClusterQueries bool
	cluster           string

	// enrichExtraParams holds the []string slice bound to setUserFilter's
	// spliced `user IN ?` placeholder. Blacklist isn't spliced into
	// enrichSelect — see setUserFilter.
	enrichExtraParams []interface{}

	// Raw user-filter lists for the builder-based slow-query paths.
	whitelistUsers []string
	blacklistUsers []string
}

// escapeSQL escapes single quotes for safe embedding in a SQL string literal.
// Defense-in-depth alongside config validation (safeIdentifierPattern).
func escapeSQL(s string) string {
	return strings.NewReplacer("\\", "\\\\", "'", "''").Replace(s)
}

// resolveSpanLimit returns the effective per-cycle SQL LIMIT for the spans
// query. limit comes from monitor.max_spans_per_cycle and directly bounds how
// many spans are fetched (and therefore exported) in a single cycle.
// MaxSpanQueryLimit is the hard safety ceiling used when limit is unset
// (<= 0) or larger than the ceiling.
func resolveSpanLimit(limit int) int {
	if limit <= 0 || limit > MaxSpanQueryLimit {
		return MaxSpanQueryLimit
	}
	return limit
}

// buildTableRef returns the SQL table expression for the given system table.
// When cluster queries are enabled, wraps in cluster('name', table).
//
// cluster() is a ClickHouse table function whose arguments must be constant
// expressions resolved at parse time — server-side ? parameterisation cannot
// be used. The cluster name is validated at config load (safeIdentifierPattern)
// and escaped here as defense-in-depth.
func buildTableRef(table, cluster string, useCluster bool) string {
	if cluster != "" && useCluster {
		return "cluster('" + escapeSQL(cluster) + "', " + table + ")"
	}
	return table
}

func buildClusterAllReplicasRef(table, cluster string) string {
	return "clusterAllReplicas('" + escapeSQL(cluster) + "', " + table + ")"
}

func buildClusterNormalizedProbeSQL(cluster string) string {
	return strings.ReplaceAll(normalizedQueryHashClusterProbeSQL, "{TABLE_SYSTEM_COLUMNS}", buildClusterAllReplicasRef("system.columns", cluster))
}

func buildQueryLogSQL(template, tableRef string, includeNormalized bool) string {
	normalizedColumns := ""
	if includeNormalized {
		normalizedColumns = queryLogNormalizedColumnsSQL
	}

	query := strings.ReplaceAll(template, tblQueryLog, tableRef)
	return strings.ReplaceAll(query, queryLogNormalizedColumns, normalizedColumns)
}

// buildCurrentQuerySQL resolves the system.processes table reference and splices
// the normalized-preview column only when supported. When normalized support is
// absent the preview column is dropped entirely, so the running query's text is
// never selected (raw or otherwise).
func buildCurrentQuerySQL(template, tableRef string, includeNormalized bool) string {
	normalizedColumn := ""
	if includeNormalized {
		normalizedColumn = processesNormalizedColumnSQL
	}
	query := strings.ReplaceAll(template, tblProcesses, tableRef)
	return strings.ReplaceAll(query, processesNormalizedColumn, normalizedColumn)
}

func detectQueryLogNormalizedSupport(ctx context.Context, conn driver.Conn, cluster string, useClusterQueries bool) bool {
	if useClusterQueries {
		if cluster == "" {
			clicklog.Warn("Normalized query_log fields disabled in cluster query mode: no cluster name configured")
			return false
		}
		var replicas, supported uint64
		if err := conn.QueryRow(ctx, buildClusterNormalizedProbeSQL(cluster)).Scan(&replicas, &supported); err != nil {
			clicklog.Warn("Could not verify query_log.normalized_query_hash support across cluster replicas; normalized query attributes disabled: %v", err)
			return false
		}
		if replicas == 0 {
			clicklog.Warn("Could not verify any cluster replicas expose system.query_log; normalized query attributes disabled")
			return false
		}
		if supported != replicas {
			clicklog.Warn("Normalized query_log fields disabled in cluster query mode: normalized_query_hash found on %d/%d replicas", supported, replicas)
			return false
		}
		clicklog.Info("Normalized query_log fields enabled in cluster query mode: normalized_query_hash found on all %d replicas", replicas)
		return true
	}

	var count uint64
	if err := conn.QueryRow(ctx, normalizedQueryHashProbeSQL).Scan(&count); err != nil {
		clicklog.Warn("Could not detect query_log.normalized_query_hash support; normalized query attributes disabled: %v", err)
		return false
	}
	return count > 0
}

func capabilityProbeContext(timeoutS int) (context.Context, context.CancelFunc) {
	if timeoutS > 0 {
		return context.WithTimeout(context.Background(), time.Duration(timeoutS)*time.Second)
	}
	return context.WithCancel(context.Background())
}

// NewClickHouseReader opens a connection, prepares the SQL templates, and
// applies the user-filter splice atomically — there is no separate post-
// construction step a caller can forget.
func NewClickHouseReader(cfg config.ClickHouseConfig, filters config.FiltersConfig) (*ClickHouseReader, error) {
	options := &clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		Settings: clickhouse.Settings{
			"readonly":           2,                  // Read-only mode (allows setting changes but no writes)
			"max_execution_time": cfg.QueryTimeoutS,  // Server-side query timeout matches client config
			"max_memory_usage":   cfg.MaxMemoryUsage, // Per-query memory limit (default: 100MB)
		},
		DialTimeout:      time.Second * 30,
		MaxOpenConns:     cfg.MaxOpenConns,
		MaxIdleConns:     cfg.MaxIdleConns,
		ConnMaxLifetime:  time.Hour,
		ConnOpenStrategy: clickhouse.ConnOpenInOrder,
	}

	if cfg.Secure {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		}

		if cfg.CACert != "" {
			caCert, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("failed to read CA certificate: %w", err)
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("failed to parse CA certificate")
			}
			tlsConfig.RootCAs = caCertPool
		}

		options.TLS = tlsConfig

		if cfg.InsecureSkipVerify {
			clicklog.Warn("TLS certificate verification is disabled - this is insecure and should only be used for testing")
		}
	}

	conn, err := clickhouse.Open(options)

	if err != nil {
		return nil, fmt.Errorf("failed to connect to ClickHouse: %w", err)
	}

	if err := conn.Ping(context.Background()); err != nil {
		if cfg.Secure && strings.Contains(err.Error(), "does not look like a TLS handshake") {
			clicklog.Error("ClickHouse at %s:%d appears to be running in plaintext mode — set secure: false in config, or configure ClickHouse to use TLS", cfg.Host, cfg.Port)
			return nil, fmt.Errorf("failed to ping ClickHouse (TLS handshake failed): %w", err)
		}
		if !cfg.Secure && strings.Contains(err.Error(), "EOF") {
			clicklog.Error("ClickHouse at %s:%d may require TLS — set secure: true in config", cfg.Host, cfg.Port)
			return nil, fmt.Errorf("failed to ping ClickHouse (connection rejected, TLS required?): %w", err)
		}
		return nil, fmt.Errorf("failed to ping ClickHouse: %w", err)
	}

	clicklog.Info("Connected to ClickHouse: %s:%d (database: %s)", cfg.Host, cfg.Port, cfg.Database)

	// Resolve table references into SQL templates once at init.
	queryLogRef := buildTableRef("system.query_log", cfg.Cluster, cfg.UseClusterQueries)
	spanLogRef := buildTableRef("system.opentelemetry_span_log", cfg.Cluster, cfg.UseClusterQueries)
	processesRef := buildTableRef("system.processes", cfg.Cluster, cfg.UseClusterQueries)
	useClusterQueryLog := cfg.UseClusterQueries

	capabilityCtx, capabilityCancel := capabilityProbeContext(cfg.QueryTimeoutS)
	queryLogNormalized := detectQueryLogNormalizedSupport(capabilityCtx, conn, cfg.Cluster, useClusterQueryLog)
	capabilityCancel()

	r := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Duration(cfg.QueryTimeoutS) * time.Second,
		queryLogSelect:     buildQueryLogSQL(queryLogSelectSQL, queryLogRef, queryLogNormalized),
		traceIDSelect:      strings.ReplaceAll(traceIDSelectSQL, tblSpanLog, spanLogRef),
		traceIDByQueryID:   strings.ReplaceAll(traceIDByQueryIDSelectSQL, tblSpanLog, spanLogRef),
		spanSelect:         strings.ReplaceAll(spanSelectSQL, tblSpanLog, spanLogRef),
		canarySelect:       strings.ReplaceAll(canarySelectSQL, tblSpanLog, spanLogRef),
		enrichSelect:       buildQueryLogSQL(queryLogEnrichSelectSQL, queryLogRef, queryLogNormalized),
		queryFamilySelect:  strings.ReplaceAll(queryFamilyExactGroupsSQL, tblQueryLog, queryLogRef),
		currentQuerySelect: buildCurrentQuerySQL(currentQueryCandidatesSelectSQL, processesRef, queryLogNormalized),
		queryLogNormalized: queryLogNormalized,
		spanLogRef:         spanLogRef,
		queryLogRef:        queryLogRef,
		useClusterQueries:  cfg.UseClusterQueries,
		cluster:            cfg.Cluster,
	}
	r.setUserFilter(filters.WhitelistUsers, filters.BlacklistUsers)
	return r, nil
}

func (c *ClickHouseReader) Close() error {
	return c.conn.Close()
}

// QueryLogNormalizedSupported reports whether the connected ClickHouse
// exposes system.query_log.normalized_query_hash safely. In cluster-query
// mode this requires every replica returned by clusterAllReplicas() to expose
// the column, avoiding mixed-version SELECT failures. The probe runs once in
// NewClickHouseReader; this accessor lets the processor mirror the result into
// a metric without re-probing.
func (c *ClickHouseReader) QueryLogNormalizedSupported() bool {
	return c.queryLogNormalized
}

// setUserFilter records the configured user lists and splices the whitelist
// clause into the enrichment SQL. Blacklist is intentionally NOT spliced
// into enrichSelect — pushing `AND user NOT IN ?` there would drop
// blacklisted users' rows from queryLogMap, leaving resolveSpanUser unable
// to identify the user, and shouldFilterSpanByUser is permissive on "" by
// design (mirrors WhitelistIPs strict / blacklist-permissive contract).
// The slow-query builder path keeps both clauses because it returns rows
// directly with user populated. Always strips the build-time anchor on
// exit so it doesn't appear in CH's query_log.
func (c *ClickHouseReader) setUserFilter(whitelist, blacklist []string) {
	// Runs even on panic — harmless in production (panic kills the process);
	// note that a test using recover() will see enrichSelect with the anchor
	// already stripped.
	defer func() {
		c.enrichSelect = strings.ReplaceAll(c.enrichSelect, enrichSelectAnchor, "")
	}()

	if len(whitelist) == 0 && len(blacklist) == 0 {
		return
	}
	// panic (not LogFatal) — these guards only ever fire in test misuse,
	// since NewClickHouseReader is the sole production caller and runs
	// exactly once. recover() in tests still works; the deferred anchor
	// strip survives.
	if len(c.whitelistUsers) > 0 || len(c.blacklistUsers) > 0 {
		panic("clickhouse.setUserFilter called more than once")
	}
	if got := strings.Count(c.enrichSelect, enrichSelectAnchor); got != 1 {
		panic(fmt.Sprintf("clickhouse.setUserFilter: expected exactly 1 anchor %q, found %d", enrichSelectAnchor, got))
	}

	// Defensive copies — see TestUserFilter_DefensiveCopy.
	c.whitelistUsers = append([]string(nil), whitelist...)
	c.blacklistUsers = append([]string(nil), blacklist...)

	if len(c.whitelistUsers) > 0 {
		c.enrichExtraParams = append(c.enrichExtraParams, c.whitelistUsers)
		c.enrichSelect = strings.Replace(c.enrichSelect, enrichSelectAnchor, "AND user IN ?", 1)
	}
}

// IsHealthy returns true if the connection is healthy, false otherwise
// This is a non-blocking check that can be used before running queries
func (c *ClickHouseReader) IsHealthy(ctx context.Context) bool {
	pingCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	if err := c.conn.Ping(pingCtx); err != nil {
		clicklog.Warn("ClickHouse connection health check failed: %v", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// query_log queries
// ---------------------------------------------------------------------------

// QueryFamilyRollupOptions bounds the offline/dashboard-analysis rollup path.
// It intentionally stays separate from live ExportSpans/ExportQuery flows.
type QueryFamilyRollupOptions struct {
	StartTime           time.Time
	EndTime             time.Time
	Lookback            time.Duration
	MinExecutionCount   uint64
	ExactGroupLimit     int // Silently clamped to maxQueryFamilyExactGroupLimit.
	SimilarityThreshold float64
	MaxPreviewLength    int
}

// queryLogBuilder holds the components for building a query_log SQL query.
// The two public Fetch methods differ only in their time-range predicate
// and sort order; everything else (columns, duration/length filters, scan
// logic) is shared through this builder.
type queryLogBuilder struct {
	query  string
	params []interface{}
}

// newQueryLogBuilder creates a builder pre-loaded with the SELECT columns
// and the caller-supplied time-range WHERE clause + params.
func (c *ClickHouseReader) newQueryLogBuilder(timePredicate string, timeParams []interface{}) *queryLogBuilder {
	return &queryLogBuilder{
		query:  c.queryLogSelect + timePredicate,
		params: append([]interface{}{}, timeParams...),
	}
}

func (b *queryLogBuilder) addDurationFilters(minDurationMs, maxDurationMs, maxQueryLength int) {
	b.query += " AND query_duration_ms >= ?"
	b.params = append(b.params, minDurationMs)

	if maxDurationMs > 0 {
		b.query += " AND query_duration_ms <= ?"
		b.params = append(b.params, maxDurationMs)
	}

	if maxQueryLength > 0 {
		b.query += " AND length(query) <= ?"
		b.params = append(b.params, maxQueryLength)
	}
}

// Slices are bound directly (no defensive copy) — the reader already
// holds defensive copies via setUserFilter and never mutates them.
func (b *queryLogBuilder) addUserFilters(whitelist, blacklist []string) {
	if len(whitelist) > 0 {
		b.query += " AND user IN ?"
		b.params = append(b.params, whitelist)
	}
	if len(blacklist) > 0 {
		b.query += " AND user NOT IN ?"
		b.params = append(b.params, blacklist)
	}
}

func (b *queryLogBuilder) addOrderAndLimit(ascending bool, limit int) {
	if ascending {
		b.query += " ORDER BY event_time ASC"
	} else {
		b.query += " ORDER BY event_time DESC"
	}
	if limit > 0 {
		b.query += " LIMIT ?"
		b.params = append(b.params, limit)
	}
}

// addCandidateOrderAndLimit orders trace-drilldown candidate search results
// most-expensive-first (then newest) so the bounded candidate list surfaces the
// queries an operator most likely wants to drill into. The deterministic
// query_id tie-break keeps output stable across reruns within a window.
func (b *queryLogBuilder) addCandidateOrderAndLimit(limit int) {
	b.query += " ORDER BY query_duration_ms DESC, event_time DESC, query_id ASC"
	if limit > 0 {
		b.query += " LIMIT ?"
		b.params = append(b.params, limit)
	}
}

// addCandidateHashFilter pushes the explicit normalized_query_hash identity into
// the SQL WHERE so the LIMIT bounds the matched rows, not a cost-truncated set.
// The column only exists when normalized support is present, so the caller must
// gate this on QueryLogNormalizedSupported(); calling it otherwise would emit a
// query referencing a missing column.
func (b *queryLogBuilder) addCandidateHashFilter(hash uint64) {
	b.query += " AND normalized_query_hash = ?"
	b.params = append(b.params, hash)
}

// addCandidateMatchFilter pushes the -match needle into the SQL WHERE as an
// OR-group over the candidate metadata fields, using ClickHouse case-insensitive
// substring matching: positionCaseInsensitive(col, ?) > 0 for scalar columns and
// arrayExists(x -> positionCaseInsensitive(x, ?) > 0, col) for the array
// columns. The needle param is bound once per term, in the same left-to-right
// order the terms appear in the SQL. normalizeQuery(query) is only matchable
// when normalized support is present; includeNormalized drops that term (and the
// previews-unavailable warning is raised by the caller) so the query never
// references the unsupported normalized expression.
//
// Raw query text is never matched: the normalized term searches
// normalizeQuery(query), the same bounded preview the report would emit.
func (b *queryLogBuilder) addCandidateMatchFilter(needle string, includeNormalized bool) {
	// Scalar columns matched with positionCaseInsensitive(col, ?) > 0.
	scalarCols := []string{"query_id", "user", "client_name", "client_hostname"}
	if includeNormalized {
		scalarCols = append(scalarCols, "normalizeQuery(query)")
	}
	// Array columns matched with arrayExists over positionCaseInsensitive.
	arrayCols := []string{"tables", "databases"}

	terms := make([]string, 0, len(scalarCols)+len(arrayCols))
	for _, col := range scalarCols {
		terms = append(terms, "positionCaseInsensitive("+col+", ?) > 0")
		b.params = append(b.params, needle)
	}
	for _, col := range arrayCols {
		terms = append(terms, "arrayExists(x -> positionCaseInsensitive(x, ?) > 0, "+col+")")
		b.params = append(b.params, needle)
	}
	b.query += " AND (" + strings.Join(terms, " OR ") + ")"
}

// executeQueryLogQuery runs the built query and scans rows into QueryLog structs.
func (c *ClickHouseReader) executeQueryLogQuery(ctx context.Context, b *queryLogBuilder) ([]model.QueryLog, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, b.query, b.params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query ClickHouse: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var logs []model.QueryLog
	for rows.Next() {
		var log model.QueryLog
		if err := rows.Scan(c.queryLogScanDest(&log)...); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		logs = append(logs, log)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	return logs, nil
}

func (c *ClickHouseReader) queryLogScanDest(log *model.QueryLog) []interface{} {
	// Keep this order in lockstep with queryLogSelectSQL. The optional
	// normalized fields are spliced into the SELECT immediately after raw
	// query, which is why they are appended at the same point here.
	dest := []interface{}{
		&log.QueryID,
		&log.QueryKind,
		&log.EventTime,
		&log.QueryDurationMs,
		&log.Query,
	}
	if c.queryLogNormalized {
		dest = append(dest, &log.NormalizedQueryHash, &log.NormalizedQuery)
	}
	dest = append(dest,
		&log.User,
		&log.ClientName,
		&log.ClientHostname,
		&log.ClientAddress,
		&log.DatabasesVisited,
		&log.TablesVisited,
		&log.ExceptionCode,
		&log.ReadRows,
		&log.ReadBytes,
		&log.WrittenRows,
		&log.WrittenBytes,
		&log.ResultRows,
		&log.ResultBytes,
		&log.MemoryUsage,
	)
	return dest
}

// FetchQueryFamilyRollups returns higher-level deterministic query families
// computed from exact normalized-query groups in system.query_log. The method
// first aggregates rows by normalized_query_hash in ClickHouse, then clusters
// only those exact groups in memory. It is not called by the hot live-export
// path.
func (c *ClickHouseReader) FetchQueryFamilyRollups(ctx context.Context, opts QueryFamilyRollupOptions) ([]model.QueryFamilyRollup, error) {
	if !c.queryLogNormalized {
		return nil, ErrQueryFamilyRollupsUnsupported
	}

	normalizedOpts, err := normalizeQueryFamilyRollupOptions(opts, time.Now())
	if err != nil {
		return nil, err
	}

	groups, err := c.fetchQueryFamilyExactGroups(ctx, normalizedOpts)
	if err != nil {
		return nil, err
	}
	return queryfamily.Rollup(ctx, groups, queryfamily.Options{
		SimilarityThreshold: normalizedOpts.SimilarityThreshold,
		MaxPreviewLength:    normalizedOpts.MaxPreviewLength,
	})
}

func normalizeQueryFamilyRollupOptions(opts QueryFamilyRollupOptions, now time.Time) (QueryFamilyRollupOptions, error) {
	if opts.EndTime.IsZero() {
		opts.EndTime = now
	}
	if opts.StartTime.IsZero() {
		if opts.Lookback <= 0 {
			opts.Lookback = defaultQueryFamilyLookback
		}
		opts.StartTime = opts.EndTime.Add(-opts.Lookback)
	}
	if opts.StartTime.After(opts.EndTime) {
		return opts, fmt.Errorf("query family rollup start time %s is after end time %s", opts.StartTime.Format(time.RFC3339), opts.EndTime.Format(time.RFC3339))
	}
	if opts.MinExecutionCount == 0 {
		opts.MinExecutionCount = 1
	}
	if opts.ExactGroupLimit <= 0 {
		opts.ExactGroupLimit = defaultQueryFamilyExactGroupLimit
	}
	if opts.ExactGroupLimit > maxQueryFamilyExactGroupLimit {
		opts.ExactGroupLimit = maxQueryFamilyExactGroupLimit
	}
	if opts.SimilarityThreshold <= 0 || opts.SimilarityThreshold > 1 {
		opts.SimilarityThreshold = queryfamily.DefaultSimilarityThreshold
	}
	if opts.MaxPreviewLength <= 0 {
		opts.MaxPreviewLength = queryfamily.DefaultMaxPreviewLength
	}
	if opts.MaxPreviewLength > queryfamily.MaxPreviewLength {
		opts.MaxPreviewLength = queryfamily.MaxPreviewLength
	}
	return opts, nil
}

func (c *ClickHouseReader) queryFamilyExactGroupsBuilder(opts QueryFamilyRollupOptions) *queryLogBuilder {
	b := &queryLogBuilder{
		query:  c.queryFamilySelect,
		params: []interface{}{opts.StartTime, opts.EndTime},
	}
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)
	b.query += " GROUP BY normalized_query_hash HAVING count() >= ? ORDER BY execution_count DESC LIMIT ?"
	b.params = append(b.params, opts.MinExecutionCount, opts.ExactGroupLimit)
	return b
}

func (c *ClickHouseReader) fetchQueryFamilyExactGroups(ctx context.Context, opts QueryFamilyRollupOptions) ([]model.QueryFamilyExactGroup, error) {
	b := c.queryFamilyExactGroupsBuilder(opts)

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, b.query, b.params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query ClickHouse query family exact groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var groups []model.QueryFamilyExactGroup
	for rows.Next() {
		var group model.QueryFamilyExactGroup
		if err := rows.Scan(
			&group.NormalizedQueryHash,
			&group.NormalizedQuery,
			&group.ExecutionCount,
			&group.P95DurationMs,
			&group.P99DurationMs,
			&group.MaxMemoryUsage,
			&group.P95ReadRows,
			&group.P95ReadBytes,
			&group.TopUsers,
			&group.TopClients,
			&group.TopTables,
			&group.FirstSeen,
			&group.LastSeen,
		); err != nil {
			return nil, fmt.Errorf("failed to scan query family exact group: %w", err)
		}
		group.NormalizedQuery = queryfamily.TruncatePreview(group.NormalizedQuery, opts.MaxPreviewLength)
		groups = append(groups, group)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query family exact group row iteration error: %w", err)
	}

	return groups, nil
}

func (c *ClickHouseReader) FetchSlowQueries(ctx context.Context, minDurationMs int, maxDurationMs int, maxQueryLength int, lookback time.Duration, limit int) ([]model.QueryLog, error) {
	return c.executeQueryLogQuery(ctx, c.slowQueriesBuilder(minDurationMs, maxDurationMs, maxQueryLength, lookback, limit))
}

func (c *ClickHouseReader) FetchSlowQueriesInRange(ctx context.Context, minDurationMs int, maxDurationMs int, maxQueryLength int, startTime, endTime time.Time, limit int) ([]model.QueryLog, error) {
	return c.executeQueryLogQuery(ctx, c.slowQueriesInRangeBuilder(minDurationMs, maxDurationMs, maxQueryLength, startTime, endTime, limit))
}

// slowQueriesBuilder assembles the *queryLogBuilder used by FetchSlowQueries.
// Split out so the wiring (duration + user + order) can be unit-tested
// without a live ClickHouse connection.
func (c *ClickHouseReader) slowQueriesBuilder(minDurationMs, maxDurationMs, maxQueryLength int, lookback time.Duration, limit int) *queryLogBuilder {
	b := c.newQueryLogBuilder(
		"event_time >= now() - INTERVAL ? SECOND AND type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		[]interface{}{int(lookback.Seconds())},
	)
	b.addDurationFilters(minDurationMs, maxDurationMs, maxQueryLength)
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)
	b.addOrderAndLimit(false, limit)
	return b
}

func (c *ClickHouseReader) slowQueriesInRangeBuilder(minDurationMs, maxDurationMs, maxQueryLength int, startTime, endTime time.Time, limit int) *queryLogBuilder {
	b := c.newQueryLogBuilder(
		"event_time >= ? AND event_time <= ? AND type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		[]interface{}{startTime, endTime},
	)
	b.addDurationFilters(minDurationMs, maxDurationMs, maxQueryLength)
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)
	b.addOrderAndLimit(true, limit)
	return b
}

// RecentQueryCandidateOptions bounds the trace-drilldown `recent` candidate
// search over system.query_log. The window is [StartTime, EndTime]; the
// duration/length posture mirrors the slow-query/backfill paths so the search
// honors the operator's existing config posture. Limit caps the candidate list.
//
// Match and NormalizedQueryHash are pushed into the SQL WHERE (before
// order/limit) so the LIMIT bounds the *matched* rows rather than a
// cost-truncated set:
//   - Match is a case-insensitive substring OR-group over the candidate
//     metadata fields (and normalizeQuery(query) when normalized support is
//     present).
//   - NormalizedQueryHash, when HasHash is set, is the explicit identity lookup
//     `normalized_query_hash = ?`. Because that column only exists when
//     normalized support is present, the builder applies it only when the
//     reader has normalized support; callers that need the
//     "unsupported on this ClickHouse" degradation must gate before calling.
type RecentQueryCandidateOptions struct {
	StartTime           time.Time
	EndTime             time.Time
	MinDurationMs       int
	MaxDurationMs       int
	MaxQueryLength      int
	Match               string
	NormalizedQueryHash uint64
	HasHash             bool
	Limit               int
}

// recentQueryCandidatesBuilder assembles the *queryLogBuilder for the `recent`
// candidate search. It reuses the slow-query column set (so normalized previews
// and full metadata are available when supported) and the same user-filter
// posture, pushes the -match / -normalized-query-hash predicates into the WHERE
// so the LIMIT bounds the matched rows, then orders most-expensive-first and
// bounds the result by Limit. It is split out so the candidate-search query can
// be unit-tested without a live ClickHouse connection. Raw query text is never
// returned to the caller: the command emits only bounded normalized previews and
// metadata.
//
// Param order is load-bearing: window, then duration/length, then user filters,
// then the hash term (one ?), then the match OR-group (one ? per term), then the
// LIMIT. Each add* helper appends its own ? params in that same order.
func (c *ClickHouseReader) recentQueryCandidatesBuilder(opts RecentQueryCandidateOptions) *queryLogBuilder {
	b := c.newQueryLogBuilder(
		"event_time >= ? AND event_time <= ? AND type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		[]interface{}{opts.StartTime, opts.EndTime},
	)
	b.addDurationFilters(opts.MinDurationMs, opts.MaxDurationMs, opts.MaxQueryLength)
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)
	// normalized_query_hash only exists when normalized support is present; the
	// command gates the unsupported-hash case before reaching here, but guard
	// the column reference defensively so a misuse can't emit a broken query.
	if opts.HasHash && c.queryLogNormalized {
		b.addCandidateHashFilter(opts.NormalizedQueryHash)
	}
	if opts.Match != "" {
		b.addCandidateMatchFilter(opts.Match, c.queryLogNormalized)
	}
	b.addCandidateOrderAndLimit(opts.Limit)
	return b
}

// FetchRecentQueryCandidates returns finished/failed query-log rows in the
// requested window, ordered most-expensive-first and bounded by Limit. It is
// the `recent` source for trace drilldown. The rows carry normalized previews
// and metadata when normalized support is available; callers must bound the
// previews before display and must not emit raw query text.
func (c *ClickHouseReader) FetchRecentQueryCandidates(ctx context.Context, opts RecentQueryCandidateOptions) ([]model.QueryLog, error) {
	return c.executeQueryLogQuery(ctx, c.recentQueryCandidatesBuilder(opts))
}

// CurrentQueryCandidateOptions bounds the trace-drilldown `current` candidate
// search over system.processes. The user filter mirrors the slow-query/recent
// posture (whitelist + blacklist) so the `current` source honors the same
// operator config as the other sources. Limit caps the candidate list.
//
// Match is an optional case-insensitive substring OR-group over the running
// query's metadata (query_id, user, client_name, client_hostname) plus the
// normalized preview when normalized support is present. There is no duration
// posture (a running query has no finished duration) and no normalized-hash
// identity (system.processes does not expose normalized_query_hash).
type CurrentQueryCandidateOptions struct {
	Match string
	Limit int
}

// currentQueryCandidatesBuilder assembles the SQL for the `current` candidate
// search over system.processes. It applies the same user-filter posture as the
// other query_log paths, pushes the optional -match needle into the WHERE
// OR-group (so the LIMIT bounds the matched rows), then orders longest-running
// first and bounds by Limit. The normalized preview match term participates
// only when normalized support is present, the same gate as the SELECT column.
// Split out so the query can be unit-tested without a live ClickHouse.
func (c *ClickHouseReader) currentQueryCandidatesBuilder(opts CurrentQueryCandidateOptions) *queryLogBuilder {
	b := &queryLogBuilder{query: c.currentQuerySelect}
	// system.processes exposes `user` directly, so the same whitelist/blacklist
	// clauses the slow-query builder uses apply unchanged.
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)
	if opts.Match != "" {
		b.addCurrentMatchFilter(opts.Match, c.queryLogNormalized)
	}
	b.query += " ORDER BY elapsed_ms DESC, query_id ASC"
	if opts.Limit > 0 {
		b.query += " LIMIT ?"
		b.params = append(b.params, opts.Limit)
	}
	return b
}

// addCurrentMatchFilter pushes the -match needle into the system.processes
// WHERE as a case-insensitive substring OR-group over the running-query
// metadata. normalizeQuery(query) participates only when includeNormalized is
// set (normalized support present) so the query never references the normalized
// expression on a ClickHouse that lacks it. system.processes has no `tables` /
// `databases` array columns, so the match is over scalar metadata only. Raw
// query text is never matched — the normalized term searches the same bounded
// preview the report would emit.
func (b *queryLogBuilder) addCurrentMatchFilter(needle string, includeNormalized bool) {
	cols := []string{"query_id", "user", "client_name", "client_hostname"}
	if includeNormalized {
		cols = append(cols, "normalizeQuery(query)")
	}
	terms := make([]string, 0, len(cols))
	for _, col := range cols {
		terms = append(terms, "positionCaseInsensitive("+col+", ?) > 0")
		b.params = append(b.params, needle)
	}
	b.query += " AND (" + strings.Join(terms, " OR ") + ")"
}

// FetchCurrentQueryCandidates returns in-flight queries from system.processes
// for the trace-drilldown `current` source, ordered longest-running first and
// bounded by Limit. The rows carry the running-query metadata (query_id,
// elapsed_ms, user/client/host/address) and a bounded normalized preview when
// normalized support is available; they have no finished query_duration_ms,
// normalized_query_hash, or event_time — the command surfaces those gaps as
// warnings rather than failing. Raw running query text is never returned.
func (c *ClickHouseReader) FetchCurrentQueryCandidates(ctx context.Context, opts CurrentQueryCandidateOptions) ([]model.QueryLog, error) {
	b := c.currentQueryCandidatesBuilder(opts)

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, b.query, b.params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query system.processes for current candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var logs []model.QueryLog
	for rows.Next() {
		var log model.QueryLog
		if err := rows.Scan(c.currentQueryScanDest(&log)...); err != nil {
			return nil, fmt.Errorf("failed to scan current candidate row: %w", err)
		}
		logs = append(logs, log)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("current candidate row iteration error: %w", err)
	}
	return logs, nil
}

// currentQueryScanDest mirrors currentQueryCandidatesSelectSQL column order.
// The optional normalized preview is spliced into the SELECT immediately after
// elapsed_ms, so it is appended at the same point here when present.
func (c *ClickHouseReader) currentQueryScanDest(log *model.QueryLog) []interface{} {
	dest := []interface{}{
		&log.QueryID,
		&log.ElapsedMs,
	}
	if c.queryLogNormalized {
		dest = append(dest, &log.NormalizedQuery)
	}
	dest = append(dest,
		&log.User,
		&log.ClientName,
		&log.ClientHostname,
		&log.ClientAddress,
	)
	return dest
}

// FetchQueryLogByQueryIDs queries system.query_log for completed queries
// matching the given query IDs, returning a map keyed by query_id.
// When multiple query_log rows exist for a query_id (QueryFinish +
// ExceptionWhileProcessing), the exception row is preferred.
//
// The join key is query_id (not trace_id) because system.query_log has no
// trace_id column in current ClickHouse versions. query_id appears in both
// tables: as a primary column in query_log, and as the clickhouse.query_id
// attribute on opentelemetry_span_log spans. This is the only enrichment
// path; setUserFilter's splice into enrichSelect therefore covers all
// query_log enrichment used by the processor.
func (c *ClickHouseReader) FetchQueryLogByQueryIDs(ctx context.Context, queryIDs []string, lookbackDays int) (map[string]model.QueryLog, error) {
	if len(queryIDs) == 0 {
		return nil, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	// LIMIT 2 BY query_id in the SQL guarantees at most 2 rows per
	// query_id (QueryFinish + ExceptionWhileProcessing); dedup below
	// keeps one. No global LIMIT needed — the IN clause bounds the set.
	params := []interface{}{queryIDs, lookbackDays}
	params = append(params, c.enrichExtraParams...)
	rows, err := c.conn.Query(queryCtx, c.enrichSelect, params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query query_log for enrichment: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]model.QueryLog, len(queryIDs))
	for rows.Next() {
		var ql model.QueryLog
		if err := rows.Scan(c.queryLogEnrichScanDest(&ql)...); err != nil {
			return nil, fmt.Errorf("failed to scan enrichment row: %w", err)
		}
		// Prefer ExceptionWhileProcessing over QueryFinish for richer
		// diagnostic info. ORDER BY event_time means QueryFinish arrives
		// first; when the exception row follows, it overwrites.
		if existing, ok := result[ql.QueryID]; ok {
			if existing.QueryKind == "ExceptionWhileProcessing" {
				continue
			}
		}
		result[ql.QueryID] = ql
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enrichment row iteration error: %w", err)
	}

	return result, nil
}

func (c *ClickHouseReader) queryLogEnrichScanDest(log *model.QueryLog) []interface{} {
	// Keep this order in lockstep with queryLogEnrichSelectSQL. Enrichment
	// intentionally omits raw query text, so optional normalized fields sit
	// immediately after query_duration_ms instead of after log.Query.
	dest := []interface{}{
		&log.QueryID,
		&log.QueryKind,
		&log.EventTime,
		&log.QueryDurationMs,
	}
	if c.queryLogNormalized {
		dest = append(dest, &log.NormalizedQueryHash, &log.NormalizedQuery)
	}
	dest = append(dest,
		&log.User,
		&log.ClientName,
		&log.ClientHostname,
		&log.ClientAddress,
		&log.DatabasesVisited,
		&log.TablesVisited,
		&log.ExceptionCode,
		&log.ReadRows,
		&log.ReadBytes,
		&log.WrittenRows,
		&log.WrittenBytes,
		&log.ResultRows,
		&log.ResultBytes,
		&log.MemoryUsage,
	)
	return dest
}

// ---------------------------------------------------------------------------
// opentelemetry_span_log queries
// ---------------------------------------------------------------------------

// FetchOpts groups optional filtering for span fetches. New options can be
// added without breaking call sites. MinTraceDurationMs is intentionally
// excluded — it is always required and remains a positional parameter.
type FetchOpts struct {
	MaxTraceDurationMs  int
	MinSpanDurationMs   int
	MaxSpanDurationMs   int
	BlacklistOperations []string // Substring matches, pushed to SQL as operation_name NOT LIKE '%x%'
}

func (c *ClickHouseReader) FetchOpenTelemetrySpans(ctx context.Context, minTraceDurationMs int, maxTraceDurationMs int, minSpanDurationMs int, maxSpanDurationMs int, lookback time.Duration, limit int) ([]model.OpenTelemetrySpan, error) {
	return c.FetchOpenTelemetrySpansWithOpts(ctx, minTraceDurationMs, lookback, limit, FetchOpts{
		MaxTraceDurationMs: maxTraceDurationMs,
		MinSpanDurationMs:  minSpanDurationMs,
		MaxSpanDurationMs:  maxSpanDurationMs,
	})
}

func (c *ClickHouseReader) FetchOpenTelemetrySpansWithOpts(ctx context.Context, minTraceDurationMs int, lookback time.Duration, limit int, opts FetchOpts) ([]model.OpenTelemetrySpan, error) {
	maxTraceDurationMs := opts.MaxTraceDurationMs
	minSpanDurationMs := opts.MinSpanDurationMs
	maxSpanDurationMs := opts.MaxSpanDurationMs
	// Two-step process:
	// 1. Find trace IDs that contain at least one span within duration
	//    range [minTraceDurationMs, maxTraceDurationMs]. The operation
	//    blacklist is NOT applied here — it only filters step 2. If a
	//    trace's only qualifying spans are all blacklisted, the trace ID
	//    consumes a LIMIT slot but returns zero spans. This is acceptable
	//    because min_trace_duration_ms filters on duration (typically 1s+)
	//    and blacklisted operations are sub-millisecond internal spans
	//    that never qualify.
	// 2. Fetch spans for those traces, filtered by duration range and
	//    operation blacklist. Step 2's LIMIT is the operator-configured
	//    monitor.max_spans_per_cycle and bounds spans actually exported.
	//    When the cap is hit, partial traces continue across cycles via
	//    lookback overlap + dedup cache.

	lookbackDays := int(lookback.Hours()/24) + 1

	// Resolve once so both steps share the same effective cap, including
	// the MaxSpanQueryLimit safety ceiling and the unset/<=0 fallback.
	effectiveLimit := resolveSpanLimit(limit)

	// Step 1: Get trace IDs with spans within duration range
	traceQuery := c.traceIDSelect
	traceParams := []interface{}{lookbackDays, int(lookback.Seconds()), minTraceDurationMs}

	if maxTraceDurationMs > 0 {
		traceQuery += " AND (finish_time_us - start_time_us) <= ? * 1000"
		traceParams = append(traceParams, maxTraceDurationMs)
	}

	traceQuery += `
			)
			GROUP BY trace_id
			ORDER BY max_finish_time DESC`

	traceQuery += " LIMIT ?"
	traceParams = append(traceParams, effectiveLimit)

	traceCtx, traceCancel := context.WithTimeout(ctx, c.queryTimeout)
	defer traceCancel()

	rows, err := c.conn.Query(traceCtx, traceQuery, traceParams...)
	if err != nil {
		return nil, fmt.Errorf("failed to query trace IDs: %w", err)
	}

	var traceIDs []uuid.UUID
	for rows.Next() {
		var traceID uuid.UUID
		var maxFinishTime uint64 // Not used, but need to scan it
		if err := rows.Scan(&traceID, &maxFinishTime); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("failed to scan trace ID: %w", err)
		}
		traceIDs = append(traceIDs, traceID)
	}
	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace ID iteration error: %w", err)
	}

	if len(traceIDs) == 0 {
		return []model.OpenTelemetrySpan{}, nil
	}

	if minSpanDurationMs > 0 || maxSpanDurationMs > 0 {
		clicklog.Debug("Found %d traces with slow spans, fetching spans with duration filters for these traces", len(traceIDs))
	} else {
		clicklog.Debug("Found %d traces with slow spans, fetching all spans for these traces", len(traceIDs))
	}

	// Step 2: Fetch spans for those traces, optionally filtered by duration range
	spansQuery := c.spanSelect
	spanParams := []interface{}{traceIDs, lookbackDays}

	// 0 = fetch all spans (no lower bound)
	if minSpanDurationMs > 0 {
		spansQuery += " AND (finish_time_us - start_time_us) >= ? * 1000"
		spanParams = append(spanParams, minSpanDurationMs)
	}

	if maxSpanDurationMs > 0 {
		spansQuery += " AND (finish_time_us - start_time_us) <= ? * 1000"
		spanParams = append(spanParams, maxSpanDurationMs)
	}

	// Operation blacklist, pushed down as SQL NOT LIKE (no ReDoS risk, excludes
	// known-noise internal ops like MergeTreeIndex). Bound as %pattern%, so a
	// literal % or _ inside a configured pattern is treated as a LIKE wildcard,
	// not matched literally — see config docs for blacklist_operations.
	for _, pattern := range opts.BlacklistOperations {
		if pattern == "" {
			continue
		}
		spansQuery += " AND operation_name NOT LIKE ?"
		spanParams = append(spanParams, "%"+pattern+"%")
	}

	spansQuery += " LIMIT ?"
	spanParams = append(spanParams, effectiveLimit)

	spansCtx, spansCancel := context.WithTimeout(ctx, c.queryTimeout)
	defer spansCancel()

	rows, err = c.conn.Query(spansCtx, spansQuery, spanParams...)
	if err != nil {
		return nil, fmt.Errorf("failed to query spans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var spans []model.OpenTelemetrySpan
	for rows.Next() {
		var span model.OpenTelemetrySpan
		if err := rows.Scan(
			&span.Hostname,
			&span.TraceID,
			&span.SpanID,
			&span.ParentSpanID,
			&span.OperationName,
			&span.Kind,
			&span.StartTimeUs,
			&span.FinishTimeUs,
			&span.FinishDate,
			&span.Attributes,
		); err != nil {
			return nil, fmt.Errorf("failed to scan span row: %w", err)
		}
		spans = append(spans, span)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("span row iteration error: %w", err)
	}

	return spans, nil
}

// FetchTraceIDsByQueryID returns the distinct trace IDs whose spans carry the
// given ClickHouse query_id span attribute, bounded by lookbackDays partition
// pruning and limit. This is the trace-drilldown bridge from a query identity
// to native trace spans; it does not run duration-based trace selection. An
// empty queryID returns no trace IDs and no error. Trace IDs are returned as
// canonical UUID strings so the CLI can echo them and pass them back into the
// span fetch.
func (c *ClickHouseReader) FetchTraceIDsByQueryID(ctx context.Context, queryID string, lookbackDays, limit int) ([]string, error) {
	if queryID == "" {
		return nil, nil
	}
	if lookbackDays < 1 {
		lookbackDays = 1
	}
	effectiveLimit := resolveSpanLimit(limit)

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, c.traceIDByQueryID, lookbackDays, queryID, effectiveLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to query trace IDs by query_id: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var traceIDs []string
	for rows.Next() {
		var traceID uuid.UUID
		if err := rows.Scan(&traceID); err != nil {
			return nil, fmt.Errorf("failed to scan trace ID: %w", err)
		}
		traceIDs = append(traceIDs, traceID.String())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace ID iteration error: %w", err)
	}
	return traceIDs, nil
}

// FetchSpansForTraceIDs fetches every available span for the given trace IDs,
// bounded by lookbackDays partition pruning, limit, and the operation
// blacklist. Unlike FetchOpenTelemetrySpansWithOpts it does NOT re-run
// duration-based trace selection: the caller has already named the exact
// traces to inspect, so the command shows all of their spans. Trace IDs are
// accepted as strings and parsed to UUIDs; unparseable IDs are skipped. An
// empty (or all-invalid) trace-ID set returns no spans and no error.
func (c *ClickHouseReader) FetchSpansForTraceIDs(ctx context.Context, traceIDs []string, lookbackDays, limit int, blacklistOperations []string) ([]model.OpenTelemetrySpan, error) {
	if len(traceIDs) == 0 {
		return []model.OpenTelemetrySpan{}, nil
	}
	if lookbackDays < 1 {
		lookbackDays = 1
	}
	parsed := make([]uuid.UUID, 0, len(traceIDs))
	for _, id := range traceIDs {
		u, err := uuid.Parse(id)
		if err != nil {
			// Skip unparseable IDs rather than failing: a drilldown for one
			// bad -trace-id value should still return the valid ones.
			continue
		}
		parsed = append(parsed, u)
	}
	if len(parsed) == 0 {
		return []model.OpenTelemetrySpan{}, nil
	}

	effectiveLimit := resolveSpanLimit(limit)

	spansQuery := c.spanSelect
	spanParams := []interface{}{parsed, lookbackDays}

	// Operation blacklist, pushed down as SQL NOT LIKE, identical posture to
	// the scheduled span fetch. No duration filters: the operator asked for a
	// specific trace, so every non-blacklisted span is in scope.
	for _, pattern := range blacklistOperations {
		if pattern == "" {
			continue
		}
		spansQuery += " AND operation_name NOT LIKE ?"
		spanParams = append(spanParams, "%"+pattern+"%")
	}

	spansQuery += " LIMIT ?"
	spanParams = append(spanParams, effectiveLimit)

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, spansQuery, spanParams...)
	if err != nil {
		return nil, fmt.Errorf("failed to query spans for trace IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var spans []model.OpenTelemetrySpan
	for rows.Next() {
		var span model.OpenTelemetrySpan
		if err := rows.Scan(
			&span.Hostname,
			&span.TraceID,
			&span.SpanID,
			&span.ParentSpanID,
			&span.OperationName,
			&span.Kind,
			&span.StartTimeUs,
			&span.FinishTimeUs,
			&span.FinishDate,
			&span.Attributes,
		); err != nil {
			return nil, fmt.Errorf("failed to scan span row: %w", err)
		}
		spans = append(spans, span)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("span row iteration error: %w", err)
	}
	return spans, nil
}

// RunCanaryQuery runs a cheap COUNT query to check if long-running spans
// exist today. Uses a hardcoded 5s timeout to keep canary queries lightweight.
// Note: only counts spans with finish_date = today(); spans from previous
// calendar days are excluded even if they're still within the lookback window.
func (c *ClickHouseReader) RunCanaryQuery(ctx context.Context, thresholdMs int) (model.CanaryResult, error) {
	canaryCtx, cancel := context.WithTimeout(ctx, canaryQueryTimeout)
	defer cancel()

	var count int64
	err := c.conn.QueryRow(canaryCtx, c.canarySelect, thresholdMs).Scan(&count)
	if err != nil {
		return model.CanaryResult{}, fmt.Errorf("canary query failed: %w", err)
	}

	return model.CanaryResult{
		Count:            count,
		LongQueriesExist: count > 0,
	}, nil
}

// CountDistinctClusterReaders counts how many distinct hosts are *currently*
// running whole-cluster span reads — cluster('name', system.opentelemetry_span_log)
// — i.e. hosts whose MOST RECENT such read is within `activeWindow`. It is the
// query_log trigger for the topology self-audit: >1 distinct host means more
// than one instance is doing cluster-wide reads (the sidecar +
// use_cluster_queries anti-pattern), which works even with HA off (no Keeper to
// count siblings).
//
// Two windows, two jobs:
//   - lookback: the coarse scan bound (outer event_time + the event_date
//     partition prune) — how much query_log is read across replicas.
//   - activeWindow: the recency trigger (HAVING on max(event_time)) — a host is
//     counted only if it is *still* reading this fresh. This is the "concurrently
//     reading now" signal, not "read at all in the last lookback". A host that
//     read once and stopped — the startup fail-open cycle, a departed leader
//     after failover, a one-off backfill from another box — ages out of the
//     active window within ~one poll interval and drops from the count, so it
//     cannot linger a full lookback and trip the debounce. See issue #223.
//
// It scans clusterAllReplicas('cluster', system.query_log) — NOT the reader's
// normal queryLogRef, which is cluster(...) (one replica per shard). In a
// replicated cluster, sidecars on non-selected replicas would be invisible to
// a cluster() scan and the count could collapse to a single host, missing
// exactly the fleet this signal exists to catch. clusterAllReplicas hits every
// replica/node.
//
// lookback and activeWindow are explicit arguments (the auditor passes
// monitor.topology_audit.query_log_lookback_minutes and the active window it
// derives from the poll cadence) so neither window can drift to a dead default.
//
// Empty cluster guard: buildClusterAllReplicasRef has no empty-name guard and
// would emit clusterAllReplicas(”, …) (invalid SQL). Validate() has required a
// non-empty clickhouse.cluster with use_cluster_queries since #218, so this is
// defense-in-depth rather than a live path; return (0, nil) immediately.
//
// The match (query text contains both `cluster(` and `opentelemetry_span_log`)
// is written with `||` so this probe's own SQL text does not contain either
// substring verbatim — otherwise the scan would count its own query_log probes
// (and sibling auditors'). Blank client_hostname rows are ignored; they can
// only ever under-count, never produce a false positive. readonly=2 is enforced
// at the connection (see NewClickHouseReader).
//
// NOTE: the match is coupled to the `cluster(` spelling of the span read
// (buildTableRef). A future span-read path that wrapped the table in
// clusterAllReplicas(...) instead would NOT contain `cluster(` and would be
// invisible to this scan (false negative). Keep this match in sync if the
// span-read table reference ever changes.
func (c *ClickHouseReader) CountDistinctClusterReaders(ctx context.Context, lookback, activeWindow time.Duration) (int, error) {
	if c.cluster == "" {
		return 0, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	// event_date (partition key) is rounded up by a day for pruning; the precise
	// event_time bound matches the actual lookback window.
	lookbackDays := int(lookback.Hours()/24) + 1
	lookbackSeconds := int(lookback.Seconds())
	activeWindowSeconds := int(activeWindow.Seconds())

	// Group the matching reads by host over the coarse lookback, then count only
	// hosts whose newest read falls inside the active window (HAVING max(event_time)).
	// That makes a host that read once and went idle drop out of the count once its
	// last read ages past activeWindow — see the activeWindow note above.
	query := `
		SELECT count() AS readers
		FROM (
			SELECT client_hostname
			FROM ` + buildClusterAllReplicasRef("system.query_log", c.cluster) + `
			WHERE event_date >= today() - INTERVAL ? DAY
			  AND event_time >= now() - INTERVAL ? SECOND
			  AND client_hostname != ''
			  AND position(query, 'cluster' || '(') > 0
			  AND position(query, 'opentelemetry_span' || '_log') > 0
			GROUP BY client_hostname
			HAVING max(event_time) >= now() - INTERVAL ? SECOND
		)
	`

	var readers uint64
	if err := c.conn.QueryRow(queryCtx, query, lookbackDays, lookbackSeconds, activeWindowSeconds).Scan(&readers); err != nil {
		return 0, fmt.Errorf("distinct cluster-reader scan failed: %w", err)
	}
	return int(readers), nil
}

// HasRecentQueryLogRows reports whether the LOCAL node's system.query_log has
// any rows inside the lookback window. It is the sanity probe behind the
// topology audit's query_log-blindness warning (#238): a zero-reader scan is
// ambiguous between "no duplicate readers" and "query_log disabled or outside
// retention", and this disambiguates it once, cheaply. Deliberately local — no
// clusterAllReplicas fan-out — so a remote replica with logging disabled is not
// caught; that residual under-count is accepted (the warning is best-effort,
// observability-only). readonly=2 is enforced at the connection.
func (c *ClickHouseReader) HasRecentQueryLogRows(ctx context.Context, lookback time.Duration) (bool, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	lookbackDays := int(lookback.Hours()/24) + 1
	lookbackSeconds := int(lookback.Seconds())

	query := `
		SELECT count() AS rows
		FROM system.query_log
		WHERE event_date >= today() - INTERVAL ? DAY
		  AND event_time >= now() - INTERVAL ? SECOND
	`

	var rows uint64
	if err := c.conn.QueryRow(queryCtx, query, lookbackDays, lookbackSeconds).Scan(&rows); err != nil {
		return false, fmt.Errorf("query_log sanity probe failed: %w", err)
	}
	return rows > 0, nil
}
