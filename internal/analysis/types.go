// Package analysis owns the query-analysis report contract and the
// deterministic Phase 1 analyzers. It is pure: analyzers operate only on
// AnalysisInput, and the package must not import internal/clickhouse — the
// ClickHouse bridge that builds AnalysisInput lives with the CLI command.
package analysis

import (
	"context"
	"time"

	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	// ReportSchemaVersion versions the AnalysisReport JSON contract.
	ReportSchemaVersion = "analysis.report.v1"

	// ComparisonReportSchemaVersion is emitted only when a baseline comparison
	// is requested, preserving the strict v1 shape for ordinary analysis.
	ComparisonReportSchemaVersion = "analysis.report.v2"

	// FindingSchemaVersion versions the Finding JSON contract. It is also the
	// leading component of the finding ID hash input, so it only changes when
	// the ID input contract changes.
	FindingSchemaVersion = "analysis.finding.v1"
)

// Severity orders findings: critical > warning > info.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// severityRank returns the sort rank for a severity (lower sorts first).
// Unknown severities sort last.
func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityWarning:
		return 1
	case SeverityInfo:
		return 2
	default:
		return 3
	}
}

// AnalysisReport is the full result for one requested window.
//
// Warnings holds command setup, config, and environment warnings detected
// before or around report construction. Data-quality and prerequisite
// warnings discovered while building analysis input belong in
// CoverageSummary.Warnings instead.
type AnalysisReport struct {
	SchemaVersion string             `json:"schema_version"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Window        AnalysisWindow     `json:"window"`
	Config        ReportConfig       `json:"config"`
	Coverage      CoverageSummary    `json:"coverage"`
	Findings      []Finding          `json:"findings"`
	AnalyzerRuns  []AnalyzerRun      `json:"analyzer_runs"`
	Comparison    *ComparisonSummary `json:"comparison,omitempty"`
	Warnings      []string           `json:"warnings,omitempty"`
}

// ReportConfig is operational metadata only. It must never include ClickHouse
// passwords, exporter tokens, webhook URLs, log file paths, or exporter
// endpoints.
type ReportConfig struct {
	ConfigPath         string `json:"config_path,omitempty"`
	ClickHouseHost     string `json:"clickhouse_host"`
	ClickHouseCluster  string `json:"clickhouse_cluster,omitempty"`
	UseClusterQueries  bool   `json:"use_cluster_queries"`
	Lookback           string `json:"lookback"`
	Timeout            string `json:"timeout"`
	RedactDimensions   bool   `json:"redact_dimensions"`
	MinTraceDurationMs int    `json:"min_trace_duration_ms"`
	MaxTraceDurationMs int    `json:"max_trace_duration_ms,omitempty"`
	MinSpanDurationMs  int    `json:"min_span_duration_ms,omitempty"`
	MaxSpanDurationMs  int    `json:"max_span_duration_ms,omitempty"`
	MinExecutions      uint64 `json:"min_executions"`
	FamilyLimit        int    `json:"family_limit"`
	SpanSampleLimit    int    `json:"span_sample_limit"`
	QueryPreviewLength int    `json:"query_preview_length"`
}

// AnalysisWindow is the bounded report window, ending at command start.
type AnalysisWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// CoverageSummary reports how complete the analysis input is. Warnings holds
// data-quality and prerequisite warnings discovered while building analysis
// input, such as span sampling being unavailable.
type CoverageSummary struct {
	NormalizedQuerySupported    bool     `json:"normalized_query_supported"`
	QueryFamilyRollupsSupported bool     `json:"query_family_rollups_supported"`
	QueryFamilyCount            int      `json:"query_family_count"`
	SpanSampleAvailable         bool     `json:"span_sample_available"`
	SpanSampleSize              int      `json:"span_sample_size"`
	SpansWithQueryID            int      `json:"spans_with_query_id"`
	QueryIDRatio                float64  `json:"query_id_ratio"`
	QueryLogByIDLookbackDays    int      `json:"query_log_by_id_lookback_days"`
	AttributionFamilyCount      int      `json:"attribution_family_count"`
	Warnings                    []string `json:"warnings,omitempty"`
}

// AnalysisInput is the bounded, read-only data analyzers operate on. Maps are
// keyed by rollup FamilyID.
type AnalysisInput struct {
	Window              AnalysisWindow
	Config              ReportConfig
	Coverage            CoverageSummary
	Families            []model.QueryFamilyRollup
	AttributionByFamily map[string]FamilyAttributionCoverage
	DimensionsByFamily  map[string]FamilyDimensionBreakdown
	Comparison          *BaselineComparison
}

// FamilyAttributionCoverage counts log_comment attribution over the bounded
// span sample mapped back to one query family.
type FamilyAttributionCoverage struct {
	FamilyID                string  `json:"family_id"`
	SampledSpans            int     `json:"sampled_spans"`
	WithLogCommentApp       int     `json:"with_log_comment_app"`
	WithLogCommentQueryName int     `json:"with_log_comment_query_name"`
	AppRatio                float64 `json:"app_ratio"`
	QueryNameRatio          float64 `json:"query_name_ratio"`
}

// FamilyDimensionBreakdown holds per-dimension execution counts for one
// family. Values is keyed by dimension name (user, client_name,
// client_hostname); each slice is sorted by ExecutionCount descending, then
// Value ascending.
type FamilyDimensionBreakdown struct {
	FamilyID string                    `json:"family_id"`
	Values   map[string][]DimensionHit `json:"values"`
}

// DimensionHit is one dimension value's executions within a family. Ratio is
// against the rollup family execution count and stored clamped to [0, 1]
// because the rollup query and dimension-count query are separate reads and
// can race near the window edge. Threshold comparisons use this stored value
// so JSON output and analyzer behavior agree.
type DimensionHit struct {
	Value          string  `json:"value"`
	ExecutionCount uint64  `json:"execution_count"`
	Ratio          float64 `json:"ratio"`
}

// Finding is one actionable query-analysis observation.
//
// Phase 1 confidence is not a probability score: all Phase 1 analyzers set
// 1.0 because findings are deterministic threshold observations and the
// evidence carries magnitude.
//
// Evidence values are limited to booleans, strings, finite numbers, and
// pre-sorted arrays of those scalars; the registry validates this before
// render because encoding/json rejects non-finite floats.
type Finding struct {
	SchemaVersion         string         `json:"schema_version"`
	ID                    string         `json:"id"`
	Analyzer              string         `json:"analyzer"`
	ConditionScope        string         `json:"-"` // Stable internal scope used by window-independent notification identity
	Severity              Severity       `json:"severity"`
	Confidence            float64        `json:"confidence"`
	Title                 string         `json:"title"`
	Summary               string         `json:"summary"`
	FamilyID              string         `json:"family_id,omitempty"`
	NormalizedQueryHashes []string       `json:"normalized_query_hashes,omitempty"`
	RepresentativeQuery   string         `json:"representative_query,omitempty"`
	WindowStart           time.Time      `json:"window_start"`
	WindowEnd             time.Time      `json:"window_end"`
	Evidence              map[string]any `json:"evidence"`
	Recommendation        string         `json:"recommendation"`
}

// Analyzer turns analysis input into findings. Implementations must be
// deterministic and side-effect free. An analyzer that returns findings
// alongside a non-nil error has those findings discarded by the registry; to
// include partial results it must return them with a nil error.
type Analyzer interface {
	Name() string
	Analyze(ctx context.Context, input AnalysisInput) ([]Finding, error)
}

// AnalyzerRun records one analyzer execution. Findings counts findings
// included in the report, not findings produced before a discard. DurationMs
// covers only the Analyze() call; input building is centralized before the
// registry runs.
type AnalyzerRun struct {
	Name       string `json:"name"`
	Findings   int    `json:"findings"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}
