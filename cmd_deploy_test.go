package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDeploy_NoArgsPrintsHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runDeploy(nil, &out, &errOut); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}

	if got := out.String(); !strings.Contains(got, "Subcommands:") ||
		!strings.Contains(got, "status") || !strings.Contains(got, "uninstall") {
		t.Errorf("stdout missing help block:\n%s", got)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr should be empty for bare invocation, got:\n%s", errOut.String())
	}
}

func TestRunDeploy_HelpFlag(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		t.Run(flag, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runDeploy([]string{flag}, &out, &errOut); code != 0 {
				t.Errorf("exit = %d for %q, want 0", code, flag)
			}
			if got := out.String(); !strings.Contains(got, "Subcommands:") {
				t.Errorf("stdout missing help for %q:\n%s", flag, got)
			}
			if errOut.Len() != 0 {
				t.Errorf("stderr should be empty for %q, got:\n%s", flag, errOut.String())
			}
		})
	}
}

func TestRunDeploy_UnknownSubcommand(t *testing.T) {
	// runDeploy now returns its exit code instead of calling os.Exit, so the
	// unknown-subcommand path can be asserted directly without a subprocess.
	var out, errOut bytes.Buffer
	code := runDeploy([]string{"bogus"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), `unknown subcommand "bogus"`) {
		t.Errorf("stderr missing unknown-subcommand line:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "Subcommands:") {
		t.Errorf("stderr missing help block:\n%s", errOut.String())
	}
}
