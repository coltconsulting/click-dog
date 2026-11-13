package processor

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

// TestProcess_CommitsTheLivePageOnlyWhenItWasHandled pins when a cycle lets
// the reader's walk move past the page it fetched. A page that was not
// handled — an export failed, or a trace filter's lookup failed — must stay
// uncommitted so the next cycle reads it again; committing it would skip
// those spans for good.
func TestProcess_CommitsTheLivePageOnlyWhenItWasHandled(t *testing.T) {
	trace := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	span := model.OpenTelemetrySpan{
		TraceID:       trace,
		SpanID:        1,
		OperationName: "query",
		Attributes:    map[string]string{"clickhouse.query_id": "q-1"},
	}
	enrich := true
	lookupErr := errors.New("query_log unavailable")

	tests := []struct {
		name       string
		filters    config.FiltersConfig
		reader     mockLiveReader
		exportErr  error
		wantErr    bool
		wantCommit int
	}{
		{
			name:       "exported page commits",
			reader:     mockLiveReader{spans: []model.OpenTelemetrySpan{span}},
			wantCommit: 1,
		},
		{
			name:       "empty page commits",
			reader:     mockLiveReader{},
			wantCommit: 1,
		},
		{
			name:      "failed export leaves the page uncommitted",
			reader:    mockLiveReader{spans: []model.OpenTelemetrySpan{span}},
			exportErr: errors.New("collector unavailable"),
			wantErr:   true,
		},
		{
			name:    "failed fetch leaves the walk uncommitted",
			reader:  mockLiveReader{fetchErr: errors.New("span log unavailable")},
			wantErr: true,
		},
		{
			// The trace filters decide on their own lookups, so a failed
			// enrichment only costs the spans their metadata.
			name:       "IP whitelist with failed enrichment still decides and commits",
			filters:    config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader:     mockLiveReader{spans: []model.OpenTelemetrySpan{span}, queryLogsErr: lookupErr},
			wantCommit: 1,
		},
		{
			name:    "IP whitelist with a failed span-log lookup fails the cycle uncommitted",
			filters: config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader: mockLiveReader{
				spans:            []model.OpenTelemetrySpan{{TraceID: trace, SpanID: 2, OperationName: "MergeTreeSelect", Attributes: map[string]string{}}},
				traceQueryIDsErr: lookupErr,
			},
			wantErr: true,
		},
		{
			name:    "user whitelist with a failed query_log lookup fails the cycle uncommitted",
			filters: config.FiltersConfig{WhitelistUsers: []string{"app"}},
			reader:  mockLiveReader{spans: []model.OpenTelemetrySpan{span}, originsErr: lookupErr},
			wantErr: true,
		},
		{
			// Without a trace filter a failed enrichment only exports
			// unenriched spans; they were delivered, so the walk moves on.
			name:       "failed enrichment without a trace filter commits",
			reader:     mockLiveReader{spans: []model.OpenTelemetrySpan{span}, queryLogsErr: lookupErr},
			wantCommit: 1,
		},
		{
			name:    "user blacklist with a failed query_log lookup fails the cycle uncommitted",
			filters: config.FiltersConfig{BlacklistUsers: []string{"batch"}},
			reader:  mockLiveReader{spans: []model.OpenTelemetrySpan{span}, originsErr: lookupErr},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Monitor: config.MonitorConfig{Enabled: true, MinTraceDurationMs: 1000, CheckIntervalS: 30, MaxSpansPerCycle: 100, EnrichFromQueryLog: &enrich},
				Filters: tt.filters,
			}
			reader := tt.reader
			reader.healthy = true
			exporter := &mockExporter{exportSpansFunc: func(_ context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
				if tt.exportErr != nil {
					return nil, tt.exportErr
				}
				keys := make([]model.SpanKey, 0, len(spans))
				for _, s := range spans {
					keys = append(keys, model.KeyOf(s))
				}
				return keys, nil
			}}
			pipeline := mustLivePipeline(t, &reader, exporter, cfg, nil, metrics.NewMetrics())

			err := pipeline.Process(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Process error = %v, wantErr %t", err, tt.wantErr)
			}
			if reader.commitCalls != tt.wantCommit {
				t.Errorf("CommitLiveSpans calls = %d, want %d", reader.commitCalls, tt.wantCommit)
			}
		})
	}
}
