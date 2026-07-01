package processor

import (
	"errors"
)

const (
	// CanarySpanName is the operation name for synthetic canary spans.
	CanarySpanName = "click-dog.canary"
)

// ErrCircuitOpen is a sentinel error indicating the circuit breaker blocked this cycle.
// It is not a real failure — the poller should not adjust backoff when it sees this.
var ErrCircuitOpen = errors.New("circuit breaker open, cycle skipped")

// ErrCanaryRan is a sentinel error indicating a canary query ran in degraded mode.
// Like ErrCircuitOpen, the poller should not adjust backoff when it sees this.
var ErrCanaryRan = errors.New("canary query executed in degraded mode")

// ErrHealthCheck is a sentinel for health check failures, avoiding an allocation per cycle.
var ErrHealthCheck = errors.New("connection health check failed")

// ErrLeaderStandby is a sentinel error indicating a cluster-mode standby skipped
// the cycle because it does not hold leadership. Like ErrCircuitOpen, it is not
// a real failure and ran no ClickHouse/export work, so the poller must not adjust
// backoff (neither penalise nor reward) when it sees this.
var ErrLeaderStandby = errors.New("not leader, cycle skipped (standby)")
