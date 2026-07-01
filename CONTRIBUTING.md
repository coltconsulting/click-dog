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

Maintainers check sign-off during review. There is no automated DCO bot yet — if you forget the trailer on a commit, you'll be asked to add it (via `--amend` or `git rebase --signoff`) before merge.

## Pull Requests

- One logical change per PR. Keep diffs reviewable.
- Tests for new behavior. Update existing tests when behavior changes.
- `make test`, `make lint`, and `make fmt-check` must pass locally before opening the PR.
- For substantial changes, open an issue first to align on direction.

## Development Workflow

See [docs/development/contributing.md](docs/development/contributing.md) for build, test, and release tooling.

## Documentation

The docs site (`docs/`) is built with MkDocs + Material and served at
click-dog.com. Preview it locally:

```bash
pip install -r requirements-docs.txt
mkdocs serve
```

`mkdocs build --strict` runs in CI on every docs PR. For how the site is
published — and why merging to `master` alone does not push it live — see
[docs/development/docs-publishing.md](docs/development/docs-publishing.md).

## License

By submitting a contribution, you agree it is licensed under the [Apache 2.0 License](LICENSE) and that you have certified the DCO on each commit.
