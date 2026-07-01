package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/queryfamily"
)

// QueryFamilyDimension names a fixed, allowlisted system.query_log column for
// the analysis-only dimension-count query. Dimension identifiers are never
// user input; only the constants below reach SQL.
type QueryFamilyDimension string

const (
	QueryFamilyDimensionUser   QueryFamilyDimension = "user"
	QueryFamilyDimensionClient QueryFamilyDimension = "client_name"
	QueryFamilyDimensionHost   QueryFamilyDimension = "client_hostname"
)

// queryFamilyDimensions is the fixed allowlist, in stable query order.
// tables is intentionally excluded: it is an array column needing arrayJoin
// fan-out, which belongs to the later hot-table analyzer.
var queryFamilyDimensions = []QueryFamilyDimension{
	QueryFamilyDimensionUser,
	QueryFamilyDimensionClient,
	QueryFamilyDimensionHost,
}

// maxQueryFamilyDimensionTopK bounds the validated LIMIT n BY literal.
const maxQueryFamilyDimensionTopK = 100

// QueryFamilyDimensionCountOptions bounds the dimension-count query. The
// MinExecutionCount / ExactGroupLimit pair mirrors QueryFamilyRollupOptions so
// the counted hash population matches the families the rollup query selects.
type QueryFamilyDimensionCountOptions struct {
	StartTime         time.Time
	EndTime           time.Time
	MinExecutionCount uint64
	ExactGroupLimit   int // Silently clamped to maxQueryFamilyExactGroupLimit.
	TopK              int // Values kept per hash and dimension; defaults to queryfamily.TopKLimit.
}

// QueryFamilyDimensionCount is one (normalized hash, dimension, value)
// execution count. NormalizedQueryHash is a uint64 matching
// model.QueryFamilyRollup.MemberHashesSorted so callers can join counts to
// rollup families by member hash.
type QueryFamilyDimensionCount struct {
	NormalizedQueryHash uint64
	Dimension           QueryFamilyDimension
	Value               string
	ExecutionCount      uint64
}

// FetchQueryFamilyDimensionCounts returns per-dimension execution counts for
// the exact normalized-query groups in the window. It exists for the skew
// analyzer; the exporter data path is intentionally untouched.
//
// All three allowlisted dimensions are counted in a single query_log scan: a
// parallel ARRAY JOIN unpivots the fixed user/client_name/client_hostname
// columns into (dimension, value) pairs, and one shared subquery restricts the
// hash population. This reads query_log twice (main scan + top-family
// subquery) instead of once per dimension, which matters because the command
// runs read-only against a shared system table on busy clusters.
func (c *ClickHouseReader) FetchQueryFamilyDimensionCounts(ctx context.Context, opts QueryFamilyDimensionCountOptions) ([]QueryFamilyDimensionCount, error) {
	if !c.queryLogNormalized {
		return nil, ErrQueryFamilyRollupsUnsupported
	}
	opts, err := normalizeQueryFamilyDimensionCountOptions(opts)
	if err != nil {
		return nil, err
	}

	b := c.queryFamilyDimensionCountsBuilder(opts)

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	rows, err := c.conn.Query(queryCtx, b.query, b.params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query dimension counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var counts []QueryFamilyDimensionCount
	for rows.Next() {
		var (
			count QueryFamilyDimensionCount
			name  string
		)
		if err := rows.Scan(&count.NormalizedQueryHash, &name, &count.Value, &count.ExecutionCount); err != nil {
			return nil, fmt.Errorf("failed to scan dimension count: %w", err)
		}
		dim := QueryFamilyDimension(name)
		if !isAllowlistedDimension(dim) {
			// Defensive: dimension_name comes from our own literal allowlist,
			// so this should be unreachable. Skip rather than trust unknown
			// labels into findings.
			continue
		}
		count.Dimension = dim
		counts = append(counts, count)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dimension count row iteration error: %w", err)
	}

	sortQueryFamilyDimensionCounts(counts)
	return counts, nil
}

func normalizeQueryFamilyDimensionCountOptions(opts QueryFamilyDimensionCountOptions) (QueryFamilyDimensionCountOptions, error) {
	if opts.StartTime.IsZero() || opts.EndTime.IsZero() {
		return opts, fmt.Errorf("query family dimension counts require explicit start and end times")
	}
	if opts.StartTime.After(opts.EndTime) {
		return opts, fmt.Errorf("query family dimension count start time %s is after end time %s", opts.StartTime.Format(time.RFC3339), opts.EndTime.Format(time.RFC3339))
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
	if opts.TopK <= 0 {
		opts.TopK = queryfamily.TopKLimit
	}
	if opts.TopK > maxQueryFamilyDimensionTopK {
		return opts, fmt.Errorf("query family dimension top-k %d exceeds maximum %d", opts.TopK, maxQueryFamilyDimensionTopK)
	}
	return opts, nil
}

// queryFamilyDimensionCountsBuilder assembles the single-scan dimension-count
// SQL. A parallel ARRAY JOIN unpivots the fixed allowlist columns into
// (dimension_name, dimension_value) pairs so all three dimensions are counted
// in one pass; the inner subquery restricts hashes to the same population the
// rollup exact-group query selects (window, type, min executions, top-N by
// execution count), so dimension counts join cleanly to rollup families.
//
// The dimension column names come only from queryFamilyDimensions constants —
// never user input — so embedding them in the array literals is safe; values
// are wrapped in toString() so the parallel value array has a uniform String
// element type regardless of LowCardinality columns. TopK is validated by
// normalizeQueryFamilyDimensionCountOptions and formatted as an integer
// literal because LIMIT n BY cannot take a driver parameter — the same pattern
// as topK(%d) in the rollup SQL. All runtime values (window bounds,
// MinExecutionCount, ExactGroupLimit) stay bound parameters.
func (c *ClickHouseReader) queryFamilyDimensionCountsBuilder(opts QueryFamilyDimensionCountOptions) *queryLogBuilder {
	nameArray, valueArray := dimensionArraySQL()
	b := &queryLogBuilder{
		query: fmt.Sprintf(`
			SELECT
				normalized_query_hash,
				dimension_name,
				dimension_value,
				count() AS execution_count
			FROM %[1]s
			ARRAY JOIN
				[%[2]s] AS dimension_name,
				[%[3]s] AS dimension_value
			WHERE event_time >= ?
			  AND event_time <= ?
			  AND type IN ('QueryFinish', 'ExceptionWhileProcessing')
			  AND normalized_query_hash != 0
			  AND dimension_value != ''`, c.queryLogRef, nameArray, valueArray),
		params: []interface{}{opts.StartTime, opts.EndTime},
	}
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)

	b.query += fmt.Sprintf(`
			  AND normalized_query_hash IN (
				SELECT normalized_query_hash
				FROM %s
				WHERE event_time >= ?
				  AND event_time <= ?
				  AND type IN ('QueryFinish', 'ExceptionWhileProcessing')
				  AND normalized_query_hash != 0`, c.queryLogRef)
	b.params = append(b.params, opts.StartTime, opts.EndTime)
	b.addUserFilters(c.whitelistUsers, c.blacklistUsers)
	b.query += `
				GROUP BY normalized_query_hash
				HAVING count() >= ?
				ORDER BY count() DESC
				LIMIT ?
			  )`
	b.params = append(b.params, opts.MinExecutionCount, opts.ExactGroupLimit)

	b.query += fmt.Sprintf(`
			GROUP BY normalized_query_hash, dimension_name, dimension_value
			ORDER BY normalized_query_hash ASC, dimension_name ASC, execution_count DESC, dimension_value ASC
			LIMIT %d BY normalized_query_hash, dimension_name`, opts.TopK)
	return b
}

// dimensionArraySQL renders the parallel ARRAY JOIN operands from the fixed
// allowlist: a name array of quoted dimension labels and a value array of the
// matching columns wrapped in toString(). Both are derived from
// queryFamilyDimensions so the dimension set has a single source of truth.
func dimensionArraySQL() (names string, values string) {
	quotedNames := make([]string, len(queryFamilyDimensions))
	toStrings := make([]string, len(queryFamilyDimensions))
	for i, dim := range queryFamilyDimensions {
		column := string(dim)
		quotedNames[i] = "'" + column + "'"
		toStrings[i] = "toString(" + column + ")"
	}
	return strings.Join(quotedNames, ", "), strings.Join(toStrings, ", ")
}

func isAllowlistedDimension(dim QueryFamilyDimension) bool {
	for _, d := range queryFamilyDimensions {
		if d == dim {
			return true
		}
	}
	return false
}

func queryFamilyDimensionRank(dim QueryFamilyDimension) int {
	for i, d := range queryFamilyDimensions {
		if d == dim {
			return i
		}
	}
	return len(queryFamilyDimensions)
}

// sortQueryFamilyDimensionCounts gives the single-scan result the same stable,
// allowlist-ordered shape the per-dimension loop used to produce: grouped by
// hash, then dimension allowlist order, then execution count desc, then value
// asc. SQL ORDER BY already returns rows sorted, but the Go sort keeps the
// contract independent of ClickHouse row ordering and the string-vs-allowlist
// difference in dimension_name ordering.
func sortQueryFamilyDimensionCounts(counts []QueryFamilyDimensionCount) {
	sort.SliceStable(counts, func(i, j int) bool {
		a, b := counts[i], counts[j]
		if a.NormalizedQueryHash != b.NormalizedQueryHash {
			return a.NormalizedQueryHash < b.NormalizedQueryHash
		}
		if ra, rb := queryFamilyDimensionRank(a.Dimension), queryFamilyDimensionRank(b.Dimension); ra != rb {
			return ra < rb
		}
		if a.ExecutionCount != b.ExecutionCount {
			return a.ExecutionCount > b.ExecutionCount
		}
		return a.Value < b.Value
	})
}
