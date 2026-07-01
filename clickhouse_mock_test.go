package main

import (
	"context"
	"time"

	"github.com/coltconsulting/click-dog/internal/model"
)

// MockClickHouseReader is a fixture for tests in the main package that need
// to drive processSpansWithProtection / fetchAndProcessSpans without standing
// up a real ClickHouse instance. It is consumed by main_test.go and
// failure_test.go.
type MockClickHouseReader struct {
	spans       []model.OpenTelemetrySpan
	queries     []model.QueryLog
	fetchErr    error
	healthy     bool
	fetchCalled int
	pingCalled  int
}

func NewMockClickHouseReader() *MockClickHouseReader {
	return &MockClickHouseReader{
		healthy: true,
	}
}

func (m *MockClickHouseReader) SetSpans(spans []model.OpenTelemetrySpan) {
	m.spans = spans
}

func (m *MockClickHouseReader) SetQueries(queries []model.QueryLog) {
	m.queries = queries
}

func (m *MockClickHouseReader) SetFetchError(err error) {
	m.fetchErr = err
}

func (m *MockClickHouseReader) SetHealthy(healthy bool) {
	m.healthy = healthy
}

func (m *MockClickHouseReader) FetchOpenTelemetrySpans(_ context.Context, _, _, _, _ int, _ time.Duration, _ int) ([]model.OpenTelemetrySpan, error) {
	m.fetchCalled++
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	return m.spans, nil
}

func (m *MockClickHouseReader) FetchSlowQueriesInRange(_ context.Context, _, _, _ int, _, _ time.Time, _ int) ([]model.QueryLog, error) {
	m.fetchCalled++
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	return m.queries, nil
}

func (m *MockClickHouseReader) IsHealthy(_ context.Context) bool {
	m.pingCalled++
	return m.healthy
}

func (m *MockClickHouseReader) Close() error {
	return nil
}
