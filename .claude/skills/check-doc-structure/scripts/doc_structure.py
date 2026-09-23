#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Structural checks over the VitePress docs tree (docs/).

The deterministic half of the check-doc-structure skill. `npm run docs:build`
already fails on a link to a page that does not exist; it does not check
fragment anchors, sidebar entries, heading hierarchy, or whether a page can be
reached at all. Those are the checks here:

  T1  every page opens with a frontmatter block carrying `title:`; pages
      without `quadrant:` are listed as [INFO] (the key is a family
      convention, not universal)
  T2  exactly one H1 per page, and no heading skips a level (## -> ####)
  T3  every relative/absolute docs link resolves to a page, and every
      `#fragment` (same-page or cross-page) resolves to a heading slug, an
      explicit {#id}, or an HTML id/name on the target page
  T4  every sidebar/nav `link:` in docs/.vitepress/config.ts resolves to a page
  T5  every page is reachable: listed in the sidebar/nav or linked from at
      least one other page (an orphan is [FAIL]); pages reachable only
      through links are listed as [INFO]
  T6  no bare <placeholder> in prose: VitePress compiles every page as a Vue
      template, so `<name>` outside backticks or a fence is an element that
      never closes and fails the whole build ("Element is missing end tag")

Prints [PASS]/[FAIL]/[INFO] lines and exits 1 on any [FAIL]. Run it through
the audit script:

  bash .claude/skills/check-doc-structure/scripts/audit-doc-structure.sh
"""

import re
import sys
import unicodedata
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[4]
DOCS = REPO_ROOT / "docs"
CONFIG = DOCS / ".vitepress" / "config.ts"

fail_count = 0


def emit(level, message):
    global fail_count
    if level == "FAIL":
        fail_count += 1
    print(f"[{level}] {message}")


def header(title):
    print()
    print(f"=== {title} ===")


# VitePress' default slugify (node_modules/vitepress/dist/node, 1.6.x), applied
# to the heading's rendered text. Note what it does NOT do the GitHub way: runs of
# separators collapse to ONE hyphen ("A / B" -> "a-b", never "a--b"), and a slug
# that starts with a digit gains a leading underscore ("6. Recover" -> "_6-recover").
R_CONTROL = re.compile(r"[\u0000-\u001f]")
R_SPECIAL = re.compile(r"[\s~`!@#$%^&*()\-_+=\[\]{}|\\;:\"'“”‘’<>,.?/]+")
R_COMBINING = re.compile("[\\u0300-\\u036f]")


def slugify(text):
    text = unicodedata.normalize("NFKD", text)
    text = R_COMBINING.sub("", text)
    text = R_CONTROL.sub("", text)
    text = R_SPECIAL.sub("-", text)
    text = re.sub(r"-{2,}", "-", text)
    text = re.sub(r"^-+|-+$", "", text)
    text = re.sub(r"^(\d)", r"_\1", text)
    return text.lower()


def heading_text(raw):
    """Approximate the rendered text of a markdown heading."""
    text = re.sub(r"\s*\{#[^}]+\}\s*$", "", raw)  # explicit anchor
    text = re.sub(r"!\[([^\]]*)\]\([^)]*\)", r"\1", text)  # images
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)  # links
    text = re.sub(r"<[^>]+>", "", text)  # inline HTML (badges)
    text = text.replace("`", "")
    text = re.sub(r"(\*\*|__|\*|_)(\S.*?\S|\S)\1", r"\2", text)
    return text.strip()


FENCE = re.compile(r"^\s*(```|~~~)")
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
LINK = re.compile(r"(?<!!)\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")  # [text] may span lines
BARE_TAG = re.compile(r"</?([A-Za-z][A-Za-z0-9-]*)(?:\s[^<>]*)?/?>")
# Elements Vue/VitePress render as HTML; anything else in angle brackets outside
# code is a placeholder the template compiler chokes on. Capitalised names are
# Vue components (<Badge>) and pass.
HTML_TAGS = {
    "a", "abbr", "b", "blockquote", "br", "caption", "code", "col", "colgroup",
    "dd", "del", "details", "div", "dl", "dt", "em", "figcaption", "figure",
    "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "img", "input", "ins", "kbd",
    "li", "mark", "ol", "p", "picture", "pre", "q", "s", "samp", "section",
    "small", "source", "span", "strong", "sub", "summary", "sup", "table",
    "tbody", "td", "tfoot", "th", "thead", "tr", "u", "ul", "var", "video",
    "svg", "path", "g", "rect", "circle", "line", "polyline", "text", "iframe",
    "script", "style", "template", "slot",
}
HTML_ID = re.compile(r"<[a-zA-Z][^>]*\s(?:id|name)=[\"']([^\"']+)[\"']")


class Page:
    def __init__(self, path):
        self.path = path
        self.rel = path.relative_to(REPO_ROOT)
        self.text = path.read_text(encoding="utf-8")
        self.lines = self.text.split("\n")
        self.frontmatter = {}
        self.body_start = 0
        self.headings = []  # (line, level, raw)
        self.anchors = set()
        self.links = []  # (line, target)
        self.bare_tags = []  # (line, tag text)
        self._parse()

    def _parse(self):
        lines = self.lines
        if lines and lines[0].strip() == "---":
            for i in range(1, len(lines)):
                if lines[i].strip() == "---":
                    self.body_start = i + 1
                    break
                m = re.match(r"^([A-Za-z0-9_-]+):\s*(.*)$", lines[i])
                if m:
                    self.frontmatter[m.group(1)] = m.group(2)
        in_fence = False
        seen = {}
        prose = [""] * len(lines)  # fenced lines blanked, numbering kept
        for i in range(self.body_start, len(lines)):
            line = lines[i]
            if FENCE.match(line):
                in_fence = not in_fence
                continue
            if in_fence:
                continue
            prose[i] = line
            m = HEADING.match(line)
            if m:
                level, raw = len(m.group(1)), m.group(2)
                self.headings.append((i + 1, level, raw))
                explicit = re.search(r"\{#([^}]+)\}\s*$", raw)
                if explicit:
                    self.anchors.add(explicit.group(1))
                else:
                    slug = slugify(heading_text(raw))
                    n = seen.get(slug, 0)
                    seen[slug] = n + 1
                    self.anchors.add(slug if n == 0 else f"{slug}-{n}")
            for hid in HTML_ID.findall(line):
                self.anchors.add(hid)
        # Inline code spans may wrap across lines; blank them out over the whole
        # body (newlines kept) before looking for bare angle-bracket tags.
        # Markdown code spans never cross a blank line, so pair backticks per
        # paragraph: one stray backtick then cannot shift the pairing of a
        # whole page.
        text = "\n".join(prose)
        text = re.sub(
            r"(?:[^\n]+\n?)+",
            lambda para: re.sub(r"`[^`]*`", lambda m: "\n" * m.group(0).count("\n"), para.group(0)),
            text,
        )
        text = re.sub(r"<!--.*?-->", lambda m: "\n" * m.group(0).count("\n"), text, flags=re.S)
        # Links are read from the same paragraph text, so a link whose text
        # wraps onto the next line ("[Owner-ref /\nGC model](#…)") is seen too.
        for link in LINK.finditer(text):
            line_no = text.count("\n", 0, link.start()) + 1
            self.links.append((line_no, link.group(1)))
        for tag in BARE_TAG.finditer(text):
            name = tag.group(1)
            if name.lower() not in HTML_TAGS and not name[0].isupper():
                line_no = text.count("\n", 0, tag.start()) + 1
                self.bare_tags.append((line_no, tag.group(0)))


def page_for(target, base):
    """Resolve a docs link target (no fragment) to a page path, or None."""
    if target.startswith("/"):
        candidate = DOCS / target.lstrip("/")
    else:
        candidate = (base.parent / target).resolve()
    candidate = Path(str(candidate))
    options = []
    if str(candidate).endswith(".md"):
        options.append(candidate)
    elif str(candidate).endswith(".html"):
        options.append(Path(str(candidate)[:-5] + ".md"))
    else:
        options.append(Path(str(candidate) + ".md"))
        options.append(candidate / "index.md")
        if str(target).endswith("/"):
            options.insert(0, candidate / "index.md")
    for option in options:
        if option.is_file():
            return option.resolve()
    return None


def main():
    pages = {}
    for path in sorted(DOCS.rglob("*.md")):
        if ".vitepress" in path.parts or "node_modules" in path.parts:
            continue
        pages[path.resolve()] = Page(path)
    emit("INFO", f"{len(pages)} page(s) under docs/")

    header("T1: frontmatter carries title (quadrant is listed where absent)")
    missing_fm = 0
    no_quadrant = []
    for page in pages.values():
        if not page.frontmatter:
            emit("FAIL", f"{page.rel}:1: no frontmatter block (VitePress needs `title:` on line 2)")
            missing_fm += 1
            continue
        if "title" not in page.frontmatter:
            emit("FAIL", f"{page.rel}:1: frontmatter has no title:")
            missing_fm += 1
        if "quadrant" not in page.frontmatter:
            no_quadrant.append(str(page.rel))
    if missing_fm == 0:
        emit("PASS", f"all {len(pages)} page(s) carry a frontmatter title")
    if no_quadrant:
        emit("INFO", f"{len(no_quadrant)} page(s) without quadrant: {', '.join(no_quadrant)}")

    header("T2: one H1 per page, no skipped heading level")
    bad = 0
    for page in pages.values():
        h1 = [h for h in page.headings if h[1] == 1]
        if len(h1) != 1:
            where = ", ".join(str(h[0]) for h in h1) or "none"
            emit("FAIL", f"{page.rel}: {len(h1)} H1 heading(s) (lines: {where})")
            bad += 1
        prev = 0
        for line, level, raw in page.headings:
            if prev and level > prev + 1:
                emit("FAIL", f"{page.rel}:{line}: H{level} '{heading_text(raw)}' follows H{prev} (skipped level)")
                bad += 1
            prev = level
    if bad == 0:
        emit("PASS", "every page has one H1 and a gap-free heading hierarchy")

    header("T3: docs links and #anchors resolve")
    broken = 0
    checked = 0
    inbound = {p: set() for p in pages}
    for page in pages.values():
        for line, target in page.links:
            if re.match(r"^[a-z][a-z0-9+.-]*:", target) or target.startswith("//"):
                continue  # http(s), mailto, …
            path_part, _, fragment = target.partition("#")
            path_part = path_part.split("?")[0]
            if path_part == "":
                dest = page.path.resolve()
            else:
                dest = page_for(path_part, page.path)
                if dest is None:
                    # Links into the repo outside docs/ or to assets are the
                    # build's business; only report docs-shaped targets.
                    if path_part.endswith((".md", "/")) or "." not in Path(path_part).name:
                        emit("FAIL", f"{page.rel}:{line}: link target '{target}' resolves to no page")
                        broken += 1
                    continue
            checked += 1
            if dest in inbound and dest != page.path.resolve():
                inbound[dest].add(page.path.resolve())
            if fragment and dest in pages and fragment not in pages[dest].anchors:
                emit("FAIL", f"{page.rel}:{line}: anchor '#{fragment}' not found in {pages[dest].rel}")
                broken += 1
    if broken == 0:
        emit("PASS", f"all {checked} docs link(s) and their anchors resolve")

    header("T4: sidebar and nav links resolve")
    config_links = []
    if CONFIG.is_file():
        for n, line in enumerate(CONFIG.read_text(encoding="utf-8").split("\n"), start=1):
            for link in re.findall(r"link:\s*'([^']+)'", line) + re.findall(r'link:\s*"([^"]+)"', line):
                config_links.append((n, link))
    else:
        emit("FAIL", f"{CONFIG.relative_to(REPO_ROOT)} not found")
    listed = set()
    dead = 0
    for n, link in config_links:
        if re.match(r"^[a-z]+:", link):
            continue
        path_part = link.split("#")[0]
        dest = page_for(path_part if path_part != "/" else "/index", DOCS / "index.md")
        if dest is None and path_part.endswith("/"):
            dest = page_for(path_part + "index", DOCS / "index.md")
        if dest is None:
            emit("FAIL", f"docs/.vitepress/config.ts:{n}: link '{link}' resolves to no page")
            dead += 1
        else:
            listed.add(dest)
    if dead == 0:
        emit("PASS", f"all {len(config_links)} sidebar/nav link(s) resolve")

    header("T5: every page is reachable (sidebar/nav or an inbound link)")
    orphans = 0
    link_only = []
    for dest, page in pages.items():
        if dest in listed:
            continue
        if inbound[dest]:
            link_only.append(str(page.rel))
            continue
        emit("FAIL", f"{page.rel}: orphan — not in the sidebar/nav and no page links to it")
        orphans += 1
    if orphans == 0:
        emit("PASS", "no orphan pages")
    if link_only:
        emit("INFO", f"{len(link_only)} page(s) reachable only through links, not the sidebar: {', '.join(sorted(link_only))}")

    header("T6: no bare <placeholder> outside code (VitePress compiles pages as Vue)")
    bare = 0
    for page in pages.values():
        for line, text in page.bare_tags:
            emit("FAIL", f"{page.rel}:{line}: '{text}' outside backticks is parsed as a Vue element — wrap it in `code`")
            bare += 1
    if bare == 0:
        emit("PASS", "no bare angle-bracket placeholder in prose")

    return 1 if fail_count else 0


if __name__ == "__main__":
    sys.exit(main())
