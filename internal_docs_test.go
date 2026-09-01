package main

import (
	"os"
	"path/filepath"
	"testing"
)

// requireInternalDocs keeps documentation contract tests strict in the
// internal repository while allowing the export-ignore public tree to run its
// Go suite.
func requireInternalDocs(t *testing.T) {
	t.Helper()
	requireInternalTree(t, "docs", "documentation")
}

// requireInternalSite checks the site source boundary independently from docs
// so either export-ignore rule can change without silently skipping the other
// tree's contracts.
func requireInternalSite(t *testing.T) {
	t.Helper()
	requireInternalTree(t, "www", "site")
}

// requireInternalTree checks the root once: when it exists, a missing file
// beneath it remains a real regression and must fail in the calling test.
func requireInternalTree(t *testing.T, root, description string) {
	t.Helper()

	info, err := os.Stat(root)
	if err == nil {
		if !info.IsDir() {
			t.Fatalf("%s exists but is not a directory", root)
		}
		return
	}
	if os.IsNotExist(err) {
		t.Skipf("%s sources are internal-only (export-ignored); contract is enforced in the internal repository", description)
	}
	t.Fatalf("stat %s: %v", root, err)
}

// TestSiteAssets_NoDocsCollision guards the www/ split. MkDocs gives docs_dir
// precedence over custom_dir, so a file present in both docs/assets/ and
// www/assets/ publishes the docs/ copy — which the prod job restores from the
// latest GA tag. A collision therefore silently reverts a landing asset to its
// released version, the exact bug moving those assets to www/ was meant to fix.
func TestSiteAssets_NoDocsCollision(t *testing.T) {
	requireInternalSite(t)

	wwwAssets, err := os.ReadDir("www/assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range wwwAssets {
		if entry.IsDir() {
			continue
		}
		docsCopy := filepath.Join("docs", "assets", entry.Name())
		if _, err := os.Stat(docsCopy); err == nil {
			t.Errorf("%s exists in both www/assets and docs/assets; docs/ wins the "+
				"MkDocs merge, so the www/ copy would never publish", entry.Name())
		}
	}
}
