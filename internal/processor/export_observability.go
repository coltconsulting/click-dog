package processor

import (
	"sync"
	"sync/atomic"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

// exportSinkWarnCounts tracks repeated per-sink export warnings. The metrics
// are the durable signal; logs use exponential backoff so a dead sink nudges
// operators without flooding every polling cycle.
var exportSinkWarnCounts sync.Map

func RecordExportObservability(m *metrics.Metrics, result model.ExportResult) {
	if m != nil {
		m.RecordExportResult(result)
	}

	for _, sink := range result.Sinks {
		name := metrics.NormalizeExportSinkName(sink.Name)
		switch {
		case sink.Error != nil:
			count, ok := shouldLogExportSinkWarning(name, "error")
			if ok {
				clicklog.Warn("Exporter sink=%q accepted %d/%d items before error (%d consecutive): %v",
					name, sink.Accepted, sink.Sent, count, sink.Error)
			}
		case sink.Accepted < sink.Sent:
			count, ok := shouldLogExportSinkWarning(name, "partial")
			if ok {
				clicklog.Warn("Exporter sink=%q accepted %d/%d items without returning an error (%d consecutive)",
					name, sink.Accepted, sink.Sent, count)
			}
		default:
			resetExportSinkWarnings(name)
		}
	}
}

func shouldLogExportSinkWarning(sink, kind string) (int64, bool) {
	key := sink + "\x00" + kind
	counter, _ := exportSinkWarnCounts.LoadOrStore(key, &atomic.Int64{})
	n := counter.(*atomic.Int64).Add(1)
	return n, n == 1 || (n >= 10 && n%(ipow10(ilog10(n))) == 0)
}

func resetExportSinkWarnings(sink string) {
	exportSinkWarnCounts.Delete(sink + "\x00error")
	exportSinkWarnCounts.Delete(sink + "\x00partial")
}

func resetExportSinkWarningCounts() {
	exportSinkWarnCounts.Range(func(key, _ any) bool {
		exportSinkWarnCounts.Delete(key)
		return true
	})
}
