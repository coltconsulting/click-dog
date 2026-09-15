# Contributing to Click-Dog

Thanks for your interest. Click-Dog is licensed under Apache 2.0 and accepts contributions under the [Developer Certificate of Origin](https://developercertificate.org/) (DCO).

## Developer Certificate of Origin

The DCO is a lightweight way for contributors to certify that they wrote, or otherwise have the right to submit, the code they are contributing. The full text is at <https://developercertificate.org/>.

You agree to the DCO by adding a `Signed-off-by` trailer to every commit:

```bash
git commit -s -m "your commit message"
```

`-s` appends, using your `user.name` and `user.email` from `git config`:

```
Signed-off-by: Jane Doe <jane@example.com>
```

Use a real name and a real email. Anonymous and pseudonymous sign-offs are not accepted.

If you forget on an earlier commit, amend or rebase to add it:

```bash
# Last commit only
git commit --amend -s --no-edit

# Multiple commits
git rebase --signoff <base-branch>
```

Maintainers check sign-off during review. There is no automated DCO bot yet. If you forget the trailer on a commit, you will be asked to add it with `--amend` or `git rebase --signoff` before merge.

## Pull Requests

- One logical change per PR. Keep diffs reviewable.
- Tests for new behavior. Update existing tests when behavior changes.
- `make check` must pass locally before opening the PR. It runs formatting, unit
  tests, the installer/release shell suites, linting, and a build. It needs the
  pinned tools, so run `make dev-setup` once first; `make check-fast`
  (formatting + unit tests) is the quick loop while you work.
- For substantial changes, open an issue first to align on direction.

## How Your PR Lands

Click-Dog is developed in a private tree and published to this repository
release by release: `master` here only advances when a release ships.
Contributor PRs are therefore imported rather than merged:

1. Open your PR against `master` here as usual; review happens on the PR.
2. Once accepted, a maintainer imports your commits into the development
   tree with `git am`, preserving you as the commit author.
3. Your change ships in the next release, and the PR is closed with a
   comment naming the release version.

Your PR will show as "closed" rather than "merged." This is expected. Your
authorship is preserved in the development history, and contributions are
credited in the release's changelog entry.

## Development Workflow

Build and test with the Makefile: `make build`, `make test`, `make check`,
`make fmt` (see `make help` for the full list). Integration tests
(`make test-integration`) need Docker.

## Documentation

The docs site (`docs/`) is built with MkDocs + Material and served at
click-dog.com. Preview it locally:

```bash
pip install -r requirements-docs.txt
mkdocs serve
```

`mkdocs build --strict` runs in CI on every docs PR. The site publishes on
release tags, not on merges to `master`, so docs changes go live with the
next release.

## License

By submitting a contribution, you agree it is licensed under the [Apache 2.0 License](LICENSE) and that you have certified the DCO on each commit.
