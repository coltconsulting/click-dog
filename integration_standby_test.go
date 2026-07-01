//go:build integration

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/leader"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
)

// TestIntegration_LeaderStandby_OnlyLeaderExports pins the headline product
// guarantee of leader-gating against the real Keeper + cluster: of two contending
// instances, only the leader's pipeline reaches the exporter; the standby skips
// every cycle with ErrLeaderStandby and exports nothing. It then forces a failover
// (Resign) and asserts the hand-off — the previously-standby instance, once
// promoted, starts exporting. The existing integration_leader_test.go exercises
// election *state*; nothing drove the pipeline gate end to end (issue #243.1).
//
// The gate under test is the production newLeaderGate (export_gate.go) — the exact
// decision table main.go wires — not a test fake.
func TestIntegration_LeaderStandby_OnlyLeaderExports(t *testing.T) {
	waitForKeeper(t, 30*time.Second)
	conns := waitForAllClickHouse(t, 60*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed spans so the leader has something real to export. The span_log table is
	// created lazily per node, so seeding on every shard also makes the whole-
	// cluster read's fan-out valid.
	for _, c := range conns {
		seedTracedQueries(t, c, 3)
	}

	basePath := uniqueBasePath(t)
	promoted := []chan struct{}{make(chan struct{}, 2), make(chan struct{}, 2)}
	newEl := func(i int) *leader.LeaderElection {
		le, err := leader.NewLeaderElection(
			config.LeaderElectionConfig{
				Hosts:          integrationAllKeeperAddrs(),
				SessionTimeout: 30,
				BasePath:       basePath,
			},
			func() { promoted[i] <- struct{}{} },
			func() {},
		)
		if err != nil {
			t.Fatalf("NewLeaderElection (%d) failed: %v", i, err)
		}
		return le
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Two instances sharing one election base path, each reading via its own
	// cluster reader (node 1 / node 2) and exporting to its own counting sink. The
	// gate is wired exactly as main.go does it (cluster mode on).
	insts := make([]*standbyInstance, 2)
	for i := range insts {
		le := newEl(i)
		go le.Run(ctx)
		reader := newClusterCHReader(t, i+1, "test_cluster")
		defer reader.Close()
		insts[i] = &standbyInstance{
			le:     le,
			reader: reader,
			exp:    &countingExporter{},
			gate:   newLeaderGate(true /* clusterMode */, le),
		}
	}

	// Wait until the election settles: both joined and exactly one leader. Both
	// must be joined before asserting standby — an instance still in its join
	// window has Joined()==false and the gate fails open (exports), which is
	// correct behavior but not the steady state this test pins.
	if !pollUntil(30*time.Second, func() bool {
		return insts[0].le.Joined() && insts[1].le.Joined() &&
			insts[0].le.IsLeader() != insts[1].le.IsLeader()
	}) {
		t.Fatalf("election did not settle to one leader: le0{joined=%v,leader=%v} le1{joined=%v,leader=%v}",
			insts[0].le.Joined(), insts[0].le.IsLeader(), insts[1].le.Joined(), insts[1].le.IsLeader())
	}

	// Phase 1 — steady state: only the leader exports.
	assertGateFollowsLeadership(t, ctx, "steady-state", insts)

	leaderIdx := 0
	if insts[1].le.IsLeader() {
		leaderIdx = 1
	}
	followerIdx := 1 - leaderIdx

	// Phase 2 — failover: resign the leader, wait for the follower to promote.
	insts[leaderIdx].le.Resign()
	insts[leaderIdx].resigned = true // post-resign it is closed → gate fails open; stop asserting it
	select {
	case <-promoted[followerIdx]:
	case <-time.After(30 * time.Second):
		t.Fatalf("instance %d did not promote after instance %d resigned", followerIdx, leaderIdx)
	}
	if !pollUntil(15*time.Second, func() bool { return insts[followerIdx].le.IsLeader() }) {
		t.Fatalf("instance %d did not become leader after failover", followerIdx)
	}

	// The instance that stood by in phase 1 must now export — the hand-off.
	assertGateFollowsLeadership(t, ctx, "post-failover", insts)
	if insts[followerIdx].exp.spanCount() == 0 {
		t.Fatalf("post-failover: promoted instance %d exported no spans — gate did not follow the hand-off", followerIdx)
	}

	insts[followerIdx].le.Resign()
	cancel()
}

// standbyInstance bundles one click-dog instance's election, reader, exporter and
// export gate for the two-instance standby test.
type standbyInstance struct {
	le       *leader.LeaderElection
	reader   *clickhouse.ClickHouseReader
	exp      *countingExporter
	gate     func() bool
	resigned bool
}

// assertGateFollowsLeadership drives a cycle on every live (non-resigned) instance
// and checks the gate matched its leadership: the leader exports at least one span,
// every standby skips with ErrLeaderStandby and exports nothing. Leadership is
// stable within a phase (no Resign in flight), so it polls a few cycles to absorb a
// transient empty fetch rather than depending on a single cycle.
func assertGateFollowsLeadership(t *testing.T, ctx context.Context, phase string, insts []*standbyInstance) {
	t.Helper()
	for i, inst := range insts {
		if inst.resigned {
			continue
		}
		isLeader := inst.le.IsLeader()
		inst.exp.reset()
		var lastErr error
		ok := pollUntil(20*time.Second, func() bool {
			lastErr = inst.driveCycle(t, ctx)
			if isLeader {
				return inst.exp.spanCount() > 0
			}
			return errors.Is(lastErr, processor.ErrLeaderStandby)
		})
		if isLeader {
			if !ok {
				t.Fatalf("%s: leader instance %d exported no spans (lastErr=%v)", phase, i, lastErr)
			}
		} else {
			if !ok {
				t.Fatalf("%s: standby instance %d never skipped with ErrLeaderStandby (lastErr=%v)", phase, i, lastErr)
			}
			if got := inst.exp.spanCount(); got != 0 {
				t.Fatalf("%s: standby instance %d exported %d spans, want 0 — the gate let a follower through", phase, i, got)
			}
		}
	}
}

// driveCycle runs one gated pipeline cycle. A fresh pipeline (and dedup cache) per
// call means the leader re-exports the seeded spans every cycle, so a positive
// count is a reliable "the leader exported" signal across retries.
func (inst *standbyInstance) driveCycle(t *testing.T, ctx context.Context) error {
	t.Helper()
	f, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	pipeline, err := processor.NewPipeline(processor.Pipeline{
		Reader:     inst.reader,
		Exporter:   inst.exp,
		Filter:     f,
		Config:     standbyTestConfig(),
		SeenSpans:  mustLRU(t, 1000),
		Metrics:    metrics.NewMetrics(),
		LeaderGate: inst.gate,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pipeline.Process(ctx)
}

func standbyTestConfig() *config.Config {
	return &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 0,
			CheckIntervalS:     30,
			LookbackS:          300,
			MaxSpansPerCycle:   1000,
			DedupCacheSize:     1000,
		},
	}
}

// countingExporter is a model.SpanExporter that only tallies what it received, so
// the test can assert which instance actually exported.
type countingExporter struct {
	mu    sync.Mutex
	spans int
}

func (c *countingExporter) ExportSpans(_ context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans += len(spans)
	keys := make([]model.SpanKey, len(spans))
	for i, s := range spans {
		keys[i] = model.KeyOf(s)
	}
	return model.ExportResult{Accepted: keys, TotalSent: len(spans), TotalAccepted: len(spans)}, nil
}

func (c *countingExporter) ExportQuery(context.Context, model.QueryLog) (model.ExportResult, error) {
	return model.ExportResult{}, nil
}

func (c *countingExporter) Close(context.Context) error { return nil }

func (c *countingExporter) spanCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spans
}

func (c *countingExporter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = 0
}
