package processor

import (
	"context"

	"github.com/coltconsulting/click-dog/internal/model"
)

// mockExporter is a fixture implementing model.SpanExporter for tests in
// the main package that need to drive processor logic without standing
// up a real OTLP gRPC backend. The corresponding tests for the exporter
// implementations themselves live in internal/export.
type mockExporter struct {
	exportSpansFunc  func(ctx context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error)
	exportQueryFunc  func(ctx context.Context, log model.QueryLog) error
	closeFunc        func(ctx context.Context) error
	exportSpansCalls int
	exportQueryCalls int
	closeCalls       int
}

func (m *mockExporter) ExportSpans(ctx context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	m.exportSpansCalls++
	if m.exportSpansFunc != nil {
		keys, err := m.exportSpansFunc(ctx, spans)
		return model.ExportResult{
			Accepted:      keys,
			TotalSent:     len(spans),
			TotalAccepted: len(keys),
			Sinks: []model.ExportSinkStatus{{
				Name:     "mock",
				Sent:     len(spans),
				Accepted: len(keys),
				Error:    err,
			}},
		}, err
	}
	keys := make([]model.SpanKey, len(spans))
	for i, s := range spans {
		keys[i] = model.KeyOf(s)
	}
	return model.ExportResult{
		Accepted:      keys,
		TotalSent:     len(spans),
		TotalAccepted: len(keys),
		Sinks: []model.ExportSinkStatus{{
			Name:     "mock",
			Sent:     len(spans),
			Accepted: len(keys),
		}},
	}, nil
}

func (m *mockExporter) ExportQuery(ctx context.Context, log model.QueryLog) (model.ExportResult, error) {
	m.exportQueryCalls++
	if m.exportQueryFunc != nil {
		err := m.exportQueryFunc(ctx, log)
		result := model.ExportResult{TotalSent: 1}
		if err == nil {
			result.TotalAccepted = 1
		}
		result.Sinks = []model.ExportSinkStatus{{
			Name:     "mock",
			Sent:     1,
			Accepted: result.TotalAccepted,
			Error:    err,
		}}
		return result, err
	}
	return model.ExportResult{
		TotalSent:     1,
		TotalAccepted: 1,
		Sinks: []model.ExportSinkStatus{{
			Name:     "mock",
			Sent:     1,
			Accepted: 1,
		}},
	}, nil
}

func (m *mockExporter) Close(ctx context.Context) error {
	m.closeCalls++
	if m.closeFunc != nil {
		return m.closeFunc(ctx)
	}
	return nil
}
