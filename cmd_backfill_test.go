package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseBackfillArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		want     *runOptions // nil means the parse must be refused
		wantCode int         // only checked when want is nil
		wantErr  string      // substring expected on stderr
	}{
		{
			name: "start and end select a backfill run",
			args: []string{"--start", "2024-01-01T00:00:00Z", "--end", "2024-01-02T00:00:00Z"},
			want: &runOptions{configPath: "click-dog.yaml", backfillStart: "2024-01-01T00:00:00Z", backfillEnd: "2024-01-02T00:00:00Z"},
		},
		{
			name: "config and dry-run are carried",
			args: []string{"--start", "a", "--end", "b", "--config", "x.yaml", "--dry-run"},
			want: &runOptions{configPath: "x.yaml", dryRun: true, backfillStart: "a", backfillEnd: "b"},
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
			name:     "boolean value word is refused with the = form",
			args:     []string{"--dry-run", "false", "--start", "a", "--end", "b"},
			wantCode: 2,
			wantErr:  `unexpected argument "false": boolean flags take their value with =, e.g. --dry-run=false`,
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
			got, code := parseBackfillArgs(tt.args, &errOut)

			if tt.want == nil {
				if got != nil {
					t.Fatalf("opts = %+v, want refusal (nil)", got)
				}
				if code != tt.wantCode {
					t.Errorf("exit = %d, want %d", code, tt.wantCode)
				}
			} else {
				if got == nil {
					t.Fatalf("parse refused (code %d): %s", code, errOut.String())
				}
				if *got != *tt.want {
					t.Errorf("opts = %+v, want %+v", *got, *tt.want)
				}
				if !got.backfill() {
					t.Error("parsed options do not select backfill mode")
				}
			}
			if tt.wantErr != "" && !strings.Contains(errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want substring %q", errOut.String(), tt.wantErr)
			}
		})
	}
}

// TestDispatch_BackfillVerbFallsThroughWithOptions asserts the backfill verb
// is a fall-through carrying its parsed options: main() runs the same pipeline
// it builds for scheduled mode, with the range set.
func TestDispatch_BackfillVerbFallsThroughWithOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	code, fellThrough, opts := dispatch(
		[]string{"click-dog", "backfill", "--start", "a", "--end", "b"}, &out, &errOut)

	if !fellThrough {
		t.Fatalf("backfill verb must fall through to the pipeline (code %d, stderr %s)", code, errOut.String())
	}
	if opts == nil || opts.backfillStart != "a" || opts.backfillEnd != "b" {
		t.Errorf("opts = %+v, want the backfill range a..b", opts)
	}
	if out.Len() > 0 || errOut.Len() > 0 {
		t.Errorf("fall-through wrote output: out=%q err=%q", out.String(), errOut.String())
	}
}

// TestRunBackfillMode_ArgumentDiagnostics pins the argument-validation
// messages to the standardized surface: they run before any dependency is
// touched (nil deps prove it), and each names the backfill range.
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
