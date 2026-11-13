package main

import (
	"os"
	"strings"
	"testing"
)

func TestSpanAttributesDocs_NormalizedQueryContract(t *testing.T) {
	requireInternalDocs(t)

	data, err := os.ReadFile("docs/span-attributes.md")
	if err != nil {
		t.Fatalf("reading span attributes docs: %v", err)
	}
	doc := string(data)

	for _, attr := range []string{
		"query_log.normalized_query_hash",
		"query_log.normalized_query",
		"db.normalized_query_hash",
		"db.normalized_query",
	} {
		if !markdownTableHasAttribute(doc, attr) {
			t.Errorf("span attributes docs missing table row for %q", attr)
		}
	}

	for _, want := range []string{
		"removes literal values",
		"table names, column names, and query shape",
		"do not perform fuzzy or semantic rollups",
		"`monitor.max_query_length`",
		"`exporters.otel[].max_query_length`",
		"`exporters.splunk_hec[].max_query_length`",
		"`0` does not disable these limits",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("span attributes docs missing %q", want)
		}
	}

	// The query-family rollup contract lives with the analyze commands.
	qa, err := os.ReadFile("docs/query-analysis.md")
	if err != nil {
		t.Fatalf("reading query analysis docs: %v", err)
	}
	for _, want := range []string{
		"## How families are derived",
		"A family keeps its member hashes",
		"Families exist only in `analyze` output",
		"the Datadog query dashboard groups by exact hash",
	} {
		if !strings.Contains(strings.Join(strings.Fields(string(qa)), " "), want) {
			t.Errorf("query analysis docs missing %q", want)
		}
	}
}

func markdownTableHasAttribute(doc, attr string) bool {
	return strings.Contains(doc, "| `"+attr+"` |")
}
