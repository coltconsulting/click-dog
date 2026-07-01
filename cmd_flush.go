package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/leader"
)

func runFlush(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog flush — trigger an immediate export cycle

Signals the running click-dog service to run an immediate fetch-and-export
cycle. The poll timer resets after the flush so there is no overlap with
the next regular cycle.

In single-node mode, sends SIGUSR1 to the local process.
In HA mode, writes a flush request to Keeper that the leader picks up.

This is best-effort — if the signal or request is lost, the next regular
cycle will run as normal.

Usage:
  click-dog flush [flags]

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

	resolvedPath, err := config.ResolveConfigPath(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Config: FAIL (%v)\n", err)
		return 1
	}
	cfg, err := config.LoadConfig(resolvedPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Config: FAIL (%v)\n", err)
		return 1
	}

	if cfg.HA.Active() {
		_, _ = fmt.Fprintln(out, "HA mode — writing flush request to Keeper...")
		_, _ = fmt.Fprintln(out, "Waiting for leader to pick up request (up to 5s)...")
		if err := leader.RequestFlush(cfg.HA.Keeper); err != nil {
			_, _ = fmt.Fprintf(out, "Failed: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(out, "Flush requested. The leader will pick it up shortly.")
		_, _ = fmt.Fprintln(out, "Check leader logs: journalctl -u click-dog -f")
	} else {
		pid, err := findClickDogPID()
		if err != nil {
			_, _ = fmt.Fprintf(out, "Could not find running click-dog process: %v\n", err)
			_, _ = fmt.Fprintln(out, "Is the service running? Check: systemctl status click-dog")
			return 1
		}

		proc, err := os.FindProcess(pid)
		if err != nil {
			_, _ = fmt.Fprintf(out, "Failed to find process %d: %v\n", pid, err)
			return 1
		}

		if err := proc.Signal(syscall.SIGUSR1); err != nil {
			_, _ = fmt.Fprintf(out, "Failed to send signal to PID %d: %v\n", pid, err)
			return 1
		}

		_, _ = fmt.Fprintf(out, "Flush signal sent to click-dog (PID %d)\n", pid)
		_, _ = fmt.Fprintln(out, "Check logs: journalctl -u click-dog -f")
	}

	return 0
}

// findClickDogPID returns the PID of the running click-dog service.
func findClickDogPID() (int, error) {
	out, err := exec.Command("systemctl", "show", "click-dog", "--property=MainPID", "--value").Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl: %w", err)
	}
	pidStr := strings.TrimSpace(string(out))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid == 0 {
		return 0, fmt.Errorf("service not running (PID=%s)", pidStr)
	}
	return pid, nil
}
