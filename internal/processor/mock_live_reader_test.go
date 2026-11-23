package processor

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

type mockLiveReader struct {
	healthy bool

	spans []model.OpenTelemetrySpan
	// pages, when set, is served one slice per fetch in order (the last page
	// repeats), standing in for the reader's keyset paging across cycles.
	pages    [][]model.OpenTelemetrySpan
	fetchErr error

	queryLogs    map[string]model.QueryLog
	queryLogsErr error

	// The span log is the spans and pages this reader serves. traceQueryIDs,
	// when it names a trace, is its record of that trace's query IDs instead:
	// a query ID no served span carries stands for a query span the span log
	// holds as span querySpanIDs[id] (1 when unset, the parent childSpan
	// gives). statements gives a query ID's db.statement there.
	traceQueryIDs    map[uuid.UUID][]string
	querySpanIDs     map[string]uint64
	statements       map[string]string
	traceQueryIDsErr error

	// origins, when set, is query_log's unfiltered user and client per
	// query; otherwise they are taken from queryLogs.
	origins          map[string]model.QueryOrigin
	originsErr       error
	lastOriginsAsked []string

	healthCalls       int
	fetchCalls        int
	commitCalls       int
	queryLogCalls     int
	traceQueryIDCalls int
	originsCalls      int
	lastMinTraceMs    int
	// lastTraceQueryStartUs and lastAncestors are the earliestStartUs and
	// ancestors of the last FetchTraceNodes call.
	lastTraceQueryStartUs uint64
	lastAncestors         model.AncestorRead
	// lastOriginsStartUs is the earliestStartUs of the last
	// FetchQueryOrigins call.
	lastOriginsStartUs uint64
	lastLookback       time.Duration
	lastLimit          int
	lastFetchOpts      clickhouse.FetchOpts
	lastQueryIDs       []string
	lastQueryDays      int
	// queryLogStartUs records each FetchQueryLogByQueryIDs call's
	// earliestStartUs, in call order.
	queryLogStartUs []uint64
}

var _ LiveReader = (*mockLiveReader)(nil)

func mustLivePipeline(
	t *testing.T,
	reader LiveReader,
	exporter model.SpanExporter,
	cfg *config.Config,
	seenSpans *lru.Cache[model.SpanKey, bool],
	m *metrics.Metrics,
) *Pipeline {
	t.Helper()

	qf, err := filter.NewQueryFilter(cfg.Filters)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if seenSpans == nil {
		seenSpans, err = lru.New[model.SpanKey, bool](1024)
		if err != nil {
			t.Fatalf("lru.New: %v", err)
		}
	}
	pipeline, err := NewPipeline(Pipeline{
		Reader:    reader,
		Exporter:  exporter,
		Filter:    qf,
		Config:    cfg,
		SeenSpans: seenSpans,
		Metrics:   m,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pipeline
}

func (r *mockLiveReader) IsHealthy(context.Context) bool {
	r.healthCalls++
	return r.healthy
}

func (r *mockLiveReader) CommitLiveSpans() {
	r.commitCalls++
}

func (r *mockLiveReader) FetchLiveSpans(
	_ context.Context,
	minTraceDurationMs int,
	lookback time.Duration,
	limit int,
	opts clickhouse.FetchOpts,
) ([]model.OpenTelemetrySpan, error) {
	r.fetchCalls++
	r.lastMinTraceMs = minTraceDurationMs
	r.lastLookback = lookback
	r.lastLimit = limit
	r.lastFetchOpts = opts
	if len(r.pages) > 0 {
		i := r.fetchCalls - 1
		if i >= len(r.pages) {
			i = len(r.pages) - 1
		}
		return r.pages[i], r.fetchErr
	}
	return r.spans, r.fetchErr
}

func (r *mockLiveReader) FetchQueryLogByQueryIDs(
	_ context.Context,
	queryIDs []string,
	lookbackDays int,
	earliestStartUs uint64,
) (map[string]model.QueryLog, error) {
	r.queryLogCalls++
	r.lastQueryIDs = append(r.lastQueryIDs[:0], queryIDs...)
	r.lastQueryDays = lookbackDays
	r.queryLogStartUs = append(r.queryLogStartUs, earliestStartUs)
	return r.queryLogs, r.queryLogsErr
}

func (r *mockLiveReader) FetchTraceNodes(
	_ context.Context,
	traceIDs []uuid.UUID,
	_ int,
	earliestStartUs uint64,
	ancestors model.AncestorRead,
) (map[uuid.UUID][]model.TraceNode, error) {
	r.traceQueryIDCalls++
	r.lastTraceQueryStartUs = earliestStartUs
	r.lastAncestors = ancestors
	if r.traceQueryIDsErr != nil {
		return nil, r.traceQueryIDsErr
	}
	all := append([]model.OpenTelemetrySpan{}, r.spans...)
	for _, page := range r.pages {
		all = append(all, page...)
	}
	statementOf := func(s model.OpenTelemetrySpan) string {
		if text, ok := r.statements[s.Attributes["clickhouse.query_id"]]; ok {
			return text
		}
		return s.Attributes["db.statement"]
	}
	// Like the SQL: spans that finished from a minute before earliestStartUs,
	// and as ancestors only the parents of the asked traces' spans read.
	var floor uint64
	if margin := uint64(time.Minute.Microseconds()); earliestStartUs > margin {
		floor = earliestStartUs - margin
	}
	asked := map[uuid.UUID]bool{}
	for _, id := range ancestors.Traces {
		asked[id] = true
	}
	parents := map[model.SpanKey]bool{}
	for _, s := range all {
		if asked[s.TraceID] && s.FinishTimeUs >= floor {
			parents[model.SpanKey{TraceID: s.TraceID, SpanID: s.ParentSpanID}] = true
		}
	}

	out := make(map[uuid.UUID][]model.TraceNode, len(traceIDs))
	for _, id := range traceIDs {
		seen := map[uint64]bool{}
		listed, override := r.traceQueryIDs[id]
		carried := map[string]bool{}
		for _, s := range all {
			if s.TraceID != id || seen[s.SpanID] || s.FinishTimeUs < floor {
				continue
			}
			qid := s.Attributes["clickhouse.query_id"]
			listedHere := !override || (qid != "" && slices.Contains(listed, qid))
			ancestor := len(ancestors.Traces) > 0 && s.StartTimeUs <= ancestors.StartedByUs && parents[model.KeyOf(s)]
			if (qid != "" && listedHere) || ancestor {
				seen[s.SpanID] = true
				carried[qid] = true
				out[id] = append(out[id], model.TraceNode{
					SpanID: s.SpanID, ParentSpanID: s.ParentSpanID, Query: s.OperationName == "query",
					QueryID: qid, Statement: statementOf(s),
				})
			}
		}
		for _, qid := range listed {
			if carried[qid] {
				continue
			}
			spanID, ok := r.querySpanIDs[qid]
			if !ok {
				spanID = 1
			}
			out[id] = append(out[id], model.TraceNode{SpanID: spanID, Query: true, QueryID: qid, Statement: r.statements[qid]})
		}
	}
	return out, nil
}

func (r *mockLiveReader) FetchQueryOrigins(
	_ context.Context,
	queryIDs []string,
	_ int,
	earliestStartUs uint64,
) (map[string]model.QueryOrigin, error) {
	r.lastOriginsStartUs = earliestStartUs
	r.originsCalls++
	r.lastOriginsAsked = append(r.lastOriginsAsked[:0], queryIDs...)
	if r.originsErr != nil {
		return nil, r.originsErr
	}
	out := make(map[string]model.QueryOrigin, len(queryIDs))
	for _, qid := range queryIDs {
		if r.origins != nil {
			if o, ok := r.origins[qid]; ok {
				out[qid] = o
			}
			continue
		}
		if ql, ok := r.queryLogs[qid]; ok {
			out[qid] = model.QueryOrigin{User: ql.User, InitialUser: ql.InitialUser, Address: ql.ClientAddress, InitialAddress: ql.InitialAddress}
		}
	}
	return out, nil
}
