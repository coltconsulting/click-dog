#!/usr/bin/env bash
#
# check-site-twins.sh — every built page has a Markdown twin and no twin
# carries a dead relative link.
#
# docs/hooks/llms_site.py writes <page>/index.md one directory deeper than
# the page's source, rewriting relative links on the way. A wrong rewrite
# shows up as a 404 for the agents those twins exist for, so this resolves
# every relative link and image in every twin against the built tree.
#
# Usage: check-site-twins.sh [SITE_DIR]   (default: target; build it first
#                                          with `mkdocs build --strict`)
#
# exit 0  every page has a twin and every relative link resolves
# exit 1  a twin is missing, or a link points at nothing

set -euo pipefail
cd "$(dirname "$0")/.."

SITE="${1:-target}"
[ -d "$SITE" ] || { echo "FAIL: $SITE does not exist; run mkdocs build first" >&2; exit 1; }

python3 - "$SITE" <<'PY'
import posixpath, re, sys
from pathlib import Path

site = Path(sys.argv[1])
link = re.compile(r'!?\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)|(?:href|src)="([^"]+)"|^[ \t]{0,3}\[[^\]]+\]:[ \t]+<?([^\s>]+)', re.M)
code = re.compile(r"(^[ \t]*(`{3,}|~{3,})[^\n]*\n.*?^[ \t]*\2[ \t]*$)|(`+)[^`\n]*?\3", re.M | re.S)  # fences and spans
external = re.compile(r"^(?:[a-z][a-z0-9+.-]*:|/|#)", re.I)

pages = {p.relative_to(site).as_posix() for p in site.rglob("index.html") if p.name == "index.html"}
twins = {p.relative_to(site).as_posix() for p in site.rglob("index.md")}
missing = sorted(p[:-len("index.html")] + "index.md" for p in pages if p[:-len("index.html")] + "index.md" not in twins)

broken = []
checked = 0
for twin in sorted(twins):
    # Code blocks and spans are examples; the hook leaves them untouched too.
    text = code.sub("", (site / twin).read_text(encoding="utf-8"))
    for m in link.finditer(text):
        target = m.group(1) or m.group(2) or m.group(3)
        if not target or external.match(target):
            continue
        path = target.partition("#")[0]
        if not path:
            continue
        resolved = posixpath.normpath(posixpath.join(posixpath.dirname(twin), path))
        checked += 1
        if not (site / resolved).is_file():
            broken.append(f"{twin}: {target} -> {resolved}")

for m in missing:
    print(f"FAIL: no twin for {m}", file=sys.stderr)
for b in broken:
    print(f"FAIL: dead link {b}", file=sys.stderr)
if missing or broken:
    sys.exit(1)
print(f"site twins OK ({len(twins)} twins, {checked} relative links resolve)")
PY
