// Package testutil contains repository-aware helpers shared by tests in
// different packages.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const publicRepository = "github.com/coltconsulting/click-dog"

// IsPublicSourceTree identifies either a source archive (which has no .git
// metadata) or a checkout whose origin is the public click-dog repository.
// Tests use this affirmative check before treating missing export-ignored
// documentation sources as expected.
func IsPublicSourceTree(root string) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("stat .git: %w", err)
	}

	out, err := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return false, fmt.Errorf("resolve origin URL: %w", err)
	}
	return normalizeRepositoryURL(string(out)) == publicRepository, nil
}

func normalizeRepositoryURL(raw string) string {
	repo := strings.TrimSpace(raw)
	repo = strings.TrimPrefix(repo, "https://")
	repo = strings.TrimPrefix(repo, "http://")
	repo = strings.TrimPrefix(repo, "ssh://git@")
	if strings.HasPrefix(repo, "git@") {
		repo = strings.TrimPrefix(repo, "git@")
		repo = strings.Replace(repo, ":", "/", 1)
	}
	return strings.TrimSuffix(repo, ".git")
}
