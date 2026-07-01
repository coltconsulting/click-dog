package selfaudit

import (
	"testing"
	"time"
)

func TestThrottle_FirstAllowsThenWindowsThenResets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	th := newThrottle(time.Hour, clock)

	// First event for a key always fires (this is the WARN on transition).
	if !th.allow("sidecar") {
		t.Fatal("first allow returned false; transition WARN would be suppressed")
	}
	// Within the window: suppressed (≤1/hour while persisting).
	now = now.Add(59 * time.Minute)
	if th.allow("sidecar") {
		t.Error("allow fired again inside the window")
	}
	// After the window: fires again.
	now = now.Add(2 * time.Minute) // 61 min total
	if !th.allow("sidecar") {
		t.Error("allow did not fire after the window elapsed")
	}
	// Reset (detection cleared) → next allow fires immediately so a fresh
	// detection always logs.
	th.reset("sidecar")
	if !th.allow("sidecar") {
		t.Error("allow did not fire immediately after reset")
	}
}

func TestThrottle_KeysAreIndependent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	th := newThrottle(time.Hour, func() time.Time { return now })

	if !th.allow("a") {
		t.Fatal("key a first allow false")
	}
	if !th.allow("b") {
		t.Error("key b suppressed by key a's window — keys must be independent")
	}
}
