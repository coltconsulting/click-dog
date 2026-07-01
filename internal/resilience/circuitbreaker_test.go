package resilience

import (
	"sync"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
)

// fakeClock is a test clock that can be advanced without sleeping
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = fc.now.Add(d)
}

// newTestCircuitBreaker creates a CircuitBreaker with a fake clock for testing
func newTestCircuitBreaker(cfg config.CircuitBreakerConfig, fc *fakeClock) *CircuitBreaker {
	cb := NewCircuitBreaker(cfg)
	cb.clock = fc
	return cb
}

func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		ResetTimeoutS:    60,
	})

	if cb.State() != CircuitClosed {
		t.Errorf("Expected initial state to be Closed, got %v", cb.State())
	}

	if !cb.Allow() {
		t.Error("Expected Allow() to return true when circuit is closed")
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		ResetTimeoutS:    60,
	})

	// Record failures up to threshold
	cb.RecordFailure()
	if cb.State() != CircuitClosed {
		t.Error("Should still be closed after 1 failure")
	}

	cb.RecordFailure()
	if cb.State() != CircuitClosed {
		t.Error("Should still be closed after 2 failures")
	}

	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Errorf("Should be open after 3 failures, got %v", cb.State())
	}

	// Verify Allow() returns false when open
	if cb.Allow() {
		t.Error("Expected Allow() to return false when circuit is open")
	}
}

func TestCircuitBreaker_SuccessResetsfailures(t *testing.T) {
	cb := NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		ResetTimeoutS:    60,
	})

	// Record 2 failures
	cb.RecordFailure()
	cb.RecordFailure()

	if cb.Failures() != 2 {
		t.Errorf("Expected 2 failures, got %d", cb.Failures())
	}

	// Record success - should reset failure count
	cb.RecordSuccess()

	if cb.Failures() != 0 {
		t.Errorf("Expected 0 failures after success, got %d", cb.Failures())
	}

	if cb.State() != CircuitClosed {
		t.Error("Should still be closed")
	}
}

func TestCircuitBreaker_HalfOpenAfterTimeout(t *testing.T) {
	fc := newFakeClock()
	cb := newTestCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 2,
		ResetTimeoutS:    1,
	}, fc)

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	if cb.State() != CircuitOpen {
		t.Error("Should be open after threshold")
	}

	// Advance past reset timeout
	fc.Advance(1100 * time.Millisecond)

	// Now Allow() should transition to half-open
	if !cb.Allow() {
		t.Error("Should allow after reset timeout")
	}

	if cb.State() != CircuitHalfOpen {
		t.Errorf("Should be half-open after timeout, got %v", cb.State())
	}
}

func TestCircuitBreaker_ClosesAfterSuccessInHalfOpen(t *testing.T) {
	fc := newFakeClock()
	cb := newTestCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeoutS:    1,
	}, fc)

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	// Advance past reset timeout
	fc.Advance(1100 * time.Millisecond)

	// Transition to half-open
	cb.Allow()

	if cb.State() != CircuitHalfOpen {
		t.Error("Should be half-open")
	}

	// Record success - should close
	cb.RecordSuccess()

	if cb.State() != CircuitClosed {
		t.Errorf("Should be closed after success in half-open, got %v", cb.State())
	}
}

func TestCircuitBreaker_ReopensOnFailureInHalfOpen(t *testing.T) {
	fc := newFakeClock()
	cb := newTestCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 2,
		ResetTimeoutS:    1,
	}, fc)

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	// Advance past reset timeout
	fc.Advance(1100 * time.Millisecond)

	// Transition to half-open
	cb.Allow()

	if cb.State() != CircuitHalfOpen {
		t.Error("Should be half-open")
	}

	// Record failure - should re-open
	cb.RecordFailure()

	if cb.State() != CircuitOpen {
		t.Errorf("Should be open after failure in half-open, got %v", cb.State())
	}
}

func TestCircuitBreaker_MultipleSuccessThreshold(t *testing.T) {
	fc := newFakeClock()
	cb := newTestCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 3, // Require 3 successes to close
		ResetTimeoutS:    1,
	}, fc)

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	// Advance past reset timeout
	fc.Advance(1100 * time.Millisecond)

	// Transition to half-open
	cb.Allow()

	// Record successes
	cb.RecordSuccess()
	if cb.State() != CircuitHalfOpen {
		t.Error("Should still be half-open after 1 success")
	}

	cb.RecordSuccess()
	if cb.State() != CircuitHalfOpen {
		t.Error("Should still be half-open after 2 successes")
	}

	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Errorf("Should be closed after 3 successes, got %v", cb.State())
	}
}

func TestCircuitBreaker_DefaultValues(t *testing.T) {
	// Test with empty config - should use defaults
	cb := NewCircuitBreaker(config.CircuitBreakerConfig{})

	// Need 3 failures (default) to open
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != CircuitClosed {
		t.Error("Should still be closed with default threshold of 3")
	}

	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Error("Should be open after 3 failures (default threshold)")
	}
}

func TestCircuitState_String(t *testing.T) {
	tests := []struct {
		state    CircuitState
		expected string
	}{
		{CircuitClosed, "closed"},
		{CircuitOpen, "open"},
		{CircuitHalfOpen, "half-open"},
		{CircuitState(99), "unknown"},
	}

	for _, tt := range tests {
		if tt.state.String() != tt.expected {
			t.Errorf("Expected %q, got %q", tt.expected, tt.state.String())
		}
	}
}
