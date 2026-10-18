# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Parse the ASCII event trees of the *-events.md pages and draw them as figures.

A tree line is `├── name` or `└── name` (a node) or `├─ condition → Type Reason / Reason`
(a leaf). A continuation line without a marker belongs to the leaf above: more
reasons, a second `Type Reason …` chip, or a `(note)`.
"""
import re
from figlib import Fig, INK, MUTED, GREY, LINE, PAPER, INK_DARK, WHITE_TEXT, text_width

MARK = re.compile(r"^([│ ]*)(├──|└──|├─|└─)\s*(.*)$")


class Node:
    def __init__(self, name, depth):
        self.name = name
        self.depth = depth
        self.children = []


class Leaf:
    def __init__(self, cond, depth):
        self.cond = cond
        self.depth = depth
        self.events = []  # [(type, [reasons])]
        self.notes = []


def parse_tree(lines):
    """Parse one ASCII tree (root line first). Returns the root Node."""
    root = Node(lines[0].strip(), -1)
    stack = [root]
    last_leaf = None
    merged = []
    for raw in lines[1:]:
        if merged and raw.replace("│", " ").strip().startswith("→") and "→" not in merged[-1]:
            merged[-1] = merged[-1].rstrip() + " " + raw.replace("│", " ").strip()
        else:
            merged.append(raw)
    for raw in merged:
        if not raw.strip() or raw.strip() == "│":
            continue
        m = MARK.match(raw)
        if m:
            depth = len(m.group(1)) // 2 if m.group(1) else 0
            depth = m.start(2) // 6  # 6 columns per level
            text = m.group(3).strip()
            while len(stack) > 1 and stack[-1].depth >= depth:
                stack.pop()
            parent = stack[-1]
            if "→" in text or text.startswith(("Normal ", "Warning ")) or text.startswith("(condition only"):
                if "→" in text:
                    cond, ev = text.split("→", 1)
                else:
                    cond, ev = "", text
                leaf = Leaf(cond.strip(), depth)
                add_event_text(leaf, ev.strip())
                parent.children.append(leaf)
                last_leaf = leaf
            else:
                node = Node(text.rstrip(":"), depth)
                parent.children.append(node)
                stack.append(node)
                last_leaf = None
        else:
            text = raw.replace("│", " ").strip()
            if last_leaf is None:
                continue
            if text.startswith("→"):
                text = text[1:].strip()
            add_event_text(last_leaf, text)
    return root


def add_event_text(leaf, text):
    text = text.strip()
    if not text:
        return
    # a trailing (note)
    note = None
    m = re.search(r"\s*\(([^()]*)\)\s*$", text)
    if m and not text.startswith("("):
        note = m.group(1)
        text = text[:m.start()].strip()
    if text.startswith("("):
        leaf.notes.append(text.strip("()"))
        text = ""
    elif text.startswith("Normal ") or text.startswith("Warning "):
        typ, rest = text.split(" ", 1)
        leaf.events.append((typ, [r.strip() for r in rest.split("/") if r.strip()]))
    elif text and leaf.events:
        leaf.events[-1][1].extend(r.strip() for r in text.split("/") if r.strip())
    if note:
        leaf.notes.append(note)


# ---------------------------------------------------------------------------
# Layout
# ---------------------------------------------------------------------------
X0, Y0 = 60, 40
INDENT = 70
NODE_H = 48
ROW = 32
CHIP_H = 28
GAP_NODE = 18


def measure(node):
    """Height of a node's subtree block, in px, below its own box."""
    h = 0
    for c in node.children:
        if isinstance(c, Node):
            h += NODE_H + GAP_NODE + measure(c) + 14
        else:
            h += leaf_height(c)
    return h


def leaf_height(leaf):
    lines = 0
    for typ, reasons in leaf.events:
        lines += max(1, len(wrap_reasons(reasons)))
    lines = max(1, lines)
    lines += len(leaf.notes)
    return lines * ROW


def wrap_reasons(reasons, limit=520):
    lines, cur = [], ""
    for r in reasons:
        cand = r if not cur else cur + " / " + r
        if text_width(cand, 20) > limit and cur:
            lines.append(cur + " /")
            cur = r
        else:
            cur = cand
    if cur:
        lines.append(cur)
    return lines


def draw_tree(fig, root, x, y):
    """Draw root at (x, y); return the bottom y."""
    w = max(260, text_width(root.name, 24, bold=True) + 44)
    fig.box(x, y, w, NODE_H + 8, root.name)
    return draw_children(fig, root, x, y + NODE_H + 8, x + w)


def draw_children(fig, node, x, y, right):
    """Draw node's children below y with the spine at x + 30. Return bottom y and right extent."""
    spine_x = x + 30
    cy = y + GAP_NODE
    last_mid = None
    cond_w = max([text_width(c.cond, 20) for c in node.children if isinstance(c, Leaf) and c.cond] + [0])
    for c in node.children:
        cx = x + INDENT
        if isinstance(c, Node):
            w = max(220, text_width(c.name, 22, bold=True) + 40)
            fig.box(cx, cy, w, NODE_H, c.name, fs=22)
            mid = cy + NODE_H / 2
            fig.arrow([(spine_x, mid), (cx, mid)], color=GREY, sw=2, head=False)
            last_mid = mid
            right = max(right, cx + w)
            bottom, right = draw_children(fig, c, cx, cy + NODE_H, right)
            cy = bottom + 14
        else:
            mid = cy + ROW / 2
            fig.arrow([(spine_x, mid), (cx - 12, mid)], color=GREY, sw=2, head=False)
            last_mid = mid
            tx = cx
            if c.cond:
                fig.text(tx, cy + 2, c.cond, fs=20, color=INK, h=28)
                tx = cx + cond_w + 30
            ly = cy
            if not c.events:
                fig.text(tx, ly + 2, "condition only, no event", fs=20, color=MUTED, h=28)
                right = max(right, tx + text_width("condition only, no event", 20))
                ly += ROW
            for typ, reasons in c.events:
                chip_w = 86 if typ == "Normal" else 98
                if typ == "Warning":
                    fig.pill(tx, ly + 2, chip_w, CHIP_H, "Warning", fill=INK, color=WHITE_TEXT, fs=18, arc=50)
                else:
                    fig.box(tx, ly + 2, chip_w, CHIP_H, "Normal", fs=18, bold=True, fill=PAPER, stroke=GREY,
                            color=INK, arc=50)
                rx = tx + chip_w + 14
                for i, line in enumerate(wrap_reasons(reasons)):
                    fig.text(rx, ly + 2 + i * ROW, line, fs=20, color=INK, bold=True, h=28)
                    right = max(right, rx + text_width(line, 20, bold=True) + 20)
                ly += ROW * max(1, len(wrap_reasons(reasons)))
            for n in c.notes:
                fig.text(tx, ly + 2, n, fs=20, color=MUTED, h=28)
                right = max(right, tx + text_width(n, 20))
                ly += ROW
            cy = ly
    if last_mid is not None:
        fig.arrow([(spine_x, y), (spine_x, last_mid)], color=GREY, sw=2, head=False)
    return cy, right


def tree_figure(fid, name, trees, footer=()):
    # dry run for size
    probe = Fig(fid, name, 10, 10)
    y = Y0
    right = 0
    for t in trees:
        b, r = draw_tree_full(probe, t, X0, y)
        y = b + 50
        right = max(right, r)
    height = y - 50 + 30 + (len(footer) * 26 + 20 if footer else 0)
    width = int(right) + 60
    fig = Fig(fid, name, width, height)
    y = Y0
    for t in trees:
        b, r = draw_tree_full(fig, t, X0, y)
        y = b + 50
    fy = y - 50 + 24
    for i, line in enumerate(footer):
        fig.text(X0 - 20, fy + i * 26, line, fs=20, color=MUTED)
    return fig


def draw_tree_full(fig, root, x, y):
    w = max(260, text_width(root.name, 24, bold=True) + 44)
    fig.box(x, y, w, NODE_H + 8, root.name)
    bottom, right = draw_children(fig, root, x, y + NODE_H + 8, x + w)
    return bottom, right


def alt_text(intro, trees):
    parts = [intro]
    for t in trees:
        parts.append(describe(t))
    return " ".join(parts)


def describe(root):
    out = [f"{root.name}:"]
    for c in root.children:
        out.append(describe_child(c, root))
    return " ".join(out)


def describe_child(c, parent):
    if isinstance(c, Node):
        inner = " ".join(describe_child(g, c) for g in c.children)
        return f"{c.name} emits {inner}" if any(isinstance(g, Leaf) for g in c.children) else f"{c.name}: {inner}"
    if not c.events:
        return f"nothing when {c.cond} (condition only);"
    evs = []
    for typ, reasons in c.events:
        evs.append(f"{typ} {' or '.join(reasons)}")
    s = " and ".join(evs)
    if c.cond:
        s += f" when {c.cond}"
    if c.notes:
        s += " (" + "; ".join(c.notes) + ")"
    return s + ";"
