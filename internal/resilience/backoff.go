package resilience

import (
	"sync"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
)

// AdaptivePoller manages adaptive polling intervals with exponential backoff
type AdaptivePoller struct {
	mu sync.RWMutex

	baseInterval     time.Duration
	maxInterval      time.Duration
	currentInterval  time.Duration
	backoffFactor    float64
	consecutiveFails int
}

// NewAdaptivePoller creates a new adaptive poller with the given configuration.
func NewAdaptivePoller(baseInterval time.Duration, cfg config.BackoffConfig) *AdaptivePoller {
	maxInterval := time.Duration(cfg.MaxIntervalS) * time.Second
	if maxInterval <= 0 {
		maxInterval = 5 * time.Minute
	}

	backoffFactor := cfg.BackoffFactor
	if backoffFactor <= 1 {
		backoffFactor = 2.0
	}

	return &AdaptivePoller{
		baseInterval:    baseInterval,
		maxInterval:     maxInterval,
		currentInterval: baseInterval,
		backoffFactor:   backoffFactor,
	}
}

// RecordSuccess records a successful poll. If the poller had backed off due to
// prior failures, a single success means ClickHouse is reachable again, so we
// snap straight back to the base interval to restore polling freshness.
func (ap *AdaptivePoller) RecordSuccess() {
	ap.mu.Lock()
	defer ap.mu.Unlock()

	ap.consecutiveFails = 0

	if ap.currentInterval > ap.baseInterval {
		ap.currentInterval = ap.baseInterval
		clicklog.Info("Backoff reset to base interval: %v", ap.currentInterval)
	}
}

// RecordFailure records a failed poll and increases the interval
func (ap *AdaptivePoller) RecordFailure() {
	ap.mu.Lock()
	defer ap.mu.Unlock()

	ap.consecutiveFails++

	// Calculate new interval with exponential backoff
	newInterval := time.Duration(float64(ap.currentInterval) * ap.backoffFactor)
	if newInterval > ap.maxInterval {
		newInterval = ap.maxInterval
	}

	if newInterval != ap.currentInterval {
		oldInterval := ap.currentInterval
		ap.currentInterval = newInterval
		clicklog.Warn("Backoff increased: %v → %v (failures=%d, factor=%.1f)", oldInterval, ap.currentInterval, ap.consecutiveFails, ap.backoffFactor)
	} else if ap.currentInterval == ap.maxInterval {
		clicklog.Debug("Backoff at maximum interval %v (failures=%d)", ap.maxInterval, ap.consecutiveFails)
	}
}

// CurrentInterval returns the current polling interval
func (ap *AdaptivePoller) CurrentInterval() time.Duration {
	ap.mu.RLock()
	defer ap.mu.RUnlock()
	return ap.currentInterval
}

// IsBackedOff returns true if the interval has been increased from the base
func (ap *AdaptivePoller) IsBackedOff() bool {
	ap.mu.RLock()
	defer ap.mu.RUnlock()
	return ap.currentInterval > ap.baseInterval
}
