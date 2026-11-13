package clickhouse

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	oteltrace "go.opentelemetry.io/otel/trace"

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
const queryLogOperationColumn = "{QUERY_LOG_OPERATION_COLUMN}"
const processesNormalizedColumn = "{PROCESSES_NORMALIZED_COLUMN}"

// tracingTestQuery is deliberately constant: the operator smoke test must be
// safe, cheap, and recognizable in ClickHouse diagnostics.
const tracingTestQuery = "SELECT 1 /* click-dog test tracing */"

// serverVersionQuery reads the connected server's build version. Like the
// smoke test it is constant and comment-tagged so it is recognizable in
// ClickHouse diagnostics, and it is permitted under the enforced readonly=2
// connection mode.
const serverVersionQuery = "SELECT version() /* click-dog server version */"

// serverTimeQuery reads the server's clock in the span log's microseconds. Like
// serverVersionQuery it is constant, comment-tagged, and readonly=2-safe.
const serverTimeQuery = "SELECT toUnixTimestamp64Micro(now64(6)) /* click-dog server time */"

// enrichSelectAnchor is a dedicated marker setUserFilter replaces with the
// whitelist clause (or strips entirely). Kept distinct from any functional
// SQL token (e.g. ORDER BY event_time) so a future template edit can't
// accidentally collide.
const enrichSelectAnchor = "-- {USER_FILTER_SPLICE}"

const queryLogNormalizedColumnsSQL = `
				normalized_query_hash,
				normalizeQuery(query) AS normalized_query,`

const queryLogOperationColumnSQL = `
				query_kind AS query_operation,`

const normalizedQueryHashProbeSQL = `
			SELECT count()
			FROM system.columns
			WHERE database = 'system'
			  AND table = 'query_log'
			  AND name = 'normalized_query_hash'
		`

const queryKindProbeSQL = `
			SELECT count()
			FROM system.columns
			WHERE database = 'system'
			  AND table = 'query_log'
			  AND name = 'query_kind'
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

const queryKindClusterProbeSQL = `
			SELECT count() AS replicas, countIf(has_query_kind) AS supported
			FROM (
				SELECT
					hostName() AS host,
					countIf(name = 'query_kind') > 0 AS has_query_kind
				FROM {TABLE_SYSTEM_COLUMNS}
				WHERE database = 'system'
				  AND table = 'query_log'
				GROUP BY host
			)
		`

const queryLogSelectSQL = `
			SELECT
				query_id,
				type, -- lifecycle event; leave query_kind for the statement operation
				` + queryLogOperationColumn + `
				event_time,
				query_duration_ms,
				query,
				` + queryLogNormalizedColumns + `
				user,
				initial_user,
				client_name,
				client_hostname,
				IPv6NumToString(address) as client_address,
				IPv6NumToString(initial_address) as initial_address,
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
			SELECT trace_id, max(finish_time) as max_finish_time, min(start_time) as min_start_time
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
// logical subject of the lookup. The finish_time_us floor keeps the read to
// the requested window: the span log is sorted by
// (finish_date, finish_time_us, trace_id), so without it every granule since
// the day floor is read for the attribute map. min_start_time is the floor of
// the span fetch that follows.
const traceIDByQueryIDSelectSQL = `
			SELECT trace_id, min(start_time_us) AS min_start_time
			FROM ` + tblSpanLog + `
			WHERE finish_date >= today() - INTERVAL ? DAY
			  AND finish_time_us >= ?
			  AND attribute['clickhouse.query_id'] = ?
			GROUP BY trace_id
			LIMIT ?
		`

// traceNodesSelectSQL is the inverse bridge: the spans recorded for a trace
// that carry a query ID, with their statements and their place in the span
// tree. The trace-scoped filters use it so a trace is decided from its
// queries, whichever span page each one landed in, and so each span can be
// traced to the query span it runs under. trace_id is the last sort-key
// column, so the finish_time_us floor is what keeps this from reading every
// granule since the day floor. The statement ends open: FetchTraceNodes adds
// traceAncestorsSQL when it asks for ancestors, then the closing parenthesis.
const traceNodesSelectSQL = `
			SELECT trace_id, span_id, parent_span_id, operation_name = 'query' AS is_query,
				attribute['clickhouse.query_id'] AS query_id,
				if(attribute['clickhouse.query_id'] != '', attribute['db.statement'], '') AS statement
			FROM ` + tblSpanLog + `
			WHERE trace_id IN ?
			  AND finish_date >= today() - INTERVAL ? DAY
			  AND finish_time_us >= ?
			  AND (attribute['clickhouse.query_id'] != ''`

// traceAncestorsSQL widens traceNodesSelectSQL to the spans that can be
// ancestors of the caller's spans: started by the asked time, and the parent
// of another span of the asked traces since the same floor. Leaves, which are
// most of a trace's spans, are never returned. The subquery reads the same
// granules and only the span tree columns. GLOBAL runs it once: in cluster
// mode a plain IN would send the cluster() subquery to every shard, and each
// would read every shard again.
const traceAncestorsSQL = `
			    OR (start_time_us <= ? AND span_id GLOBAL IN (
			      SELECT parent_span_id FROM ` + tblSpanLog + `
			      WHERE trace_id IN ?
			        AND finish_date >= today() - INTERVAL ? DAY
			        AND finish_time_us >= ?))`

// queryOriginsSelectSQL resolves who ran each query and from where, for the
// trace-scoped user and IP filters. It reads every lifecycle row from its
// event_time floor, which holds the finish row of each query whose query span
// the filters read, and keeps it from reading every granule since the day
// floor, as for queryLogEnrichSelectSQL. It is never narrowed by the user
// whitelist splice the enrichment SQL carries: the filters must see every
// query they read for a trace to decide it.
const queryOriginsSelectSQL = `
			SELECT query_id, any(user), any(initial_user),
				any(IPv6NumToString(address)), any(IPv6NumToString(initial_address))
			FROM ` + tblQueryLog + `
			WHERE query_id IN ?
			  AND event_date >= today() - ?
			  AND event_time >= toDateTime(?)
			GROUP BY query_id
		`

// queryLogEnrichSelectSQL fetches query_log rows for enrichment by query_id.
// Query text is intentionally omitted — it can be large and sensitive, and
// db.statement already carries the SQL from the span attributes.
//
// Note: system.query_log does not have a trace_id column (as of CH 24.1+).
// The join key is query_id, which appears in both query_log (primary column)
// and opentelemetry_span_log (via the clickhouse.query_id span attribute).
// query_log is sorted by (event_date, event_time) and query_id is not in the
// key, so the event_time floor is what keeps this from reading every granule
// since the day floor.
const queryLogEnrichSelectSQL = `
			SELECT
				query_id,
				type, -- lifecycle event; leave query_kind for the statement operation
				` + queryLogOperationColumn + `
				event_time,
				query_duration_ms,
				` + queryLogNormalizedColumns + `
				user,
				initial_user,
				client_name,
				client_hostname,
				IPv6NumToString(address) as client_address,
				IPv6NumToString(initial_address) as initial_address,
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
			  AND event_time >= toDateTime(?)
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
				countIf(type = 'QueryFinish') AS successful_count,
				countIf(type = 'ExceptionWhileProcessing') AS failed_count,
				toFloat64(quantileTDigest(0.95)(query_duration_ms)) AS p95_duration_ms,
				toFloat64(quantileTDigest(0.99)(query_duration_ms)) AS p99_duration_ms,
				tupleElement(sumMap([toInt64(exception_code)], [toUInt64(type = 'ExceptionWhileProcessing')]), 1) AS exception_codes,
				tupleElement(sumMap([toInt64(exception_code)], [toUInt64(type = 'ExceptionWhileProcessing')]), 2) AS exception_counts,
				max(memory_usage) AS max_memory_usage,
				toFloat64(quantileTDigest(0.95)(read_rows)) AS p95_read_rows,
				toFloat64(quantileTDigest(0.95)(read_bytes)) AS p95_read_bytes,
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

// Compile-time check that *ClickHouseReader satisfies the optional
// model.ServerVersionReader companion. The processor reaches it through a
// runtime type assertion, so without this a signature drift would silently
// stop stamping clickhouse.version instead of failing the build.
var _ model.ServerVersionReader = (*ClickHouseReader)(nil)

type ClickHouseReader struct {
	conn         driver.Conn
	queryTimeout time.Duration

	// liveMu guards the committed Live Span Selection walk and the page a
	// fetch holds pending until CommitLiveSpans.
	liveMu      sync.Mutex
	live        liveWalk
	livePending *liveWalk
	// liveLostThrough is the span-log finish time, in microseconds, up to
	// which the walk has already reported rows it can no longer read.
	liveLostThrough uint64
	// now is this host's clock; nil means time.Now. Tests replace it.
	now func() time.Time

	// Precomputed SQL templates with table references resolved at init.
	queryLogSelect     string
	traceIDSelect      string
	traceIDByQueryID   string
	traceNodes         string
	traceAncestors     string
	spanSelect         string
	canarySelect       string
	enrichSelect       string
	queryOriginsSelect string
	queryFamilySelect  string
	currentQuerySelect string
	queryLogNormalized bool
	queryLogOperation  bool

	// Resolved table references (cluster-aware) reused by the readiness
	// doctor to build its diagnostic SQL without re-deriving cluster mode.
	spanLogRef        string
	queryLogRef       string
	processesRef      string
	useClusterQueries bool
	cluster           string

	// enrichExtraParams holds the []string slice bound to setUserFilter's
	// spliced `user IN ?` placeholder. Blacklist isn't spliced into
	// enrichSelect — see setUserFilter.
	enrichExtraParams []interface{}

	// Raw user-filter lists for the builder-based slow-query paths.
	whitelistUsers []string
	blacklistUsers []string

	// blacklist_queries patterns and whitelist_ips prefixes (normalized by
	// config.ClientPrefix) for the same paths; see addRowFilters.
	blacklistQueries []string
	clientPrefixes   []string
}

// liveWalk is scheduled mode's committed position in Live Span Selection. The
// walk runs through the lookback window oldest first: every qualifying trace
// keyed at or below the watermark has been delivered, and a trace page whose
// spans outran the cap is drained across fetches behind the span cursor. Only
// CommitLiveSpans moves it, so a page that was fetched but not delivered is
// read again. The dedup cache still owns delivery identity; the walk only
// decides which rows a capped fetch spends its budget on.
type liveWalk struct {
	signature liveWalkSignature

	// watermark is nil until the first page drains; the walk then starts at
	// the lookback floor. A watermark older than the floor needs no clamp:
	// trace selection never returns rows outside the window.
	watermark *traceKey

	page     []uuid.UUID
	pageLast traceKey
	// pageStart is the earliest start of a qualifying span in page, which
	// bounds how far back the page's spans are read.
	pageStart uint64
	spanAfter *spanKey

	// headAt is when, by this host's clock, the latest fetch that reached the
	// head of the window started. Every trace that had settled by then was
	// delivered, so on a quiet span log the walk has read well past its
	// watermark. Zero until a fetch reaches the head.
	headAt time.Time
}

// traceKey orders trace pages: a trace's newest qualifying finish time, with
// the trace ID breaking ties between traces finishing in the same microsecond.
type traceKey struct {
	finish uint64
	id     uuid.UUID
}

// spanKey orders span pages by (finish time, trace ID, span ID).
type spanKey struct {
	finish uint64
	trace  uuid.UUID
	span   uint64
}

// traceBounds restricts a trace page by traceKey. Nil bounds are open.
type traceBounds struct {
	after      *traceKey
	before     *traceKey
	atOrBefore *traceKey
	// finishedFrom additionally ignores qualifying spans that finished before
	// it. It narrows a widened window without changing any trace's key.
	finishedFrom *uint64
}

type liveWalkSignature struct {
	minTraceDurationMs int
	maxTraceDurationMs int
	minSpanDurationMs  int
	maxSpanDurationMs  int
	lookback           time.Duration
	limit              int
	blacklist          string
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

func buildClusterQueryKindProbeSQL(cluster string) string {
	return strings.ReplaceAll(queryKindClusterProbeSQL, "{TABLE_SYSTEM_COLUMNS}", buildClusterAllReplicasRef("system.columns", cluster))
}

func buildQueryLogSQL(template, tableRef string, includeNormalized bool) string {
	return buildQueryLogSQLWithCapabilities(template, tableRef, includeNormalized, false)
}

func buildQueryLogSQLWithCapabilities(template, tableRef string, includeNormalized, includeOperation bool) string {
	normalizedColumns := ""
	if includeNormalized {
		normalizedColumns = queryLogNormalizedColumnsSQL
	}
	operationColumn := ""
	if includeOperation {
		operationColumn = queryLogOperationColumnSQL
	}

	query := strings.ReplaceAll(template, tblQueryLog, tableRef)
	query = strings.ReplaceAll(query, queryLogNormalizedColumns, normalizedColumns)
	return strings.ReplaceAll(query, queryLogOperationColumn, operationColumn)
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
			clicklog.Warn("Normalized query_log fields disabled in cluster mode: no cluster name configured")
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
			clicklog.Warn("Normalized query_log fields disabled in cluster mode: normalized_query_hash found on %d/%d replicas", supported, replicas)
			return false
		}
		clicklog.Info("Normalized query_log fields enabled in cluster mode: normalized_query_hash found on all %d replicas", replicas)
		return true
	}

	var count uint64
	if err := conn.QueryRow(ctx, normalizedQueryHashProbeSQL).Scan(&count); err != nil {
		clicklog.Warn("Could not detect query_log.normalized_query_hash support; normalized query attributes disabled: %v", err)
		return false
	}
	return count > 0
}

func detectQueryLogOperationSupport(ctx context.Context, conn driver.Conn, cluster string, useClusterQueries bool) bool {
	if useClusterQueries {
		if cluster == "" {
			clicklog.Warn("Query operation enrichment disabled in cluster mode: no cluster name configured")
			return false
		}
		var replicas, supported uint64
		if err := conn.QueryRow(ctx, buildClusterQueryKindProbeSQL(cluster)).Scan(&replicas, &supported); err != nil {
			clicklog.Warn("Could not verify query_log.query_kind support across cluster replicas; query operation enrichment disabled: %v", err)
			return false
		}
		if replicas == 0 {
			clicklog.Warn("Could not verify any cluster replicas expose system.query_log; query operation enrichment disabled")
			return false
		}
		if supported != replicas {
			clicklog.Warn("Query operation enrichment disabled in cluster mode: query_kind found on %d/%d replicas", supported, replicas)
			return false
		}
		clicklog.Info("Query operation enrichment enabled in cluster mode: query_kind found on all %d replicas", replicas)
		return true
	}

	var count uint64
	if err := conn.QueryRow(ctx, queryKindProbeSQL).Scan(&count); err != nil {
		clicklog.Warn("Could not detect query_log.query_kind support; query operation enrichment disabled: %v", err)
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

// runCapabilityProbe gives each startup capability check its own complete
// timeout budget. Keeping context creation and cancellation together prevents
// a slow earlier probe from silently consuming the budget of later probes.
func runCapabilityProbe(timeoutS int, probe func(context.Context) bool) bool {
	ctx, cancel := capabilityProbeContext(timeoutS)
	defer cancel()
	return probe(ctx)
}

// NewClickHouseReader opens a connection, prepares the SQL templates, and
// applies the user-filter splice atomically — there is no separate post-
// construction step a caller can forget.
func NewClickHouseReader(cfg config.ClickHouseConfig, filters config.FiltersConfig) (*ClickHouseReader, error) {
	return newClickHouseReader(context.Background(), cfg, filters, true)
}

// NewTracingTestReader opens the normal native-protocol connection under ctx
// but skips the query-log capability probes used by scheduled processing. The
// tracing smoke test needs only the exact span-log read path, and avoiding
// unrelated probes keeps its workload and command-level deadline explicit.
func NewTracingTestReader(ctx context.Context, cfg config.ClickHouseConfig) (*ClickHouseReader, error) {
	return newClickHouseReader(ctx, cfg, config.FiltersConfig{}, false)
}

func newClickHouseReader(ctx context.Context, cfg config.ClickHouseConfig, filters config.FiltersConfig, probeCapabilities bool) (*ClickHouseReader, error) {
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
		// MinVersion is Go's current client default, stated explicitly so the
		// floor is a property of this config rather than of the toolchain. Not
		// a behavior change; see internal/export/otel.go for the same note.
		tlsConfig := &tls.Config{
			MinVersion:         tls.VersionTLS12,
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

	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
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

	var queryLogNormalized, queryLogOperation bool
	if probeCapabilities {
		queryLogNormalized = runCapabilityProbe(cfg.QueryTimeoutS, func(ctx context.Context) bool {
			return detectQueryLogNormalizedSupport(ctx, conn, cfg.Cluster, useClusterQueryLog)
		})
		queryLogOperation = runCapabilityProbe(cfg.QueryTimeoutS, func(ctx context.Context) bool {
			return detectQueryLogOperationSupport(ctx, conn, cfg.Cluster, useClusterQueryLog)
		})
	}

	r := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Duration(cfg.QueryTimeoutS) * time.Second,
		queryLogSelect:     buildQueryLogSQLWithCapabilities(queryLogSelectSQL, queryLogRef, queryLogNormalized, queryLogOperation),
		traceIDSelect:      strings.ReplaceAll(traceIDSelectSQL, tblSpanLog, spanLogRef),
		traceIDByQueryID:   strings.ReplaceAll(traceIDByQueryIDSelectSQL, tblSpanLog, spanLogRef),
		traceNodes:         strings.ReplaceAll(traceNodesSelectSQL, tblSpanLog, spanLogRef),
		traceAncestors:     strings.ReplaceAll(traceAncestorsSQL, tblSpanLog, spanLogRef),
		spanSelect:         strings.ReplaceAll(spanSelectSQL, tblSpanLog, spanLogRef),
		canarySelect:       strings.ReplaceAll(canarySelectSQL, tblSpanLog, spanLogRef),
		enrichSelect:       buildQueryLogSQLWithCapabilities(queryLogEnrichSelectSQL, queryLogRef, queryLogNormalized, queryLogOperation),
		queryOriginsSelect: strings.ReplaceAll(queryOriginsSelectSQL, tblQueryLog, queryLogRef),
		queryFamilySelect:  strings.ReplaceAll(queryFamilyExactGroupsSQL, tblQueryLog, queryLogRef),
		currentQuerySelect: buildCurrentQuerySQL(currentQueryCandidatesSelectSQL, processesRef, queryLogNormalized),
		queryLogNormalized: queryLogNormalized,
		queryLogOperation:  queryLogOperation,
		spanLogRef:         spanLogRef,
		queryLogRef:        queryLogRef,
		processesRef:       processesRef,
		useClusterQueries:  cfg.UseClusterQueries,
		cluster:            cfg.Cluster,
	}
	r.setUserFilter(filters.WhitelistUsers, filters.BlacklistUsers)
	if err := r.setRowFilters(filters.BlacklistQueries, filters.WhitelistIPs); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return r, nil
}

// ExecuteTracingTestQuery submits the tracing smoke-test canary with both the
// sampled upstream span context and a diagnostic query ID attached through
// clickhouse-go's native query options. An active Go span alone is not enough
// for native-protocol propagation.
func (c *ClickHouseReader) ExecuteTracingTestQuery(ctx context.Context, spanContext oteltrace.SpanContext, queryID string) error {
	queryCtx := clickhouse.Context(
		ctx,
		clickhouse.WithSpan(spanContext),
		clickhouse.WithQueryID(queryID),
	)

	var result uint8
	if err := c.conn.QueryRow(queryCtx, tracingTestQuery).Scan(&result); err != nil {
		return fmt.Errorf("tracing test query failed: %w", err)
	}
	if result != 1 {
		return fmt.Errorf("tracing test query returned %d, want 1", result)
	}
	return nil
}

func (c *ClickHouseReader) Close() error {
	return c.conn.Close()
}

// QueryLogNormalizedSupported reports whether the connected ClickHouse
// exposes system.query_log.normalized_query_hash safely. In cluster mode
// this requires every replica returned by clusterAllReplicas() to expose
// the column, avoiding mixed-version SELECT failures. The probe runs once in
// NewClickHouseReader; this accessor lets startup wiring mirror the result into
// a metric without re-probing.
func (c *ClickHouseReader) QueryLogNormalizedSupported() bool {
	return c.queryLogNormalized
}

// QueryLogOperationSupported reports whether the connected ClickHouse exposes
// system.query_log.query_kind safely. In cluster mode every replica must
// expose the column. Startup wiring uses this result for the
// query_operation_supported self-metric without issuing another probe.
func (c *ClickHouseReader) QueryLogOperationSupported() bool {
	return c.queryLogOperation
}

// setUserFilter records the configured user lists and splices the whitelist
// clause, on the originating user, into the enrichment SQL as defense in
// depth: the live path decides both lists per trace from its own unfiltered
// FetchQueryOrigins lookup, and enrichment only adds metadata to spans of
// traces that passed. The blacklist is not spliced, so enrichment rows stay
// available for every allowed trace. The backfill and analyze builders keep
// both clauses, with the other row filters (addRowFilters), because they
// return rows directly. Always
// strips the build-time anchor on exit so it doesn't appear in CH's
// query_log.
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
		c.enrichSelect = strings.Replace(c.enrichSelect, enrichSelectAnchor, "AND "+originatingUserSQL+" IN ?", 1)
	}
}

// setRowFilters records blacklist_queries and whitelist_ips for the builders
// that return query_log or system.processes rows directly (addRowFilters).
// The live path decides both per trace instead (processor.FilteredTraces), so
// the enrichment SQL does not carry them.
func (c *ClickHouseReader) setRowFilters(blacklistQueries, whitelistIPs []string) error {
	c.blacklistQueries = append([]string(nil), blacklistQueries...)
	c.clientPrefixes = make([]string, 0, len(whitelistIPs))
	for _, entry := range whitelistIPs {
		prefix, err := config.ClientPrefix(entry)
		if err != nil {
			return fmt.Errorf("invalid whitelist_ips entry %q: %w", entry, err)
		}
		c.clientPrefixes = append(c.clientPrefixes, prefix.String())
	}
	return nil
}

// Ping pings ClickHouse within healthCheckTimeout and returns the failure
// without logging it. The /readyz probe uses it: probes run at the prober's
// rate, so the health server logs a failing ping once per change of state
// rather than on every call.
func (c *ClickHouseReader) Ping(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	return c.conn.Ping(pingCtx)
}

// IsHealthy pings ClickHouse and logs a warning when the ping fails. The
// per-cycle health check and `click-dog check` use it, so the warning follows
// the cycle rate.
func (c *ClickHouseReader) IsHealthy(ctx context.Context) bool {
	if err := c.Ping(ctx); err != nil {
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
// addUserFilters applies the user lists to query_log rows. The whitelist
// judges the originating client's account (initial_user), so a distributed
// query's secondary rows, which run as the interserver account, follow their
// client. The blacklist matches either account, so a blacklisted client's
// secondary rows and a blacklisted interserver account are both excluded.
func (b *queryLogBuilder) addUserFilters(whitelist, blacklist []string) {
	if len(whitelist) > 0 {
		b.query += " AND " + originatingUserSQL + " IN ?"
		b.params = append(b.params, whitelist)
	}
	if len(blacklist) > 0 {
		b.query += " AND user NOT IN ? AND initial_user NOT IN ?"
		b.params = append(b.params, blacklist, blacklist)
	}
}

// originatingUserSQL is the account of the client that started a query_log
// row's query, falling back to the row's own user where ClickHouse left
// initial_user empty.
const originatingUserSQL = "if(initial_user != '', initial_user, user)"

// originatingAddressSQL is the address of the client that started a row's
// query, falling back to the row's own address where ClickHouse left
// initial_address unset, the same rule as processor.OriginatingAddress. An
// IPv4 client is unmapped from ::ffff:a.b.c.d so it falls in an IPv4 prefix;
// the strip is anchored on the dotted quad so no other IPv6 address that
// happens to start with ::ffff: is mangled.
// toString, not IPv6NumToString: the row SELECT aliases the String
// IPv6NumToString(initial_address) AS initial_address, and ClickHouse resolves
// the name in WHERE to that alias, while the aggregate queries see the IPv6
// column. toString renders both the same.
const originatingAddressSQL = "replaceRegexpOne(if(toString(initial_address) IN ('::', '::ffff:0.0.0.0'), " +
	"toString(address), toString(initial_address)), '^::ffff:([0-9]+[.][0-9]+[.][0-9]+[.][0-9]+)$', '\\\\1')"

// initialQueryScope says where addRowFilters looks up a secondary query's
// initial query: the table the builder reads, and the lower bound of its
// window. An initial query's QueryFinish or ExceptionWhileProcessing row is
// logged after every secondary query it started finishes, so a secondary row
// in the window has its initial row at or after the same bound. An initial
// query still running past the window's start is in system.processes, not
// query_log, and is the one case the lookup misses.
type initialQueryScope struct {
	table  string
	since  string // " AND event_time >= ?"-style predicate; empty for system.processes
	params []interface{}
}

// addRowFilters applies every filter a query_log or system.processes row can
// be judged on: both user lists, blacklist_queries on the row's query text,
// and whitelist_ips on its originating client. The analyze and backfill paths
// read rows and aggregates straight from these tables, so the filters have to
// hold in SQL: an aggregate cannot be filtered afterward. The operation
// filters describe span-log operations and do not apply.
//
// The user and IP filters judge the initial_* columns, which every row of a
// distributed query shares. A secondary query's own text is the rewrite a
// shard runs, so blacklist_queries also drops rows whose initial query
// matches, the way the live path drops the whole trace.
func (b *queryLogBuilder) addRowFilters(c *ClickHouseReader, initial initialQueryScope) {
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)
	if len(c.blacklistQueries) > 0 {
		for _, pattern := range c.blacklistQueries {
			b.query += " AND NOT match(query, ?)"
			b.params = append(b.params, pattern)
		}
		matches := make([]string, len(c.blacklistQueries))
		for i := range matches {
			matches[i] = "match(query, ?)"
		}
		b.query += " AND (is_initial_query OR initial_query_id NOT IN (SELECT query_id FROM " + initial.table +
			" WHERE is_initial_query" + initial.since + " AND (" + strings.Join(matches, " OR ") + ")))"
		b.params = append(b.params, initial.params...)
		for _, pattern := range c.blacklistQueries {
			b.params = append(b.params, pattern)
		}
	}
	if len(c.clientPrefixes) > 0 {
		clauses := make([]string, len(c.clientPrefixes))
		for i, prefix := range c.clientPrefixes {
			clauses[i] = "isIPAddressInRange(" + originatingAddressSQL + ", ?)"
			b.params = append(b.params, prefix)
		}
		b.query += " AND (" + strings.Join(clauses, " OR ") + ")"
	}
}

// queryLogSince scopes the initial-query lookup to query_log rows at or after
// start.
func (c *ClickHouseReader) queryLogSince(start interface{}, predicate string) initialQueryScope {
	return initialQueryScope{table: c.queryLogRef, since: " AND " + predicate, params: []interface{}{start}}
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
	}
	if c.queryLogOperation {
		dest = append(dest, &log.QueryOperation)
	}
	dest = append(dest, &log.EventTime, &log.QueryDurationMs, &log.Query)
	if c.queryLogNormalized {
		dest = append(dest, &log.NormalizedQueryHash, &log.NormalizedQuery)
	}
	dest = append(dest,
		&log.User,
		&log.InitialUser,
		&log.ClientName,
		&log.ClientHostname,
		&log.ClientAddress,
		&log.InitialAddress,
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
	b.addRowFilters(c, c.queryLogSince(opts.StartTime, "event_time >= ?"))
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
		var exceptionCodes []int64
		var exceptionCounts []uint64
		if err := rows.Scan(
			&group.NormalizedQueryHash,
			&group.NormalizedQuery,
			&group.ExecutionCount,
			&group.SuccessfulCount,
			&group.FailedCount,
			&group.P95DurationMs,
			&group.P99DurationMs,
			&exceptionCodes,
			&exceptionCounts,
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
		if len(exceptionCodes) != len(exceptionCounts) {
			return nil, fmt.Errorf("failed to scan query family exact group: exception code/count length mismatch (%d != %d)", len(exceptionCodes), len(exceptionCounts))
		}
		for i, code := range exceptionCodes {
			if exceptionCounts[i] == 0 {
				continue
			}
			group.TopExceptions = append(group.TopExceptions, model.QueryExceptionCount{Code: int32(code), Count: exceptionCounts[i]})
		}
		sort.Slice(group.TopExceptions, func(i, j int) bool {
			if group.TopExceptions[i].Count != group.TopExceptions[j].Count {
				return group.TopExceptions[i].Count > group.TopExceptions[j].Count
			}
			return group.TopExceptions[i].Code < group.TopExceptions[j].Code
		})
		if len(group.TopExceptions) > queryfamily.TopKLimit {
			group.TopExceptions = group.TopExceptions[:queryfamily.TopKLimit]
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

// slowQueriesBuilder assembles the *queryLogBuilder used by FetchSlowQueries.
// Split out so the wiring (duration + user + order) can be unit-tested
// without a live ClickHouse connection.
func (c *ClickHouseReader) slowQueriesBuilder(minDurationMs, maxDurationMs, maxQueryLength int, lookback time.Duration, limit int) *queryLogBuilder {
	b := c.newQueryLogBuilder(
		"event_time >= now() - INTERVAL ? SECOND AND type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		[]interface{}{int(lookback.Seconds())},
	)
	b.addDurationFilters(minDurationMs, maxDurationMs, maxQueryLength)
	b.addRowFilters(c, c.queryLogSince(int(lookback.Seconds()), "event_time >= now() - INTERVAL ? SECOND"))
	b.addOrderAndLimit(false, limit)
	return b
}

// QueryLogCursor is a position in a range read ordered by event time and then
// query ID: the event time and query ID of the last row read.
type QueryLogCursor struct {
	EventTime time.Time
	QueryID   string
}

// CursorAfter returns the position just after log in a range read. The event
// time is kept in UTC: a scanned event_time carries the server's time zone,
// and the driver binds a time in that zone as bare wall-clock text, which
// names two instants in the hour a daylight-saving change repeats.
func CursorAfter(log model.QueryLog) *QueryLogCursor {
	return &QueryLogCursor{EventTime: log.EventTime.UTC(), QueryID: log.QueryID}
}

// pageLookupSkew is how far below a backfill page's cursor the
// blacklist_queries initial-query lookup starts, to tolerate clock skew
// between the hosts that wrote a query's query_log rows.
const pageLookupSkew = time.Minute

// FetchSlowQueriesInRangePage returns one page of the slow queries finished in
// [startTime, endTime]: at most limit rows in (event_time, query_id) order,
// starting after cursor when it is set. Passing the last row's CursorAfter
// reads the next page, so a caller reads the whole range in pages of limit
// rows. event_time has one-second resolution, so the query ID orders rows
// within a second. Rows that share both, from a client that reuses a query ID,
// are all skipped by a cursor at their key, so a caller must not end a page
// inside such a group.
func (c *ClickHouseReader) FetchSlowQueriesInRangePage(ctx context.Context, minDurationMs int, maxDurationMs int, maxQueryLength int, startTime, endTime time.Time, cursor *QueryLogCursor, limit int) ([]model.QueryLog, error) {
	return c.executeQueryLogQuery(ctx, c.slowQueriesInRangePageBuilder(minDurationMs, maxDurationMs, maxQueryLength, startTime, endTime, cursor, limit))
}

// slowQueriesInRangePageBuilder builds one page's read. Every time it binds is
// in UTC, which the driver renders as an unambiguous instant whatever the
// server's time zone. A later page's rows finished at or after the cursor, and
// their initial queries finished no earlier, so blacklist_queries'
// initial-query lookup reads from the cursor rather than from the start of the
// range, less pageLookupSkew: each host stamps its own query_log rows, and an
// initiator whose clock runs behind a shard's can log its finish earlier than
// the shard's row.
func (c *ClickHouseReader) slowQueriesInRangePageBuilder(minDurationMs, maxDurationMs, maxQueryLength int, startTime, endTime time.Time, cursor *QueryLogCursor, limit int) *queryLogBuilder {
	startTime, endTime = startTime.UTC(), endTime.UTC()
	b := c.newQueryLogBuilder(
		"event_time >= ? AND event_time <= ? AND type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		[]interface{}{startTime, endTime},
	)
	b.addDurationFilters(minDurationMs, maxDurationMs, maxQueryLength)
	since := startTime
	if cursor != nil {
		if floor := cursor.EventTime.UTC().Add(-pageLookupSkew); floor.After(since) {
			since = floor
		}
	}
	b.addRowFilters(c, c.queryLogSince(since, "event_time >= ?"))
	if cursor != nil {
		at := cursor.EventTime.UTC()
		b.query += " AND (event_time > ? OR (event_time = ? AND query_id > ?))"
		b.params = append(b.params, at, at, cursor.QueryID)
	}
	b.query += " ORDER BY event_time ASC, query_id ASC"
	if limit > 0 {
		b.query += " LIMIT ?"
		b.params = append(b.params, limit)
	}
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
// and full metadata are available when supported) and the same row-filter
// posture, pushes the -match / -normalized-query-hash predicates into the WHERE
// so the LIMIT bounds the matched rows, then orders most-expensive-first and
// bounds the result by Limit. It is split out so the candidate-search query can
// be unit-tested without a live ClickHouse connection. Raw query text is never
// returned to the caller: the command emits only bounded normalized previews and
// metadata.
//
// Param order is load-bearing: window, then duration/length, then row filters,
// then the hash term (one ?), then the match OR-group (one ? per term), then the
// LIMIT. Each add* helper appends its own ? params in that same order.
func (c *ClickHouseReader) recentQueryCandidatesBuilder(opts RecentQueryCandidateOptions) *queryLogBuilder {
	b := c.newQueryLogBuilder(
		"event_time >= ? AND event_time <= ? AND type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		[]interface{}{opts.StartTime, opts.EndTime},
	)
	b.addDurationFilters(opts.MinDurationMs, opts.MaxDurationMs, opts.MaxQueryLength)
	b.addRowFilters(c, c.queryLogSince(opts.StartTime, "event_time >= ?"))
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
	logs, err := c.executeQueryLogQuery(ctx, c.recentQueryCandidatesBuilder(opts))
	if err != nil {
		return nil, err
	}
	// The shared query-log scanner also serves backfill, which needs raw SQL.
	// Trace-drilldown candidates must expose only the normalized preview: clear
	// raw text at this API boundary so future callers cannot accidentally leak
	// it even if the presentation layer forgets to suppress the field.
	for i := range logs {
		logs[i].Query = ""
	}
	return logs, nil
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
// search over system.processes. It applies the same row filters as the
// other query_log paths, pushes the optional -match needle into the WHERE
// OR-group (so the LIMIT bounds the matched rows), then orders longest-running
// first and bounds by Limit. The normalized preview match term participates
// only when normalized support is present, the same gate as the SELECT column.
// Split out so the query can be unit-tested without a live ClickHouse.
func (c *ClickHouseReader) currentQueryCandidatesBuilder(opts CurrentQueryCandidateOptions) *queryLogBuilder {
	b := &queryLogBuilder{query: c.currentQuerySelect}
	// system.processes exposes the same user, address, and query columns, so
	// the row filters the slow-query builder uses apply unchanged; a running
	// secondary query's initial query is running too.
	b.addRowFilters(c, initialQueryScope{table: c.processesRef})
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

// queryLogStartMargin is how far before the earliest span start the caller
// knows for a set of queries their query_log rows are read. query_log is
// sorted by (event_date, event_time), so a read by query_id costs every
// granule after its event_time floor; without one it costs every granule
// since the day floor.
//
// The floor is safe because the server that runs a query stamps both its
// spans and its query_log rows, from one clock, and stamps the finish rows
// when the query finishes: after every span of the query started. The margin
// absorbs event_time's whole-second truncation, a step of that clock between a
// span's start and the query's finish, and the gap between a QueryStart row
// and the start of the query's span. The query_log flush interval delays when
// a row becomes visible, not its event_time, so it cannot move a row below
// the floor.
const queryLogStartMargin = time.Minute

// queryLogReadFloor is the oldest event_time, in Unix seconds, a query_log
// read by query_id accepts for queries whose earliest known span start is
// earliestStartUs. It saturates at 0, which bounds nothing.
func queryLogReadFloor(earliestStartUs uint64) uint64 {
	startS := earliestStartUs / uint64(time.Second/time.Microsecond)
	margin := uint64(queryLogStartMargin / time.Second)
	if startS <= margin {
		return 0
	}
	return startS - margin
}

// originsReadFloor is the oldest event_time, in Unix seconds, FetchQueryOrigins
// accepts. It is one second below queryLogReadFloor: FetchTraceNodes returns
// query spans that finished from spanReadFloor of the same start, in
// microseconds, and a query's finish row is stamped a little before its query
// span finishes, so a span just past a whole second can have its row in the
// second before. The trace filters withhold a trace whose query has no row,
// so the two reads must not disagree.
func originsReadFloor(earliestStartUs uint64) uint64 {
	if f := queryLogReadFloor(earliestStartUs); f > 0 {
		return f - 1
	}
	return 0
}

// FetchQueryLogByQueryIDs queries system.query_log for completed queries
// matching the given query IDs, returning a map keyed by query_id.
// When multiple query_log rows exist for a query_id (QueryFinish +
// ExceptionWhileProcessing), the exception row is preferred.
//
// earliestStartUs is the earliest start_time_us among the caller's spans of
// these queries; rows stamped more than queryLogStartMargin before it are not
// read. 0 reads back to the lookbackDays day floor.
//
// The join key is query_id (not trace_id) because system.query_log has no
// trace_id column in current ClickHouse versions. query_id appears in both
// tables: as a primary column in query_log, and as the clickhouse.query_id
// attribute on opentelemetry_span_log spans. This is the only enrichment
// path; setUserFilter's splice into enrichSelect therefore covers all
// query_log enrichment used by the processor.
func (c *ClickHouseReader) FetchQueryLogByQueryIDs(ctx context.Context, queryIDs []string, lookbackDays int, earliestStartUs uint64) (map[string]model.QueryLog, error) {
	if len(queryIDs) == 0 {
		return nil, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	// LIMIT 2 BY query_id in the SQL guarantees at most 2 rows per
	// query_id (QueryFinish + ExceptionWhileProcessing); dedup below
	// keeps one. No global LIMIT needed — the IN clause bounds the set.
	params := []interface{}{queryIDs, lookbackDays, queryLogReadFloor(earliestStartUs)}
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
	}
	if c.queryLogOperation {
		dest = append(dest, &log.QueryOperation)
	}
	dest = append(dest, &log.EventTime, &log.QueryDurationMs)
	if c.queryLogNormalized {
		dest = append(dest, &log.NormalizedQueryHash, &log.NormalizedQuery)
	}
	dest = append(dest,
		&log.User,
		&log.InitialUser,
		&log.ClientName,
		&log.ClientHostname,
		&log.ClientAddress,
		&log.InitialAddress,
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
	// QuerySpansOnly keeps only `query` spans in the span page, so a span limit
	// counts queries rather than their child spans. Only the one-shot
	// FetchOpenTelemetrySpansWithOpts honors it; the live walk reads every span.
	QuerySpansOnly bool
}

func (c *ClickHouseReader) FetchOpenTelemetrySpans(ctx context.Context, minTraceDurationMs int, maxTraceDurationMs int, minSpanDurationMs int, maxSpanDurationMs int, lookback time.Duration, limit int) ([]model.OpenTelemetrySpan, error) {
	return c.FetchOpenTelemetrySpansWithOpts(ctx, minTraceDurationMs, lookback, limit, FetchOpts{
		MaxTraceDurationMs: maxTraceDurationMs,
		MinSpanDurationMs:  minSpanDurationMs,
		MaxSpanDurationMs:  maxSpanDurationMs,
	})
}

// FetchOpenTelemetrySpansWithOpts returns one newest-first snapshot of Live
// Span Selection: the newest qualifying traces inside the lookback window and
// up to limit of their spans. It keeps no position between calls, so it suits
// one-shot readers such as `analyze queries`; scheduled mode uses
// FetchLiveSpans, which walks the window across cycles.
func (c *ClickHouseReader) FetchOpenTelemetrySpansWithOpts(ctx context.Context, minTraceDurationMs int, lookback time.Duration, limit int, opts FetchOpts) ([]model.OpenTelemetrySpan, error) {
	sel := newSpanSelection(minTraceDurationMs, lookback, opts)
	sel.querySpansOnly = opts.QuerySpansOnly
	effectiveLimit := resolveSpanLimit(limit)

	traceIDs, _, earliestStart, err := c.queryTracePage(ctx, sel, traceBounds{}, false, effectiveLimit)
	if err != nil {
		return nil, err
	}
	if len(traceIDs) == 0 {
		return []model.OpenTelemetrySpan{}, nil
	}
	return c.querySpanPage(ctx, sel, traceIDs, earliestStart, nil, false, effectiveLimit)
}

// maxLiveTracePagesPerFetch bounds the trace-page queries one FetchLiveSpans
// issues. Pages normally stop on the span cap long before this; it exists for
// pages whose spans the SQL span filters remove entirely, which advance the
// watermark without spending any of the cap.
const maxLiveTracePagesPerFetch = 8

// liveRetryReach is how many lookback windows the forward walk may reach back
// to resume from its committed position. At the defaults a failed export
// followed by the first backoff step retries 90s after the last commit, plus up
// to one 7.5s span-log flush, against a 40s lookback; three windows cover that.
// A page that can never export is dropped once it is older than this, instead
// of pinning the walk.
const liveRetryReach = 3

// LiveSettle is how long a trace must have been quiet before the live walk
// reads it: its newest qualifying span finished at least this long ago by
// ClickHouse's clock. ClickHouse flushes span_log and query_log on separate
// timers (7.5s each by default), so a trace read sooner can be missing its
// query span or its query_log row, which leaves it without query text or a
// resolvable user. The window shifts back by the same amount, so a lookback
// shorter than the settle still selects traces.
const LiveSettle = 15 * time.Second

// traceStartMargin is how far before the earliest start the caller knows for
// a set of traces their span reads reach. The span log is sorted by
// (finish_date, finish_time_us, trace_id), so a read by trace ID or by span
// attribute costs every granule after its finish_time_us floor; without one it
// costs every granule since the day floor.
//
// The floor is safe because every span of a ClickHouse query descends from the
// query's `query` span and finishes after that span starts, and the `query`
// span is at least as long as any span under it, so it qualifies whenever one
// of them does unless max_trace_duration_ms excludes it. Once it is selected,
// the floor sits before its start and the whole query is read. The margin
// absorbs clock skew between the servers of a distributed query, and reaches
// the earlier queries of a trace an application propagates into ClickHouse
// when they finished shortly before the selected one started.
const traceStartMargin = time.Minute

// spanReadFloor is the oldest finish_time_us a trace-scoped span read accepts
// for traces whose earliest known start is earliestStartUs.
func spanReadFloor(earliestStartUs uint64) uint64 {
	margin := uint64(traceStartMargin.Microseconds())
	if earliestStartUs <= margin {
		return 0
	}
	return earliestStartUs - margin
}

// spanReadFloorSince is spanReadFloor for a caller's time. The zero time, or
// any time before the epoch, gives 0: a read bounded by the day floor alone.
func spanReadFloorSince(since time.Time) uint64 {
	us := since.UnixMicro()
	if us <= 0 {
		return 0
	}
	return spanReadFloor(uint64(us))
}

// FetchLiveSpans returns scheduled mode's next page of Live Span Selection, at
// most limit spans. The page's progress is held pending: CommitLiveSpans
// records it once the caller has handled every span, and a fetch without a
// commit reads from the last committed position again, so a failed export or
// a failed fetch is retried rather than skipped. The retry reaches back up to
// liveRetryReach lookback windows; a walk with no committed position starts at
// the lookback floor.
//
// Traces are selected in two steps: trace IDs with a span inside
// [minTraceDurationMs, MaxTraceDurationMs] in the lookback window, then the
// spans of those traces under the span duration filters and the operation
// blacklist, back to traceStartMargin before the earliest qualifying span in
// the page started. The blacklist is not applied to trace selection, so a
// trace whose qualifying spans are all blacklisted consumes a trace-page slot
// and returns no spans; min_trace_duration_ms is typically 1s+ and
// blacklisted operations are sub-millisecond internals, so that is rare.
func (c *ClickHouseReader) FetchLiveSpans(ctx context.Context, minTraceDurationMs int, lookback time.Duration, limit int, opts FetchOpts) ([]model.OpenTelemetrySpan, error) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	c.livePending = nil
	fetchStart := c.localNow()

	sel := newSpanSelection(minTraceDurationMs, lookback, opts)
	sel.settle = LiveSettle
	effectiveLimit := resolveSpanLimit(limit)
	signature := liveWalkSignature{
		minTraceDurationMs: minTraceDurationMs,
		maxTraceDurationMs: opts.MaxTraceDurationMs,
		minSpanDurationMs:  opts.MinSpanDurationMs,
		maxSpanDurationMs:  opts.MaxSpanDurationMs,
		lookback:           lookback,
		limit:              effectiveLimit,
		blacklist:          strings.Join(opts.BlacklistOperations, "\x00"),
	}
	walk := c.live
	if walk.signature != signature {
		walk = liveWalk{signature: signature}
	}

	startWatermark := walk.watermark
	remaining := effectiveLimit
	tracePages := 0
	caughtUp := false
	var spans []model.OpenTelemetrySpan

	// The forward walk's window reaches back far enough to resume from a
	// committed position that fell behind the lookback floor; finishedFrom
	// keeps it from reading anything older than that position.
	walkSel := newSpanSelection(minTraceDurationMs, liveRetryReach*lookback, opts)
	walkSel.settle = LiveSettle

	// The next trace page reads from the watermark, or from the end of the
	// page still being drained.
	resume := walk.watermark
	if walk.page != nil {
		resume = &walk.pageLast
	}
	if resume != nil {
		c.reportUnreadableSpans(ctx, walkSel, resume.finish, walk.headAt, lookback)
	}

	// Oldest first from the watermark. The oldest rows are the next to fall out
	// of reach, and a watermark means the budget is never spent re-reading
	// pages an earlier fetch delivered, so inflow below max_spans_per_cycle per
	// check interval is read in full once a burst or failure has passed.
	for remaining > 0 && tracePages < maxLiveTracePagesPerFetch {
		// Only a short page selected by this fetch reaches the head: a page
		// carried over from an earlier fetch was short as of that fetch.
		pageReachesHead := false
		if walk.page == nil {
			pageLimit := remaining
			pageSel, bounds := sel, traceBounds{after: walk.watermark}
			if walk.watermark != nil {
				pageSel, bounds.finishedFrom = walkSel, &walk.watermark.finish
			}
			ids, last, earliestStart, err := c.queryTracePage(ctx, pageSel, bounds, true, pageLimit)
			tracePages++
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				caughtUp = true
				break
			}
			walk.page, walk.pageLast, walk.pageStart, walk.spanAfter = ids, last, earliestStart, nil
			pageReachesHead = len(ids) < pageLimit
		}

		page, err := c.querySpanPage(ctx, walkSel, walk.page, walk.pageStart, walk.spanAfter, true, remaining)
		if err != nil {
			return nil, err
		}
		spans = append(spans, page...)
		if len(page) == remaining {
			// The page may hold more spans: the next fetch continues behind
			// the last one rather than re-reading the page from its start.
			last := page[len(page)-1]
			walk.spanAfter = &spanKey{finish: last.FinishTimeUs, trace: last.TraceID, span: last.SpanID}
			remaining = 0
			break
		}
		remaining -= len(page)
		pageLast := walk.pageLast
		walk.watermark = &pageLast
		walk.page, walk.spanAfter = nil, nil
		if pageReachesHead {
			caughtUp = true
			break
		}
	}

	// Once the walk reaches the head, spend what is left of the cap re-reading
	// what earlier fetches delivered, newest first. Span-log rows flush late,
	// and a span that lands behind the watermark is otherwise never read; the
	// newest rows are where late flushes land. The dedup cache suppresses
	// everything already exported.
	if caughtUp && startWatermark != nil {
		bounds := traceBounds{atOrBefore: startWatermark}
		for remaining > 0 && tracePages < maxLiveTracePagesPerFetch {
			pageLimit := remaining
			ids, last, earliestStart, err := c.queryTracePage(ctx, sel, bounds, false, pageLimit)
			tracePages++
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				break
			}
			page, err := c.querySpanPage(ctx, sel, ids, earliestStart, nil, false, remaining)
			if err != nil {
				return nil, err
			}
			spans = append(spans, page...)
			remaining -= len(page)
			if len(ids) < pageLimit {
				break
			}
			bounds.before = &last
		}
	}

	clicklog.Debug("Live span selection returned %d spans (caught up: %t)", len(spans), caughtUp)
	if caughtUp {
		// Held pending with the rest of the walk: only a delivered fetch
		// counts as having read up to the head.
		walk.headAt = fetchStart
	}
	c.livePending = &walk
	return spans, nil
}

// localNow reads this host's clock.
func (c *ClickHouseReader) localNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// reportUnreadableSpans warns about span-log rows the live walk can no longer
// read: rows that finished after the walk last read but before the oldest
// finish time its trace pages reach. The walk last read up to resumeUs, where
// its next trace page starts, or, when later, up to the settle before headAt,
// the start of the latest delivered fetch that reached the head: on a quiet
// span log the watermark stays at the last trace while every fetch reads on.
// Rows fall behind the reach when exports fail for longer than it, as in a
// collector outage. The walk leaves them behind by design so a page that can
// never export does not pin it; this makes the loss visible. Each stretch of
// time is reported once, however many fetches find it unreadable. The caller
// holds liveMu.
//
// The trace page measures its window on the server's clock, so the floor is
// read from the server. This host's clock decides whether to ask: a healthy
// or quiet walk sits well inside the window and issues no extra query. slack
// absorbs clock skew between the two hosts; a host whose clock runs behind
// the server's by more than slack can miss a short loss. headAt is converted
// to the server's clock with the skew measured at the read. A failed clock
// read skips the check.
func (c *ClickHouseReader) reportUnreadableSpans(ctx context.Context, walkSel spanSelection, resumeUs uint64, headAt time.Time, slack time.Duration) {
	window := time.Duration(walkSel.windowSeconds()) * time.Second
	localNow := c.localNow()
	readThrough := int64(resumeUs)
	if !headAt.IsZero() {
		readThrough = max(readThrough, headAt.Add(-walkSel.settle).UnixMicro())
	}
	if near := localNow.Add(slack - window).UnixMicro(); near <= 0 || readThrough >= near {
		return
	}
	serverNow, err := c.ServerTime(ctx)
	if err != nil {
		clicklog.Debug("Skipping the live walk's reach check: %v", err)
		return
	}
	floorUs := serverNow.Add(-window).UnixMicro()
	if floorUs <= 0 {
		return
	}
	floor := uint64(floorUs)
	from := max(resumeUs, c.liveLostThrough)
	if !headAt.IsZero() {
		skew := serverNow.Sub(localNow)
		if head := headAt.Add(skew - walkSel.settle).UnixMicro(); head > 0 {
			from = max(from, uint64(head))
		}
	}
	if from >= floor {
		return
	}
	c.liveLostThrough = floor
	clicklog.Warn("Live export fell behind its retry reach of %s: span-log rows that finished from %s to %s can no longer be read and were not exported. Backfill can export that range from query_log; a larger monitor.lookback_s widens the reach",
		window, formatSpanLogTime(from), formatSpanLogTime(floor))
}

// formatSpanLogTime renders a span-log finish time in microseconds as RFC 3339
// UTC.
func formatSpanLogTime(us uint64) string {
	return time.UnixMicro(int64(us)).UTC().Format(time.RFC3339)
}

// CommitLiveSpans records that every span the last FetchLiveSpans returned has
// been handled, so the next fetch continues past them. Without a commit the
// next fetch reads the same page again. A commit with nothing pending is a
// no-op.
func (c *ClickHouseReader) CommitLiveSpans() {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if c.livePending == nil {
		return
	}
	c.live = *c.livePending
	c.livePending = nil
}

// spanSelection carries one fetch's Live Span Selection filters into the trace
// and span page queries.
type spanSelection struct {
	lookback time.Duration
	// settle, when set, selects only traces whose newest qualifying span
	// finished at least this long ago, and shifts the window back by it.
	settle             time.Duration
	lookbackDays       int
	minTraceDurationMs int
	maxTraceDurationMs int
	minSpanDurationMs  int
	maxSpanDurationMs  int
	blacklist          []string
	// querySpansOnly limits the span page, not the trace page, to `query`
	// spans.
	querySpansOnly bool
}

// windowSeconds is how far back from the server's current time a trace page
// reads, in whole seconds as the SQL binds it.
func (s spanSelection) windowSeconds() int {
	return int((s.lookback + s.settle).Seconds())
}

func newSpanSelection(minTraceDurationMs int, lookback time.Duration, opts FetchOpts) spanSelection {
	return spanSelection{
		lookback:           lookback,
		lookbackDays:       int(lookback.Hours()/24) + 1,
		minTraceDurationMs: minTraceDurationMs,
		maxTraceDurationMs: opts.MaxTraceDurationMs,
		minSpanDurationMs:  opts.MinSpanDurationMs,
		maxSpanDurationMs:  opts.MaxSpanDurationMs,
		blacklist:          opts.BlacklistOperations,
	}
}

// querySpanPage runs step 2 of Live Span Selection: one page of spans for the
// given traces in keyset order, continuing past cursor when it is set, with
// the duration filters and operation blacklist pushed down as SQL. It reads
// spans that finished from traceStartMargin before earliestStart, the earliest
// start of a qualifying span in the trace page.
func (c *ClickHouseReader) querySpanPage(ctx context.Context, sel spanSelection, traceIDs []uuid.UUID, earliestStart uint64, cursor *spanKey, oldestFirst bool, limit int) ([]model.OpenTelemetrySpan, error) {
	spansQuery := c.spanSelect + " AND finish_time_us >= ?"
	spanParams := []interface{}{traceIDs, sel.lookbackDays, spanReadFloor(earliestStart)}

	// 0 = fetch all spans (no lower bound)
	if sel.minSpanDurationMs > 0 {
		spansQuery += " AND (finish_time_us - start_time_us) >= ? * 1000"
		spanParams = append(spanParams, sel.minSpanDurationMs)
	}

	if sel.maxSpanDurationMs > 0 {
		spansQuery += " AND (finish_time_us - start_time_us) <= ? * 1000"
		spanParams = append(spanParams, sel.maxSpanDurationMs)
	}

	// Operation blacklist, pushed down as SQL NOT LIKE (no ReDoS risk, excludes
	// known-noise internal ops like MergeTreeIndex). Bound as %pattern%, so a
	// literal % or _ inside a configured pattern is treated as a LIKE wildcard,
	// not matched literally — see config docs for blacklist_operations.
	for _, pattern := range sel.blacklist {
		if pattern == "" {
			continue
		}
		spansQuery += " AND operation_name NOT LIKE ?"
		spanParams = append(spanParams, "%"+pattern+"%")
	}
	if sel.querySpansOnly {
		spansQuery += " AND operation_name = 'query'"
	}

	order, cmp := "DESC", "<"
	if oldestFirst {
		order, cmp = "ASC", ">"
	}
	if cursor != nil {
		spansQuery += " AND (finish_time_us, trace_id, span_id) " + cmp + " (?, ?, ?)"
		spanParams = append(spanParams, cursor.finish, cursor.trace, cursor.span)
	}

	spansQuery += " ORDER BY finish_time_us " + order + ", trace_id " + order + ", span_id " + order + " LIMIT ?"
	spanParams = append(spanParams, limit)

	spansCtx, spansCancel := context.WithTimeout(ctx, c.queryTimeout)
	defer spansCancel()

	rows, err := c.conn.Query(spansCtx, spansQuery, spanParams...)
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

// queryTracePage runs step 1 of Live Span Selection: one page of trace IDs
// with a qualifying span inside the lookback window, in traceKey order within
// bounds. It returns the page, the last row's key, and the earliest start of
// a qualifying span in the page.
func (c *ClickHouseReader) queryTracePage(ctx context.Context, sel spanSelection, bounds traceBounds, oldestFirst bool, limit int) ([]uuid.UUID, traceKey, uint64, error) {
	traceQuery := c.traceIDSelect
	traceParams := []interface{}{sel.lookbackDays, sel.windowSeconds(), sel.minTraceDurationMs}

	if sel.maxTraceDurationMs > 0 {
		traceQuery += " AND (finish_time_us - start_time_us) <= ? * 1000"
		traceParams = append(traceParams, sel.maxTraceDurationMs)
	}

	if bounds.finishedFrom != nil {
		traceQuery += " AND finish_time_us >= ?"
		traceParams = append(traceParams, *bounds.finishedFrom)
	}

	traceQuery += `
			)
			GROUP BY trace_id`
	var having []string
	if sel.settle > 0 {
		having = append(having, "max_finish_time <= toUnixTimestamp64Micro(now64(6)) - ?")
		traceParams = append(traceParams, sel.settle.Microseconds())
	}
	for _, b := range []struct {
		op  string
		key *traceKey
	}{{">", bounds.after}, {"<", bounds.before}, {"<=", bounds.atOrBefore}} {
		if b.key != nil {
			having = append(having, "(max_finish_time, trace_id) "+b.op+" (?, ?)")
			traceParams = append(traceParams, b.key.finish, b.key.id)
		}
	}
	if len(having) > 0 {
		traceQuery += " HAVING " + strings.Join(having, " AND ")
	}
	order := "DESC"
	if oldestFirst {
		order = "ASC"
	}
	traceQuery += " ORDER BY max_finish_time " + order + ", trace_id " + order + " LIMIT ?"
	traceParams = append(traceParams, limit)

	traceCtx, traceCancel := context.WithTimeout(ctx, c.queryTimeout)
	defer traceCancel()
	rows, err := c.conn.Query(traceCtx, traceQuery, traceParams...)
	if err != nil {
		return nil, traceKey{}, 0, fmt.Errorf("failed to query trace IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var traceIDs []uuid.UUID
	var last traceKey
	var earliestStart uint64
	for rows.Next() {
		var traceID uuid.UUID
		var maxFinishTime, minStartTime uint64
		if err := rows.Scan(&traceID, &maxFinishTime, &minStartTime); err != nil {
			return nil, traceKey{}, 0, fmt.Errorf("failed to scan trace ID: %w", err)
		}
		if len(traceIDs) == 0 || minStartTime < earliestStart {
			earliestStart = minStartTime
		}
		traceIDs = append(traceIDs, traceID)
		last = traceKey{finish: maxFinishTime, id: traceID}
	}
	if err := rows.Err(); err != nil {
		return nil, traceKey{}, 0, fmt.Errorf("trace ID iteration error: %w", err)
	}
	return traceIDs, last, earliestStart, nil
}

// FetchTraceIDsByQueryID returns the distinct trace IDs whose spans carry the
// given ClickHouse query_id span attribute, bounded by lookbackDays partition
// pruning, limit, and since: the start of the window the query finished in.
// Spans that finished more than traceStartMargin before since are not read.
// This is the trace-drilldown bridge from a query identity to native trace
// spans; it does not run duration-based trace selection. An empty queryID
// returns no trace IDs and no error. Trace IDs are returned as canonical UUID
// strings so the CLI can echo them and pass them back into the span fetch.
//
// It also returns the earliest start of the matched spans, on the server's
// clock, as the since to hand FetchSpansForTraceIDs: the matched span is the
// query's `query` span, and every span of the query finishes after it starts.
// It is the zero time when nothing matched.
func (c *ClickHouseReader) FetchTraceIDsByQueryID(ctx context.Context, queryID string, lookbackDays int, since time.Time, limit int) ([]string, time.Time, error) {
	if queryID == "" {
		return nil, time.Time{}, nil
	}
	if lookbackDays < 1 {
		lookbackDays = 1
	}
	effectiveLimit := resolveSpanLimit(limit)

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, c.traceIDByQueryID, lookbackDays, spanReadFloorSince(since), queryID, effectiveLimit)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to query trace IDs by query_id: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var traceIDs []string
	var earliestStart uint64
	for rows.Next() {
		var traceID uuid.UUID
		var minStartTime uint64
		if err := rows.Scan(&traceID, &minStartTime); err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to scan trace ID: %w", err)
		}
		if len(traceIDs) == 0 || minStartTime < earliestStart {
			earliestStart = minStartTime
		}
		traceIDs = append(traceIDs, traceID.String())
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("trace ID iteration error: %w", err)
	}
	if len(traceIDs) == 0 {
		return traceIDs, time.Time{}, nil
	}
	return traceIDs, time.UnixMicro(int64(earliestStart)), nil
}

// FetchTraceNodes returns, for each trace that has any, the spans recorded
// for it within lookbackDays that carry a clickhouse.query_id, with their
// statements and span tree links, plus the ancestors ancestors asks for.
// Traces with no such span inside the read are absent from the result.
//
// earliestStartUs is the earliest start_time_us among the caller's spans of
// these traces. Spans that finished more than traceStartMargin before it are
// not read: the query span a caller's span runs under finished after that span
// started, so it is always inside the read.
func (c *ClickHouseReader) FetchTraceNodes(ctx context.Context, traceIDs []uuid.UUID, lookbackDays int, earliestStartUs uint64, ancestors model.AncestorRead) (map[uuid.UUID][]model.TraceNode, error) {
	if len(traceIDs) == 0 {
		return map[uuid.UUID][]model.TraceNode{}, nil
	}
	if lookbackDays < 1 {
		lookbackDays = 1
	}

	floor := spanReadFloor(earliestStartUs)
	query := c.traceNodes
	params := []interface{}{traceIDs, lookbackDays, floor}
	if len(ancestors.Traces) > 0 {
		query += c.traceAncestors
		params = append(params, ancestors.StartedByUs, ancestors.Traces, lookbackDays, floor)
	}
	query += ")"

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query trace nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[uuid.UUID][]model.TraceNode)
	for rows.Next() {
		var traceID uuid.UUID
		var isQuery uint8
		var n model.TraceNode
		if err := rows.Scan(&traceID, &n.SpanID, &n.ParentSpanID, &isQuery, &n.QueryID, &n.Statement); err != nil {
			return nil, fmt.Errorf("failed to scan trace node: %w", err)
		}
		n.Query = isQuery != 0
		out[traceID] = append(out[traceID], n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace node iteration error: %w", err)
	}
	return out, nil
}

// FetchQueryOrigins returns the user, initial_user, address, and
// initial_address of each query found in query_log within lookbackDays.
// Queries with no row are absent from the result.
//
// earliestStartUs is the earliest start_time_us among the caller's spans of
// the traces these queries came from, the same start FetchTraceNodes read
// from. Rows stamped before originsReadFloor of it are not read.
func (c *ClickHouseReader) FetchQueryOrigins(ctx context.Context, queryIDs []string, lookbackDays int, earliestStartUs uint64) (map[string]model.QueryOrigin, error) {
	out := make(map[string]model.QueryOrigin, len(queryIDs))
	if len(queryIDs) == 0 {
		return out, nil
	}
	if lookbackDays < 1 {
		lookbackDays = 1
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, c.queryOriginsSelect, queryIDs, lookbackDays, originsReadFloor(earliestStartUs))
	if err != nil {
		return nil, fmt.Errorf("failed to query query origins: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var queryID string
		var o model.QueryOrigin
		if err := rows.Scan(&queryID, &o.User, &o.InitialUser, &o.Address, &o.InitialAddress); err != nil {
			return nil, fmt.Errorf("failed to scan query origins: %w", err)
		}
		out[queryID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query origins iteration error: %w", err)
	}
	return out, nil
}

// FetchSpansForTraceIDs fetches every available span for the given trace IDs,
// bounded by lookbackDays partition pruning, limit, the operation blacklist,
// and since. Unlike FetchOpenTelemetrySpansWithOpts it does NOT re-run
// duration-based trace selection: the caller has already named the exact
// traces to inspect, so the command shows all of their spans. Trace IDs are
// accepted as strings and parsed to UUIDs; unparseable IDs are skipped. An
// empty (or all-invalid) trace-ID set returns no spans and no error.
//
// since is the earliest start the caller knows for the traces, or the start of
// the window they finished in; the zero time reads back to the day floor.
// It reads spans that finished from traceStartMargin before since. When a read
// returns a span that started before its floor, the next read covers the gap
// back to traceStartMargin before that start, until no read does or limit is
// reached. Every span read is so read with what ran under it: a long query
// that started before since and finished after it has its `query` span, which
// finishes after the spans under it, in the first read, and every span under
// it finishes after it starts. Each read covers only its own gap, so together
// they never read more than the day floor alone. trace_id is the last
// sort-key column, so the finish_time_us bounds are what keep the reads from
// covering every granule since the day floor.
func (c *ClickHouseReader) FetchSpansForTraceIDs(ctx context.Context, traceIDs []string, lookbackDays int, since time.Time, limit int, blacklistOperations []string) ([]model.OpenTelemetrySpan, error) {
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
	floor := spanReadFloorSince(since)

	spans, err := c.querySpansForTraceIDs(ctx, parsed, lookbackDays, floor, 0, effectiveLimit, blacklistOperations)
	if err != nil {
		return nil, err
	}
	// Spans of earlier reads all started after the current floor, so only the
	// latest read can reach further back. Every read that continues returns a
	// span, so the reads are bounded by limit.
	read := spans
	for len(read) > 0 && len(spans) < effectiveLimit {
		earliestStart := read[0].StartTimeUs
		for _, span := range read[1:] {
			earliestStart = min(earliestStart, span.StartTimeUs)
		}
		if earliestStart >= floor {
			break
		}
		reach := spanReadFloor(earliestStart)
		read, err = c.querySpansForTraceIDs(ctx, parsed, lookbackDays, reach, floor, effectiveLimit-len(spans), blacklistOperations)
		if err != nil {
			return nil, err
		}
		spans = append(spans, read...)
		floor = reach
	}
	return spans, nil
}

// querySpansForTraceIDs reads at most limit spans of traceIDs that finished
// at or after from and, when before is non-zero, before before.
func (c *ClickHouseReader) querySpansForTraceIDs(ctx context.Context, traceIDs []uuid.UUID, lookbackDays int, from, before uint64, limit int, blacklistOperations []string) ([]model.OpenTelemetrySpan, error) {
	spansQuery := c.spanSelect + " AND finish_time_us >= ?"
	spanParams := []interface{}{traceIDs, lookbackDays, from}
	if before > 0 {
		spansQuery += " AND finish_time_us < ?"
		spanParams = append(spanParams, before)
	}

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
	spanParams = append(spanParams, limit)

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

// ServerVersion returns the connected ClickHouse server's version string.
//
// The value describes the server this reader is connected to, not the node
// that produced any particular row: under cluster queries a read fans out to
// replicas that can be on different versions during a rolling upgrade. Callers
// must not present it as a per-row or per-node claim.
//
// Callers refresh this periodically rather than caching it at startup. A
// ClickHouse upgrade does not restart click-dog, so a value resolved once
// would misreport the version for the life of the process.
func (c *ClickHouseReader) ServerVersion(ctx context.Context) (string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	var version string
	if err := c.conn.QueryRow(queryCtx, serverVersionQuery).Scan(&version); err != nil {
		return "", fmt.Errorf("reading ClickHouse server version: %w", err)
	}
	return version, nil
}

// ServerTime returns the connected server's current time. The server stamps its
// span-log rows by this clock, so a span-log bound taken from it does not
// depend on this host's clock.
func (c *ClickHouseReader) ServerTime(ctx context.Context) (time.Time, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	var us int64
	if err := c.conn.QueryRow(queryCtx, serverTimeQuery).Scan(&us); err != nil {
		return time.Time{}, fmt.Errorf("reading ClickHouse server time: %w", err)
	}
	return time.UnixMicro(us), nil
}

// RunCanaryQuery runs a cheap COUNT query to check if long-running spans
// exist today. Uses a hardcoded 5s timeout to keep canary queries lightweight.
// Note: only counts spans with finish_date = today(); spans from previous
// calendar days are excluded even if they're still within the lookback window.
func (c *ClickHouseReader) RunCanaryQuery(ctx context.Context, thresholdMs int) (model.CanaryResult, error) {
	canaryCtx, cancel := context.WithTimeout(ctx, canaryQueryTimeout)
	defer cancel()

	// ClickHouse count() is UInt64. Scan its native type first: clickhouse-go
	// deliberately rejects scanning UInt64 directly into *int64.
	// The error goes back unwrapped: the processor names the canary step
	// ("canary query failed: ...") in its log line, its returned error, and
	// /status, and a second prefix here would double it.
	var rawCount uint64
	if err := c.conn.QueryRow(canaryCtx, c.canarySelect, thresholdMs).Scan(&rawCount); err != nil {
		return model.CanaryResult{}, err
	}
	if rawCount > math.MaxInt64 {
		return model.CanaryResult{}, fmt.Errorf("canary count %d exceeds int64 result capacity", rawCount)
	}
	count := int64(rawCount)

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
