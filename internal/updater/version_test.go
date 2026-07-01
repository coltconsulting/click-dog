package updater

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input   string
		want    CalVer
		wantErr bool
	}{
		{"v25.04.0", CalVer{Year: 25, Month: 4, Index: 0}, false},
		{"25.04.0", CalVer{Year: 25, Month: 4, Index: 0}, false},
		{"v25.10.3", CalVer{Year: 25, Month: 10, Index: 3}, false},
		{"v25.9.0", CalVer{Year: 25, Month: 9, Index: 0}, false},
		{"v0.0.0", CalVer{}, false},
		{"invalid", CalVer{}, true},
		{"v25.04", CalVer{}, true},
		{"v25.04.0.1", CalVer{}, true}, // SplitN produces ["25","04","0.1"]; Atoi("0.1") fails
		{"v25.xx.0", CalVer{}, true},
		{"", CalVer{}, true},
		// Any "-<qualifier>" suffix is a prerelease, kept lower-cased — no
		// allowlist, so dev-build hashes and arbitrary qualifiers all parse.
		{"v26.04.1-12a1754", CalVer{Year: 26, Month: 4, Index: 1, Pre: "12a1754"}, false},
		{"v26.04.3-bbc18f1", CalVer{Year: 26, Month: 4, Index: 3, Pre: "bbc18f1"}, false},
		{"26.04.1-abc123", CalVer{Year: 26, Month: 4, Index: 1, Pre: "abc123"}, false},
		{"v26.04.1-rc1", CalVer{Year: 26, Month: 4, Index: 1, Pre: "rc1"}, false},
		{"v26.05.1-alpha", CalVer{Year: 26, Month: 5, Index: 1, Pre: "alpha"}, false},
		{"v26.05.1-ALPHA", CalVer{Year: 26, Month: 5, Index: 1, Pre: "alpha"}, false},
		{"v26.05.1-beta.2", CalVer{Year: 26, Month: 5, Index: 1, Pre: "beta.2"}, false},
		{"v26.05.1-test", CalVer{Year: 26, Month: 5, Index: 1, Pre: "test"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseVersion(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseVersion(%q) error=%v, wantErr=%v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestCalVer_Less(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v25.04.0", "v25.04.1", true},
		{"v25.04.1", "v25.04.0", false},
		{"v25.04.0", "v25.04.0", false},
		{"v25.04.0", "v25.05.0", true},
		{"v25.9.0", "v25.10.0", true},  // The case that breaks lexicographic
		{"v25.10.0", "v25.9.0", false}, // Reverse
		{"v24.12.0", "v25.01.0", true},
		{"v25.01.0", "v24.12.0", false},
		// Prerelease precedence within the same numeric version.
		{"v26.05.1-alpha", "v26.05.1-beta", true},
		{"v26.05.1-beta", "v26.05.1-rc", true},
		{"v26.05.1-rc", "v26.05.1", true},               // prerelease < stable
		{"v26.05.1", "v26.05.1-alpha", false},           // stable not < prerelease
		{"v26.05.1-alpha", "v26.05.1", true},            // prerelease < stable
		{"v26.05.1-alpha", "v26.05.1-alpha.1", true},    // fewer identifiers < more
		{"v26.05.1-alpha.2", "v26.05.1-alpha.10", true}, // numeric, not lexical
		{"v26.05.1-alpha.10", "v26.05.1-alpha.2", false},
		{"v26.05.1-alpha", "v26.05.1-alpha", false},
		// -test is a prerelease too: below GA, above rc (lexical t > r).
		{"v26.05.1-test", "v26.05.1", true},        // -test moves to GA
		{"v26.05.1", "v26.05.1-test", false},       // GA not downgraded to -test
		{"v26.05.1-rc", "v26.05.1-test", true},     // rc < test
		{"v26.05.1-test", "v26.05.1-alpha", false}, // test not < alpha
		// Numeric version still dominates the prerelease ordering.
		{"v26.04.6", "v26.05.1-alpha", true},
		{"v26.05.1-rc", "v26.06.1-alpha", true},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			a, _ := ParseVersion(tt.a)
			b, _ := ParseVersion(tt.b)
			if got := a.Less(b); got != tt.want {
				t.Errorf("%s.Less(%s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCalVer_String(t *testing.T) {
	tests := []struct {
		v    CalVer
		want string
	}{
		{CalVer{Year: 25, Month: 4, Index: 1}, "v25.04.1"},
		{CalVer{Year: 26, Month: 5, Index: 1, Pre: "alpha"}, "v26.05.1-alpha"},
		{CalVer{Year: 26, Month: 5, Index: 1, Pre: "beta.2"}, "v26.05.1-beta.2"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
