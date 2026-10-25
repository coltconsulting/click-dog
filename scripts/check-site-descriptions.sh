#!/usr/bin/env bash
#
# check-site-descriptions.sh — every built page has a snippet-length,
# non-empty <meta name="description">, and no two docs pages share one.
#
# Google shows roughly 155 characters of a description; a page whose
# description is cut mid-list gets a worse snippet than a page with none.
# The 404 page inherits site_description, so it is exempt from uniqueness.
#
# Usage: check-site-descriptions.sh [SITE_DIR]   (default: target)

set -euo pipefail
cd "$(dirname "$0")/.."

SITE="${1:-target}"
[ -d "$SITE" ] || { echo "FAIL: $SITE does not exist; run mkdocs build first" >&2; exit 1; }

python3 - "$SITE" <<'PY'
import html, re, sys
from pathlib import Path

LIMIT = 155
site = Path(sys.argv[1])
pages = sorted(site.rglob("index.html")) + [site / "404.html"]
fails, seen = [], {}
for p in pages:
    rel = p.relative_to(site)
    m = re.search(r'<meta name="description" content="([^"]*)"', p.read_text(encoding="utf-8", errors="replace"))
    if not m or not m.group(1).strip():
        fails.append(f"{rel}: no description"); continue
    desc = html.unescape(m.group(1))
    if len(desc) > LIMIT:
        fails.append(f"{rel}: description is {len(desc)} characters (limit {LIMIT})")
    if p.name != "404.html":
        if desc in seen:
            fails.append(f"{rel}: description duplicates {seen[desc]}")
        seen[desc] = str(rel)
for f in fails:
    print(f"FAIL: {f}", file=sys.stderr)
if fails:
    sys.exit(1)
print(f"site descriptions OK ({len(pages)} pages, all ≤ {LIMIT} characters, docs pages distinct)")
PY
