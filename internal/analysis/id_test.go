package analysis

import (
	"strings"
	"testing"
	"time"
)

func TestFindingID_StableAcrossHashOrder(t *testing.T) {
	start := time.Date(2026, 6, 12, 1, 0, 0, 0, time.UTC)

	a := FindingID("resource_hog", "family", "qf_0011223344556677", start, []uint64{200, 100, 300})
	b := FindingID("resource_hog", "family", "qf_0011223344556677", start, []uint64{300, 200, 100})
	if a != b {
		t.Errorf("hash order changed ID: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "resource_hog:family:") {
		t.Errorf("ID = %q, want resource_hog:family: prefix", a)
	}
	suffix := strings.TrimPrefix(a, "resource_hog:family:")
	if len(suffix) != 12 {
		t.Errorf("hash suffix %q length = %d, want 12", suffix, len(suffix))
	}
}

func TestFindingID_ComponentsChangeID(t *testing.T) {
	start := time.Date(2026, 6, 12, 1, 0, 0, 0, time.UTC)
	base := FindingID("resource_hog", "family", "qf_1", start, []uint64{100})

	tests := []struct {
		name string
		id   string
	}{
		{"different analyzer", FindingID("skew", "family", "qf_1", start, []uint64{100})},
		{"different scope", FindingID("resource_hog", "user", "qf_1", start, []uint64{100})},
		{"different subject", FindingID("resource_hog", "family", "qf_2", start, []uint64{100})},
		{"different window", FindingID("resource_hog", "family", "qf_1", start.Add(time.Hour), []uint64{100})},
		{"different hashes", FindingID("resource_hog", "family", "qf_1", start, []uint64{101})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.id == base {
				t.Errorf("ID did not change: %q", tt.id)
			}
		})
	}
}

func TestFindingID_CoverageSubjectsStable(t *testing.T) {
	start := time.Date(2026, 6, 12, 1, 0, 0, 0, time.UTC)

	// The Phase 1 coverage subjects must produce distinct, reproducible IDs.
	subjects := []string{
		"normalized_query_hash_unsupported",
		"rollups_unsupported",
		"no_query_families",
		"span_sample",
		"query_id_ratio",
	}
	seen := make(map[string]string, len(subjects))
	for _, scope := range subjects {
		id := FindingID("coverage", scope, "coverage:"+scope, start, nil)
		again := FindingID("coverage", scope, "coverage:"+scope, start, nil)
		if id != again {
			t.Errorf("scope %s: ID not reproducible: %q vs %q", scope, id, again)
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("scope %s collides with %s on ID %q", scope, prev, id)
		}
		seen[id] = scope
	}
}

func TestSortHashStrings_NumericOrder(t *testing.T) {
	hashes := []string{"9", "100", "20", "3"}
	sortHashStrings(hashes)
	want := []string{"3", "9", "20", "100"}
	for i := range want {
		if hashes[i] != want[i] {
			t.Fatalf("sortHashStrings = %v, want %v", hashes, want)
		}
	}
}

func TestHashStrings_SortedDecimal(t *testing.T) {
	got := hashStrings([]uint64{300, 100, 200})
	want := []string{"100", "200", "300"}
	if len(got) != len(want) {
		t.Fatalf("hashStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hashStrings = %v, want %v", got, want)
		}
	}
	if hashStrings(nil) != nil {
		t.Error("hashStrings(nil) should be nil")
	}
}
