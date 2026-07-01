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
	// docs/development is internal-only (export-ignored), so the whole tree is
	// absent from the public archive. Skip there — this governance check runs on
	// the internal repo, where the tree exists. A missing file while the tree IS
	// present is a real regression and still fails below.
	if _, err := os.Stat("docs/development"); os.IsNotExist(err) {
		t.Skip("docs/development is internal-only (export-ignored); domain-language governance runs on the internal repo")
	}
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
