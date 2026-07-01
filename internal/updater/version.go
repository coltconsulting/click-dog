package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// CalVer represents a calendar version with three integer segments (YY.MM.idx)
// plus an optional prerelease identifier. Pre is empty for a stable release and
// holds the lower-cased text after the first hyphen for a prerelease — ANY
// "-<qualifier>" (alpha, beta, rc, test, …). This matches the release tooling's
// rule (Makefile / .goreleaser.yaml): a tag is a prerelease iff it carries a
// "-<qualifier>" suffix. A stable release sorts above any prerelease of the
// same numeric version.
type CalVer struct {
	Year  int
	Month int
	Index int
	Pre   string
}

// ParseVersion parses a calendar version string like "v25.04.1", "25.04.1", or
// "v26.05.1-alpha" into its segments. Any "-<qualifier>" suffix is kept
// (lower-cased) as the prerelease identifier — there is no allowlist, so a new
// qualifier like "-test" or "-rc2" is handled without code changes. Returns an
// error for a malformed numeric version.
func ParseVersion(s string) (CalVer, error) {
	s = strings.TrimPrefix(s, "v")

	var pre string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		pre = strings.ToLower(s[idx+1:])
		s = s[:idx]
	}

	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return CalVer{}, fmt.Errorf("version %q must have exactly 3 dot-separated segments", s)
	}

	year, err := strconv.Atoi(parts[0])
	if err != nil {
		return CalVer{}, fmt.Errorf("invalid year segment %q: %w", parts[0], err)
	}
	month, err := strconv.Atoi(parts[1])
	if err != nil {
		return CalVer{}, fmt.Errorf("invalid month segment %q: %w", parts[1], err)
	}
	index, err := strconv.Atoi(parts[2])
	if err != nil {
		return CalVer{}, fmt.Errorf("invalid index segment %q: %w", parts[2], err)
	}

	return CalVer{Year: year, Month: month, Index: index, Pre: pre}, nil
}

// String returns the version as "vYY.MM.idx" with a "-pre" suffix when present.
func (v CalVer) String() string {
	s := fmt.Sprintf("v%d.%02d.%d", v.Year, v.Month, v.Index)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Less returns true if v is strictly less than other. Numeric segments are
// compared first; on an exact numeric tie a prerelease sorts below the stable
// release and prereleases sort among themselves by semantic-version precedence.
func (v CalVer) Less(other CalVer) bool {
	if v.Year != other.Year {
		return v.Year < other.Year
	}
	if v.Month != other.Month {
		return v.Month < other.Month
	}
	if v.Index != other.Index {
		return v.Index < other.Index
	}
	return comparePrerelease(v.Pre, other.Pre) < 0
}

// Equal returns true if v and other are the same version, prerelease included.
func (v CalVer) Equal(other CalVer) bool {
	return v.Year == other.Year && v.Month == other.Month &&
		v.Index == other.Index && v.Pre == other.Pre
}

// comparePrerelease orders two prerelease identifier strings by semantic-version
// precedence. The empty string represents a stable release and sorts ABOVE any
// prerelease. Returns -1, 0, or 1.
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" { // a is stable, b is a prerelease
		return 1
	}
	if b == "" { // a is a prerelease, b is stable
		return -1
	}

	aIDs := strings.Split(a, ".")
	bIDs := strings.Split(b, ".")
	for i := 0; i < len(aIDs) && i < len(bIDs); i++ {
		if c := compareIdentifier(aIDs[i], bIDs[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers equal: the longer set has higher precedence.
	switch {
	case len(aIDs) < len(bIDs):
		return -1
	case len(aIDs) > len(bIDs):
		return 1
	default:
		return 0
	}
}

// compareIdentifier compares a single dot-separated prerelease identifier.
// Numeric identifiers compare numerically and sort below alphanumeric ones.
func compareIdentifier(a, b string) int {
	aNum, aErr := strconv.Atoi(a)
	bNum, bErr := strconv.Atoi(b)
	aIsNum, bIsNum := aErr == nil, bErr == nil
	switch {
	case aIsNum && bIsNum:
		switch {
		case aNum < bNum:
			return -1
		case aNum > bNum:
			return 1
		default:
			return 0
		}
	case aIsNum: // numeric sorts below alphanumeric
		return -1
	case bIsNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}
