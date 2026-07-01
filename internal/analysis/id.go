package analysis

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FindingID builds a stable finding ID of the shape
// <analyzer>:<scope>:<hash12>, where hash12 is the first 12 hex characters of
// SHA-256 over:
//
//	analysis.finding.v1|analyzer|scope|subject_id|window_start_utc|sorted_normalized_query_hashes
//
// For family findings subjectID is the family ID; non-family coverage
// findings use stable subjects such as "coverage:query_id_ratio". hashes are
// sorted numerically before hashing, so member order never changes the ID.
func FindingID(analyzer, scope, subjectID string, windowStart time.Time, hashes []uint64) string {
	sorted := append([]uint64(nil), hashes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	parts := make([]string, len(sorted))
	for i, h := range sorted {
		parts[i] = strconv.FormatUint(h, 10)
	}

	input := strings.Join([]string{
		FindingSchemaVersion,
		analyzer,
		scope,
		subjectID,
		windowStart.UTC().Format(time.RFC3339),
		strings.Join(parts, ","),
	}, "|")

	sum := sha256.Sum256([]byte(input))
	return analyzer + ":" + scope + ":" + hex.EncodeToString(sum[:])[:12]
}

// hashStrings converts sorted member hashes to the decimal-string form used
// in Finding.NormalizedQueryHashes, preserving numeric order.
func hashStrings(hashes []uint64) []string {
	if len(hashes) == 0 {
		return nil
	}
	sorted := append([]uint64(nil), hashes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := make([]string, len(sorted))
	for i, h := range sorted {
		out[i] = strconv.FormatUint(h, 10)
	}
	return out
}

// sortHashStrings sorts decimal hash strings numerically (shorter strings are
// smaller; equal lengths compare lexically). Used by the registry to
// normalize NormalizedQueryHashes regardless of how an analyzer built them.
func sortHashStrings(hashes []string) {
	sort.Slice(hashes, func(i, j int) bool {
		if len(hashes[i]) != len(hashes[j]) {
			return len(hashes[i]) < len(hashes[j])
		}
		return hashes[i] < hashes[j]
	})
}
