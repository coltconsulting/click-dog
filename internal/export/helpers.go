package export

import (
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	liveSpanSourceKey     = "click_dog.source"
	liveSpanDefaultSource = "span_log"
	liveSpanQueryKey      = "db.statement"
)

func liveSpanSource(attributes map[string]string) string {
	if source, ok := attributes[liveSpanSourceKey]; ok {
		return source
	}
	return liveSpanDefaultSource
}

// liveSpanAttributesForHEC returns the nested attributes serialized by Splunk
// HEC: string attributes as strings and string-array attributes as JSON arrays.
// An array replaces a string attribute of the same name, as in the OTLP exporter.
// The effective source is promoted to click_dog_source on the event, so it is
// omitted here to avoid two representations that can disagree. Copying also lets
// the exporter truncate SQL without mutating the span shared by other exporters
// in a MultiExporter fan-out.
func liveSpanAttributesForHEC(span model.OpenTelemetrySpan, maxQueryLength int) map[string]interface{} {
	if span.Attributes == nil && span.StringSliceAttributes == nil {
		return nil
	}

	exported := make(map[string]interface{}, len(span.Attributes)+len(span.StringSliceAttributes))
	for key, value := range span.Attributes {
		if key == liveSpanSourceKey {
			continue
		}
		if key == liveSpanQueryKey {
			value = TruncateQuery(value, maxQueryLength)
		}
		exported[key] = value
	}
	for key, values := range span.StringSliceAttributes {
		if key == liveSpanSourceKey {
			continue
		}
		exported[key] = values
	}
	return exported
}

// TruncateQuery trims whitespace and truncates the query to maxLen characters.
// If maxLen <= 0, no truncation is applied.
func TruncateQuery(query string, maxLen int) string {
	query = strings.TrimSpace(query)
	if maxLen > 0 && len(query) > maxLen {
		return query[:maxLen] + "..."
	}
	return query
}

// Attribute helpers

func StringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

func IntAttr(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}},
	}
}

func BoolAttr(key string, value bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}},
	}
}

func StringSliceAttr(key string, values []string) *commonpb.KeyValue {
	vals := make([]*commonpb.AnyValue, len(values))
	for i, v := range values {
		vals[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}
	}
	return &commonpb.KeyValue{
		Key: key,
		Value: &commonpb.AnyValue{
			Value: &commonpb.AnyValue_ArrayValue{
				ArrayValue: &commonpb.ArrayValue{Values: vals},
			},
		},
	}
}
