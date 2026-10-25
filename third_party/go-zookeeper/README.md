# go-zookeeper/zk (patched)

`zk/` is `github.com/go-zookeeper/zk` v1.0.4, the latest release, with one
patch applied. The `replace` in the root `go.mod` builds the module from it.
`LICENSE` is the upstream BSD license. The upstream tests and examples are left
out.

## The patch

`recvLoop` writes `Conn.lastZxid` on every response with no synchronization.
After a reconnect, the connection loop's `sendSetWatches` reads it while the
new `recvLoop` is already decoding responses to requests that were queued
during the outage. That read is the race. `authenticate` also touches the
field on the connection loop, but only after `wg.Wait()` has joined the
previous `recvLoop`, so it is ordered. It is made atomic anyway so that every
access agrees. The race detector reports this whenever Keeper drops with a watch
registered and a request in flight. That is the normal state of a leader
election that is watching a znode, so the `-race` integration suite fails
`TestIntegration_LeaderElection_KeeperFlapFailsOpenAndRecovers` and
`..._ResignDuringOutageDoesNotRejoin`.

The patch makes the four accesses in `conn.go` atomic, as in upstream PR
[go-zookeeper/zk#35](https://github.com/go-zookeeper/zk/pull/35). That PR has
been open since 2020, and upstream `master` still has the race.

`lastzxid.patch` is that change as a diff against upstream v1.0.4. Every
other file in `zk/` is byte-identical to the release. The atomics remove the
data race, not an ordering gap upstream also has: a reconnect can still send a
`RelativeZxid` newer than changes made during the outage. The election only
watches for a predecessor's deletion, which fires regardless, so it is
unaffected.

## Regenerating or checking it

```sh
go mod download -json github.com/go-zookeeper/zk@v1.0.4   # prints "Dir"
```

Copy `conn.go`, `constants.go`, `create_mode.go`, `dnshostprovider.go`,
`flw.go`, `lock.go`, `structs.go`, `util.go`, `go.mod`, and `LICENSE` from that
directory into `zk/`, then `patch -p1 -d zk < lastzxid.patch`. `diff -r` of the
result against `zk/` must be empty.

## What the replace costs

- **Vulnerability scanning.** govulncheck looks advisories up by a module's
  replacement path, so it cannot match this copy. `scripts/lib/vulncheck.sh`,
  which `make check-vuln` and CI run, therefore also scans a copy of the
  module with the replace dropped. An advisory for upstream v1.0.4 still
  fails the gate.
- **`go install`.** Go refuses `go install
  github.com/coltconsulting/click-dog@<version>` for a module whose `go.mod`
  has a `replace`. Install from a release, `install.sh`, or a clone instead.
- **CI coverage.** Root `go test ./...` skips this nested module, and the
  upstream tests are left out. CI's integration path filter includes
  `third_party/*`, so a change here runs the Keeper integration tests.

## Removing it

Delete this directory and the `replace`. Do that once a go-zookeeper release
fixes the race, or once the election moves to a client without the race. The
test `TestKeeperClient_LastZxidAccessesAreAtomic` in `internal/leader` fails if
the build stops using this copy, so delete it at the same time.
