package analysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestDocsAnalysisSchemasMatchGoContract(t *testing.T) {
	reportSchema := readDocsSchema(t, "analysis-report-v1.schema.json")
	findingSchema := readDocsSchema(t, "analysis-finding-v1.schema.json")

	requireSchemaConst(t, reportSchema, "schema_version", ReportSchemaVersion)
	requireRequiredFields(t, "report", reportSchema, requiredJSONFields(reflect.TypeOf(AnalysisReport{})))
	requireRequiredFields(t, "report.config", schemaDef(t, reportSchema, "config"), requiredJSONFields(reflect.TypeOf(ReportConfig{})))
	requireRequiredFields(t, "report.coverage", schemaDef(t, reportSchema, "coverage"), requiredJSONFields(reflect.TypeOf(CoverageSummary{})))
	requireRequiredFields(t, "report.analyzer_run", schemaDef(t, reportSchema, "analyzer_run"), requiredJSONFields(reflect.TypeOf(AnalyzerRun{})))

	requireSchemaConst(t, findingSchema, "schema_version", FindingSchemaVersion)
	requireRequiredFields(t, "finding", findingSchema, requiredJSONFields(reflect.TypeOf(Finding{})))
	requireStringEnum(t, findingSchema, "severity", []string{
		string(SeverityCritical),
		string(SeverityWarning),
		string(SeverityInfo),
	})
}

func TestDocsAnalysisReportExampleMatchesGoContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "examples", "analysis-report-redacted.json"))
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode example as object: %v", err)
	}
	requireObjectKeys(t, "report example", raw, allJSONFields(reflect.TypeOf(AnalysisReport{})))
	requireRequiredKeys(t, "report example", raw, requiredJSONFields(reflect.TypeOf(AnalysisReport{})))

	requireObjectKeys(t, "report example config", objectValue(t, raw, "config"), allJSONFields(reflect.TypeOf(ReportConfig{})))
	requireRequiredKeys(t, "report example config", objectValue(t, raw, "config"), requiredJSONFields(reflect.TypeOf(ReportConfig{})))
	requireObjectKeys(t, "report example coverage", objectValue(t, raw, "coverage"), allJSONFields(reflect.TypeOf(CoverageSummary{})))
	requireRequiredKeys(t, "report example coverage", objectValue(t, raw, "coverage"), requiredJSONFields(reflect.TypeOf(CoverageSummary{})))

	for i, item := range arrayValue(t, raw, "findings") {
		finding, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("finding[%d] is %T, want object", i, item)
		}
		requireObjectKeys(t, "finding example", finding, allJSONFields(reflect.TypeOf(Finding{})))
		requireRequiredKeys(t, "finding example", finding, requiredJSONFields(reflect.TypeOf(Finding{})))
	}
	for i, item := range arrayValue(t, raw, "analyzer_runs") {
		run, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("analyzer_runs[%d] is %T, want object", i, item)
		}
		requireObjectKeys(t, "analyzer_run example", run, allJSONFields(reflect.TypeOf(AnalyzerRun{})))
		requireRequiredKeys(t, "analyzer_run example", run, requiredJSONFields(reflect.TypeOf(AnalyzerRun{})))
	}

	var report AnalysisReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode example as AnalysisReport: %v", err)
	}
	if report.SchemaVersion != ReportSchemaVersion {
		t.Fatalf("report schema_version = %q, want %q", report.SchemaVersion, ReportSchemaVersion)
	}
	for i, finding := range report.Findings {
		if finding.SchemaVersion != FindingSchemaVersion {
			t.Fatalf("finding[%d] schema_version = %q, want %q", i, finding.SchemaVersion, FindingSchemaVersion)
		}
	}
}

func readDocsSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return schema
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func requiredJSONFields(typ reflect.Type) []string {
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, opts, ok := jsonTag(field)
		if !ok || opts["omitempty"] {
			continue
		}
		fields = append(fields, name)
	}
	sort.Strings(fields)
	return fields
}

func allJSONFields(typ reflect.Type) []string {
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, ok := jsonTag(typ.Field(i))
		if ok {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}

func jsonTag(field reflect.StructField) (string, map[string]bool, bool) {
	if !field.IsExported() {
		return "", nil, false
	}
	tag := field.Tag.Get("json")
	if tag == "" || tag == "-" {
		return "", nil, false
	}
	parts := strings.Split(tag, ",")
	if parts[0] == "" {
		return "", nil, false
	}
	opts := make(map[string]bool, len(parts)-1)
	for _, opt := range parts[1:] {
		opts[opt] = true
	}
	return parts[0], opts, true
}

func schemaDef(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	defs := objectValue(t, schema, "$defs")
	return objectValue(t, defs, name)
}

func requireSchemaConst(t *testing.T, schema map[string]any, propertyName, want string) {
	t.Helper()
	properties := objectValue(t, schema, "properties")
	property := objectValue(t, properties, propertyName)
	if got, _ := property["const"].(string); got != want {
		t.Fatalf("%s const = %q, want %q", propertyName, got, want)
	}
}

func requireRequiredFields(t *testing.T, label string, schema map[string]any, want []string) {
	t.Helper()
	got := stringArrayValue(t, schema, "required")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s required fields = %v, want %v", label, got, want)
	}
}

func requireStringEnum(t *testing.T, schema map[string]any, propertyName string, want []string) {
	t.Helper()
	properties := objectValue(t, schema, "properties")
	property := objectValue(t, properties, propertyName)
	got := stringArrayValue(t, property, "enum")
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s enum = %v, want %v", propertyName, got, want)
	}
}

func requireObjectKeys(t *testing.T, label string, obj map[string]any, allowed []string) {
	t.Helper()
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key := range obj {
		if !allowedSet[key] {
			t.Fatalf("%s contains unexpected key %q (allowed: %v)", label, key, allowed)
		}
	}
}

func requireRequiredKeys(t *testing.T, label string, obj map[string]any, required []string) {
	t.Helper()
	for _, key := range required {
		if _, ok := obj[key]; !ok {
			t.Fatalf("%s is missing key %q", label, key)
		}
	}
}

func objectValue(t *testing.T, obj map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("missing object key %q", key)
	}
	value, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", key, raw)
	}
	return value
}

func arrayValue(t *testing.T, obj map[string]any, key string) []any {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("missing array key %q", key)
	}
	value, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s is %T, want array", key, raw)
	}
	return value
}

func stringArrayValue(t *testing.T, obj map[string]any, key string) []string {
	t.Helper()
	raw := arrayValue(t, obj, key)
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		value, ok := item.(string)
		if !ok {
			t.Fatalf("%s contains %T, want string", key, item)
		}
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}
