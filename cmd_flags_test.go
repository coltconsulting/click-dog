package main

import (
	"bytes"
	"io"
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
