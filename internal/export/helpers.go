package export

import (
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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
// HEC. The effective source is promoted to click_dog_source on the event, so it
// is omitted here to avoid two representations that can disagree. Copying also
// lets the exporter truncate SQL without mutating the span shared by other
// exporters in a MultiExporter fan-out.
func liveSpanAttributesForHEC(attributes map[string]string, maxQueryLength int) map[string]string {
	if attributes == nil {
		return nil
	}

	exported := make(map[string]string, len(attributes))
	for key, value := range attributes {
		if key == liveSpanSourceKey {
			continue
		}
		if key == liveSpanQueryKey {
			value = TruncateQuery(value, maxQueryLength)
		}
		exported[key] = value
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
