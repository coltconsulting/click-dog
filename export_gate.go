package main

// leadership is the narrow slice of *leader.LeaderElection the export gate
// needs. Keeping it an interface decouples the gate from the concrete election
// type and makes the decision table trivially fakeable in tests.
type leadership interface {
	Joined() bool
	IsLeader() bool
}

// newLeaderGate returns the cluster-mode export gate, or nil for the sidecar /
// single-node case (clusterMode false → always export, gate is a no-op).
//
// The rule is shouldExport = !clusterMode || !electionActive || IsLeader():
//
//   - clusterMode is cfg.ClickHouse.UseClusterQueries. Off → nil gate.
//   - el is nil when no election was started: a Keeper-less cluster reader
//     (Option A, backward-compat) or an election-constructor failure
//     (standalone fallback). Either way the reader always exports.
//   - electionActive is el.Joined(): false during the Keeper dial/join window
//     and during a session-expiry reconnect, so the instance exports then too
//     (fail-open, no startup dead zone). Keying on Joined() rather than a bare
//     IsLeader() avoids the zero-value trap where isLeader is false before any
//     election ran.
//   - Once joined, only the leader exports; followers stand by.
//
// The guarantee is "no steady-state duplication," not "never duplicates":
// partitions and the standalone fallback leave bounded duplicate windows.
// Stable IDs identify repeats, but downstream collapse is backend-specific.
func newLeaderGate(clusterMode bool, el leadership) func() bool {
	if !clusterMode {
		return nil
	}
	return func() bool {
		if el == nil || !el.Joined() {
			return true
		}
		return el.IsLeader()
	}
}
