package processor

import (
	"context"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
)

func TestResolveSpanUser(t *testing.T) {
	tests := []struct {
		name        string
		attributes  map[string]string
		queryLogMap map[string]model.QueryLog
		want        string
	}{
		{
			name:       "no attributes returns empty",
			attributes: map[string]string{},
			want:       "",
		},
		{
			name: "direct clickhouse.user attribute wins",
			attributes: map[string]string{
				"clickhouse.user":     "alice",
				"clickhouse.query_id": "qid-1",
			},
			queryLogMap: map[string]model.QueryLog{
				"qid-1": {User: "bob"},
			},
			want: "alice",
		},
		{
			name: "fallback to query_log enrichment by query_id",
			attributes: map[string]string{
				"clickhouse.query_id": "qid-1",
			},
			queryLogMap: map[string]model.QueryLog{
				"qid-1": {User: "bob"},
			},
			want: "bob",
		},
		{
			name: "query_id present but missing from map",
			attributes: map[string]string{
				"clickhouse.query_id": "qid-missing",
			},
			queryLogMap: map[string]model.QueryLog{
				"other-qid": {User: "ignored"},
			},
			want: "",
		},
		{
			name: "no query_id and no clickhouse.user attribute",
			attributes: map[string]string{
				"db.statement": "SELECT 1",
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			span := model.OpenTelemetrySpan{Attributes: tt.attributes}
			got := ResolveSpanUser(span, tt.queryLogMap)
			if got != tt.want {
				t.Errorf("ResolveSpanUser(...) = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldFilterSpanByUser(t *testing.T) {
	spanWithQID := func(qid string) model.OpenTelemetrySpan {
		return model.OpenTelemetrySpan{Attributes: map[string]string{"clickhouse.query_id": qid}}
	}
	spanNoQID := model.OpenTelemetrySpan{Attributes: map[string]string{"db.statement": "SELECT 1"}}

	qmap := map[string]model.QueryLog{
		"q1": {QueryID: "q1", User: "patient_records"},
		"q2": {QueryID: "q2", User: "app_frontend"},
	}

	tests := []struct {
		name string
		cfg  config.FiltersConfig
		span model.OpenTelemetrySpan
		want bool
	}{
		{
			name: "no filter passes any span",
			cfg:  config.FiltersConfig{},
			span: spanWithQID("q1"),
			want: false,
		},
		{
			name: "whitelist drops unknown user",
			cfg:  config.FiltersConfig{WhitelistUsers: []string{"app_frontend"}},
			span: spanWithQID("missing-from-map"),
			want: true,
		},
		{
			name: "whitelist drops span with no query_id",
			cfg:  config.FiltersConfig{WhitelistUsers: []string{"app_frontend"}},
			span: spanNoQID,
			want: true,
		},
		{
			name: "whitelist passes whitelisted enriched user",
			cfg:  config.FiltersConfig{WhitelistUsers: []string{"patient_records"}},
			span: spanWithQID("q1"),
			want: false,
		},
		{
			name: "blacklist drops blacklisted enriched user",
			cfg:  config.FiltersConfig{BlacklistUsers: []string{"patient_records"}},
			span: spanWithQID("q1"),
			want: true,
		},
		{
			name: "blacklist passes unknown user",
			cfg:  config.FiltersConfig{BlacklistUsers: []string{"patient_records"}},
			span: spanWithQID("missing-from-map"),
			want: false,
		},
		{
			name: "blacklist passes span with no query_id",
			cfg:  config.FiltersConfig{BlacklistUsers: []string{"patient_records"}},
			span: spanNoQID,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := filter.NewQueryFilter(tt.cfg)
			if err != nil {
				t.Fatalf("NewQueryFilter failed: %v", err)
			}
			if got := ShouldFilterSpanByUser(tt.span, f, qmap); got != tt.want {
				t.Errorf("ShouldFilterSpanByUser = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProcessQueriesBatch_UserWhitelist(t *testing.T) {
	exp := &mockExporter{}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{
		WhitelistUsers: []string{"app_frontend"},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	queries := []model.QueryLog{
		{QueryID: "q1", User: "app_frontend", Query: "SELECT 1"},
		{QueryID: "q2", User: "patient_records", Query: "SELECT 2"},
		{QueryID: "q3", User: "", Query: "SELECT 3"}, // unknown user → dropped under whitelist
	}

	ProcessQueriesBatch(context.Background(), queries, exp, qf, &config.Config{}, nil)

	if exp.exportQueryCalls != 1 {
		t.Errorf("expected 1 query exported under whitelist, got %d", exp.exportQueryCalls)
	}
}

func TestProcessQueriesBatch_UserBlacklist(t *testing.T) {
	exp := &mockExporter{}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{
		BlacklistUsers: []string{"patient_records"},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	queries := []model.QueryLog{
		{QueryID: "q1", User: "app_frontend", Query: "SELECT 1"},
		{QueryID: "q2", User: "patient_records", Query: "SELECT 2"},
		{QueryID: "q3", User: "", Query: "SELECT 3"}, // unknown user passes blacklist-only mode
	}

	ProcessQueriesBatch(context.Background(), queries, exp, qf, &config.Config{}, nil)

	if exp.exportQueryCalls != 2 {
		t.Errorf("expected 2 queries exported (blacklist of one), got %d", exp.exportQueryCalls)
	}
}
