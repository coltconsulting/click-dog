package leader

import (
	"context"
	"testing"

	"github.com/go-zookeeper/zk"
)

// TestFailOpen_ClearsStateSoGateFailsOpen pins the core invariant behind the
// post-join error handling in Run(): when the election goroutine exits
// unexpectedly, failOpen must clear election state so the export gate stops
// treating this instance as a standby. Otherwise Joined()==true with
// IsLeader()==false strands the reader (and, if it holds the lowest-seq znode,
// the whole cluster) in permanent skipped cycles.
func TestFailOpen_ClearsStateSoGateFailsOpen(t *testing.T) {
	le := &LeaderElection{myNode: "/click-dog/election/candidate-0000000001"}
	le.isLeader.Store(true) // pretend we had been promoted

	le.failOpen() // conn is nil — delete is best-effort, state-clear must still happen

	if le.Joined() {
		t.Error("Joined() must be false after failOpen so the gate fails open (exports)")
	}
	if le.IsLeader() {
		t.Error("IsLeader() must be false after failOpen (demoted)")
	}
}

// TestSessionExpiry_DropsCandidateBeforeBackoff pins the fail-open contract for
// the reconnect window. handleSessionExpiry demotes then calls dropCandidate
// before its backoff sleep, so during the sleep Joined()==false and the export
// gate keeps exporting through the partition — rather than leaving a stale
// joined-follower (Joined()==true, IsLeader()==false) that skips for the whole
// backoff window. This reproduces that state transition without a live Keeper.
func TestSessionExpiry_DropsCandidateBeforeBackoff(t *testing.T) {
	le := &LeaderElection{
		myNode: "/click-dog/election/candidate-0000000001",
		mySeq:  "candidate-0000000001",
	}
	le.isLeader.Store(true) // had been the leader before the session expired

	// The state transition handleSessionExpiry performs before sleeping.
	le.handleDemotion()
	le.dropCandidate()

	if le.Joined() {
		t.Error("Joined() must be false after dropCandidate so the gate exports during reconnect")
	}
	if le.IsLeader() {
		t.Error("IsLeader() must be false after demotion")
	}
}

// TestRejoinWithRetry_FailsOpenAndStopsOnCancel pins the recovery contract that
// replaced the old return-on-first-error behavior: rejoinWithRetry clears
// candidate state up front (so Joined()==false ⇒ the export gate fails OPEN for
// the whole retry window) and returns false promptly when ctx is canceled,
// which Run maps to a clean nil exit. An already-canceled ctx short-circuits
// before any Keeper I/O, so this covers both the initial/own-node join (no
// sleep, no reconnect) and the session-expiry shape (sleep first) without a
// live Keeper.
func TestRejoinWithRetry_FailsOpenAndStopsOnCancel(t *testing.T) {
	for _, tc := range []struct {
		name              string
		sleepFirst, recon bool
	}{
		{"initial/own-node join", false, false},
		{"session-expiry reconnect", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			le := &LeaderElection{
				myNode: "/click-dog/election/candidate-0000000001",
				mySeq:  "candidate-0000000001",
			}
			le.isLeader.Store(true) // had been leader before the disruption

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // already canceled → no Keeper I/O, returns promptly

			if le.rejoinWithRetry(ctx, tc.sleepFirst, tc.recon) {
				t.Error("rejoinWithRetry should return false when ctx is already canceled")
			}
			if le.Joined() {
				t.Error("candidate state must be cleared (Joined()==false) so the gate fails open during retry")
			}
			if le.IsLeader() {
				t.Error("must be demoted during rejoin")
			}
		})
	}
}

// TestRecoverLostSession_FailsOpen pins the watch-loop recovery contract: when
// getWatchTarget/ExistsW errors (or our candidate vanishes from the list), the
// loop must clear candidate state so Joined()==false (gate exports) and demote
// the stale leader bit, instead of retrying with stale state and stranding a
// follower gated-off or a stale leader still exporting. An already-canceled
// ctx exercises the state-clear without Keeper I/O.
func TestRecoverLostSession_FailsOpen(t *testing.T) {
	le := &LeaderElection{
		myNode: "/click-dog/election/candidate-0000000001",
		mySeq:  "candidate-0000000001",
	}
	le.isLeader.Store(true) // a former leader hitting a watch-path error

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if le.recoverLostSession(ctx, "Error watching own node", context.Canceled) {
		t.Error("recoverLostSession should return false when ctx is canceled")
	}
	if le.Joined() {
		t.Error("candidate state must be cleared (Joined()==false) so the gate exports during recovery")
	}
	if le.IsLeader() {
		t.Error("stale leader bit must be cleared on lost-session recovery")
	}
}

// TestRejoinWithRetry_BailsWhenClosed guards the graceful-shutdown contract:
// once Resign() has marked the election closed, a watch-path/closed-connection
// error routed into rejoinWithRetry must NOT reconnect or create a fresh
// candidate (which would resurrect a resigned instance, and which a later
// no-op Resign would never clean up). It bails before touching state — even
// with a live ctx — leaving Resign as the sole owner of shutdown cleanup.
func TestRejoinWithRetry_BailsWhenClosed(t *testing.T) {
	le := &LeaderElection{
		myNode: "/click-dog/election/candidate-0000000001",
		mySeq:  "candidate-0000000001",
		closed: true,
	}

	if le.rejoinWithRetry(context.Background(), false, false) {
		t.Error("rejoinWithRetry must return false (no rejoin) once closed")
	}
	// Bailed before handleDemotion/dropCandidate, so candidate state is left
	// exactly as Resign left it — rejoin never ran.
	if !le.Joined() {
		t.Error("rejoinWithRetry must not alter candidate state when closed (Resign owns cleanup)")
	}
}

// TestBackoffSleep_StopsOnCancel pins the ctx-aware wait: a canceled ctx must
// abort the backoff immediately (false) so SIGTERM doesn't wait out a 30s delay.
func TestBackoffSleep_StopsOnCancel(t *testing.T) {
	le := &LeaderElection{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if le.backoffSleep(ctx) {
		t.Error("backoffSleep must return false when ctx is canceled")
	}
}

// TestFailOpen_NoOpWhenClosed ensures failOpen does not interfere with the
// graceful shutdown path: once Resign() has marked the election closed, it
// owns cleanup and failOpen must be a no-op.
func TestFailOpen_NoOpWhenClosed(t *testing.T) {
	le := &LeaderElection{myNode: "/click-dog/election/candidate-0000000001", closed: true}

	le.failOpen()

	if !le.Joined() {
		t.Error("failOpen on a closed election must be a no-op (Resign owns cleanup)")
	}
}

// TestRouteSessionEvent pins the le.events dispatch the watch loop depends on
// (issue #243.3). The existing failopen tests drive handleSessionExpiry's state
// transition directly; nothing covered the event→handler routing — whether a
// given event even reaches handleSessionExpiry. routeSessionEvent makes that
// decision unit-testable without a live Keeper: only a genuine session expiry
// must trigger reconnect-and-rejoin; every other event the library delivers here
// must be a no-op that leaves election state untouched.
func TestRouteSessionEvent(t *testing.T) {
	t.Run("session expiry routes into rejoin and fails open", func(t *testing.T) {
		le := &LeaderElection{
			myNode: "/click-dog/election/candidate-0000000001",
			mySeq:  "candidate-0000000001",
		}
		le.isLeader.Store(true) // had been leader when the session expired

		// rejoinWithRetry clears candidate state up front, then bails on the
		// already-canceled ctx before any Keeper I/O — so this exercises the
		// expiry→rejoin route without a live connection.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if le.routeSessionEvent(ctx, zk.Event{Type: zk.EventSession, State: zk.StateExpired}) {
			t.Error("expiry event must return false when ctx is canceled during reconnect")
		}
		if le.Joined() {
			t.Error("expiry must drop candidate state (Joined()==false) so the gate fails open")
		}
		if le.IsLeader() {
			t.Error("expiry must demote the stale leader bit")
		}
	})

	// Everything that is not a session-expiry event must be ignored. A live ctx
	// here is deliberate: had any of these wrongly routed into handleSessionExpiry
	// it would have cleared state and blocked on the reconnect backoff rather than
	// returning at once.
	for _, tc := range []struct {
		name string
		evt  zk.Event
	}{
		{"reconnect (StateConnected)", zk.Event{Type: zk.EventSession, State: zk.StateConnected}},
		{"transient disconnect", zk.Event{Type: zk.EventSession, State: zk.StateDisconnected}},
		{"node-deleted watch notification", zk.Event{Type: zk.EventNodeDeleted}},
	} {
		t.Run("non-expiry is a no-op: "+tc.name, func(t *testing.T) {
			le := &LeaderElection{
				myNode: "/click-dog/election/candidate-0000000001",
				mySeq:  "candidate-0000000001",
			}
			le.isLeader.Store(true)

			if !le.routeSessionEvent(context.Background(), tc.evt) {
				t.Error("non-expiry event must return true (keep looping)")
			}
			if !le.Joined() || !le.IsLeader() {
				t.Error("non-expiry event must leave election state untouched")
			}
		})
	}
}
