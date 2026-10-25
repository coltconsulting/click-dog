package leader

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestKeeperClient_LastZxidAccessesAreAtomic pins the patched go-zookeeper copy
// in third_party/go-zookeeper. v1.0.4 races on Conn.lastZxid across a
// reconnect, but the race fires only when Keeper drops with a request in
// flight, so the -race integration suite cannot catch a regression reliably.
// This fails deterministically if the go.mod replace is dropped or conn.go is
// re-copied unpatched. Delete it with the replace once upstream ships a fix.
func TestKeeperClient_LastZxidAccessesAreAtomic(t *testing.T) {
	// Ask the go command which source tree this build compiles, rather than
	// reading third_party directly: a removed replace resolves to the
	// unpatched module cache copy, and that is the case to catch.
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/go-zookeeper/zk").Output()
	if err != nil {
		t.Fatalf("go list github.com/go-zookeeper/zk: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	src, err := os.ReadFile(filepath.Join(dir, "conn.go"))
	if err != nil {
		t.Fatalf("read the zk client source: %v", err)
	}

	all := len(regexp.MustCompile(`\bc\.lastZxid\b`).FindAll(src, -1))
	atomic := len(regexp.MustCompile(`\batomic\.(Load|Store)Int64\(&c\.lastZxid\b`).FindAll(src, -1))
	// v1.0.4 has four accesses. Fewer means upstream moved the field, and
	// this check would pass without checking anything.
	if all < 4 {
		t.Fatalf("found %d c.lastZxid accesses in %s, want at least 4; re-check the patch against this client version", all, dir)
	}
	if atomic != all {
		t.Fatalf("%d of %d c.lastZxid accesses in %s are not atomic; the build is not using the patched client (see third_party/go-zookeeper/README.md)", all-atomic, all, dir)
	}
}
