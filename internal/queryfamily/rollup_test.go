package queryfamily

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/model"
)

func TestExtractFeatures(t *testing.T) {
	features := ExtractFeatures(`
		SELECT e.user_id, count()
		FROM analytics.events AS e
		INNER JOIN analytics.accounts AS a ON a.id = e.account_id
		WHERE e.created_at >= ? AND e.status = ?
		GROUP BY e.user_id
		ORDER BY count() DESC
		LIMIT ?
	`)

	if features.StatementKind != "SELECT" {
		t.Fatalf("StatementKind = %q, want SELECT", features.StatementKind)
	}
	for _, table := range []string{"analytics.accounts", "analytics.events"} {
		if !slices.Contains(features.Tables, table) {
			t.Errorf("Tables = %v, missing %q", features.Tables, table)
		}
	}
	if !features.Clauses.Where || !features.Clauses.Join || !features.Clauses.GroupBy || !features.Clauses.OrderBy || !features.Clauses.Limit {
		t.Errorf("Clauses = %+v, want WHERE/JOIN/GROUP BY/ORDER BY/LIMIT", features.Clauses)
	}
	if len(features.Shingles) == 0 {
		t.Fatal("expected token shingles")
	}
	foundSelectShingle := false
	for shingle := range features.Shingles {
		if strings.Contains(shingle, "select") && strings.Contains(shingle, "e.user_id") {
			foundSelectShingle = true
			break
		}
	}
	if !foundSelectShingle {
		t.Errorf("expected a select/user_id shingle, got %v", features.Shingles)
	}
}

func TestExtractFeaturesQuotedIdentifiers(t *testing.T) {
	features := ExtractFeatures("SELECT `user_id` FROM `app`.`users` WHERE `tenant_id` = ?")

	if !slices.Contains(features.Tables, "app.users") {
		t.Fatalf("Tables = %v, want app.users from quoted multipart identifier", features.Tables)
	}
	if _, ok := features.TokenSet["user_id"]; !ok {
		t.Fatalf("TokenSet = %v, want quoted column without quotes", features.TokenSet)
	}
	if _, ok := features.TokenSet["app.users"]; !ok {
		t.Fatalf("TokenSet = %v, want quoted table without quotes", features.TokenSet)
	}
}

func TestExtractFeaturesUpdateTable(t *testing.T) {
	features := ExtractFeatures("UPDATE app.users SET status = ? WHERE id = ?")

	if features.StatementKind != "UPDATE" {
		t.Fatalf("StatementKind = %q, want UPDATE", features.StatementKind)
	}
	if !slices.Contains(features.Tables, "app.users") {
		t.Fatalf("Tables = %v, want app.users", features.Tables)
	}
	if !features.Clauses.Where {
		t.Fatalf("Clauses = %+v, want WHERE", features.Clauses)
	}
}

func TestSimilarityMergesSameShapeWithOptionalPredicateDifference(t *testing.T) {
	base := ExtractFeatures("SELECT id, name FROM app.users WHERE tenant_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?")
	optionalPredicate := ExtractFeatures("SELECT id, name FROM app.users WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ?")

	result, ok := ShouldMerge(base, optionalPredicate, DefaultSimilarityThreshold)
	if !ok {
		t.Fatalf("expected queries to merge, score %.3f, blocker %q, reasons %v", result.Score, result.BlockedReason, result.Reasons)
	}
	if result.Score < DefaultSimilarityThreshold {
		t.Fatalf("score %.3f below threshold", result.Score)
	}
	if !slices.Contains(result.SharedTables, "app.users") {
		t.Errorf("SharedTables = %v, want app.users", result.SharedTables)
	}
}

func TestSimilarityTablelessQueriesRequireVeryHighTokenSimilarity(t *testing.T) {
	same := ExtractFeatures("SELECT count() WHERE id = ?")
	sameAgain := ExtractFeatures("SELECT count() WHERE id = ?")

	result, ok := ShouldMerge(same, sameAgain, DefaultSimilarityThreshold)
	if !ok {
		t.Fatalf("identical tableless queries should merge, score %.3f, blocker %q", result.Score, result.BlockedReason)
	}
	if len(result.SharedTables) != 0 {
		t.Fatalf("SharedTables = %v, want none", result.SharedTables)
	}

	different := ExtractFeatures("SELECT max() WHERE account_id = ? ORDER BY total LIMIT ?")
	result, ok = ShouldMerge(same, different, DefaultSimilarityThreshold)
	if ok {
		t.Fatalf("different tableless queries merged with score %.3f", result.Score)
	}
	if result.BlockedReason != "no safely extracted shared table and token similarity below tableless threshold" {
		t.Fatalf("BlockedReason = %q, want tableless threshold blocker", result.BlockedReason)
	}
}

func TestSimilarityDoesNotMergeUnrelatedTables(t *testing.T) {
	events := ExtractFeatures("SELECT id, name FROM app.events WHERE tenant_id = ? LIMIT ?")
	users := ExtractFeatures("SELECT id, name FROM app.users WHERE tenant_id = ? LIMIT ?")

	result, ok := ShouldMerge(events, users, DefaultSimilarityThreshold)
	if ok {
		t.Fatalf("unrelated tables merged with score %.3f", result.Score)
	}
	if result.BlockedReason != "no shared referenced tables" {
		t.Fatalf("BlockedReason = %q, want no shared referenced tables", result.BlockedReason)
	}
}

func TestSimilarityDoesNotMergeDifferentStatementKinds(t *testing.T) {
	selectQuery := ExtractFeatures("SELECT id FROM app.users WHERE id = ?")
	insertQuery := ExtractFeatures("INSERT INTO app.users (id, name) VALUES (?, ?)")

	result, ok := ShouldMerge(selectQuery, insertQuery, DefaultSimilarityThreshold)
	if ok {
		t.Fatalf("different statement kinds merged with score %.3f", result.Score)
	}
	if !strings.Contains(result.BlockedReason, "different statement kinds") {
		t.Fatalf("BlockedReason = %q, want different statement kinds", result.BlockedReason)
	}
}

func TestSimilarityRecognizesAlreadyNormalizedLiteralDifferences(t *testing.T) {
	left := ExtractFeatures("SELECT id FROM app.users WHERE id = ?")
	right := ExtractFeatures("SELECT id FROM app.users WHERE id = ?")

	result, ok := ShouldMerge(left, right, DefaultSimilarityThreshold)
	if !ok {
		t.Fatalf("identical normalized queries should merge, score %.3f, blocker %q", result.Score, result.BlockedReason)
	}
	if result.TokenSimilarity != 1 {
		t.Errorf("TokenSimilarity = %.3f, want 1", result.TokenSimilarity)
	}
}

func TestRollupCoalescedRepresentativeUsesHighestExecutionCount(t *testing.T) {
	rollups, err := Rollup(context.Background(), []model.QueryFamilyExactGroup{
		{
			NormalizedQueryHash: 500,
			NormalizedQuery:     "SELECT id FROM app.users WHERE id = ?",
			ExecutionCount:      2,
			TopTables:           []string{"app.users"},
		},
		{
			NormalizedQueryHash: 500,
			NormalizedQuery:     "SELECT id, email FROM app.users WHERE id = ?",
			ExecutionCount:      9,
			TopTables:           []string{"app.users"},
		},
	}, Options{})
	if err != nil {
		t.Fatalf("Rollup failed: %v", err)
	}

	if len(rollups) != 1 {
		t.Fatalf("rollups length = %d, want 1", len(rollups))
	}
	if rollups[0].RepresentativeQuery != "SELECT id, email FROM app.users WHERE id = ?" {
		t.Fatalf("RepresentativeQuery = %q, want higher-count duplicate representative", rollups[0].RepresentativeQuery)
	}
	if rollups[0].Stats.ExecutionCount != 11 {
		t.Fatalf("ExecutionCount = %d, want coalesced sum 11", rollups[0].Stats.ExecutionCount)
	}
	if len(rollups[0].MergeReasons) != 0 {
		t.Fatalf("MergeReasons = %+v, want none for singleton family", rollups[0].MergeReasons)
	}
}

func TestRollupCoalescesExactGroupsBeforeClustering(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	groups := []model.QueryFamilyExactGroup{
		{
			NormalizedQueryHash: 100,
			NormalizedQuery:     "SELECT id, name FROM app.users WHERE tenant_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?",
			ExecutionCount:      7,
			P95DurationMs:       120,
			P99DurationMs:       180,
			MaxMemoryUsage:      1024,
			P95ReadRows:         1000,
			P95ReadBytes:        8000,
			TopUsers:            []string{"api"},
			TopClients:          []string{"clickhouse-go"},
			TopTables:           []string{"app.users"},
			FirstSeen:           now.Add(-2 * time.Hour),
			LastSeen:            now.Add(-time.Hour),
		},
		{
			NormalizedQueryHash: 100,
			NormalizedQuery:     "SELECT id, name FROM app.users WHERE tenant_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?",
			ExecutionCount:      5,
			P95DurationMs:       150,
			P99DurationMs:       220,
			MaxMemoryUsage:      2048,
			P95ReadRows:         1200,
			P95ReadBytes:        9000,
			TopUsers:            []string{"worker"},
			TopClients:          []string{"clickhouse-client"},
			TopTables:           []string{"app.users"},
			FirstSeen:           now.Add(-3 * time.Hour),
			LastSeen:            now,
		},
		{
			NormalizedQueryHash: 200,
			NormalizedQuery:     "SELECT id, name FROM app.users WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ?",
			ExecutionCount:      11,
			P95DurationMs:       90,
			P99DurationMs:       140,
			MaxMemoryUsage:      512,
			P95ReadRows:         800,
			P95ReadBytes:        6000,
			TopUsers:            []string{"api"},
			TopClients:          []string{"clickhouse-go"},
			TopTables:           []string{"app.users"},
			FirstSeen:           now.Add(-90 * time.Minute),
			LastSeen:            now.Add(-30 * time.Minute),
		},
		{
			NormalizedQueryHash: 300,
			NormalizedQuery:     "SELECT count() FROM app.orders WHERE account_id = ?",
			ExecutionCount:      4,
			P95DurationMs:       40,
			P99DurationMs:       60,
			MaxMemoryUsage:      256,
			TopUsers:            []string{"billing"},
			TopClients:          []string{"clickhouse-go"},
			TopTables:           []string{"app.orders"},
			FirstSeen:           now.Add(-4 * time.Hour),
			LastSeen:            now.Add(-3 * time.Hour),
		},
	}

	rollups, err := Rollup(context.Background(), groups, Options{})
	if err != nil {
		t.Fatalf("Rollup failed: %v", err)
	}
	if len(rollups) != 2 {
		t.Fatalf("rollups length = %d, want 2: %+v", len(rollups), rollups)
	}

	family := rollups[0]
	if got, want := family.MemberHashesSorted, []uint64{100, 200}; !slices.Equal(got, want) {
		t.Fatalf("MemberHashesSorted = %v, want %v", got, want)
	}
	if family.Stats.ExecutionCount != 23 {
		t.Errorf("ExecutionCount = %d, want coalesced sum 23", family.Stats.ExecutionCount)
	}
	if family.Stats.P95DurationMs != 150 {
		t.Errorf("P95DurationMs = %.1f, want conservative max 150", family.Stats.P95DurationMs)
	}
	if family.Stats.MaxMemoryUsage != 2048 {
		t.Errorf("MaxMemoryUsage = %d, want 2048", family.Stats.MaxMemoryUsage)
	}
	if family.RepresentativeQuery != "SELECT id, name FROM app.users WHERE tenant_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?" {
		t.Errorf("RepresentativeQuery = %q", family.RepresentativeQuery)
	}
	if len(family.MergeReasons) == 0 {
		t.Fatal("expected merge explanations")
	}
	if !strings.Contains(strings.Join(family.MergeReasons[0].Reasons, " "), "shared tables: app.users") {
		t.Errorf("merge reasons = %v, want shared table explanation", family.MergeReasons[0].Reasons)
	}
	for _, value := range []string{"api", "worker"} {
		if !slices.Contains(family.Stats.TopUsers, value) {
			t.Errorf("TopUsers = %v, missing %q", family.Stats.TopUsers, value)
		}
	}
}

func TestRollupBoundsRepresentativePreview(t *testing.T) {
	rollups, err := Rollup(context.Background(), []model.QueryFamilyExactGroup{{
		NormalizedQueryHash: 42,
		NormalizedQuery:     "SELECT " + strings.Repeat("very_long_column, ", 20) + "id FROM app.users WHERE id = ?",
		ExecutionCount:      1,
		TopTables:           []string{"app.users"},
	}}, Options{MaxPreviewLength: 40})
	if err != nil {
		t.Fatalf("Rollup failed: %v", err)
	}

	if len(rollups) != 1 {
		t.Fatalf("rollups length = %d, want 1", len(rollups))
	}
	if got := len([]rune(rollups[0].RepresentativeQuery)); got > 40 {
		t.Fatalf("RepresentativeQuery length = %d, want <= 40: %q", got, rollups[0].RepresentativeQuery)
	}
	if !strings.HasSuffix(rollups[0].RepresentativeQuery, "...") {
		t.Fatalf("RepresentativeQuery = %q, want ellipsis", rollups[0].RepresentativeQuery)
	}
}

func TestTruncatePreviewTinyLimit(t *testing.T) {
	if got := TruncatePreview("SELECT count() FROM app.users", 2); got != "SE" {
		t.Fatalf("TruncatePreview tiny limit = %q, want SE", got)
	}
}

func TestTokenizeSQLUnterminatedBlockComment(t *testing.T) {
	tokens := tokenizeSQL("SELECT /* unfinished comment")
	if got, want := strings.Join(tokens, ","), "SELECT"; got != want {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
}

func TestRollupHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Rollup(ctx, []model.QueryFamilyExactGroup{{
		NormalizedQueryHash: 1,
		NormalizedQuery:     "SELECT id FROM app.users WHERE id = ?",
		ExecutionCount:      1,
	}}, Options{})
	if err != context.Canceled {
		t.Fatalf("Rollup error = %v, want context.Canceled", err)
	}
}

func BenchmarkRollup(b *testing.B) {
	for _, size := range []int{500, 1000} {
		b.Run(strconv.Itoa(size)+"_groups", func(b *testing.B) {
			groups := benchmarkGroups(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Rollup(context.Background(), groups, Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkGroups(n int) []model.QueryFamilyExactGroup {
	groups := make([]model.QueryFamilyExactGroup, 0, n)
	for i := range n {
		var query strings.Builder
		query.WriteString("SELECT metric_")
		query.WriteString(strconv.Itoa(i))
		for j := range 120 {
			query.WriteString(", dimension_")
			query.WriteString(strconv.Itoa(i))
			query.WriteString("_")
			query.WriteString(strconv.Itoa(j))
		}
		query.WriteString(" WHERE tenant_")
		query.WriteString(strconv.Itoa(i))
		query.WriteString(" = ?")
		for j := range 80 {
			query.WriteString(" AND predicate_")
			query.WriteString(strconv.Itoa(i))
			query.WriteString("_")
			query.WriteString(strconv.Itoa(j))
			query.WriteString(" = ?")
		}
		groups = append(groups, model.QueryFamilyExactGroup{
			NormalizedQueryHash: uint64(i + 1),
			NormalizedQuery:     query.String(),
			ExecutionCount:      uint64(n - i),
		})
	}
	return groups
}
