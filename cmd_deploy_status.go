package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// override in tests
var (
	execLookPath          = exec.LookPath
	execCommandContext    = exec.CommandContext
	deployStatusHealthURL = "http://127.0.0.1:8686/healthz"
)

const (
	deployStatusHealthTimeout = 2 * time.Second
	deployStatusExecTimeout   = 3 * time.Second
)

// deployStatusReport is the stable JSON contract for `deploy status --json`.
// HealthStatus is one of "ok", "unreachable", or "error"; HealthStatusCode
// carries the HTTP status when the endpoint responded (0 otherwise). The
// pair lets consumers distinguish "couldn't connect" from "responded 503"
// without parsing a freeform string.
type deployStatusReport struct {
	Version          string `json:"version"`
	BinaryPath       string `json:"binary_path"`
	SystemdActive    bool   `json:"systemd_active"`
	SystemdEnabled   bool   `json:"systemd_enabled"`
	HealthStatus     string `json:"health_status"`
	HealthStatusCode int    `json:"health_status_code"`

	systemdActiveRaw  string
	systemdEnabledRaw string
}

func runDeployStatus(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("deploy status", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "Emit status as JSON")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog deploy status — report installed version, systemd state, and health endpoint

Usage:
  click-dog deploy status [flags]

Exits 0 when systemd reports active and the health endpoint returned 200;
otherwise exits 1. The plain-text output is printed regardless of exit code.

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	report := collectDeployStatus()

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			_, _ = fmt.Fprintf(errOut, "Error encoding JSON: %v\n", err)
			return 1
		}
	} else {
		printDeployStatusText(out, report)
	}

	if report.SystemdActive && report.HealthStatus == "ok" {
		return 0
	}
	return 1
}

func collectDeployStatus() deployStatusReport {
	r := deployStatusReport{}
	r.BinaryPath, r.Version = collectVersion()
	r.systemdActiveRaw, r.systemdEnabledRaw, r.SystemdActive, r.SystemdEnabled = collectSystemd()
	r.HealthStatus, r.HealthStatusCode = collectHealth(deployStatusHealthURL)
	return r
}

func collectVersion() (binaryPath, ver string) {
	// LookPath returns the first match on $PATH. If an operator has a stale
	// dev click-dog earlier on PATH than /usr/local/bin/click-dog, status
	// reports that one — by design, since that's what would run if the
	// operator typed `click-dog`. Path-pinning would hide the real ambient
	// state from a status command.
	path, err := execLookPath("click-dog")
	if err != nil {
		// Running click-dog produced this report — even if it's not on
		// PATH, the in-process version is still authoritative.
		return "", version
	}

	// Resolve the same-binary case (avoid shelling out to ourselves) after
	// following symlinks on both sides, so a /usr/local/bin/click-dog
	// symlink to the test binary still trips the short-circuit.
	if self, selfErr := os.Executable(); selfErr == nil {
		if sameFile(self, path) {
			return path, version
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), deployStatusExecTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, path, "-version")
	out, err := cmd.Output()
	if err != nil {
		return path, "unknown"
	}
	v := strings.TrimSpace(string(out))
	v = strings.TrimPrefix(v, "click-dog ")
	if v == "" {
		v = "unknown"
	}
	return path, v
}

func sameFile(a, b string) bool {
	resolveOr := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return resolveOr(a) == resolveOr(b)
}

func collectSystemd() (activeRaw, enabledRaw string, active, enabled bool) {
	if _, err := execLookPath("systemctl"); err != nil {
		return "not-installed", "not-installed", false, false
	}

	activeRaw = runSystemctl("is-active", "click-dog")
	enabledRaw = runSystemctl("is-enabled", "click-dog")

	active = activeRaw == "active"
	enabled = enabledRaw == "enabled"
	return activeRaw, enabledRaw, active, enabled
}

func runSystemctl(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), deployStatusExecTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, "systemctl", args...)
	out, err := cmd.Output()
	// systemctl prints the state on stdout and exits non-zero for any state
	// other than "active"/"enabled". Treat stdout as authoritative and only
	// fall back to the error string when stdout was empty.
	s := strings.TrimSpace(string(out))
	if s != "" {
		return s
	}
	if err != nil {
		// `systemctl is-active` on a missing unit prints "inactive" to
		// stdout on most distros, but on older systemd or when the unit
		// file is missing it can be empty with a non-zero exit. Map that
		// case to "not-installed" so operators see something meaningful.
		return "not-installed"
	}
	return ""
}

func collectHealth(url string) (status string, code int) {
	client := &http.Client{Timeout: deployStatusHealthTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return "unreachable", 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return "ok", resp.StatusCode
	}
	return "error", resp.StatusCode
}

func printDeployStatusText(out io.Writer, r deployStatusReport) {
	binary := r.BinaryPath
	if binary == "" {
		binary = "(in-process)"
	}
	health := r.HealthStatus
	if r.HealthStatusCode != 0 && r.HealthStatus != "ok" {
		health = fmt.Sprintf("%s (HTTP %d)", r.HealthStatus, r.HealthStatusCode)
	}
	_, _ = fmt.Fprintf(out, "Binary:           %s\n", binary)
	_, _ = fmt.Fprintf(out, "Version:          %s\n", r.Version)
	_, _ = fmt.Fprintf(out, "systemd active:   %s\n", r.systemdActiveRaw)
	_, _ = fmt.Fprintf(out, "systemd enabled:  %s\n", r.systemdEnabledRaw)
	_, _ = fmt.Fprintf(out, "Health endpoint:  %s (%s)\n", health, deployStatusHealthURL)
}
