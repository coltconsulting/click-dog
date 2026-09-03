package main

import (
	"os"
	"strings"
	"testing"
)

func TestSpanAttributesDocs_NormalizedQueryContract(t *testing.T) {
	requireInternalSource(t, "docs/span-attributes.md")
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
		"Query Family Rollups",
		"not part of the hot live-export path",
		"preserves its member `normalized_query_hash` values",
		"no AI/LLM naming",
		"Dashboard/export limitation",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("span attributes docs missing %q", want)
		}
	}
}

func markdownTableHasAttribute(doc, attr string) bool {
	return strings.Contains(doc, "| `"+attr+"` |")
}
