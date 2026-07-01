package processor

import (
	"errors"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

// TestUpdatePollerState_LeaderStandbyLeavesBackoffUnchanged pins that a
// cluster standby skip (ErrLeaderStandby) neither rewards nor penalises the
// adaptive poller: a standby does no fetch/export work, so an elevated backoff
// from prior real failures must carry over rather than be silently reset.
func TestUpdatePollerState_LeaderStandbyLeavesBackoffUnchanged(t *testing.T) {
	base := 10 * time.Second
	poller := resilience.NewAdaptivePoller(base, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 2.0,
	})
	ticker := time.NewTicker(base)
	defer ticker.Stop()

	// Drive backoff up with two real failures.
	realErr := errors.New("clickhouse unreachable")
	UpdatePollerState(poller, realErr, ticker, base, nil)
	UpdatePollerState(poller, realErr, ticker, base, nil)
	elevated := poller.CurrentInterval()
	if elevated <= base {
		t.Fatalf("expected elevated interval after failures, got %v", elevated)
	}

	// Standby skips must leave the elevated interval untouched.
	UpdatePollerState(poller, ErrLeaderStandby, ticker, base, nil)
	UpdatePollerState(poller, ErrLeaderStandby, ticker, base, nil)
	if got := poller.CurrentInterval(); got != elevated {
		t.Errorf("ErrLeaderStandby changed backoff: got %v, want %v (unchanged)", got, elevated)
	}

	// A genuine success still resets — proving the sentinel was the only thing ignored.
	UpdatePollerState(poller, nil, ticker, base, nil)
	if got := poller.CurrentInterval(); got != base {
		t.Errorf("real success should reset to base: got %v, want %v", got, base)
	}
}
