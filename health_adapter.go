package main

import (
	"sync/atomic"

	"github.com/coltconsulting/click-dog/internal/health"
	"github.com/coltconsulting/click-dog/internal/leader"
	"github.com/coltconsulting/click-dog/internal/metrics"
)

// metricsCycleSource adapts *metrics.Metrics to health.CycleSource.
// The two packages keep independent Snapshot types to avoid a
// cross-import; this forwards a single atomic Snapshot call into the
// health-side shape.
//
// WARNING: the copy below is manual and field-by-field. Adding a field
// to either metrics.Snapshot or health.Snapshot requires updating the
// other struct AND this function. A missed field is a silent data-drop
// at the boundary, not a compile error.
type metricsCycleSource struct {
	m *metrics.Metrics
}

// clusterSource adapts leader election + the configured static peer list
// to health.ClusterSource.
//
// The election pointer is loaded atomically because the health server is
// constructed (and its handlers registered) BEFORE leader election is
// started — runScheduledMode builds the LeaderElection and stores it via
// SetElection. Until that store happens, IsLeader follows haEnabled:
// HA-disabled deployments are always-leader; HA-enabled deployments are
// not-yet-leader so /clusterz returns 404 during the startup window.
//
// The peer list is captured at construction time. Phase 2 of #19 swaps
// the static list for a Keeper-derived view; the interface stays the
// same, so the change there is a single-file edit.
type clusterSource struct {
	haEnabled bool
	peers     []string
	election  atomic.Pointer[leader.LeaderElection]
}

func newClusterSource(haEnabled bool, peers []string) *clusterSource {
	return &clusterSource{haEnabled: haEnabled, peers: peers}
}

// SetElection swaps the live election in. Called once from runScheduledMode
// after leader.NewLeaderElection succeeds. Safe to call from any goroutine.
func (c *clusterSource) SetElection(el *leader.LeaderElection) {
	c.election.Store(el)
}

func (c *clusterSource) IsLeader() bool {
	if el := c.election.Load(); el != nil {
		return el.IsLeader()
	}
	// HA disabled → single-node, always leader.
	// HA enabled but election not yet wired → not leader; better to 404
	// briefly during startup than to serve a "leader" view that won't
	// match what the cluster eventually agrees on.
	return !c.haEnabled
}

// Peers returns a defensive copy of the configured peer list. Nothing in
// the current call path mutates the returned slice, but handing out the
// internal backing array invites future code (e.g. an in-place sort or
// dedupe in the fanout path) to corrupt the source-of-truth list. The
// allocation cost is paid at most once per cache miss — not per request
// — so the safety win comes essentially free.
func (c *clusterSource) Peers() []string {
	out := make([]string, len(c.peers))
	copy(out, c.peers)
	return out
}

func (a metricsCycleSource) Snapshot() health.Snapshot {
	s := a.m.Snapshot()
	return health.Snapshot{
		LastCycle: health.CycleSnapshot{
			Exported:   s.LastCycle.Exported,
			Filtered:   s.LastCycle.Filtered,
			Duplicates: s.LastCycle.Duplicates,
			DurationMs: s.LastCycle.DurationMs,
			Err:        s.LastCycle.Err,
			Skipped:    s.LastCycle.Skipped,
			SkipReason: s.LastCycle.SkipReason,
			At:         s.LastCycle.At,
		},
		HaveLastCycle:       s.HaveLastCycle,
		CircuitBreakerState: s.CircuitBreakerState,
		BackoffIntervalSecs: s.BackoffIntervalSecs,
		Leader:              s.Leader,
		UpSince:             s.UpSince,
		TopologyWarning:     s.TopologyWarning,
	}
}
