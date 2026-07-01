package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/model"
)

// fakePinger lets tests toggle ClickHouse reachability.
type fakePinger struct {
	healthy bool
	calls   int
}

func (f *fakePinger) IsHealthy(_ context.Context) bool {
	f.calls++
	return f.healthy
}

// fakeSource is a minimal CycleSource for tests. Field names mirror the
// public Snapshot struct one-for-one so test setup stays obvious.
type fakeSource struct {
	last       CycleSnapshot
	haveLast   bool
	cbState    string
	backoffSec float64
	upSince    time.Time
}

func (f *fakeSource) Snapshot() Snapshot {
	return Snapshot{
		LastCycle:           f.last,
		HaveLastCycle:       f.haveLast,
		CircuitBreakerState: f.cbState,
		BackoffIntervalSecs: f.backoffSec,
		UpSince:             f.upSince,
	}
}

func newTestServer(p Pinger, s CycleSource) *Server {
	srv := NewServer(p, s, "test-1.0.0")
	// Keep readiness fast in tests.
	srv.readyTimeout = 50 * time.Millisecond
	return srv
}

func TestHealthz_AlwaysOK(t *testing.T) {
	srv := newTestServer(&fakePinger{healthy: false}, &fakeSource{cbState: "open"})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.healthz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	// json.Encoder appends a trailing newline; strip for comparison.
	got := strings.TrimSpace(string(body))
	if got != `{"status":"ok"}` {
		t.Errorf("healthz body = %q, want {\"status\":\"ok\"}", got)
	}
}

func TestReadyz_PreservesLegacyKeyOrder(t *testing.T) {
	// /readyz used to be built from a map[string]any, whose keys serialize
	// alphabetically — so existing consumers see "checks" before
	// "status". The struct-based refactor must preserve that order or
	// any external literal-string matcher silently breaks. Pin the byte
	// order here so a future re-shuffle of readyzResult fields trips
	// this test.
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	srv.readyz(w, req)

	body := strings.TrimSpace(w.Body.String())
	checksAt := strings.Index(body, `"checks"`)
	statusAt := strings.Index(body, `"status"`)
	if checksAt < 0 || statusAt < 0 {
		t.Fatalf("body missing expected keys: %s", body)
	}
	if checksAt >= statusAt {
		t.Errorf("readyz body has \"status\" before \"checks\" — legacy contract requires \"checks\" first; body=%s", body)
	}
}

func TestReadyz_OKWhenHealthyAndClosed(t *testing.T) {
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", w.Code)
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("readyz body invalid JSON: %v", err)
	}
	if got["status"] != "ready" {
		t.Errorf("readyz status field = %v, want ready", got["status"])
	}
	checks, _ := got["checks"].(map[string]any)
	if checks["clickhouse"] != "ok" {
		t.Errorf("clickhouse check = %v, want ok", checks["clickhouse"])
	}
	if checks["circuit_breaker"] != "closed" {
		t.Errorf("circuit_breaker check = %v, want closed", checks["circuit_breaker"])
	}
}

func TestReadyz_503WhenClickHouseDown(t *testing.T) {
	srv := newTestServer(&fakePinger{healthy: false}, &fakeSource{cbState: "closed"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["status"] != "not_ready" {
		t.Errorf("status = %v, want not_ready", got["status"])
	}
	checks, _ := got["checks"].(map[string]any)
	if checks["clickhouse"] != "unreachable" {
		t.Errorf("clickhouse check = %v, want unreachable", checks["clickhouse"])
	}
}

func TestReadyz_503WhenBothFail(t *testing.T) {
	// Both ClickHouse unreachable AND breaker open. 503 is already required
	// from either condition alone; this test pins the contract that /readyz
	// reports BOTH failure reasons in checks so operators don't have to
	// re-probe after fixing one to discover the other.
	srv := newTestServer(&fakePinger{healthy: false}, &fakeSource{cbState: "open"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	checks, _ := got["checks"].(map[string]any)
	if checks["clickhouse"] != "unreachable" {
		t.Errorf("clickhouse check = %v, want unreachable", checks["clickhouse"])
	}
	if checks["circuit_breaker"] != "open" {
		t.Errorf("circuit_breaker check = %v, want open", checks["circuit_breaker"])
	}
}

func TestReadyz_503WhenCircuitOpen(t *testing.T) {
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "open"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	checks, _ := got["checks"].(map[string]any)
	if checks["circuit_breaker"] != "open" {
		t.Errorf("circuit_breaker = %v, want open", checks["circuit_breaker"])
	}
}

func TestReadyz_503WhenCircuitHalfOpen(t *testing.T) {
	// half-open means the breaker is probing; not yet ready.
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "half_open"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 when half_open", w.Code)
	}
}

func TestReadyz_HyphenatedHalfOpen_Normalized(t *testing.T) {
	// A CycleSource that emits the raw resilience.CircuitState.String() form
	// ("half-open" with a hyphen) must still be treated as not-closed, AND
	// the checks.circuit_breaker field must carry the canonical "half_open".
	// Guards the defense-in-depth branch of normalizeCBState at the handler.
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "half-open"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 for hyphenated half-open", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	checks, _ := got["checks"].(map[string]any)
	if checks["circuit_breaker"] != "half_open" {
		t.Errorf("circuit_breaker = %v, want canonical half_open (not raw half-open)", checks["circuit_breaker"])
	}
}

func TestReadyz_NilPingerAndSource_200NoChecks(t *testing.T) {
	// Degenerate: no dependencies to probe. Nothing can fail, so readyz
	// returns 200 with an empty checks map. Locks the behavior so a future
	// refactor doesn't accidentally return 503 for "no checks ran".
	srv := newTestServer(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	srv.readyz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200 when no deps", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["status"] != "ready" {
		t.Errorf("status = %v, want ready", got["status"])
	}
	checks, _ := got["checks"].(map[string]any)
	if len(checks) != 0 {
		t.Errorf("checks = %v, want empty map", checks)
	}
}

func TestStatus_JSONShape(t *testing.T) {
	const fakeUptime = 2 * time.Hour
	now := time.Now().Add(-fakeUptime)
	src := &fakeSource{
		cbState:    "closed",
		backoffSec: 30,
		upSince:    now,
		haveLast:   true,
		last: CycleSnapshot{
			Exported:   15,
			Filtered:   3,
			Duplicates: 2,
			DurationMs: 450,
			At:         time.Now(),
		},
	}
	srv := newTestServer(&fakePinger{healthy: true}, src)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	srv.status(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", w.Code)
	}

	var got statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status body invalid JSON: %v", err)
	}

	if got.Version != "test-1.0.0" {
		t.Errorf("version = %q, want test-1.0.0", got.Version)
	}
	// upSince was set 2h ago; allow a small fudge for test execution time
	// but assert we're solidly in that window (not, say, 0).
	wantMinUptime := int64((fakeUptime - 1*time.Minute).Seconds())
	if got.UptimeS < wantMinUptime {
		t.Errorf("uptime_s = %d, want >=%d (upSince was %v ago)", got.UptimeS, wantMinUptime, fakeUptime)
	}
	if !got.ClickHouseHealthy {
		t.Error("clickhouse_healthy = false, want true")
	}
	if got.CircuitBreaker != "closed" {
		t.Errorf("circuit_breaker = %q, want closed", got.CircuitBreaker)
	}
	if got.BackoffIntervalS != 30 {
		t.Errorf("backoff_interval_s = %v, want 30", got.BackoffIntervalS)
	}
	if got.LastCycle == nil {
		t.Fatal("last_cycle missing")
	}
	if got.LastCycle.Exported != 15 || got.LastCycle.Filtered != 3 ||
		got.LastCycle.Duplicates != 2 || got.LastCycle.DurationMs != 450 {
		t.Errorf("last_cycle mismatch: %+v", got.LastCycle)
	}
	// `at` is part of the stable /status public API — pin that it's emitted
	// when the source has a recorded cycle timestamp. Format correctness is
	// trusted to time.Format(RFC3339); here we just verify non-empty.
	if got.LastCycle.At == "" {
		t.Error("last_cycle.at is empty; want RFC3339 timestamp")
	}
}

func TestStatus_NoCycleYet(t *testing.T) {
	src := &fakeSource{cbState: "closed", upSince: time.Now()}
	srv := newTestServer(&fakePinger{healthy: true}, src)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	srv.status(w, req)

	var got statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status body invalid JSON: %v", err)
	}
	if got.LastCycle != nil {
		t.Errorf("expected no last_cycle before first cycle, got %+v", got.LastCycle)
	}
	// Key invariant: before any cycle runs the endpoint must NOT report
	// ClickHouse healthy, even with a default-closed breaker. A false green
	// on startup would mask a broken ClickHouse connection during pod init.
	if got.ClickHouseHealthy {
		t.Error("clickhouse_healthy = true before first cycle; want false")
	}
}

func TestStatus_CircuitOpenSkipReportsUnhealthy(t *testing.T) {
	// A skipped cycle (breaker open, no canary) records Err="" and
	// counts=0 but must not count as evidence of health. Regression guard
	// against the scenario where /status would flip clickhouse_healthy
	// green on a "cycle" that never touched ClickHouse.
	src := &fakeSource{
		cbState:  "closed", // breaker has since closed
		upSince:  time.Now(),
		haveLast: true,
		last:     CycleSnapshot{Skipped: true, SkipReason: model.CycleSkipReasonCircuitOpen, At: time.Now()},
	}
	srv := newTestServer(&fakePinger{healthy: true}, src)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	srv.status(w, req)

	var got statusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ClickHouseHealthy {
		t.Error("clickhouse_healthy = true when last cycle was skipped; want false")
	}
	if got.LastCycle == nil {
		t.Fatal("last_cycle missing")
	}
	if !got.LastCycle.Skipped {
		t.Errorf("last_cycle.skipped not surfaced in JSON: %+v", got.LastCycle)
	}
	if got.LastCycle.SkipReason != model.CycleSkipReasonCircuitOpen {
		t.Errorf("last_cycle.skip_reason = %q, want %s", got.LastCycle.SkipReason, model.CycleSkipReasonCircuitOpen)
	}
	if got.Standby {
		t.Error("standby = true for circuit-open skip; want false")
	}
}

func TestStatus_LeaderStandbySkipReportsHealthyStandby(t *testing.T) {
	src := &fakeSource{
		cbState:  "closed",
		upSince:  time.Now(),
		haveLast: true,
		last:     CycleSnapshot{Skipped: true, SkipReason: model.CycleSkipReasonLeaderStandby, At: time.Now()},
	}
	srv := newTestServer(&fakePinger{healthy: true}, src)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	srv.status(w, req)

	var got statusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.ClickHouseHealthy {
		t.Error("clickhouse_healthy = false for leader standby; want true")
	}
	if !got.Standby {
		t.Error("standby = false for leader standby skip; want true")
	}
	if got.LastCycle == nil {
		t.Fatal("last_cycle missing")
	}
	if got.LastCycle.SkipReason != model.CycleSkipReasonLeaderStandby {
		t.Errorf("last_cycle.skip_reason = %q, want %s", got.LastCycle.SkipReason, model.CycleSkipReasonLeaderStandby)
	}
}

func TestStatus_LastCycleErrored_ReportsUnhealthy(t *testing.T) {
	// Most recent cycle failed → CH not healthy, even if the breaker hasn't
	// opened yet (e.g. failure_threshold > 1 and we've only had one fail).
	src := &fakeSource{
		cbState:  "closed",
		upSince:  time.Now(),
		haveLast: true,
		last:     CycleSnapshot{Err: "connection refused", At: time.Now()},
	}
	srv := newTestServer(&fakePinger{healthy: true}, src)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	srv.status(w, req)

	var got statusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ClickHouseHealthy {
		t.Error("clickhouse_healthy = true when last cycle errored; want false")
	}
}

func TestReadyz_NilPinger_ReportsOnlyBreaker(t *testing.T) {
	// Without a pinger we can't check ClickHouse; readiness depends solely on
	// the breaker. Closed → 200.
	srv := newTestServer(nil, &fakeSource{cbState: "closed"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	checks, _ := got["checks"].(map[string]any)
	if _, present := checks["clickhouse"]; present {
		t.Errorf("clickhouse check should be absent when pinger is nil, got %v", checks["clickhouse"])
	}
	if checks["circuit_breaker"] != "closed" {
		t.Errorf("circuit_breaker = %v, want closed", checks["circuit_breaker"])
	}
}

func TestReadyz_NilSource_ReportsOnlyCH(t *testing.T) {
	// Without a source we can't see the breaker; readiness depends solely on
	// the pinger. Healthy → 200; unreachable → 503.
	srv := newTestServer(&fakePinger{healthy: true}, nil)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	srv.readyz(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("readyz (nil source, healthy CH) = %d, want 200", w.Code)
	}

	srv = newTestServer(&fakePinger{healthy: false}, nil)
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w = httptest.NewRecorder()
	srv.readyz(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz (nil source, unreachable CH) = %d, want 503", w.Code)
	}
}

func TestStatus_NilSource_FallsBackToClosed(t *testing.T) {
	// With no source, status can still report version and a default breaker
	// state of "closed". Without cycle evidence, clickhouse_healthy is false
	// — we can't claim health we haven't observed.
	//
	// UptimeS is intentionally 0 on this path: uptime is read from
	// source.UpSince(), and with no source there's no start timestamp to
	// subtract from. Production always wires a source, so this only affects
	// the degenerate test/misuse case.
	srv := newTestServer(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	srv.status(w, req)

	var got statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status body invalid JSON: %v", err)
	}
	if got.CircuitBreaker != "closed" {
		t.Errorf("circuit_breaker = %q, want closed (default)", got.CircuitBreaker)
	}
	if got.ClickHouseHealthy {
		t.Error("clickhouse_healthy should be false when no cycle has been observed")
	}
	if got.UptimeS != 0 {
		t.Errorf("uptime_s = %d, want 0 when source is nil (no UpSince to compute from)", got.UptimeS)
	}
	if got.LastCycle != nil {
		t.Errorf("last_cycle should be absent with nil source, got %+v", got.LastCycle)
	}
}

func TestStatus_DoesNotLivePing(t *testing.T) {
	// Regression guard: /status MUST NOT call Pinger.IsHealthy on every
	// request — it infers ClickHouseHealthy from the breaker state so
	// dashboards polling /status don't stack 2s timeouts on a slow CH.
	p := &fakePinger{healthy: false}
	src := &fakeSource{
		cbState:  "closed",
		upSince:  time.Now(),
		haveLast: true,
		last:     CycleSnapshot{Exported: 1, At: time.Now()},
	}
	srv := newTestServer(p, src)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	srv.status(w, req)

	if p.calls != 0 {
		t.Errorf("pinger.IsHealthy called %d times from /status, want 0", p.calls)
	}

	var got statusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	// Closed breaker + a successful recent cycle → healthy, regardless of
	// what the pinger would say. The pinger is ignored by /status.
	if !got.ClickHouseHealthy {
		t.Error("clickhouse_healthy should be true when breaker is closed and last cycle succeeded")
	}
}

func TestNormalizeCBState(t *testing.T) {
	// Slice (not map) so failures report in a stable order — matches the
	// pattern used in TestCanonicalListenAddr.
	cases := []struct {
		in, want string
	}{
		{"", "closed"},
		{"closed", "closed"},
		{"open", "open"},
		{"half-open", "half_open"},
		{"half_open", "half_open"},
		{"weird", "weird"},
	}
	for _, c := range cases {
		if got := normalizeCBState(c.in); got != c.want {
			t.Errorf("normalizeCBState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRegisterHandlers_RoutesAllThree(t *testing.T) {
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed", upSince: time.Now()})
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	for _, path := range []string{"/healthz", "/readyz", "/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: code = %d, want 200", path, w.Code)
		}
	}
}

func TestStart_BindError(t *testing.T) {
	// Mirror metrics.TestStartMetricsServer_BindError. Bind failures must
	// surface as a returned error so main can clicklog.Fatal rather than
	// leave a silent goroutine exit behind a pod that serves no probes.
	srv := NewServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed"}, "test")
	_, err := Start("totally:not:a:port", srv)
	if err == nil {
		t.Fatal("expected bind error for invalid address, got nil")
	}
}

func TestStart_SetsBoundAddr(t *testing.T) {
	// Binding on :0 gives the OS-assigned port; srv.Addr must reflect it so
	// tests (and any future consumer) can dial the live listener without
	// threading the net.Listener separately.
	srv := NewServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed", upSince: time.Now()}, "test")
	httpSrv, err := Start("127.0.0.1:0", srv)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = httpSrv.Shutdown(context.Background())
	}()
	if httpSrv.Addr == "" {
		t.Fatal("srv.Addr is empty after bind; want the OS-assigned address")
	}
	if !strings.Contains(httpSrv.Addr, "127.0.0.1:") {
		t.Errorf("srv.Addr = %q, want 127.0.0.1:<port>", httpSrv.Addr)
	}
}

func TestRegisterHandlers_RejectsNonGET(t *testing.T) {
	// Probes should never be POSTed to; make sure a stray client doesn't
	// get a 200 for POST /healthz.
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed", upSince: time.Now()})
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	for _, path := range []string{"/healthz", "/readyz", "/status"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: code = %d, want 405", path, w.Code)
		}
	}
}
