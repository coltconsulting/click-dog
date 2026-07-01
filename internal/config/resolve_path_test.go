package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveConfigPath_FindsSystemThenLocal(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "system.yaml")
	local := filepath.Join(dir, "local.yaml")
	for _, p := range []string{sys, local} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// System path wins when both exist.
	if got, err := resolveConfigPath("", sys, local); err != nil || got != sys {
		t.Fatalf("both present: got %q, %v; want %q", got, err, sys)
	}
	// Falls back to the local file when the system path is absent.
	got, err := resolveConfigPath("", filepath.Join(dir, "missing.yaml"), local)
	if err != nil || got != local {
		t.Fatalf("system absent: got %q, %v; want %q", got, err, local)
	}
}

func TestResolveConfigPath_NoneFound(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveConfigPath("", filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml"))
	if err == nil || !strings.Contains(err.Error(), "no config file found") {
		t.Fatalf("want 'no config file found', got %v", err)
	}
}

func TestResolveConfigPath_ExplicitNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveConfigPath(filepath.Join(dir, "missing.yaml"), DefaultConfigPath, DefaultConfigFlag)
	if err == nil || !strings.Contains(err.Error(), "config file not found") {
		t.Fatalf("want 'config file not found', got %v", err)
	}
}

// TestResolveConfigPath_PermissionDenied is the regression test for the prod
// report: /etc/click-dog locked to the service user (drwxr-x---) made `stat`
// fail with EACCES, which the old code reported as "no config file found". A
// permission error must be surfaced as such, for both an explicit -config path
// and the default scan — never collapsed into "not found".
func TestResolveConfigPath_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(locked, "click-dog.yaml")
	if err := os.WriteFile(cfg, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Drop traverse permission so stat on the file inside fails with EACCES —
	// mirrors a directory owned by the click-dog service user.
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // restore so TempDir cleanup can remove it

	// Explicit -config path that can't be accessed.
	_, err := resolveConfigPath(cfg, DefaultConfigPath, DefaultConfigFlag)
	if err == nil || !strings.Contains(err.Error(), "cannot access config at") {
		t.Fatalf("explicit path: want permission error, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "not found") {
		t.Errorf("explicit path: permission error must not say 'not found': %v", err)
	}

	// Default scan landing on a locked system path (no readable fallback).
	_, err = resolveConfigPath("", cfg, filepath.Join(parent, "no-local.yaml"))
	if err == nil || !strings.Contains(err.Error(), "cannot access config at") {
		t.Fatalf("scan: want permission error, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "no config file found") {
		t.Errorf("scan: permission error must not collapse to 'no config file found': %v", err)
	}
}

// TestResolveConfigPath_PermissionDoesNotMaskReadableLocal ensures a locked
// system path doesn't hide a readable local config — the readable one wins.
func TestResolveConfigPath_PermissionDoesNotMaskReadableLocal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	sys := filepath.Join(locked, "click-dog.yaml")
	if err := os.WriteFile(sys, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(parent, "local.yaml")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	got, err := resolveConfigPath("", sys, local)
	if err != nil || got != local {
		t.Fatalf("got %q, %v; want readable local %q", got, err, local)
	}
}

// TestResolveConfigPath_BothCandidatesLocked pins the invariant that when every
// candidate is unreadable, the permission error is surfaced (not "not found"),
// and it's the first (system) candidate's error.
func TestResolveConfigPath_BothCandidatesLocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}
	parent := t.TempDir()
	mk := func(name string) string {
		d := filepath.Join(parent, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, "click-dog.yaml")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
		return f
	}
	sys := mk("sys")
	local := mk("local")

	_, err := resolveConfigPath("", sys, local)
	if err == nil || !strings.Contains(err.Error(), "cannot access config at") {
		t.Fatalf("want permission error, got %v", err)
	}
	if !strings.Contains(err.Error(), sys) {
		t.Errorf("expected the system-path error to win, got %v", err)
	}
}
