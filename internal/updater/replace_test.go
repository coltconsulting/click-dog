package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplace_HappyPath(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "click-dog")
	newBin := filepath.Join(dir, "click-dog.tmp")

	// Create existing binary
	if err := os.WriteFile(canonical, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	// Create new binary
	if err := os.WriteFile(newBin, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := Replace(canonical, newBin); err != nil {
		t.Fatalf("Replace failed: %v", err)
	}

	// Canonical should have new content
	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Errorf("canonical has %q, want %q", string(data), "new")
	}

	// .prev should have old content
	prev := canonical + ".prev"
	data, err = os.ReadFile(prev)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old" {
		t.Errorf(".prev has %q, want %q", string(data), "old")
	}
}

func TestReplace_FirstInstall(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "click-dog")
	newBin := filepath.Join(dir, "click-dog.tmp")

	// No existing binary — first install
	if err := os.WriteFile(newBin, []byte("first"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := Replace(canonical, newBin); err != nil {
		t.Fatalf("Replace failed: %v", err)
	}

	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Errorf("canonical has %q, want %q", string(data), "first")
	}
}

func TestReplace_NewPathMissing(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "click-dog")
	newBin := filepath.Join(dir, "nonexistent")

	if err := os.WriteFile(canonical, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	err := Replace(canonical, newBin)
	if err == nil {
		t.Fatal("expected error when new binary doesn't exist")
	}

	// Original should be restored from .prev
	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old" {
		t.Errorf("canonical should be restored, has %q, want %q", string(data), "old")
	}
}

func TestFindAsset_Matching(t *testing.T) {
	r := &Release{
		TagName: "v25.04.1",
		Assets: []Asset{
			{Name: "click-dog_25.04.1_linux_amd64.tar.gz", BrowserDownloadURL: "https://example.com/linux_amd64"},
			{Name: "click-dog_25.04.1_linux_arm64.tar.gz", BrowserDownloadURL: "https://example.com/linux_arm64"},
		},
	}

	asset, err := r.FindAsset()
	if err != nil {
		// This might fail on non-linux or non-amd64 — skip gracefully
		t.Skipf("FindAsset: %v (expected on this OS/arch)", err)
	}
	if asset.BrowserDownloadURL == "" {
		t.Error("expected non-empty download URL")
	}
}
