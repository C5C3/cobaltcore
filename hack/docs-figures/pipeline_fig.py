# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Lay out a sub-reconciler pipeline figure in the geometry of service-reconciler-pipeline."""
from figlib import Fig, INK, MUTED, text_width

BOX_W, BOX_H = 270, 56
BOX_X = 240
FRAME_X, FRAME_Y, FRAME_W = 100, 40, 1290
NOTE_X = 530
GROUP_X, GROUP_W = 120, 840
GROUP_COLS = (140, 420, 700)
GROUP_BOX_W = 210
LINE = 26


def pipeline_figure(spec):
    """spec: dict(fid, name, title, subtitle, entries, exit_notes, footer, side)

    entries: list of ("step", label, [notes]) | ("group", title, subtitle, [(label, [notes]), ...])
    side: optional list of (anchor_entry_index, [lines]) drawn right of a group frame.
    """
    fid = spec["fid"]
    # First pass: compute vertical positions.
    y = 150
    layout = []  # (kind, y, height, ...)
    for e in spec["entries"]:
        if e[0] == "step":
            notes = e[2]
            n = len(notes)
            if n >= 3:
                y += 11  # the notes reach above the box
            note_top = y + 14 if n <= 1 else y + 27 - 13 * n
            layout.append(("step", y, e[1], notes, note_top))
            y = y + 92 + (11 if n >= 3 else 0)
        else:
            members = e[3]
            wide = max(text_width(m[0], 24) for m in members) > 200 or \
                max(text_width(n, 20) for m in members for n in m[1]) > 260
            per_row = 2 if wide else 3
            rows = [members[i:i + per_row] for i in range(0, len(members), per_row)]
            row_heights = [134] * len(rows)
            last_notes = max(len(m[1]) for m in rows[-1])
            frame_h = 93 + 134 * (len(rows) - 1) + max(108, 62 + LINE * last_notes + 18)
            layout.append(("group", y, e[1], e[2], rows, row_heights, frame_h))
            y = y + frame_h + 36
    last_bottom = y - 36
    frame_h = last_bottom + 24 - FRAME_Y
    frame_bottom = FRAME_Y + frame_h
    exit_y = frame_bottom + 40
    footer_y = frame_bottom + 180
    footer = spec.get("footer", [])
    height = footer_y + LINE * max(len(footer), 1) + 42
    width = spec.get("width", 1808)
    fig = Fig(fid, spec["name"], width, height)

    fig.frame(FRAME_X, FRAME_Y, FRAME_W, frame_h)
    fig.text(FRAME_X + 19, FRAME_Y + 13, spec["title"], fs=26, bold=True, color=INK)
    fig.text(FRAME_X + 19, FRAME_Y + 50, spec["subtitle"], fs=20, color=MUTED)

    first_cy = None
    prev_bottom_center = None
    for item in layout:
        if item[0] == "step":
            _, sy, label, notes, note_top = item
            cx = BOX_X + BOX_W / 2
            if first_cy is None:
                first_cy = sy + BOX_H / 2
                fig.dot(50, first_cy)
                fig.arrow([(60, first_cy), (BOX_X, first_cy)])
            elif prev_bottom_center is not None:
                fig.arrow([prev_bottom_center, (cx, sy)])
            fig.box(BOX_X, sy, BOX_W, BOX_H, label)
            for i, note in enumerate(notes):
                fig.text(NOTE_X, note_top + i * LINE, note, fs=20, color=MUTED)
            prev_bottom_center = (cx, sy + BOX_H)
        else:
            _, gy, gtitle, gsub, rows, row_heights, gh = item
            cx = BOX_X + BOX_W / 2
            if prev_bottom_center is not None:
                fig.arrow([prev_bottom_center, (cx, gy)])
            fig.frame(GROUP_X, gy, GROUP_W, gh)
            fig.text(GROUP_X + 19, gy + 13, gtitle, fs=26, bold=True, color=INK)
            fig.text(GROUP_X + 19, gy + 50, gsub, fs=20, color=MUTED)
            ry = gy + 93
            per_row = max(len(r) for r in rows)
            cols = GROUP_COLS if per_row == 3 else (140, 560)
            bw = GROUP_BOX_W if per_row == 3 else 350
            for row, rh in zip(rows, row_heights):
                for col, (label, notes) in zip(cols, row):
                    fig.box(col, ry, bw, BOX_H, label)
                    for i, note in enumerate(notes):
                        fig.text(col, ry + 62 + i * LINE, note, fs=20, color=MUTED)
                ry += rh
            prev_bottom_center = (cx, gy + gh)

    for anchor, lines in spec.get("side", []):
        item = layout[anchor]
        gy = item[1]
        base = gy + 73.5
        for i, line in enumerate(lines):
            fig.text(990, base + i * LINE, line, fs=20, color=MUTED)

    # Exit: updateStatus
    ux, uy, uw, uh = 1330, exit_y, 220, 56
    fig.box(ux, uy, uw, uh, "updateStatus")
    for i, note in enumerate(spec.get("exit_notes", [])):
        fig.text(ux, uy + 64 + i * LINE, note, fs=20, color=MUTED)
    mid_y = uy + uh / 2
    fig.arrow([prev_bottom_center, (prev_bottom_center[0], mid_y), (ux, mid_y)])
    fig.text(820, mid_y - 36, "every entry returned zero", fs=20, color=INK, h=24)
    fig.arrow([(FRAME_X + FRAME_W, first_cy), (1440, first_cy), (1440, uy)])
    fig.text(1456, first_cy + 40, "first requeue or error", fs=20, color=INK, h=24)

    for i, line in enumerate(footer):
        fig.text(40, footer_y + i * LINE, line, fs=20, color=MUTED)
    return fig


def seq(label, *notes):
    return ("step", label, list(notes))


def group(title, members, subtitle="every member starts; the shortest requeue is returned"):
    return ("group", title, subtitle, [(m[0], list(m[1:])) for m in members])
