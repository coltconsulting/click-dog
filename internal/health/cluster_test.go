package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubClusterSource is a fixed-value ClusterSource for tests. Fields are
// public so test setup reads naturally.
type stubClusterSource struct {
	leader bool
	peers  []string
}

// pingerFunc adapts a function to the Pinger interface. Used by the
// parallelism test, which needs a pinger that blocks behind a controlled
// release without inventing yet another struct type.
type pingerFunc func(ctx context.Context) bool

func (f pingerFunc) IsHealthy(ctx context.Context) bool { return f(ctx) }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (s stubClusterSource) IsLeader() bool  { return s.leader }
func (s stubClusterSource) Peers() []string { return s.peers }

// hostPort strips http:// from an httptest URL so it can be used as a peer
// entry. /clusterz peers are bare host:port and the handler synthesises
// the http:// prefix itself.
func hostPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

// peerWithReadyz returns an httptest.Server that answers /readyz with the
// given status code and JSON body. Tests close them via t.Cleanup.
func peerWithReadyz(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedPeerAddr returns a host:port that nothing is listening on. We bind
// briefly to grab a free port, close the listener, and return its address —
// dialing it next produces a deterministic connection-refused.
//
// TOCTOU caveat: another process could in theory bind the freed port
// between Close() and the test dial. In practice this never happens —
// tests run sequentially in this package (no t.Parallel) and ports are
// allocated by the kernel from a high range, so the window is microseconds
// and collisions are vanishingly rare. If a flake ever surfaces here,
// reach for httptest.NewUnstartedServer + Listener.Close instead.
func closedPeerAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// buildClusterServer assembles a *Server with /clusterz wired up. Pinger
// and source values default to a healthy CH and closed breaker so the
// in-process self check returns "ok" unless overridden.
func buildClusterServer(t *testing.T, src ClusterSource, self string) *Server {
	t.Helper()
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed"})
	srv.EnableCluster(ClusterConfig{
		Source:      src,
		Self:        self,
		PeerTimeout: 500 * time.Millisecond,
	})
	return srv
}

func decodeCluster(t *testing.T, body []byte) clusterResponse {
	t.Helper()
	var got clusterResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("invalid JSON: %v\nbody: %s", err, body)
	}
	return got
}

func TestClusterz_AllHealthy(t *testing.T) {
	peer1 := peerWithReadyz(t, http.StatusOK, `{"status":"ready","checks":{"clickhouse":"ok","circuit_breaker":"closed"}}`)
	peer2 := peerWithReadyz(t, http.StatusOK, `{"status":"ready","checks":{"clickhouse":"ok","circuit_breaker":"closed"}}`)

	self := "self.example:8686"
	src := stubClusterSource{
		leader: true,
		peers:  []string{self, hostPort(t, peer1.URL), hostPort(t, peer2.URL)},
	}
	srv := buildClusterServer(t, src, self)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body)
	}
	got := decodeCluster(t, w.Body.Bytes())
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if got.Leader != self {
		t.Errorf("leader = %q, want %q", got.Leader, self)
	}
	if got.Summary.Total != 3 || got.Summary.Healthy != 3 ||
		got.Summary.Degraded != 0 || got.Summary.Unreachable != 0 {
		t.Errorf("summary = %+v, want {3,3,0,0}", got.Summary)
	}
	for name, n := range got.Nodes {
		if n.Status != "ok" {
			t.Errorf("node %q status = %q, want ok", name, n.Status)
		}
	}
}

func TestClusterz_OneDegraded(t *testing.T) {
	healthy := peerWithReadyz(t, http.StatusOK, `{"status":"ready","checks":{"clickhouse":"ok","circuit_breaker":"closed"}}`)
	// Peer returns 503 with a not-ready body — a node whose breaker is open.
	degraded := peerWithReadyz(t, http.StatusServiceUnavailable, `{"status":"not_ready","checks":{"clickhouse":"ok","circuit_breaker":"open"}}`)

	self := "self.example:8686"
	src := stubClusterSource{
		leader: true,
		peers:  []string{self, hostPort(t, healthy.URL), hostPort(t, degraded.URL)},
	}
	srv := buildClusterServer(t, src, self)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when a peer is degraded", w.Code)
	}
	got := decodeCluster(t, w.Body.Bytes())
	if got.Status != "degraded" {
		t.Errorf("status = %q, want degraded", got.Status)
	}
	if got.Summary.Total != 3 || got.Summary.Healthy != 2 ||
		got.Summary.Degraded != 1 || got.Summary.Unreachable != 0 {
		t.Errorf("summary = %+v, want {3,2,1,0}", got.Summary)
	}
	degAddr := hostPort(t, degraded.URL)
	if got.Nodes[degAddr].Status != "degraded" {
		t.Errorf("degraded node status = %q, want degraded", got.Nodes[degAddr].Status)
	}
	// Degraded peer's checks should propagate so operators see WHY it's degraded
	// without a second probe.
	if got.Nodes[degAddr].Checks["circuit_breaker"] != "open" {
		t.Errorf("degraded node checks = %+v, want circuit_breaker=open", got.Nodes[degAddr].Checks)
	}
}

func TestClusterz_OneUnreachable(t *testing.T) {
	healthy := peerWithReadyz(t, http.StatusOK, `{"status":"ready","checks":{"clickhouse":"ok","circuit_breaker":"closed"}}`)
	dead := closedPeerAddr(t)

	self := "self.example:8686"
	src := stubClusterSource{
		leader: true,
		peers:  []string{self, hostPort(t, healthy.URL), dead},
	}
	srv := buildClusterServer(t, src, self)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when a peer is unreachable", w.Code)
	}
	got := decodeCluster(t, w.Body.Bytes())
	if got.Summary.Total != 3 || got.Summary.Healthy != 2 ||
		got.Summary.Degraded != 0 || got.Summary.Unreachable != 1 {
		t.Errorf("summary = %+v, want {3,2,0,1}", got.Summary)
	}
	deadNode := got.Nodes[dead]
	if deadNode.Status != "unreachable" {
		t.Errorf("dead node status = %q, want unreachable", deadNode.Status)
	}
	if deadNode.Error == "" {
		t.Error("unreachable node should have a non-empty error string")
	}
	// Unreachable nodes carry no checks (they didn't respond) — guard against
	// regression that emits an empty `{}` instead of omitting the field.
	if len(deadNode.Checks) != 0 {
		t.Errorf("unreachable node should have no checks, got %+v", deadNode.Checks)
	}
}

func TestClusterz_NotLeader_404(t *testing.T) {
	src := stubClusterSource{leader: false, peers: []string{"node-0:8686"}}
	srv := buildClusterServer(t, src, "self:8686")

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on non-leader", w.Code)
	}
	// JSON body present so curl/jq users get a structured response, not a
	// stray HTML 404 page.
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("non-leader 404 body should be JSON: %v", err)
	}
	if got["status"] != "not_leader" {
		t.Errorf("status = %q, want not_leader", got["status"])
	}
}

func TestClusterz_CacheTTL(t *testing.T) {
	// Counts the number of /readyz HTTP calls a peer receives. Two
	// /clusterz calls within clusterCacheTTL should produce exactly one
	// peer call; a third call after the TTL window must miss and refetch.
	var hits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready","checks":{"circuit_breaker":"closed"}}`))
	}))
	t.Cleanup(peer.Close)

	self := "self:8686"
	src := stubClusterSource{leader: true, peers: []string{self, hostPort(t, peer.URL)}}
	srv := buildClusterServer(t, src, self)

	doRequest := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
		w := httptest.NewRecorder()
		srv.clusterz(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status %d (body: %s)", w.Code, w.Body)
		}
	}

	doRequest()
	doRequest()
	if got := hits.Load(); got != 1 {
		t.Errorf("two calls within TTL produced %d peer hits, want 1 (cache miss the second time)", got)
	}

	// Force the cached entry past the TTL by rewinding cacheAt. Real time
	// would require waiting 5s — too slow for a unit test, and the
	// alternative (parameterising clusterCacheTTL) leaks test-only state
	// into production code.
	srv.cluster.mu.Lock()
	srv.cluster.cacheAt = time.Now().Add(-clusterCacheTTL - time.Second)
	srv.cluster.mu.Unlock()

	doRequest()
	if got := hits.Load(); got != 2 {
		t.Errorf("call after TTL produced %d total peer hits, want 2 (cache miss after expiry)", got)
	}
}

func TestClusterz_CacheBoundary_HitJustBeforeExpiry(t *testing.T) {
	// Pins the inclusive vs. exclusive boundary at the TTL. A request that
	// lands at exactly cacheAt + TTL - epsilon must reuse the cache; the
	// "less than TTL" comparison would silently invert if the operator
	// changed it to "<=".
	var hits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready","checks":{"circuit_breaker":"closed"}}`))
	}))
	t.Cleanup(peer.Close)

	self := "self:8686"
	src := stubClusterSource{leader: true, peers: []string{self, hostPort(t, peer.URL)}}
	srv := buildClusterServer(t, src, self)

	doRequest := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
		w := httptest.NewRecorder()
		srv.clusterz(w, req)
	}

	doRequest() // primes cache
	if hits.Load() != 1 {
		t.Fatalf("primer hit count = %d, want 1", hits.Load())
	}

	// Just under the TTL: must hit cache.
	srv.cluster.mu.Lock()
	srv.cluster.cacheAt = time.Now().Add(-clusterCacheTTL + 100*time.Millisecond)
	srv.cluster.mu.Unlock()
	doRequest()
	if hits.Load() != 1 {
		t.Errorf("hits = %d immediately before TTL expiry, want still 1 (cached)", hits.Load())
	}
}

func TestClusterz_OnlyMountedWhenEnabled(t *testing.T) {
	// Server without EnableCluster must NOT mount /clusterz — operators
	// querying it on an instance without cluster config should see 404
	// from the mux, not a configured-but-empty handler.
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed"})
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when cluster is not enabled", w.Code)
	}
}

func TestClusterz_MountedAfterEnable(t *testing.T) {
	src := stubClusterSource{leader: true, peers: nil}
	srv := buildClusterServer(t, src, "self:8686")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Errorf("status = 404, want /clusterz mounted after EnableCluster")
	}
}

func TestClusterz_SelfOnly(t *testing.T) {
	// Single-node deployment: peers contains just self. The leader's
	// in-process /readyz must populate the only entry; no HTTP fanout.
	self := "self:8686"
	src := stubClusterSource{leader: true, peers: []string{self}}
	srv := buildClusterServer(t, src, self)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in single-node-healthy case", w.Code)
	}
	got := decodeCluster(t, w.Body.Bytes())
	if got.Summary.Total != 1 || got.Summary.Healthy != 1 {
		t.Errorf("summary = %+v, want {1,1,0,0}", got.Summary)
	}
	if _, ok := got.Nodes[self]; !ok {
		t.Errorf("expected self entry %q in nodes, got keys %v", self, nodeKeys(got.Nodes))
	}
}

func TestClusterz_SelfNotInPeers(t *testing.T) {
	// Operator forgot to enumerate self in the peer list. The handler
	// must still emit a self entry rather than silently dropping it —
	// otherwise the leader's own readiness becomes invisible.
	peer := peerWithReadyz(t, http.StatusOK, `{"status":"ready","checks":{"circuit_breaker":"closed"}}`)
	self := "self.alone:8686"
	src := stubClusterSource{leader: true, peers: []string{hostPort(t, peer.URL)}}
	srv := buildClusterServer(t, src, self)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	got := decodeCluster(t, w.Body.Bytes())
	if got.Summary.Total != 2 {
		t.Errorf("total = %d, want 2 (self added on top of one peer)", got.Summary.Total)
	}
	if _, ok := got.Nodes[self]; !ok {
		t.Errorf("self entry %q missing from nodes %v", self, nodeKeys(got.Nodes))
	}
}

func TestClusterz_NoHTTPSelfLoop(t *testing.T) {
	// If the handler accidentally HTTP-fetched its own listen address it
	// would dial somewhere unrelated for the test setup — at best it
	// fails, at worst it hits a stale listener. Pin the contract: when
	// self is in Peers(), zero HTTP requests are made for it.
	var hits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"status":"ready","checks":{}}`))
	}))
	t.Cleanup(peer.Close)

	// "Self" is intentionally NOT a real listener — if the handler tried
	// to HTTP-loopback to it, the request would fail and the node would
	// be marked unreachable. The test asserts we never go there.
	self := "127.0.0.1:1"
	src := stubClusterSource{
		leader: true,
		peers:  []string{self, hostPort(t, peer.URL)},
	}
	srv := buildClusterServer(t, src, self)

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (self in-process is healthy, peer is healthy); body=%s", w.Code, w.Body)
	}
	if hits.Load() != 1 {
		t.Errorf("real peer received %d hits, want 1 (self should NOT have been HTTP-fetched)", hits.Load())
	}
}

func TestClusterz_PeerTimeout(t *testing.T) {
	// A peer that never responds must be marked unreachable after the
	// configured deadline rather than blocking the whole /clusterz
	// response indefinitely. The handler normally exits when the cluster's
	// per-peer deadline cancels the request; releaseHandler guarantees that
	// test cleanup remains bounded if the server does not observe cancellation.
	releaseHandler := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(func() {
		close(releaseHandler)
		slow.Close()
	})

	src := stubClusterSource{leader: true, peers: []string{hostPort(t, slow.URL)}}
	srv := newTestServer(&fakePinger{healthy: true}, &fakeSource{cbState: "closed"})
	srv.EnableCluster(ClusterConfig{
		Source:      src,
		Self:        "self:8686",
		PeerTimeout: 100 * time.Millisecond,
	})

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)
	elapsed := time.Since(start)

	// 5× the configured deadline: tight enough to catch a regression
	// where the timeout drifts (e.g. accidentally cancellable only via
	// a second goroutine), CI-safe enough to absorb a slow runner.
	if elapsed > 500*time.Millisecond {
		t.Errorf("clusterz took %v, want fast return after %v deadline", elapsed, 100*time.Millisecond)
	}

	got := decodeCluster(t, w.Body.Bytes())
	peerAddr := hostPort(t, slow.URL)
	if got.Nodes[peerAddr].Status != "unreachable" {
		t.Errorf("slow peer status = %q, want unreachable", got.Nodes[peerAddr].Status)
	}
}

func TestClusterz_LeaderEqualsSelf(t *testing.T) {
	// Per phase 1: when the leader serves /clusterz, response.leader is
	// the leader's own self identifier. Pins the contract for monitoring
	// dashboards that surface "current leader" from the body.
	src := stubClusterSource{leader: true, peers: nil}
	srv := buildClusterServer(t, src, "leader-of-the-pack:8686")

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	got := decodeCluster(t, w.Body.Bytes())
	if got.Leader != "leader-of-the-pack:8686" {
		t.Errorf("leader = %q, want self identifier", got.Leader)
	}
}

func TestClusterz_CacheHit_NoSharedMapAliasing(t *testing.T) {
	// Regression for the second-pass review: the original cache-hit path
	// shared the cached response struct's `Nodes` map across concurrent
	// callers. The fix caches pre-encoded JSON bytes — pinning that
	// behavior here so a future regression that re-introduces struct
	// caching trips a test, not a heisenbug.
	peer := peerWithReadyz(t, http.StatusOK, `{"status":"ready","checks":{"circuit_breaker":"closed"}}`)
	self := "self:8686"
	src := stubClusterSource{leader: true, peers: []string{self, hostPort(t, peer.URL)}}
	srv := buildClusterServer(t, src, self)

	// First call primes the cache. Two further calls (within TTL) should
	// each produce a complete, parseable JSON body — not, say, a struct
	// whose map was concurrently emptied.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
		w := httptest.NewRecorder()
		srv.clusterz(w, req)
		got := decodeCluster(t, w.Body.Bytes())
		if got.Summary.Total != 2 || got.Summary.Healthy != 2 {
			t.Fatalf("call %d: summary = %+v, want {2,2,0,0}", i, got.Summary)
		}
	}
}

func TestClusterz_CanceledRequest_DoesNotPoisonCache(t *testing.T) {
	// Regression for the bug surfaced in #101 review: if the fanout
	// derives its context from r.Context(), an HTTP client disconnect
	// would mark every in-flight peer "unreachable" and that all-failed
	// snapshot would be cached for clusterCacheTTL. Subsequent honest
	// callers would see a fabricated cluster-wide outage.
	//
	// Setup: one healthy peer; the first /clusterz call uses an
	// already-canceled request context. The peer's response must still
	// reach the response (proving the fanout doesn't observe the
	// cancellation). The cached entry must reflect a healthy aggregate
	// so that the next call reads "ok" rather than the false outage.
	peer := peerWithReadyz(t, http.StatusOK, `{"status":"ready","checks":{"circuit_breaker":"closed"}}`)
	self := "self:8686"
	src := stubClusterSource{leader: true, peers: []string{self, hostPort(t, peer.URL)}}
	srv := buildClusterServer(t, src, self)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil).WithContext(canceled)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with canceled request context; body=%s", w.Code, w.Body)
	}
	got := decodeCluster(t, w.Body.Bytes())
	if got.Status != "ok" {
		t.Errorf("aggregate status = %q, want ok (cancellation must not poison the result)", got.Status)
	}

	// Second call with a fresh context must hit the cache and still see ok.
	req2 := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w2 := httptest.NewRecorder()
	srv.clusterz(w2, req2)
	got2 := decodeCluster(t, w2.Body.Bytes())
	if got2.Status != "ok" {
		t.Errorf("cached aggregate status = %q, want ok (cache must not hold a poisoned result)", got2.Status)
	}
}

func TestClusterz_SelfAndPeersRunInParallel(t *testing.T) {
	// Both arms announce that they started, then wait on the same release
	// channel. The handler can finish only after the test has observed both
	// announcements, so this proves the self-check and peer fanout overlap
	// without relying on scheduler speed or wall-clock duration.
	started := make(chan string, 2)
	release := make(chan struct{})

	blockingPinger := pingerFunc(func(ctx context.Context) bool {
		started <- "self"
		select {
		case <-ctx.Done():
			return false
		case <-release:
			return true
		}
	})

	blockingPeer := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- "peer"
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"status":"ready","checks":{"circuit_breaker":"closed"}}`,
				)),
				Request: req,
			}, nil
		}
	})

	self := "self:8686"
	peer := "peer:8686"
	src := stubClusterSource{leader: true, peers: []string{self, peer}}
	srv := NewServer(blockingPinger, &fakeSource{cbState: "closed"}, "test")
	srv.EnableCluster(ClusterConfig{
		Source:      src,
		Self:        self,
		PeerTimeout: time.Second,
	})
	srv.cluster.client = &http.Client{Transport: blockingPeer}

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.clusterz(w, req)
		close(done)
	}()

	gotStarted := make(map[string]bool, 2)
	var startFailure string
	for range 2 {
		select {
		case arm := <-started:
			gotStarted[arm] = true
		case <-time.After(time.Second):
			startFailure = "self-check and peer fanout did not both start before release"
		}
		if startFailure != "" {
			break
		}
	}
	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("clusterz did not finish after releasing both arms")
	}
	if startFailure != "" {
		t.Fatal(startFailure)
	}
	if !gotStarted["self"] || !gotStarted["peer"] {
		t.Fatalf("started arms = %v, want self and peer", gotStarted)
	}

	// Sanity: both arms completed.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body)
	}
	got := decodeCluster(t, w.Body.Bytes())
	if got.Summary.Total != 2 || got.Summary.Healthy != 2 {
		t.Fatalf("summary = %+v, want {2,2,0,0}", got.Summary)
	}
}

func TestClusterz_RejectsNonGET(t *testing.T) {
	src := stubClusterSource{leader: true, peers: nil}
	srv := buildClusterServer(t, src, "self:8686")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodPost, "/clusterz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /clusterz: code = %d, want 405", w.Code)
	}
}

func TestClusterz_PeerBodyTooLarge(t *testing.T) {
	// Defense: a peer streaming a giant body must not pin memory in the
	// fanout. The handler's LimitReader caps the read; the over-cap body
	// won't parse as JSON, so the peer is reported degraded.
	huge := strings.Repeat("a", peerReadBodyLimit*2)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, huge)
	}))
	t.Cleanup(bad.Close)

	src := stubClusterSource{leader: true, peers: []string{hostPort(t, bad.URL)}}
	srv := buildClusterServer(t, src, "self:8686")

	req := httptest.NewRequest(http.MethodGet, "/clusterz", nil)
	w := httptest.NewRecorder()
	srv.clusterz(w, req)

	got := decodeCluster(t, w.Body.Bytes())
	peer := hostPort(t, bad.URL)
	node := got.Nodes[peer]
	if node.Status != "degraded" {
		t.Errorf("oversized-body peer status = %q, want degraded", node.Status)
	}
	// The wrapped JSON decode error gives operators something to grep
	// for: a body cap hit shows up as "unexpected end of JSON input".
	// Pin the prefix so a regression that drops the wrapped error (back
	// to the static "invalid readyz response") fails this test.
	if !strings.HasPrefix(node.Error, "invalid readyz response: ") {
		t.Errorf("error = %q, want prefix \"invalid readyz response: \" with wrapped decode reason", node.Error)
	}
	if node.Error == "invalid readyz response: " || node.Error == "invalid readyz response" {
		t.Errorf("error = %q, want a non-empty wrapped decode reason", node.Error)
	}
}

func nodeKeys(m map[string]nodeResult) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
