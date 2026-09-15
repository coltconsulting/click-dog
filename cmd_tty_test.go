package main

import (
	"os"
	"strings"
	"testing"
)

// TestIsTTY_NonTerminalStdin pins the regression that made systemd and cron
// runs look interactive. /dev/null and /dev/zero are character devices, so the
// old os.ModeCharDevice test reported them as terminals: `click-dog init`
// dropped into prompts that EOF'd into defaults (or hung forever on a device
// that never EOFs), and `analyze trace --wizard` half-ran and exited 1 instead
// of the documented exit 2.
//
// The test swaps os.Stdin because isTTY() reads it directly; that is also why
// the probe is a package-level function rather than taking a file argument —
// callers ask about the process's real stdin.
func TestIsTTY_NonTerminalStdin(t *testing.T) {
	devZero := "/dev/zero"
	if _, err := os.Stat(devZero); err != nil {
		devZero = os.DevNull // fall back where /dev/zero is absent
	}

	tests := []struct {
		name string
		open func(t *testing.T) *os.File
	}{
		{
			name: "os.DevNull is a character device but not a terminal",
			open: func(t *testing.T) *os.File {
				f, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatalf("open %s: %v", os.DevNull, err)
				}
				return f
			},
		},
		{
			name: "a character device that never reaches EOF is not a terminal",
			open: func(t *testing.T) *os.File {
				f, err := os.Open(devZero)
				if err != nil {
					t.Fatalf("open %s: %v", devZero, err)
				}
				return f
			},
		},
		{
			name: "a pipe is not a terminal",
			open: func(t *testing.T) *os.File {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatalf("pipe: %v", err)
				}
				t.Cleanup(func() { _ = w.Close() })
				return r
			},
		},
		{
			name: "a regular file is not a terminal",
			open: func(t *testing.T) *os.File {
				p := t.TempDir() + "/stdin"
				if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				f, err := os.Open(p)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return f
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.open(t)
			t.Cleanup(func() { _ = f.Close() })

			orig := os.Stdin
			os.Stdin = f
			t.Cleanup(func() { os.Stdin = orig })

			if isTTY() {
				t.Error("isTTY() = true for a non-terminal stdin, want false")
			}
		})
	}
}

// TestIsTTY_DependentsAreKnown pins the blast radius. isTTY gates three
// interactive prompts and a change to the probe changes all three, including
// the deploy password prompt, which is easy to overlook because it sits in a
// different file from the other two. A new dependent should force a deliberate
// update here rather than inheriting the change silently.
//
// Dependents, not call sites: cmd_analyze_trace.go takes a reference
// (analyzeTraceIsTTY = isTTY) rather than calling it, and is affected all the
// same.
func TestIsTTY_DependentsAreKnown(t *testing.T) {
	callers := map[string]string{
		"cmd_init.go":            "declaration and the init short prompt",
		"cmd_analyze_trace.go":   "analyze trace --wizard gate",
		"cmd_deploy_generate.go": "deploy password prompt",
	}

	found := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if strings.Contains(string(src), "isTTY") {
			found[name] = true
		}
	}

	for file, role := range callers {
		if !found[file] {
			t.Errorf("expected %s to depend on isTTY (%s); did it move?", file, role)
		}
		delete(found, file)
	}
	for file := range found {
		t.Errorf("%s depends on isTTY but is not a known dependent; confirm terminal "+
			"detection is right for it, then add it here", file)
	}
}
