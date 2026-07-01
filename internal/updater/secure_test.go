package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSecureBinDir(t *testing.T) {
	tests := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"root-owned 0755", 0o755, false},
		{"owner-only 0700", 0o700, false},
		{"group-writable 0775", 0o775, true},
		{"other-writable 0707", 0o707, true},
		{"world-writable 0777", 0o777, true},
		{"sticky world-writable 01777", 0o777 | os.ModeSticky, false},
		{"sticky group-writable 01775", 0o775 | os.ModeSticky, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bin")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tt.mode); err != nil {
				t.Fatal(err)
			}

			err := EnsureSecureBinDir(dir)
			if tt.wantErr && err == nil {
				t.Errorf("mode %#o: expected error, got nil", tt.mode)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("mode %#o: unexpected error: %v", tt.mode, err)
			}
		})
	}
}

func TestEnsureSecureBinDir_Missing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := EnsureSecureBinDir(dir); err == nil {
		t.Fatal("expected error for missing directory")
	}
}
