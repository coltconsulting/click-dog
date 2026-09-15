package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, _, err := loadConfig(*configPath)
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
			// Process discovery is systemd-only, so this is also the path a
			// container or non-systemd host always lands on. Both documented
			// alternatives target the process directly and need no systemctl.
			_, _ = fmt.Fprintf(out, "Without systemd, flush the process directly: curl -X POST %s/flush (or send SIGUSR1 to the pid)\n",
				adminFlushURL(cfg.Metrics.AdminListenAddress))
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

// adminFlushURL renders the admin listener's base URL for operator hints.
// A wildcard bind is not dialable as written, so an unspecified host is
// rewritten to loopback — the listener is reachable there whatever it binds.
func adminFlushURL(listenAddress string) string {
	addr := listenAddress
	if addr == "" {
		addr = "127.0.0.1:9091"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port; hand it back rather than inventing an address.
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
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
