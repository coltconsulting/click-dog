package processor

import (
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/model"
)

func TestExtractLogComment_JSONInURL(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"http.url": `/?log_comment=%7B%22app%22%3A+%22Dash8%22%2C+%22query_name%22%3A+%22what_watched_programs%22%7D`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment.app"] != "Dash8" {
		t.Errorf("expected log_comment.app=Dash8, got %q", span.Attributes["log_comment.app"])
	}
	if span.Attributes["log_comment.query_name"] != "what_watched_programs" {
		t.Errorf("expected log_comment.query_name=what_watched_programs, got %q", span.Attributes["log_comment.query_name"])
	}
}

func TestExtractLogComment_JSONInURLWithOtherParams(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"http.url": `/?enable_http_compression=1&allow_experimental_analyzer=1&log_comment=%7B%22env%22%3A+%22prod%22%2C+%22app%22%3A+%22Dash8%22%7D`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment.env"] != "prod" {
		t.Errorf("expected log_comment.env=prod, got %q", span.Attributes["log_comment.env"])
	}
	if span.Attributes["log_comment.app"] != "Dash8" {
		t.Errorf("expected log_comment.app=Dash8, got %q", span.Attributes["log_comment.app"])
	}
}

func TestExtractLogComment_PlainString(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"http.url": `/?log_comment=deploy-v42`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment"] != "deploy-v42" {
		t.Errorf("expected log_comment=deploy-v42, got %q", span.Attributes["log_comment"])
	}
	// Should not have any log_comment. prefixed keys
	for k := range span.Attributes {
		if k != "log_comment" && k != "http.url" {
			t.Errorf("unexpected attribute: %s", k)
		}
	}
}

func TestExtractLogComment_NoLogComment(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"http.url": `/?enable_http_compression=1`,
		},
	}

	origLen := len(span.Attributes)
	ExtractLogComment(&span)

	if len(span.Attributes) != origLen {
		t.Errorf("expected no new attributes, got %d (was %d)", len(span.Attributes), origLen)
	}
}

func TestExtractLogComment_FallbackToAnyAttribute(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"clickhouse.uri": `/?log_comment=%7B%22app%22%3A+%22test%22%7D`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment.app"] != "test" {
		t.Errorf("expected log_comment.app=test, got %q", span.Attributes["log_comment.app"])
	}
}

func TestExtractLogComment_DirectAttribute(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"clickhouse.setting.log_comment": `{"app": "Dash8", "query_name": "what_watched"}`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment.app"] != "Dash8" {
		t.Errorf("expected log_comment.app=Dash8, got %q", span.Attributes["log_comment.app"])
	}
	if span.Attributes["log_comment.query_name"] != "what_watched" {
		t.Errorf("expected log_comment.query_name=what_watched, got %q", span.Attributes["log_comment.query_name"])
	}
}

func TestExtractLogComment_DirectAttributePreferredOverURI(t *testing.T) {
	// When both direct and URI attributes exist, direct wins (no decoding needed).
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"clickhouse.setting.log_comment": `{"app": "Direct"}`,
			"http.url":                       `/?log_comment=%7B%22app%22%3A+%22FromURI%22%7D`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment.app"] != "Direct" {
		t.Errorf("expected direct attribute to win, got log_comment.app=%q", span.Attributes["log_comment.app"])
	}
}

func TestExtractLogComment_DirectAttributePlainString(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"clickhouse.setting.log_comment": `deploy-v42`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment"] != "deploy-v42" {
		t.Errorf("expected log_comment=deploy-v42, got %q", span.Attributes["log_comment"])
	}
}

func TestExtractLogComment_HTTPTarget(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"http.target": `/?log_comment=%7B%22app%22%3A+%22MyApp%22%7D`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment.app"] != "MyApp" {
		t.Errorf("expected log_comment.app=MyApp, got %q", span.Attributes["log_comment.app"])
	}
}

func TestExtractLogComment_NestedJSON(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"http.url": `/?log_comment=%7B%22app%22%3A%22test%22%2C%22meta%22%3A%7B%22team%22%3A%22search%22%7D%7D`,
		},
	}

	ExtractLogComment(&span)

	if span.Attributes["log_comment.app"] != "test" {
		t.Errorf("expected log_comment.app=test, got %q", span.Attributes["log_comment.app"])
	}
	// Nested object should be serialized as JSON, not Go map syntax
	meta := span.Attributes["log_comment.meta"]
	if meta != `{"team":"search"}` {
		t.Errorf("expected log_comment.meta as JSON, got %q", meta)
	}
}

func TestExtractLogComment_OversizedDropped(t *testing.T) {
	// A log_comment larger than maxLogCommentSize must be skipped entirely:
	// no JSON parse, no fallback string attribute. Bounds per-span memory
	// against a hostile/buggy ClickHouse client.
	huge := strings.Repeat("a", maxLogCommentSize+1)
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"log_comment": huge,
			"keep":        "me",
		},
	}

	ExtractLogComment(&span)

	if _, ok := span.Attributes["log_comment.app"]; ok {
		t.Error("oversized log_comment was unmarshaled (should have been dropped)")
	}
	// Original log_comment value must not be re-stored as a fallback attribute.
	if got := span.Attributes["log_comment"]; got != huge {
		t.Errorf("oversized log_comment must be left as-is on the input map, not rewritten; got len %d", len(got))
	}
	// Other attributes must be untouched.
	if span.Attributes["keep"] != "me" {
		t.Errorf("unrelated attribute lost: %q", span.Attributes["keep"])
	}
}

func TestExtractLogComment_DoesNotMutateOriginal(t *testing.T) {
	original := map[string]string{
		"http.url":  `/?log_comment=%7B%22app%22%3A+%22test%22%7D`,
		"other_key": "other_value",
	}
	span := model.OpenTelemetrySpan{
		Attributes: original,
	}

	ExtractLogComment(&span)

	// Original map should be unchanged
	if _, ok := original["log_comment.app"]; ok {
		t.Error("original map was mutated")
	}
	// New map should have the extracted key
	if span.Attributes["log_comment.app"] != "test" {
		t.Errorf("expected log_comment.app=test, got %q", span.Attributes["log_comment.app"])
	}
	// Original keys should be preserved
	if span.Attributes["other_key"] != "other_value" {
		t.Errorf("expected other_key=other_value, got %q", span.Attributes["other_key"])
	}
}
