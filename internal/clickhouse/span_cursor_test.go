package clickhouse

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
)

func spanCursorTestRow(traceID uuid.UUID, spanID, finish uint64) []any {
	return []any{
		"host-a",
		traceID,
		spanID,
		uint64(0),
		"SELECT",
		"internal",
		finish - 10,
		finish,
		time.Unix(0, 0).UTC(),
		map[string]string{},
	}
}

// liveSpanRow is one span-log row. visibleAt is when it flushed into the span
// log; a query issued earlier does not see it.
type liveSpanRow struct {
	trace     uuid.UUID
	span      uint64
	finish    uint64
	visibleAt uint64
}

// liveSpanLog answers Live Span Selection's trace and span page queries from
// memory the way ClickHouse would: trace selection is bounded by the lookback
// window, span pages are not, and keyset predicates and ordering follow the
// SQL the reader actually sends. All times are microseconds.
type liveSpanLog struct {
	*fakeConn
	t        *testing.T
	nowUs    uint64
	rows     []liveSpanRow
	failNext int // fail the query this many calls from now (1 = next)
	queries  int
}

func newLiveSpanLog(t *testing.T) (*liveSpanLog, *ClickHouseReader) {
	l := &liveSpanLog{fakeConn: &fakeConn{}, t: t}
	return l, &ClickHouseReader{
		conn:          l,
		queryTimeout:  time.Second,
		traceIDSelect: strings.ReplaceAll(traceIDSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
		spanSelect:    strings.ReplaceAll(spanSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}
}

func (l *liveSpanLog) add(trace uuid.UUID, span, finish, visibleAt uint64) {
	l.rows = append(l.rows, liveSpanRow{trace: trace, span: span, finish: finish, visibleAt: visibleAt})
}

func (l *liveSpanLog) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	l.queries++
	if l.failNext > 0 {
		l.failNext--
		if l.failNext == 0 {
			return nil, errors.New("span log unavailable")
		}
	}
	switch {
	case strings.Contains(query, "SELECT trace_id, max(finish_time)"):
		return l.tracePage(query, args), nil
	case strings.Contains(query, "WHERE trace_id IN ?"):
		return l.spanPage(query, args), nil
	}
	l.t.Fatalf("unexpected SQL:\n%s", query)
	return nil, nil
}

// bindIndex returns the position of clause's first bind argument.
func bindIndex(query, clause string) (int, bool) {
	i := strings.Index(query, clause)
	if i < 0 {
		return 0, false
	}
	return strings.Count(query[:i], "?"), true
}

func compareTraceKeys(a, b traceKey) int {
	switch {
	case a.finish < b.finish:
		return -1
	case a.finish > b.finish:
		return 1
	}
	return bytes.Compare(a.id[:], b.id[:])
}

func compareSpanKeys(a, b spanKey) int {
	if c := compareTraceKeys(traceKey{a.finish, a.trace}, traceKey{b.finish, b.trace}); c != 0 {
		return c
	}
	switch {
	case a.span < b.span:
		return -1
	case a.span > b.span:
		return 1
	}
	return 0
}

func (l *liveSpanLog) tracePage(query string, args []any) driver.Rows {
	var floor uint64
	if window := uint64(args[1].(int)) * uint64(time.Second/time.Microsecond); l.nowUs > window {
		floor = l.nowUs - window
	}
	if i, ok := bindIndex(query, "AND finish_time_us >= ?"); ok {
		floor = max(floor, args[i].(uint64))
	}
	newest := map[uuid.UUID]uint64{}
	for _, r := range l.rows {
		if r.visibleAt <= l.nowUs && r.finish >= floor && r.finish > newest[r.trace] {
			newest[r.trace] = r.finish
		}
	}
	var keys []traceKey
	for id, finish := range newest {
		keys = append(keys, traceKey{finish: finish, id: id})
	}
	for _, b := range []struct {
		clause string
		keep   func(int) bool
	}{
		{"(max_finish_time, trace_id) > (?, ?)", func(c int) bool { return c > 0 }},
		{"(max_finish_time, trace_id) < (?, ?)", func(c int) bool { return c < 0 }},
		{"(max_finish_time, trace_id) <= (?, ?)", func(c int) bool { return c <= 0 }},
	} {
		i, ok := bindIndex(query, b.clause)
		if !ok {
			continue
		}
		bound := traceKey{finish: args[i].(uint64), id: args[i+1].(uuid.UUID)}
		kept := keys[:0]
		for _, k := range keys {
			if b.keep(compareTraceKeys(k, bound)) {
				kept = append(kept, k)
			}
		}
		keys = kept
	}
	desc := strings.Contains(query, "ORDER BY max_finish_time DESC")
	sort.Slice(keys, func(i, j int) bool { return (compareTraceKeys(keys[i], keys[j]) < 0) != desc })
	if limit := args[len(args)-1].(int); len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([][]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, []any{k.id, k.finish})
	}
	return newFakeRows(out...)
}

func (l *liveSpanLog) spanPage(query string, args []any) driver.Rows {
	inPage := map[uuid.UUID]bool{}
	for _, id := range args[0].([]uuid.UUID) {
		inPage[id] = true
	}
	var keys []spanKey
	for _, r := range l.rows {
		if r.visibleAt <= l.nowUs && inPage[r.trace] {
			keys = append(keys, spanKey{finish: r.finish, trace: r.trace, span: r.span})
		}
	}
	for _, b := range []struct {
		clause string
		keep   func(int) bool
	}{
		{"(finish_time_us, trace_id, span_id) > (?, ?, ?)", func(c int) bool { return c > 0 }},
		{"(finish_time_us, trace_id, span_id) < (?, ?, ?)", func(c int) bool { return c < 0 }},
	} {
		i, ok := bindIndex(query, b.clause)
		if !ok {
			continue
		}
		bound := spanKey{finish: args[i].(uint64), trace: args[i+1].(uuid.UUID), span: args[i+2].(uint64)}
		kept := keys[:0]
		for _, k := range keys {
			if b.keep(compareSpanKeys(k, bound)) {
				kept = append(kept, k)
			}
		}
		keys = kept
	}
	desc := strings.Contains(query, "ORDER BY finish_time_us DESC")
	sort.Slice(keys, func(i, j int) bool { return (compareSpanKeys(keys[i], keys[j]) < 0) != desc })
	if limit := args[len(args)-1].(int); len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([][]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, spanCursorTestRow(k.trace, k.span, k.finish))
	}
	return newFakeRows(out...)
}

func seconds(s float64) uint64 { return uint64(s * float64(time.Second/time.Microsecond)) }

func testTraceID(i uint64) uuid.UUID {
	var id uuid.UUID
	// Scramble the high half so trace-ID order does not follow time order.
	binary.BigEndian.PutUint64(id[:8], i*0x9e3779b97f4a7c15)
	binary.BigEndian.PutUint64(id[8:], i)
	return id
}

type spanIdentity struct {
	trace uuid.UUID
	span  uint64
}

func identities(spans []model.OpenTelemetrySpan) []spanIdentity {
	out := make([]spanIdentity, 0, len(spans))
	for _, s := range spans {
		out = append(out, spanIdentity{trace: s.TraceID, span: s.SpanID})
	}
	return out
}

// runLiveCycles drives scheduled cycles every interval against log, committing
// each fetch unless failCycle says its export failed. It returns every span
// identity a committed fetch delivered.
func runLiveCycles(t *testing.T, log *liveSpanLog, reader *ClickHouseReader, interval, lookback time.Duration, capSpans, cycles int, failCycle func(int) bool) map[spanIdentity]bool {
	t.Helper()
	nows := make([]uint64, cycles)
	for k := range nows {
		nows[k] = seconds((time.Duration(k+1) * interval).Seconds())
	}
	return runLiveSchedule(t, log, reader, nows, lookback, capSpans, failCycle)
}

// runLiveSchedule is runLiveCycles with explicit cycle times, numbered from 1.
func runLiveSchedule(t *testing.T, log *liveSpanLog, reader *ClickHouseReader, nows []uint64, lookback time.Duration, capSpans int, failCycle func(int) bool) map[spanIdentity]bool {
	t.Helper()
	delivered := map[spanIdentity]bool{}
	for i, now := range nows {
		k := i + 1
		log.nowUs = now
		spans, err := reader.FetchLiveSpans(context.Background(), 1, lookback, capSpans, FetchOpts{})
		if err != nil {
			t.Fatalf("cycle %d: FetchLiveSpans: %v", k, err)
		}
		if len(spans) > capSpans {
			t.Fatalf("cycle %d returned %d spans, over the cap of %d", k, len(spans), capSpans)
		}
		if failCycle != nil && failCycle(k) {
			continue
		}
		for _, id := range identities(spans) {
			delivered[id] = true
		}
		reader.CommitLiveSpans()
	}
	return delivered
}

// seedSteadyInflow writes traces of spansPerTrace spans at tracesPerSec up to
// endUs. Child spans finish up to a millisecond before their root. Every row
// becomes visible at the next flush tick, as it does on a single ClickHouse
// node, which flushes everything buffered at once.
func seedSteadyInflow(log *liveSpanLog, tracesPerSec float64, spansPerTrace int, endUs, flushEveryUs uint64) {
	step := uint64(float64(time.Second/time.Microsecond) / tracesPerSec)
	n := uint64(0)
	for root := step; root < endUs; root += step {
		n++
		visible := (root/flushEveryUs + 1) * flushEveryUs
		for j := 0; j < spansPerTrace; j++ {
			log.add(testTraceID(n), uint64(j+1), root-uint64(j)*100, visible)
		}
	}
}

// TestFetchLiveSpans_SteadyInflowBelowCapacityLosesNothing pins the regression
// where a capped walk that continued an older page skipped the newest traces
// for that cycle: at 26–32 spans/s, below the documented sustainable
// max_spans_per_cycle / check_interval_s (33.3/s at the defaults), roughly a
// third of traces were never read. Every span that became visible while its
// trace was still inside the lookback window must be delivered.
func TestFetchLiveSpans_SteadyInflowBelowCapacityLosesNothing(t *testing.T) {
	const (
		interval = 30 * time.Second
		lookback = 40 * time.Second
		capSpans = 1000
		cycles   = 40
	)
	for _, spansPerSec := range []float64{20, 26, 30, 32} {
		log, reader := newLiveSpanLog(t)
		endUs := seconds((cycles * interval).Seconds())
		seedSteadyInflow(log, spansPerSec/10, 10, endUs, seconds(7.5))

		delivered := runLiveCycles(t, log, reader, interval, lookback, capSpans, cycles, nil)

		// Judge rows visible by the second-to-last cycle; the last cycle may
		// legitimately still hold a backlog.
		judgeUntil := seconds(((cycles - 1) * interval).Seconds())
		lost, total := 0, 0
		for _, r := range log.rows {
			if r.visibleAt > judgeUntil {
				continue
			}
			total++
			if !delivered[spanIdentity{trace: r.trace, span: r.span}] {
				lost++
			}
		}
		if lost != 0 {
			t.Errorf("%.0f spans/s: lost %d of %d spans", spansPerSec, lost, total)
		}
	}
}

// TestFetchLiveSpans_UncommittedPageIsReadAgain pins that a fetch whose export
// failed does not move the walk: the next fetch returns the same page, and only
// a commit moves past it.
func TestFetchLiveSpans_UncommittedPageIsReadAgain(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	for i := uint64(1); i <= 5; i++ {
		log.add(testTraceID(i), 1, seconds(float64(i)), 0)
	}
	log.nowUs = seconds(10)
	fetch := func() []spanIdentity {
		spans, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 2, FetchOpts{})
		if err != nil {
			t.Fatalf("FetchLiveSpans: %v", err)
		}
		return identities(spans)
	}

	first := fetch()
	if len(first) != 2 {
		t.Fatalf("first page = %v, want the two oldest spans", first)
	}
	if again := fetch(); !equalIdentities(again, first) {
		t.Fatalf("uncommitted page re-read as %v, want %v", again, first)
	}
	reader.CommitLiveSpans()
	if next := fetch(); len(next) != 2 || equalIdentities(next, first) {
		t.Fatalf("page after commit = %v, want the next two spans", next)
	}
}

// TestFetchLiveSpans_FailedQueryKeepsTheCommittedPosition pins that a query
// error part-way through a fetch discards what that fetch read: nothing is
// pending, so a commit is a no-op and the next fetch starts from the last
// committed position.
func TestFetchLiveSpans_FailedQueryKeepsTheCommittedPosition(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	for i := uint64(1); i <= 3; i++ {
		log.add(testTraceID(i), 1, seconds(float64(i)), 0)
	}
	log.nowUs = seconds(10)

	log.failNext = 2 // the trace page succeeds, its span page fails
	if _, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 10, FetchOpts{}); err == nil {
		t.Fatal("FetchLiveSpans succeeded despite a failed span page")
	}
	reader.CommitLiveSpans()

	spans, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 10, FetchOpts{})
	if err != nil {
		t.Fatalf("FetchLiveSpans after failure: %v", err)
	}
	if len(spans) != 3 {
		t.Fatalf("fetch after a failed query returned %d spans, want all 3", len(spans))
	}
}

// TestFetchLiveSpans_IntermittentExportFailuresLoseNothing drives a failed
// export every third cycle at the defaults. Most of a failed page is already
// behind the lookback floor when the next cycle runs, so only the retry
// reaching back to the committed position recovers it. The inflow leaves too
// little spare cap for the late-flush rescan to cover a skipped page.
func TestFetchLiveSpans_IntermittentExportFailuresLoseNothing(t *testing.T) {
	const (
		interval = 30 * time.Second
		lookback = 40 * time.Second
		capSpans = 1000
		cycles   = 30
	)
	log, reader := newLiveSpanLog(t)
	seedSteadyInflow(log, 1.8, 10, seconds((cycles * interval).Seconds()), seconds(7.5))

	delivered := runLiveCycles(t, log, reader, interval, lookback, capSpans, cycles, func(k int) bool { return k%3 == 0 })

	assertDeliveredUntil(t, log, delivered, seconds(((cycles - 2) * interval).Seconds()))
}

// TestFetchLiveSpans_BackoffGapAfterAFailureLosesNothing covers the production
// profile: a failed export doubles the poll interval, so the retry runs 60s
// after the failed cycle against a 40s lookback. The failed page and the
// unfetched rows behind the floor are both still recovered.
func TestFetchLiveSpans_BackoffGapAfterAFailureLosesNothing(t *testing.T) {
	const (
		lookback = 40 * time.Second
		capSpans = 1000
	)
	log, reader := newLiveSpanLog(t)
	var nows []uint64
	for s := 30.0; s <= 300; s += 30 {
		nows = append(nows, seconds(s))
		if s == 150 {
			s += 30 // backoff: the cycle after the failure runs 60s later
		}
	}
	seedSteadyInflow(log, 2, 10, nows[len(nows)-1], seconds(7.5))

	failed := 5 // the cycle at 150s
	delivered := runLiveSchedule(t, log, reader, nows, lookback, capSpans, func(k int) bool { return k == failed })

	assertDeliveredUntil(t, log, delivered, nows[len(nows)-2])
}

// TestFetchLiveSpans_RetryReachesBackThreeLookbacksAndNoFurther pins both edges
// of the retry reach: an uncommitted page behind the lookback floor is still
// read while it is within three lookback windows, and a page that never commits
// stops being read after that instead of pinning the walk.
func TestFetchLiveSpans_RetryReachesBackThreeLookbacksAndNoFurther(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	committed, pending := testTraceID(1), testTraceID(2)
	log.add(committed, 1, seconds(1), 0)
	log.add(pending, 1, seconds(10), 0)
	fetch := func(now float64) []spanIdentity {
		log.nowUs = seconds(now)
		spans, err := reader.FetchLiveSpans(context.Background(), 1, 40*time.Second, 1, FetchOpts{})
		if err != nil {
			t.Fatalf("FetchLiveSpans at %.0fs: %v", now, err)
		}
		return identities(spans)
	}

	if got := fetch(5); !equalIdentities(got, []spanIdentity{{committed, 1}}) {
		t.Fatalf("first fetch = %v, want the committed trace", got)
	}
	reader.CommitLiveSpans()

	// Never committed from here on: every export of the pending trace fails.
	want := []spanIdentity{{pending, 1}}
	if got := fetch(15); !equalIdentities(got, want) {
		t.Fatalf("fetch at 15s = %v, want %v", got, want)
	}
	if got := fetch(70); !equalIdentities(got, want) {
		t.Fatalf("fetch at 70s (trace 60s old, behind the 40s floor) = %v, want it retried", got)
	}
	if got := fetch(120); !equalIdentities(got, want) {
		t.Fatalf("fetch at 120s (trace 110s old, within three lookbacks) = %v, want it retried", got)
	}
	if got := fetch(135); len(got) != 0 {
		t.Fatalf("fetch at 135s (trace 125s old, past three lookbacks) = %v, want it dropped", got)
	}
}

// TestFetchLiveSpans_FreshWalkStartsAtTheLookbackFloor pins that only a
// committed position widens the window: a walk that has delivered nothing reads
// the plain lookback window.
func TestFetchLiveSpans_FreshWalkStartsAtTheLookbackFloor(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	log.add(testTraceID(1), 1, seconds(10), 0)
	log.nowUs = seconds(60)

	spans, err := reader.FetchLiveSpans(context.Background(), 1, 40*time.Second, 10, FetchOpts{})
	if err != nil {
		t.Fatalf("FetchLiveSpans: %v", err)
	}
	if len(spans) != 0 {
		t.Fatalf("fresh walk returned %v, a trace 50s old behind the 40s floor", identities(spans))
	}
}

func assertDeliveredUntil(t *testing.T, log *liveSpanLog, delivered map[spanIdentity]bool, visibleBy uint64) {
	t.Helper()
	lost := 0
	for _, r := range log.rows {
		if r.visibleAt <= visibleBy && !delivered[spanIdentity{trace: r.trace, span: r.span}] {
			if lost == 0 {
				t.Errorf("span %s/%d (finish %dµs) was never delivered", r.trace, r.span, r.finish)
			}
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("%d spans visible by %dµs were never delivered", lost, visibleBy)
	}
}

// TestFetchLiveSpans_DrainsATraceLargerThanTheCap pins the forward-progress
// guarantee the walk exists for: a trace with more spans than the cap is
// drained across fetches instead of re-reading its first page forever.
func TestFetchLiveSpans_DrainsATraceLargerThanTheCap(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	big := testTraceID(1)
	for span := uint64(1); span <= 5; span++ {
		log.add(big, span, seconds(5)+span, 0)
	}
	log.nowUs = seconds(10)

	var got []spanIdentity
	for i := 0; i < 3; i++ {
		spans, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 2, FetchOpts{})
		if err != nil {
			t.Fatalf("FetchLiveSpans: %v", err)
		}
		got = append(got, identities(spans)...)
		reader.CommitLiveSpans()
	}
	want := []spanIdentity{{big, 1}, {big, 2}, {big, 3}, {big, 4}, {big, 5}}
	if !equalIdentities(got, want) {
		t.Fatalf("drained spans = %v, want each span once in finish order %v", got, want)
	}
}

// TestFetchLiveSpans_RescanFindsASpanThatFlushedBehindTheWatermark covers the
// span-log flush lag in cluster mode: a node that flushes later can land a row
// whose trace is already behind the watermark. Once the walk reaches the head,
// the rest of the cap re-reads delivered traces newest first and finds it.
func TestFetchLiveSpans_RescanFindsASpanThatFlushedBehindTheWatermark(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	early, late := testTraceID(1), testTraceID(2)
	log.add(early, 1, seconds(1), seconds(1))
	log.add(late, 1, seconds(2), seconds(2))
	// Another node's span of the first trace flushes after the walk has
	// passed that trace.
	log.add(early, 2, seconds(0.5), seconds(15))

	fetch := func(now float64) []spanIdentity {
		log.nowUs = seconds(now)
		spans, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 100, FetchOpts{})
		if err != nil {
			t.Fatalf("FetchLiveSpans: %v", err)
		}
		reader.CommitLiveSpans()
		return identities(spans)
	}

	if got := fetch(10); len(got) != 2 {
		t.Fatalf("first fetch = %v, want both visible traces", got)
	}
	found := false
	for _, id := range fetch(20) {
		if id == (spanIdentity{early, 2}) {
			found = true
		}
	}
	if !found {
		t.Fatal("the late-flushed span behind the watermark was not re-read")
	}
}

// TestFetchLiveSpans_WalksOldestFirstFromTheWatermark pins the SQL shape: the
// forward walk reads trace and span pages in ascending keyset order, continues
// past the committed watermark, and only the rescan reads newest first.
func TestFetchLiveSpans_WalksOldestFirstFromTheWatermark(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	log.add(testTraceID(1), 1, seconds(1), 0)
	log.nowUs = seconds(10)
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(query string) (driver.Rows, error) {
		return log.Query(context.Background(), query, conn.queries[len(conn.queries)-1]...)
	}
	reader.conn = conn

	if _, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 10, FetchOpts{}); err != nil {
		t.Fatalf("FetchLiveSpans: %v", err)
	}
	reader.CommitLiveSpans()
	if _, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 10, FetchOpts{}); err != nil {
		t.Fatalf("FetchLiveSpans: %v", err)
	}

	wants := []string{
		"ORDER BY max_finish_time ASC, trace_id ASC",
		"ORDER BY finish_time_us ASC, trace_id ASC, span_id ASC",
		"HAVING (max_finish_time, trace_id) > (?, ?) ORDER BY max_finish_time ASC",
		"HAVING (max_finish_time, trace_id) <= (?, ?) ORDER BY max_finish_time DESC",
		"ORDER BY finish_time_us DESC, trace_id DESC, span_id DESC",
	}
	for i, want := range wants {
		if i >= len(conn.sqls) || !strings.Contains(conn.sqls[i], want) {
			t.Fatalf("query %d lacks %q; queries:\n%s", i, want, strings.Join(conn.sqls, "\n---\n"))
		}
	}

	// A fresh walk reads the plain window; once a position is committed the
	// forward walk widens it to the retry reach and floors it at the position.
	if strings.Contains(conn.sqls[0], "AND finish_time_us >= ?") || conn.queries[0][1] != int(time.Minute.Seconds()) {
		t.Errorf("fresh trace page = %v, want the plain 60s window without a position floor", conn.queries[0])
	}
	if !strings.Contains(conn.sqls[2], "AND finish_time_us >= ?") || conn.queries[2][1] != liveRetryReach*int(time.Minute.Seconds()) || conn.queries[2][3] != seconds(1) {
		t.Errorf("resumed trace page args = %v, want a %d-lookback window floored at the committed position", conn.queries[2], liveRetryReach)
	}
	if strings.Contains(conn.sqls[3], "AND finish_time_us >= ?") || conn.queries[3][1] != int(time.Minute.Seconds()) {
		t.Errorf("rescan trace page = %v, want the plain 60s window", conn.queries[3])
	}
}

// TestFetchLiveSpans_FilterChangeRestartsTheWalk pins that a different
// selection (here the cap) discards the old position rather than continuing a
// walk whose pages were chosen under other filters.
func TestFetchLiveSpans_FilterChangeRestartsTheWalk(t *testing.T) {
	log, reader := newLiveSpanLog(t)
	for i := uint64(1); i <= 3; i++ {
		log.add(testTraceID(i), 1, seconds(float64(i)), 0)
	}
	log.nowUs = seconds(10)

	if _, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 3, FetchOpts{}); err != nil {
		t.Fatalf("FetchLiveSpans: %v", err)
	}
	reader.CommitLiveSpans()
	spans, err := reader.FetchLiveSpans(context.Background(), 1, time.Minute, 5, FetchOpts{})
	if err != nil {
		t.Fatalf("FetchLiveSpans: %v", err)
	}
	if len(spans) != 3 {
		t.Fatalf("fetch under a new cap returned %d spans, want the walk restarted at the floor (3)", len(spans))
	}
}

func equalIdentities(a, b []spanIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
