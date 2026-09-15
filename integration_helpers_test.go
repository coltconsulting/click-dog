//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	oteltrace "go.opentelemetry.io/otel/trace"

	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/leader"
	"github.com/coltconsulting/click-dog/internal/model"
)

// Default integration cluster addresses (matching docker-compose.integration.yml).
// Override via environment variables for non-standard setups.

func integrationCHAddr(node int) string {
	envKey := fmt.Sprintf("CLICKHOUSE%d_ADDR", node)
	if addr := os.Getenv(envKey); addr != "" {
		return addr
	}
	// Ports 19000, 19001, 19002 for nodes 1, 2, 3
	return fmt.Sprintf("localhost:%d", 18999+node)
}

func integrationCHHost(node int) string {
	host, _, _ := strings.Cut(integrationCHAddr(node), ":")
	return host
}

func integrationCHPort(node int) int {
	_, portStr, _ := strings.Cut(integrationCHAddr(node), ":")
	port := 19000
	fmt.Sscanf(portStr, "%d", &port)
	return port
}

func integrationOTELAddr() string {
	if addr := os.Getenv("OTEL_ADDR"); addr != "" {
		return addr
	}
	return "localhost:14317"
}

func integrationKeeperAddr(node int) string {
	envKey := fmt.Sprintf("KEEPER%d_ADDR", node)
	if addr := os.Getenv(envKey); addr != "" {
		return addr
	}
	// Ports 19181, 19182, 19183 for nodes 1, 2, 3
	return fmt.Sprintf("localhost:%d", 19180+node)
}

func integrationAllKeeperAddrs() []string {
	return []string{
		integrationKeeperAddr(1),
		integrationKeeperAddr(2),
		integrationKeeperAddr(3),
	}
}

// -------------------------------------------------------------------
// Service readiness helpers
// -------------------------------------------------------------------

func waitForClickHouse(t *testing.T, node int, timeout time.Duration) driver.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addr := integrationCHAddr(node)

	for time.Now().Before(deadline) {
		conn, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{addr},
			Auth: clickhouse.Auth{
				Database: "default",
				Username: "default",
			},
			DialTimeout: 2 * time.Second,
		})
		if err == nil {
			if err := conn.Ping(context.Background()); err == nil {
				return conn
			}
			conn.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("ClickHouse node %d not ready after %v at %s", node, timeout, addr)
	return nil
}

func waitForAllClickHouse(t *testing.T, timeout time.Duration) []driver.Conn {
	t.Helper()
	conns := make([]driver.Conn, 3)
	for i := 1; i <= 3; i++ {
		conns[i-1] = waitForClickHouse(t, i, timeout)
	}
	return conns
}

func waitForKeeper(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addrs := integrationAllKeeperAddrs()

	for time.Now().Before(deadline) {
		le, err := leader.NewLeaderElection(
			config.LeaderElectionConfig{
				Hosts:          addrs,
				SessionTimeout: 5,
				BasePath:       "/click-dog/health-check",
			},
			func() {},
			func() {},
		)
		if err == nil {
			le.Resign()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("ClickHouse Keeper not ready after %v at %v", timeout, addrs)
}

func waitForOTELCollector(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addr := integrationOTELAddr()
	var lastErr error

	for time.Now().Before(deadline) {
		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: addr,
			ServiceName:      "health-check",
		})
		if err == nil {
			probeTimeout := 2 * time.Second
			if remaining := time.Until(deadline); remaining < probeTimeout {
				probeTimeout = remaining
			}
			probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			lastErr = exporter.CheckConnectivity(probeCtx)
			cancel()
			if closeErr := exporter.Close(context.Background()); closeErr != nil && lastErr == nil {
				lastErr = closeErr
			}
			if lastErr == nil {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("OTEL collector not ready after %v at %s (last error: %v)", timeout, addr, lastErr)
}

// -------------------------------------------------------------------
// Data seeding helpers
// -------------------------------------------------------------------

// slowQueryFixture identifies exactly the rows created by one seed operation.
// QueryIDs and Marker are unique per call, even when a test seeds more than
// once. WindowStart/WindowEnd are clock-skew-padded bounds suitable for reader
// APIs that select by time rather than by query ID.
type slowQueryFixture struct {
	Marker      string
	QueryIDs    []string
	DurationMs  int
	WindowStart time.Time
	WindowEnd   time.Time
}

// tracedQueryFixture identifies exactly the native spans and query-log rows
// created by one seed operation. ParentSpanIDs are the upstream span IDs sent
// to ClickHouse; native child spans should retain one of them as their parent.
type tracedQueryFixture struct {
	Marker        string
	QueryIDs      []string
	TraceIDs      []uuid.UUID
	ParentSpanIDs []uint64
	WindowStart   time.Time
	WindowEnd     time.Time
}

// newIntegrationFixtureMarker returns a SQL-safe, process-independent identity.
// t.Name is deliberately not embedded in SQL: test names are useful labels but
// are not constrained to SQL-literal-safe characters.
func newIntegrationFixtureMarker() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

func integrationFixtureQueryID(marker string, index int) string {
	return fmt.Sprintf("click-dog-int-%s-%03d", marker, index)
}

func newIntegrationSpanID() uint64 {
	for {
		seed := uuid.New()
		if id := binary.BigEndian.Uint64(seed[:8]); id != 0 {
			return id
		}
	}
}

func sqlStringList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		value = strings.NewReplacer("\\", "\\\\", "'", "''").Replace(value)
		quoted[i] = "'" + value + "'"
	}
	return strings.Join(quoted, ", ")
}

// seedSlowQueryFixture runs N queries with sleep() on a specific node and
// returns their exact identities and time bounds.
func seedSlowQueryFixture(t *testing.T, conn driver.Conn, count int, durationMs int) slowQueryFixture {
	t.Helper()
	if count <= 0 {
		t.Fatalf("slow-query fixture count must be positive, got %d", count)
	}
	ctx := context.Background()
	sleepSec := float64(durationMs) / 1000.0

	// Snapshot before issuing seed queries so the post-flush count filter can
	// require event_time_microseconds >= seedStart. Without this lower bound
	// a leftover row from a prior `make integration` run with the same marker
	// (the marker is derived from t.Name) would let the poll return instantly
	// against stale data and the test would silently skip the real wait.
	//
	// The -clockSkewMargin offset absorbs small client/server clock drift —
	// if the test process's clock runs ahead of the ClickHouse server's, a
	// strict bound would exclude the seeded rows and the poll would time
	// out despite a successful seed. Margin still excludes any row older
	// than a couple seconds before the seed call.
	windowStart := time.Now().Add(-clockSkewMargin)
	seedStartUs := windowStart.UnixMicro()

	marker := newIntegrationFixtureMarker()
	queryIDs := make([]string, 0, count)
	for i := 0; i < count; i++ {
		queryID := integrationFixtureQueryID(marker, i)
		queryIDs = append(queryIDs, queryID)
		query := fmt.Sprintf(
			"SELECT sleep(%f), '%s-%d' AS test_marker SETTINGS max_execution_time=30",
			sleepSec, marker, i,
		)
		queryCtx := clickhouse.Context(ctx, clickhouse.WithQueryID(queryID))
		if err := conn.Exec(queryCtx, query); err != nil {
			t.Fatalf("Failed to seed slow query %d: %v", i, err)
		}
	}

	// Flush logs so they appear in system tables immediately, then poll until
	// the seeded rows are visible. Replaces a fixed time.Sleep that would race
	// against ClickHouse's async log buffer flush on slow CI hosts.
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("Failed to flush logs: %v", err)
	}
	// event_time_microseconds (not event_time) so this matches the µs
	// resolution used by seedTracedQueries — the two helpers now agree on
	// what "newer than the seed call" means.
	countQuery := fmt.Sprintf(
		"SELECT count() FROM system.query_log "+
			"WHERE query_id IN (%s) "+
			"AND type = 'QueryFinish' "+
			"AND event_time_microseconds >= fromUnixTimestamp64Micro(%d)",
		sqlStringList(queryIDs),
		seedStartUs,
	)
	waitForRowCount(t, conn, "seeded query_log rows", countQuery, uint64(count), 5*time.Second)

	return slowQueryFixture{
		Marker:      marker,
		QueryIDs:    queryIDs,
		DurationMs:  durationMs,
		WindowStart: windowStart,
		WindowEnd:   time.Now().Add(clockSkewMargin),
	}
}

// seedSlowQueries is retained for existing integration tests. New assertions
// should call seedSlowQueryFixture and select the returned exact identities.
func seedSlowQueries(t *testing.T, conn driver.Conn, count int, durationMs int) {
	t.Helper()
	_ = seedSlowQueryFixture(t, conn, count, durationMs)
}

// clockSkewMargin is the slack the seed helpers subtract from time.Now() before
// embedding it as the lower bound in their post-flush count queries. The bound
// is compared against ClickHouse-server timestamps (event_time_microseconds /
// finish_time_us); if the test process's clock runs ahead of the server's
// (CI runner skew, virtualized clocks), a strict bound would exclude the
// seeded rows. Two seconds is generous enough for any reasonable drift on
// localhost Docker and still strictly narrower than the prior `time.Sleep(1s)`
// window the gate replaces, so stale-row protection is preserved.
const clockSkewMargin = 2 * time.Second

func seedTracedQueryFixture(t *testing.T, conn driver.Conn, count int) tracedQueryFixture {
	t.Helper()
	return seedTracedQueryFixtureWithDuration(t, conn, count, 50*time.Millisecond)
}

func seedTracedQueryFixtureWithDuration(t *testing.T, conn driver.Conn, count int, duration time.Duration) tracedQueryFixture {
	t.Helper()
	if count <= 0 {
		t.Fatalf("traced-query fixture count must be positive, got %d", count)
	}
	if duration < 0 {
		t.Fatalf("traced-query fixture duration must not be negative, got %v", duration)
	}

	// Snapshot in microseconds so the post-flush count filter can require
	// finish_time_us >= seedStartUs. Exact per-call trace IDs prevent stale rows
	// from satisfying the wait; -clockSkewMargin absorbs client/server drift.
	windowStart := time.Now().Add(-clockSkewMargin)
	seedStartUs := windowStart.UnixMicro()
	marker := newIntegrationFixtureMarker()
	queryIDs := make([]string, 0, count)
	traceIDs := make([]uuid.UUID, 0, count)
	parentSpanIDs := make([]uint64, 0, count)

	for i := 0; i < count; i++ {
		// Create a trace context so ClickHouse populates opentelemetry_span_log.
		// clickhouse-go requires clickhouse.Context + WithSpan (not just Go context).
		traceID := uuid.New()
		spanSeed := uuid.New()
		var spanID oteltrace.SpanID
		copy(spanID[:], spanSeed[:len(spanID)])
		queryID := integrationFixtureQueryID(marker, i)
		spanCtx := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    oteltrace.TraceID(traceID),
			SpanID:     spanID,
			TraceFlags: oteltrace.FlagsSampled,
		})
		ctx := clickhouse.Context(
			context.Background(),
			clickhouse.WithSpan(spanCtx),
			clickhouse.WithQueryID(queryID),
		)

		query := fmt.Sprintf(
			"SELECT number, sleep(%f), '%s' AS integration_marker FROM system.numbers LIMIT %d SETTINGS max_execution_time=30",
			duration.Seconds(), marker, 10+i,
		)
		if err := conn.Exec(ctx, query); err != nil {
			t.Fatalf("Failed to seed traced query %d: %v", i, err)
		}
		queryIDs = append(queryIDs, queryID)
		traceIDs = append(traceIDs, traceID)
		parentSpanIDs = append(parentSpanIDs, binary.BigEndian.Uint64(spanID[:]))
	}

	if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("Failed to flush logs: %v", err)
	}
	traceIDSQL := make([]string, len(traceIDs))
	for i, traceID := range traceIDs {
		traceIDSQL[i] = "toUUID('" + traceID.String() + "')"
	}
	// Require every exact trace identity from this seed call to materialize. A
	// prior test can no longer satisfy the wait, even inside clock-skew slack.
	countQuery := fmt.Sprintf(
		"SELECT uniqExact(trace_id) FROM system.opentelemetry_span_log "+
			"WHERE trace_id IN (%s) "+
			"AND finish_time_us >= %d",
		strings.Join(traceIDSQL, ", "),
		seedStartUs,
	)
	waitForRowCount(t, conn, "seeded opentelemetry_span_log traces", countQuery, uint64(count), 5*time.Second)

	return tracedQueryFixture{
		Marker:        marker,
		QueryIDs:      queryIDs,
		TraceIDs:      traceIDs,
		ParentSpanIDs: parentSpanIDs,
		WindowStart:   windowStart,
		WindowEnd:     time.Now().Add(clockSkewMargin),
	}
}

// seedTracedQueries is retained for existing integration tests. New assertions
// should call seedTracedQueryFixture and select the returned exact identities.
func seedTracedQueries(t *testing.T, conn driver.Conn, count int) {
	t.Helper()
	_ = seedTracedQueryFixture(t, conn, count)
}

// waitForRowCount polls a ClickHouse count() expression until it returns at
// least minCount or the timeout expires. Used after SYSTEM FLUSH LOGS to wait
// for seeded rows to materialize in system tables, replacing fixed sleeps
// that were both slow on fast CI hosts and flaky on slow ones.
func waitForRowCount(t *testing.T, conn driver.Conn, label, countQuery string, minCount uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got uint64
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := conn.QueryRow(ctx, countQuery).Scan(&got)
		cancel()
		if err == nil && got >= minCount {
			return
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("%s: count query never reached %d within %v (last count=%d, last err=%v): %s", label, minCount, timeout, got, lastErr, countQuery)
	}
	t.Fatalf("%s: count query never reached %d within %v (last count=%d): %s", label, minCount, timeout, got, countQuery)
}

func seedSlowQueryFixturesOnAllNodes(t *testing.T, conns []driver.Conn, countPerNode int, durationMs int) []slowQueryFixture {
	t.Helper()
	fixtures := make([]slowQueryFixture, 0, len(conns))
	for i, conn := range conns {
		t.Logf("Seeding %d slow queries on node %d", countPerNode, i+1)
		fixtures = append(fixtures, seedSlowQueryFixture(t, conn, countPerNode, durationMs))
	}
	return fixtures
}

// -------------------------------------------------------------------
// OTEL file exporter verification helpers
// -------------------------------------------------------------------

func otelOutputDir() string {
	if dir := os.Getenv("OTEL_OUTPUT_DIR"); dir != "" {
		return dir
	}
	_, filename, _, _ := runtime.Caller(0)
	projectRoot := filepath.Dir(filename)
	return filepath.Join(projectRoot, "testing", "otel-output")
}

// otelSpanObservation is the collector-side identity and metadata later tests
// use to assert delivery of an exact span rather than an unrelated count bump.
type otelSpanObservation struct {
	Name               string
	TraceID            uuid.UUID
	SpanID             uint64
	ParentSpanID       uint64
	ServiceName        string
	ResourceAttributes map[string]interface{}
	Attributes         map[string]interface{}
}

// readOTELFileExporterObservations reads every complete JSONL record currently
// visible. The collector may be appending the last line concurrently, so one
// unterminated final fragment is ignored until the next poll. A malformed
// newline-terminated record is durable corruption and fails the test.
func readOTELFileExporterObservations(t *testing.T) []otelSpanObservation {
	t.Helper()
	filePath := filepath.Join(otelOutputDir(), "traces.jsonl")

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Logf("OTEL output file does not exist: %s", filePath)
			return nil
		}
		t.Fatalf("Failed to read OTEL output file: %v", err)
	}

	t.Logf("OTEL output file size: %d bytes", len(data))
	if len(data) > 0 && len(data) < 2000 {
		t.Logf("OTEL output file contents: %s", string(data))
	}

	observations, incompleteTail, err := parseOTELFileExporterObservations(data)
	if err != nil {
		t.Fatalf("Failed to parse OTEL output file: %v", err)
	}
	if incompleteTail {
		t.Log("OTEL output ends with an incomplete JSONL record; waiting for the collector to finish it")
	}
	return observations
}

func waitForOTELFileObservations(
	t *testing.T,
	timeout time.Duration,
	condition func([]otelSpanObservation) bool,
	failure string,
) []otelSpanObservation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var observations []otelSpanObservation
	for time.Now().Before(deadline) {
		observations = readOTELFileExporterObservations(t)
		if condition(observations) {
			return observations
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s after %v; collector has %d observations", failure, timeout, len(observations))
	return nil
}

func waitForOTELSpanKeys(t *testing.T, serviceName string, spans []model.OpenTelemetrySpan, timeout time.Duration) []otelSpanObservation {
	t.Helper()
	expected := make(map[model.SpanKey]struct{}, len(spans))
	for _, span := range spans {
		expected[model.KeyOf(span)] = struct{}{}
	}
	return waitForOTELFileObservations(t, timeout, func(observations []otelSpanObservation) bool {
		remaining := make(map[model.SpanKey]struct{}, len(expected))
		for key := range expected {
			remaining[key] = struct{}{}
		}
		for _, observation := range observations {
			if observation.ServiceName != serviceName {
				continue
			}
			delete(remaining, model.SpanKey{TraceID: observation.TraceID, SpanID: observation.SpanID})
		}
		return len(remaining) == 0
	}, fmt.Sprintf("expected %d exact spans from service %q", len(expected), serviceName))
}

func observationsWithAttribute(observations []otelSpanObservation, serviceName, key, value string) []otelSpanObservation {
	var matches []otelSpanObservation
	for _, observation := range observations {
		if observation.ServiceName == serviceName && observation.Attributes[key] == value {
			matches = append(matches, observation)
		}
	}
	return matches
}

func parseOTELFileExporterObservations(data []byte) ([]otelSpanObservation, bool, error) {
	complete := data
	incompleteTail := false
	if len(data) > 0 && data[len(data)-1] != '\n' {
		incompleteTail = true
		lastNewline := bytes.LastIndexByte(data, '\n')
		if lastNewline < 0 {
			complete = nil
		} else {
			complete = data[:lastNewline+1]
		}
	}

	var observations []otelSpanObservation
	for lineIndex, line := range bytes.Split(complete, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(line, &payload); err != nil {
			return nil, incompleteTail, fmt.Errorf("JSONL line %d (%d bytes): %w", lineIndex+1, len(line), err)
		}
		extracted, err := extractSpanObservationsFromOTLP(payload)
		if err != nil {
			return nil, incompleteTail, fmt.Errorf("JSONL line %d: %w", lineIndex+1, err)
		}
		observations = append(observations, extracted...)
	}
	return observations, incompleteTail, nil
}

func extractSpanObservationsFromOTLP(payload map[string]interface{}) ([]otelSpanObservation, error) {
	var observations []otelSpanObservation
	resourceSpans, ok := payload["resourceSpans"].([]interface{})
	if !ok {
		return observations, nil
	}
	for _, rs := range resourceSpans {
		rsMap, ok := rs.(map[string]interface{})
		if !ok {
			continue
		}
		resourceAttributes := extractOTLPAttributes(rsMap["resource"])
		serviceName, _ := resourceAttributes["service.name"].(string)
		scopeSpans, ok := rsMap["scopeSpans"].([]interface{})
		if !ok {
			continue
		}
		for _, ss := range scopeSpans {
			ssMap, ok := ss.(map[string]interface{})
			if !ok {
				continue
			}
			spans, ok := ssMap["spans"].([]interface{})
			if !ok {
				continue
			}
			for _, s := range spans {
				sMap, ok := s.(map[string]interface{})
				if !ok {
					continue
				}
				traceID, err := parseOTLPTraceID(stringValue(sMap["traceId"]))
				if err != nil {
					return nil, fmt.Errorf("span %q trace ID: %w", stringValue(sMap["name"]), err)
				}
				spanID, err := parseOTLPSpanID(stringValue(sMap["spanId"]), false)
				if err != nil {
					return nil, fmt.Errorf("span %q span ID: %w", stringValue(sMap["name"]), err)
				}
				parentSpanID, err := parseOTLPSpanID(stringValue(sMap["parentSpanId"]), true)
				if err != nil {
					return nil, fmt.Errorf("span %q parent span ID: %w", stringValue(sMap["name"]), err)
				}
				observations = append(observations, otelSpanObservation{
					Name:               stringValue(sMap["name"]),
					TraceID:            traceID,
					SpanID:             spanID,
					ParentSpanID:       parentSpanID,
					ServiceName:        serviceName,
					ResourceAttributes: resourceAttributes,
					Attributes:         extractOTLPAttributes(sMap),
				})
			}
		}
	}
	return observations, nil
}

func parseOTLPTraceID(encoded string) (uuid.UUID, error) {
	decoded, err := decodeOTLPID(encoded, 16)
	if err != nil {
		return uuid.Nil, err
	}
	traceID, err := uuid.FromBytes(decoded)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid 16-byte identity: %w", err)
	}
	return traceID, nil
}

func parseOTLPSpanID(encoded string, allowEmpty bool) (uint64, error) {
	if encoded == "" && allowEmpty {
		return 0, nil
	}
	decoded, err := decodeOTLPID(encoded, 8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(decoded), nil
}

// OTLP/JSON encodes trace/span IDs as lowercase hex. Accept base64 as well so
// observations remain compatible with collectors that use protojson directly.
func decodeOTLPID(encoded string, byteLength int) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("identity is empty")
	}
	hexValue := strings.ReplaceAll(encoded, "-", "")
	if decoded, err := hex.DecodeString(hexValue); err == nil && len(decoded) == byteLength {
		return decoded, nil
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := encoding.DecodeString(encoded); err == nil && len(decoded) == byteLength {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("identity %q is not %d-byte hex or base64", encoded, byteLength)
}

func extractOTLPAttributes(container interface{}) map[string]interface{} {
	containerMap, ok := container.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	items, ok := containerMap["attributes"].([]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	attributes := make(map[string]interface{}, len(items))
	for _, item := range items {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		key, ok := itemMap["key"].(string)
		if !ok || key == "" {
			continue
		}
		if value, ok := extractOTLPAnyValue(itemMap["value"]); ok {
			attributes[key] = value
		}
	}
	return attributes
}

func extractOTLPAnyValue(raw interface{}) (interface{}, bool) {
	value, ok := raw.(map[string]interface{})
	if !ok {
		return nil, false
	}
	for _, key := range []string{"stringValue", "boolValue", "intValue", "doubleValue", "bytesValue"} {
		if item, exists := value[key]; exists {
			return item, true
		}
	}
	if array, ok := value["arrayValue"].(map[string]interface{}); ok {
		items, _ := array["values"].([]interface{})
		result := make([]interface{}, 0, len(items))
		for _, item := range items {
			if parsed, ok := extractOTLPAnyValue(item); ok {
				result = append(result, parsed)
			}
		}
		return result, true
	}
	if kvlist, ok := value["kvlistValue"].(map[string]interface{}); ok {
		return extractOTLPKeyValues(kvlist["values"]), true
	}
	return nil, false
}

func extractOTLPKeyValues(raw interface{}) map[string]interface{} {
	items, _ := raw.([]interface{})
	result := make(map[string]interface{}, len(items))
	for _, item := range items {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		key, _ := itemMap["key"].(string)
		if value, ok := extractOTLPAnyValue(itemMap["value"]); ok && key != "" {
			result[key] = value
		}
	}
	return result
}

func stringValue(value interface{}) string {
	result, _ := value.(string)
	return result
}

func TestIntegrationHelper_ParseOTELFileExporterObservationsAllowsIncompleteTail(t *testing.T) {
	payload := []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"fixture-service"}}]},"scopeSpans":[{"spans":[{"traceId":"00112233445566778899aabbccddeeff","spanId":"0123456789abcdef","parentSpanId":"0011223344556677","name":"fixture-span","attributes":[{"key":"fixture.id","value":{"stringValue":"fixture-1"}}]}]}]}]}` + "\n" + `{"resourceSpans":`)
	observations, incompleteTail, err := parseOTELFileExporterObservations(payload)
	if err != nil {
		t.Fatalf("parseOTELFileExporterObservations: %v", err)
	}
	if !incompleteTail {
		t.Fatal("expected incomplete final JSONL record to be reported")
	}
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	got := observations[0]
	if got.Name != "fixture-span" || got.TraceID != uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff") || got.SpanID != 0x0123456789abcdef || got.ParentSpanID != 0x0011223344556677 {
		t.Fatalf("unexpected span identity: %+v", got)
	}
	if got.ServiceName != "fixture-service" || got.Attributes["fixture.id"] != "fixture-1" {
		t.Fatalf("unexpected span metadata: %+v", got)
	}
}

func TestIntegrationHelper_ParseOTELFileExporterObservationsRejectsMalformedCompletedRecord(t *testing.T) {
	_, incompleteTail, err := parseOTELFileExporterObservations([]byte("{not-json}\n"))
	if err == nil {
		t.Fatal("expected malformed completed JSONL record to fail")
	}
	if incompleteTail {
		t.Fatal("newline-terminated malformed record must not be classified as an incomplete tail")
	}
}

// -------------------------------------------------------------------
// Test isolation helpers
// -------------------------------------------------------------------

// uniqueBasePath returns a unique Keeper base path per test to avoid interference.
func uniqueBasePath(t *testing.T) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "-")
	return fmt.Sprintf("/click-dog/test-%s-%d", name, time.Now().UnixNano())
}

// newCHReader creates a ClickHouseReader connected to the given node.
func newCHReader(t *testing.T, node int) *chreader.ClickHouseReader {
	t.Helper()
	reader, err := chreader.NewClickHouseReader(config.ClickHouseConfig{
		Host:           integrationCHHost(node),
		Port:           integrationCHPort(node),
		Database:       "default",
		Username:       "default",
		Password:       "",
		MaxOpenConns:   2,
		MaxIdleConns:   1,
		QueryTimeoutS:  30,
		MaxMemoryUsage: 104857600,
	}, config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create ClickHouseReader for node %d: %v", node, err)
	}
	return reader
}

// newClusterCHReader creates a ClickHouseReader with cluster() queries enabled,
// connected to the specified node.
func newClusterCHReader(t *testing.T, node int, clusterName string) *chreader.ClickHouseReader {
	t.Helper()
	reader, err := chreader.NewClickHouseReader(config.ClickHouseConfig{
		Host:              integrationCHHost(node),
		Port:              integrationCHPort(node),
		Database:          "default",
		Username:          "default",
		Password:          "",
		Cluster:           clusterName,
		UseClusterQueries: true,
		MaxOpenConns:      2,
		MaxIdleConns:      1,
		QueryTimeoutS:     30,
		MaxMemoryUsage:    104857600,
	}, config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create cluster ClickHouseReader for node %d: %v", node, err)
	}
	return reader
}

// newOTELExporter creates an OTELExporter connected to the integration collector.
func newOTELExporter(t *testing.T, serviceName string) *export.OTELExporter {
	t.Helper()
	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: integrationOTELAddr(),
		ServiceName:      serviceName,
	})
	if err != nil {
		t.Fatalf("Failed to create OTELExporter: %v", err)
	}
	return exporter
}
