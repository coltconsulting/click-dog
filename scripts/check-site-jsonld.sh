#!/usr/bin/env bash
#
# check-site-jsonld.sh — the entity graph the site emits is consistent.
#
# www/home.html (the landing, inside {% raw %}) and www/main.html (every docs
# page) each carry a JSON-LD graph, and the shared nodes are duplicated by
# hand because the landing cannot include a Jinja partial. This parses both,
# asserts that every property the two copies both declare for a node with
# the same @id has the same value, that each docs page's TechArticle has a
# description, and that the BreadcrumbList matches the visible breadcrumb
# (Material's partials/path.html) plus the page's own title.
#
# Usage: check-site-jsonld.sh [SITE_DIR]   (default: target; build it first
#                                           with `mkdocs build --strict`)
#
# exit 0  graphs parse, shared nodes agree, breadcrumbs match
# exit 1  otherwise; every mismatch is printed

set -euo pipefail
cd "$(dirname "$0")/.."

SITE="${1:-target}"
[ -d "$SITE" ] || { echo "FAIL: $SITE does not exist; run mkdocs build first" >&2; exit 1; }

python3 - "$SITE" <<'PY'
import html as htmllib
import json, re, sys
from pathlib import Path

site = Path(sys.argv[1])
fails = []

def graph(path):
    html = path.read_text(encoding="utf-8")
    m = re.search(r'<script type="application/ld\+json">(.*?)</script>', html, re.S)
    if not m:
        fails.append(f"{path}: no ld+json block")
        return None, html
    try:
        return json.loads(m.group(1)), html
    except json.JSONDecodeError as e:
        fails.append(f"{path}: invalid JSON-LD: {e}")
        return None, html

def by_id(g):
    return {n["@id"]: n for n in g["@graph"] if "@id" in n}

landing, _ = graph(site / "index.html")
docs_pages = sorted(p for p in site.rglob("index.html") if p != site / "index.html")

# 1. Every node a docs page shares with the landing is present there, and the
# two copies agree wherever both declare a property. The shared set is derived
# rather than listed: it is every @id the docs template emits except the two
# that belong to the page itself, so adding a node to the graph brings it under
# this check without a third place to edit. A renamed @id on either side shows
# up here as a node missing from the other graph, which is the drift the two
# hand-maintained copies exist to risk.
if landing:
    l = by_id(landing)
    sample, _ = graph(docs_pages[0])
    if sample:
        d = by_id(sample)
        page_own = ("#article", "#breadcrumb")
        shared = {nid for nid in d if not nid.endswith(page_own)}
        if not shared:
            fails.append(f"{docs_pages[0].relative_to(site)}: no shared nodes in the graph at all")
        for nid in sorted(shared):
            if nid not in l:
                fails.append(f"shared node {nid} is in the docs graph but not the landing (renamed @id?)")
                continue
            for k, v in d[nid].items():
                if k in l[nid] and l[nid][k] != v:
                    fails.append(f"shared node {nid} disagrees on {k!r}: landing={l[nid][k]!r} docs={v!r}")
        for nid in sorted(set(l) - shared):
            fails.append(f"shared node {nid} is in the landing graph but not the docs graph (renamed @id?)")

# 2. Every docs page: TechArticle with a description; breadcrumb matches the visible trail.
crumb = re.compile(r'md-path__link">\s*<span class="md-ellipsis">\s*(.*?)\s*</span>', re.S)
for p in docs_pages:
    g, html = graph(p)
    if not g:
        continue
    nodes = g["@graph"]
    art = next((n for n in nodes if n.get("@type") == "TechArticle"), None)
    if not art or not art.get("description"):
        fails.append(f"{p.relative_to(site)}: TechArticle missing or without description")
    bc = next((n for n in nodes if n.get("@type") == "BreadcrumbList"), None)
    visible = [re.sub(r"\s+", " ", c).strip() for c in crumb.findall(html)]
    if not visible:
        # Material renders no breadcrumb for a page directly under home, so
        # the markup must be absent there too.
        if bc:
            fails.append(f"{p.relative_to(site)}: BreadcrumbList without a visible breadcrumb")
        continue
    if not bc:
        fails.append(f"{p.relative_to(site)}: visible breadcrumb but no BreadcrumbList")
        continue
    items = bc["itemListElement"]
    names = [i["name"] for i in items]
    tm = re.search(r"<title>(.*?)</title>", html, re.S)
    if not tm:
        fails.append(f"{p.relative_to(site)}: no <title>")
        continue
    # <title> is HTML-escaped by main.html; the crumb name is raw text.
    title = htmllib.unescape(tm.group(1))
    visible = [htmllib.unescape(v) for v in visible]
    if names[:-1] != visible:
        fails.append(f"{p.relative_to(site)}: breadcrumb {names[:-1]} != visible {visible}")
    if not title.startswith(names[-1]):
        fails.append(f"{p.relative_to(site)}: last crumb {names[-1]!r} is not the page title {title!r}")
    if any("item" not in i for i in items[:-1]):
        fails.append(f"{p.relative_to(site)}: an intermediate crumb has no item URL")
    if [i["position"] for i in items] != list(range(1, len(items) + 1)):
        fails.append(f"{p.relative_to(site)}: positions are not 1..n")

for f in fails:
    print(f"FAIL: {f}", file=sys.stderr)
if fails:
    sys.exit(1)
print(f"site JSON-LD OK (landing + {len(docs_pages)} docs pages; shared nodes agree; breadcrumbs match)")
PY
