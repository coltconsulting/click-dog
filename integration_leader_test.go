//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/leader"
)

func TestIntegration_LeaderElection_SingleNode(t *testing.T) {
	waitForKeeper(t, 30*time.Second)

	basePath := uniqueBasePath(t)
	promoted := make(chan struct{}, 1)

	le, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 30,
			BasePath:       basePath,
		},
		func() { promoted <- struct{}{} },
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go le.Run(ctx)

	// Should become leader since it's the only candidate
	select {
	case <-promoted:
		// ok
	case <-time.After(10 * time.Second):
		t.Fatal("Single node did not become leader within 10s")
	}

	if !le.IsLeader() {
		t.Error("Single node should be leader")
	}

	cancel()
	le.Resign()
}

func TestIntegration_LeaderElection_TwoNodes(t *testing.T) {
	waitForKeeper(t, 30*time.Second)

	basePath := uniqueBasePath(t)

	le1, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 30,
			BasePath:       basePath,
		},
		func() {},
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection (1) failed: %v", err)
	}

	le2, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 30,
			BasePath:       basePath,
		},
		func() {},
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection (2) failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	go le1.Run(ctx)
	go le2.Run(ctx)

	elections := []*leader.LeaderElection{le1, le2}
	leaderIdx := waitForExactlyOneLeader(t, 15*time.Second, elections)

	if err := elections[leaderIdx].Resign(); err != nil {
		t.Fatalf("leader resign failed: %v", err)
	}
	followerIdx := 1 - leaderIdx
	if !pollUntil(20*time.Second, elections[followerIdx].IsLeader) {
		t.Fatalf("node %d did not promote after node %d resigned", followerIdx, leaderIdx)
	}
	elections[followerIdx].Resign()

	cancel()
}

func TestIntegration_LeaderElection_ThreeNodes(t *testing.T) {
	waitForKeeper(t, 30*time.Second)

	basePath := uniqueBasePath(t)

	type nodeInfo struct {
		le *leader.LeaderElection
	}

	nodes := make([]nodeInfo, 3)
	for i := range nodes {
		le, err := leader.NewLeaderElection(
			config.LeaderElectionConfig{
				Hosts:          integrationAllKeeperAddrs(),
				SessionTimeout: 30,
				BasePath:       basePath,
			},
			func() {},
			func() {},
		)
		if err != nil {
			t.Fatalf("NewLeaderElection (%d) failed: %v", i, err)
		}
		nodes[i] = nodeInfo{le: le}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, n := range nodes {
		go n.le.Run(ctx)
	}

	elections := make([]*leader.LeaderElection, len(nodes))
	for i := range nodes {
		elections[i] = nodes[i].le
	}
	leaderIdx := waitForExactlyOneLeader(t, 15*time.Second, elections)

	// Resign the leader and wait for a new one to emerge
	if err := nodes[leaderIdx].le.Resign(); err != nil {
		t.Fatalf("leader resign failed: %v", err)
	}
	if !pollUntil(20*time.Second, func() bool {
		count, idx := currentLeader(elections)
		return count == 1 && idx != leaderIdx
	}) {
		count, idx := currentLeader(elections)
		t.Fatalf("expected one replacement leader after node %d resigned; count=%d leader=%d", leaderIdx, count, idx)
	}

	cancel()
	for _, n := range nodes {
		n.le.Resign()
	}
}

func TestIntegration_LeaderElection_Resign(t *testing.T) {
	waitForKeeper(t, 30*time.Second)

	basePath := uniqueBasePath(t)
	promoted := make(chan struct{}, 1)

	le, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 30,
			BasePath:       basePath,
		},
		func() { promoted <- struct{}{} },
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go le.Run(ctx)

	select {
	case <-promoted:
	case <-time.After(10 * time.Second):
		t.Fatal("Did not become leader")
	}

	// Resign and verify
	le.Resign()

	if le.IsLeader() {
		t.Error("Should not be leader after Resign()")
	}

	// Resign again should be idempotent
	if err := le.Resign(); err != nil {
		t.Errorf("Second Resign() should not error, got: %v", err)
	}

	cancel()
}

func TestIntegration_LeaderElection_BasePath(t *testing.T) {
	waitForKeeper(t, 30*time.Second)

	// Deeply nested base path
	basePath := uniqueBasePath(t) + "/nested/deep"
	promoted := make(chan struct{}, 1)

	le, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 30,
			BasePath:       basePath,
		},
		func() { promoted <- struct{}{} },
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection with nested path failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go le.Run(ctx)
	select {
	case <-promoted:
	case <-time.After(10 * time.Second):
		t.Fatal("nested base-path candidate did not become leader")
	}
	if !le.IsLeader() {
		t.Error("nested base-path candidate should report leader after promotion")
	}

	cancel()
	le.Resign()
}

// keeperContainers are the integration ClickHouse nodes that co-host the Keeper
// ensemble; stopping all three takes Keeper fully offline (a "flap").
var keeperContainers = []string{"clickhouse-int-1", "clickhouse-int-2", "clickhouse-int-3"}

// pollUntil polls cond every 100ms until it returns true or timeout elapses.
// Returns the final cond() result so callers can assert on it.
func pollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

func currentLeader(elections []*leader.LeaderElection) (count, index int) {
	index = -1
	for i, election := range elections {
		if election.IsLeader() {
			count++
			index = i
		}
	}
	return count, index
}

func waitForExactlyOneLeader(t *testing.T, timeout time.Duration, elections []*leader.LeaderElection) int {
	t.Helper()
	if !pollUntil(timeout, func() bool {
		count, _ := currentLeader(elections)
		if count != 1 {
			return false
		}
		for _, election := range elections {
			if !election.Joined() {
				return false
			}
		}
		return true
	}) {
		count, index := currentLeader(elections)
		joined := 0
		for _, election := range elections {
			if election.Joined() {
				joined++
			}
		}
		t.Fatalf("expected all %d candidates joined with exactly one leader within %v; joined=%d leaders=%d leader=%d", len(elections), timeout, joined, count, index)
	}
	_, index := currentLeader(elections)
	return index
}

// restartKeeperEnsemble brings every Keeper node back and waits for readiness.
// Registered via t.Cleanup so a failed flap test can't leave Keeper down for
// the rest of the suite.
func restartKeeperEnsemble(t *testing.T) {
	t.Helper()
	for _, c := range keeperContainers {
		dockerStart(t, c)
	}
	waitForKeeper(t, 60*time.Second)
}

// TestIntegration_LeaderElection_KeeperFlapFailsOpenAndRecovers is the merge
// gate for the recovery hardening. It proves the two headline guarantees
// against a real Keeper outage:
//
//   - Fail OPEN during the outage: candidate state is dropped (Joined()==false,
//     so the export gate would export) and leadership is released, rather than
//     stranding a gated standby.
//   - Retry-until-canceled recovery: the SAME Run goroutine rejoins and leads
//     again once Keeper returns. With the pre-hardening code Run() returned on
//     the first rejoin error, so the node would never recover.
func TestIntegration_LeaderElection_KeeperFlapFailsOpenAndRecovers(t *testing.T) {
	waitForKeeper(t, 30*time.Second)
	basePath := uniqueBasePath(t)

	le, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 10,
			BasePath:       basePath,
		},
		func() {},
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer le.Resign()

	go le.Run(ctx)

	if !pollUntil(15*time.Second, le.IsLeader) {
		t.Fatal("single node did not become leader")
	}

	// Take Keeper fully offline; ensure it comes back even if we fail.
	ensembleRestored := false
	t.Cleanup(func() {
		if !ensembleRestored {
			restartKeeperEnsemble(t)
		}
	})
	for _, c := range keeperContainers {
		dockerStop(t, c)
	}

	// Fail open: the hardening drops candidate state and demotes during the
	// outage instead of holding stale Joined()==true.
	if !pollUntil(30*time.Second, func() bool { return !le.Joined() }) {
		t.Fatal("election did not fail open (Joined stayed true) during Keeper outage")
	}
	if le.IsLeader() {
		t.Error("should not report leader while Keeper is unreachable")
	}

	// Bring Keeper back; the same goroutine must recover to leader.
	for _, c := range keeperContainers {
		dockerStart(t, c)
	}
	waitForKeeper(t, 60*time.Second)
	ensembleRestored = true

	if !pollUntil(60*time.Second, le.IsLeader) {
		t.Fatal("election did not recover to leader after Keeper returned (Run goroutine did not rejoin)")
	}
	cancel()
}

// TestIntegration_LeaderElection_ResignDuringOutageDoesNotRejoin pins the
// Resign()/closed lifecycle against a real outage: a resign issued while Keeper
// is down marks the election closed, and the watch loop's closed-connection
// errors must NOT route into a rejoin. After Keeper returns the instance must
// stay out of the election even though its Run ctx is still alive.
func TestIntegration_LeaderElection_ResignDuringOutageDoesNotRejoin(t *testing.T) {
	waitForKeeper(t, 30*time.Second)
	basePath := uniqueBasePath(t)

	le, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 10,
			BasePath:       basePath,
		},
		func() {},
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- le.Run(ctx) }()

	if !pollUntil(15*time.Second, le.IsLeader) {
		t.Fatal("single node did not become leader")
	}

	ensembleRestored := false
	t.Cleanup(func() {
		if !ensembleRestored {
			restartKeeperEnsemble(t)
		}
	})
	for _, c := range keeperContainers {
		dockerStop(t, c)
	}

	// Wait until the outage has been observed before resigning. This keeps the
	// lifecycle under test (closed while Keeper is down) while avoiding a race
	// between fault injection and the assertion itself.
	if !pollUntil(30*time.Second, func() bool { return !le.Joined() }) {
		t.Fatal("election did not drop candidate state during Keeper outage")
	}
	if err := le.Resign(); err != nil {
		t.Fatalf("Resign during Keeper outage: %v", err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned an error after resign: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit after resign during Keeper outage")
	}

	for _, c := range keeperContainers {
		dockerStart(t, c)
	}
	waitForKeeper(t, 60*time.Second)
	ensembleRestored = true

	// Run has terminated, so recovery cannot resurrect this closed election.
	if le.IsLeader() {
		t.Fatal("resigned instance reported leader after Keeper returned")
	}
	cancel()
}
