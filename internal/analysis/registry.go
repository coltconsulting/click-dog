package analysis

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Registry holds the compiled-in analyzers in their stable execution order.
// Execution is sequential in Phase 1: AnalysisInput is read-only so parallel
// execution is possible, but sequential keeps memory and diagnostics
// predictable while the contract is new.
type Registry struct {
	analyzers []Analyzer
}

// NewRegistry returns the Phase 1 registry: coverage, resource_hog,
// attribution_gap, skew.
func NewRegistry() *Registry {
	return &Registry{analyzers: []Analyzer{
		&coverageAnalyzer{},
		&resourceHogAnalyzer{},
		&attributionGapAnalyzer{},
		&skewAnalyzer{},
	}}
}

// NewRegistryWithRegression returns the Phase 1 registry followed by the two
// baseline-aware analyzers. Callers use it only for explicit comparisons so
// ordinary v1 reports keep their original analyzer inventory.
func NewRegistryWithRegression() *Registry {
	reg := NewRegistry()
	reg.analyzers = append(reg.analyzers,
		&latencyRegressionAnalyzer{},
		&failureSpikeAnalyzer{},
	)
	return reg
}

// AnalyzerNames returns the registry's analyzer names in execution order.
// Used by the domain-language parity test and useful for diagnostics.
func (r *Registry) AnalyzerNames() []string {
	names := make([]string, len(r.analyzers))
	for i, a := range r.analyzers {
		names[i] = a.Name()
	}
	return names
}

// Run executes every analyzer in order and returns the included findings plus
// one AnalyzerRun per analyzer. If an analyzer returns findings and an error,
// the findings are discarded and the run records the error with Findings 0.
// Findings with invalid evidence are dropped individually and the run error
// names the rejected finding ID.
func (r *Registry) Run(ctx context.Context, input AnalysisInput) ([]Finding, []AnalyzerRun) {
	var findings []Finding
	runs := make([]AnalyzerRun, 0, len(r.analyzers))

	for _, a := range r.analyzers {
		start := time.Now()
		produced, err := a.Analyze(ctx, input)
		run := AnalyzerRun{
			Name:       a.Name(),
			DurationMs: time.Since(start).Milliseconds(),
		}

		if err != nil {
			// Partial-failure semantics stay deterministic: findings returned
			// with an error are discarded, never half-included.
			run.Error = err.Error()
			runs = append(runs, run)
			continue
		}

		var rejected []string
		for _, f := range produced {
			normalizeFinding(&f, input.Window)
			if vErr := validateEvidence(f.Evidence); vErr != nil {
				rejected = append(rejected, fmt.Sprintf("finding %s: %v", f.ID, vErr))
				continue
			}
			findings = append(findings, f)
			run.Findings++
		}
		if len(rejected) > 0 {
			run.Error = strings.Join(rejected, "; ")
		}
		runs = append(runs, run)
	}

	return findings, runs
}

// normalizeFinding applies the registry-owned common fields so analyzers
// cannot drift on schema version, window stamps, or hash ordering.
func normalizeFinding(f *Finding, window AnalysisWindow) {
	f.SchemaVersion = FindingSchemaVersion
	f.WindowStart = window.Start
	f.WindowEnd = window.End
	sortHashStrings(f.NormalizedQueryHashes)
	if f.Evidence == nil {
		f.Evidence = map[string]any{}
	}
}

// validateEvidence enforces the Phase 1 evidence contract: booleans, strings,
// finite numbers, and arrays of those scalars. Non-finite floats are invalid
// because encoding/json rejects them.
func validateEvidence(evidence map[string]any) error {
	keys := make([]string, 0, len(evidence))
	for k := range evidence {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := validateEvidenceValue(evidence[k]); err != nil {
			return fmt.Errorf("evidence %q: %w", k, err)
		}
	}
	return nil
}

func validateEvidenceValue(v any) error {
	switch val := v.(type) {
	case []string, []int, []int64, []uint64:
		return nil
	case []float64:
		for _, f := range val {
			if err := validateFinite(f); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, elem := range val {
			if err := validateEvidenceScalar(elem); err != nil {
				return err
			}
		}
		return nil
	default:
		return validateEvidenceScalar(v)
	}
}

func validateEvidenceScalar(v any) error {
	switch val := v.(type) {
	case bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return nil
	case float32:
		return validateFinite(float64(val))
	case float64:
		return validateFinite(val)
	case nil:
		return fmt.Errorf("nil evidence value")
	default:
		return fmt.Errorf("unsupported evidence type %T", v)
	}
}

func validateFinite(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("non-finite float %v", f)
	}
	return nil
}

// roundRatio rounds evidence ratios to 4 decimal places so JSON output stays
// tidy and deterministic without losing threshold-relevant precision.
func roundRatio(r float64) float64 {
	return math.Round(r*10000) / 10000
}

// SortFindings applies the stable JSON report order: severity, analyzer,
// family id, id. Table output sorts differently (severity, analyzer,
// summary) because it prioritizes human scanning over machine diffs.
func SortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if severityRank(a.Severity) != severityRank(b.Severity) {
			return severityRank(a.Severity) < severityRank(b.Severity)
		}
		if a.Analyzer != b.Analyzer {
			return a.Analyzer < b.Analyzer
		}
		if a.FamilyID != b.FamilyID {
			return a.FamilyID < b.FamilyID
		}
		return a.ID < b.ID
	})
}

// BuildReport runs the registry and assembles the sorted AnalysisReport.
// warnings is for command setup / config / environment warnings; coverage
// warnings travel inside input.Coverage.
func BuildReport(ctx context.Context, reg *Registry, input AnalysisInput, generatedAt time.Time, warnings []string) AnalysisReport {
	findings, runs := reg.Run(ctx, input)
	SortFindings(findings)
	if findings == nil {
		findings = []Finding{}
	}
	report := AnalysisReport{
		SchemaVersion: ReportSchemaVersion,
		GeneratedAt:   generatedAt.UTC(),
		Window:        input.Window,
		Config:        input.Config,
		Coverage:      input.Coverage,
		Findings:      findings,
		AnalyzerRuns:  runs,
		Warnings:      warnings,
	}
	if input.Comparison != nil {
		report.SchemaVersion = ComparisonReportSchemaVersion
		report.Comparison = &input.Comparison.Summary
	}
	return report
}
