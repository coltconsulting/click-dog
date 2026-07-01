package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// argCapturingConn records the QueryRow SQL and bound args. fakeConn.QueryRow
// discards its args (`_ ...any`), so CountDistinctClusterReaders tests that need
// to pin the lookback plumbing use this instead.
type argCapturingConn struct {
	*fakeConn
	row     driver.Row
	gotQ    string
	gotArgs []any
	calls   int
}

func (c *argCapturingConn) QueryRow(_ context.Context, query string, args ...any) driver.Row {
	c.calls++
	c.gotQ = query
	c.gotArgs = append([]any(nil), args...)
	return c.row
}

func TestCountDistinctClusterReaders_EmptyClusterShortCircuits(t *testing.T) {
	cc := &argCapturingConn{fakeConn: &fakeConn{}, row: fakeRow{count: 99}}
	reader := &ClickHouseReader{conn: cc, queryTimeout: time.Second, cluster: ""}

	got, err := reader.CountDistinctClusterReaders(context.Background(), 15*time.Minute, 90*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0 (empty cluster short-circuit)", got)
	}
	if cc.calls != 0 {
		t.Errorf("QueryRow calls = %d, want 0 — empty cluster must issue no query (clusterAllReplicas('', …) is invalid SQL)", cc.calls)
	}
}

func TestCountDistinctClusterReaders_ScansAllReplicasWithLookback(t *testing.T) {
	cc := &argCapturingConn{fakeConn: &fakeConn{}, row: fakeRow{count: 3}}
	reader := &ClickHouseReader{conn: cc, queryTimeout: time.Second, cluster: "prod"}

	// Deliberately non-default windows (90 min ≠ the 15-min lookback default; 90 s
	// active window) so neither knob can silently drift to a dead default:
	// 90 min => 5400 s, 1 day; 90 s active window => 90 s.
	got, err := reader.CountDistinctClusterReaders(context.Background(), 90*time.Minute, 90*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3 {
		t.Errorf("count = %d, want 3", got)
	}

	// Must scan clusterAllReplicas (every replica/node), NOT the one-replica
	// -per-shard cluster() form that would miss sidecars on non-selected replicas.
	if !strings.Contains(cc.gotQ, "clusterAllReplicas('prod', system.query_log)") {
		t.Errorf("query does not scan clusterAllReplicas('prod', system.query_log):\n%s", cc.gotQ)
	}
	if strings.Contains(cc.gotQ, "cluster('prod'") {
		t.Errorf("query uses the cluster() one-replica-per-shard form, which under-counts replicated fleets:\n%s", cc.gotQ)
	}
	// Self-match guard: the probe's own SQL text must not contain the verbatim
	// substrings it matches on, or it would count its own (and sibling auditors')
	// query_log probes.
	if strings.Contains(cc.gotQ, "opentelemetry_span_log") {
		t.Errorf("probe SQL contains the verbatim 'opentelemetry_span_log' substring it matches on — it would count itself:\n%s", cc.gotQ)
	}
	// Recency trigger: a host counts only if its MOST RECENT read is within the
	// active window — a HAVING on max(event_time), not a bare countDistinct over
	// the whole lookback (the temporal false-positive fixed in #223).
	if !strings.Contains(cc.gotQ, "HAVING max(event_time)") {
		t.Errorf("query has no active-window HAVING max(event_time) recency filter; a departed reader would linger the full lookback:\n%s", cc.gotQ)
	}

	// lookbackDays=1, lookbackSeconds=5400, activeWindowSeconds=90 — pins both
	// windows all the way to the bound params.
	if len(cc.gotArgs) != 3 {
		t.Fatalf("bound args = %v, want [days, lookbackSeconds, activeWindowSeconds]", cc.gotArgs)
	}
	if cc.gotArgs[0] != 1 {
		t.Errorf("lookbackDays arg = %v, want 1", cc.gotArgs[0])
	}
	if cc.gotArgs[1] != 5400 {
		t.Errorf("lookbackSeconds arg = %v, want 5400 (90 min)", cc.gotArgs[1])
	}
	if cc.gotArgs[2] != 90 {
		t.Errorf("activeWindowSeconds arg = %v, want 90", cc.gotArgs[2])
	}
}

// TestHasRecentQueryLogRows pins the blindness sanity probe (#238): it must
// read the LOCAL system.query_log — no clusterAllReplicas fan-out, this is a
// cheap once-per-process disambiguation — with both window args bound.
func TestHasRecentQueryLogRows(t *testing.T) {
	cc := &argCapturingConn{fakeConn: &fakeConn{}, row: fakeRow{count: 2}}
	reader := &ClickHouseReader{conn: cc, queryTimeout: time.Second, cluster: "prod"}

	got, err := reader.HasRecentQueryLogRows(context.Background(), 90*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("rows present should report true")
	}
	if !strings.Contains(cc.gotQ, "FROM system.query_log") || strings.Contains(cc.gotQ, "clusterAllReplicas") {
		t.Errorf("sanity probe must read the local system.query_log only:\n%s", cc.gotQ)
	}
	if len(cc.gotArgs) != 2 || cc.gotArgs[0] != 1 || cc.gotArgs[1] != 5400 {
		t.Errorf("bound args = %v, want [1, 5400] (90 min lookback)", cc.gotArgs)
	}

	cc.row = fakeRow{count: 0}
	if got, err = reader.HasRecentQueryLogRows(context.Background(), 90*time.Minute); err != nil || got {
		t.Errorf("empty query_log should report (false, nil), got (%v, %v)", got, err)
	}
}

func TestCountDistinctClusterReaders_ErrorPropagates(t *testing.T) {
	cc := &argCapturingConn{fakeConn: &fakeConn{}, row: fakeRow{err: errors.New("keeper down")}}
	reader := &ClickHouseReader{conn: cc, queryTimeout: time.Second, cluster: "prod"}

	if _, err := reader.CountDistinctClusterReaders(context.Background(), 15*time.Minute, 90*time.Second); err == nil {
		t.Fatal("expected error to propagate from the scan, got nil")
	}
}

func TestHasRecentQueryLogRows_ErrorPropagates(t *testing.T) {
	cc := &argCapturingConn{fakeConn: &fakeConn{}, row: fakeRow{err: errors.New("clickhouse down")}}
	reader := &ClickHouseReader{conn: cc, queryTimeout: time.Second, cluster: "prod"}

	if _, err := reader.HasRecentQueryLogRows(context.Background(), 15*time.Minute); err == nil {
		t.Fatal("expected error to propagate from the sanity probe, got nil")
	}
}
