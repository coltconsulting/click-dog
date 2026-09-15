package main

import (
	"os"
	"strings"
	"testing"
)

// TestDomainLanguage_SubcommandsDocumented pins the CLI subcommand registry to
// the domain-language doc. Every top-level verb is a user-facing surface, so the
// naming rule requires an Operator Commands entry. Adding a subcommand must come
// with its entry or this fails. See docs/development/domain-language.md, section
// "Keeping This In Sync".
func TestDomainLanguage_SubcommandsDocumented(t *testing.T) {
	const docPath = "docs/development/domain-language.md"
	requireInternalDocs(t)

	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(raw)

	for name := range subcommands {
		if !strings.Contains(doc, name) {
			t.Errorf("subcommand %q is not documented in %s; add an Operator Commands entry", name, docPath)
		}
	}
}
