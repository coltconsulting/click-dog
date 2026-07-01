package analysis

import (
	"os"
	"strings"
	"testing"
)

// domainLanguageDoc is the naming source of truth, relative to this package.
const domainLanguageDoc = "../../docs/development/domain-language.md"

// TestDomainLanguage_AnalyzersDocumented pins the compiled-in analyzer registry
// to the domain-language doc. Every analyzer is user-facing (its name shows up
// in report JSON, AnalyzerRun output, and the table renderer), so the naming
// rule requires a domain-language entry. Adding a Phase 2 analyzer (latency
// regression, failure spike, hot table) must come with its entry or this fails.
func TestDomainLanguage_AnalyzersDocumented(t *testing.T) {
	// See the note in the top-level domain_language_test.go: docs/development is
	// export-ignored, so it is absent from the public archive tree. Skip there;
	// enforce on internal where the tree exists.
	if _, err := os.Stat("../../docs/development"); os.IsNotExist(err) {
		t.Skip("docs/development is internal-only (export-ignored); domain-language governance runs on the internal repo")
	}
	raw, err := os.ReadFile(domainLanguageDoc)
	if err != nil {
		t.Fatalf("read %s: %v", domainLanguageDoc, err)
	}
	doc := string(raw)

	for _, name := range NewRegistry().AnalyzerNames() {
		if !strings.Contains(doc, name) {
			t.Errorf("analyzer %q is not documented in %s; add a Query Analysis Concepts entry (see the Keeping This In Sync section)", name, domainLanguageDoc)
		}
	}
}
