package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// nativeUInt64Row deliberately models clickhouse-go's strict UInt64 scan
// contract. Unlike the shared fakeRow helper, it does not use reflection to
// convert UInt64 into a signed destination; that makes this test fail against
// the pre-fix *int64 scan in RunCanaryQuery.
type nativeUInt64Row uint64

func (r nativeUInt64Row) Err() error { return nil }

func (r nativeUInt64Row) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("nativeUInt64Row.Scan got %d destinations, want 1", len(dest))
	}
	count, ok := dest[0].(*uint64)
	if !ok {
		return fmt.Errorf("cannot scan UInt64 into %T", dest[0])
	}
	*count = uint64(r)
	return nil
}

func (nativeUInt64Row) ScanStruct(any) error {
	return fmt.Errorf("nativeUInt64Row.ScanStruct is unsupported")
}

func TestRunCanaryQuery_ScansNativeUnsignedCount(t *testing.T) {
	conn := &fakeConn{row: nativeUInt64Row(7)}
	reader := &ClickHouseReader{
		conn:         conn,
		canarySelect: canarySelectSQL,
		queryTimeout: time.Second,
	}

	result, err := reader.RunCanaryQuery(context.Background(), 1500)
	if err != nil {
		t.Fatalf("RunCanaryQuery: %v", err)
	}
	if result.Count != 7 || !result.LongQueriesExist {
		t.Fatalf("result = %+v, want count 7 and long queries", result)
	}
}

// failingPingConn is a fakeConn whose ping fails.
type failingPingConn struct {
	fakeConn
	err error
}

func (c *failingPingConn) Ping(context.Context) error { return c.err }

// The /readyz probe calls Ping, which must not log: the health server logs a
// failing ping once per change of state. The polling cycle's IsHealthy keeps
// its warning on every failure.
func TestReader_PingDoesNotLogButIsHealthyWarns(t *testing.T) {
	buf := captureWarns(t)
	reader := &ClickHouseReader{conn: &failingPingConn{err: errors.New("connection refused")}}

	if err := reader.Ping(context.Background()); err == nil {
		t.Fatal("Ping = nil, want the ping error")
	}
	if buf.Len() != 0 {
		t.Errorf("Ping logged %q, want nothing", buf.String())
	}

	if reader.IsHealthy(context.Background()) {
		t.Fatal("IsHealthy = true, want false")
	}
	if !strings.Contains(buf.String(), "[WARN] ClickHouse connection health check failed: connection refused") {
		t.Errorf("IsHealthy log = %q, want the health-check warning", buf.String())
	}
}

// The processor names the canary step when it logs and returns this error, so
// the reader must not prefix it too: release testing saw "canary query failed:
// canary query failed: dial tcp ..." in the log and in /status.
func TestRunCanaryQuery_ReturnsDriverErrorWithoutPrefix(t *testing.T) {
	driverErr := errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")
	reader := &ClickHouseReader{
		conn:         &fakeConn{row: fakeRow{err: driverErr}},
		canarySelect: canarySelectSQL,
		queryTimeout: time.Second,
	}

	_, err := reader.RunCanaryQuery(context.Background(), 1500)
	if !errors.Is(err, driverErr) {
		t.Fatalf("error = %v, want the driver error", err)
	}
	if strings.Contains(err.Error(), "canary") {
		t.Errorf("error = %q, want no canary prefix from the reader", err)
	}
}

func TestRunCanaryQuery_RejectsCountAboveSignedResultCapacity(t *testing.T) {
	conn := &fakeConn{row: nativeUInt64Row(uint64(math.MaxInt64) + 1)}
	reader := &ClickHouseReader{
		conn:         conn,
		canarySelect: canarySelectSQL,
		queryTimeout: time.Second,
	}

	_, err := reader.RunCanaryQuery(context.Background(), 1500)
	if err == nil || !strings.Contains(err.Error(), "exceeds int64 result capacity") {
		t.Fatalf("error = %v, want signed-capacity error", err)
	}
}
