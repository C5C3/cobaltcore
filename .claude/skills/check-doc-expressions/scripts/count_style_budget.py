#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Count STYLE_GUIDE.md's rhetorical-device budget per documentation page.

The counting half of check-doc-expressions step 3. For every page it strips
what is not prose (frontmatter, fenced code, HTML comments and tags, inline
code, link and image URLs, VitePress code imports and container markers,
table separator rows) while keeping line numbers, then counts:

  em       em-dashes (U+2014)                          <= 2 per 1,000 words
  ital     italic spans *x* / _x_ (not **bold**, not
           list bullets, not snake_case)               <= 4 per 1,000 words
  anti     antithesis CANDIDATES: "rather than",
           "not X, but Y", "not just X but Y",
           "doesn't just", "X, not Y"                  <= 1 per page
  call     ::: callout boxes (info, tip, warning,
           danger, note, caution, important; not
           details, code-group, v-pre)                 <= 2 per page
  aph      aphoristic one-liner close CANDIDATES: a
           prose paragraph of two or more sentences
           that ends on a declarative sentence of at
           most 7 words, without digits or code        <= 1 per page
  filler   retired filler vocabulary                   0
  label    quality self-labels                         0

A table cell holding only a dash (the empty-cell convention) is not counted
as an em-dash. "exactly" before a number ("exactly one of clusterRef or host")
states a quantity and is not counted as filler; "clean" inside a compound
("db-clean") or as "clean up" is not counted as a self-label.

Rate budgets scale with max(1, words / 1000), so a page under 1,000 words
gets the full per-1,000 allowance. A page is [OVER] when any count exceeds its
allowance; its excess is the sum of the overshoots, which orders the summary.
Antithesis and aphorism detection are heuristics: they report candidates the
reader has to judge. Filler and label hits are exact word matches, but a hit
can still be the right word ("deliberately" naming a design decision the page
then explains), so they are candidates too.

Run it through the wrapper:

  bash .claude/skills/check-doc-expressions/scripts/count-style-budget.sh [--strict] [--top N] [paths...]
  bash .claude/skills/check-doc-expressions/scripts/count-style-budget.sh --page docs/quick-start.md

Exit code 0 unless --strict is given and a page is over budget.
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path

RATE_BUDGET = {"em": 2, "ital": 4}
PAGE_BUDGET = {"anti": 1, "call": 2, "aph": 1, "filler": 0, "label": 0}
DEVICES = ("em", "ital", "anti", "call", "aph", "filler", "label")

CALLOUT_TYPES = {"info", "tip", "warning", "danger", "note", "caution", "important"}

# Retired filler vocabulary (STYLE_GUIDE.md, Do/Don't 8).
FILLER = (
    "load-bearing",
    "by construction",
    "structural rather than aspirational",
    "first-class",
    "precisely",
    r"exactly(?![\s-]+(?:\*\*)?(?:one|two|three|four|five|once|twice|\d))",
    "deliberately",
)
# Quality self-labels (STYLE_GUIDE.md, Do/Don't 5 and the pre-commit check).
# "clean up" / "clean-up" is a verb, not a label.
LABELS = (
    r"robust(?:ly)?",
    r"clean(?![\s-]+up\b)",
    r"battle-tested",
    r"seamless(?:ly)?",
)

# No paragraph break inside a match.
_SAME_PARA = r"(?:(?!\n[ \t]*\n)[^.;:!?])"
ANTITHESIS = re.compile(
    r"\brather\s+than\b"
    r"|\bnot\s+(?:just|only|merely|simply)\b" + _SAME_PARA + r"{1,120}?\bbut\b"
    r"|\bnot\b" + _SAME_PARA + r"{1,80}?,\s*but\b"
    r"|\b(?:does|do|is|are|was|were)(?:n't|\s+not)\s+just\b"
    r"|,\s+not\s+(?!yet\b|necessarily\b|all\b|always\b|every\b|even\b)\w",
    re.IGNORECASE,
)
FILLER_RE = re.compile(
    r"\b(?:" + "|".join(p.replace(" ", r"\s+") for p in FILLER) + r")\b", re.IGNORECASE
)
LABEL_RE = re.compile(r"(?<![\w-])(?:" + "|".join(LABELS) + r")(?![\w-])", re.IGNORECASE)

ITALIC_STAR = re.compile(r"(?<![\w*\\])\*(?=[^\s*])(.+?)(?<=[^\s*\\])\*(?![\w*])")
ITALIC_UNDERSCORE = re.compile(r"(?<![\w\\])_(?=[^\s_])(.+?)(?<=[^\s_\\])_(?!\w)")
BOLD = re.compile(r"(\*\*|__)(?=\S)(.+?)(?<=\S)\1")

FENCE_OPEN = re.compile(r"^[ \t]*(`{3,}|~{3,})")
HTML_COMMENT = re.compile(r"<!--.*?-->", re.DOTALL)
INLINE_CODE = re.compile(r"(`+)(?!`)(.+?)(?<!`)\1(?!`)")
IMAGE = re.compile(r"!\[[^\]]*\]\([^)]*\)")
LINK = re.compile(r"\[([^\]]*)\]\([^)]*\)")
AUTOLINK = re.compile(r"<(?:https?|mailto):[^>\s]+>")
BARE_URL = re.compile(r"\bhttps?://\S+")
HTML_TAG = re.compile(r"</?[A-Za-z][A-Za-z0-9-]*(?:\s[^<>]*)?/?>")
HEADING_ANCHOR = re.compile(r"\s*\{#[^}]*\}")
REF_DEFINITION = re.compile(r"^[ \t]*\[[^\]]+\]:\s+\S")
CODE_IMPORT = re.compile(r"^[ \t]*<<<\s")
CONTAINER = re.compile(r"^[ \t]*:{3,}[ \t]*([A-Za-z-]*)")
TABLE_SEPARATOR = re.compile(r"^[ \t]*\|?[ \t]*:?-{3,}")
LIST_ITEM = re.compile(r"^[ \t]*(?:[-*+]|\d+[.)])[ \t]+")
WORD = re.compile(r"[A-Za-z0-9]")
CODE = "CODE"  # stands in for an inline code span
SENTENCE_SPLIT = re.compile(r"(?<=[.!?])[\"')\]]*\s+")
ABBREVIATIONS = re.compile(r"\b(?:e\.g|i\.e|etc|vs|cf)\.", re.IGNORECASE)
EMPTY_CELL = re.compile(r"(?<=\|)[ \t]*[\u2014\u2013-][ \t]*(?=\|)")
# A closing sentence that starts with one of these is an instruction, not a
# slogan.
IMPERATIVES = frozenset(
    "add apply avoid check complete confirm copy create delete disable do don't edit "
    "enable expect follow give install keep make note open pass pin point read refer "
    "remove replace restart run see set start stop use verify wait".split()
)


@dataclass
class Hit:
    line: int
    device: str
    text: str


@dataclass
class Page:
    path: str
    words: int = 0
    hits: list[Hit] = field(default_factory=list)

    def count(self, device: str) -> int:
        return sum(1 for h in self.hits if h.device == device)

    def allowance(self, device: str) -> float:
        if device in RATE_BUDGET:
            return RATE_BUDGET[device] * max(1.0, self.words / 1000.0)
        return float(PAGE_BUDGET[device])

    def over(self, device: str) -> bool:
        return self.count(device) > self.allowance(device)

    def excess(self) -> float:
        return sum(max(0.0, self.count(d) - self.allowance(d)) for d in DEVICES)


def clean(raw: str) -> tuple[list[str], list[str], list[Hit]]:
    """Return (prose lines, kind per line, callout hits), line-aligned with raw.

    kind is "prose", "heading", "list", "table", "container" or "blank"; the
    aphorism heuristic only reads "prose" paragraphs.
    """
    # HTML comments first (they can hold fences or span lines): keep newlines.
    text = HTML_COMMENT.sub(lambda m: "\n" * m.group(0).count("\n"), raw)
    lines = text.split("\n")
    out: list[str] = []
    kinds: list[str] = []
    callouts: list[Hit] = []

    i = 0
    if lines and lines[0].strip() == "---":
        out.append("")
        kinds.append("blank")
        i = 1
        while i < len(lines):
            end = lines[i].strip() == "---"
            out.append("")
            kinds.append("blank")
            i += 1
            if end:
                break

    fence: str | None = None
    for line in lines[i:]:
        if fence is not None:
            stripped = line.strip()
            if stripped.startswith(fence) and stripped.strip(fence[0]) == "":
                fence = None
            out.append("")
            kinds.append("blank")
            continue
        m = FENCE_OPEN.match(line)
        if m:
            fence = m.group(1)
            out.append("")
            kinds.append("blank")
            continue
        if CODE_IMPORT.match(line) or REF_DEFINITION.match(line) or TABLE_SEPARATOR.match(line):
            out.append("")
            kinds.append("blank")
            continue
        m = CONTAINER.match(line)
        if m:
            kind = m.group(1).lower()
            if kind in CALLOUT_TYPES:
                callouts.append(Hit(len(out) + 1, "call", line.strip()))
            out.append("")
            kinds.append("container")
            continue

        s = line
        # One word per code span, so "`helm upgrade`s" stays one token.
        s = INLINE_CODE.sub(CODE, s)
        s = IMAGE.sub(" ", s)
        s = LINK.sub(r"\1", s)
        s = AUTOLINK.sub(" ", s)
        s = BARE_URL.sub(" ", s)
        s = HTML_TAG.sub(" ", s)
        s = HEADING_ANCHOR.sub("", s)
        stripped = line.lstrip()
        if stripped.startswith("|"):
            s = EMPTY_CELL.sub(" ", s)
        out.append(s)
        if not s.strip():
            kinds.append("blank")
        elif stripped.startswith("#"):
            kinds.append("heading")
        elif stripped.startswith("|"):
            kinds.append("table")
        elif LIST_ITEM.match(line):
            kinds.append("list")
        elif stripped.startswith(">"):
            kinds.append("quote")
        else:
            kinds.append("prose")
    return out, kinds, callouts


def line_of(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


def snippet(text: str, start: int, end: int, pad: int = 30) -> str:
    a = max(0, start - pad)
    b = min(len(text), end + pad)
    return " ".join(text[a:b].split())


def aphorism_candidates(lines: list[str], kinds: list[str]) -> list[Hit]:
    hits: list[Hit] = []
    n = len(lines)
    i = 0
    while i < n:
        if kinds[i] != "prose":
            i += 1
            continue
        j = i
        while j < n and kinds[j] == "prose":
            j += 1
        para = " ".join(lines[k].strip() for k in range(i, j))
        para = ABBREVIATIONS.sub("x", para)
        sentences = [s for s in SENTENCE_SPLIT.split(para) if s.strip()]
        if len(sentences) >= 2:
            last = sentences[-1].strip()
            words = [w for w in last.split() if WORD.search(w)]
            first = re.sub(r"[^a-z']", "", words[0].lower()) if words else ""
            if (
                0 < len(words) <= 7
                and last.rstrip("\"')]*_").endswith((".", "!"))
                and first not in IMPERATIVES
                and first != "for"
                and not re.search(r"\d", last)
                and CODE not in last
            ):
                hits.append(Hit(j, "aph", last))
        i = j
    return hits


def analyse(path: Path) -> Page:
    raw = path.read_text(encoding="utf-8")
    lines, kinds, callouts = clean(raw)
    page = Page(str(path))
    page.hits.extend(callouts)

    for no, line in enumerate(lines, start=1):
        if not line.strip():
            continue
        page.words += sum(1 for tok in line.split() if WORD.search(tok))
        for m in re.finditer("—", line):
            page.hits.append(Hit(no, "em", snippet(line, m.start(), m.end())))
        unbolded = BOLD.sub(r"\2", line)
        for rx in (ITALIC_STAR, ITALIC_UNDERSCORE):
            for m in rx.finditer(unbolded):
                page.hits.append(Hit(no, "ital", m.group(0)))

    text = "\n".join(lines)
    for m in ANTITHESIS.finditer(text):
        page.hits.append(Hit(line_of(text, m.start()), "anti", snippet(text, m.start(), m.end())))
    for m in FILLER_RE.finditer(text):
        page.hits.append(Hit(line_of(text, m.start()), "filler", snippet(text, m.start(), m.end(), 20)))
    for m in LABEL_RE.finditer(text):
        page.hits.append(Hit(line_of(text, m.start()), "label", snippet(text, m.start(), m.end(), 20)))
    page.hits.extend(aphorism_candidates(lines, kinds))
    page.hits.sort(key=lambda h: (h.line, DEVICES.index(h.device)))
    return page


def summary_line(page: Page) -> str:
    parts = [f"words={page.words}"]
    for d in DEVICES:
        c = page.count(d)
        cell = f"{d}={c}"
        if d in RATE_BUDGET:
            rate = c * 1000.0 / page.words if page.words else 0.0
            cell += f"({rate:.1f}/k)"
        if page.over(d):
            cell += "!"
        parts.append(cell)
    tag = "[OVER]" if page.excess() > 0 else "[PASS]"
    excess = f" excess={page.excess():.1f}" if page.excess() > 0 else ""
    return f"{tag} {page.path} " + " ".join(parts) + excess


def collect(paths: list[str], root: Path) -> list[Path]:
    if not paths:
        found = [p for p in sorted((root / "docs").rglob("*.md")) if ".vitepress" not in p.parts]
        readme = root / "README.md"
        if readme.is_file():
            found.append(readme)
        return found
    out: list[Path] = []
    for arg in paths:
        p = Path(arg)
        if not p.is_absolute():
            p = Path.cwd() / p
        if p.is_dir():
            out.extend(q for q in sorted(p.rglob("*.md")) if ".vitepress" not in q.parts)
        elif p.is_file():
            out.append(p)
        else:
            print(f"error: no such file or directory: {arg}", file=sys.stderr)
            sys.exit(2)
    return out


def rel(p: Path, root: Path) -> Path:
    try:
        return p.resolve().relative_to(root)
    except ValueError:
        return p


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("paths", nargs="*", help="pages or directories (default: docs/**/*.md and README.md)")
    ap.add_argument("--page", help="print every hit on one page with its line number")
    ap.add_argument("--top", type=int, default=10, help="pages in the over-budget summary (default 10)")
    ap.add_argument("--strict", action="store_true", help="exit 1 when any page is over budget")
    ap.add_argument("--root", default=".", help=argparse.SUPPRESS)
    args = ap.parse_args()
    root = Path(args.root).resolve()

    if args.page:
        target = Path(args.page)
        if not target.is_file():
            print(f"error: no such page: {args.page}", file=sys.stderr)
            return 2
        page = analyse(target)
        page.path = str(rel(target, root))
        print(summary_line(page))
        for h in page.hits:
            print(f"  {page.path}:{h.line}: {h.device}: {h.text}")
        return 1 if args.strict and page.excess() > 0 else 0

    pages = []
    for p in collect(args.paths, root):
        page = analyse(p)
        page.path = str(rel(p, root))
        pages.append(page)
        print(summary_line(page))

    over = [p for p in pages if p.excess() > 0]
    print()
    print(f"[INFO] {len(pages)} page(s), {len(over)} over budget; "
          f"'!' marks the device over its allowance; anti and aph are candidates to judge")
    if over:
        print(f"[INFO] top {min(args.top, len(over))} by excess:")
        for p in sorted(over, key=lambda x: (-x.excess(), x.path))[: args.top]:
            devs = ", ".join(f"{d} {p.count(d)}/{p.allowance(d):.3g}" for d in DEVICES if p.over(d))
            print(f"[INFO]   {p.excess():5.1f}  {p.path}  ({devs})")
    if args.strict and over:
        print(f"[FAIL] {len(over)} page(s) over the STYLE_GUIDE.md budget")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
