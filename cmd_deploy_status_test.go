package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runDeployStatus calls os.Exit, so each scenario re-executes the test
// binary as a subprocess gated by env vars. The child sets up the
// overridable exec/lookup hooks via applyDeployStatusScenario before
// dispatching into runDeployStatus.
func runDeployStatusViaScenario(t *testing.T, scenario, healthURL string, args []string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunDeployStatus_Helper") //nolint:gosec
	env := append(os.Environ(),
		"GO_DEPLOY_STATUS_HELPER=1",
		"DEPLOY_STATUS_SCENARIO="+scenario,
		"DEPLOY_STATUS_TEST_ARGS="+strings.Join(args, "\x00"),
	)
	if healthURL != "" {
		env = append(env, "DEPLOY_STATUS_TEST_HEALTH_URL="+healthURL)
	}
	cmd.Env = env

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exit = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), exit
}

func TestRunDeployStatus_Helper(t *testing.T) {
	if os.Getenv("GO_DEPLOY_STATUS_HELPER") != "1" {
		t.Skip("helper only runs in a subprocess")
	}
	applyDeployStatusScenario(os.Getenv("DEPLOY_STATUS_SCENARIO"))
	if url := os.Getenv("DEPLOY_STATUS_TEST_HEALTH_URL"); url != "" {
		deployStatusHealthURL = url
	}

	raw := os.Getenv("DEPLOY_STATUS_TEST_ARGS")
	var args []string
	if raw != "" {
		args = strings.Split(raw, "\x00")
	}
	// runDeployStatus returns its exit code instead of calling os.Exit; surface
	// it so the parent subprocess observes the scenario's intended exit.
	os.Exit(runDeployStatus(args, os.Stdout, os.Stderr))
}

func TestDeployStatusFakeExec(t *testing.T) {
	if os.Getenv("GO_DEPLOY_STATUS_FAKE_EXEC") != "1" {
		t.Skip("fake exec only runs when invoked via fakeExitCmd")
	}
	_, _ = io.WriteString(os.Stdout, os.Getenv("GO_DEPLOY_STATUS_FAKE_STDOUT"))
	code := 0
	_, _ = fmt.Sscanf(os.Getenv("GO_DEPLOY_STATUS_FAKE_EXIT"), "%d", &code)
	os.Exit(code)
}

func fakeExitCmd(ctx context.Context, stdoutPayload string, exitCode int) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestDeployStatusFakeExec") //nolint:gosec
	cmd.Env = append(os.Environ(),
		"GO_DEPLOY_STATUS_FAKE_EXEC=1",
		"GO_DEPLOY_STATUS_FAKE_STDOUT="+stdoutPayload,
		fmt.Sprintf("GO_DEPLOY_STATUS_FAKE_EXIT=%d", exitCode),
	)
	return cmd
}

func applyDeployStatusScenario(scenario string) {
	switch scenario {
	case "systemctl_missing":
		execLookPath = func(string) (string, error) {
			return "", errors.New("not found")
		}
		execCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return fakeExitCmd(ctx, "", 0)
		}
		deployStatusHealthURL = "http://127.0.0.1:1/unreachable"

	case "systemctl_inactive":
		execLookPath = func(name string) (string, error) {
			if name == "systemctl" {
				return "/usr/bin/systemctl", nil
			}
			return "", errors.New("not found")
		}
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if name == "systemctl" {
				if len(args) > 0 && args[0] == "is-active" {
					return fakeExitCmd(ctx, "inactive\n", 3)
				}
				return fakeExitCmd(ctx, "disabled\n", 1)
			}
			return fakeExitCmd(ctx, "", 0)
		}
		deployStatusHealthURL = "http://127.0.0.1:1/unreachable"

	case "all_green":
		execLookPath = func(name string) (string, error) {
			if name == "systemctl" {
				return "/usr/bin/systemctl", nil
			}
			if name == "click-dog" {
				return "/usr/local/bin/click-dog", nil
			}
			return "", errors.New("not found")
		}
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if name == "systemctl" {
				if len(args) > 0 && args[0] == "is-active" {
					return fakeExitCmd(ctx, "active\n", 0)
				}
				return fakeExitCmd(ctx, "enabled\n", 0)
			}
			if strings.HasSuffix(name, "click-dog") {
				return fakeExitCmd(ctx, "click-dog v26.05.1\n", 0)
			}
			return fakeExitCmd(ctx, "", 0)
		}
	}
}

func TestDeployStatus_SystemctlMissing(t *testing.T) {
	stdout, _, exit := runDeployStatusViaScenario(t, "systemctl_missing", "", nil)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	for _, want := range []string{
		"Binary:           (in-process)",
		"systemd active:   not-installed",
		"systemd enabled:  not-installed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\nfull output:\n%s", want, stdout)
		}
	}
}

func TestDeployStatus_SystemctlInactive(t *testing.T) {
	stdout, _, exit := runDeployStatusViaScenario(t, "systemctl_inactive", "", nil)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(stdout, "systemd active:   inactive") {
		t.Errorf("stdout missing 'inactive' line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "systemd enabled:  disabled") {
		t.Errorf("stdout missing 'disabled' line:\n%s", stdout)
	}
}

func TestDeployStatus_AllGreen_PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stdout, _, exit := runDeployStatusViaScenario(t, "all_green", srv.URL+"/healthz", nil)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s", exit, stdout)
	}
	for _, want := range []string{
		"Binary:           /usr/local/bin/click-dog",
		"Version:          v26.05.1",
		"systemd active:   active",
		"systemd enabled:  enabled",
		"Health endpoint:  ok",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\nfull output:\n%s", want, stdout)
		}
	}
}

func TestDeployStatus_AllGreen_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stdout, _, exit := runDeployStatusViaScenario(t, "all_green", srv.URL+"/healthz", []string{"--json"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s", exit, stdout)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	for k, want := range map[string]any{
		"systemd_active":     true,
		"systemd_enabled":    true,
		"health_status":      "ok",
		"health_status_code": float64(200),
		"binary_path":        "/usr/local/bin/click-dog",
		"version":            "v26.05.1",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

func TestDeployStatus_HealthNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	stdout, _, exit := runDeployStatusViaScenario(t, "all_green", srv.URL+"/healthz", []string{"--json"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1 (503 is not ok)\nstdout:\n%s", exit, stdout)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if got["health_status"] != "error" {
		t.Errorf("health_status = %v, want \"error\"", got["health_status"])
	}
	if got["health_status_code"] != float64(503) {
		t.Errorf("health_status_code = %v, want 503", got["health_status_code"])
	}
}
