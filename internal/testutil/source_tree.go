// Package testutil contains repository-aware helpers shared by tests in
// different packages.
package testutil

import (
	"fmt"
	"os"
	"path/filepath"
)

var internalSourceMarkers = []string{
	"Makefile.internal",
	"scripts/check-public.sh",
}

// IsPublicSourceTree distinguishes the published source tree (including
// contributor forks) from the internal development tree using files that are
// deliberately export-ignored. Tests use this check before treating missing
// documentation and site sources as expected.
func IsPublicSourceTree(root string) (bool, error) {
	for _, marker := range internalSourceMarkers {
		if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat internal source marker %s: %w", marker, err)
		}
	}
	return true, nil
}
