package main

import (
	"os"
	"testing"

	"github.com/coltconsulting/click-dog/internal/testutil"
)

// internalSourcesPresent reports whether this checkout contains any of the
// documentation-site/spec sources that are deliberately removed from the
// public source archive. If one marker is present, missing sibling files are
// treated as real drift rather than silently skipped.
func internalSourcesPresent(t *testing.T) bool {
	t.Helper()
	for _, path := range []string{"docs", "overrides", "openspec", "mkdocs.yml"} {
		if _, err := os.Stat(path); err == nil {
			return true
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat internal source marker %s: %v", path, err)
		}
	}
	return false
}

// requireInternalSource skips a contract only in the public source archive,
// where every internal documentation source is absent by design. In an
// internal checkout, a missing expected file remains a hard failure.
func requireInternalSource(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		return
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat internal source %s: %v", path, err)
	}
	if internalSourcesPresent(t) {
		t.Fatalf("internal source %s is missing", path)
	}
	if !publicSourceTree(t) {
		t.Fatalf("internal source %s is missing outside the public source tree", path)
	}
	t.Skipf("%s is not included in the public source archive", path)
}

func publicSourceTree(t *testing.T) bool {
	t.Helper()
	public, err := testutil.IsPublicSourceTree(".")
	if err != nil {
		t.Fatalf("identify public source tree: %v", err)
	}
	return public
}
