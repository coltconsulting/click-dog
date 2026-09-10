package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

func TestRewriteBackfillArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantArgv []string // nil means the rewrite must be refused
		wantCode int      // only checked when wantArgv is nil
		wantErr  string   // substring expected on stderr
	}{
		{
			name:     "start and end map onto legacy mode flags",
			args:     []string{"--start", "2024-01-01T00:00:00Z", "--end", "2024-01-02T00:00:00Z"},
			wantArgv: []string{"-backfill-end", "2024-01-02T00:00:00Z", "-backfill-start", "2024-01-01T00:00:00Z"},
		},
		{
			name: "config and dry-run pass through",
			args: []string{"--start", "a", "--end", "b", "--config", "x.yaml", "--dry-run"},
			wantArgv: []string{
				"-config", "x.yaml", "-dry-run", "-backfill-end", "b", "-backfill-start", "a",
			},
		},
		{
			name:     "missing end is refused",
			args:     []string{"--start", "2024-01-01T00:00:00Z"},
			wantCode: 2,
			wantErr:  "both --start and --end are required",
		},
		{
			name:     "positional arg is refused",
			args:     []string{"--start", "a", "--end", "b", "extra"},
			wantCode: 2,
			wantErr:  `unexpected argument "extra"`,
		},
		{
			name:     "unknown flag is refused",
			args:     []string{"--start", "a", "--end", "b", "--bogus"},
			wantCode: 2,
			wantErr:  "-bogus",
		},
		{
			name:     "help returns 0 with usage",
			args:     []string{"-h"},
			wantCode: 0,
			wantErr:  "click-dog backfill —",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errOut bytes.Buffer
			argv, code := rewriteBackfillArgs(tt.args, &errOut)

			if tt.wantArgv == nil {
				if argv != nil {
					t.Fatalf("argv = %v, want refusal (nil)", argv)
				}
				if code != tt.wantCode {
					t.Errorf("exit = %d, want %d", code, tt.wantCode)
				}
			} else {
				if argv == nil {
					t.Fatalf("rewrite refused (code %d): %s", code, errOut.String())
				}
				// fs.Visit iterates flags in lexical order, so the translated
				// argv order is deterministic and asserted exactly.
				if !slices.Equal(argv, tt.wantArgv) {
					t.Errorf("argv = %v, want %v", argv, tt.wantArgv)
				}
			}
			if tt.wantErr != "" && !strings.Contains(errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want substring %q", errOut.String(), tt.wantErr)
			}
		})
	}
}

// TestDispatch_BackfillVerbFallsThroughRewritten asserts the backfill verb is
// a fall-through with translated argv: main() must continue into flag parsing
// using the rewritten args, keeping the pipeline wiring untouched.
func TestDispatch_BackfillVerbFallsThroughRewritten(t *testing.T) {
	var out, errOut bytes.Buffer
	code, fellThrough, argv := dispatch(
		[]string{"click-dog", "backfill", "--start", "a", "--end", "b"}, &out, &errOut)

	if !fellThrough {
		t.Fatalf("backfill verb must fall through to flag parsing (code %d, stderr %s)", code, errOut.String())
	}
	want := []string{"click-dog", "-backfill-end", "b", "-backfill-start", "a"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if out.Len() > 0 || errOut.Len() > 0 {
		t.Errorf("fall-through wrote output: out=%q err=%q", out.String(), errOut.String())
	}
}

// TestRunBackfillMode_ArgumentDiagnostics pins the argument-validation
// messages to the standardized surface: they run before any dependency is
// touched (nil deps prove it), and none may teach the deprecated
// -backfill-start/-backfill-end spelling.
func TestRunBackfillMode_ArgumentDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		start, end string
		wantErr    string
	}{
		{"bad start", "not-a-time", "2024-01-02T00:00:00Z", "invalid backfill start time format"},
		{"bad end", "2024-01-01T00:00:00Z", "not-a-time", "invalid backfill end time format"},
		{"start after end", "2024-01-02T00:00:00Z", "2024-01-01T00:00:00Z", "backfill start time must be before end time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runBackfillMode(context.Background(), nil, nil, nil, nil, tt.start, tt.end, nil, nil)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %q, want substring %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "backfill-start") || strings.Contains(err.Error(), "backfill-end") {
				t.Errorf("err %q teaches the deprecated flag spelling", err)
			}
		})
	}
}

// A bad backfill invocation must NOT fall through into scheduled mode — it has
// to exit with the parse error's code.
func TestDispatch_BackfillVerbBadArgsDoesNotFallThrough(t *testing.T) {
	var out, errOut bytes.Buffer
	code, fellThrough, _ := dispatch([]string{"click-dog", "backfill"}, &out, &errOut)

	if fellThrough {
		t.Fatal("invalid backfill args must not fall through to the daemon")
	}
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}
