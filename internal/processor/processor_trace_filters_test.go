package processor

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

var (
	traceA = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	traceB = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
)

func querySpan(trace uuid.UUID, span uint64, qid, statement string) model.OpenTelemetrySpan {
	return model.OpenTelemetrySpan{
		TraceID: trace, SpanID: span, OperationName: "query",
		Attributes: map[string]string{"clickhouse.query_id": qid, "db.statement": statement},
	}
}

func childSpan(trace uuid.UUID, span uint64) model.OpenTelemetrySpan {
	return childOf(trace, span, 1)
}

func childOf(trace uuid.UUID, span, parent uint64) model.OpenTelemetrySpan {
	return model.OpenTelemetrySpan{
		TraceID: trace, SpanID: span, ParentSpanID: parent, OperationName: "MergeTreeSelect",
		Attributes: map[string]string{},
	}
}

// under gives span a parent and an operation name.
func under(s model.OpenTelemetrySpan, parent uint64, operation string) model.OpenTelemetrySpan {
	s.ParentSpanID, s.OperationName = parent, operation
	return s
}

func withUser(s model.OpenTelemetrySpan, user string) model.OpenTelemetrySpan {
	s.Attributes["clickhouse.user"] = user
	return s
}

func origin(user, initialUser, addr, initialAddr string) model.QueryOrigin {
	return model.QueryOrigin{User: user, InitialUser: initialUser, Address: addr, InitialAddress: initialAddr}
}

// TestProcess_TraceScopedFilters pins the live path's filters that need a
// query's client, user, or statement: whitelist_ips, whitelist_users,
// blacklist_users, and blacklist_queries. Only a trace's query spans carry
// those, so each is decided per trace from every query the span log records
// for it, and applied to every span of the trace: an allowed query's child
// spans are exported with it, and a rejected query's child spans are dropped
// with it. Unresolved traces are withheld, and a failed lookup fails the cycle.
func TestProcess_TraceScopedFilters(t *testing.T) {
	// Page: trace A (query q-a + child) and trace B (query q-b + child).
	page := []model.OpenTelemetrySpan{
		querySpan(traceA, 1, "q-a", "SELECT * FROM events"), childSpan(traceA, 2),
		querySpan(traceB, 3, "q-b", "SELECT ssn FROM patients"), childOf(traceB, 4, 3),
	}
	appA := map[string]model.QueryOrigin{
		"q-a": origin("app", "app", "192.168.4.7", "192.168.4.7"),
		"q-b": origin("etl", "etl", "10.4.0.9", "10.4.0.9"),
	}
	lookupErr := errors.New("query_log unavailable")
	a := []string{"A/1", "A/2"}

	tests := []struct {
		name        string
		filters     config.FiltersConfig
		reader      mockLiveReader
		want        []string // exported trace/span, sorted
		wantErr     bool
		wantLookups bool // false: no span-log or query_log lookup at all
	}{
		{
			name:   "no trace-scoped filter exports everything without lookups",
			reader: mockLiveReader{spans: page, origins: appA},
			want:   []string{"A/1", "A/2", "B/3", "B/4"},
		},

		// whitelist_ips
		{
			name:        "IP whitelist exports the allowed trace with its child",
			filters:     config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader:      mockLiveReader{spans: page, origins: appA},
			want:        a,
			wantLookups: true,
		},
		{
			name:    "IP whitelist follows a distributed query's originating client",
			filters: config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-a", ""), childSpan(traceA, 2), querySpan(traceA, 3, "q-shard", "")},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a", "q-shard"}},
				origins: map[string]model.QueryOrigin{
					"q-a":     origin("app", "app", "192.168.4.7", "192.168.4.7"),
					"q-shard": origin("default", "app", "10.0.0.9", "192.168.4.7"),
				},
			},
			want:        []string{"A/1", "A/2", "A/3"},
			wantLookups: true,
		},
		{
			name:    "IP whitelist on the server subnet admits no distributed query",
			filters: config.FiltersConfig{WhitelistIPs: []string{"10.0.0.0/8"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-a", ""), querySpan(traceA, 3, "q-shard", "")},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a", "q-shard"}},
				origins: map[string]model.QueryOrigin{
					"q-a":     origin("app", "app", "192.168.4.7", "192.168.4.7"),
					"q-shard": origin("default", "app", "10.0.0.9", "192.168.4.7"),
				},
			},
			wantLookups: true,
		},
		{
			// Under traceparent one trace can carry queries from several
			// clients; one outside the whitelist withholds the trace.
			name:    "IP whitelist drops a trace with a query from another client",
			filters: config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-a", ""), childSpan(traceA, 2), querySpan(traceA, 3, "q-other", "")},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a", "q-other"}},
				origins: map[string]model.QueryOrigin{
					"q-a":     origin("app", "app", "192.168.4.7", "192.168.4.7"),
					"q-other": origin("app", "app", "10.9.9.9", "10.9.9.9"),
				},
			},
			wantLookups: true,
		},
		{
			name:    "IP whitelist resolves a child page through the span log",
			filters: config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{childSpan(traceA, 2)},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a"}},
				origins:       appA,
			},
			want:        []string{"A/2"},
			wantLookups: true,
		},
		{
			// Enrichment only adds metadata; the decision has its own lookup.
			name:        "IP whitelist does not depend on enrichment",
			filters:     config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader:      mockLiveReader{spans: page, origins: appA, queryLogsErr: lookupErr},
			want:        a,
			wantLookups: true,
		},

		// whitelist_users
		{
			name:        "user whitelist exports the allowed trace with its child",
			filters:     config.FiltersConfig{WhitelistUsers: []string{"app"}},
			reader:      mockLiveReader{spans: page, origins: appA},
			want:        a,
			wantLookups: true,
		},
		{
			name:    "user whitelist admits a shard query run for an allowed client",
			filters: config.FiltersConfig{WhitelistUsers: []string{"app"}},
			reader: mockLiveReader{
				spans:   []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-shard", ""), childSpan(traceA, 2)},
				origins: map[string]model.QueryOrigin{"q-shard": origin("default", "app", "10.0.0.9", "192.168.4.7")},
			},
			want:        a,
			wantLookups: true,
		},
		{
			// From 25.8 the shard's query span carries the account it ran as.
			name:    "user whitelist ignores the interserver account stamped on a shard span",
			filters: config.FiltersConfig{WhitelistUsers: []string{"app"}},
			reader: mockLiveReader{
				spans: []model.OpenTelemetrySpan{
					withUser(querySpan(traceA, 1, "q-shard", ""), "default"), childSpan(traceA, 2),
				},
				origins: map[string]model.QueryOrigin{"q-shard": origin("default", "app", "10.0.0.9", "192.168.4.7")},
			},
			want:        a,
			wantLookups: true,
		},
		{
			name:    "user blacklist drops a trace from the account stamped on its span",
			filters: config.FiltersConfig{BlacklistUsers: []string{"default"}},
			reader: mockLiveReader{
				spans: []model.OpenTelemetrySpan{
					withUser(querySpan(traceA, 1, "q-shard", ""), "default"), childSpan(traceA, 2),
				},
				origins: map[string]model.QueryOrigin{"q-shard": origin("app", "app", "10.0.0.9", "192.168.4.7")},
			},
		},
		{
			name:    "whitelisting the interserver account admits no other client's shard query",
			filters: config.FiltersConfig{WhitelistUsers: []string{"default"}},
			reader: mockLiveReader{
				spans:   []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-shard", ""), childSpan(traceA, 2)},
				origins: map[string]model.QueryOrigin{"q-shard": origin("default", "etl", "10.0.0.9", "10.4.0.9")},
			},
			wantLookups: true,
		},
		{
			name:    "user whitelist drops a trace with another user's query",
			filters: config.FiltersConfig{WhitelistUsers: []string{"app"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-a", ""), childSpan(traceA, 2)},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a", "q-b"}},
				origins:       appA,
			},
			wantLookups: true,
		},
		{
			name:        "user whitelist withholds a query with no query_log row",
			filters:     config.FiltersConfig{WhitelistUsers: []string{"app"}},
			reader:      mockLiveReader{spans: page, origins: map[string]model.QueryOrigin{}},
			wantLookups: true,
		},

		// blacklist_users
		{
			name:        "user blacklist drops the blacklisted trace with its child",
			filters:     config.FiltersConfig{BlacklistUsers: []string{"etl"}},
			reader:      mockLiveReader{spans: page, origins: appA},
			want:        a,
			wantLookups: true,
		},
		{
			name:    "user blacklist resolves a child page through the span log",
			filters: config.FiltersConfig{BlacklistUsers: []string{"etl"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{childSpan(traceB, 4)},
				traceQueryIDs: map[uuid.UUID][]string{traceB: {"q-b"}},
				origins:       appA,
			},
			wantLookups: true,
		},
		{
			name:    "user blacklist matches a shard query on its initial user",
			filters: config.FiltersConfig{BlacklistUsers: []string{"etl"}},
			reader: mockLiveReader{
				spans:   []model.OpenTelemetrySpan{querySpan(traceB, 3, "q-shard", ""), childSpan(traceB, 4)},
				origins: map[string]model.QueryOrigin{"q-shard": origin("default", "etl", "10.0.0.9", "10.4.0.9")},
			},
			wantLookups: true,
		},
		{
			// The page holds an allowed query of the trace; the blacklisted
			// initiator landed in another page and only the span log has it.
			name:    "user blacklist drops a trace whose blacklisted query is in another page",
			filters: config.FiltersConfig{BlacklistUsers: []string{"etl"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-a", ""), childSpan(traceA, 2)},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a", "q-b"}},
				origins:       appA,
			},
			wantLookups: true,
		},
		{
			// ClickHouse logs a query span when the query finishes, which
			// brings the trace back to be decided then.
			name:        "user blacklist withholds a trace with no query span yet",
			filters:     config.FiltersConfig{BlacklistUsers: []string{"etl"}},
			reader:      mockLiveReader{spans: []model.OpenTelemetrySpan{childSpan(traceA, 2)}, origins: appA},
			wantLookups: true,
		},
		{
			name:        "user blacklist withholds a query with no query_log row",
			filters:     config.FiltersConfig{BlacklistUsers: []string{"etl"}},
			reader:      mockLiveReader{spans: page, origins: map[string]model.QueryOrigin{"q-b": appA["q-b"]}},
			wantLookups: true,
		},
		{
			name:        "user whitelist and blacklist both apply",
			filters:     config.FiltersConfig{WhitelistUsers: []string{"app", "etl"}, BlacklistUsers: []string{"etl"}},
			reader:      mockLiveReader{spans: page, origins: appA},
			want:        a,
			wantLookups: true,
		},

		// blacklist_queries
		{
			name:        "query blacklist drops the matching query's child spans too",
			filters:     config.FiltersConfig{BlacklistQueries: []string{"patients"}},
			reader:      mockLiveReader{spans: page},
			want:        a,
			wantLookups: true,
		},
		{
			name:    "query blacklist decides a child page from the span log's statement",
			filters: config.FiltersConfig{BlacklistQueries: []string{"patients"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{childSpan(traceB, 4), childSpan(traceA, 2)},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a"}, traceB: {"q-b"}},
				statements:    map[string]string{"q-a": "SELECT * FROM events", "q-b": "SELECT ssn FROM patients"},
			},
			want:        []string{"A/2"},
			wantLookups: true,
		},
		{
			// A distributed query's rewritten secondary SQL need not match;
			// the initial query's statement decides the trace.
			name:    "query blacklist drops every query of a trace with one matching statement",
			filters: config.FiltersConfig{BlacklistQueries: []string{`FROM patients\b`}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{querySpan(traceB, 5, "q-b-shard", "SELECT ssn FROM patients_local"), childSpan(traceB, 4)},
				traceQueryIDs: map[uuid.UUID][]string{traceB: {"q-b", "q-b-shard"}},
				statements:    map[string]string{"q-b": "SELECT ssn FROM patients"},
			},
			wantLookups: true,
		},
		{
			name:        "query blacklist withholds a trace with no query span yet",
			filters:     config.FiltersConfig{BlacklistQueries: []string{"patients"}},
			reader:      mockLiveReader{spans: []model.OpenTelemetrySpan{childSpan(traceA, 2)}},
			wantLookups: true,
		},

		// A span is exported only under a query span that was read.
		{
			// The query span of a query still running is not written yet; the
			// trace's other query must not vouch for its spans.
			name:    "a running query's spans are withheld though another query of the trace passes",
			filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			reader: mockLiveReader{spans: []model.OpenTelemetrySpan{
				querySpan(traceA, 1, "q-a", "SELECT 1"), childOf(traceA, 11, 10), childOf(traceA, 12, 11),
			}},
			want:        []string{"A/1"},
			wantLookups: true,
		},
		{
			// blacklist_operations keeps QueryPipelineEx out of the page, and the
			// trace's query span is in a later page; the span log links them.
			name:    "a span whose parent the page lacks is attributed through the span log",
			filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			reader: mockLiveReader{pages: [][]model.OpenTelemetrySpan{
				{childOf(traceA, 3, 2)},
				{under(childSpan(traceA, 2), 1, "QueryPipelineEx"), querySpan(traceA, 1, "q-a", "SELECT 1")},
			}},
			want:        []string{"A/3"},
			wantLookups: true,
		},
		{
			name:    "a connection span is attributed to the query under it",
			filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			reader: mockLiveReader{spans: []model.OpenTelemetrySpan{
				under(childSpan(traceA, 5), 99, "TCPHandler"),
				under(querySpan(traceA, 6, "q-a", "SELECT 1"), 5, "query"),
				childOf(traceA, 7, 6),
			}},
			want:        []string{"A/5", "A/6", "A/7"},
			wantLookups: true,
		},
		{
			// Span 20 is inside a query still running (span 19, not written);
			// a dictionary load it started finished as query 21.
			name:    "a span that starts a nested query is not attributed by it",
			filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			reader: mockLiveReader{spans: []model.OpenTelemetrySpan{
				under(childSpan(traceA, 20), 19, "DB::InterpreterSelectQuery::execute()"),
				under(querySpan(traceA, 21, "q-dict", "SELECT * FROM dict_source"), 20, "query"),
				childOf(traceA, 22, 21),
			}},
			want:        []string{"A/21", "A/22"},
			wantLookups: true,
		},
		{
			name:    "a span that carries a query ID but is not a query span anchors nothing",
			filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			reader: mockLiveReader{spans: []model.OpenTelemetrySpan{
				under(querySpan(traceA, 8, "q-run", ""), 30, "RemoteQueryExecutor::execute"),
				childOf(traceA, 9, 8),
			}},
			wantLookups: true,
		},

		{
			// Cluster mode: the page read reached the replica holding mallory's
			// query, the span log read another replica, which holds only q-y.
			name:    "a page query span the span log read did not return is still judged",
			filters: config.FiltersConfig{WhitelistUsers: []string{"app"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-x", ""), childSpan(traceA, 2)},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-y"}},
				querySpanIDs:  map[string]uint64{"q-y": 50},
				origins: map[string]model.QueryOrigin{
					"q-x": origin("mallory", "mallory", "10.0.0.5", "10.0.0.5"),
					"q-y": origin("app", "app", "10.0.0.6", "10.0.0.6"),
				},
			},
			wantLookups: true,
		},
		{
			name:    "a query span with no query ID anchors nothing",
			filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			reader: mockLiveReader{
				spans:         []model.OpenTelemetrySpan{under(childSpan(traceA, 1), 0, "query"), childSpan(traceA, 2)},
				traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a"}},
				querySpanIDs:  map[string]uint64{"q-a": 40},
			},
			wantLookups: true,
		},
		{
			// A shard came back after the INSERT finished; the queued send
			// starts long after the INSERT's query span, below the read floor.
			name:    "a delayed async insert send whose INSERT is no longer read is withheld",
			filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			reader: mockLiveReader{spans: []model.OpenTelemetrySpan{
				under(childSpan(traceA, 30), 20, "void DB::DistributedAsyncInsertDirectoryQueue::processFile(std::string &, const SettingsChanges &)"),
				under(childSpan(traceA, 31), 30, "TCPHandler"),
				under(querySpan(traceA, 40, "q-shard", "INSERT INTO t_local FORMAT Native"), 31, "query"),
			}},
			want:        []string{"A/31", "A/40"},
			wantLookups: true,
		},

		// whitelist_operations is per span by design: it names operations.
		{
			name:    "operation whitelist keeps only the named operations",
			filters: config.FiltersConfig{WhitelistOperations: []string{"query"}},
			reader:  mockLiveReader{spans: page},
			want:    []string{"A/1", "B/3"},
		},
		{
			name:        "operation whitelist applies after the trace decision",
			filters:     config.FiltersConfig{WhitelistOperations: []string{"query"}, WhitelistUsers: []string{"app"}},
			reader:      mockLiveReader{spans: page, origins: appA},
			want:        []string{"A/1"},
			wantLookups: true,
		},

		// Lookup failures fail the cycle and export nothing.
		{
			name:        "a failed span-log lookup fails the cycle",
			filters:     config.FiltersConfig{BlacklistQueries: []string{"patients"}},
			reader:      mockLiveReader{spans: page, traceQueryIDsErr: lookupErr},
			wantErr:     true,
			wantLookups: true,
		},
		{
			name:        "a failed query_log lookup fails the cycle",
			filters:     config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
			reader:      mockLiveReader{spans: page, originsErr: lookupErr},
			wantErr:     true,
			wantLookups: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enrich := true
			cfg := &config.Config{
				Monitor: config.MonitorConfig{Enabled: true, CheckIntervalS: 30, MaxSpansPerCycle: 100, EnrichFromQueryLog: &enrich},
				Filters: tt.filters,
			}
			reader := tt.reader
			reader.healthy = true
			var got []string
			exporter := &mockExporter{exportSpansFunc: func(_ context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
				keys := make([]model.SpanKey, 0, len(spans))
				for _, s := range spans {
					got = append(got, strings.ToUpper(s.TraceID.String()[:1])+"/"+strconv.FormatUint(s.SpanID, 10))
					keys = append(keys, model.KeyOf(s))
				}
				return keys, nil
			}}
			pipeline := mustLivePipeline(t, &reader, exporter, cfg, nil, metrics.NewMetrics())

			err := pipeline.Process(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Process error = %v, wantErr %t", err, tt.wantErr)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("exported %v, want %v", got, tt.want)
			}
			wantCommit := 1
			if tt.wantErr {
				wantCommit = 0
			}
			if reader.commitCalls != wantCommit {
				t.Errorf("CommitLiveSpans calls = %d, want %d", reader.commitCalls, wantCommit)
			}
			if looked := reader.traceQueryIDCalls > 0; looked != tt.wantLookups {
				t.Errorf("span-log lookup ran = %t, want %t", looked, tt.wantLookups)
			}
			if strings.HasPrefix(tt.name, "query blacklist") && reader.originsCalls != 0 {
				t.Errorf("query blacklist alone read query_log %d times, want none", reader.originsCalls)
			}
		})
	}
}

// TestFilteredTraces_SpanLogLookupStartsAtTheDecidedTraces pins the bound on
// the span-log lookup: it reads from the earliest start among the page's spans
// of the traces it decides, so a trace the page's own statement already
// dropped does not widen the read.
func TestFilteredTraces_SpanLogLookupStartsAtTheDecidedTraces(t *testing.T) {
	page := []model.OpenTelemetrySpan{
		querySpan(traceB, 1, "q-b", "SELECT ssn FROM patients"),
		querySpan(traceA, 2, "q-a", "SELECT * FROM events"),
		childOf(traceA, 3, 2),
	}
	page[0].StartTimeUs = 1_000_000
	page[1].StartTimeUs = 5_000_000
	page[2].StartTimeUs = 4_000_000
	f, err := filter.NewQueryFilter(config.FiltersConfig{BlacklistQueries: []string{"patients"}})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	reader := &mockLiveReader{spans: page}
	decision, err := FilteredTraces(context.Background(), reader, page, f, 1)
	if err != nil {
		t.Fatalf("FilteredTraces: %v", err)
	}
	if !decision.Dropped[traceB] || decision.Dropped[traceA] || len(decision.Unattributed) != 0 {
		t.Fatalf("decision = %+v, want trace B dropped and nothing else withheld", decision)
	}
	if reader.lastTraceQueryStartUs != 4_000_000 {
		t.Errorf("lookup earliest start = %d, want 4000000 (trace A's earliest span)", reader.lastTraceQueryStartUs)
	}
	if got := EarliestStartUs(page, nil); got != 0 {
		t.Errorf("EarliestStartUs with no traces = %d, want 0", got)
	}
}

// TestProcess_RunningQueryIsDecidedWhenItFinishes follows the leak the
// attribution rule closes across two cycles: an allowed query and a query
// still running share a traceparent. The first cycle exports only the allowed
// query's span. When the running query's span lands, the trace is read again
// and decided on it.
func TestProcess_RunningQueryIsDecidedWhenItFinishes(t *testing.T) {
	first := []model.OpenTelemetrySpan{
		querySpan(traceA, 1, "q-a", "SELECT 1"), childOf(traceA, 11, 10), childOf(traceA, 12, 11),
	}
	for _, tt := range []struct {
		name      string
		statement string
		want      []string
	}{
		{"a blocked query's spans never leave", "SELECT * FROM secret_report", []string{"A/1"}},
		{"an allowed query's spans follow it", "SELECT * FROM events", []string{"A/1", "A/10", "A/11", "A/12"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			second := []model.OpenTelemetrySpan{
				querySpan(traceA, 10, "q-run", tt.statement), childOf(traceA, 11, 10), childOf(traceA, 12, 11),
			}
			// The span log holds the second page only from the second cycle on.
			reader := &mockLiveReader{healthy: true, pages: [][]model.OpenTelemetrySpan{first, second}}
			enrich := true
			cfg := &config.Config{
				Monitor: config.MonitorConfig{Enabled: true, CheckIntervalS: 30, MaxSpansPerCycle: 100, EnrichFromQueryLog: &enrich},
				Filters: config.FiltersConfig{BlacklistQueries: []string{"secret"}},
			}
			var got []string
			exporter := &mockExporter{exportSpansFunc: func(_ context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
				keys := make([]model.SpanKey, 0, len(spans))
				for _, s := range spans {
					got = append(got, strings.ToUpper(s.TraceID.String()[:1])+"/"+strconv.FormatUint(s.SpanID, 10))
					keys = append(keys, model.KeyOf(s))
				}
				return keys, nil
			}}
			pipeline := mustLivePipeline(t, reader, exporter, cfg, nil, metrics.NewMetrics())

			hidden := reader.pages
			reader.pages = hidden[:1]
			if err := pipeline.Process(context.Background()); err != nil {
				t.Fatalf("cycle 1: %v", err)
			}
			if strings.Join(got, ",") != "A/1" {
				t.Fatalf("cycle 1 exported %v, want only the allowed query's span A/1", got)
			}
			reader.pages = hidden[1:]
			if err := pipeline.Process(context.Background()); err != nil {
				t.Fatalf("cycle 2: %v", err)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("exported %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFilteredTraces_AsksOnlyForTheAncestorsItNeeds pins the span-log read's
// ancestor request: the traces with spans the page cannot attribute, up to the
// latest such span's start, and none when the page attributes every span.
func TestFilteredTraces_AsksOnlyForTheAncestorsItNeeds(t *testing.T) {
	f, err := filter.NewQueryFilter(config.FiltersConfig{BlacklistQueries: []string{"secret"}})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	whole := []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-a", "SELECT 1"), childOf(traceA, 2, 1)}
	reader := &mockLiveReader{spans: whole}
	if _, err := FilteredTraces(context.Background(), reader, whole, f, 1); err != nil {
		t.Fatalf("FilteredTraces: %v", err)
	}
	if len(reader.lastAncestors.Traces) != 0 {
		t.Errorf("asked for ancestors %+v of a page that attributes every span", reader.lastAncestors)
	}

	split := []model.OpenTelemetrySpan{querySpan(traceA, 1, "q-a", "SELECT 1"), childOf(traceB, 7, 6), childOf(traceB, 8, 6)}
	split[1].StartTimeUs, split[2].StartTimeUs = 4_000_000, 5_000_000
	reader = &mockLiveReader{spans: split}
	if _, err := FilteredTraces(context.Background(), reader, split, f, 1); err != nil {
		t.Fatalf("FilteredTraces: %v", err)
	}
	if got := reader.lastAncestors; len(got.Traces) != 1 || got.Traces[0] != traceB || got.StartedByUs != 5_000_000 {
		t.Errorf("ancestors = %+v, want trace B up to 5000000", got)
	}
}

// TestFilteredTraces_AncestorsStartBeforeTheirSpans pins what the ancestor
// bound may skip: a span that started after the latest span to attribute
// cannot be its ancestor, and a later span under the same query span must not
// make the read reach it.
func TestFilteredTraces_AncestorsStartBeforeTheirSpans(t *testing.T) {
	f, err := filter.NewQueryFilter(config.FiltersConfig{BlacklistQueries: []string{"secret"}})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	at := func(s model.OpenTelemetrySpan, startUs, finishUs uint64) model.OpenTelemetrySpan {
		s.StartTimeUs, s.FinishTimeUs = startUs, finishUs
		return s
	}
	page := []model.OpenTelemetrySpan{at(childOf(traceA, 3, 2), 61_000_000, 62_000_000)}
	spanLog := []model.OpenTelemetrySpan{
		at(querySpan(traceA, 1, "q-a", "SELECT 1"), 60_000_000, 70_000_000),
		at(under(childSpan(traceA, 2), 1, "QueryPipelineEx"), 60_500_000, 69_000_000),
		// Started after span 3, so it is no ancestor of it, though it is a parent.
		at(under(childSpan(traceA, 4), 1, "QueryPipelineEx"), 65_000_000, 68_000_000),
		at(childOf(traceA, 5, 4), 66_000_000, 67_000_000),
	}
	reader := &mockLiveReader{spans: page, pages: [][]model.OpenTelemetrySpan{spanLog}}
	decision, err := FilteredTraces(context.Background(), reader, page, f, 1)
	if err != nil {
		t.Fatalf("FilteredTraces: %v", err)
	}
	if decision.Withholds(&page[0]) {
		t.Errorf("span 3 withheld; its parent 2 and query span 1 are in the span log: %+v", decision)
	}
	if got := reader.lastAncestors.StartedByUs; got != 61_000_000 {
		t.Errorf("ancestors started by %d, want 61000000 (span 3's start)", got)
	}
}
