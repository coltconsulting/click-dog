package analysis

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	// BaselineSchemaVersion versions the compact known-good snapshot contract.
	BaselineSchemaVersion = "analysis.baseline.v1"

	// ComparisonSchemaVersion versions the report comparison summary.
	ComparisonSchemaVersion = "analysis.comparison.v1"

	// BaselineMaxBytes bounds reads before JSON decoding.
	BaselineMaxBytes int64 = 8 << 20

	// BaselineStaleAfter conservatively disables regression claims when the
	// known-good window is more than 30 days behind the current window.
	BaselineStaleAfter = 30 * 24 * time.Hour

	// MaxBaselineExceptionCodes is the documented per-hash exception identity
	// cap in baseline artifacts and regression evidence.
	MaxBaselineExceptionCodes = 5
)

// BaselineCompatibility records only settings that affect exact-group
// selection, aggregation, or rollup identity. FilterFingerprint is a digest of
// the effective user filters; raw user names never enter the artifact.
type BaselineCompatibility struct {
	FamilyAlgorithmVersion    string  `json:"family_algorithm_version"`
	SimilarityThreshold       float64 `json:"similarity_threshold"`
	FilterFingerprint         string  `json:"filter_fingerprint"`
	MinExecutions             uint64  `json:"min_executions"`
	FamilyLimit               int     `json:"family_limit"`
	QueryPreviewLength        int     `json:"query_preview_length"`
	NormalizedQuerySupported  bool    `json:"normalized_query_supported"`
	QueryFamilyRollupsSupport bool    `json:"query_family_rollups_supported"`
}

// BaselineSnapshot is a privacy-minimized known-good analysis artifact. It
// deliberately omits query text, dimensions, config paths, endpoints, and
// credentials.
type BaselineSnapshot struct {
	SchemaVersion string                `json:"schema_version"`
	BaselineID    string                `json:"baseline_id"`
	CreatedAt     time.Time             `json:"created_at"`
	Window        AnalysisWindow        `json:"window"`
	Compatibility BaselineCompatibility `json:"compatibility"`
	Groups        []BaselineExactGroup  `json:"groups"`
}

// BaselineExactGroup stores comparison metrics by exact normalized hash. The
// hash is a decimal string so uint64 identities remain lossless in JSON tools.
type BaselineExactGroup struct {
	NormalizedQueryHash string                   `json:"normalized_query_hash"`
	FamilyID            string                   `json:"family_id"`
	ExecutionCount      uint64                   `json:"execution_count"`
	SuccessfulCount     uint64                   `json:"successful_count"`
	FailedCount         uint64                   `json:"failed_count"`
	FailureRate         float64                  `json:"failure_rate"`
	P95DurationMs       float64                  `json:"p95_duration_ms"`
	P99DurationMs       float64                  `json:"p99_duration_ms"`
	TopExceptions       []BaselineExceptionCount `json:"top_exceptions"`
}

// BaselineExceptionCount is one bounded exception-code frequency.
type BaselineExceptionCount struct {
	Code  int32  `json:"code"`
	Count uint64 `json:"count"`
}

// ComparisonStatus states whether regression analyzers may use the baseline.
type ComparisonStatus string

const (
	ComparisonCompatible   ComparisonStatus = "compatible"
	ComparisonStale        ComparisonStatus = "stale"
	ComparisonIncompatible ComparisonStatus = "incompatible"
)

// ComparisonCounts reports exact-hash and family match coverage. Matched and
// current-only family counts use current rollup membership; baseline-only
// families use the family IDs captured in the artifact.
type ComparisonCounts struct {
	MatchedHashes        int `json:"matched_hashes"`
	CurrentOnlyHashes    int `json:"current_only_hashes"`
	BaselineOnlyHashes   int `json:"baseline_only_hashes"`
	MatchedFamilies      int `json:"matched_families"`
	CurrentOnlyFamilies  int `json:"current_only_families"`
	BaselineOnlyFamilies int `json:"baseline_only_families"`
}

// ComparisonSummary is the machine-readable baseline section added to v2
// reports. Warnings explain why a valid artifact was not usable.
type ComparisonSummary struct {
	SchemaVersion     string           `json:"schema_version"`
	Status            ComparisonStatus `json:"status"`
	BaselineID        string           `json:"baseline_id"`
	BaselineWindow    AnalysisWindow   `json:"baseline_window"`
	BaselineCreatedAt time.Time        `json:"baseline_created_at"`
	Counts            ComparisonCounts `json:"counts"`
	Warnings          []string         `json:"warnings,omitempty"`
}

// BaselineComparison carries the report summary plus the validated exact-hash
// lookup used by regression analyzers. The lookup and usability bit are never
// serialized directly.
type BaselineComparison struct {
	Summary         ComparisonSummary
	baselineByHash  map[uint64]BaselineExactGroup
	currentOnlyHash map[uint64]bool
	usable          bool
}

// Usable reports whether regression analyzers may make comparison claims.
func (c *BaselineComparison) Usable() bool {
	return c != nil && c.usable
}

// BuildBaseline constructs and validates a deterministic exact-hash snapshot.
func BuildBaseline(input AnalysisInput, createdAt time.Time, compatibility BaselineCompatibility) (BaselineSnapshot, error) {
	snapshot := BaselineSnapshot{
		SchemaVersion: BaselineSchemaVersion,
		CreatedAt:     createdAt.UTC(),
		Window:        input.Window,
		Compatibility: compatibility,
		Groups:        []BaselineExactGroup{},
	}

	seen := make(map[uint64]bool)
	for _, family := range input.Families {
		for _, member := range family.Members {
			if member.NormalizedQueryHash == 0 || seen[member.NormalizedQueryHash] {
				return snapshot, fmt.Errorf("baseline contains duplicate or zero normalized query hash %d", member.NormalizedQueryHash)
			}
			seen[member.NormalizedQueryHash] = true
			successful, failed := normalizedOutcomeCounts(member.ExecutionCount, member.SuccessfulCount, member.FailedCount)
			snapshot.Groups = append(snapshot.Groups, BaselineExactGroup{
				NormalizedQueryHash: strconv.FormatUint(member.NormalizedQueryHash, 10),
				FamilyID:            family.FamilyID,
				ExecutionCount:      member.ExecutionCount,
				SuccessfulCount:     successful,
				FailedCount:         failed,
				FailureRate:         roundedFailureRate(successful, failed),
				P95DurationMs:       member.P95DurationMs,
				P99DurationMs:       member.P99DurationMs,
				TopExceptions:       baselineExceptions(member.TopExceptions),
			})
		}
	}
	if len(snapshot.Groups) == 0 {
		return snapshot, fmt.Errorf("cannot save a baseline with no exact query groups")
	}
	sort.Slice(snapshot.Groups, func(i, j int) bool {
		return compareHashStrings(snapshot.Groups[i].NormalizedQueryHash, snapshot.Groups[j].NormalizedQueryHash) < 0
	})
	snapshot.BaselineID = computeBaselineID(snapshot)
	if err := ValidateBaseline(snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// ValidateBaseline validates schema, compatibility, metrics, ordering, and the
// content-derived identity before an artifact can influence findings.
func ValidateBaseline(snapshot BaselineSnapshot) error {
	if snapshot.SchemaVersion != BaselineSchemaVersion {
		return fmt.Errorf("unsupported baseline schema_version %q (want %q)", snapshot.SchemaVersion, BaselineSchemaVersion)
	}
	if snapshot.CreatedAt.IsZero() {
		return fmt.Errorf("baseline created_at is required")
	}
	if snapshot.Window.Start.IsZero() || snapshot.Window.End.IsZero() || !snapshot.Window.Start.Before(snapshot.Window.End) {
		return fmt.Errorf("baseline window must have start before end")
	}
	if snapshot.CreatedAt.Before(snapshot.Window.End) {
		return fmt.Errorf("baseline created_at must not be before window end")
	}
	if err := validateBaselineCompatibility(snapshot.Compatibility); err != nil {
		return err
	}
	if len(snapshot.Groups) == 0 {
		return fmt.Errorf("baseline groups must not be empty")
	}
	seen := make(map[uint64]bool, len(snapshot.Groups))
	var previous string
	for i, group := range snapshot.Groups {
		hash, err := strconv.ParseUint(group.NormalizedQueryHash, 10, 64)
		if err != nil || hash == 0 {
			return fmt.Errorf("baseline groups[%d].normalized_query_hash %q is not a non-zero uint64", i, group.NormalizedQueryHash)
		}
		if seen[hash] {
			return fmt.Errorf("baseline contains duplicate normalized query hash %s", group.NormalizedQueryHash)
		}
		seen[hash] = true
		if previous != "" && compareHashStrings(previous, group.NormalizedQueryHash) >= 0 {
			return fmt.Errorf("baseline groups are not sorted by normalized query hash")
		}
		previous = group.NormalizedQueryHash
		if group.FamilyID == "" {
			return fmt.Errorf("baseline groups[%d].family_id is required", i)
		}
		if group.SuccessfulCount+group.FailedCount != group.ExecutionCount {
			return fmt.Errorf("baseline groups[%d] outcome counts do not equal execution_count", i)
		}
		if !finiteNonNegative(group.P95DurationMs) || !finiteNonNegative(group.P99DurationMs) {
			return fmt.Errorf("baseline groups[%d] duration metrics must be finite and non-negative", i)
		}
		wantRate := roundedFailureRate(group.SuccessfulCount, group.FailedCount)
		if math.Abs(group.FailureRate-wantRate) > 0.0000001 {
			return fmt.Errorf("baseline groups[%d].failure_rate does not match outcome counts", i)
		}
		if err := validateBaselineExceptions(group.TopExceptions); err != nil {
			return fmt.Errorf("baseline groups[%d].top_exceptions: %w", i, err)
		}
	}
	if want := computeBaselineID(snapshot); snapshot.BaselineID != want {
		return fmt.Errorf("baseline_id %q does not match artifact contents", snapshot.BaselineID)
	}
	return nil
}

func validateBaselineCompatibility(compatibility BaselineCompatibility) error {
	if compatibility.FamilyAlgorithmVersion == "" {
		return fmt.Errorf("baseline compatibility family_algorithm_version is required")
	}
	if !finiteNonNegative(compatibility.SimilarityThreshold) || compatibility.SimilarityThreshold <= 0 || compatibility.SimilarityThreshold > 1 {
		return fmt.Errorf("baseline compatibility similarity_threshold must be in (0, 1]")
	}
	if compatibility.FilterFingerprint == "" {
		return fmt.Errorf("baseline compatibility filter_fingerprint is required")
	}
	encodedFingerprint := strings.TrimPrefix(compatibility.FilterFingerprint, "sha256:")
	if encodedFingerprint == compatibility.FilterFingerprint || len(encodedFingerprint) != sha256.Size*2 {
		return fmt.Errorf("baseline compatibility filter_fingerprint must be a sha256 digest")
	}
	if _, err := hex.DecodeString(encodedFingerprint); err != nil {
		return fmt.Errorf("baseline compatibility filter_fingerprint must be a sha256 digest")
	}
	if compatibility.MinExecutions == 0 || compatibility.FamilyLimit <= 0 || compatibility.QueryPreviewLength <= 0 {
		return fmt.Errorf("baseline compatibility analysis limits must be positive")
	}
	if !compatibility.NormalizedQuerySupported || !compatibility.QueryFamilyRollupsSupport {
		return fmt.Errorf("baseline compatibility requires normalized hashes and query-family rollups")
	}
	return nil
}

func validateBaselineExceptions(exceptions []BaselineExceptionCount) error {
	if len(exceptions) > MaxBaselineExceptionCodes {
		return fmt.Errorf("contains %d entries, maximum is %d", len(exceptions), MaxBaselineExceptionCodes)
	}
	seen := make(map[int32]bool, len(exceptions))
	for i, exception := range exceptions {
		if exception.Count == 0 {
			return fmt.Errorf("entry %d has zero count", i)
		}
		if seen[exception.Code] {
			return fmt.Errorf("contains duplicate code %d", exception.Code)
		}
		seen[exception.Code] = true
		if i > 0 {
			previous := exceptions[i-1]
			if previous.Count < exception.Count || (previous.Count == exception.Count && previous.Code >= exception.Code) {
				return fmt.Errorf("entries are not ordered by count descending then code ascending")
			}
		}
	}
	return nil
}

// LoadBaseline reads one strictly decoded, size-bounded baseline artifact.
func LoadBaseline(path string) (BaselineSnapshot, error) {
	var snapshot BaselineSnapshot
	f, err := os.Open(path)
	if err != nil {
		return snapshot, fmt.Errorf("open baseline: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, BaselineMaxBytes+1))
	if err != nil {
		return snapshot, fmt.Errorf("read baseline: %w", err)
	}
	if int64(len(data)) > BaselineMaxBytes {
		return snapshot, fmt.Errorf("baseline exceeds %d-byte limit", BaselineMaxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, fmt.Errorf("decode baseline: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return snapshot, fmt.Errorf("decode baseline: trailing JSON value")
		}
		return snapshot, fmt.Errorf("decode baseline: %w", err)
	}
	if err := ValidateBaseline(snapshot); err != nil {
		return snapshot, fmt.Errorf("validate baseline: %w", err)
	}
	return snapshot, nil
}

// WriteBaselineAtomic writes a 0600 temporary file in the destination
// directory, fsyncs it, atomically renames it over the target, then fsyncs the
// directory so the replacement itself is durable across a crash.
func WriteBaselineAtomic(path string, snapshot BaselineSnapshot) error {
	if err := ValidateBaseline(snapshot); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create baseline temporary file: %w", err)
	}
	tempPath := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = os.Remove(tempPath)
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("set baseline temporary file permissions: %w", err)
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snapshot); err != nil {
		return fmt.Errorf("encode baseline: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync baseline temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close baseline temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace baseline: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open baseline directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync baseline directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close baseline directory after sync: %w", err)
	}
	return nil
}

// BuildBaselineComparison validates compatibility, calculates match coverage,
// and prepares the exact-hash lookup used by regression analyzers.
func BuildBaselineComparison(input AnalysisInput, snapshot BaselineSnapshot, current BaselineCompatibility) BaselineComparison {
	comparison := BaselineComparison{
		Summary: ComparisonSummary{
			SchemaVersion:     ComparisonSchemaVersion,
			Status:            ComparisonCompatible,
			BaselineID:        snapshot.BaselineID,
			BaselineWindow:    snapshot.Window,
			BaselineCreatedAt: snapshot.CreatedAt,
		},
		baselineByHash:  make(map[uint64]BaselineExactGroup, len(snapshot.Groups)),
		currentOnlyHash: make(map[uint64]bool),
	}
	baselineFamilies := make(map[string]bool)
	for _, group := range snapshot.Groups {
		hash, _ := strconv.ParseUint(group.NormalizedQueryHash, 10, 64)
		comparison.baselineByHash[hash] = group
		baselineFamilies[group.FamilyID] = true
	}

	matchedBaselineFamilies := make(map[string]bool)
	currentHashes := make(map[uint64]bool)
	for _, family := range input.Families {
		hashes := family.MemberHashesSorted
		if len(family.Members) > 0 {
			hashes = make([]uint64, 0, len(family.Members))
			for _, member := range family.Members {
				hashes = append(hashes, member.NormalizedQueryHash)
			}
		}
		familyMatched := false
		for _, hash := range hashes {
			currentHashes[hash] = true
			if baseline, ok := comparison.baselineByHash[hash]; ok {
				familyMatched = true
				matchedBaselineFamilies[baseline.FamilyID] = true
			}
		}
		if familyMatched {
			comparison.Summary.Counts.MatchedFamilies++
		} else {
			comparison.Summary.Counts.CurrentOnlyFamilies++
		}
	}
	for hash := range currentHashes {
		if _, ok := comparison.baselineByHash[hash]; ok {
			comparison.Summary.Counts.MatchedHashes++
		} else {
			comparison.Summary.Counts.CurrentOnlyHashes++
			comparison.currentOnlyHash[hash] = true
		}
	}
	comparison.Summary.Counts.BaselineOnlyHashes = len(comparison.baselineByHash) - comparison.Summary.Counts.MatchedHashes
	comparison.Summary.Counts.BaselineOnlyFamilies = len(baselineFamilies) - len(matchedBaselineFamilies)

	mismatches := compatibilityMismatches(snapshot.Compatibility, current)
	if snapshot.Window.End.After(input.Window.Start) {
		mismatches = append(mismatches, "baseline window must end before the current analysis window starts")
	}
	if len(mismatches) > 0 {
		comparison.Summary.Status = ComparisonIncompatible
		comparison.Summary.Warnings = mismatches
		return comparison
	}
	if input.Window.Start.Sub(snapshot.Window.End) > BaselineStaleAfter {
		comparison.Summary.Status = ComparisonStale
		comparison.Summary.Warnings = []string{fmt.Sprintf("baseline window ended more than %s before the current window; regression analyzers skipped", BaselineStaleAfter)}
		return comparison
	}
	comparison.usable = true
	return comparison
}

func compatibilityMismatches(baseline, current BaselineCompatibility) []string {
	var mismatches []string
	if baseline.FamilyAlgorithmVersion != current.FamilyAlgorithmVersion {
		mismatches = append(mismatches, "query-family algorithm version differs from the baseline")
	}
	if baseline.SimilarityThreshold != current.SimilarityThreshold {
		mismatches = append(mismatches, "query-family similarity threshold differs from the baseline")
	}
	if baseline.FilterFingerprint != current.FilterFingerprint {
		mismatches = append(mismatches, "effective query-analysis user filters differ from the baseline")
	}
	if baseline.MinExecutions != current.MinExecutions {
		mismatches = append(mismatches, "min-executions differs from the baseline")
	}
	if baseline.FamilyLimit != current.FamilyLimit {
		mismatches = append(mismatches, "family-limit differs from the baseline")
	}
	if baseline.QueryPreviewLength != current.QueryPreviewLength {
		mismatches = append(mismatches, "query-preview-length differs from the baseline")
	}
	if !current.NormalizedQuerySupported || !current.QueryFamilyRollupsSupport {
		mismatches = append(mismatches, "current ClickHouse capabilities do not support normalized query-family comparison")
	}
	return mismatches
}

func computeBaselineID(snapshot BaselineSnapshot) string {
	snapshot.BaselineID = ""
	data, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(data)
	return "bl_" + hex.EncodeToString(sum[:])[:16]
}

func baselineExceptions(exceptions []model.QueryExceptionCount) []BaselineExceptionCount {
	ordered := append([]model.QueryExceptionCount(nil), exceptions...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Count != ordered[j].Count {
			return ordered[i].Count > ordered[j].Count
		}
		return ordered[i].Code < ordered[j].Code
	})
	if len(ordered) > MaxBaselineExceptionCodes {
		ordered = ordered[:MaxBaselineExceptionCodes]
	}
	out := make([]BaselineExceptionCount, 0, len(ordered))
	for _, exception := range ordered {
		if exception.Count > 0 {
			out = append(out, BaselineExceptionCount{Code: exception.Code, Count: exception.Count})
		}
	}
	return out
}

func normalizedOutcomeCounts(executions, successful, failed uint64) (uint64, uint64) {
	if successful == 0 && failed == 0 && executions > 0 {
		return executions, 0
	}
	return successful, failed
}

func roundedFailureRate(successful, failed uint64) float64 {
	total := successful + failed
	if total == 0 {
		return 0
	}
	return math.Round((float64(failed)/float64(total))*1e8) / 1e8
}

func finiteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func compareHashStrings(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
