package config

import "strings"

// lookupPath checks whether a dot-separated path exists in a nested map.
// Returns true if the terminal key is present (regardless of its value).
// LoadConfig uses it on the raw YAML map to tell an omitted key from one set
// to its zero value.
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
