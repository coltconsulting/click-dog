//go:build integration

package main

// The cd-13 node suite. It drives the built binary as real processes inside
// the three ClickHouse containers of testing/docker-compose.integration.yml,
// so every instance is an actual node: its own hostname (which is what the
// topology self-audit keys readers on), a loopback ClickHouse connection like
// a production sidecar, Keeper and the collector by container name. Assertions
// read the collector's file exporter and each node's published health and
// metrics listeners, never internal state.
//
// It tests click-dog's distributed behavior only — election, standby gating,
// failover, sidecar scoping, coordinated flush, the self-audit, the check
// remedy — with seeded traced queries standing in for a workload. It does not
// test ClickHouse's own distributed query behavior.
//
// Three of these cases fail on the code that shipped in v26.09.1-beta.1 and
// passed every other gate: the standby startup duplicates, the every-other-
// cycle blind spot, and promotion waiting for the next tick.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

// -------------------------------------------------------------------
// Node addressing
// -------------------------------------------------------------------

// Each container publishes the in-container click-dog listeners on distinct
// host ports (see the compose file). Override per node with
// CLICKDOG<n>_HEALTH_ADDR / CLICKDOG<n>_METRICS_ADDR, e.g. to put the three
// nodes on 127.0.0.1 / 127.0.0.2 / 127.0.0.3 with the same port.
func nodeHealthAddr(node int) string {
	if addr := os.Getenv(fmt.Sprintf("CLICKDOG%d_HEALTH_ADDR", node)); addr != "" {
		return addr
	}
	return fmt.Sprintf("127.0.0.1:%d", 18685+node)
}

func nodeMetricsAddr(node int) string {
	if addr := os.Getenv(fmt.Sprintf("CLICKDOG%d_METRICS_ADDR", node)); addr != "" {
		return addr
	}
	return fmt.Sprintf("127.0.0.1:%d", 19089+node)
}

func nodeContainer(node int) string { return fmt.Sprintf("clickhouse-int-%d", node) }

// nodeHostname is the container hostname ClickHouse reports as hostName() and
// writes into query_log.client_hostname for a co-located reader.
func nodeHostname(node int) string { return fmt.Sprintf("clickhouse%d", node) }

var nodeKeeperHosts = []string{"clickhouse1:9181", "clickhouse2:9181", "clickhouse3:9181"}

const nodeBinaryPath = "/tmp/click-dog"

// -------------------------------------------------------------------
// Binary build + install
// -------------------------------------------------------------------

var (
	nodeBinaryOnce sync.Once
	nodeBinaryHost string
	nodeBinaryErr  error

	nodeInstallMu sync.Mutex
	nodeInstalled = map[int]bool{}
)

// buildNodeBinary cross-compiles the package once for the containers'
// architecture. The suite must run the real binary, not the test binary: the
// startup ordering and the promotion cycle live in main.go.
func buildNodeBinary(t *testing.T) string {
	t.Helper()
	nodeBinaryOnce.Do(func() {
		out, err := exec.Command("docker", "exec", nodeContainer(1), "uname", "-m").Output()
		if err != nil {
			nodeBinaryErr = fmt.Errorf("docker exec uname -m: %w", err)
			return
		}
		arch := map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}[strings.TrimSpace(string(out))]
		if arch == "" {
			nodeBinaryErr = fmt.Errorf("unsupported container architecture %q", strings.TrimSpace(string(out)))
			return
		}
		dir, err := os.MkdirTemp("", "click-dog-nodes-")
		if err != nil {
			nodeBinaryErr = err
			return
		}
		nodeBinaryHost = filepath.Join(dir, "click-dog")
		build := exec.Command("go", "build", "-ldflags=-s -w -X main.version=nodes-test", "-o", nodeBinaryHost, ".")
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			nodeBinaryErr = fmt.Errorf("go build linux/%s: %w\n%s", arch, err, out)
		}
	})
	if nodeBinaryErr != nil {
		t.Fatalf("node binary: %v", nodeBinaryErr)
	}
	return nodeBinaryHost
}

func installNodeBinary(t *testing.T, node int) {
	t.Helper()
	bin := buildNodeBinary(t)
	nodeInstallMu.Lock()
	defer nodeInstallMu.Unlock()
	if nodeInstalled[node] {
		return
	}
	if out, err := exec.Command("docker", "cp", bin, nodeContainer(node)+":"+nodeBinaryPath).CombinedOutput(); err != nil {
		t.Fatalf("docker cp binary to %s: %v\n%s", nodeContainer(node), err, out)
	}
	nodeInstalled[node] = true
}

// -------------------------------------------------------------------
// Config rendering
// -------------------------------------------------------------------

type nodeConfig struct {
	Node int
	// Name is the process identity inside the container (config, log, and
	// pkill pattern) and the default exporter service name.
	Name           string
	ServiceName    string
	ClusterQueries bool
	KeeperHosts    []string
	BasePath       string
	SessionTimeout int // seconds; 0 → 5, the validated minimum
	CheckInterval  int // seconds; 0 → 5 (lookback becomes 15s)
	ClusterHealth  bool
	AuditInterval  int // seconds; 0 → audit left at defaults
	AuditDebounce  int
	User, Password string
}

func (c nodeConfig) serviceName() string {
	if c.ServiceName != "" {
		return c.ServiceName
	}
	return c.Name
}

func (c nodeConfig) yaml() string {
	session, check := c.SessionTimeout, c.CheckInterval
	if session == 0 {
		session = 5
	}
	if check == 0 {
		check = 5
	}
	user, password := c.User, c.Password
	if user == "" {
		user = "default"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "clickhouse:\n  host: 127.0.0.1\n  port: 9000\n  database: system\n  username: %s\n  password: %q\n  query_timeout_s: 30\n", user, password)
	if c.ClusterQueries {
		b.WriteString("  cluster: test_cluster\n  use_cluster_queries: true\n")
	}
	fmt.Fprintf(&b, "exporters:\n  otel:\n    - collector_address: otel-collector:4317\n      service_name: %s\n", c.serviceName())
	fmt.Fprintf(&b, "monitor:\n  enabled: true\n  min_trace_duration_ms: 1000\n  check_interval_s: %d\n  max_spans_per_cycle: 1000\n  dedup_cache_size: 10000\n", check)
	if c.AuditInterval > 0 {
		fmt.Fprintf(&b, "  topology_audit:\n    interval_s: %d\n    debounce_count: %d\n", c.AuditInterval, c.AuditDebounce)
	}
	if len(c.KeeperHosts) > 0 {
		b.WriteString("ha:\n  keeper:\n    hosts:\n")
		for _, h := range c.KeeperHosts {
			fmt.Fprintf(&b, "      - %q\n", h)
		}
		fmt.Fprintf(&b, "    session_timeout_s: %d\n    base_path: %q\n", session, c.BasePath)
	}
	b.WriteString("metrics:\n  enabled: true\n  listen_address: \"0.0.0.0:9090\"\n  admin_listen_address: \"127.0.0.1:9100\"\n  otlp:\n    enabled: false\n")
	b.WriteString("health:\n  enabled: true\n  listen_address: \"0.0.0.0:8686\"\n")
	if c.ClusterHealth {
		fmt.Fprintf(&b, "  cluster:\n    enabled: true\n    self: %q\n    peers:\n", nodeHostname(c.Node)+":8686")
		for n := 1; n <= 3; n++ {
			fmt.Fprintf(&b, "      - %q\n", nodeHostname(n)+":8686")
		}
	}
	b.WriteString("log_level: info\n")
	return b.String()
}

// -------------------------------------------------------------------
// Node process control
// -------------------------------------------------------------------

type nodeProc struct {
	t       *testing.T
	cfg     nodeConfig
	stopped bool
}

func (p *nodeProc) configPath() string { return "/tmp/" + p.cfg.Name + ".yaml" }
func (p *nodeProc) logPath() string    { return "/tmp/" + p.cfg.Name + ".log" }
func (p *nodeProc) container() string  { return nodeContainer(p.cfg.Node) }

// dockerExec runs a command inside the node's container and returns its
// combined output; the container's exit status comes back as *exec.ExitError.
func dockerExec(container string, args ...string) (string, error) {
	full := append([]string{"exec", container}, args...)
	out, err := exec.Command("docker", full...).CombinedOutput()
	return string(out), err
}

// installNodeConfig writes the rendered config into the container.
func installNodeConfig(t *testing.T, cfg nodeConfig) string {
	t.Helper()
	host := filepath.Join(t.TempDir(), cfg.Name+".yaml")
	if err := os.WriteFile(host, []byte(cfg.yaml()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	target := nodeContainer(cfg.Node) + ":/tmp/" + cfg.Name + ".yaml"
	if out, err := exec.Command("docker", "cp", host, target).CombinedOutput(); err != nil {
		t.Fatalf("docker cp config to %s: %v\n%s", target, err, out)
	}
	return "/tmp/" + cfg.Name + ".yaml"
}

// startNode installs the binary and config and starts click-dog inside the
// container. `exec` makes the shell become the process, so pkill on the config
// path finds exactly one process.
func startNode(t *testing.T, cfg nodeConfig) *nodeProc {
	t.Helper()
	installNodeBinary(t, cfg.Node)
	installNodeConfig(t, cfg)
	p := &nodeProc{t: t, cfg: cfg}
	cmd := fmt.Sprintf("exec %s --config %s > %s 2>&1", nodeBinaryPath, p.configPath(), p.logPath())
	if out, err := dockerExec(p.container(), "sh", "-c", "rm -f "+p.logPath()); err != nil {
		t.Fatalf("reset log in %s: %v\n%s", p.container(), err, out)
	}
	full := []string{"exec", "-d", p.container(), "sh", "-c", cmd}
	if out, err := exec.Command("docker", full...).CombinedOutput(); err != nil {
		t.Fatalf("start %s in %s: %v\n%s", cfg.Name, p.container(), err, out)
	}
	t.Cleanup(p.stop)
	t.Logf("started %s in %s", cfg.Name, p.container())
	return p
}

func (p *nodeProc) signal(sig string) {
	p.t.Helper()
	// pkill exits 1 when nothing matched — already gone is fine.
	_, _ = dockerExec(p.container(), "pkill", "-"+sig, "-f", p.configPath())
}

func (p *nodeProc) stop() {
	if p.stopped {
		return
	}
	p.stopped = true
	p.signal("TERM")
	// Give the graceful shutdown (election resign, listener close) a moment so
	// the next test's process on this node does not race the listener ports.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := dockerExec(p.container(), "pgrep", "-f", p.configPath()); err != nil || strings.TrimSpace(out) == "" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.signal("KILL")
}

func (p *nodeProc) kill() {
	p.stopped = true
	p.signal("KILL")
}

func (p *nodeProc) logs() string {
	out, _ := dockerExec(p.container(), "cat", p.logPath())
	return out
}

func (p *nodeProc) logContains(substr string) bool {
	return strings.Contains(p.logs(), substr)
}

// candidateSequence returns the zero-padded sequence suffix of this node's
// election znode from its join log line, or "" if it has not joined. Keeper
// assigns the suffixes in join order, so comparing them as strings gives the
// election order.
func (p *nodeProc) candidateSequence() string {
	const marker = "-candidate-"
	logs := p.logs()
	i := strings.Index(logs, "Joined leader election at ")
	if i < 0 {
		return ""
	}
	line := logs[i:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	j := strings.LastIndex(line, marker)
	if j < 0 {
		return ""
	}
	return strings.TrimRight(line[j+len(marker):], ")")
}

func (p *nodeProc) waitForLog(substr string, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.logContains(substr) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	p.t.Fatalf("%s: log never contained %q within %v\n--- log ---\n%s", p.cfg.Name, substr, timeout, tailLines(p.logs(), 40))
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var nodeHTTP = &http.Client{Timeout: 5 * time.Second}

func (p *nodeProc) get(path string) (int, string, http.Header) {
	p.t.Helper()
	addr := nodeHealthAddr(p.cfg.Node)
	if path == "/metrics" {
		addr = nodeMetricsAddr(p.cfg.Node)
	}
	resp, err := nodeHTTP.Get("http://" + addr + path)
	if err != nil {
		return 0, "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// metrics returns every sample as name{labels} → value, plus the bare name for
// unlabeled series.
func (p *nodeProc) metrics() map[string]float64 {
	p.t.Helper()
	out := map[string]float64{}
	_, body, _ := p.get("/metrics")
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sp := strings.LastIndex(line, " ")
		if sp < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			continue
		}
		out[line[:sp]] = v
	}
	return out
}

func (p *nodeProc) metric(series string) float64 {
	v, ok := p.metrics()[series]
	if !ok {
		p.t.Fatalf("%s: metric %q not present", p.cfg.Name, series)
	}
	return v
}

func (p *nodeProc) status() map[string]interface{} {
	p.t.Helper()
	code, body, _ := p.get("/status")
	if code != 200 {
		p.t.Fatalf("%s: /status returned %d: %s", p.cfg.Name, code, body)
	}
	return decodeJSONObject(p.t, body)
}

func (p *nodeProc) waitForMetric(series string, want float64, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	var last float64
	var seen bool
	for time.Now().Before(deadline) {
		if v, ok := p.metrics()[series]; ok {
			last, seen = v, true
			if v == want {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.t.Fatalf("%s: %s never reached %v within %v (last=%v, present=%v)", p.cfg.Name, series, want, timeout, last, seen)
}

// runCLI runs a click-dog subcommand inside the node's container with this
// node's config, returning combined output and the exit error, if any.
func (p *nodeProc) runCLI(args ...string) (string, error) {
	full := append([]string{nodeBinaryPath}, args...)
	full = append(full, "--config", p.configPath())
	return dockerExec(p.container(), full...)
}

func decodeJSONObject(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, body)
	}
	return m
}

// -------------------------------------------------------------------
// Ground truth: seeded traces vs the collector
// -------------------------------------------------------------------

// seededSpans is the source identity set for one or more seed calls: every
// (trace_id, span_id) row ClickHouse wrote for the seeded traces, captured
// from the seeding node's span log. Delivery is judged against this set, so a
// missing child is a zero-copy finding rather than an unseen identity.
type seededSpans struct {
	traces []uuid.UUID
	spans  map[uuid.UUID]map[uint64]bool
}

func (s *seededSpans) add(o seededSpans) {
	if s.spans == nil {
		s.spans = map[uuid.UUID]map[uint64]bool{}
	}
	for _, id := range o.traces {
		if s.spans[id] == nil {
			s.traces = append(s.traces, id)
			s.spans[id] = map[uint64]bool{}
		}
		for span := range o.spans[id] {
			s.spans[id][span] = true
		}
	}
}

func (s seededSpans) spanCount() int {
	n := 0
	for _, spans := range s.spans {
		n += len(spans)
	}
	return n
}

// captureSeededSpans reads the exact span identities ClickHouse recorded for
// the given traces from the seeding node's local span log. The seed helper has
// already flushed logs and waited for the traces to materialize.
func captureSeededSpans(t *testing.T, conn driver.Conn, traceIDs []uuid.UUID) seededSpans {
	t.Helper()
	ids := make([]string, len(traceIDs))
	for i, id := range traceIDs {
		ids[i] = "toUUID('" + id.String() + "')"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := conn.Query(ctx, "SELECT trace_id, span_id FROM system.opentelemetry_span_log WHERE trace_id IN ("+strings.Join(ids, ", ")+")")
	if err != nil {
		t.Fatalf("read seeded span identities: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := seededSpans{traces: append([]uuid.UUID(nil), traceIDs...), spans: map[uuid.UUID]map[uint64]bool{}}
	for _, id := range traceIDs {
		out.spans[id] = map[uint64]bool{}
	}
	for rows.Next() {
		var trace uuid.UUID
		var span uint64
		if err := rows.Scan(&trace, &span); err != nil {
			t.Fatalf("scan seeded span identity: %v", err)
		}
		out.spans[trace][span] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("seeded span identities: %v", err)
	}
	for _, id := range traceIDs {
		if len(out.spans[id]) == 0 {
			t.Fatalf("seeded trace %s has no spans in span_log", id)
		}
	}
	return out
}

// traceDelivery is what the collector received for one seeded trace: copies
// per span identity, and which exporter service names and ClickHouse hosts the
// spans came from.
type traceDelivery struct {
	copies   map[uint64]int
	services map[string]int
	hosts    map[string]int
}

func collectorDeliveries(obs []otelSpanObservation, traceIDs []uuid.UUID) map[uuid.UUID]*traceDelivery {
	want := make(map[uuid.UUID]bool, len(traceIDs))
	for _, id := range traceIDs {
		want[id] = true
	}
	out := map[uuid.UUID]*traceDelivery{}
	for _, o := range obs {
		if !want[o.TraceID] {
			continue
		}
		d := out[o.TraceID]
		if d == nil {
			d = &traceDelivery{copies: map[uint64]int{}, services: map[string]int{}, hosts: map[string]int{}}
			out[o.TraceID] = d
		}
		d.copies[o.SpanID]++
		d.services[o.ServiceName]++
		if h, _ := o.Attributes["hostname"].(string); h != "" {
			d.hosts[h]++
		}
	}
	return out
}

// deliveryProblems compares the collector's observations against the seeded
// source identities: every seeded span must have been delivered between min
// and max times, so a span the exporter dropped is reported with zero copies
// rather than silently absent. Pure so the check itself can be tested.
func deliveryProblems(expected seededSpans, obs []otelSpanObservation, min, max int) []string {
	got := collectorDeliveries(obs, expected.traces)
	var problems []string
	for _, id := range expected.traces {
		d := got[id]
		for span := range expected.spans[id] {
			n := 0
			var services map[string]int
			if d != nil {
				n = d.copies[span]
				services = d.services
			}
			if n < min || n > max {
				problems = append(problems, fmt.Sprintf("trace %s span %x delivered %d times, want %d..%d (services %v)", id, span, n, min, max, services))
			}
		}
	}
	return problems
}

// waitForTracesDelivered blocks until every seeded span identity has reached
// the collector at least minCopies times, then returns the full observation
// set for exact assertions.
func waitForTracesDelivered(t *testing.T, expected seededSpans, minCopies int, timeout time.Duration) []otelSpanObservation {
	t.Helper()
	return waitForOTELFileObservations(t, timeout, func(obs []otelSpanObservation) bool {
		return len(deliveryProblems(expected, obs, minCopies, int(^uint(0)>>1))) == 0
	}, fmt.Sprintf("not every seeded span (%d spans in %d traces) reached the collector %d time(s)", expected.spanCount(), len(expected.traces), minCopies))
}

// assertDeliveryCopies fails the test for every seeded span identity delivered
// outside min..max. Exactly-once (1,1) is the steady-state contract; a
// failover boundary is documented as duplicate-prone, so those cases allow
// (1,2).
func assertDeliveryCopies(t *testing.T, obs []otelSpanObservation, expected seededSpans, min, max int) map[uuid.UUID]*traceDelivery {
	t.Helper()
	for _, p := range deliveryProblems(expected, obs, min, max) {
		t.Error(p)
	}
	return collectorDeliveries(obs, expected.traces)
}

// seedOn seeds count traced queries of about 1.2s on the given node and
// returns every span identity ClickHouse recorded for them. The seed helper
// flushes logs and waits for the rows, so the traces are in span_log when
// this returns.
func seedOn(t *testing.T, conns []driver.Conn, node, count int) seededSpans {
	t.Helper()
	fx := seedTracedQueryFixtureWithDuration(t, conns[node-1], count, 1200*time.Millisecond)
	return captureSeededSpans(t, conns[node-1], fx.TraceIDs)
}

// TestNodesHelper_DeliveryProblemsFlagMissingChild pins the helper the suite's
// no-gap assertions rest on: a partial trace (root delivered, child dropped)
// must be a finding, and so must a duplicate above max.
func TestNodesHelper_DeliveryProblemsFlagMissingChild(t *testing.T) {
	trace := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	expected := seededSpans{traces: []uuid.UUID{trace}, spans: map[uuid.UUID]map[uint64]bool{trace: {1: true, 2: true}}}
	root := otelSpanObservation{Name: "query", TraceID: trace, SpanID: 1, ServiceName: "n1"}
	child := otelSpanObservation{Name: "child", TraceID: trace, SpanID: 2, ServiceName: "n1"}

	if got := deliveryProblems(expected, []otelSpanObservation{root, child}, 1, 1); len(got) != 0 {
		t.Fatalf("complete trace reported problems: %v", got)
	}
	got := deliveryProblems(expected, []otelSpanObservation{root}, 1, 1)
	if len(got) != 1 || !strings.Contains(got[0], "span 2 delivered 0 times") {
		t.Fatalf("root-only delivery must flag the missing child, got %v", got)
	}
	if got := deliveryProblems(expected, nil, 1, 1); len(got) != 2 {
		t.Fatalf("an undelivered trace must flag every span, got %v", got)
	}
	if got := deliveryProblems(expected, []otelSpanObservation{root, root, child}, 1, 1); len(got) != 1 || !strings.Contains(got[0], "span 1 delivered 2 times") {
		t.Fatalf("a duplicate above max must be flagged, got %v", got)
	}
	if got := deliveryProblems(expected, []otelSpanObservation{root, root, child, child}, 1, 2); len(got) != 0 {
		t.Fatalf("copies inside min..max must pass, got %v", got)
	}
}

// nodeSuiteConns opens a connection per node and confirms the collector is up.
func nodeSuiteConns(t *testing.T) []driver.Conn {
	t.Helper()
	conns := waitForAllClickHouse(t, 60*time.Second)
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	waitForKeeper(t, 30*time.Second)
	waitForOTELCollector(t, 30*time.Second)
	// ClickHouse creates system.opentelemetry_span_log lazily on the first
	// traced query, and a cluster read fails while any node lacks it. Seed one
	// fast trace per node so the suite is self-contained on a fresh cluster.
	for n := 1; n <= 3; n++ {
		seedTracedQueryFixtureWithDuration(t, conns[n-1], 1, 0)
	}
	return conns
}

func clusterNode(t *testing.T, node int, name, basePath string) nodeConfig {
	return nodeConfig{Node: node, Name: name, ClusterQueries: true, KeeperHosts: nodeKeeperHosts, BasePath: basePath, ClusterHealth: true}
}

// -------------------------------------------------------------------
// Case 1: a standby's startup cycle is gated
// -------------------------------------------------------------------

func TestNodes_StandbyStartupIsGated(t *testing.T) {
	conns := nodeSuiteConns(t)
	basePath := uniqueBasePath(t)

	leader := startNode(t, clusterNode(t, 1, "n1-leader", basePath))
	leader.waitForLog("This instance is now the LEADER", 20*time.Second)
	leader.waitForLog("Startup check passed", 10*time.Second)

	// Seed on a node the leader does not connect to: the cluster read must
	// bring it in, and it is what a fail-open standby would re-export.
	seeded := seedOn(t, conns, 2, 3)
	waitForTracesDelivered(t, seeded, 1, 30*time.Second)

	standbys := []*nodeProc{
		startNode(t, clusterNode(t, 2, "n2-standby", basePath)),
		startNode(t, clusterNode(t, 3, "n3-standby", basePath)),
	}
	for _, s := range standbys {
		s.waitForLog("Startup check: standby (not leader) — export gated, no work this cycle", 20*time.Second)
		if s.logContains("Exported: ") {
			t.Errorf("%s exported during startup:\n%s", s.cfg.Name, tailLines(s.logs(), 20))
		}
	}

	// Let a full cycle pass so the skipped counter and standby status settle.
	time.Sleep(7 * time.Second)
	for _, s := range standbys {
		m := s.metrics()
		if m["click_dog_spans_exported_total"] != 0 {
			t.Errorf("%s exported %v spans; a standby must export nothing", s.cfg.Name, m["click_dog_spans_exported_total"])
		}
		if m[`click_dog_last_success_timestamp_seconds{role="standby"}`] != 0 {
			t.Errorf("%s standby last-success gauge = %v, want 0", s.cfg.Name, m[`click_dog_last_success_timestamp_seconds{role="standby"}`])
		}
		if m["click_dog_leader"] != 0 {
			t.Errorf("%s click_dog_leader = %v, want 0", s.cfg.Name, m["click_dog_leader"])
		}
		if m[`click_dog_cycle_results_total{result="skipped"}`] < 1 {
			t.Errorf("%s recorded no skipped cycles", s.cfg.Name)
		}
		st := s.status()
		if st["standby"] != true {
			t.Errorf("%s /status.standby = %v, want true", s.cfg.Name, st["standby"])
		}
		if lc, _ := st["last_cycle"].(map[string]interface{}); lc == nil || lc["skip_reason"] != "leader_standby" {
			t.Errorf("%s /status.last_cycle = %v, want skip_reason leader_standby", s.cfg.Name, st["last_cycle"])
		}
		code, body, hdr := s.get("/clusterz")
		if code != 404 || !strings.Contains(body, `"not_leader"`) {
			t.Errorf("%s /clusterz = %d %s, want 404 not_leader", s.cfg.Name, code, body)
		}
		if hdr.Get("Location") != "" {
			t.Errorf("%s /clusterz follower carried a Location header (reserved for phase 2): %q", s.cfg.Name, hdr.Get("Location"))
		}
	}

	if v := leader.metric("click_dog_leader"); v != 1 {
		t.Errorf("leader click_dog_leader = %v, want 1", v)
	}
	if code, body, _ := leader.get("/clusterz"); code != 200 {
		t.Errorf("leader /clusterz = %d with every peer up: %s", code, body)
	}

	// The wire contract: the seeded spans were delivered exactly once, by the
	// leader alone.
	got := assertDeliveryCopies(t, readOTELFileExporterObservations(t), seeded, 1, 1)
	for id, d := range got {
		if len(d.services) != 1 || d.services["n1-leader"] == 0 {
			t.Errorf("trace %s exported by %v, want only n1-leader", id, d.services)
		}
	}
}

// -------------------------------------------------------------------
// Case 2: every cycle sees every trace inside the lookback
// -------------------------------------------------------------------

func TestNodes_EveryCycleExports(t *testing.T) {
	conns := nodeSuiteConns(t)
	// A 15s interval gives a 25s lookback. The pre-fix reader re-read the
	// window only every second cycle (30s), so a rolling 5s plus the span-log
	// flush latency was blind and roughly a third of arrivals never exported;
	// fifteen arrivals make that failure near-certain rather than probable.
	cfg := clusterNode(t, 1, "n1-only", uniqueBasePath(t))
	cfg.CheckInterval = 15
	leader := startNode(t, cfg)
	leader.waitForLog("Startup check passed", 20*time.Second)

	var seeded seededSpans
	for i := 0; i < 15; i++ {
		seeded.add(seedOn(t, conns, i%3+1, 1))
		time.Sleep(1800 * time.Millisecond)
	}
	obs := waitForTracesDelivered(t, seeded, 1, 45*time.Second)
	assertDeliveryCopies(t, obs, seeded, 1, 1)

	if v := leader.metric(`click_dog_cycle_results_total{result="error"}`); v != 0 {
		t.Errorf("leader recorded %v error cycles", v)
	}
}

// -------------------------------------------------------------------
// Case 3: failover promotes fast and covers the boundary
// -------------------------------------------------------------------

func TestNodes_FailoverPromotesAndCoversTheBoundary(t *testing.T) {
	conns := nodeSuiteConns(t)
	basePath := uniqueBasePath(t)

	// The hand-off order below (A → B → C) is the election order, which is the
	// order the candidates joined in, not the order the processes were launched:
	// startNode is a detached docker exec, so two launches back to back can join
	// either way round. Wait for each standby to be joined and gated before
	// starting the next, then check the sequence numbers say what we assume.
	a := startNode(t, clusterNode(t, 1, "n1-a", basePath))
	a.waitForLog("This instance is now the LEADER", 20*time.Second)
	b := startNode(t, clusterNode(t, 2, "n2-b", basePath))
	b.waitForLog("Startup check: standby", 20*time.Second)
	c := startNode(t, clusterNode(t, 3, "n3-c", basePath))
	c.waitForLog("Startup check: standby", 20*time.Second)
	if seqB, seqC := b.candidateSequence(), c.candidateSequence(); seqB == "" || seqC == "" || seqB >= seqC {
		t.Fatalf("election order is not A → B → C: B candidate %q, C candidate %q", seqB, seqC)
	}

	// Seed continuously across the two hand-offs so the boundary is exercised
	// with real arrivals, not a quiet cluster. The worker is stopped and joined
	// by cleanup as well as on the normal path, so an early fatal below cannot
	// leave it seeding into closed connections after the test has ended.
	var mu sync.Mutex
	var seeded seededSpans
	stopSeedCh := make(chan struct{})
	var wg sync.WaitGroup
	var stopOnce sync.Once
	stopSeed := func() {
		stopOnce.Do(func() { close(stopSeedCh) })
		wg.Wait()
	}
	t.Cleanup(stopSeed)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stopSeedCh:
				return
			default:
			}
			if t.Failed() {
				return
			}
			ids := seedOn(t, conns, i%3+1, 1)
			mu.Lock()
			seeded.add(ids)
			mu.Unlock()
			time.Sleep(1500 * time.Millisecond)
		}
	}()

	time.Sleep(6 * time.Second)

	// Graceful loss: SIGTERM resigns, so promotion is near-instant.
	t0 := time.Now()
	a.stop()
	b.waitForLog("This instance is now the LEADER", 10*time.Second)
	b.waitForLog("Promoted to leader — running immediate cycle to cover the failover window", 5*time.Second)
	t.Logf("SIGTERM failover: promotion in %v", time.Since(t0))

	time.Sleep(6 * time.Second)

	// Hard loss: SIGKILL leaves the session to expire (5s) before promotion.
	t1 := time.Now()
	b.kill()
	c.waitForLog("This instance is now the LEADER", 25*time.Second)
	promoted := time.Since(t1)
	c.waitForLog("Promoted to leader — running immediate cycle to cover the failover window", 5*time.Second)
	t.Logf("SIGKILL failover: promotion in %v", promoted)
	if promoted > 20*time.Second {
		t.Errorf("SIGKILL promotion took %v, want about the 5s session timeout plus election overhead", promoted)
	}

	time.Sleep(6 * time.Second)
	stopSeed()

	mu.Lock()
	all := seeded
	mu.Unlock()
	if len(all.traces) < 8 {
		t.Fatalf("seeded only %d traces across the failover", len(all.traces))
	}
	obs := waitForTracesDelivered(t, all, 1, 40*time.Second)
	// No gap: every seeded span arrives. The boundary is documented as a
	// duplicate-prone overlap, so allow a second copy but never a third.
	assertDeliveryCopies(t, obs, all, 1, 2)

	if v := c.metric("click_dog_leader"); v != 1 {
		t.Errorf("new leader click_dog_leader = %v, want 1", v)
	}
	if code, _, _ := c.get("/clusterz"); code == 404 || code == 0 {
		t.Errorf("new leader /clusterz = %d, want it served here (200 or 503 with dead peers)", code)
	}
}

// -------------------------------------------------------------------
// Case 4: sidecars export disjoint scopes and share one flush
// -------------------------------------------------------------------

func TestNodes_SidecarsExportDisjointScopesAndShareFlush(t *testing.T) {
	conns := nodeSuiteConns(t)
	basePath := uniqueBasePath(t)

	var sidecars []*nodeProc
	for n := 1; n <= 3; n++ {
		sidecars = append(sidecars, startNode(t, nodeConfig{
			Node: n, Name: fmt.Sprintf("n%d-sidecar", n), KeeperHosts: nodeKeeperHosts, BasePath: basePath, ClusterHealth: true,
		}))
	}
	for _, s := range sidecars {
		s.waitForLog("Startup check passed", 20*time.Second)
		s.waitForLog("Joined leader election", 20*time.Second)
	}

	perNode := map[int]seededSpans{}
	var all seededSpans
	for n := 1; n <= 3; n++ {
		perNode[n] = seedOn(t, conns, n, 2)
		all.add(perNode[n])
	}
	obs := waitForTracesDelivered(t, all, 1, 30*time.Second)
	got := assertDeliveryCopies(t, obs, all, 1, 1)
	for n := 1; n <= 3; n++ {
		for _, id := range perNode[n].traces {
			d := got[id]
			if d == nil {
				continue
			}
			want := fmt.Sprintf("n%d-sidecar", n)
			if len(d.services) != 1 || d.services[want] == 0 {
				t.Errorf("trace seeded on node %d exported by %v, want only %s", n, d.services, want)
			}
			if len(d.hosts) != 1 || d.hosts[nodeHostname(n)] == 0 {
				t.Errorf("trace seeded on node %d carried hostnames %v, want only %s", n, d.hosts, nodeHostname(n))
			}
		}
	}

	// Every sidecar is an active exporter regardless of coordination
	// leadership, and exactly one of them serves /clusterz.
	var coordinator, follower *nodeProc
	for _, s := range sidecars {
		if v := s.metric("click_dog_leader"); v != 1 {
			t.Errorf("%s click_dog_leader = %v; sidecars report 1 regardless of Keeper leadership", s.cfg.Name, v)
		}
		if v := s.metric(`click_dog_cycle_results_total{result="skipped"}`); v != 0 {
			t.Errorf("%s skipped %v cycles; sidecars are never gated", s.cfg.Name, v)
		}
		if code, _, _ := s.get("/clusterz"); code == 404 {
			follower = s
		} else {
			if coordinator != nil {
				t.Errorf("both %s and %s serve /clusterz", coordinator.cfg.Name, s.cfg.Name)
			}
			coordinator = s
		}
	}
	if coordinator == nil || follower == nil {
		t.Fatalf("expected one coordination leader and at least one follower among the sidecars")
	}

	// A flush issued from a follower's config goes through Keeper and is
	// consumed by the coordination leader only.
	out, err := follower.runCLI("flush")
	if err != nil {
		t.Fatalf("click-dog flush from %s: %v\n%s", follower.cfg.Name, err, out)
	}
	if !strings.Contains(out, "writing flush request to Keeper") {
		t.Errorf("flush did not take the Keeper path:\n%s", out)
	}
	coordinator.waitForLog("Flush request found in Keeper", 15*time.Second)
	time.Sleep(2 * time.Second)
	for _, s := range sidecars {
		if s != coordinator && s.logContains("Flush request found in Keeper") {
			t.Errorf("%s consumed the Keeper flush request; only the coordination leader should", s.cfg.Name)
		}
	}
}

// -------------------------------------------------------------------
// Case 5: the self-audit catches co-located cluster readers
// -------------------------------------------------------------------

func TestNodes_TopologyAuditDetectsCoLocatedClusterReaders(t *testing.T) {
	conns := nodeSuiteConns(t)

	// Three cluster readers, no shared election: the anti-pattern. Each is a
	// real node with its own hostname, which is what the audit counts.
	var readers []*nodeProc
	for n := 1; n <= 3; n++ {
		readers = append(readers, startNode(t, nodeConfig{
			Node: n, Name: fmt.Sprintf("n%d-ap", n), ClusterQueries: true, AuditInterval: 30, AuditDebounce: 2,
		}))
	}
	for _, r := range readers {
		r.waitForLog("Topology self-audit active", 20*time.Second)
	}

	// The duplication is real: every span arrives once per reader.
	seeded := seedOn(t, conns, 2, 2)
	obs := waitForTracesDelivered(t, seeded, 3, 40*time.Second)
	assertDeliveryCopies(t, obs, seeded, 3, 3)

	// Detection lands after interval × debounce plus jitter.
	for _, r := range readers {
		r.waitForMetric(`click_dog_topology_warning{reason="sidecar_cluster_queries"}`, 1, 120*time.Second)
		if v := r.metric(`click_dog_topology_warning{reason="multi_instance_cluster_queries"}`); v != 0 {
			t.Errorf("%s multi_instance reason = %v, want 0 for a loopback reader", r.cfg.Name, v)
		}
		if st := r.status(); st["topology_warning"] != "sidecar_cluster_queries" {
			t.Errorf("%s /status.topology_warning = %v", r.cfg.Name, st["topology_warning"])
		}
		if code, _, _ := r.get("/readyz"); code != 200 {
			t.Errorf("%s /readyz = %d; the audit is observability-only", r.cfg.Name, code)
		}
		if !r.logContains("Topology audit DETECTED (sidecar_cluster_queries)") {
			t.Errorf("%s never logged the detection WARN", r.cfg.Name)
		}
	}
}

// -------------------------------------------------------------------
// Case 6: check names the REMOTE grant in cluster mode
// -------------------------------------------------------------------

func TestNodes_CheckNamesRemoteGrant(t *testing.T) {
	conns := nodeSuiteConns(t)
	ctx := context.Background()
	user := "cd_norem_" + newIntegrationFixtureMarker()[:8]
	for _, q := range []string{
		fmt.Sprintf("CREATE USER IF NOT EXISTS %s ON CLUSTER test_cluster IDENTIFIED WITH plaintext_password BY 'x'", user),
		fmt.Sprintf("GRANT ON CLUSTER test_cluster SELECT ON system.opentelemetry_span_log TO %s", user),
		fmt.Sprintf("GRANT ON CLUSTER test_cluster SELECT ON system.query_log TO %s", user),
	} {
		if err := conns[0].Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_ = conns[0].Exec(context.Background(), fmt.Sprintf("DROP USER IF EXISTS %s ON CLUSTER test_cluster", user))
	})

	installNodeBinary(t, 1)
	cfg := nodeConfig{Node: 1, Name: "n1-check", ClusterQueries: true, User: user, Password: "x"}
	installNodeConfig(t, cfg)
	p := &nodeProc{t: t, cfg: cfg}

	out, err := p.runCLI("check")
	if err == nil {
		t.Fatalf("check passed with only the two per-table grants in cluster mode:\n%s", out)
	}
	if !strings.Contains(out, "[FAIL] span_log readable") || !strings.Contains(out, "REMOTE") {
		t.Errorf("check remedy does not name REMOTE:\n%s", out)
	}

	for _, q := range []string{
		fmt.Sprintf("GRANT ON CLUSTER test_cluster REMOTE ON *.* TO %s", user),
		fmt.Sprintf("GRANT ON CLUSTER test_cluster SELECT ON system.columns TO %s", user),
	} {
		if err := conns[0].Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if out, err := p.runCLI("check"); err != nil {
		t.Errorf("check still failing with the documented cluster-mode grants: %v\n%s", err, out)
	}
}

// -------------------------------------------------------------------
// Case 7: Keeper unreachable at startup fails open, both ways
// -------------------------------------------------------------------

func TestNodes_KeeperUnreachableStartupFailsOpen(t *testing.T) {
	conns := nodeSuiteConns(t)

	// Resolvable address, nothing listening: the join wait times out and the
	// startup cycle runs fail-open.
	dead := startNode(t, nodeConfig{Node: 1, Name: "n1-deadkeeper", ClusterQueries: true, KeeperHosts: []string{"127.0.0.1:19999"}, BasePath: uniqueBasePath(t)})
	dead.waitForLog("Leader election not joined after 5s — running the startup cycle fail-open", 20*time.Second)
	dead.waitForLog("Startup check passed", 5*time.Second)
	seeded := seedOn(t, conns, 3, 1)
	assertDeliveryCopies(t, waitForTracesDelivered(t, seeded, 1, 30*time.Second), seeded, 1, 1)
	if v := dead.metric("click_dog_leader"); v != 1 {
		t.Errorf("fail-open reader click_dog_leader = %v, want 1", v)
	}
	dead.stop()

	// Unresolvable host: election construction fails and the process runs
	// standalone until restart.
	bad := startNode(t, nodeConfig{Node: 1, Name: "n1-badkeeper", ClusterQueries: true, KeeperHosts: []string{"keeper.invalid:9181"}, BasePath: uniqueBasePath(t)})
	bad.waitForLog("Failed to join leader election, running in standalone mode", 20*time.Second)
	bad.waitForLog("Startup check passed", 5*time.Second)
	seeded = seedOn(t, conns, 2, 1)
	assertDeliveryCopies(t, waitForTracesDelivered(t, seeded, 1, 30*time.Second), seeded, 1, 1)
}

// -------------------------------------------------------------------
// Case 8: backfill on a standby is exempt from leader gating
// -------------------------------------------------------------------

func TestNodes_BackfillOnStandbyIsExempt(t *testing.T) {
	conns := nodeSuiteConns(t)
	basePath := uniqueBasePath(t)

	leader := startNode(t, clusterNode(t, 1, "n1-lead", basePath))
	leader.waitForLog("This instance is now the LEADER", 20*time.Second)
	standby := startNode(t, clusterNode(t, 2, "n2-stand", basePath))
	standby.waitForLog("Startup check: standby", 20*time.Second)

	seedOn(t, conns, 2, 2)
	start := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	end := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	out, err := standby.runCLI("backfill", "--start", start, "--end", end)
	if err != nil {
		t.Fatalf("backfill on the standby's config failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Backfill mode: metrics and health listener(s) not started") {
		t.Errorf("backfill bound listeners while the standby held them:\n%s", out)
	}
	if !strings.Contains(out, "Backfill complete") || strings.Contains(out, "exported=0 ") {
		t.Errorf("backfill on a standby exported nothing; it must be exempt from leader gating:\n%s", out)
	}
	if standby.logContains("Exported: ") {
		t.Errorf("the standby's own scheduled loop exported during the backfill:\n%s", tailLines(standby.logs(), 20))
	}
}
