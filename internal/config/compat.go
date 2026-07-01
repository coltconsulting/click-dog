package config

import (
	"fmt"
	"strings"
)

// deprecation describes a deprecated config field and its replacement.
type deprecation struct {
	Field       string // dot-separated YAML path, e.g. "otel"
	Replacement string // what to use instead, e.g. "exporters.otel[]"
	Since       string // version when deprecated, e.g. "v25.04.0"
}

// deprecations is the registry of all deprecated config fields. Currently
// empty: the only former entry (top-level otel: → exporters.otel[]) was removed
// outright pre-GA rather than carried as a compat shim. The machinery is kept
// for the next deprecation.
var deprecations []deprecation

// checkDeprecations inspects a raw YAML map for deprecated fields and returns
// warning messages for each one found. Nested fields use dot-separated paths
// (e.g. "exporters.otel.tls.insecure"). Array indexing is out of scope —
// deprecate the parent key if an array element's schema changes.
func checkDeprecations(raw map[string]interface{}) []string {
	var warnings []string
	for _, d := range deprecations {
		if lookupPath(raw, d.Field) {
			warnings = append(warnings, fmt.Sprintf(
				"config field %q is deprecated since %s, use %q instead",
				d.Field, d.Since, d.Replacement,
			))
		}
	}
	return warnings
}

// lookupPath checks whether a dot-separated path exists in a nested map.
// Returns true if the terminal key is present (regardless of its value).
func lookupPath(m map[string]interface{}, path string) bool {
	parts := strings.Split(path, ".")
	current := m
	for i, part := range parts {
		val, ok := current[part]
		if !ok {
			return false
		}
		if i == len(parts)-1 {
			return true
		}
		nested, ok := val.(map[string]interface{})
		if !ok {
			return false
		}
		current = nested
	}
	// Unreachable: len(parts) >= 1 from Split, so the i == len(parts)-1
	// branch always fires on the last iteration.
	panic("unreachable")
}
