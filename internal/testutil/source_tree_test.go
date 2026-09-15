package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPublicSourceTree_SourceArchive(t *testing.T) {
	public, err := IsPublicSourceTree(t.TempDir())
	if err != nil {
		t.Fatalf("IsPublicSourceTree: %v", err)
	}
	if !public {
		t.Fatal("a source archive without .git metadata should be treated as public")
	}
}

func TestIsPublicSourceTree_PublicForkCheckout(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	public, err := IsPublicSourceTree(root)
	if err != nil {
		t.Fatalf("IsPublicSourceTree: %v", err)
	}
	if !public {
		t.Fatal("a public fork checkout without internal markers should be treated as public")
	}
}

func TestIsPublicSourceTree_InternalMarkers(t *testing.T) {
	for _, marker := range internalSourceMarkers {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, marker)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("internal"), 0600); err != nil {
				t.Fatal(err)
			}

			public, err := IsPublicSourceTree(root)
			if err != nil {
				t.Fatalf("IsPublicSourceTree: %v", err)
			}
			if public {
				t.Fatalf("tree containing %s should be treated as internal", marker)
			}
		})
	}
}
