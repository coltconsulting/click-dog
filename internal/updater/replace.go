package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SmokeTest runs the binary at path with -version and returns the version
// string. Returns an error if the binary exits non-zero or doesn't produce
// parseable output.
func SmokeTest(binaryPath string) (string, error) {
	out, err := exec.Command(binaryPath, "-version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("smoke test failed: %w (output: %s)", err, string(out))
	}

	// Output format: "click-dog vYY.MM.idx"
	version := strings.TrimSpace(string(out))
	parts := strings.Fields(version)
	if len(parts) >= 2 {
		return parts[1], nil
	}
	return version, nil
}

// Replace atomically replaces the binary at canonicalPath with the new binary
// at newPath. It preserves the old binary as canonicalPath.prev for rollback.
//
// Note: .prev is silently overwritten if it already exists from a prior update.
// This makes rollback single-hop only (can revert to the immediately previous
// version, not further back). This is intentional — multi-hop rollback adds
// complexity with little benefit since operators can always install a specific
// version via the installer's -b flag.
//
// Sequence:
//  1. Rename old -> old.prev  (rollback point)
//  2. Rename new -> canonical (atomic on same filesystem)
//  3. If step 2 fails, restore old.prev -> canonical
func Replace(canonicalPath, newPath string) error {
	prevPath := canonicalPath + ".prev"
	dir := filepath.Dir(canonicalPath)

	// Check write permission to directory
	if err := checkWritable(dir); err != nil {
		return fmt.Errorf("directory %s is not writable: %w", dir, err)
	}

	// Step 1: Back up current binary (ok if it doesn't exist — first install).
	// Capture its mode first so the replacement keeps the operator's chosen
	// permissions instead of inheriting the 0700 used during extraction — a
	// binary deliberately set to e.g. 0755 (world-executable) should stay that
	// way rather than being locked down to owner-only.
	if info, err := os.Stat(canonicalPath); err == nil {
		if chErr := os.Chmod(newPath, info.Mode().Perm()); chErr != nil {
			return fmt.Errorf("preserving mode %v on %s: %w", info.Mode().Perm(), newPath, chErr)
		}
		if err := os.Rename(canonicalPath, prevPath); err != nil {
			return fmt.Errorf("backing up %s to %s: %w", canonicalPath, prevPath, err)
		}
	}

	// Step 2: Atomic rename new -> canonical
	if err := os.Rename(newPath, canonicalPath); err != nil {
		// Restore from .prev
		if restoreErr := os.Rename(prevPath, canonicalPath); restoreErr != nil {
			return fmt.Errorf("rename failed (%v) AND restore failed (%v) — manual intervention required", err, restoreErr)
		}
		return fmt.Errorf("rename %s -> %s failed (restored from .prev): %w", newPath, canonicalPath, err)
	}

	return nil
}

// checkWritable tests if the directory is writable by creating and removing a temp file.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".click-dog-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}
