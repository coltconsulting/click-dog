package queryfamily

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	// DefaultSimilarityThreshold is intentionally conservative. False
	// negatives are preferable to surprising family merges.
	DefaultSimilarityThreshold = 0.75
	DefaultMaxPreviewLength    = 500
	MaxPreviewLength           = 2000

	shingleSize = 3
	// maxSimilarityToks caps similarity work for very wide normalized queries;
	// tokens beyond this point are ignored for token/shingle scoring.
	maxSimilarityToks       = 200
	tokenSimilarityWeight   = 0.85
	shingleSimilarityWeight = 0.10
	clauseSimilarityWeight  = 0.05
	// Tableless comparisons are allowed only for near-identical shapes
	// because table overlap is the strongest false-positive guard.
	tablelessTokenThreshold = 0.90

	// TopKLimit bounds stored top users, clients, and tables per family.
	TopKLimit = 5
)

// Options configures deterministic query-family rollups.
type Options struct {
	SimilarityThreshold float64
	MaxPreviewLength    int
}

// ClauseStructure captures the major clause skeleton used for explainable
// query-family comparisons.
type ClauseStructure struct {
	Where   bool
	Join    bool
	GroupBy bool
	OrderBy bool
	Limit   bool
}

// Names returns the enabled clause names in stable display order.
func (c ClauseStructure) Names() []string {
	var names []string
	if c.Where {
		names = append(names, "WHERE")
	}
	if c.Join {
		names = append(names, "JOIN")
	}
	if c.GroupBy {
		names = append(names, "GROUP BY")
	}
	if c.OrderBy {
		names = append(names, "ORDER BY")
	}
	if c.Limit {
		names = append(names, "LIMIT")
	}
	return names
}

// Features are deterministic structural signals extracted from a normalized
// query preview. They deliberately avoid literal values and AI-generated names.
type Features struct {
	StatementKind string
	Tables        []string
	Clauses       ClauseStructure
	Tokens        []string
	TokenSet      map[string]struct{}
	Shingles      map[string]struct{}
}

// SimilarityResult explains the structural comparison between two exact
// normalized-query groups.
type SimilarityResult struct {
	Score             float64
	TokenSimilarity   float64
	ShingleSimilarity float64
	ClauseSimilarity  float64
	SharedTables      []string
	Reasons           []string
	BlockedReason     string
}

// Rollup groups exact normalized-query groups into higher-level deterministic
// query families. Duplicate hashes are coalesced defensively before clustering
// so callers cannot accidentally cluster individual executions.
func Rollup(ctx context.Context, groups []model.QueryFamilyExactGroup, opts Options) ([]model.QueryFamilyRollup, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts = normalizeOptions(opts)
	groups = coalesceExactGroups(groups, opts.MaxPreviewLength)
	if len(groups) == 0 {
		return nil, nil
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].ExecutionCount != groups[j].ExecutionCount {
			return groups[i].ExecutionCount > groups[j].ExecutionCount
		}
		return groups[i].NormalizedQueryHash < groups[j].NormalizedQueryHash
	})

	features := make(map[uint64]Features, len(groups))
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		features[group.NormalizedQueryHash] = ExtractFeatures(group.NormalizedQuery)
	}

	// Families are built greedily in execution-count/hash order. The complete-
	// link check keeps each family internally conservative, while the first-fit
	// assignment keeps the bounded offline path simple and deterministic.
	var builders []familyBuilder
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		groupFeatures := features[group.NormalizedQueryHash]
		placed := false
		for i := range builders {
			reasons, ok := completeLinkReasons(group, groupFeatures, builders[i].groups, features, opts.SimilarityThreshold)
			if !ok {
				continue
			}
			builders[i].groups = append(builders[i].groups, group)
			builders[i].reasons = append(builders[i].reasons, reasons...)
			placed = true
			break
		}
		if !placed {
			builders = append(builders, familyBuilder{groups: []model.QueryFamilyExactGroup{group}})
		}
	}

	rollups := make([]model.QueryFamilyRollup, 0, len(builders))
	for _, builder := range builders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rollups = append(rollups, buildRollup(builder, opts.MaxPreviewLength))
	}
	sort.Slice(rollups, func(i, j int) bool {
		if rollups[i].Stats.ExecutionCount != rollups[j].Stats.ExecutionCount {
			return rollups[i].Stats.ExecutionCount > rollups[j].Stats.ExecutionCount
		}
		return rollups[i].FamilyID < rollups[j].FamilyID
	})
	return rollups, nil
}

type familyBuilder struct {
	groups  []model.QueryFamilyExactGroup
	reasons []model.QueryFamilyMergeReason
}

func normalizeOptions(opts Options) Options {
	if opts.SimilarityThreshold <= 0 || opts.SimilarityThreshold > 1 {
		opts.SimilarityThreshold = DefaultSimilarityThreshold
	}
	if opts.MaxPreviewLength <= 0 {
		opts.MaxPreviewLength = DefaultMaxPreviewLength
	}
	if opts.MaxPreviewLength > MaxPreviewLength {
		opts.MaxPreviewLength = MaxPreviewLength
	}
	return opts
}

func completeLinkReasons(
	candidate model.QueryFamilyExactGroup,
	candidateFeatures Features,
	members []model.QueryFamilyExactGroup,
	features map[uint64]Features,
	threshold float64,
) ([]model.QueryFamilyMergeReason, bool) {
	reasons := make([]model.QueryFamilyMergeReason, 0, len(members))
	for _, member := range members {
		result, ok := ShouldMerge(features[member.NormalizedQueryHash], candidateFeatures, threshold)
		if !ok {
			return nil, false
		}
		reasons = append(reasons, model.QueryFamilyMergeReason{
			LeftHash:          member.NormalizedQueryHash,
			RightHash:         candidate.NormalizedQueryHash,
			Similarity:        result.Score,
			TokenSimilarity:   result.TokenSimilarity,
			ShingleSimilarity: result.ShingleSimilarity,
			ClauseSimilarity:  result.ClauseSimilarity,
			Reasons:           append([]string(nil), result.Reasons...),
		})
	}
	return reasons, true
}

func buildRollup(builder familyBuilder, maxPreviewLength int) model.QueryFamilyRollup {
	groups := append([]model.QueryFamilyExactGroup(nil), builder.groups...)
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].ExecutionCount != groups[j].ExecutionCount {
			return groups[i].ExecutionCount > groups[j].ExecutionCount
		}
		return groups[i].NormalizedQueryHash < groups[j].NormalizedQueryHash
	})

	hashes := make([]uint64, 0, len(groups))
	members := make([]model.QueryFamilyMember, 0, len(groups))
	for _, group := range groups {
		hashes = append(hashes, group.NormalizedQueryHash)
		members = append(members, model.QueryFamilyMember{
			NormalizedQueryHash: group.NormalizedQueryHash,
			NormalizedQuery:     TruncatePreview(group.NormalizedQuery, maxPreviewLength),
			ExecutionCount:      group.ExecutionCount,
		})
	}
	// MemberHashesSorted is sorted by hash for deterministic FamilyID generation.
	// Members keep representative order by execution count for display.
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })

	return model.QueryFamilyRollup{
		FamilyID:            familyID(hashes),
		RepresentativeQuery: TruncatePreview(groups[0].NormalizedQuery, maxPreviewLength),
		MemberHashesSorted:  hashes,
		Members:             members,
		Stats:               aggregateStats(groups),
		MergeReasons:        append([]model.QueryFamilyMergeReason(nil), builder.reasons...),
	}
}

func coalesceExactGroups(groups []model.QueryFamilyExactGroup, maxPreviewLength int) []model.QueryFamilyExactGroup {
	byHash := make(map[uint64][]model.QueryFamilyExactGroup)
	for _, group := range groups {
		if group.NormalizedQueryHash == 0 {
			continue
		}
		group.NormalizedQuery = TruncatePreview(group.NormalizedQuery, maxPreviewLength)
		byHash[group.NormalizedQueryHash] = append(byHash[group.NormalizedQueryHash], group)
	}

	coalesced := make([]model.QueryFamilyExactGroup, 0, len(byHash))
	for _, sameHash := range byHash {
		if len(sameHash) == 1 {
			coalesced = append(coalesced, sameHash[0])
			continue
		}
		stats := aggregateStats(sameHash)
		representative := sameHash[0]
		for _, group := range sameHash[1:] {
			if group.ExecutionCount > representative.ExecutionCount ||
				(group.ExecutionCount == representative.ExecutionCount && len(group.NormalizedQuery) < len(representative.NormalizedQuery)) {
				representative = group
			}
		}
		coalesced = append(coalesced, model.QueryFamilyExactGroup{
			NormalizedQueryHash: representative.NormalizedQueryHash,
			NormalizedQuery:     representative.NormalizedQuery,
			ExecutionCount:      stats.ExecutionCount,
			P95DurationMs:       stats.P95DurationMs,
			P99DurationMs:       stats.P99DurationMs,
			MaxMemoryUsage:      stats.MaxMemoryUsage,
			P95ReadRows:         stats.P95ReadRows,
			P95ReadBytes:        stats.P95ReadBytes,
			TopUsers:            stats.TopUsers,
			TopClients:          stats.TopClients,
			TopTables:           stats.TopTables,
			FirstSeen:           stats.FirstSeen,
			LastSeen:            stats.LastSeen,
		})
	}
	return coalesced
}

func aggregateStats(groups []model.QueryFamilyExactGroup) model.QueryFamilyStats {
	var stats model.QueryFamilyStats
	for _, group := range groups {
		stats.ExecutionCount += group.ExecutionCount
		stats.P95DurationMs = math.Max(stats.P95DurationMs, group.P95DurationMs)
		stats.P99DurationMs = math.Max(stats.P99DurationMs, group.P99DurationMs)
		if group.MaxMemoryUsage > stats.MaxMemoryUsage {
			stats.MaxMemoryUsage = group.MaxMemoryUsage
		}
		stats.P95ReadRows = math.Max(stats.P95ReadRows, group.P95ReadRows)
		stats.P95ReadBytes = math.Max(stats.P95ReadBytes, group.P95ReadBytes)
		if stats.FirstSeen.IsZero() || (!group.FirstSeen.IsZero() && group.FirstSeen.Before(stats.FirstSeen)) {
			stats.FirstSeen = group.FirstSeen
		}
		if group.LastSeen.After(stats.LastSeen) {
			stats.LastSeen = group.LastSeen
		}
	}
	stats.TopUsers = weightedTopValues(groups, func(g model.QueryFamilyExactGroup) []string { return g.TopUsers })
	stats.TopClients = weightedTopValues(groups, func(g model.QueryFamilyExactGroup) []string { return g.TopClients })
	stats.TopTables = weightedTopValues(groups, func(g model.QueryFamilyExactGroup) []string { return g.TopTables })
	return stats
}

func weightedTopValues(groups []model.QueryFamilyExactGroup, values func(model.QueryFamilyExactGroup) []string) []string {
	weights := make(map[string]uint64)
	for _, group := range groups {
		seen := make(map[string]struct{})
		for _, value := range values(group) {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			weights[value] += group.ExecutionCount
		}
	}
	type entry struct {
		value  string
		weight uint64
	}
	entries := make([]entry, 0, len(weights))
	for value, weight := range weights {
		entries = append(entries, entry{value: value, weight: weight})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].weight != entries[j].weight {
			return entries[i].weight > entries[j].weight
		}
		return entries[i].value < entries[j].value
	})
	if len(entries) > TopKLimit {
		entries = entries[:TopKLimit]
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.value)
	}
	return out
}

func familyID(hashes []uint64) string {
	h := fnv.New64a()
	var buf [8]byte
	for _, hash := range hashes {
		binary.BigEndian.PutUint64(buf[:], hash)
		_, _ = h.Write(buf[:])
	}
	return fmt.Sprintf("qf_%016x", h.Sum64())
}

// ExtractFeatures derives deterministic comparison features from normalized
// SQL text.
func ExtractFeatures(normalizedQuery string) Features {
	tokens := tokenizeSQL(normalizedQuery)
	kind := statementKind(tokens)
	semantic := semanticTokens(tokens)
	if len(semantic) > maxSimilarityToks {
		semantic = semantic[:maxSimilarityToks]
	}

	features := Features{
		StatementKind: kind,
		Tables:        extractTables(tokens),
		Clauses:       extractClauseStructure(tokens),
		Tokens:        semantic,
		TokenSet:      makeSet(semantic),
		Shingles:      buildShingles(semantic),
	}
	return features
}

// ShouldMerge returns true when the structural similarity is above the
// threshold and hard safety gates pass.
func ShouldMerge(a, b Features, threshold float64) (SimilarityResult, bool) {
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultSimilarityThreshold
	}
	result := Compare(a, b)
	if result.BlockedReason != "" {
		return result, false
	}
	if (len(a.Tables) == 0 || len(b.Tables) == 0) && result.TokenSimilarity < tablelessTokenThreshold {
		result.BlockedReason = "no safely extracted shared table and token similarity below tableless threshold"
		return result, false
	}
	return result, result.Score >= threshold
}

// Compare scores two feature sets. It does not perform the threshold check,
// but it does apply non-negotiable safety blockers such as statement mismatch.
func Compare(a, b Features) SimilarityResult {
	result := SimilarityResult{}
	if a.StatementKind != "" && b.StatementKind != "" && a.StatementKind != b.StatementKind {
		result.BlockedReason = fmt.Sprintf("different statement kinds: %s vs %s", a.StatementKind, b.StatementKind)
		return result
	}

	sharedTables := intersection(a.Tables, b.Tables)
	if len(a.Tables) > 0 && len(b.Tables) > 0 && len(sharedTables) == 0 {
		result.BlockedReason = "no shared referenced tables"
		return result
	}

	result.TokenSimilarity = jaccard(a.TokenSet, b.TokenSet)
	result.ShingleSimilarity = jaccard(a.Shingles, b.Shingles)
	result.ClauseSimilarity = clauseSimilarity(a.Clauses, b.Clauses)
	result.Score = tokenSimilarityWeight*result.TokenSimilarity +
		shingleSimilarityWeight*result.ShingleSimilarity +
		clauseSimilarityWeight*result.ClauseSimilarity
	result.SharedTables = sharedTables
	result.Reasons = comparisonReasons(a, b, result)
	return result
}

func comparisonReasons(a, b Features, result SimilarityResult) []string {
	var reasons []string
	if a.StatementKind != "" && a.StatementKind == b.StatementKind {
		reasons = append(reasons, "same statement kind: "+a.StatementKind)
	}
	if len(result.SharedTables) > 0 {
		reasons = append(reasons, "shared tables: "+strings.Join(result.SharedTables, ", "))
	}
	if names := intersection(a.Clauses.Names(), b.Clauses.Names()); len(names) > 0 {
		reasons = append(reasons, "shared clause structure: "+strings.Join(names, ", "))
	}
	reasons = append(reasons, fmt.Sprintf("token similarity %.2f, shingle similarity %.2f, clause similarity %.2f",
		result.TokenSimilarity, result.ShingleSimilarity, result.ClauseSimilarity))
	return reasons
}

func statementKind(tokens []string) string {
	for _, token := range tokens {
		if token == "" || isPunctuation(token) {
			continue
		}
		return strings.ToUpper(token)
	}
	return ""
}

func extractClauseStructure(tokens []string) ClauseStructure {
	var clauses ClauseStructure
	for i, token := range tokens {
		upper := strings.ToUpper(token)
		switch upper {
		case "WHERE":
			clauses.Where = true
		case "JOIN":
			clauses.Join = true
		case "GROUP":
			if nextUpper(tokens, i) == "BY" {
				clauses.GroupBy = true
			}
		case "ORDER":
			if nextUpper(tokens, i) == "BY" {
				clauses.OrderBy = true
			}
		case "LIMIT":
			clauses.Limit = true
		}
	}
	return clauses
}

// extractTables is deliberately conservative about table positions, but it
// still walks the full token stream. A nested SELECT in a WHERE clause can
// therefore add its table to the feature set; that broadens candidates but the
// shared-table blocker still prevents disjoint-table merges.
func extractTables(tokens []string) []string {
	seen := make(map[string]struct{})
	var tables []string
	for i := 0; i < len(tokens); i++ {
		switch strings.ToUpper(tokens[i]) {
		case "FROM":
			i = collectFromTables(tokens, i+1, seen, &tables)
		case "JOIN", "INTO", "UPDATE":
			if table, ok := tableAfterKeyword(tokens, i+1); ok {
				addTable(table, seen, &tables)
			}
		}
	}
	sort.Strings(tables)
	return tables
}

func collectFromTables(tokens []string, start int, seen map[string]struct{}, tables *[]string) int {
	expectTable := true
	i := start
	for ; i < len(tokens); i++ {
		upper := strings.ToUpper(tokens[i])
		if isClauseBoundary(tokens, i) || upper == "JOIN" {
			// extractTables assigns this value to the outer loop index, and
			// the loop's post statement then increments it. Returning i-1
			// deliberately makes the outer loop process this boundary token.
			return i - 1
		}
		if tokens[i] == "(" {
			i = skipBalanced(tokens, i)
			expectTable = false
			continue
		}
		if tokens[i] == "," {
			expectTable = true
			continue
		}
		if !expectTable {
			continue
		}
		if tableCandidate(tokens, i) {
			addTable(cleanIdentifier(tokens[i]), seen, tables)
			expectTable = false
		}
	}
	return i
}

func tableAfterKeyword(tokens []string, start int) (string, bool) {
	for i := start; i < len(tokens); i++ {
		if tokens[i] == "(" || isClauseBoundary(tokens, i) {
			return "", false
		}
		if tableCandidate(tokens, i) {
			return cleanIdentifier(tokens[i]), true
		}
	}
	return "", false
}

func addTable(table string, seen map[string]struct{}, tables *[]string) {
	if table == "" {
		return
	}
	if _, ok := seen[table]; ok {
		return
	}
	seen[table] = struct{}{}
	*tables = append(*tables, table)
}

func tableCandidate(tokens []string, i int) bool {
	token := tokens[i]
	upper := strings.ToUpper(token)
	if token == "" || isPunctuation(token) || upper == "AS" || upper == "SELECT" || isClauseBoundary(tokens, i) {
		return false
	}
	if i+1 < len(tokens) && tokens[i+1] == "(" {
		return false
	}
	return true
}

func isClauseBoundary(tokens []string, i int) bool {
	upper := strings.ToUpper(tokens[i])
	switch upper {
	case "WHERE", "PREWHERE", "GROUP", "ORDER", "LIMIT", "HAVING", "SETTINGS", "UNION", "FORMAT":
		return true
	}
	return false
}

func nextUpper(tokens []string, i int) string {
	if i+1 >= len(tokens) {
		return ""
	}
	return strings.ToUpper(tokens[i+1])
}

func skipBalanced(tokens []string, start int) int {
	depth := 0
	for i := start; i < len(tokens); i++ {
		switch tokens[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(tokens) - 1
}

// tokenizeSQL expects ClickHouse normalizeQuery output. It handles quoted
// identifiers, comments, and placeholders, but does not try to parse raw SQL
// string literals because literal values should already be replaced with ?.
func tokenizeSQL(query string) []string {
	var tokens []string
	runes := []rune(query)
	for i := 0; i < len(runes); {
		r := runes[i]
		if unicode.IsSpace(r) {
			i++
			continue
		}
		if r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			i += 2
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			i += 2
			for i+1 < len(runes) && (runes[i] != '*' || runes[i+1] != '/') {
				i++
			}
			if i+1 < len(runes) {
				i += 2
			} else {
				i = len(runes)
			}
			continue
		}
		if r == '`' || r == '"' {
			token, next := readQuotedIdentifier(runes, i)
			tokens = append(tokens, token)
			i = next
			continue
		}
		if isIdentifierRune(r) {
			start := i
			for i < len(runes) && (isIdentifierRune(runes[i]) || runes[i] == '.') {
				i++
			}
			tokens = append(tokens, string(runes[start:i]))
			continue
		}
		if r == '?' || r == ',' || r == '(' || r == ')' || r == '*' {
			tokens = append(tokens, string(r))
		}
		i++
	}
	return tokens
}

func semanticTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" || token == "," || token == "(" || token == ")" {
			continue
		}
		out = append(out, strings.ToLower(cleanIdentifier(token)))
	}
	return out
}

func cleanIdentifier(token string) string {
	token = strings.TrimSpace(token)
	token = strings.Trim(token, "`\"")
	return strings.ToLower(token)
}

func isIdentifierRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$'
}

func isPunctuation(token string) bool {
	return token == "," || token == "(" || token == ")" || token == "*"
}

func makeSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token != "" {
			set[token] = struct{}{}
		}
	}
	return set
}

func buildShingles(tokens []string) map[string]struct{} {
	shingles := make(map[string]struct{})
	if len(tokens) == 0 {
		return shingles
	}
	if len(tokens) < shingleSize {
		shingles[strings.Join(tokens, "\x1f")] = struct{}{}
		return shingles
	}
	for i := 0; i <= len(tokens)-shingleSize; i++ {
		shingles[strings.Join(tokens[i:i+shingleSize], "\x1f")] = struct{}{}
	}
	return shingles
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersections := 0
	if len(a) > len(b) {
		a, b = b, a
	}
	for value := range a {
		if _, ok := b[value]; ok {
			intersections++
		}
	}
	union := len(a) + len(b) - intersections
	return float64(intersections) / float64(union)
}

func clauseSimilarity(a, b ClauseStructure) float64 {
	return jaccard(makeSet(a.Names()), makeSet(b.Names()))
}

func intersection(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(a))
	for _, value := range a {
		set[value] = struct{}{}
	}
	var out []string
	for _, value := range b {
		if _, ok := set[value]; ok {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// TruncatePreview collapses whitespace and bounds a normalized query preview.
func TruncatePreview(query string, maxLen int) string {
	query = strings.Join(strings.Fields(query), " ")
	if maxLen <= 0 {
		return query
	}
	runes := []rune(query)
	if len(runes) <= maxLen {
		return query
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func readQuotedIdentifier(runes []rune, i int) (string, int) {
	// normalizeQuery output is not expected to contain escaped quote
	// characters inside quoted identifiers, so this scans to the next quote.
	var parts []string
	for i < len(runes) && (runes[i] == '`' || runes[i] == '"') {
		quote := runes[i]
		i++
		start := i
		for i < len(runes) && runes[i] != quote {
			i++
		}
		if start < i {
			parts = append(parts, string(runes[start:i]))
		}
		if i < len(runes) {
			i++
		}
		if i >= len(runes) || runes[i] != '.' {
			break
		}
		i++
		if i < len(runes) && runes[i] != '`' && runes[i] != '"' {
			start = i
			for i < len(runes) && isIdentifierRune(runes[i]) {
				i++
			}
			if start < i {
				parts = append(parts, string(runes[start:i]))
			}
			break
		}
	}
	return strings.Join(parts, "."), i
}
