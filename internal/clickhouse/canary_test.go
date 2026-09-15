package clickhouse

import (
	"context"
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
