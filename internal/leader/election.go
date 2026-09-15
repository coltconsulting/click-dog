package leader

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-zookeeper/zk"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
)

// maxCandidates is the sanity-check ceiling for the number of candidate znodes.
// If the election sees more candidates than this, it logs a warning — likely a
// znode leak from sessions that didn't clean up their ephemeral nodes.
const maxCandidates = 10

// reconnectBackoff defines the delays between successive reconnection attempts
// to prevent hammering Keeper when the connection is flapping. Each step is
// jittered at use (see jitterDuration) so instances are decorrelated.
var reconnectBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

func keeperTLSDialer() zk.Dialer {
	return func(network, address string, timeout time.Duration) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: timeout}
		return tls.DialWithDialer(dialer, network, address, &tls.Config{
			MinVersion: tls.VersionTLS12,
		})
	}
}

// jitterDuration applies "equal jitter" (half fixed, half random) to a backoff
// step, returning a value in [d/2, d]. A fixed ladder shared across instances
// doesn't actually decorrelate them: a Keeper flap that drops every instance at
// once would have them all walk 1→2→5→10→30s in lockstep and reconnect in a
// synchronized storm when Keeper returns. Jittering each step spreads the
// reconnections out. See issue #223.
func jitterDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// LeaderElection manages leader election via ClickHouse Keeper using the
// ZooKeeper-compatible protocol. Each instance creates an ephemeral sequential
// znode; the lowest sequence number is the leader. When the leader dies, its
// session expires, the ephemeral znode is deleted, and the next candidate
// auto-promotes.
//
// Split-brain safety: during a Keeper partition, multiple instances may briefly
// believe they are leader. This means duplicate span exports are possible.
// Stable span identities make repeat delivery observable, but downstream
// systems are not assumed to collapse it. This is by design — availability is
// preferred over strict single-leader guarantees during transient network
// events.
type LeaderElection struct {
	config config.LeaderElectionConfig

	conn   *zk.Conn
	events <-chan zk.Event

	basePath string // e.g. /click-dog/election
	myNode   string // full path of our ephemeral sequential znode
	mySeq    string // just the sequence suffix for comparison

	isLeader   atomic.Bool
	epoch      atomic.Uint64 // monotonic fencing token; incremented on each promotion
	onPromoted func()        // called when this instance becomes leader
	onDemoted  func()        // called when this instance loses leadership

	reconnectAttempts int // consecutive reconnect failures, indexes into reconnectBackoff

	// mu guards conn, events, myNode, mySeq, and closed. Use RLock when
	// reading le.conn to call zk methods (ExistsW, Children, etc.) — this
	// prevents a concurrent reconnect() from swapping le.conn mid-call.
	// Use full Lock when writing any protected field (reconnect, Resign,
	// createCandidate).
	mu     sync.RWMutex
	closed bool

	// ready is closed once Run has finished its initial join and first
	// leadership check, or given up because ctx was canceled. AwaitJoin waits
	// on it; readyOnce makes the close idempotent across Run's exit paths.
	ready     chan struct{}
	readyOnce sync.Once
}

// NewLeaderElection connects to Keeper and joins the election.
// onPromoted is called when this instance becomes leader.
// onDemoted is called when this instance loses leadership.
//
// WARNING: Callbacks must not call Resign() or any other LeaderElection method
// that acquires le.mu — doing so will deadlock. Callbacks are invoked while
// the election loop is running; use them only for lightweight signalling.
func NewLeaderElection(cfg config.LeaderElectionConfig, onPromoted, onDemoted func()) (*LeaderElection, error) {
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("ha.keeper.hosts must not be empty")
	}

	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = 10
	}

	if cfg.BasePath == "" {
		cfg.BasePath = "/click-dog/election"
	}

	le := &LeaderElection{
		config:     cfg,
		basePath:   cfg.BasePath,
		onPromoted: onPromoted,
		onDemoted:  onDemoted,
		ready:      make(chan struct{}),
	}

	if err := le.connect(); err != nil {
		return nil, err
	}

	return le, nil
}

// connect establishes a new connection to Keeper, replacing any existing one.
// Must be called with le.mu held, or during init before concurrent access.
func (le *LeaderElection) connect() error {
	sessionTimeout := time.Duration(le.config.SessionTimeout) * time.Second

	var conn *zk.Conn
	var events <-chan zk.Event
	var err error
	// The client's own logger defaults to the stdlib logger on stderr, outside
	// clicklog's level and format. During a Keeper partition it emitted over
	// 5,000 lines per instance in two minutes ("re-submitting `0` credentials
	// after reconnect" alone a thousand times), burying the election's own
	// WARN lines. Route it through clicklog at debug level instead.
	logger := zk.WithLogger(keeperClientLogger{})
	if le.config.Secure {
		conn, events, err = zk.Connect(le.config.Hosts, sessionTimeout, logger, zk.WithDialer(keeperTLSDialer()))
	} else {
		conn, events, err = zk.Connect(le.config.Hosts, sessionTimeout, logger)
	}
	if err != nil {
		return fmt.Errorf("failed to connect to Keeper: %w", err)
	}

	// Add digest authentication if configured
	if le.config.AuthUser != "" {
		auth := le.config.AuthUser + ":" + le.config.AuthPassword
		if err := conn.AddAuth("digest", []byte(auth)); err != nil {
			conn.Close()
			return fmt.Errorf("failed to authenticate with Keeper: %w", err)
		}
	}

	le.conn = conn
	le.events = events
	return nil
}

// keeperClientLogger adapts the ZooKeeper client's Printf-style logger to
// clicklog so its connection chatter lands at debug level in the service log
// rather than unformatted on stderr.
type keeperClientLogger struct{}

func (keeperClientLogger) Printf(format string, args ...interface{}) {
	clicklog.Debug("keeper client: "+format, args...)
}

// reconnect closes the old connection and establishes a fresh one.
// Must be called with le.mu held.
func (le *LeaderElection) reconnect() error {
	if le.conn != nil {
		le.conn.Close()
	}
	le.myNode = ""
	le.mySeq = ""
	return le.connect()
}

// acl returns the appropriate ACL for znodes. If auth credentials are
// configured, uses AuthACL (restricts to authenticated sessions). Otherwise
// uses WorldACL (any client).
func (le *LeaderElection) acl() []zk.ACL {
	if le.config.AuthUser != "" {
		return zk.AuthACL(zk.PermAll)
	}
	return zk.WorldACL(zk.PermAll)
}

// Run participates in the election. It blocks until ctx is canceled.
// It creates the base path if needed, creates an ephemeral sequential znode,
// then watches for leadership changes.
func (le *LeaderElection) Run(ctx context.Context) (err error) {
	// Backstop: join/rejoin now retry internally until ctx is canceled (see
	// rejoinWithRetry), so Run no longer exits on transient Keeper errors — it
	// returns nil on clean shutdown (Resign handles cleanup). But should any
	// future path return a non-nil error, drop election state so the export
	// gate fails OPEN rather than stranding this reader (and possibly the whole
	// cluster) in permanent standby. A dead election goroutine must never leave
	// Joined()==true with IsLeader()==false.
	defer func() {
		if err != nil {
			le.failOpen()
		}
	}()
	// Release AwaitJoin on every exit path, not only the happy one.
	defer le.signalReady()

	// Initial join, retrying through transient Keeper errors until ctx is
	// canceled. Candidate state stays cleared during the retry window so the
	// export gate fails OPEN (Joined()==false ⇒ this reader keeps exporting),
	// yielding a bounded duplicate window that recovers to a single exporter
	// once the join succeeds — never a dead goroutine or a permanent standby.
	if !le.rejoinWithRetry(ctx, false, false) {
		return nil // ctx canceled before we could join — clean shutdown
	}

	le.mu.RLock()
	node := le.myNode
	le.mu.RUnlock()
	clicklog.Info("Joined leader election at %s (node: %s)", le.basePath, node)

	// Initial leadership check is best-effort: the watch loop re-checks every
	// iteration, so a transient error here must not kill the goroutine.
	if err := le.checkLeadership(); err != nil {
		clicklog.Warn("Initial leadership check failed (re-checked in watch loop): %v", err)
	}
	le.signalReady()

	// Watch loop
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Get the node we need to watch (predecessor or ourselves if leader).
		// A Children() connection/session error here — or our own candidate
		// gone from the list ("not found in candidates") — means we can no
		// longer trust our election state, so fail open and rejoin rather than
		// retry with stale candidate/leader state (see recoverLostSession).
		watchNode, err := le.getWatchTarget()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !le.recoverLostSession(ctx, "Error getting watch target", err) {
				return nil
			}
			continue
		}

		// Re-check leadership every iteration. When the loop restarts after
		// consuming a session event from le.events, the predecessor may have
		// been deleted between iterations. Without this check, we'd enter the
		// leader watch branch (watchNode=="") without ever calling onPromoted.
		if err := le.checkLeadership(); err != nil {
			clicklog.Warn("Error re-checking leadership: %v", err)
		}

		le.mu.RLock()
		myNode := le.myNode
		conn := le.conn
		le.mu.RUnlock()

		if watchNode == "" {
			// We're the leader, watch our own node for deletion (session loss)
			exists, _, watchCh, err := conn.ExistsW(myNode)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if !le.recoverLostSession(ctx, "Error watching own node", err) {
					return nil
				}
				continue
			}
			if !exists {
				// Our node disappeared (session expired) — re-join on the live
				// connection, retrying through transient errors so a flaky
				// Keeper can't strand us in a dead goroutine.
				clicklog.Warn("Our candidate node disappeared, re-joining election")
				if !le.rejoinWithRetry(ctx, false, false) {
					return nil // ctx canceled
				}
				continue
			}

			select {
			case <-ctx.Done():
				return nil
			case evt := <-watchCh:
				clicklog.Debug("Watch event on own node: %v", evt.Type)
				if err := le.checkLeadership(); err != nil {
					clicklog.Warn("Error checking leadership: %v", err)
				}
			case evt := <-le.events:
				if !le.routeSessionEvent(ctx, evt) {
					return nil // ctx canceled during reconnect
				}
			}
		} else {
			// Watch the predecessor node
			exists, _, watchCh, err := conn.ExistsW(watchNode)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if !le.recoverLostSession(ctx, "Error watching predecessor", err) {
					return nil
				}
				continue
			}
			if !exists {
				// Predecessor already gone, re-check immediately
				if err := le.checkLeadership(); err != nil {
					clicklog.Warn("Error checking leadership: %v", err)
				}
				continue
			}

			select {
			case <-ctx.Done():
				return nil
			case <-watchCh:
				// Predecessor changed, re-check
				if err := le.checkLeadership(); err != nil {
					clicklog.Warn("Error checking leadership: %v", err)
				}
			case evt := <-le.events:
				if !le.routeSessionEvent(ctx, evt) {
					return nil // ctx canceled during reconnect
				}
			}
		}
	}
}

// routeSessionEvent dispatches an event delivered on the Keeper connection's
// event channel (le.events) while the watch loop is blocked. Only a genuine
// session expiry forces reconnect-and-rejoin; every other event the library
// surfaces here (connection-state flaps, watch notifications) is a no-op — the
// watch loop re-checks leadership on its own next iteration. Returns false iff
// ctx was canceled during the reconnect, which Run maps to a clean nil exit;
// true means keep looping. Extracted from the two inline copies in Run so the
// event→rejoin wiring is unit-testable without a live Keeper.
func (le *LeaderElection) routeSessionEvent(ctx context.Context, evt zk.Event) bool {
	if evt.Type == zk.EventSession && evt.State == zk.StateExpired {
		return le.handleSessionExpiry(ctx)
	}
	return true
}

// handleSessionExpiry reconnects and re-joins the election after a session
// expiry event. It backs off before reconnecting (don't hammer a just-died
// Keeper) and retries until it rejoins or ctx is canceled. Returns false iff
// ctx was canceled, which the caller maps to a clean nil Run() exit.
func (le *LeaderElection) handleSessionExpiry(ctx context.Context) bool {
	clicklog.Warn("Keeper session expired, reconnecting and re-joining election")
	return le.rejoinWithRetry(ctx, true /* sleepFirst */, true /* reconnect */)
}

// recoverLostSession fails open and rejoins after the watch loop hit an error
// it cannot distinguish from a lost Keeper session — getWatchTarget/ExistsW
// returning a connection/session error, or our own candidate gone from the
// candidate list. rejoinWithRetry drops candidate state first, so Joined()
// reports false and the export gate stays OPEN throughout recovery: a watch-
// path error can therefore never strand a follower gated-off, nor leave a
// stale leader still believing it leads. Returns false iff ctx was canceled.
func (le *LeaderElection) recoverLostSession(ctx context.Context, what string, cause error) bool {
	clicklog.Warn("%s (%v) — treating as lost Keeper session; failing open and rejoining", what, cause)
	return le.rejoinWithRetry(ctx, true /* sleepFirst */, true /* reconnect */)
}

// rejoinWithRetry (re-)establishes this instance's candidate node, retrying
// with exponential backoff until it succeeds or ctx is canceled. It demotes
// and clears local candidate identity first, so for the ENTIRE retry window
// Joined() reports false and the export gate fails OPEN (Joined()==false ⇒ this
// reader keeps exporting). A Keeper outage therefore produces a *bounded*
// duplicate window that recovers to a single exporter once the join succeeds —
// never a dead goroutine, a permanent standby, or a permanently-doubled export.
//
//   - sleepFirst: back off before the first attempt (session expiry — avoid
//     reconnecting into a just-expired/flapping Keeper).
//   - reconnect: replace the (presumed dead) connection before re-joining. The
//     initial and own-node re-joins reuse the live connection but escalate to a
//     reconnect if a join step fails.
//
// Returns true once rejoined, false if ctx was canceled first.
func (le *LeaderElection) rejoinWithRetry(ctx context.Context, sleepFirst, reconnect bool) bool {
	// Do not rejoin once Resign() has marked us closed. Resign sets closed,
	// deletes the candidate, then closes the connection — and that close
	// surfaces to Run()'s watch loop as an error that routes here. Without this
	// guard we would reconnect and create a fresh candidate after a graceful
	// resignation; worse, closed stays true so a later Resign is a no-op and
	// would never clean the rejoined node up. Bailing before touching state
	// leaves Resign as the sole owner of shutdown cleanup.
	if le.isClosed() {
		return false
	}
	le.handleDemotion()
	le.dropCandidate()

	backoff := sleepFirst
	for {
		if backoff && !le.backoffSleep(ctx) {
			return false
		}
		backoff = true // every attempt after the first backs off

		if ctx.Err() != nil {
			return false
		}
		// Re-check after the backoff sleep: Resign may have run during the wait.
		if le.isClosed() {
			return false
		}

		if reconnect {
			// Closure so the unlock is deferred — a panic in reconnect() must
			// not leave le.mu permanently held (matches Resign/failOpen).
			err := func() error {
				le.mu.Lock()
				defer le.mu.Unlock()
				return le.reconnect()
			}()
			if err != nil {
				clicklog.Warn("Keeper reconnect failed during rejoin: %v; retrying", err)
				continue
			}
		}

		if err := le.ensureBasePath(); err != nil {
			clicklog.Warn("ensureBasePath failed during rejoin: %v; retrying", err)
			reconnect = true // the connection itself may be the problem
			continue
		}
		if err := le.createCandidate(); err != nil {
			clicklog.Warn("createCandidate failed during rejoin: %v; retrying", err)
			reconnect = true
			continue
		}

		le.reconnectAttempts = 0 // rejoined cleanly — reset the backoff ladder
		return true
	}
}

// backoffSleep waits the next step of the reconnect backoff ladder (capped at
// the last entry for steady state) and advances the attempt counter. Returns
// false if ctx is canceled during the wait, so SIGTERM doesn't have to sleep
// out a 30s delay mid-reconnect.
func (le *LeaderElection) backoffSleep(ctx context.Context) bool {
	idx := le.reconnectAttempts
	if idx >= len(reconnectBackoff) {
		idx = len(reconnectBackoff) - 1
	}
	delay := jitterDuration(reconnectBackoff[idx])
	le.reconnectAttempts++
	clicklog.Info("Waiting %v before Keeper retry (attempt %d)", delay, le.reconnectAttempts)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// IsLeader returns true if this instance is the current leader.
func (le *LeaderElection) IsLeader() bool {
	return le.isLeader.Load()
}

// signalReady marks the initial join attempt as finished. Safe to call more
// than once and on a zero-value election (tests), where there is no channel.
func (le *LeaderElection) signalReady() {
	if le.ready == nil {
		return
	}
	le.readyOnce.Do(func() { close(le.ready) })
}

// AwaitJoin blocks until the initial join attempt has completed — the
// candidate znode exists and leadership has been checked once — or ctx is
// canceled or timeout elapses, and reports whether the instance is joined.
//
// A cluster reader runs its startup cycle only after this. Run joins in its
// own goroutine, so without the wait a standby's first cycle sees
// Joined()==false, fails open, and exports one full lookback window that the
// leader is already exporting: a duplicate burst on every restart, and a
// nonzero last-success gauge on an instance the docs promise stays at zero.
// An unreachable Keeper keeps the wait bounded by timeout, so startup still
// fails open rather than stalling.
func (le *LeaderElection) AwaitJoin(ctx context.Context, timeout time.Duration) bool {
	if le.ready == nil {
		return le.Joined()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-le.ready:
		return le.Joined()
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// Joined reports whether this instance currently holds a candidate znode —
// i.e. Run() has joined the election and not since lost its session. It is
// false during the initial Keeper dial/join window and throughout a
// session-expiry reconnect: createCandidate sets myNode; dropCandidate clears
// it at the start of session-expiry handling (before the backoff sleep), and
// createCandidate re-sets it after the reconnect succeeds.
//
// The export gate keys leadership on this rather than IsLeader() alone: a
// not-yet-joined instance must keep exporting (fail-open), since isLeader's
// zero value is false and would otherwise stall a reader that has no live
// election to defer to.
func (le *LeaderElection) Joined() bool {
	le.mu.RLock()
	defer le.mu.RUnlock()
	return le.myNode != ""
}

// dropCandidate clears this instance's local candidate identity (myNode/mySeq)
// so Joined() reports false. Called on session expiry before the reconnect
// backoff so the export gate fails OPEN during the outage instead of treating
// stale candidate state as a standby. The ephemeral znode itself is already
// gone with the expired Keeper session, so there is nothing to delete.
func (le *LeaderElection) dropCandidate() {
	le.mu.Lock()
	le.myNode = ""
	le.mySeq = ""
	le.mu.Unlock()
}

// isClosed reports whether Resign() has run (graceful shutdown). The rejoin
// path checks this so a closed-connection error during resignation can't
// resurrect a resigned candidate.
func (le *LeaderElection) isClosed() bool {
	le.mu.RLock()
	defer le.mu.RUnlock()
	return le.closed
}

// Epoch returns the current fencing token — a monotonically increasing counter
// that increments each time this instance is promoted to leader. Downstream
// systems can use this to reject stale-leader writes: if an operation arrives
// with an epoch lower than the last seen, the sender is a stale leader.
func (le *LeaderElection) Epoch() uint64 {
	return le.epoch.Load()
}

// failOpen drops election state after an unexpected Run() exit so the export
// gate stops treating this instance as a standby. A dead election goroutine
// must never leave a cluster reader permanently gated (Joined()==true,
// IsLeader()==false); worse, if it holds the lowest-seq znode the rest of the
// cluster defers to it and the whole export path stalls in skipped cycles
// until restart. We best-effort delete the candidate (so peers don't defer to
// a dead node) and clear myNode, so Joined() reports false and the gate fails
// OPEN — this reader resumes exporting until the process restarts and rejoins.
// No-op once closed (Resign already ran the graceful path).
func (le *LeaderElection) failOpen() {
	le.mu.Lock()
	defer le.mu.Unlock()

	if le.closed {
		return
	}
	le.handleDemotion()
	if le.myNode != "" {
		// Delete is best-effort: the znode is ephemeral and dies with the
		// session anyway; the critical part is clearing myNode so Joined()
		// reports false. Guard conn so the state-clear can't be blocked by a
		// nil/torn-down connection.
		if le.conn != nil {
			if err := le.conn.Delete(le.myNode, -1); err != nil && !errors.Is(err, zk.ErrNoNode) {
				clicklog.Warn("Error deleting candidate node on fail-open: %v", err)
			}
		}
		le.myNode = ""
		le.mySeq = "" // clear both, matching dropCandidate — no stale seq
	}
}

// Resign voluntarily gives up leadership by deleting our ephemeral node.
// Call this during graceful shutdown for immediate leadership transfer
// instead of waiting for session timeout.
func (le *LeaderElection) Resign() error {
	le.mu.Lock()
	defer le.mu.Unlock()

	if le.closed {
		return nil
	}
	le.closed = true

	le.handleDemotion()

	if le.myNode != "" {
		if err := le.conn.Delete(le.myNode, -1); err != nil && !errors.Is(err, zk.ErrNoNode) {
			clicklog.Warn("Error deleting candidate node on resign: %v", err)
		}
	}

	// myNode is left set intentionally. Unlike the fail-open paths, a resign is
	// a graceful shutdown: we want the export gate to stay in *skip* mode
	// (IsLeader()==false, Joined()==true ⇒ standby) for the brief teardown
	// window, not fail open and resume exports on an instance that is going
	// away. Run() exits right after, so the stale myNode is never consumed.
	le.conn.Close()
	clicklog.Info("Resigned from leader election")
	return nil
}

// ensureBasePath creates the base znode path recursively if it doesn't exist.
func (le *LeaderElection) ensureBasePath() error {
	le.mu.RLock()
	conn := le.conn
	le.mu.RUnlock()

	nodeACL := le.acl()
	parts := strings.Split(strings.TrimPrefix(le.basePath, "/"), "/")
	current := ""
	for _, part := range parts {
		current = current + "/" + part
		exists, _, err := conn.Exists(current)
		if err != nil {
			return fmt.Errorf("error checking path %s: %w", current, err)
		}
		if !exists {
			_, err := conn.Create(current, nil, 0, nodeACL)
			if err != nil && !errors.Is(err, zk.ErrNodeExists) {
				return fmt.Errorf("error creating path %s: %w", current, err)
			}
		}
	}
	return nil
}

// createCandidate creates an ephemeral sequential znode for this instance.
func (le *LeaderElection) createCandidate() error {
	le.mu.RLock()
	conn := le.conn
	le.mu.RUnlock()

	nodePath := le.basePath + "/candidate-"
	created, err := conn.CreateProtectedEphemeralSequential(nodePath, nil, le.acl())
	if err != nil {
		return fmt.Errorf("failed to create ephemeral sequential node: %w", err)
	}

	le.mu.Lock()
	le.myNode = created
	le.mySeq = path.Base(created)
	le.mu.Unlock()

	clicklog.Debug("Created candidate node: %s", created)
	return nil
}

// znodeSeq extracts the 10-digit sequence number suffix from a znode name.
// CreateProtectedEphemeralSequential creates nodes like "_c_<GUID>-candidate-0000000001";
// the sequence number is always the last 10 characters.
func znodeSeq(name string) string {
	if len(name) >= 10 {
		return name[len(name)-10:]
	}
	return name
}

// sortBySeq sorts znode child names by their sequence number suffix,
// which is the correct ordering for leader election.
func sortBySeq(children []string) {
	sort.Slice(children, func(i, j int) bool {
		return znodeSeq(children[i]) < znodeSeq(children[j])
	})
}

// checkLeadership determines if this instance is the leader by comparing
// sequence numbers of all candidates.
func (le *LeaderElection) checkLeadership() error {
	le.mu.RLock()
	conn := le.conn
	le.mu.RUnlock()

	children, _, err := conn.Children(le.basePath)
	if err != nil {
		return fmt.Errorf("failed to list candidates: %w", err)
	}

	if len(children) == 0 {
		return fmt.Errorf("no candidates found (should not happen)")
	}

	if len(children) > maxCandidates {
		clicklog.Warn("Unexpected number of election candidates: %d (max expected: %d) — possible znode leak from unclean shutdowns", len(children), maxCandidates)
	}

	sortBySeq(children)

	le.mu.Lock()
	mySeq := le.mySeq
	le.mu.Unlock()

	// The lowest sequence number is the leader
	wasLeader := le.isLeader.Load()
	myNum := znodeSeq(mySeq)
	isNowLeader := znodeSeq(children[0]) == myNum

	le.isLeader.Store(isNowLeader)

	if isNowLeader && !wasLeader {
		newEpoch := le.epoch.Add(1)
		clicklog.Info("This instance is now the LEADER (%s, epoch=%d)", mySeq, newEpoch)
		safeCallback("onPromoted", le.onPromoted)
	} else if !isNowLeader && wasLeader {
		clicklog.Info("This instance is no longer the leader (leader is %s, we are %s)", children[0], mySeq)
		le.handleDemotion()
	}

	if isNowLeader {
		clicklog.Debug("Leader status confirmed (%d candidates)", len(children))
	} else {
		clicklog.Debug("Follower status (%d candidates, leader: %s, we: %s)", len(children), children[0], mySeq)
	}

	return nil
}

// getWatchTarget returns the path of the node to watch. If we're the leader,
// returns "" (watch ourselves). Otherwise returns the predecessor node.
func (le *LeaderElection) getWatchTarget() (string, error) {
	le.mu.RLock()
	conn := le.conn
	le.mu.RUnlock()

	children, _, err := conn.Children(le.basePath)
	if err != nil {
		return "", fmt.Errorf("failed to list candidates: %w", err)
	}

	sortBySeq(children)

	le.mu.Lock()
	mySeq := le.mySeq
	le.mu.Unlock()

	// Find our position by sequence number
	myNum := znodeSeq(mySeq)
	myIdx := -1
	for i, child := range children {
		if child == mySeq || znodeSeq(child) == myNum {
			myIdx = i
			break
		}
	}

	if myIdx < 0 {
		return "", fmt.Errorf("our node %s not found in candidates", mySeq)
	}

	if myIdx == 0 {
		// We're the leader
		return "", nil
	}

	// Watch the predecessor
	predecessor := le.basePath + "/" + children[myIdx-1]
	return predecessor, nil
}

// safeCallback invokes fn with panic recovery so that a misbehaving callback
// does not crash the election goroutine or leave the instance in a broken state.
func safeCallback(name string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			clicklog.Error("Panic in %s callback (recovered): %v", name, r)
		}
	}()
	fn()
}

// handleDemotion handles the transition from leader to follower. Swap makes the
// leader→follower flip atomic so that when the Run loop and a shutdown Resign
// race (both observe isLeader==true), exactly one of them fires onDemoted.
func (le *LeaderElection) handleDemotion() {
	if le.isLeader.Swap(false) {
		safeCallback("onDemoted", le.onDemoted)
	}
}

// flushPath returns the path to the ephemeral flush-request znode.
func (le *LeaderElection) flushPath() string {
	return le.basePath + "/flush"
}

// WatchFlush polls for a flush-request znode and sends on flushChan when found.
// Only the leader acts on it — deletes the znode and triggers the flush.
// Runs until ctx is canceled. Best-effort: errors are logged and retried.
func (le *LeaderElection) WatchFlush(ctx context.Context, flushChan chan<- struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !le.isLeader.Load() {
				continue
			}

			le.mu.RLock()
			conn := le.conn
			le.mu.RUnlock()

			if conn == nil {
				continue
			}

			exists, _, err := conn.Exists(le.flushPath())
			if err != nil || !exists {
				continue
			}

			clicklog.Info("Flush request found in Keeper — triggering cycle")
			if err := conn.Delete(le.flushPath(), -1); err != nil {
				clicklog.Warn("Failed to delete flush znode: %v", err)
			}

			select {
			case flushChan <- struct{}{}:
			default:
			}
		}
	}
}

// RequestFlush creates an ephemeral flush-request znode in Keeper.
// The leader will pick it up and run an immediate export cycle.
// This is a standalone operation — connects to Keeper, writes, disconnects.
func RequestFlush(cfg config.LeaderElectionConfig) error {
	if len(cfg.Hosts) == 0 {
		return fmt.Errorf("ha.keeper.hosts must not be empty")
	}
	if cfg.BasePath == "" {
		cfg.BasePath = "/click-dog/election"
	}

	sessionTimeout := time.Duration(cfg.SessionTimeout) * time.Second
	if sessionTimeout <= 0 {
		sessionTimeout = 10 * time.Second
	}

	var conn *zk.Conn
	var err error
	if cfg.Secure {
		conn, _, err = zk.Connect(cfg.Hosts, sessionTimeout,
			zk.WithLogInfo(false),
			zk.WithDialer(keeperTLSDialer()))
	} else {
		conn, _, err = zk.Connect(cfg.Hosts, sessionTimeout,
			zk.WithLogInfo(false))
	}
	if err != nil {
		return fmt.Errorf("failed to connect to Keeper: %w", err)
	}
	defer conn.Close()

	flushPath := cfg.BasePath + "/flush"

	// Use CreateProtectedEphemeralSequential? No — just a plain ephemeral node.
	// If it already exists, that's fine — a flush is already pending.
	var acl []zk.ACL
	if cfg.AuthUser != "" {
		acl = zk.DigestACL(zk.PermAll, cfg.AuthUser, cfg.AuthPassword)
		if err := conn.AddAuth("digest", []byte(cfg.AuthUser+":"+cfg.AuthPassword)); err != nil {
			return fmt.Errorf("keeper auth failed: %w", err)
		}
	} else {
		acl = zk.WorldACL(zk.PermAll)
	}

	_, err = conn.Create(flushPath, nil, zk.FlagEphemeral, acl)
	if err != nil {
		if errors.Is(err, zk.ErrNodeExists) {
			return nil // flush already requested
		}
		return fmt.Errorf("failed to create flush znode: %w", err)
	}

	// Keep connection alive briefly so the ephemeral node persists
	// long enough for the leader to see it (polls every 2s).
	// The caller (cmd_flush.go) prints a message before calling us.
	time.Sleep(5 * time.Second)
	return nil
}
