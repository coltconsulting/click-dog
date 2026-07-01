package selfaudit

import "time"

// throttle emits at most once per window, keyed by an opaque token. It is local
// to this package because clicklog has no throttle — only level filtering — and
// the topology WARN must repeat at most once per hour while a detection
// persists. Not safe for concurrent use; the auditor calls it only from its own
// single goroutine.
type throttle struct {
	window time.Duration
	now    func() time.Time
	last   map[string]time.Time
}

func newThrottle(window time.Duration, now func() time.Time) *throttle {
	return &throttle{window: window, now: now, last: make(map[string]time.Time)}
}

// allow reports whether an event keyed by key may fire now, recording the time
// when it returns true. The first call for a key (or the first after reset)
// always returns true.
func (t *throttle) allow(key string) bool {
	n := t.now()
	if last, ok := t.last[key]; ok && n.Sub(last) < t.window {
		return false
	}
	t.last[key] = n
	return true
}

// reset forgets a key so the next allow returns true — used when a detection
// clears, so a fresh detection always logs immediately.
func (t *throttle) reset(key string) { delete(t.last, key) }
