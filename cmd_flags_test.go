package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestSubcommandFlagParsing_NoProcessExit locks in the flag-parsing half of the
// "handlers return exit codes instead of exiting" contract (#298). Every
// dispatch handler builds its FlagSet with flag.ContinueOnError, so `--help`
// returns 0 and an unknown flag returns 2 *to the caller* — flag.Parse no
// longer calls os.Exit on those paths.
//
// The guard value is structural: if any handler reverted to flag.ExitOnError,
// these in-process calls would terminate the test binary (flag.Parse would
// os.Exit) instead of returning a code, so this test would crash rather than
// fail — either way, the regression is caught. `--help` and an unknown flag
// both short-circuit inside Parse, before any config load / network / stdin,
// so the handlers are safe to invoke directly here.
func TestSubcommandFlagParsing_NoProcessExit(t *testing.T) {
	handlers := []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
	}{
		{"check", runCheck},
		{"validate", runValidate},
		{"init", runInit},
		{"flush", runFlush},
		{"test", runTest},
		{"test export", runTestExport},
		{"test tracing", runTestTracing},
		{"test-span", runTestSpan},
		{"self-update", runSelfUpdate},
		{"create-dashboards", runCreateDashboards},
		{"deploy status", runDeployStatus},
		{"deploy kubernetes", runDeployKubernetes},
		{"deploy docker", runDeployDocker},
	}
	for _, h := range handlers {
		t.Run(h.name+"/help-returns-0", func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := h.run([]string{"--help"}, &out, &errOut); code != 0 {
				t.Errorf("%s --help: exit = %d, want 0", h.name, code)
			}
		})
		t.Run(h.name+"/unknown-flag-returns-2", func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := h.run([]string{"--definitely-not-a-real-flag"}, &out, &errOut); code != 2 {
				t.Errorf("%s --definitely-not-a-real-flag: exit = %d, want 2", h.name, code)
			}
		})
	}
}

// singleDashLongOption matches a single-dash long option like ` -config` while
// letting `--config`, single-char shorts (`-c`), hyphenated words
// (`test-span`), and dates through. It pins the standardized --flag spelling.
var singleDashLongOption = regexp.MustCompile(`(?:^|[\s"'(\[` + "`" + `])-[a-z][a-z0-9][a-z0-9-]*`)

// TestHelpOutput_UsesDoubleDashLongOptions pins every command's help text (and
// the root banner) to the documented --flag spelling, so generated help cannot
// drift back to advertising single-dash long options. Lines that deliberately
// name the deprecated mode-flag spellings are exempt.
func TestHelpOutput_UsesDoubleDashLongOptions(t *testing.T) {
	outputs := map[string]string{}

	handlers := []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
		args []string
	}{
		{"check", runCheck, []string{"--help"}},
		{"validate", runValidate, []string{"--help"}},
		{"init", runInit, []string{"--help"}},
		{"flush", runFlush, []string{"--help"}},
		{"analyze", runAnalyze, []string{"--help"}},
		{"analyze queries", runAnalyzeQueries, []string{"--help"}},
		{"analyze trace", runAnalyzeTrace, []string{"--help"}},
		{"test", runTest, []string{"--help"}},
		{"test export", runTestExport, []string{"--help"}},
		{"test tracing", runTestTracing, []string{"--help"}},
		{"test-span", runTestSpan, []string{"--help"}},
		{"self-update", runSelfUpdate, []string{"--help"}},
		{"create-dashboards", runCreateDashboards, []string{"--help"}},
		{"deploy status", runDeployStatus, []string{"--help"}},
		{"deploy kubernetes", runDeployKubernetes, []string{"--help"}},
		{"deploy docker", runDeployDocker, []string{"--help"}},
	}
	for _, h := range handlers {
		var out, errOut bytes.Buffer
		h.run(h.args, &out, &errOut)
		outputs[h.name] = out.String() + errOut.String()
	}

	var banner bytes.Buffer
	printUsage(&banner)
	outputs["root banner"] = banner.String()

	var backfillHelp bytes.Buffer
	_, _ = rewriteBackfillArgs([]string{"-h"}, &backfillHelp)
	outputs["backfill"] = backfillHelp.String()

	for name, text := range outputs {
		if text == "" {
			t.Errorf("%s: produced no help output", name)
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(strings.ToLower(line), "deprecated") {
				continue
			}
			if m := singleDashLongOption.FindString(line); m != "" {
				t.Errorf("%s help advertises single-dash long option %q in line %q", name, strings.TrimSpace(m), line)
			}
		}
	}
}

// TestMain_RejectsArgumentsAfterFlags pins #511 M15. Flag parsing stops at the
// first non-flag, so `click-dog --config x validate` used to drop the verb and
// start the scheduled daemon — a CI config check that hangs, or a backfill that
// becomes a live exporter. main re-runs in a subprocess because it calls
// os.Exit.
func TestMain_RejectsArgumentsAfterFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--config", "missing.yaml", "validate"}, `the "validate" command must come before its flags: click-dog validate [flags]`},
		{[]string{"--config", "missing.yaml", "backfill", "--start", "2026-09-01T00:00:00Z"}, `the "backfill" command must come before its flags`},
		{[]string{"--dry-run", "prod"}, `unexpected argument "prod"`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMain_StrayArgsHelper$") //nolint:gosec // Args[0] is the test binary, not user input.
			cmd.Env = append(os.Environ(), "GO_MAIN_STRAY_ARGS="+strings.Join(tc.args, "\x1f"))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("exit = %v, want 2; stderr:\n%s", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestMain_StrayArgsHelper(t *testing.T) {
	raw := os.Getenv("GO_MAIN_STRAY_ARGS")
	if raw == "" {
		t.Skip("helper only runs in a subprocess")
	}
	os.Args = append([]string{"click-dog"}, strings.Split(raw, "\x1f")...)
	main()
	t.Fatal("main returned instead of exiting")
}
