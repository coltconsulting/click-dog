package export

import (
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

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
