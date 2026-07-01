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
	promoted1 := make(chan struct{}, 1)
	promoted2 := make(chan struct{}, 1)

	le1, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          integrationAllKeeperAddrs(),
			SessionTimeout: 30,
			BasePath:       basePath,
		},
		func() { promoted1 <- struct{}{} },
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
		func() { promoted2 <- struct{}{} },
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection (2) failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	go le1.Run(ctx)
	go le2.Run(ctx)

	// Wait for initial election to settle (allow time for Keeper replication)
	time.Sleep(5 * time.Second)

	// Exactly one should be leader
	if le1.IsLeader() == le2.IsLeader() {
		t.Fatalf("Expected exactly one leader: le1=%v le2=%v", le1.IsLeader(), le2.IsLeader())
	}

	// Resign the leader, verify failover
	if le1.IsLeader() {
		le1.Resign()
		select {
		case <-promoted2:
			// ok
		case <-time.After(20 * time.Second):
			t.Fatal("le2 did not promote after le1 resigned")
		}
		if !le2.IsLeader() {
			t.Error("le2 should be leader after le1 resigned")
		}
		le2.Resign()
	} else {
		le2.Resign()
		select {
		case <-promoted1:
			// ok
		case <-time.After(20 * time.Second):
			t.Fatal("le1 did not promote after le2 resigned")
		}
		if !le1.IsLeader() {
			t.Error("le1 should be leader after le2 resigned")
		}
		le1.Resign()
	}

	cancel()
}

func TestIntegration_LeaderElection_ThreeNodes(t *testing.T) {
	waitForKeeper(t, 30*time.Second)

	basePath := uniqueBasePath(t)

	type nodeInfo struct {
		le       *leader.LeaderElection
		promoted chan struct{}
		demoted  chan struct{}
	}

	nodes := make([]nodeInfo, 3)
	for i := range nodes {
		p := make(chan struct{}, 2)
		d := make(chan struct{}, 2)
		le, err := leader.NewLeaderElection(
			config.LeaderElectionConfig{
				Hosts:          integrationAllKeeperAddrs(),
				SessionTimeout: 30,
				BasePath:       basePath,
			},
			func() { p <- struct{}{} },
			func() { d <- struct{}{} },
		)
		if err != nil {
			t.Fatalf("NewLeaderElection (%d) failed: %v", i, err)
		}
		nodes[i] = nodeInfo{le: le, promoted: p, demoted: d}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, n := range nodes {
		go n.le.Run(ctx)
	}

	// Wait for election (allow time for Keeper replication)
	time.Sleep(5 * time.Second)

	// Count leaders
	leaderCount := 0
	leaderIdx := -1
	for i, n := range nodes {
		t.Logf("Node %d: IsLeader=%v", i, n.le.IsLeader())
		if n.le.IsLeader() {
			leaderCount++
			leaderIdx = i
		}
	}

	if leaderCount != 1 {
		t.Fatalf("Expected exactly 1 leader among 3 nodes, got %d", leaderCount)
	}

	t.Logf("Node %d is leader", leaderIdx)

	// Resign the leader and wait for a new one to emerge
	nodes[leaderIdx].le.Resign()
	time.Sleep(10 * time.Second)

	// A new leader should emerge
	newLeaderCount := 0
	for i, n := range nodes {
		if i == leaderIdx {
			continue // resigned node
		}
		t.Logf("After resign: Node %d IsLeader=%v", i, n.le.IsLeader())
		if n.le.IsLeader() {
			newLeaderCount++
		}
	}

	if newLeaderCount != 1 {
		t.Errorf("Expected 1 new leader after resignation, got %d", newLeaderCount)
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
		t.Fatalf("NewLeaderElection with nested path failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go le.Run(ctx)
	time.Sleep(2 * time.Second)

	// Should be running without error (base path created recursively)
	if !le.IsLeader() {
		t.Error("Should be leader (single node)")
	}

	cancel()
	le.Resign()
}

// keeperContainers are the integration ClickHouse nodes that co-host the Keeper
// ensemble; stopping all three takes Keeper fully offline (a "flap").
var keeperContainers = []string{"clickhouse-int-1", "clickhouse-int-2", "clickhouse-int-3"}

// pollUntil polls cond every 500ms until it returns true or timeout elapses.
// Returns the final cond() result so callers can assert on it.
func pollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return cond()
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

	go le.Run(ctx)

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

	// Resign mid-outage: marks closed (best-effort delete/close on a dead conn).
	le.Resign()

	for _, c := range keeperContainers {
		dockerStart(t, c)
	}
	waitForKeeper(t, 60*time.Second)
	ensembleRestored = true

	// A resigned instance must not rejoin even though Keeper is healthy again.
	// IsLeader is the durable contract: had it rejoined it would be the only
	// candidate and would re-take leadership within a session TTL, so a sustained
	// false here proves it stayed out. We deliberately do NOT assert !Joined():
	// Resign() intentionally leaves myNode set during teardown (see Resign), and
	// the closed-guard makes rejoinWithRetry bail before dropCandidate, so nothing
	// clears myNode afterwards. Joined() only reads false here if the outage's
	// fail-open dropCandidate happened to run before Resign — an incidental race,
	// not a guarantee.
	if pollUntil(25*time.Second, le.IsLeader) {
		t.Fatal("resigned instance rejoined and became leader after Keeper returned")
	}
	cancel()
}

func TestIntegration_LeaderElection_MultipleKeeperEndpoints(t *testing.T) {
	waitForKeeper(t, 30*time.Second)

	// Connect using all 3 Keeper endpoints for HA
	basePath := uniqueBasePath(t)
	allAddrs := integrationAllKeeperAddrs()
	t.Logf("Connecting to Keeper endpoints: %v", allAddrs)

	promoted := make(chan struct{}, 1)
	le, err := leader.NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts:          allAddrs,
			SessionTimeout: 30,
			BasePath:       basePath,
		},
		func() { promoted <- struct{}{} },
		func() {},
	)
	if err != nil {
		t.Fatalf("NewLeaderElection with 3 endpoints failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go le.Run(ctx)

	select {
	case <-promoted:
		t.Log("Successfully elected leader using 3 Keeper endpoints")
	case <-time.After(10 * time.Second):
		t.Fatal("Did not become leader using 3 Keeper endpoints")
	}

	cancel()
	le.Resign()
}
