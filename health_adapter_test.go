package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/health"
	"github.com/coltconsulting/click-dog/internal/metrics"
)

// TestSnapshotStructsInSync guards against silent data-drop at the
// metrics↔health boundary. The adapter (metricsCycleSource.Snapshot)
// does a manual field-by-field copy between metrics.Snapshot and
// health.Snapshot — a field added to one side but not the other would
// survive compile and unit tests yet silently arrive as zero in /status.
//
// Checks BOTH name parity and type parity. Name-only parity would miss a
// type change (e.g. DurationMs int64 → DurationMs float64 on one side):
// the adapter's `Field: s.Field` copy would silently truncate or fail at
// compile time depending on the types involved. A type-parity check makes
// that misalignment surface in this test instead.
//
// Note on nested types: CycleSnapshot itself has matching field names on
// both sides (Exported, Filtered, ...) but its Go type differs
// (metrics.CycleSnapshot vs health.CycleSnapshot). The CycleSnapshot pair
// is checked separately below, so comparing the `LastCycle` field's type
// directly would always fail. We compare field types by their STRING
// representation and strip the package qualifier so metrics.CycleSnapshot
// and health.CycleSnapshot compare equal as "CycleSnapshot".
func TestSnapshotStructsInSync(t *testing.T) {
	pairs := []struct {
		name                    string
		metricsType, healthType reflect.Type
	}{
		{
			"Snapshot",
			reflect.TypeOf(metrics.Snapshot{}),
			reflect.TypeOf(health.Snapshot{}),
		},
		{
			"CycleSnapshot",
			reflect.TypeOf(metrics.CycleSnapshot{}),
			reflect.TypeOf(health.CycleSnapshot{}),
		},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			m := exportedFieldPairs(p.metricsType)
			h := exportedFieldPairs(p.healthType)
			if !reflect.DeepEqual(m, h) {
				t.Errorf("metrics.%s fields %v do not match health.%s fields %v\n"+
					"health_adapter.go must be updated when either side changes",
					p.name, m, p.name, h)
			}
		})
	}
}

// TestMetricsCycleSource_ValuesPropagate spot-checks that a populated
// metrics.Snapshot actually arrives on the health side with the same
// values. Complements TestSnapshotStructsInSync (which catches shape
// drift but not bogus copy logic).
func TestMetricsCycleSource_ValuesPropagate(t *testing.T) {
	m := metrics.NewMetrics()
	m.SetCircuitBreakerState("open")
	m.SetBackoffInterval(42 * time.Second)
	m.RecordCycle(11, 7, 3, 99, errForTest("boom"))

	got := metricsCycleSource{m: m}.Snapshot()

	if got.CircuitBreakerState != "open" {
		t.Errorf("CircuitBreakerState = %q, want open", got.CircuitBreakerState)
	}
	if got.BackoffIntervalSecs != 42 {
		t.Errorf("BackoffIntervalSecs = %v, want 42", got.BackoffIntervalSecs)
	}
	if !got.HaveLastCycle {
		t.Fatal("HaveLastCycle = false after RecordCycle")
	}
	if got.LastCycle.Exported != 11 || got.LastCycle.Filtered != 7 ||
		got.LastCycle.Duplicates != 3 || got.LastCycle.DurationMs != 99 {
		t.Errorf("LastCycle counts mismatch: %+v", got.LastCycle)
	}
	if got.LastCycle.Err != "boom" {
		t.Errorf("LastCycle.Err = %q, want boom", got.LastCycle.Err)
	}
	if got.LastCycle.Skipped {
		t.Error("LastCycle.Skipped = true after RecordCycle (should be false)")
	}

	// Separately: skipped path must also survive the adapter.
	m2 := metrics.NewMetrics()
	m2.RecordSkippedCycle(5)
	skipped := metricsCycleSource{m: m2}.Snapshot()
	if !skipped.LastCycle.Skipped {
		t.Error("Skipped=true did not survive the adapter")
	}
}

// exportedFieldPairs returns a stable "Name Type" slice for reflection
// equality across the two packages. We strip the package qualifier from
// type names so metrics.CycleSnapshot and health.CycleSnapshot compare
// as "CycleSnapshot" — the nested-type pairing is validated by its own
// entry in TestSnapshotStructsInSync.
func exportedFieldPairs(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		out = append(out, f.Name+" "+stripPkgQualifier(f.Type.String()))
	}
	return out
}

// stripPkgQualifier turns "metrics.CycleSnapshot" into "CycleSnapshot"
// while leaving primitive type names ("int64", "string", "time.Time")
// intact. time.Time is fine because it's in the same package on both
// sides; only our own internal/<pkg> qualifiers need normalizing.
func stripPkgQualifier(typeName string) string {
	for _, prefix := range []string{"metrics.", "health."} {
		if strings.HasPrefix(typeName, prefix) {
			return typeName[len(prefix):]
		}
	}
	return typeName
}

type errForTest string

func (e errForTest) Error() string { return string(e) }

func TestIsCrossWildcard(t *testing.T) {
	// The v4↔v6 wildcard cross is the pair where OS behavior diverges
	// (Linux default binds one socket, macOS/BSD/bindv6only=1 bind two).
	// Go's short ":P" is a v6 wildcard, so ":P" vs "[::]:P" is NOT a
	// cross but ":P" vs "0.0.0.0:P" IS.
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"both v4", "0.0.0.0:9090", "0.0.0.0:9090", false},
		{"both v6 explicit", "[::]:9090", "[::]:9090", false},
		{"both short (v6)", ":9090", ":9090", false},
		{"v6 explicit vs short", "[::]:9090", ":9090", false},
		{"v4 vs v6 explicit", "0.0.0.0:9090", "[::]:9090", true},
		{"v4 vs short (implicit v6)", "0.0.0.0:9090", ":9090", true},
		{"short vs v4", ":9090", "0.0.0.0:9090", true},
		{"explicit host", "127.0.0.1:9090", "0.0.0.0:9090", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCrossWildcard(c.a, c.b); got != c.want {
				t.Errorf("isCrossWildcard(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestSameListenAddress(t *testing.T) {
	// Covers the listener-sharing normalization: addresses that bind to the
	// same socket compare equal even when written differently. Operators
	// shouldn't get two listeners silently just because one side wrote
	// "0.0.0.0:9090" and the other wrote ":9090".
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical wildcard", ":9090", ":9090", true},
		{"identical explicit", "127.0.0.1:9090", "127.0.0.1:9090", true},
		{"v4 wildcard vs short", "0.0.0.0:9090", ":9090", true},
		{"short vs v4 wildcard", ":9090", "0.0.0.0:9090", true},
		{"v6 wildcard vs short", "[::]:9090", ":9090", true},
		{"different ports", ":9090", ":9091", false},
		{"wildcard vs localhost", ":9090", "127.0.0.1:9090", false},
		// This case asserts the canonicalisation treats the v4 and v6
		// wildcards as equivalent. It passes on every OS because
		// canonicalListenAddr is pure string manipulation — but the
		// real-world binding semantics only match on Linux with the
		// default IPV6_V6ONLY=0. On macOS/BSD the two forms bind
		// different sockets; see canonicalListenAddr's comment for the
		// full caveat. We're asserting our own normalization, not
		// cross-platform socket behavior.
		{"v6 vs v4 wildcard (Linux semantics)", "[::]:9090", "0.0.0.0:9090", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameListenAddress(c.a, c.b); got != c.want {
				t.Errorf("sameListenAddress(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestCanonicalListenAddr(t *testing.T) {
	// Slice (not map) so failures report in a stable order.
	cases := []struct {
		in, want string
	}{
		{":9090", ":9090"},
		{"0.0.0.0:9090", ":9090"},
		{"[::]:9090", ":9090"},
		{"127.0.0.1:9090", "127.0.0.1:9090"},
		{"host.example:443", "host.example:443"},
	}
	for _, c := range cases {
		if got := canonicalListenAddr(c.in); got != c.want {
			t.Errorf("canonicalListenAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
