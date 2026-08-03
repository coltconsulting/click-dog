# ai/ — orchestration workspace

Layout (mayor-method):

- `ai/prompts/` — committed, durable worker/RFC prompts. Safe to link from
  other committed docs.
- `ai/decisions/` — one file per operator call. **Gitignored** (session-local).
- `ai/findings/` — audit output. **Gitignored**.
- `ai/extended-context/` — scratch context. **Gitignored**.

Rules:

- Never link a gitignored file (`decisions/`, `findings/`,
  `extended-context/`) from a committed doc. Mentioning the convention by
  name is fine; relying on the content is not.
- Work is tracked in the project's bead tracker: `.beads/issues.jsonl`
  (bd-format JSONL; see the `merge=beads` rule in `.gitattributes`). No
  parallel TODO lists.
