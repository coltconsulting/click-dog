# Contributing to Click-Dog

Click-Dog is licensed under Apache 2.0 and accepts contributions under the [Developer Certificate of Origin](https://developercertificate.org/) (DCO).

## Developer Certificate of Origin

The DCO is a lightweight way for contributors to certify that they wrote, or otherwise have the right to submit, the code they are contributing. The full text is at <https://developercertificate.org/>.

You agree to the DCO by adding a `Signed-off-by` trailer to every commit:

```bash
git commit -s -m "commit message"
```

`-s` appends, using the `user.name` and `user.email` from `git config`:

```
Signed-off-by: Jane Doe <jane@example.com>
```

Use a real name and a real email. Anonymous and pseudonymous sign-offs are not accepted.

To add a missing sign-off to an earlier commit, amend or rebase:

```bash
# Last commit only
git commit --amend -s --no-edit

# Multiple commits
git rebase --signoff <base-branch>
```

A DCO check runs on every push to a pull request and fails if any commit lacks the `Signed-off-by` trailer. Add it with `--amend` or `git rebase --signoff` and push again.

## Pull Requests

- One logical change per PR. Keep diffs reviewable.
- Tests for new behavior. Update existing tests when behavior changes.
- `make check` must pass locally before opening the PR. It runs formatting, unit
  tests, the installer/release shell suites, linting, and a build. It needs the
  pinned tools, so run `make dev-setup` once first: the gates refuse a linter
  or other tool from the `PATH` that is not the pinned version, and
  `make doctor` shows which ones differ. `make check-fast` (formatting + unit
  tests) is the quick loop during development.
- For substantial changes, open an issue first to align on direction.

## How a PR Lands

Click-Dog is developed in a private tree and published to this repository
release by release: `master` here only advances when a release ships.
Contributor PRs are therefore imported rather than merged:

1. Open the PR against `master` here as usual; review happens on the PR.
2. Once accepted, a maintainer imports the commits into the development
   tree with `git am`, preserving the original commit author.
3. The change ships in the next release, and a maintainer closes the PR
   with a comment naming the release version.

The PR shows as "closed" rather than "merged." This is expected. The
development history preserves authorship, and the release's changelog entry
credits each contribution.

## Development Workflow

Build and test with the Makefile: `make build`, `make test`, `make check`,
`make fmt` (see `make help` for the full list). Integration tests
(`make test-integration`) need Docker.

## Documentation

The documentation at [click-dog.com](https://click-dog.com/) is maintained in
the development tree and is not part of this repository. Report a docs
problem as an issue.

## License

By submitting a contribution, you agree it is licensed under the [Apache 2.0 License](LICENSE) and that you have certified the DCO on each commit.
