package testutil

import "testing"

func TestNormalizeRepositoryURL(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/coltconsulting/click-dog.git",
		"http://github.com/coltconsulting/click-dog",
		"git@github.com:coltconsulting/click-dog.git",
		"ssh://git@github.com/coltconsulting/click-dog.git",
	} {
		if got := normalizeRepositoryURL(raw); got != publicRepository {
			t.Errorf("normalizeRepositoryURL(%q) = %q, want %q", raw, got, publicRepository)
		}
	}
}
