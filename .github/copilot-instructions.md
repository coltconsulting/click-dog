# Copilot Instructions

Read `AGENTS.md`, `CLAUDE.md`, and
`docs/development/domain-language.md` before suggesting broad changes.

Keep AI-related suggestions downstream of deterministic Query Analysis JSON.
Do not introduce MCP, LLM calls, automatic query rewrites, automatic `EXPLAIN`,
or hot-path analysis behavior unless the issue explicitly asks for it.

Prefer existing Go style, package boundaries, and test patterns. User-facing
terminology should match `docs/development/domain-language.md`.
