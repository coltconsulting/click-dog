package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
)

// clusterCacheTTL bounds how stale a /clusterz response can be. Bursty
// pollers (HAProxy active-check, monitoring scrapes) collapse onto a single
// fanout within this window. A goroutine-driven refresher would be tidier
// but adds a lifecycle to manage; lazy revalidation on the request path is
// simpler and the read latency is dominated by the slowest peer anyway.
const clusterCacheTTL = 5 * time.Second

// peerReadBodyLimit caps how much of a peer's /readyz body we consume.
// /readyz responses are tiny JSON; the cap is purely a defense against a
// misbehaving (or hostile) peer that streams an unbounded body and pins
// memory until peer_timeout_ms expires.
const peerReadBodyLimit = 64 << 10 // 64 KiB

// ClusterSource provides leader/peer information to the /clusterz handler.
// It is satisfied in production by an adapter over leader.LeaderElection
// plus the configured static peer list (phase 1 of #19) — and in tests by
// hand-rolled stubs.
type ClusterSource interface {
	// IsLeader reports whether THIS node should serve /clusterz.
	// Returning false routes /clusterz to a 404. When HA is disabled, the
	// production adapter returns true unconditionally so single-node
	// deployments still get an aggregate endpoint.
	IsLeader() bool

	// Peers returns the host:port list to fan out /readyz against. Includes
	// the leader's own entry when present in the configured list — the
	// /clusterz handler matches it by string equality against ClusterConfig.Self
	// and fills that slot with an in-process /readyz instead of looping
	// through HTTP.
	Peers() []string
}

// ClusterConfig wires the /clusterz handler to its dependencies. Pass it
// to (*Server).EnableCluster to turn the endpoint on.
//
// Self is the response key for this node. When non-empty AND present in
// Source.Peers(), HTTP fanout skips that entry and uses the in-process
// /readyz result instead. When non-empty but NOT in Peers(), the in-process
// result is added as an extra entry under Self. (This handles single-node
// deployments where the operator did not bother enumerating peers.)
type ClusterConfig struct {
	Source      ClusterSource
	Self        string
	PeerTimeout time.Duration
}

// EnableCluster wires the /clusterz endpoint onto srv. RegisterHandlers
// mounts the route only after this has been called.
//
// Calling it more than once replaces the previous configuration; the
// production wiring calls it exactly once during startup.
func (s *Server) EnableCluster(cfg ClusterConfig) {
	if cfg.PeerTimeout <= 0 {
		// A zero deadline would expire immediately, so every peer would
		// race to "context deadline exceeded" before TCP completes. That's
		// almost certainly a bug at the call site; fall back to a value
		// that at least surfaces a readable response.
		clicklog.Warn("health.EnableCluster: PeerTimeout=%v is non-positive; defaulting to 3s", cfg.PeerTimeout)
		cfg.PeerTimeout = 3 * time.Second
	}
	// Surface a likely misconfiguration: operator listed peers but
	// forgot to put their own self entry in the list. The handler still
	// works (it adds self as an extra entry) but the response shows
	// total = len(peers) + 1, which can confuse dashboards. Single-node
	// deployments validly run with peers: [] — those don't get a warning.
	if cfg.Self != "" {
		peers := cfg.Source.Peers()
		if len(peers) > 0 {
			found := false
			for _, p := range peers {
				if p == cfg.Self {
					found = true
					break
				}
			}
			if !found {
				clicklog.Warn("health.cluster.self %q is not in health.cluster.peers — /clusterz will report total = len(peers) + 1; double-check the config if that's unexpected", cfg.Self)
			}
		}
	}
	s.cluster = &clusterServer{
		source:  cfg.Source,
		self:    cfg.Self,
		timeout: cfg.PeerTimeout,
		client: &http.Client{
			// Timeout here is belt-and-braces; the real per-peer deadline
			// is enforced via context on each request so cancellation
			// propagates through Read/Write rather than racing the client.
			Timeout: cfg.PeerTimeout + time.Second,
		},
	}
}

// clusterServer holds the runtime state for /clusterz: the source of
// leader/peer info, the per-peer HTTP client, and the cached aggregate.
type clusterServer struct {
	source  ClusterSource
	self    string
	timeout time.Duration
	client  *http.Client

	// mu guards every cache field below. The mutex is held only across
	// short reads/writes — the actual fanout runs unlocked so concurrent
	// /clusterz requests during a cache miss don't serialize. Doing two
	// fanouts back-to-back is a wasted pair of round-trips but never
	// produces an inconsistent response, which is the trade we want.
	//
	// TODO(phase 2): if bursty polling against the leader during a
	// cache miss becomes measurable, wrap the fanout in a
	// `golang.org/x/sync/singleflight.Group` keyed by the empty string
	// — collapses N concurrent miss-triggers into one fanout with no
	// correctness change. Skipped for phase 1 because peer counts are
	// small and the wasted-fanout cost is observable but not painful.
	//
	// We cache the pre-encoded JSON bytes rather than the response
	// struct. Caching the struct would alias its `Nodes` map across
	// concurrent readers — safe today (the miss path swaps the cache
	// pointer instead of mutating the old map) but a latent footgun if
	// a future maintainer ever updates the entry in place. Bytes are
	// trivially copy-on-write at the slice level, and the encode work
	// happens once per TTL window instead of once per request.
	mu        sync.Mutex
	cacheBody []byte
	cacheCode int
	cachePrim bool // true once cacheBody/cacheCode have been populated
	cacheAt   time.Time
}

// clusterResponse is the JSON shape returned by /clusterz. The structure
// is part of the public API — see docs/observability.md for the operator-
// facing contract. Field order in the struct (status, leader, nodes,
// summary) matches the documented response so the JSON read order is
// stable for human-eyes-on debugging.
type clusterResponse struct {
	Status  string                `json:"status"`
	Leader  string                `json:"leader,omitempty"`
	Nodes   map[string]nodeResult `json:"nodes"`
	Summary clusterSummary        `json:"summary"`
}

// nodeResult is one node's contribution. Status is one of:
//   - "ok"          — peer responded 200 with a valid /readyz body.
//   - "degraded"    — peer responded but not 200, or with an invalid body.
//   - "unreachable" — peer never responded (timeout, connection refused, …).
//
// Checks is the per-dependency map echoed from the peer's /readyz; absent
// for unreachable peers (nothing to echo). Error carries the dial / read
// error string for unreachable peers — operators want it inline so they
// don't have to cross-reference logs.
type nodeResult struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
	Error  string            `json:"error,omitempty"`
}

type clusterSummary struct {
	Total       int `json:"total"`
	Healthy     int `json:"healthy"`
	Degraded    int `json:"degraded"`
	Unreachable int `json:"unreachable"`
}

// clusterz is the HTTP handler for /clusterz. Mounted by RegisterHandlers
// only when EnableCluster has been called.
func (s *Server) clusterz(w http.ResponseWriter, r *http.Request) {
	cs := s.cluster
	if cs == nil {
		// Should be unreachable: RegisterHandlers gates registration on
		// cs != nil. Keep the guard so a future code path that mounts
		// the handler directly doesn't panic on a nil dereference.
		http.NotFound(w, r)
		return
	}

	if !cs.source.IsLeader() {
		// Phase 1: the leader's address is unknown to followers (Keeper
		// payload schema is a phase-2 deliverable). The Location header
		// is therefore omitted; the contract still allows it for phase 2
		// to fill in without changing this branch.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not_leader"})
		return
	}

	// Cache-hit fast path: short critical section, copy the cached body
	// out, release the lock before writing the response so a slow client
	// doesn't block other concurrent /clusterz callers. The slice header
	// copy is enough — the underlying bytes are immutable for the cache's
	// lifetime (the miss path always allocates a fresh slice).
	cs.mu.Lock()
	if cs.cachePrim && time.Since(cs.cacheAt) < clusterCacheTTL {
		body := cs.cacheBody
		code := cs.cacheCode
		cs.mu.Unlock()
		writeCachedJSON(w, code, body)
		return
	}
	cs.mu.Unlock()

	// Detach the fanout from the HTTP request context. If the triggering
	// client disconnects (or hits its own deadline) before the fanout
	// finishes, propagating that cancellation would mark every in-flight
	// peer "unreachable" and cache the false outage for clusterCacheTTL.
	// Per-peer deadlines are still enforced via cs.timeout below.
	//
	// Trade-off: in-flight fanouts cannot be canceled by graceful
	// shutdown either, so the worst-case shutdown drain is one
	// peer_timeout_ms (3s by default) per in-flight /clusterz call.
	// Acceptable for a health endpoint. If a future maintainer wires
	// a server-level shutdown signal here, replace context.Background()
	// with a context derived from that signal — keep the request
	// context out of the loop either way.
	resp, code := s.computeClusterResponse(context.Background())
	body, err := json.Marshal(resp)
	if err != nil {
		// json.Marshal on a struct of strings/ints/maps cannot fail in
		// practice, but if it ever does we don't want to cache a partial
		// response. Fall back to writeJSON which logs the encode error
		// and serves a non-cached degraded response.
		clicklog.Warn("clusterz: failed to encode response: %v", err)
		writeJSON(w, code, resp)
		return
	}
	body = append(body, '\n') // match writeJSON's trailing newline so output is byte-identical to a non-cached response.

	cs.mu.Lock()
	cs.cacheBody = body
	cs.cacheCode = code
	cs.cachePrim = true
	cs.cacheAt = time.Now()
	cs.mu.Unlock()

	writeCachedJSON(w, code, body)
}

// writeCachedJSON writes a pre-encoded JSON body. Distinct from writeJSON
// because we have raw bytes, not a value to encode, and we want to avoid
// re-marshalling on every cache hit.
func writeCachedJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(body); err != nil {
		clicklog.Warn("clusterz: failed to write response: %v", err)
	}
}

// computeClusterResponse runs the parallel fanout to all peers (skipping
// self) plus the in-process self check, and folds the results into a
// clusterResponse with a populated summary.
func (s *Server) computeClusterResponse(ctx context.Context) (clusterResponse, int) {
	cs := s.cluster
	peers := cs.source.Peers()

	nodes := make(map[string]nodeResult, len(peers)+1)
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Self-check: in-process /readyz. No HTTP self-loop — saves a TCP
	// round-trip and avoids the cycle where /clusterz on a slow listener
	// blocks waiting for its own /readyz.
	//
	// Runs concurrently with the peer fanout, not before it. The
	// readyz evaluator drives a ClickHouse ping bounded by readyTimeout
	// (2s by default), and peer fetches are bounded by peerTimeout —
	// the two deadlines are independent. Sequencing self before the
	// peer goroutines would stack them, making worst-case /clusterz
	// latency readyTimeout + peerTimeout instead of the
	// max(readyTimeout, peerTimeout) operators expect after tuning
	// peer_timeout_ms.
	if cs.self != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rz := s.evaluateReadyz(ctx)
			n := readyzToNode(rz)
			mu.Lock()
			nodes[cs.self] = n
			mu.Unlock()
		}()
	}

	for _, p := range peers {
		if p == cs.self {
			continue
		}
		peer := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := cs.fetchPeer(ctx, peer)
			mu.Lock()
			nodes[peer] = n
			mu.Unlock()
		}()
	}
	wg.Wait()

	summary := clusterSummary{}
	for _, n := range nodes {
		summary.Total++
		switch n.Status {
		case "ok":
			summary.Healthy++
		case "unreachable":
			summary.Unreachable++
		default:
			summary.Degraded++
		}
	}

	overall := "ok"
	code := http.StatusOK
	if summary.Healthy != summary.Total {
		overall = "degraded"
		code = http.StatusServiceUnavailable
	}

	return clusterResponse{
		Status:  overall,
		Leader:  cs.self,
		Nodes:   nodes,
		Summary: summary,
	}, code
}

// fetchPeer issues a single GET <peer>/readyz with the per-peer deadline.
// Any failure (dial, read, decode) is folded into a nodeResult — the
// fanout never propagates errors upward; instead it surfaces them on the
// individual node entry so operators can see which peer is misbehaving
// without re-running the probe by hand.
func (cs *clusterServer) fetchPeer(parent context.Context, addr string) nodeResult {
	ctx, cancel := context.WithTimeout(parent, cs.timeout)
	defer cancel()

	// Phase 1 (#19): scheme is hardcoded to http://. /readyz has no
	// authentication or TLS today, so the leader's fanout inherits the
	// same posture. Phase 2 will add a configurable scheme alongside
	// Keeper-derived peer discovery — flagged here so the assumption
	// doesn't get forgotten.
	url := fmt.Sprintf("http://%s/readyz", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nodeResult{Status: "unreachable", Error: err.Error()}
	}

	resp, err := cs.client.Do(req)
	if err != nil {
		return nodeResult{Status: "unreachable", Error: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, peerReadBodyLimit))
	if err != nil {
		return nodeResult{Status: "unreachable", Error: err.Error()}
	}

	var rz readyzResult
	if jsonErr := json.Unmarshal(body, &rz); jsonErr != nil {
		// Non-JSON or wrong shape — peer is not a click-dog /readyz
		// endpoint or it's misbehaving. Mark degraded so operators see
		// the anomaly without us inventing a synthetic ok. The decode
		// error gets wrapped in for diagnostics: "unexpected end of
		// JSON input" tells the operator they hit the body-size cap;
		// "invalid character ..." tells them the peer returned HTML.
		return nodeResult{Status: "degraded", Error: fmt.Sprintf("invalid readyz response: %v", jsonErr)}
	}

	if resp.StatusCode == http.StatusOK {
		return nodeResult{Status: "ok", Checks: rz.Checks}
	}
	return nodeResult{Status: "degraded", Checks: rz.Checks}
}

// readyzToNode converts an in-process /readyz outcome into the same node
// shape that fetchPeer produces, so the response is uniform regardless of
// whether the entry came from HTTP or in-process evaluation.
func readyzToNode(rz readyzResult) nodeResult {
	if rz.Status == "ready" {
		return nodeResult{Status: "ok", Checks: rz.Checks}
	}
	return nodeResult{Status: "degraded", Checks: rz.Checks}
}
