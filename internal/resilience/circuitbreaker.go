package resilience

import (
	"sync"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
)

// CircuitState represents the state of the circuit breaker
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // Normal operation
	CircuitOpen                         // Blocking requests
	CircuitHalfOpen                     // Testing if service recovered
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Clock abstracts time for testing. Exported so that external test packages
// (e.g. root-level failure_test.go) can provide fake clocks.
type Clock interface {
	Now() time.Time
}

// realClock uses the standard time package
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// CircuitBreaker implements the circuit breaker pattern to protect ClickHouse
type CircuitBreaker struct {
	mu sync.RWMutex

	state       CircuitState
	failures    int       // Consecutive failures
	successes   int       // Consecutive successes (in half-open state)
	lastFailure time.Time // Time of last failure
	// Configuration
	failureThreshold int           // Number of failures to open circuit
	successThreshold int           // Number of successes to close circuit (from half-open)
	resetTimeout     time.Duration // Time to wait before trying again
	clock            Clock
}

// NewCircuitBreaker creates a new circuit breaker with the given configuration
func NewCircuitBreaker(cfg config.CircuitBreakerConfig) *CircuitBreaker {
	failureThreshold := cfg.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}

	successThreshold := cfg.SuccessThreshold
	if successThreshold <= 0 {
		successThreshold = 1
	}

	resetTimeout := time.Duration(cfg.ResetTimeoutS) * time.Second
	if resetTimeout <= 0 {
		resetTimeout = 60 * time.Second
	}

	return &CircuitBreaker{
		state:            CircuitClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		resetTimeout:     resetTimeout,
		clock:            realClock{},
	}
}

// NewCircuitBreakerWithClock creates a circuit breaker with a custom clock for testing.
func NewCircuitBreakerWithClock(cfg config.CircuitBreakerConfig, c Clock) *CircuitBreaker {
	cb := NewCircuitBreaker(cfg)
	cb.clock = c
	return cb
}

// Allow returns true if the circuit breaker allows a request to proceed
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.clock.Now()

	switch cb.state {
	case CircuitClosed:
		return true

	case CircuitOpen:
		// Check if reset timeout has passed
		if now.Sub(cb.lastFailure) >= cb.resetTimeout {
			cb.setState(CircuitHalfOpen)
			clicklog.Info("Circuit breaker transitioning to half-open state")
			return true
		}
		return false

	case CircuitHalfOpen:
		// In half-open, allow requests but track carefully
		return true

	default:
		return true
	}
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0

	switch cb.state {
	case CircuitHalfOpen:
		cb.successes++
		if cb.successes >= cb.successThreshold {
			cb.setState(CircuitClosed)
			cb.successes = 0
			clicklog.Info("Circuit breaker closed after successful recovery")
		} else {
			clicklog.Debug("Circuit breaker half-open: success %d/%d", cb.successes, cb.successThreshold)
		}
	case CircuitClosed:
	}
}

// RecordFailure records a failed request
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = cb.clock.Now()
	cb.successes = 0

	switch cb.state {
	case CircuitClosed:
		if cb.failures >= cb.failureThreshold {
			cb.setState(CircuitOpen)
			clicklog.Warn("Circuit breaker opened after %d consecutive failures", cb.failures)
		} else {
			clicklog.Debug("Circuit breaker: failure %d/%d", cb.failures, cb.failureThreshold)
		}
	case CircuitHalfOpen:
		// Failure in half-open state - go back to open
		cb.setState(CircuitOpen)
		clicklog.Warn("Circuit breaker re-opened after failure in half-open state")
	}
}

// State returns the current state of the circuit breaker
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Failures returns the current consecutive failure count
func (cb *CircuitBreaker) Failures() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.failures
}

// setState changes the state
func (cb *CircuitBreaker) setState(newState CircuitState) {
	cb.state = newState
}
