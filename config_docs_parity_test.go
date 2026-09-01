package main

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
)

const (
	configReferenceStart = "<!-- config-reference:start -->"
	configReferenceEnd   = "<!-- config-reference:end -->"
)

var (
	exampleYAMLKeyPattern = regexp.MustCompile(`^([ ]*)(-[ ]+)?([a-z][a-z0-9_]*):(?:[ \t]*(.*))?$`)
	configPathPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\[\])?(?:\.[a-z][a-z0-9_]*(?:\[\])?)*$`)
)

type configPathDiff struct {
	Missing    []string
	Stale      []string
	Duplicates []string
}

func TestConfigDocs_Parity(t *testing.T) {
	canonical, err := canonicalYAMLPaths(reflect.TypeOf(config.Config{}))
	if err != nil {
		t.Fatalf("derive canonical configuration paths: %v", err)
	}

	exampleData, err := os.ReadFile("config.yaml.example")
	if err != nil {
		t.Fatalf("read config.yaml.example: %v", err)
	}
	examplePaths, err := configPathsFromExample(string(exampleData))
	if err != nil {
		t.Fatalf("parse config.yaml.example: %v", err)
	}
	if diff := compareConfigPaths(canonical, examplePaths); !diff.empty() {
		t.Error(formatConfigPathDiff("config.yaml.example", diff))
	}

	requireInternalDocs(t)

	docsData, err := os.ReadFile("docs/configuration.md")
	if err != nil {
		t.Fatalf("read docs/configuration.md: %v", err)
	}
	docsPaths, err := configPathsFromReferenceTables(string(docsData))
	if err != nil {
		t.Fatalf("parse docs/configuration.md: %v", err)
	}
	if diff := compareConfigPaths(canonical, docsPaths); !diff.empty() {
		t.Error(formatConfigPathDiff("docs/configuration.md authoritative reference tables", diff))
	}
}

func canonicalYAMLPaths(root reflect.Type) ([]string, error) {
	paths := make(map[string]struct{})
	if err := walkYAMLPaths(dereferenceType(root), "", make(map[reflect.Type]bool), paths); err != nil {
		return nil, err
	}
	return sortedKeys(paths), nil
}

func walkYAMLPaths(current reflect.Type, prefix string, active map[reflect.Type]bool, paths map[string]struct{}) error {
	current = dereferenceType(current)
	if current.Kind() != reflect.Struct {
		return fmt.Errorf("root type %s is not a struct", current)
	}
	if active[current] {
		return fmt.Errorf("recursive configuration type %s encountered at %q", current, prefix)
	}
	active[current] = true
	defer delete(active, current)

	for i := 0; i < current.NumField(); i++ {
		field := current.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag, ok := field.Tag.Lookup("yaml")
		if !ok {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(tag, ",", 2)[0])
		if name == "" || name == "-" {
			continue
		}

		path := joinConfigPath(prefix, name)
		fieldType := dereferenceType(field.Type)
		switch fieldType.Kind() {
		case reflect.Struct:
			if err := walkYAMLPaths(fieldType, path, active, paths); err != nil {
				return err
			}
		case reflect.Slice, reflect.Array:
			elementType := dereferenceType(fieldType.Elem())
			if elementType.Kind() == reflect.Struct {
				if err := walkYAMLPaths(elementType, path+"[]", active, paths); err != nil {
					return err
				}
			} else {
				paths[path] = struct{}{}
			}
		case reflect.Map:
			// YAML map keys are dynamic rather than part of the stable config
			// schema, so maps remain leaf settings even when values are structs.
			paths[path] = struct{}{}
		default:
			paths[path] = struct{}{}
		}
	}
	return nil
}

func dereferenceType(value reflect.Type) reflect.Type {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	return value
}

func joinConfigPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

type yamlDeclaration struct {
	indent int
	path   string
}

func configPathsFromExample(text string) ([]string, error) {
	var stack []yamlDeclaration
	declarations := make(map[string]struct{})
	listContainers := make(map[string]struct{})

	for lineNumber, line := range strings.Split(text, "\n") {
		candidate := uncommentYAMLDeclaration(line)
		matches := exampleYAMLKeyPattern.FindStringSubmatch(candidate)
		if matches == nil {
			continue
		}

		indent := len(matches[1])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}

		if matches[2] != "" {
			if len(stack) == 0 {
				return nil, fmt.Errorf("line %d has a mapping list entry without a parent", lineNumber+1)
			}
			listContainers[stack[len(stack)-1].path] = struct{}{}
		}

		parent := ""
		if len(stack) > 0 {
			parent = stack[len(stack)-1].path
		}
		path := joinConfigPath(parent, matches[3])
		declarations[path] = struct{}{}
		if strings.TrimSpace(matches[4]) == "" {
			stack = append(stack, yamlDeclaration{indent: indent, path: path})
		}
	}

	normalized := make(map[string]struct{}, len(declarations))
	for path := range declarations {
		normalized[normalizeExamplePath(path, listContainers)] = struct{}{}
	}

	leaves := make(map[string]struct{})
	for path := range normalized {
		isContainer := false
		for candidate := range normalized {
			if strings.HasPrefix(candidate, path+".") {
				isContainer = true
				break
			}
		}
		if !isContainer {
			leaves[path] = struct{}{}
		}
	}
	return sortedKeys(leaves), nil
}

func uncommentYAMLDeclaration(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "#") {
		return line
	}
	leading := line[:len(line)-len(trimmed)]
	content := strings.TrimPrefix(trimmed, "#")
	content = strings.TrimPrefix(content, " ")
	return leading + content
}

func normalizeExamplePath(path string, listContainers map[string]struct{}) string {
	segments := strings.Split(path, ".")
	rawPrefix := ""
	for i, segment := range segments {
		rawPrefix = joinConfigPath(rawPrefix, segment)
		if _, ok := listContainers[rawPrefix]; ok {
			segments[i] += "[]"
		}
	}
	return strings.Join(segments, ".")
}

func configPathsFromReferenceTables(text string) ([]string, error) {
	insideReference := false
	foundStart := false
	foundEnd := false
	inTable := false
	var paths []string

	for lineNumber, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case configReferenceStart:
			if insideReference || foundStart {
				return nil, fmt.Errorf("line %d repeats %s", lineNumber+1, configReferenceStart)
			}
			insideReference = true
			foundStart = true
			continue
		case configReferenceEnd:
			if !insideReference {
				return nil, fmt.Errorf("line %d has %s without a start marker", lineNumber+1, configReferenceEnd)
			}
			insideReference = false
			foundEnd = true
			inTable = false
			continue
		}
		if !insideReference {
			continue
		}

		cells, ok := markdownTableCells(trimmed)
		if !ok {
			inTable = false
			continue
		}
		if cells[0] == "Configuration path" {
			inTable = true
			continue
		}
		if !inTable || isMarkdownSeparator(cells[0]) {
			continue
		}

		path := strings.TrimSpace(cells[0])
		if len(path) < 3 || path[0] != '`' || path[len(path)-1] != '`' {
			return nil, fmt.Errorf("line %d configuration path %q must be wrapped in backticks", lineNumber+1, path)
		}
		path = strings.Trim(path, "`")
		if !configPathPattern.MatchString(path) {
			return nil, fmt.Errorf("line %d has invalid fully qualified configuration path %q", lineNumber+1, path)
		}
		paths = append(paths, path)
	}

	if !foundStart || !foundEnd || insideReference {
		return nil, fmt.Errorf("authoritative reference must contain one matched %s / %s pair", configReferenceStart, configReferenceEnd)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("authoritative reference contains no Configuration path table rows")
	}
	return paths, nil
}

func markdownTableCells(line string) ([]string, bool) {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil, false
	}
	parts := strings.Split(line[1:len(line)-1], "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, len(parts) > 0
}

func isMarkdownSeparator(cell string) bool {
	cell = strings.Trim(cell, ":")
	return len(cell) >= 3 && strings.Trim(cell, "-") == ""
}

func compareConfigPaths(canonical, documented []string) configPathDiff {
	want := make(map[string]struct{}, len(canonical))
	for _, path := range canonical {
		want[path] = struct{}{}
	}
	counts := make(map[string]int, len(documented))
	for _, path := range documented {
		counts[path]++
	}

	var diff configPathDiff
	for path := range want {
		if counts[path] == 0 {
			diff.Missing = append(diff.Missing, path)
		}
	}
	for path, count := range counts {
		if _, ok := want[path]; !ok {
			diff.Stale = append(diff.Stale, path)
		}
		if count > 1 {
			diff.Duplicates = append(diff.Duplicates, path)
		}
	}
	sort.Strings(diff.Missing)
	sort.Strings(diff.Stale)
	sort.Strings(diff.Duplicates)
	return diff
}

func (diff configPathDiff) empty() bool {
	return len(diff.Missing) == 0 && len(diff.Stale) == 0 && len(diff.Duplicates) == 0
}

func formatConfigPathDiff(artifact string, diff configPathDiff) string {
	var details []string
	if len(diff.Missing) > 0 {
		details = append(details, "missing canonical paths: "+strings.Join(diff.Missing, ", "))
	}
	if len(diff.Stale) > 0 {
		details = append(details, "stale/non-canonical paths: "+strings.Join(diff.Stale, ", "))
	}
	if len(diff.Duplicates) > 0 {
		details = append(details, "duplicate paths (each reference row must appear exactly once): "+strings.Join(diff.Duplicates, ", "))
	}
	return artifact + " configuration parity failed:\n  " + strings.Join(details, "\n  ")
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type configPathFixtureNested struct {
	Enabled bool   `yaml:"enabled"`
	Hidden  string `yaml:"-"`
}

type configPathFixture struct {
	Nested      configPathFixtureNested            `yaml:"nested,omitempty"`
	Pointer     *configPathFixtureNested           `yaml:"pointer"`
	ScalarSlice []string                           `yaml:"scalar_slice"`
	StructSlice []*configPathFixtureNested         `yaml:"struct_slice"`
	Labels      map[string]configPathFixtureNested `yaml:"labels"`
	Ignored     string                             `yaml:"-"`
}

type recursiveConfigPathFixture struct {
	Child *recursiveConfigPathFixture `yaml:"child"`
}

func TestCanonicalYAMLPaths_HandlesConfigShapes(t *testing.T) {
	got, err := canonicalYAMLPaths(reflect.TypeOf(configPathFixture{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"labels",
		"nested.enabled",
		"pointer.enabled",
		"scalar_slice",
		"struct_slice[].enabled",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical paths = %v, want %v", got, want)
	}
}

func TestCanonicalYAMLPaths_RejectsRecursiveTypes(t *testing.T) {
	_, err := canonicalYAMLPaths(reflect.TypeOf(recursiveConfigPathFixture{}))
	if err == nil || !strings.Contains(err.Error(), "recursive configuration type") {
		t.Fatalf("canonicalYAMLPaths recursive error = %v", err)
	}
}

func TestConfigPathsFromExample_HandlesCommentsListsAndRepeatedParents(t *testing.T) {
	fixture := `
root:
  enabled: true
# root:
#   expert: 3
items:
  - name: first
# items:
#   - detail: expert
values:
  - one
# This prose: is not a declaration.
#   # nested_comment: is prose, too
# - "not: a key"
`
	got, err := configPathsFromExample(fixture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"items[].detail", "items[].name", "root.enabled", "root.expert", "values"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("example paths = %v, want %v", got, want)
	}
}

func TestConfigPathsFromReferenceTables_PreservesDuplicateRows(t *testing.T) {
	fixture := configReferenceStart + `
| Configuration path | Default | Description |
|---|---|---|
| ` + "`parent.enabled`" + ` | false | First |
| ` + "`parent.enabled`" + ` | false | Duplicate |
| ` + "`other.enabled`" + ` | false | Same leaf, different parent |
` + configReferenceEnd
	paths, err := configPathsFromReferenceTables(fixture)
	if err != nil {
		t.Fatal(err)
	}
	diff := compareConfigPaths([]string{"other.enabled", "parent.enabled"}, paths)
	if !reflect.DeepEqual(diff.Duplicates, []string{"parent.enabled"}) {
		t.Fatalf("duplicate paths = %v, want [parent.enabled]", diff.Duplicates)
	}
}

func TestCompareConfigPaths_ReportsQualifiedMissingAndStalePaths(t *testing.T) {
	want := []string{"alpha.enabled", "alpha.timeout_s", "beta.enabled"}
	got := []string{"alpha.enabled", "beta.timeout_s", "removed.enabled"}
	diff := compareConfigPaths(want, got)
	if !reflect.DeepEqual(diff.Missing, []string{"alpha.timeout_s", "beta.enabled"}) {
		t.Errorf("missing paths = %v", diff.Missing)
	}
	if !reflect.DeepEqual(diff.Stale, []string{"beta.timeout_s", "removed.enabled"}) {
		t.Errorf("stale paths = %v", diff.Stale)
	}
}
