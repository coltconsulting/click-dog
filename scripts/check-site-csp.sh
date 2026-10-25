#!/usr/bin/env bash
#
# check-site-csp.sh — every external resource the built site references is
# allowed by the Content-Security-Policy in www/_headers.
#
# The policy is enforced from the first deploy and has no report endpoint, so
# a docs page adding a badge, an embedded frame or a CDN script would break
# silently in the browser. This scans script/link/img/source/iframe/video/
# audio tags, CSS url() in stylesheets and inline <style>, and fetch()/
# WebSocket/EventSource/XHR literals in inline and external scripts, maps each to the
# directive that governs it, and fails if the host is not listed there. 'self'
# resolves to mkdocs.yml's site_url and is allowed with a warning, since the
# same absolute URL is cross-origin on staging. Plain <a href> navigations are
# not restricted by CSP and are ignored.
#
# Usage: check-site-csp.sh [SITE_DIR]   (default: target; build it first
#                                        with `mkdocs build --strict`)
#
# exit 0  every referenced host is allowed
# exit 1  a resource would be blocked, or _headers has no CSP

set -euo pipefail
cd "$(dirname "$0")/.."

SITE="${1:-target}"
[ -d "$SITE" ] || { echo "FAIL: $SITE does not exist; run mkdocs build first" >&2; exit 1; }
[ -f "$SITE/_headers" ] || { echo "FAIL: $SITE/_headers missing (is www/_headers copied?)" >&2; exit 1; }

python3 - "$SITE" <<'PY'
import re, sys
from pathlib import Path
from urllib.parse import urlsplit

site = Path(sys.argv[1])
headers = (site / "_headers").read_text(encoding="utf-8")
# 'self' is the production origin (mkdocs.yml site_url); the same absolute URL
# is cross-origin on staging, so it is reported as a warning, not allowed silently.
# Read from the repo's own mkdocs.yml, which is what docs.yml builds. A prod
# build substitutes mkdocs.yml from the pinned GA tag; site_url has never
# differed between the two, but if it did, the checked-in file wins here.
site_url = re.search(r"^site_url:\s*(\S+)", Path("mkdocs.yml").read_text(encoding="utf-8"), re.M)
self_origin = None
if site_url:
    u = urlsplit(site_url.group(1)); self_origin = f"{u.scheme}://{u.netloc}"
warnings = []
m = re.search(r"^\s*Content-Security-Policy:\s*(.+)$", headers, re.M)
if not m:
    print("FAIL: no Content-Security-Policy in _headers", file=sys.stderr); sys.exit(1)
policy = {}
for part in m.group(1).split(";"):
    toks = part.split()
    if toks:
        policy[toks[0]] = set(toks[1:])

def allowed(directive, url, where=""):
    sources = policy.get(directive) or policy.get("default-src", set())
    if url.startswith("data:"):
        return "data:" in sources
    host = urlsplit(url)
    origin = f"{host.scheme}://{host.netloc}"
    if origin == self_origin and "'self'" in sources:
        warnings.append(f"{where}: {url} is first-party on {self_origin} but cross-origin on staging and local previews; prefer a relative path")
        return True
    return origin in sources or f"https://{host.netloc}" in sources or "*" in sources

tag = re.compile(r"<(script|link|img|source|iframe|video|audio)\b([^>]*)>", re.I)
attr = re.compile(r'\b(src|href|srcset)="([^"]*)"', re.I)
cssurl = re.compile(r"url\((['\"]?)(https?://[^)'\"]+|data:[^)'\"]+)\1\)")
# Fetch targets written as literals in inline scripts: fetch('…'), new WebSocket('…'),
# EventSource('…'), xhr.open('GET', '…'). Governed by connect-src.
fetchlit = re.compile(r"(?:fetch|WebSocket|EventSource)\(\s*['\"](https?://[^'\"]+)|\.open\(\s*['\"][A-Z]+['\"]\s*,\s*['\"](https?://[^'\"]+)")
inline_style = re.compile(r"<style\b[^>]*>(.*?)</style>", re.S | re.I)
inline_script = re.compile(r"<script\b(?![^>]*\bsrc=)(?![^>]*application/ld\+json)[^>]*>(.*?)</script>", re.S | re.I)
fails, checked = [], 0

def directive_for(name, attrs):
    rel = (re.search(r'\brel="([^"]*)"', attrs, re.I) or [None, ""])[1].lower()
    as_ = (re.search(r'\bas="([^"]*)"', attrs, re.I) or [None, ""])[1].lower()
    if name == "script": return "script-src"
    if name == "link":
        if "stylesheet" in rel: return "style-src"
        if "preload" in rel or "modulepreload" in rel:
            return {"font": "font-src", "script": "script-src", "style": "style-src", "image": "img-src"}.get(as_, "default-src")
        if "icon" in rel: return "img-src"
        return None  # canonical, alternate, prev/next, manifest: not fetched by CSP-governed loads
    if name in ("img", "source"): return "img-src"
    if name == "iframe": return "frame-src" if "frame-src" in policy else "child-src"
    return "media-src"

for path in sorted(site.rglob("*.html")):
    html = path.read_text(encoding="utf-8", errors="replace")
    rel = path.relative_to(site)
    for block in inline_style.findall(html):
        for _, url in cssurl.findall(block):
            checked += 1
            d = "font-src" if re.search(r"\.(woff2?|ttf|otf)(\?|$)", url) else "img-src"
            if not allowed(d, url, str(rel)):
                fails.append(f"{rel}: inline <style> url({url[:60]}) not allowed by {d}")
    for block in inline_script.findall(html):
        for a, b in fetchlit.findall(block):
            url = a or b
            checked += 1
            if not allowed("connect-src", url, str(rel)):
                fails.append(f"{rel}: inline script fetches {url} not allowed by connect-src")
    for name, attrs in tag.findall(html):
        d = directive_for(name.lower(), attrs)
        if not d: continue
        for _, value in attr.findall(attrs):
            for candidate in value.split(","):
                url = candidate.strip().split()[0] if candidate.strip() else ""
                if not url or not re.match(r"^(https?:)?//|^data:", url): continue
                if url.startswith("//"): url = "https:" + url
                checked += 1
                if not allowed(d, url, str(rel)):
                    fails.append(f"{rel}: <{name}> {url} not allowed by {d}")
for path in sorted(site.rglob("*.css")):
    rel = path.relative_to(site)
    css = path.read_text(encoding="utf-8", errors="replace")
    for _, url in cssurl.findall(css):
        checked += 1
        d = "font-src" if re.search(r"\.(woff2?|ttf|otf)(\?|$)", url) else "img-src"
        if not allowed(d, url, str(rel)):
            fails.append(f"{rel}: css url({url[:60]}) not allowed by {d}")
# External scripts get the same fetch-literal scan as inline ones.
for path in sorted(site.rglob("*.js")):
    rel = path.relative_to(site)
    js = path.read_text(encoding="utf-8", errors="replace")
    for a, b in fetchlit.findall(js):
        url = a or b
        checked += 1
        if not allowed("connect-src", url, str(rel)):
            fails.append(f"{rel}: script fetches {url} not allowed by connect-src")

for w in sorted(set(warnings)):
    print(f"WARN: {w}", file=sys.stderr)
for f in fails:
    print(f"FAIL: {f}", file=sys.stderr)
if fails:
    sys.exit(1)
print(f"site CSP OK ({checked} external references, all allowed)")
PY
