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
	requireInternalDocs(t)

	raw, err := os.ReadFile(domainLanguageDoc)
	if err != nil {
		t.Fatalf("read %s: %v", domainLanguageDoc, err)
	}
	doc := string(raw)

	for _, name := range NewRegistryWithRegression().AnalyzerNames() {
		if !strings.Contains(doc, name) {
			t.Errorf("analyzer %q is not documented in %s; add a Query Analysis Concepts entry (see the Keeping This In Sync section)", name, domainLanguageDoc)
		}
	}
}
