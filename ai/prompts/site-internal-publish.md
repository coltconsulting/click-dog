# site-internal-publish — RFC / worker prompt

- **Initiative:** decouple click-dog.com from GitHub Pages and from the binary
  release lifecycle; publish it from this private repo to an external static
  host with its own versioning and test/prod channels.
- **Status:** active. Tracker: `.beads/issues.jsonl` (beads `cd-1`…`cd-6`).
- **Owner:** Mayor session (orchestration only). Workers implement one bead each.
- **Date:** 2026-07-22.

## 1. Goal

Publish click-dog.com entirely from `coltconsulting/click-dog-internal`,
independent of the click-dog binary release/artifact lifecycle, with its own
versioning (`site-vN`) and two channels (staging + prod).

Hard rules (violating any of these fails the initiative):

1. **Never leak internal→public.** The site (docs tree, landing, site tooling)
   is NOT open source and must never reach `coltconsulting/click-dog`. The
   public repo's `master` advances only via `scripts/publish-to-public.sh`
   (`make update-public PUSH=1`).
2. **No vendor Git app on this repo.** The host must not be connected to the
   private repo via its Git integration. Deploys are driven by GitHub Actions
   in this repo using the host CLI + an API token stored as an Actions secret.
3. **Pre-GA infra: correctness + safety over speed.** No external users depend
   on staging.
4. Never link a gitignored file (`ai/decisions/`, `ai/findings/`,
   `ai/extended-context/`) from a committed doc.

## 2. Current state (verified 2026-07-22, master `2c9bb85`)

Publishing today is GitHub Pages, coupled to release tags:

- `.github/workflows/docs.yml` — `build` job (`mkdocs build --strict`) runs on
  docs-path PRs and master pushes; `deploy` job (docs.yml:51-67) runs
  `mkdocs gh-deploy --force` on any `v*` tag push or `workflow_dispatch`,
  writing the `gh-pages` branch. `permissions: contents: write` (docs.yml:31-32).
  Tag pushes bypass `paths:` filters (GitHub behavior), so every release
  republishes the site.
- GitHub's generated `pages-build-deployment` serves `gh-pages` at
  click-dog.com; the custom domain comes from `docs/CNAME`.
- `mkdocs.yml` — `custom_dir: overrides` (line 11): `overrides/home.html` is
  the standalone marketing landing, `overrides/main.html` injects brand
  fonts/favicon/social-card meta. `site_url: https://click-dog.com` (line 3).
  `exclude_docs: /development/**` (lines 80-81) keeps contributor docs out of
  the built site. Build deps: `requirements-docs.txt`; output dir `site/`
  (gitignored, .gitignore:34).
- Social preview: `docs/assets/social-card.svg` (source) +
  `docs/assets/social-card.png` (generated via `make docs-social-card`).
- `.gitattributes:11-20` — export-ignore for `docs/development/**`,
  `openspec/**`, `scripts/publish-to-public.sh`, `scripts/import-public-pr.sh`.
  Everything else — including `docs/`, `overrides/`, `mkdocs.yml`,
  `requirements-docs.txt`, `.github/workflows/docs.yml` — currently SHIPS in
  the public archive.
- `scripts/publish-to-public.sh` — GA-tag regex `^v[0-9]+\.[0-9]+\.[0-9]+$`
  (line 62); latest-GA selection one-liner (line 66); materializes
  `git archive <tag>` into a public checkout, then a leak backstop
  (lines 125-132) aborts if staged paths match an internal-only list.
  Invoked by `make update-public` (Makefile:237-243); dry-run by default.
- Release tags: CalVer `vYY.MM.idx`; prereleases carry `-test/-alpha/-beta`.
  GA tags on the remote today: `v26.07.1`, `v26.07.2`, `v26.07.3` (latest GA
  = `v26.07.3`); prerelease example `v26.06.6-beta`.
- Docs describing the old model (all must change in B3):
  `docs/development/docs-publishing.md` (entire page),
  `docs/development/releasing.md:81-86` and `:135-138`,
  `docs/development/specs/open-source-clean-slate-migration.md:39-58`
  (boundary table: "Only user-facing docs (the mkdocs site) ship") and
  `:161-174` ("Docs site / GitHub Pages" carryover bullet), plus the two
  factual mentions in `CLAUDE.md` (CI-workflows line and the
  "Docs site (click-dog.com)" bullet under Git Workflow).

## 3. Target design

### 3.1 Host and projects

**Cloudflare Pages** (charter default; Netlify is the documented variant —
operator ratifies, bead `cd-6`). Two **direct-upload** projects, vendor Git
integration OFF, production branch set to `main` on both:

| Project | Serves | URL |
|---|---|---|
| `click-dog` | prod | `click-dog.pages.dev` + custom domain `click-dog.com` |
| `click-dog-staging` | staging | `click-dog-staging.pages.dev` |

Deploy command (no third-party marketplace action; plain npm):

    npx --yes wrangler@4 pages deploy site \
      --project-name=<project> --branch=main --commit-dirty=true

`--branch=main` makes every upload a *production* deployment of its own
project (that is how the staging project gets a stable URL). Secrets (Actions
secrets, wired by the operator): `CLOUDFLARE_API_TOKEN` (scoped to
Cloudflare Pages: Edit), `CLOUDFLARE_ACCOUNT_ID`.

Netlify variant (kept as commented-out steps in `site.yml`, not a second live
path): `npx --yes netlify-cli deploy --dir=site --prod` with
`NETLIFY_AUTH_TOKEN` + `NETLIFY_SITE_ID` / `NETLIFY_STAGING_SITE_ID`.

### 3.2 Channels and triggers (`.github/workflows/site.yml`)

| Trigger | Channel | Content |
|---|---|---|
| push to `master` touching site paths | staging | master HEAD, unpinned |
| tag `site-v*` (e.g. `site-v1`) | prod | pinned (see 3.3) |
| GA tag `v\d+\.\d+\.\d+` (no `-` suffix) | prod (docs refresh) | pinned (see 3.3) |
| `workflow_dispatch` (input `channel`: staging\|prod) | chosen | staging: run ref; prod: pinned |

Site paths for the staging trigger: `docs/**`, `overrides/**`, `mkdocs.yml`,
`requirements-docs.txt`, `.github/workflows/site.yml`.

Mechanics that are easy to get wrong (verified against GitHub docs/behavior):

- Tag patterns are globs, not regexes: `v*` also matches `v26.07.4-test`. The
  prod job therefore guards with
  `startsWith(github.ref, 'refs/tags/site-v') || !contains(github.ref_name, '-')`
  (prereleases always carry `-`), plus a belt-and-suspenders bash regex step
  (`^site-v[0-9]+$` / `^v[0-9]+\.[0-9]+\.[0-9]+$`) that hard-fails on
  anything exotic (e.g. a malformed two-part `v26.07`).
- `paths:` filters apply only to branch pushes, never tag pushes — already the
  documented behavior the old docs.yml relied on.
- The `secrets` context is not reliable in job-level `if:`; gate deploys with a
  step that maps secrets into env and emits a `wired=true/false` output. When
  secrets are unwired the build still validates and the deploy step SKIPS with
  a `::warning::` annotation ("deploy SKIPPED — secrets not configured") so
  pre-cutover master pushes stay green but visibly undeployed.
- Concurrency: prod job `group: site-prod, cancel-in-progress: false`;
  staging job `group: site-staging, cancel-in-progress: true`.
- `permissions: contents: read` — this workflow writes nothing to the repo.
- Prod checkout needs `fetch-depth: 0` + `fetch-tags: true` (latest-GA
  computation needs the full tag list).

### 3.3 Prod content assembly ("pin docs, keep landing")

PRODUCT DOCS ship pinned to the latest GA tag; MARKETING LANDING and build
tooling ship from the triggering ref. Precisely, in the prod job:

1. `BASE` = the commit the workflow ran on (`github.sha`: the `site-vN` tag's
   commit, the GA tag's commit, or master for `workflow_dispatch` prod). Every
   such ref is a master commit, which is how "landing overlaid from master"
   stays true while builds remain deterministic and auditable.
2. `GA_TAG` = same selection rule as `scripts/publish-to-public.sh:66`:
   `git tag -l 'v*.*.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1`.
   Using "latest GA" rather than "the pushed tag" makes re-runs idempotent and
   means re-pushing an old tag can never downgrade prod docs.
3. Swap deletion-safely (`git checkout <tree> -- <path>` does NOT delete files
   absent in the source tree, so remove first):
   `rm -rf docs mkdocs.yml && git checkout "$GA_TAG" -- docs mkdocs.yml`
4. Re-overlay the landing-owned files that live under `docs/` from BASE:
   `git checkout "$BASE" -- docs/assets/social-card.svg docs/assets/social-card.png`
   (`overrides/**` and `requirements-docs.txt` were never swapped, so they are
   already BASE's.)
5. `pip install -r requirements-docs.txt && mkdocs build --strict` — the
   strict build is the compatibility gate for "tag's mkdocs.yml + master's
   overrides" drift; a prod build that fails strict does not deploy.
6. Deploy `site/` to the prod project.

Pre-GA bootstrap: if no GA tag exists (cannot happen today — v26.07.x exist —
but defensive), docs ship from BASE unpinned with a loud `::warning::`. This
preserves the ability to launch the site before a first GA in a fresh-history
world and self-heals the moment a GA tag lands.

Staging assembly is trivial: checkout the push ref, strict build, deploy to
the staging project.

Reads the release TAG only — never GoReleaser output, releases, or artifacts.

### 3.4 Site versioning

`site-vN` (`site-v1`, `site-v2`, …): annotated tags on master commits,
monotonically increasing integer, fully independent of `vYY.MM.idx`. Cutting
`site-vN` is the "ship prod" action for landing/site changes; GA release tags
refresh prod docs as a side effect. Version links inside the docs already use
`github.com/coltconsulting/click-dog/releases/latest/…` redirects, so no
build-time version injection is needed (bead `cd-5` is the backlogged
fallback if a page ever needs a concrete version string).

### 3.5 docs.yml reduced to a build-only gate

`docs.yml` keeps exactly one purpose: `mkdocs build --strict` as a PR
validation gate (`pull_request` on docs paths + `workflow_dispatch` for
on-demand validation). Remove: the `v*` tags trigger, the `push: master`
trigger (staging builds in site.yml cover master), the entire `deploy` job,
and drop `permissions` to `contents: read`. Rewrite the header comment; it
must state that all deploys live in site.yml and this workflow publishes
nothing.

## 4. Interrogation log (ambiguities found → resolved)

1. **Spec's B1 list omitted `site.yml` itself.** The new workflow is site
   tooling and must also be export-ignored, or the public repo ships a
   workflow referencing secrets/projects it must not know about. Added to B1.
2. **Leak-backstop deletion bug (would break the first post-B1 release).**
   The public repo currently tracks `docs/`, `overrides/`, `mkdocs.yml`. The
   first publish after B1 stages their *deletions* in the public checkout;
   `scripts/publish-to-public.sh:128` matches staged names with no
   `--diff-filter`, so the backstop would abort on the very deletions that
   are the point. B1 must add `--diff-filter=d` (ignore deletions — flag only
   files present in the new tree) while extending the path list.
3. **"Overlay from master" vs determinism.** Resolved in 3.3: BASE = trigger
   ref, which is always a master commit; docs+mkdocs.yml swapped to latest GA;
   only `docs/assets/social-card.*` needs re-overlay. `workflow_dispatch`
   prod runs literally from master.
4. **GA-tag prod runs use "latest GA", not "the tag that fired"** —
   idempotent re-runs; an old-tag re-push cannot downgrade docs (3.3.2).
5. **Tag glob vs prerelease tags** — `v*` glob matches `-test/-alpha/-beta`;
   guarded by `!contains(ref_name, '-')` + regex step (3.2).
6. **Secrets in `if:`** — gate via env-mapped step output; unwired secrets
   skip-with-warning instead of failing or silently "succeeding" (3.2).
7. **`git checkout <tree> -- dir` doesn't delete** — deletion-safe swap via
   `rm -rf` first (3.3.3).
8. **No GA tag edge** — defensive unpinned fallback with warning (3.3).
9. **`docs/CNAME`** — GitHub-Pages-only mechanism; irrelevant to Cloudflare,
   harmless in the built output, covered by the `docs/**` export-ignore. Keep
   the file until the old Pages setup is decommissioned (operator, `cd-6`),
   then delete with the decommission.
10. **Shallow CI/agent clones** — tag math requires `fetch-depth: 0` +
    `fetch-tags: true` in site.yml; local verification needs an explicit
    `git fetch origin tag v26.07.3` in shallow checkouts.
11. **Public repo consequence** — after B1, the next `make update-public`
    dry-run legitimately shows mass deletions of `docs/**`/`overrides/`/
    `mkdocs.yml` from the public tree. That is the intended outcome, not a
    leak; noted here so the release operator is not surprised.
12. **Old "merge ≠ publish" gotcha inverts.** After B2, a master push touching
    site paths DOES deploy (staging). Prod remains tag/dispatch-gated. B3 must
    rewrite that section of docs-publishing.md, not merely trim it.

## 5. Beads

Full per-bead specs (what's missing · where · change · done · tests ·
do-not-touch) live in `.beads/issues.jsonl`:

- `cd-1` epic — this initiative.
- `cd-2` / B1 (P1, SOLO) — remove the site from the public export
  (.gitattributes + publish-to-public.sh backstop, incl. `--diff-filter=d`).
- `cd-3` / B2 (P1, SOLO) — add `.github/workflows/site.yml`; reduce docs.yml
  to a build-only gate.
- `cd-4` / B3 (P2, SOLO) — rewrite docs-publishing.md; update releasing.md,
  the clean-slate spec carryover, and CLAUDE.md's two factual mentions;
  document `site-vN`.
- `cd-5` / B4 (P3, backlog) — build-time version injection fallback; do not
  implement now.
- `cd-6` — operator-only / do-not-dispatch: host ratification, Pages
  projects, token secrets, domain cutover, GitHub Pages decommission.

Global do-not-touch for every worker: `release.yml`, `ci.yml`, `claude.yml`,
`claude-code-review.yml`, `degradation.yml`, Makefile release targets,
`.goreleaser*`, the existing export-ignore rules (.gitattributes:11-20), any
`ai/` or `.beads/` content, and **never create or push a remote tag**.

## 6. Cutover sequence (operator, after B1–B3 merge)

1. Ratify host (default Cloudflare Pages). Create `click-dog` +
   `click-dog-staging` as direct-upload projects, Git integration OFF,
   production branch `main`.
2. Add `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID` Actions secrets.
3. Verify staging: push (or dispatch) a site-path change to master; confirm
   `click-dog-staging.pages.dev`.
4. Cut `site-v1`; confirm prod content on `click-dog.pages.dev` (docs =
   v26.07.3 content, landing = master).
5. Move click-dog.com: add custom domain to the prod project, point DNS at
   Cloudflare, remove the custom domain from the internal repo's Pages
   settings. Verify TLS + content.
6. Decommission old Pages LAST: disable Pages on this repo, then delete the
   `gh-pages` branch and `docs/CNAME`. Rollback before this step is trivial
   (gh-pages branch still serves if DNS is pointed back).

## 7. Done when

- `git archive master | tar -t | grep -E '^(overrides/|mkdocs\.yml|docs/)'`
  prints nothing (plus `requirements-docs.txt` and the two site workflows).
- master→staging, `site-vN`→prod, and GA-tag→docs-refresh all work once the
  operator wires secrets + projects.
- `make update-public` dry-run shows no site paths entering the public tree.
- All docs listed in §2 describe the new model; no doc links a gitignored
  `ai/` path.
