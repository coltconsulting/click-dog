#!/usr/bin/env bash
#
# check-site-fonts.sh — every shipped font file covers what its @font-face
# rule declares.
#
# Google Fonts serves one woff2 per unicode-range block, and the blocks all
# look alike from the outside: a cyrillic-ext subset was once committed under
# a "-latin" name and declared over the latin unicode-range, so bold-italic
# text pulled 9 KB and then fell back to a system font. The CSS parity check
# in check-site-escape.sh compares rules, not fonts; this opens each file
# named by an @font-face rule in www/home.html and asserts its cmap covers
# printable ASCII plus the punctuation and symbols the latin range promises.
#
# Needs fontTools + brotli (requirements-docs.txt).
#
# exit 0  every declared font covers the required code points
# exit 1  a file is missing, unreadable, or lacks a required glyph

set -euo pipefail
cd "$(dirname "$0")/.."

python3 - <<'PY'
import re, sys
from pathlib import Path
from fontTools.ttLib import TTFont

REQUIRED = list(range(0x20, 0x7F)) + [
    0x00A0, 0x00E9, 0x0131, 0x0152, 0x0153, 0x02C6, 0x02DA, 0x02DC,
    0x2013, 0x2014, 0x2018, 0x2019, 0x201C, 0x201D, 0x2026,
    0x20AC, 0x2122, 0x2191, 0x2193, 0x2212, 0x2215,
]
rules = re.findall(r"@font-face\{([^{]*?)\}", Path("www/home.html").read_text(encoding="utf-8"))
fails, seen = [], 0
for rule in rules:
    m = re.search(r"url\(/assets/fonts/([^)]+)\)", rule)
    if not m:
        fails.append(f"rule without a first-party url(): {rule[:60]}"); continue
    path = Path("www/assets/fonts") / m.group(1)
    family = re.search(r"font-family:'([^']+)'", rule).group(1)
    style = re.search(r"font-style:(\w+)", rule).group(1)
    weight = re.search(r"font-weight:([\d ]+);", rule).group(1)
    if not path.is_file():
        fails.append(f"{path}: missing (declared for {family} {style} {weight})"); continue
    try:
        cmap = set(TTFont(path).getBestCmap())
    except Exception as e:
        fails.append(f"{path}: unreadable: {e}"); continue
    missing = [c for c in REQUIRED if c not in cmap]
    seen += 1
    if missing:
        sample = " ".join(f"U+{c:04X}" for c in missing[:8])
        fails.append(f"{path}: {family} {style} {weight} lacks {len(missing)} required code points ({sample}{' …' if len(missing) > 8 else ''}); wrong subset?")
for f in fails:
    print(f"FAIL: {f}", file=sys.stderr)
if fails or not seen:
    sys.exit(1)
print(f"site fonts OK ({seen} declared faces cover the latin range)")
PY
