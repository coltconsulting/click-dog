// Package health provides HTTP liveness, readiness, and status endpoints.
//
// Three endpoints are exposed:
//   - GET /healthz — liveness; 200 whenever the server is responsive.
//   - GET /readyz  — readiness; 200 when ClickHouse is reachable AND the
//     circuit breaker is closed; 503 otherwise.
//   - GET /status  — JSON snapshot of circuit breaker state, current backoff,
//     uptime, and the last cycle's counts/duration.
//
// The package is intentionally dependency-free of the top-level binary: it
// consumes small interfaces (Pinger, CycleSource) so that tests and alternate
// readiness sources can plug in without pulling in the whole ClickHouse reader.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/model"
)

// DefaultReadyTimeout is the per-request deadline for dependency pings
// (currently ClickHouse) during /readyz handling. Exported so callers can
// see the contract; also referenced from the docs.
const DefaultReadyTimeout = 2 * time.Second

// Pinger verifies an upstream dependency is reachable within a bounded time.
// In production this is wrapped around clickhouse.ClickHouseReader.IsHealthy.
type Pinger interface {
	IsHealthy(ctx context.Context) bool
}

// CycleSource returns a single atomic snapshot of everything /status and
// /readyz need. A single call ensures all fields come from the same lock
// window — without this, three sequential RLock acquisitions could return
// values from three different states if a cycle updated them in between.
// CircuitBreakerState may contain any hyphenated or underscored variant;
// normalizeCBState canonicalizes to the underscore form used in JSON
// responses and dashboards.
type CycleSource interface {
	Snapshot() Snapshot
}

// Snapshot mirrors metrics.Snapshot without an import cycle. Callers
// construct one at the package boundary via an adapter.
//
// IMPORTANT: Keep in sync with metrics.Snapshot. Adding a field here
// also requires updating the metrics struct and the adapter in
// health_adapter.go — the adapter does a manual field-by-field copy
// and silently drops any field you forget.
type Snapshot struct {
	LastCycle           CycleSnapshot
	HaveLastCycle       bool
	CircuitBreakerState string
	BackoffIntervalSecs float64
	Leader              int
	UpSince             time.Time
	// TopologyWarning is the active topology-audit reason (empty when clean).
	// Surfaced at /status only; it must NOT influence ClickHouseHealthy/readiness.
	TopologyWarning string
}

// normalizeCBState returns the canonical external spelling of a circuit
// breaker state. Empty → "closed" (the initial state before any cycle
// runs); "half-open" (as emitted by resilience.CircuitState.String) →
// "half_open" (the spelling used elsewhere in metrics and API output).
//
// In the production path (metricsCycleSource → metrics.CircuitBreakerState)
// the value is already canonicalised at the write side in
// metrics.SetCircuitBreakerState, so the "half-open" case here is
// defense-in-depth for alternate CycleSource implementations (tests, or a
// future source that feeds raw resilience.CircuitState.String output).
func normalizeCBState(s string) string {
	switch s {
	case "":
		return "closed"
	case "half-open":
		return "half_open"
	default:
		return s
	}
}

// writeJSON centralises JSON responses across /healthz, /readyz, and
// /status. json.NewEncoder.Encode appends a trailing "\n" to every body
// (tests strip it with strings.TrimSpace). An Encode failure after
// WriteHeader leaves the client with a truncated body and a status
// already flushed, so log at Warn rather than swallow silently.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		clicklog.Warn("health: failed to encode response: %v", err)
	}
}

// CycleSnapshot mirrors metrics.CycleSnapshot without creating an import
// cycle; callers are expected to convert at the boundary.
//
// Skipped=true means the cycle was short-circuited (breaker open, no
// canary) without touching ClickHouse. /status renders it as a skip so
// clickhouse_healthy doesn't flip green on a "cycle" that proved nothing.
//
// IMPORTANT: Keep in sync with metrics.CycleSnapshot — see the matching
// note on Snapshot above. The adapter doesn't fail at compile time if a
// field goes missing on one side.
type CycleSnapshot struct {
	Exported   int
	Filtered   int
	Duplicates int
	DurationMs int64
	Err        string
	Skipped    bool
	SkipReason string
	At         time.Time
}

// Server wires the health handlers onto a mux. The server does not own a
// listener — main.go is responsible for choosing whether to share the
// metrics listener or start a dedicated one.
type Server struct {
	pinger       Pinger
	source       CycleSource
	version      string
	readyTimeout time.Duration

	// cluster is non-nil only when EnableCluster has been called. It gates
	// whether RegisterHandlers mounts /clusterz.
	cluster *clusterServer
}

// NewServer constructs a Server. pinger and source may be nil if the
// corresponding signal is unavailable; readyz then degrades to reporting
// whatever it does have.
//
// If both are nil, /readyz has nothing to check and will report 200 with
// an empty `checks` map — which is almost certainly a misconfiguration in
// production (the probe can never fail, even when click-dog is broken).
// A Warn log at construction time makes that visible at startup rather
// than requiring operators to notice that their readiness probe is
// trivially always-green.
func NewServer(pinger Pinger, source CycleSource, version string) *Server {
	if pinger == nil && source == nil {
		clicklog.Warn("health.NewServer: both pinger and source are nil; /readyz will always return 200 with no checks")
	}
	return &Server{
		pinger:       pinger,
		source:       source,
		version:      version,
		readyTimeout: DefaultReadyTimeout,
	}
}

// RegisterHandlers mounts /healthz, /readyz, and /status onto mux. /clusterz
// is mounted only when EnableCluster has been called — it's a no-op
// otherwise, since serving an unconfigured cluster endpoint would hand
// operators a perpetually-empty aggregate.
//
// Uses Go 1.22+ method-qualified patterns so non-GET requests get a 405
// instead of being silently treated as GETs.
func (s *Server) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /status", s.status)
	if s.cluster != nil {
		mux.HandleFunc("GET /clusterz", s.clusterz)
	}
}

// healthz is a pure liveness probe: if the goroutine servicing HTTP is
// responsive enough to run this handler, we're alive.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyzResult is the in-process /readyz outcome. The JSON byte order
// matters: the previous implementation built the body from a
// map[string]any whose keys serialize alphabetically, so existing
// consumers see "checks" first and "status" second. Field order in this
// struct preserves that contract — flip Checks above Status here and
// the HTTP body changes for any literal-string matcher in the wild.
//
// The same struct is used both to render the HTTP response and to
// embed the leader's self-check inside a /clusterz aggregate without
// going through HTTP.
type readyzResult struct {
	Checks map[string]string `json:"checks"`
	Status string            `json:"status"`
}

// evaluateReadyz computes a /readyz result without touching net/http. It
// is the single source of truth for readiness logic — the HTTP handler and
// the /clusterz self-check both call it, so the two paths can never drift.
func (s *Server) evaluateReadyz(parent context.Context) (int, readyzResult) {
	res := readyzResult{Checks: map[string]string{}}
	ok := true

	if s.pinger != nil {
		ctx, cancel := context.WithTimeout(parent, s.readyTimeout)
		defer cancel()
		if s.pinger.IsHealthy(ctx) {
			res.Checks["clickhouse"] = "ok"
		} else {
			res.Checks["clickhouse"] = "unreachable"
			ok = false
		}
	}

	if s.source != nil {
		// Canonicalisation is enforced at the write side in
		// metrics.SetCircuitBreakerState, so the production path already
		// sees the underscore form. normalizeCBState here is defense in
		// depth for alternate CycleSource implementations — keep both so
		// a future source that emits the hyphenated form still produces
		// stable JSON.
		state := normalizeCBState(s.source.Snapshot().CircuitBreakerState)
		res.Checks["circuit_breaker"] = state
		if state != "closed" {
			ok = false
		}
	}

	if ok {
		res.Status = "ready"
		return http.StatusOK, res
	}
	res.Status = "not_ready"
	return http.StatusServiceUnavailable, res
}

// readyz reports 200 only when ClickHouse is reachable (when a pinger is
// provided) AND the circuit breaker is closed (when a source is provided).
// A 503 body includes which checks failed so operators don't need to cross
// reference logs.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	code, res := s.evaluateReadyz(r.Context())
	writeJSON(w, code, res)
}

// statusResponse is the JSON shape returned by /status. Keys are stable
// public API — downstream dashboards depend on them.
type statusResponse struct {
	Version           string         `json:"version"`
	UptimeS           int64          `json:"uptime_s"`
	ClickHouseHealthy bool           `json:"clickhouse_healthy"`
	Standby           bool           `json:"standby,omitempty"`
	CircuitBreaker    string         `json:"circuit_breaker"`
	BackoffIntervalS  float64        `json:"backoff_interval_s"`
	LastCycle         *lastCycleJSON `json:"last_cycle,omitempty"`
	// TopologyWarning surfaces the topology self-audit reason when the sidecar +
	// use_cluster_queries anti-pattern is detected; absent when clean. Advisory
	// only — it does NOT gate clickhouse_healthy or /readyz.
	TopologyWarning string `json:"topology_warning,omitempty"`
}

type lastCycleJSON struct {
	Exported   int    `json:"exported"`
	Filtered   int    `json:"filtered"`
	Duplicates int    `json:"duplicates"`
	DurationMs int64  `json:"duration_ms"`
	Err        string `json:"error,omitempty"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	At         string `json:"at,omitempty"`
}

// status returns a JSON snapshot of click-dog's current operational state.
// Unlike /readyz, /status does NOT issue a live ClickHouse ping — it infers
// clickhouse_healthy from the circuit breaker state so dashboards can poll
// this endpoint without stacking up 2s timeouts when ClickHouse is slow.
//
// clickhouse_healthy requires a closed circuit breaker plus either a successful
// real cycle or an explicit leader-standby skip. Breaker-open skips still do
// not count as health evidence, but a long-lived HA follower is healthy when it
// is intentionally idle behind the leader gate.
func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	resp := statusResponse{Version: s.version}
	cbClosed := true
	cycleOK := false
	standby := false

	if s.source != nil {
		// One call, one lock window: the five fields below are guaranteed
		// mutually consistent. A cycle landing mid-snapshot won't be able
		// to produce a "closed" breaker with a stale backoff or vice versa.
		snap := s.source.Snapshot()
		resp.UptimeS = int64(time.Since(snap.UpSince).Seconds())
		resp.CircuitBreaker = normalizeCBState(snap.CircuitBreakerState)
		resp.BackoffIntervalS = snap.BackoffIntervalSecs
		resp.TopologyWarning = snap.TopologyWarning // advisory; omitempty when clean
		cbClosed = resp.CircuitBreaker == "closed"

		if snap.HaveLastCycle {
			lc := snap.LastCycle
			standby = lc.Skipped && lc.SkipReason == model.CycleSkipReasonLeaderStandby
			cycleOK = lc.Err == "" && (!lc.Skipped || standby)
			resp.LastCycle = &lastCycleJSON{
				Exported:   lc.Exported,
				Filtered:   lc.Filtered,
				Duplicates: lc.Duplicates,
				DurationMs: lc.DurationMs,
				Err:        lc.Err,
				Skipped:    lc.Skipped,
				SkipReason: lc.SkipReason,
			}
			if !lc.At.IsZero() {
				resp.LastCycle.At = lc.At.UTC().Format(time.RFC3339)
			}
		}
	} else {
		resp.CircuitBreaker = "closed"
	}
	resp.Standby = standby
	resp.ClickHouseHealthy = cbClosed && cycleOK

	writeJSON(w, http.StatusOK, resp)
}

// Start creates an http.Server listening on addr and spawns a goroutine to
// serve health endpoints. The returned server should have Shutdown called
// on program exit.
//
// The bind is performed synchronously (via net.Listen) so a port collision
// or invalid address surfaces as a startup error in main, not as a silent
// goroutine exit. Without this, a failed bind would leave the pod running
// but answering no probes — worst-possible K8s behavior.
func Start(addr string, s *Server) (*http.Server, error) {
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("health: bind %s: %w", addr, err)
	}

	srv := &http.Server{
		// Surface the actual bound address so callers (and tests binding
		// on ":0") can read the OS-assigned port from srv.Addr. Mirrors
		// metrics.StartMetricsServer.
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		// Kubernetes probes reuse keep-alive connections; bound idle time so
		// stale sockets get reclaimed instead of accumulating across rollouts.
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		clicklog.Info("Health server listening on %s", ln.Addr())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			clicklog.Error("Health server error: %v", err)
		}
	}()

	return srv, nil
}
