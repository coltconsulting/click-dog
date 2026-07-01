package export

import "github.com/coltconsulting/click-dog/internal/model"

func failedResult(name string, sent int, err error) model.ExportResult {
	return model.ExportResult{
		TotalSent: sent,
		Sinks: []model.ExportSinkStatus{{
			Name:  name,
			Sent:  sent,
			Error: err,
		}},
	}
}

func acceptedCountResult(name string, sent, accepted int) model.ExportResult {
	return model.ExportResult{
		TotalSent:     sent,
		TotalAccepted: accepted,
		Sinks: []model.ExportSinkStatus{{
			Name:     name,
			Sent:     sent,
			Accepted: accepted,
		}},
	}
}

func acceptedSpansResult(name string, sent int, keys []model.SpanKey) model.ExportResult {
	result := acceptedCountResult(name, sent, len(keys))
	result.Accepted = keys
	return result
}
