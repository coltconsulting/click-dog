package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// ClickHouse data-plane readiness checks (the live part of `click-dog check`)
//
// A bare ClickHouse ping can report success even when the data plane Datadog
// dashboards depend on is not ready: the span log or query log may be
// missing/ungranted, no recent spans may exist, the duration threshold may
// exclude everything, spans may lack clickhouse.query_id (breaking enrichment +
// user filters), or normalized_query_hash may be unavailable (empty
// query-family widgets). `click-dog check` runs these probes by default;
// `--quick` (and the offline `-validate` mode) skip them.
//
// Gathering (live queries) is split from evaluation (pure classification) so
// the pass/warn/fail decisions are unit-testable without a live connection,
// mirroring the slowQueriesBuilder split elsewhere in this package.
// ---------------------------------------------------------------------------

// ReadinessStatus is the outcome of a single deep readiness check.
type ReadinessStatus int

const (
	// ReadinessPass means the prerequisite is satisfied.
	ReadinessPass ReadinessStatus = iota
	// ReadinessWarn means a non-fatal gap exists (empty/recent-data or an
	// optional capability). The deep check does not fail the command on warn.
	ReadinessWarn
	// ReadinessFail means a required table/grant is missing. The command fails.
	ReadinessFail
	// ReadinessSkip means the check did not apply (e.g. nothing to sample).
	ReadinessSkip
)

func (s ReadinessStatus) String() string {
	switch s {
	case ReadinessPass:
		return "PASS"
	case ReadinessWarn:
		return "WARN"
	case ReadinessFail:
		return "FAIL"
	case ReadinessSkip:
		return "SKIP"
	default:
		return "?"
	}
}

// ReadinessCheck is one line of the deep readiness report.
type ReadinessCheck struct {
	Name   string          // short label, e.g. "span_log readable"
	Status ReadinessStatus // pass/warn/fail/skip
	Detail string          // what was observed
	Remedy string          // the exact SQL/config prerequisite to fix (warn/fail only)
}

// ReadinessOptions parameterises the deep checks.
type ReadinessOptions struct {
	// Lookback bounds the "recent data" window for span/query-log checks.
	Lookback time.Duration
	// MinTraceDurationMs mirrors monitor.min_trace_duration_ms so the duration
	// distribution check can report whether any recent span would qualify.
	MinTraceDurationMs int
	// EnrichmentNeeded is true when the current config relies on system.query_log
	// (enrich_from_query_log, or user whitelist/blacklist). When true a missing
	// query_log fails the command; otherwise it only warns.
	EnrichmentNeeded bool
	// SampleLimit caps how many distinct span query IDs are pulled to probe the
	// enrichment join. Defaults to defaultReadinessSampleLimit when <= 0.
	SampleLimit int
}

const (
	defaultReadinessLookback    = 24 * time.Hour
	defaultReadinessSampleLimit = 20
)

// Table references use the same {TABLE_*} placeholders as reader.go and are
// resolved via strings.ReplaceAll against the reader's cluster-aware refs.
// Runtime values flow through ? parameters only.

const doctorSpanStatsSQL = `
			SELECT
				count() AS total,
				countIf((finish_time_us - start_time_us) >= ? * 1000) AS qualifying,
				-- finish_time_us/start_time_us are UInt64, but ClickHouse types
				-- their subtraction as Int64 (it permits negatives), so the result
				-- column is Int64 and scanning it into a uint64 dest fails
				-- ("converting Int64 to *uint64"). The delta is always >= 0
				-- (finish >= start), so cast back to UInt64 to match the scan dest.
				toUInt64(max(finish_time_us - start_time_us)) AS max_us
			FROM ` + tblSpanLog + `
			WHERE finish_date >= today() - INTERVAL ? DAY
			  AND finish_time_us >= toUnixTimestamp64Micro(now64() - INTERVAL ? SECOND)
		`

const doctorQueryIDPresenceSQL = `
			SELECT
				count() AS sampled,
				countIf(attribute['clickhouse.query_id'] != '') AS with_qid
			FROM ` + tblSpanLog + `
			WHERE finish_date >= today() - INTERVAL ? DAY
			  AND finish_time_us >= toUnixTimestamp64Micro(now64() - INTERVAL ? SECOND)
		`

const doctorSampleQueryIDsSQL = `
			SELECT DISTINCT attribute['clickhouse.query_id'] AS qid
			FROM ` + tblSpanLog + `
			WHERE finish_date >= today() - INTERVAL ? DAY
			  AND finish_time_us >= toUnixTimestamp64Micro(now64() - INTERVAL ? SECOND)
			  AND attribute['clickhouse.query_id'] != ''
			LIMIT ?
		`

// event_date (the partition key) is rounded up by a day for pruning; the
// precise event_time bound makes the reported count match the actual lookback
// window so the "rows in the last <lookback>" detail isn't overstated.
const doctorQueryLogCountSQL = `
			SELECT count() AS total
			FROM ` + tblQueryLog + `
			WHERE event_date >= today() - INTERVAL ? DAY
			  AND event_time >= now() - INTERVAL ? SECOND
		`

// The type filter must mirror the runtime enrichment join
// (queryLogEnrichSelectSQL in reader.go): runtime only joins terminal rows
// (QueryFinish / ExceptionWhileProcessing), so the probe must too — counting
// QueryStart rows would overstate join viability versus what enrichment can
// actually match. countDistinct keeps "matched" comparable to the number of
// sampled query IDs (query_log has multiple rows per query_id).
const doctorEnrichJoinSQL = `
			SELECT countDistinct(query_id) AS matched
			FROM ` + tblQueryLog + `
			WHERE query_id IN ?
			  AND type IN ('QueryFinish', 'ExceptionWhileProcessing')
			  AND event_date >= today() - INTERVAL ? DAY
		`

// readinessObs holds the raw observations from the live queries. Evaluation is
// a pure function of this struct.
type readinessObs struct {
	lookback           time.Duration
	minTraceDurationMs int
	enrichmentNeeded   bool
	useClusterQueries  bool
	clusterName        string

	spanLogErr     error
	spanTotal      uint64
	spanQualifying uint64
	spanMaxUs      uint64

	qidSampleErr error
	qidSampled   uint64
	qidWith      uint64

	queryLogErr   error
	queryLogCount uint64

	joinSampleErr error
	joinSampleIDs int
	joinErr       error
	joinMatched   uint64

	normErr              error
	normSupported        bool
	normClusterReplicas  uint64
	normClusterSupported uint64
}

// RunReadinessChecks performs the deep ClickHouse readiness diagnostics and
// returns one ReadinessCheck per probed prerequisite. It never returns an
// error: per-query failures are folded into the report as FAIL/WARN lines so
// the operator sees every prerequisite in one pass.
func (c *ClickHouseReader) RunReadinessChecks(ctx context.Context, opts ReadinessOptions) []ReadinessCheck {
	if opts.Lookback <= 0 {
		opts.Lookback = defaultReadinessLookback
	}
	if opts.SampleLimit <= 0 {
		opts.SampleLimit = defaultReadinessSampleLimit
	}
	return evaluateReadiness(c.gatherReadiness(ctx, opts))
}

func (c *ClickHouseReader) gatherReadiness(ctx context.Context, opts ReadinessOptions) readinessObs {
	obs := readinessObs{
		lookback:           opts.Lookback,
		minTraceDurationMs: opts.MinTraceDurationMs,
		enrichmentNeeded:   opts.EnrichmentNeeded,
		useClusterQueries:  c.useClusterQueries,
		clusterName:        c.cluster,
	}

	lookbackDays := int(opts.Lookback.Hours()/24) + 1
	lookbackSeconds := int(opts.Lookback.Seconds())

	// span_log: selectability + recent count + duration distribution in one pass.
	spanStatsSQL := strings.ReplaceAll(doctorSpanStatsSQL, tblSpanLog, c.spanLogRef)
	obs.spanLogErr = c.scanRow(ctx, spanStatsSQL,
		[]interface{}{opts.MinTraceDurationMs, lookbackDays, lookbackSeconds},
		&obs.spanTotal, &obs.spanQualifying, &obs.spanMaxUs)

	// clickhouse.query_id presence — only probed when enrichment/user-filters
	// are configured. Otherwise query_id on spans is irrelevant and the extra
	// attribute-Map scan over the lookback isn't worth running. Also requires
	// the span log to be readable.
	if opts.EnrichmentNeeded && obs.spanLogErr == nil {
		qidSQL := strings.ReplaceAll(doctorQueryIDPresenceSQL, tblSpanLog, c.spanLogRef)
		obs.qidSampleErr = c.scanRow(ctx, qidSQL,
			[]interface{}{lookbackDays, lookbackSeconds},
			&obs.qidSampled, &obs.qidWith)
	}

	// query_log selectability.
	queryLogSQL := strings.ReplaceAll(doctorQueryLogCountSQL, tblQueryLog, c.queryLogRef)
	obs.queryLogErr = c.scanRow(ctx, queryLogSQL,
		[]interface{}{lookbackDays, lookbackSeconds}, &obs.queryLogCount)

	// Enrichment join viability: pull a few live span query IDs and look them
	// up in query_log. Same gating as the query_id probe — only meaningful, and
	// only worth the sample + lookup, when enrichment/user-filters are active
	// and both tables read.
	if opts.EnrichmentNeeded && obs.spanLogErr == nil && obs.queryLogErr == nil {
		ids, err := c.sampleSpanQueryIDs(ctx, lookbackDays, lookbackSeconds, opts.SampleLimit)
		obs.joinSampleErr = err
		obs.joinSampleIDs = len(ids)
		if err == nil && len(ids) > 0 {
			joinSQL := strings.ReplaceAll(doctorEnrichJoinSQL, tblQueryLog, c.queryLogRef)
			obs.joinErr = c.scanRow(ctx, joinSQL,
				[]interface{}{ids, lookbackDays}, &obs.joinMatched)
		}
	}

	// normalized_query_hash support (drives query-family dashboard widgets).
	if c.useClusterQueries {
		if c.cluster == "" {
			obs.normErr = errors.New("cluster name is empty")
		} else {
			obs.normErr = c.scanRow(ctx, buildClusterNormalizedProbeSQL(c.cluster), nil, &obs.normClusterReplicas, &obs.normClusterSupported)
			obs.normSupported = obs.normErr == nil && obs.normClusterReplicas > 0 && obs.normClusterSupported == obs.normClusterReplicas
		}
	} else {
		var count uint64
		obs.normErr = c.scanRow(ctx, normalizedQueryHashProbeSQL, nil, &count)
		obs.normSupported = obs.normErr == nil && count > 0
	}

	return obs
}

// scanRow runs a single-row query under the reader's query timeout.
func (c *ClickHouseReader) scanRow(ctx context.Context, query string, params []interface{}, dest ...interface{}) error {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()
	return c.conn.QueryRow(queryCtx, query, params...).Scan(dest...)
}

func (c *ClickHouseReader) sampleSpanQueryIDs(ctx context.Context, lookbackDays, lookbackSeconds, limit int) ([]string, error) {
	query := strings.ReplaceAll(doctorSampleQueryIDsSQL, tblSpanLog, c.spanLogRef)

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, query, lookbackDays, lookbackSeconds, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// evaluateReadiness maps raw observations to the pass/warn/fail report. Pure,
// so all scenarios (healthy, missing span log, missing query log, no recent
// data, enrichment unavailable) are table-testable.
func evaluateReadiness(obs readinessObs) []ReadinessCheck {
	var checks []ReadinessCheck
	add := func(name string, status ReadinessStatus, detail, remedy string) {
		checks = append(checks, ReadinessCheck{Name: name, Status: status, Detail: detail, Remedy: remedy})
	}
	lookbackDesc := formatLookback(obs.lookback)

	// --- span_log selectable + recent data + duration distribution ---
	if obs.spanLogErr != nil {
		add("span_log readable", ReadinessFail,
			fmt.Sprintf("system.opentelemetry_span_log is missing or unreadable: %v", obs.spanLogErr),
			"enable OpenTelemetry span logging in ClickHouse and grant SELECT on system.opentelemetry_span_log to the monitoring user")
	} else {
		add("span_log readable", ReadinessPass, "system.opentelemetry_span_log is selectable", "")
		if obs.spanTotal == 0 {
			add("span_log recent data", ReadinessWarn,
				fmt.Sprintf("no spans recorded in the last %s", lookbackDesc),
				"confirm OpenTelemetry tracing is active (opentelemetry_start_trace_probability > 0) or widen --lookback")
		} else {
			add("span_log recent data", ReadinessPass,
				fmt.Sprintf("%d spans recorded in the last %s", obs.spanTotal, lookbackDesc), "")

			maxMs := obs.spanMaxUs / 1000
			if obs.minTraceDurationMs > 0 && obs.spanQualifying == 0 {
				add("duration vs min_trace_duration_ms", ReadinessWarn,
					fmt.Sprintf("0 of %d recent spans meet min_trace_duration_ms=%d (max observed %dms)",
						obs.spanTotal, obs.minTraceDurationMs, maxMs),
					"lower monitor.min_trace_duration_ms below the observed max, or confirm slow queries actually occur")
			} else {
				add("duration vs min_trace_duration_ms", ReadinessPass,
					fmt.Sprintf("%d of %d recent spans meet min_trace_duration_ms=%d (max observed %dms)",
						obs.spanQualifying, obs.spanTotal, obs.minTraceDurationMs, maxMs), "")
			}
		}
	}

	// --- clickhouse.query_id presence on spans ---
	switch {
	case obs.spanLogErr != nil:
		// Span log already failed; sampling is impossible. No separate line.
	case !obs.enrichmentNeeded:
		add("clickhouse.query_id on spans", ReadinessSkip,
			"enrichment and user filters are not configured; query_id propagation not checked", "")
	case obs.qidSampleErr != nil:
		add("clickhouse.query_id on spans", ReadinessWarn,
			fmt.Sprintf("could not sample span attributes: %v", obs.qidSampleErr), "")
	case obs.qidSampled == 0:
		add("clickhouse.query_id on spans", ReadinessSkip, "no recent spans to sample", "")
	case obs.qidWith == 0:
		add("clickhouse.query_id on spans", ReadinessWarn,
			fmt.Sprintf("0 of %d recent spans carry clickhouse.query_id", obs.qidSampled),
			"query_log enrichment and user filters need clickhouse.query_id on spans; ensure ClickHouse propagates query_id into span attributes")
	default:
		add("clickhouse.query_id on spans", ReadinessPass,
			fmt.Sprintf("%d of %d recent spans carry clickhouse.query_id", obs.qidWith, obs.qidSampled), "")
	}

	// --- query_log selectable ---
	if obs.queryLogErr != nil {
		status := ReadinessWarn
		detail := fmt.Sprintf("system.query_log is missing or unreadable: %v", obs.queryLogErr)
		if obs.enrichmentNeeded {
			status = ReadinessFail
		} else {
			detail += " (not required by current config)"
		}
		add("query_log readable", status, detail,
			"grant SELECT on system.query_log (needed for enrichment, backfill, user filters, and query-family dashboards)")
	} else {
		add("query_log readable", ReadinessPass,
			fmt.Sprintf("system.query_log is selectable (%d rows in the last %s)", obs.queryLogCount, lookbackDesc), "")
	}

	// --- enrichment join viability ---
	switch {
	case obs.spanLogErr != nil || obs.queryLogErr != nil:
		// One of the tables is unreadable; the join verdict would be misleading.
	case !obs.enrichmentNeeded:
		add("query_log enrichment join", ReadinessSkip,
			"enrichment and user filters are not configured; enrichment join not checked", "")
	case obs.joinSampleErr != nil:
		add("query_log enrichment join", ReadinessWarn,
			fmt.Sprintf("could not sample span query IDs: %v", obs.joinSampleErr), "")
	case obs.joinSampleIDs == 0:
		add("query_log enrichment join", ReadinessSkip,
			"no span query IDs available to test the enrichment join", "")
	case obs.joinErr != nil:
		add("query_log enrichment join", ReadinessWarn,
			fmt.Sprintf("query_log lookup failed: %v", obs.joinErr), "")
	case obs.joinMatched == 0:
		add("query_log enrichment join", ReadinessWarn,
			fmt.Sprintf("0 of %d sampled span query IDs found in system.query_log", obs.joinSampleIDs),
			"query_log retention may be shorter than span retention, or the monitoring user lacks SELECT on system.query_log")
	default:
		add("query_log enrichment join", ReadinessPass,
			fmt.Sprintf("%d of %d sampled span query IDs found in system.query_log", obs.joinMatched, obs.joinSampleIDs), "")
	}

	// --- normalized_query_hash support ---
	switch {
	case obs.normErr != nil:
		if obs.useClusterQueries {
			add("normalized_query_hash support", ReadinessWarn,
				fmt.Sprintf("could not verify system.query_log.normalized_query_hash on all cluster replicas: %v", obs.normErr),
				"check that clickhouse.cluster matches a ClickHouse cluster and that the monitoring user has SELECT on system.columns; restart click-dog after fixing because the probe runs once at startup")
		} else {
			add("normalized_query_hash support", ReadinessWarn,
				fmt.Sprintf("could not probe system.query_log.normalized_query_hash: %v", obs.normErr),
				"grant SELECT on system.columns to the monitoring user so click-dog can detect normalized_query_hash support")
		}
	// Keep the zero-replica case ahead of the generic mixed-support case so
	// operators do not see a confusing "0 of 0 replicas" message.
	case obs.useClusterQueries && obs.normClusterReplicas == 0:
		add("normalized_query_hash support", ReadinessWarn,
			"could not verify any cluster replicas expose system.query_log; normalized query fields are disabled",
			"check that clickhouse.cluster matches a ClickHouse cluster and that the monitoring user has SELECT on system.columns")
	case obs.useClusterQueries && !obs.normSupported:
		add("normalized_query_hash support", ReadinessWarn,
			fmt.Sprintf("system.query_log.normalized_query_hash is available on %d of %d cluster replicas; normalized query fields are disabled",
				obs.normClusterSupported, obs.normClusterReplicas),
			"upgrade lagging replicas so every node in the cluster exposes system.query_log.normalized_query_hash, then restart click-dog (the probe runs once at startup)")
	case !obs.normSupported:
		add("normalized_query_hash support", ReadinessWarn,
			"system.query_log.normalized_query_hash is unavailable; query-family dashboard widgets will be empty",
			"upgrade ClickHouse to a version that exposes normalized_query_hash")
	default:
		if obs.useClusterQueries {
			add("normalized_query_hash support", ReadinessPass,
				fmt.Sprintf("system.query_log.normalized_query_hash is available on all %d cluster replicas; normalized query fields are enabled",
					obs.normClusterReplicas), "")
		} else {
			add("normalized_query_hash support", ReadinessPass,
				"system.query_log.normalized_query_hash is available", "")
		}
	}

	// --- cluster query mode ---
	if obs.useClusterQueries {
		add("cluster query mode", ReadinessPass,
			fmt.Sprintf("enabled (cluster=%q); span_log and query_log checks above used cluster() references", obs.clusterName), "")
	}

	return checks
}

// formatLookback renders a lookback window for human-readable detail strings.
func formatLookback(d time.Duration) string {
	if d <= 0 {
		d = defaultReadinessLookback
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return d.String()
}
