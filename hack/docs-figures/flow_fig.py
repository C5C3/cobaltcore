# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Three generic figure layouts: a gate tree (a pass with its exits), a chain of
steps (vertical, with notes and badges), and lanes of horizontal chains."""
from figlib import Fig, INK, INK_DARK, MUTED, GREY, LINE, PAPER, GL_FILL, WHITE_TEXT, text_width

# ---------------------------------------------------------------------------
# Gate tree
# ---------------------------------------------------------------------------
# node(name, note=None, children=[...])      a step of the pass, drawn as a box
# gate(cond, reason, note=None)              an exit: condition -> reason chip, note
# goto(cond, target, note=None)              a branch into a function, drawn bold
# done(text, note=None)                      an outcome without a condition


def node(name, note=None, children=()):
    return ("node", name, note, list(children))


def gate(cond, reason, note=None):
    return ("gate", cond, reason, note)


def goto(cond, target, note=None):
    return ("goto", cond, target, note)


def done(text, note=None):
    return ("done", text, None, note)


INDENT = 70
NODE_H = 48
ROW = 34
CHIP_H = 28


def _leaf_lines(leaf):
    return 1 + (1 if leaf[3] and len(leaf[3]) > 60 else 0)


def _draw_children(fig, children, x, y, right, arrow_targets):
    spine_x = x + 30
    cy = y + 18
    last_mid = None
    cond_w = max([text_width(c[1], 20) for c in children if c[0] in ("gate", "goto") and c[1]] + [0])
    prev_leaf = False
    for c in children:
        cx = x + INDENT
        if c[0] == "node":
            _, name, note, kids = c
            if prev_leaf:
                cy += 12
            prev_leaf = False
            w = max(220, text_width(name, 22, bold=True) + 40)
            fig.box(cx, cy, w, NODE_H, name, fs=22)
            arrow_targets[name] = (cx, cy, w, NODE_H)
            mid = cy + NODE_H / 2
            fig.arrow([(spine_x, mid), (cx, mid)], color=GREY, sw=2, head=False)
            last_mid = mid
            right = max(right, cx + w)
            if note:
                fig.text(cx + w + 16, cy + 10, note, fs=20, color=MUTED, h=28)
                right = max(right, cx + w + 16 + text_width(note, 20))
            if kids:
                bottom, right = _draw_children(fig, kids, cx, cy + NODE_H, right, arrow_targets)
                cy = bottom + 14
            else:
                cy += NODE_H + 18
        else:
            kind, cond, target, note = c
            mid = cy + ROW / 2
            fig.arrow([(spine_x, mid), (cx - 12, mid)], color=GREY, sw=2, head=False)
            last_mid = mid
            tx = cx
            if kind != "done" and cond:
                fig.text(tx, cy + 3, cond, fs=20, color=INK, h=28)
                tx = cx + cond_w + 30
            if kind == "gate":
                w = text_width(target, 18, bold=True) + 24
                fig.box(tx, cy + 3, w, CHIP_H, target, fs=18, bold=True, fill=PAPER, stroke=INK_DARK, color=INK_DARK, arc=50)
                tx += w + 14
            elif kind == "goto":
                fig.arrow([(tx, mid), (tx + 34, mid)])
                tx += 46
                fig.text(tx, cy + 3, target, fs=20, color=INK, bold=True, h=28)
                tx += text_width(target, 20, bold=True) + 14
            else:
                fig.text(tx, cy + 3, cond, fs=20, color=INK, bold=True, h=28)
                tx += text_width(cond, 20, bold=True) + 14
            right = max(right, tx)
            if note:
                fig.text(tx, cy + 3, note, fs=20, color=MUTED, h=28)
                right = max(right, tx + text_width(note, 20))
            cy += ROW
            prev_leaf = True
    if last_mid is not None:
        fig.arrow([(spine_x, y), (spine_x, last_mid)], color=GREY, sw=2, head=False)
    return cy, right


def gate_tree_figure(fid, name, roots, footer=(), links=()):
    """roots: list of (root_name, root_note, children). links: list of
    (from_node_name, to_root_index, label) drawn as a dark arrow."""
    def draw(fig):
        y = 40
        right = 0
        targets = {}
        root_boxes = []
        for rname, rnote, kids in roots:
            w = max(260, text_width(rname, 24, bold=True) + 44)
            fig.box(60, y, w, NODE_H + 8, rname)
            root_boxes.append((60, y, w, NODE_H + 8))
            if rnote:
                fig.text(60 + w + 16, y + 14, rnote, fs=20, color=MUTED, h=28)
                right = max(right, 60 + w + 16 + text_width(rnote, 20))
            right = max(right, 60 + w)
            bottom, right = _draw_children(fig, kids, 60, y + NODE_H + 8, right, targets)
            y = bottom + 60
        for src, dst, label in links:
            sx, sy, sw_, sh = targets[src]
            dx, dy, dw, dh = root_boxes[dst]
            lx = sx + sw_ + 60
            fig.arrow([(sx + sw_, sy + sh / 2), (lx, sy + sh / 2), (lx, dy + dh / 2), (dx + dw, dy + dh / 2)])
            fig.text(lx + 16, sy + sh / 2 + 14, label, fs=20, color=INK, h=24)
            right = max(right, lx + 16 + text_width(label, 20))
        fy = y - 60 + 30
        for i, line in enumerate(footer):
            fig.text(40, fy + i * 26, line, fs=20, color=MUTED)
            right = max(right, 40 + text_width(line, 20))
        return int(right) + 60, fy + 26 * len(footer) + 40

    probe = Fig(fid, name, 10, 10)
    w, h = draw(probe)
    fig = Fig(fid, name, w, h)
    draw(fig)
    return fig


# ---------------------------------------------------------------------------
# Chain of steps
# ---------------------------------------------------------------------------

def chain_figure(fid, name, steps, badges=True, footer=(), box_w=None, last_grey=False, title=None):
    """steps: list of (label, [notes]) drawn top to bottom with arrows."""
    bw = box_w or max(260, max(text_width(s[0], 24, bold=True) for s in steps) + 48)
    x = 100 if badges else 60
    def draw(fig):
        y = 60
        right = x + bw
        if title:
            fig.text(40, 20, title, fs=26, bold=True, color=INK)
        prev = None
        for i, (label, notes) in enumerate(steps):
            n = len(notes)
            if n >= 3:
                y += 13 * (n - 2)
            grey = last_grey and i == len(steps) - 1
            fig.box(x, y, bw, 56, label, fill=GL_FILL if grey else PAPER, stroke=GREY if grey else LINE)
            if badges:
                fig.badge(x - 40, y + 28, str(i + 1))
            if prev is not None:
                fig.arrow([prev, (x + bw / 2, y)])
            top = y + 14 if n <= 1 else y + 27 - 13 * n
            for j, note in enumerate(notes):
                fig.text(x + bw + 20, top + j * 26, note, fs=20, color=MUTED)
                right = max(right, x + bw + 20 + text_width(note, 20))
            prev = (x + bw / 2, y + 56)
            y += 92 + (13 * (n - 2) if n >= 3 else 0)
        fy = y - 36 + 30
        for i, line in enumerate(footer):
            fig.text(40, fy + i * 26, line, fs=20, color=MUTED)
            right = max(right, 40 + text_width(line, 20))
        return int(right) + 60, fy + 26 * len(footer) + 30

    probe = Fig(fid, name, 10, 10)
    w, h = draw(probe)
    fig = Fig(fid, name, w, h)
    draw(fig)
    return fig


# ---------------------------------------------------------------------------
# Lanes of horizontal chains
# ---------------------------------------------------------------------------

def lanes_figure(fid, name, lanes, width=1760, footer=()):
    """lanes: list of (title, subtitle, [step labels]). Each lane wraps its chain."""
    def draw(fig):
        y = 40
        for title, subtitle, steps in lanes:
            fig.text(40, y, title, fs=26, bold=True, color=INK)
            if subtitle:
                fig.text(40, y + 36, subtitle, fs=20, color=MUTED)
            y += 76 if subtitle else 44
            x = 60
            prev = None
            row_top = y
            for i, label in enumerate(steps):
                w = text_width(label, 22, bold=True) + 36
                if x + w > width - 40:
                    # wrap: elbow from the previous box down to the next row
                    nx = 60
                    ny = y + 56 + 40
                    if prev is not None:
                        fig.arrow([prev, (prev[0] + 30, prev[1]), (prev[0] + 30, prev[1] + 28 + 20),
                                   (nx - 30, prev[1] + 28 + 20), (nx - 30, ny + 28), (nx, ny + 28)])
                        prev = None
                    x, y = nx, ny
                fig.box(x, y, w, 56, label, fs=22)
                fig.badge(x + 16, y, str(i + 1), r=14, fs=16)
                if prev is not None:
                    fig.arrow([prev, (x, y + 28)])
                prev = (x + w, y + 28)
                x += w + 50
            y += 56 + 60
        fy = y - 10
        for i, line in enumerate(footer):
            fig.text(40, fy + i * 26, line, fs=20, color=MUTED)
        return width, fy + 26 * len(footer) + 30

    probe = Fig(fid, name, 10, 10)
    w, h = draw(probe)
    fig = Fig(fid, name, w, h)
    draw(fig)
    return fig
