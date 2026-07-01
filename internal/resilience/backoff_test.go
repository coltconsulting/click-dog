package resilience

import (
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
)

func TestAdaptivePoller_InitialState(t *testing.T) {
	baseInterval := 30 * time.Second
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 2.0,
	})

	if poller.CurrentInterval() != baseInterval {
		t.Errorf("Expected initial interval %v, got %v", baseInterval, poller.CurrentInterval())
	}

	if poller.IsBackedOff() {
		t.Error("Should not be backed off initially")
	}
}

func TestAdaptivePoller_BackoffOnFailure(t *testing.T) {
	baseInterval := 10 * time.Second
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 2.0,
	})

	// First failure - should double
	poller.RecordFailure()
	expectedInterval := 20 * time.Second
	if poller.CurrentInterval() != expectedInterval {
		t.Errorf("Expected interval %v after first failure, got %v", expectedInterval, poller.CurrentInterval())
	}
	// Second failure - should double again
	poller.RecordFailure()
	expectedInterval = 40 * time.Second
	if poller.CurrentInterval() != expectedInterval {
		t.Errorf("Expected interval %v after second failure, got %v", expectedInterval, poller.CurrentInterval())
	}

	// Third failure
	poller.RecordFailure()
	expectedInterval = 80 * time.Second
	if poller.CurrentInterval() != expectedInterval {
		t.Errorf("Expected interval %v after third failure, got %v", expectedInterval, poller.CurrentInterval())
	}

	if !poller.IsBackedOff() {
		t.Error("Should be backed off after failures")
	}
}

func TestAdaptivePoller_MaxIntervalCapped(t *testing.T) {
	baseInterval := 30 * time.Second
	maxInterval := 60 * time.Second
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  60, // 60 second max
		BackoffFactor: 2.0,
	})

	// First failure: 30 -> 60
	poller.RecordFailure()
	if poller.CurrentInterval() != maxInterval {
		t.Errorf("Expected interval %v, got %v", maxInterval, poller.CurrentInterval())
	}

	// Second failure: should stay at max
	poller.RecordFailure()
	if poller.CurrentInterval() != maxInterval {
		t.Errorf("Expected interval to stay at max %v, got %v", maxInterval, poller.CurrentInterval())
	}

}

func TestAdaptivePoller_ResetOnSuccess(t *testing.T) {
	baseInterval := 10 * time.Second
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 2.0,
	})

	// Create some backoff
	poller.RecordFailure()
	poller.RecordFailure()

	if poller.CurrentInterval() != 40*time.Second {
		t.Errorf("Expected backed off interval, got %v", poller.CurrentInterval())
	}

	// Record success - should reset
	poller.RecordSuccess()

	if poller.CurrentInterval() != baseInterval {
		t.Errorf("Expected interval to reset to base %v, got %v", baseInterval, poller.CurrentInterval())
	}
}

func TestAdaptivePoller_ResetsAfterSustainedOutage(t *testing.T) {
	// Regression: a success after a long run of failures must reset to base.
	// The original implementation gated the reset on a "last success was
	// recent" check that could never pass once backoff exceeded the old
	// reset window, so the poller stayed pinned at maxInterval forever after
	// any real outage.
	baseInterval := 30 * time.Second
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 2.0,
	})

	// Climb to the max interval (30→60→120→240→300) over many failures.
	for i := 0; i < 6; i++ {
		poller.RecordFailure()
	}
	if poller.CurrentInterval() != 300*time.Second {
		t.Fatalf("expected max interval 300s after sustained failures, got %v", poller.CurrentInterval())
	}

	// A single success once ClickHouse recovers must snap back to base.
	poller.RecordSuccess()
	if poller.CurrentInterval() != baseInterval {
		t.Fatalf("expected reset to base %v after recovery, got %v", baseInterval, poller.CurrentInterval())
	}
}

func TestAdaptivePoller_DefaultValues(t *testing.T) {
	baseInterval := 30 * time.Second
	// Empty config should use defaults
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{})

	// Should use default max interval (5 minutes)
	// Failure enough times to hit max
	for i := 0; i < 10; i++ {
		poller.RecordFailure()
	}

	maxExpected := 5 * time.Minute
	if poller.CurrentInterval() != maxExpected {
		t.Errorf("Expected max interval %v with defaults, got %v", maxExpected, poller.CurrentInterval())
	}
}

func TestAdaptivePoller_CustomBackoffFactor(t *testing.T) {
	baseInterval := 10 * time.Second
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  600,
		BackoffFactor: 1.5, // 1.5x instead of 2x
	})

	poller.RecordFailure()
	expectedInterval := 15 * time.Second // 10 * 1.5
	if poller.CurrentInterval() != expectedInterval {
		t.Errorf("Expected interval %v with 1.5x factor, got %v", expectedInterval, poller.CurrentInterval())
	}

	poller.RecordFailure()
	expectedInterval = 22500 * time.Millisecond // 15 * 1.5 = 22.5
	if poller.CurrentInterval() != expectedInterval {
		t.Errorf("Expected interval %v, got %v", expectedInterval, poller.CurrentInterval())
	}
}

func TestAdaptivePoller_InvalidBackoffFactor(t *testing.T) {
	baseInterval := 10 * time.Second
	// Backoff factor <= 1 should default to 2.0
	poller := NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 0.5, // Invalid - should use 2.0
	})

	poller.RecordFailure()
	expectedInterval := 20 * time.Second // Should use 2.0 factor
	if poller.CurrentInterval() != expectedInterval {
		t.Errorf("Expected default 2.0 factor to give %v, got %v", expectedInterval, poller.CurrentInterval())
	}
}
