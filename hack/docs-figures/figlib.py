# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Emit a draw.io source and its SVG export in the house style of docs/diagrams/.

The SVG mirrors what draw.io exports for these shapes: a background rect,
rounded rects, texts on a baseline at y + h/2 + 0.36 * fontSize, polylines with
a filled block arrow head, circles. Dash patterns are the draw.io pattern times
the stroke width.
"""
import math
from xml.sax.saxutils import escape

FONT = "IBM Plex Sans"
FONT_SOURCE = "fontSource=https%3A%2F%2Ffonts.googleapis.com%2Fcss%3Ffamily%3DIBM%2BPlex%2BSans;"
BG = "#F6F5F1"

# Palette (docs/contributing/architecture-diagrams.md, Visual conventions)
INK = "#2B3440"
INK_DARK = "#17202B"
MUTED = "#55606E"
LINE = "#3A4452"
GREY = "#7D8794"
FRAME_FILL = "#EEF0F3"
PAPER = "#FDFCF9"
BLUE = "#2457C5"
BLUE_TEXT = "#1D47A6"
BLUE_FILL = "#E7EDF9"
GREEN = "#2F8A57"
GREEN_TEXT = "#1E6B40"
GREEN_FILL = "#E6F1EA"
MAGENTA = "#B0387F"
MAGENTA_TEXT = "#8A2B63"
MAGENTA_FILL = "#F7E8F0"
RED = "#C6343C"
RED_TEXT = "#A3262E"
RED_FILL = "#F8E8E9"
OCHRE = "#B38A00"
OCHRE_TEXT = "#6F5600"
OCHRE_FILL = "#FAF3DA"
GL_FILL = "#E9ECEF"
WHITE_TEXT = "#FBFAF7"


def text_width(s, fs, bold=False):
    """Rough advance width of s in IBM Plex Sans at font size fs."""
    w = 0.0
    for ch in s:
        if ch in "iljtfrI.,:;'()[]|! ":
            w += 0.32
        elif ch in "mwMW":
            w += 0.82
        elif ch.isupper():
            w += 0.68
        elif ch.isdigit():
            w += 0.58
        else:
            w += 0.57
    if bold:
        w *= 1.07
    return int(round(w * fs)) + 4


def xa(s):
    """Escape for an XML attribute value."""
    return escape(s, {'"': "&quot;"})


class Fig:
    def __init__(self, fid, name, width, height):
        self.fid = fid
        self.name = name
        self.w = width
        self.h = height
        self.cells = []
        self.svg = []
        self.n = 0

    def _id(self):
        self.n += 1
        return f"{self.fid}-{self.n}"

    # -- shapes -------------------------------------------------------------

    def frame(self, x, y, w, h, arc=26, fill=FRAME_FILL, stroke=GREY, sw=2, dashed=None):
        style = f"rounded=1;absoluteArcSize=1;arcSize={arc};fillColor={fill};strokeColor={stroke};strokeWidth={sw};"
        if dashed:
            style += f"dashed=1;dashPattern={dashed};"
        style += "whiteSpace=wrap;html=1;"
        self.cells.append(
            f'<mxCell id="{self._id()}" value="" style="{style}" vertex="1" parent="1">'
            f'<mxGeometry x="{x}" y="{y}" width="{w}" height="{h}" as="geometry"/></mxCell>')
        extra = ""
        if dashed:
            a, b = dashed.split()
            extra = f' stroke-dasharray="{int(a) * sw} {int(b) * sw}"'
        self.svg.append(
            f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{arc // 2}" fill="{fill}" stroke="{stroke}" stroke-width="{sw}"{extra}/>')

    def text(self, x, y, s, fs=20, bold=False, color=MUTED, align="left", w=None, h=None):
        """A free text. x is the left edge (align left), the centre (center) or the right edge (right)."""
        if w is None:
            w = text_width(s, fs)
        if h is None:
            h = int(round(fs * 1.4)) if fs >= 24 else 28
            if fs == 26:
                h = 34
        gx = {"left": x, "center": x - w / 2, "right": x - w}[align]
        gx = int(round(gx))
        style = (f"text;html=1;strokeColor=none;fillColor=none;fontFamily={FONT};{FONT_SOURCE}"
                 f"fontSize={fs};fontStyle={1 if bold else 0};fontColor={color};align={align};"
                 f"verticalAlign=middle;spacing=0;whiteSpace=wrap;")
        self.cells.append(
            f'<mxCell id="{self._id()}" value="{xa(s)}" style="{style}" vertex="1" parent="1">'
            f'<mxGeometry x="{gx}" y="{y}" width="{w}" height="{h}" as="geometry"/></mxCell>')
        self._svg_text(gx, y, w, h, s, fs, bold, color, align)

    def _svg_text(self, x, y, w, h, s, fs, bold, color, align):
        base = round(y + h / 2 + 0.36 * fs, 2)
        base = int(base) if float(base).is_integer() else base
        weight = 700 if bold else 400
        if align == "center":
            tx = x + w / 2
            tx = int(tx) if float(tx).is_integer() else tx
            anchor = ' text-anchor="middle"'
        elif align == "right":
            tx = x + w
            anchor = ' text-anchor="end"'
        else:
            tx = x
            anchor = ""
        self.svg.append(
            f'<text x="{tx}" y="{base}" font-size="{fs}" font-weight="{weight}" fill="{color}"{anchor}>{escape(s)}</text>')

    def box(self, x, y, w, h, label, fs=24, bold=True, fill=PAPER, stroke=LINE, color=INK, arc=12, sw=2, dashed=None):
        style = f"rounded=1;absoluteArcSize=1;arcSize={arc};fillColor={fill};strokeColor={stroke};strokeWidth={sw};"
        if dashed:
            style += f"dashed=1;dashPattern={dashed};"
        style += (f"whiteSpace=wrap;html=1;fontFamily={FONT};{FONT_SOURCE}fontSize={fs};"
                  f"fontStyle={1 if bold else 0};fontColor={color};align=center;verticalAlign=middle;spacing=0;")
        self.cells.append(
            f'<mxCell id="{self._id()}" value="{xa(label)}" style="{style}" vertex="1" parent="1">'
            f'<mxGeometry x="{x}" y="{y}" width="{w}" height="{h}" as="geometry"/></mxCell>')
        extra = ""
        if dashed:
            a, b = dashed.split()
            extra = f' stroke-dasharray="{int(a) * sw} {int(b) * sw}"'
        self.svg.append(
            f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{arc // 2}" fill="{fill}" stroke="{stroke}" stroke-width="{sw}"{extra}/>')
        if label:
            self._svg_text(x, y, w, h, label, fs, bold, color, "center")

    def pill(self, x, y, w, h, label, fill=INK, color=WHITE_TEXT, fs=24, arc=40):
        """A filled pill without a stroke: a chip or a state."""
        style = (f"rounded=1;absoluteArcSize=1;arcSize={arc};fillColor={fill};strokeColor=none;whiteSpace=wrap;html=1;"
                 f"fontFamily={FONT};{FONT_SOURCE}fontSize={fs};fontStyle=1;fontColor={color};align=center;verticalAlign=middle;spacing=0;")
        self.cells.append(
            f'<mxCell id="{self._id()}" value="{xa(label)}" style="{style}" vertex="1" parent="1">'
            f'<mxGeometry x="{x}" y="{y}" width="{w}" height="{h}" as="geometry"/></mxCell>')
        self.svg.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{arc // 2}" fill="{fill}"/>')
        self._svg_text(x, y, w, h, label, fs, True, color, "center")

    def dot(self, cx, cy, r=10, fill=INK_DARK):
        style = f"ellipse;aspect=fixed;fillColor={fill};strokeColor=none;whiteSpace=wrap;html=1;"
        self.cells.append(
            f'<mxCell id="{self._id()}" value="" style="{style}" vertex="1" parent="1">'
            f'<mxGeometry x="{fmt(cx - r)}" y="{fmt(cy - r)}" width="{2 * r}" height="{2 * r}" as="geometry"/></mxCell>')
        self.svg.append(f'<circle cx="{fmt(cx)}" cy="{fmt(cy)}" r="{r}" fill="{fill}"/>')

    def badge(self, cx, cy, label, r=16, fill=INK_DARK, color=PAPER, fs=20):
        """A numbered step badge: a filled circle with a bold label."""
        style = (f"ellipse;aspect=fixed;fillColor={fill};strokeColor=none;fontColor={color};fontStyle=1;fontSize={fs};"
                 f"whiteSpace=wrap;html=1;fontFamily={FONT};{FONT_SOURCE}align=center;verticalAlign=middle;spacing=0;")
        self.cells.append(
            f'<mxCell id="{self._id()}" value="{xa(label)}" style="{style}" vertex="1" parent="1">'
            f'<mxGeometry x="{cx - r}" y="{cy - r}" width="{2 * r}" height="{2 * r}" as="geometry"/></mxCell>')
        self.svg.append(f'<circle cx="{fmt(cx)}" cy="{fmt(cy)}" r="{r}" fill="{fill}"/>')
        self._svg_text(cx - r, cy - r, 2 * r, 2 * r, label, fs, True, color, "center")

    def arrow(self, points, color=LINE, sw=3, dashed=None, head=True, open_head=False):
        """A polyline through points with a block head at the last point."""
        pts = [(float(x), float(y)) for x, y in points]
        style = f"edgeStyle=none;rounded=0;strokeColor={color};strokeWidth={sw};"
        if head:
            style += "endArrow=open;endFill=0;" if open_head else "endArrow=block;endFill=1;"
        else:
            style += "endArrow=none;"
        if dashed:
            style += f"dashed=1;dashPattern={dashed};"
        style += "whiteSpace=wrap;html=1;"
        src, tgt = pts[0], pts[-1]
        mid = "".join(f'<mxPoint x="{fmt(x)}" y="{fmt(y)}"/>' for x, y in pts[1:-1])
        arr = f'<Array as="points">{mid}</Array>' if mid else ""
        self.cells.append(
            f'<mxCell id="{self._id()}" value="" style="{style}" edge="1" parent="1"><mxGeometry relative="1" as="geometry">'
            f'<mxPoint x="{fmt(src[0])}" y="{fmt(src[1])}" as="sourcePoint"/><mxPoint x="{fmt(tgt[0])}" y="{fmt(tgt[1])}" as="targetPoint"/>'
            f'{arr}</mxGeometry></mxCell>')
        extra = ""
        if dashed:
            a, b = dashed.split()
            extra = f' stroke-dasharray="{int(a) * sw} {int(b) * sw}"'
        if head:
            (x1, y1), (x2, y2) = pts[-2], pts[-1]
            dx, dy = x2 - x1, y2 - y1
            length = math.hypot(dx, dy) or 1.0
            ux, uy = dx / length, dy / length
            back = 10.5 if not open_head else 0
            line_pts = pts[:-1] + [(x2 - ux * back, y2 - uy * back)]
            svg_pts = " ".join(f"{fmt(x)},{fmt(y)}" for x, y in line_pts)
            self.svg.append(f'<polyline points="{svg_pts}" fill="none" stroke="{color}" stroke-width="{sw}"{extra}/>')
            bx, by = x2 - ux * 15, y2 - uy * 15
            px, py = -uy * 7.2, ux * 7.2
            if open_head:
                self.svg.append(
                    f'<polyline points="{fmt(bx + px)},{fmt(by + py)} {fmt(x2)},{fmt(y2)} {fmt(bx - px)},{fmt(by - py)}" '
                    f'fill="none" stroke="{color}" stroke-width="{sw}"/>')
            else:
                self.svg.append(
                    f'<polygon points="{fmt(x2)},{fmt(y2)} {fmt(bx + px)},{fmt(by + py)} {fmt(bx - px)},{fmt(by - py)}" fill="{color}"/>')
        else:
            svg_pts = " ".join(f"{fmt(x)},{fmt(y)}" for x, y in pts)
            self.svg.append(f'<polyline points="{svg_pts}" fill="none" stroke="{color}" stroke-width="{sw}"{extra}/>')

    # -- output -------------------------------------------------------------

    def drawio(self):
        head = (f'<mxfile host="drawio" type="device">\n'
                f'  <diagram id="{self.fid}" name="{xa(self.name)}">\n'
                f'    <mxGraphModel dx="{self.w}" dy="{self.h}" grid="1" gridSize="10" guides="1" tooltips="1" connect="1" '
                f'arrows="1" fold="1" page="0" pageScale="1" pageWidth="{self.w}" pageHeight="{self.h}" background="{BG}" '
                f'math="0" shadow="0">\n      <root>\n        <mxCell id="0"/>\n        <mxCell id="1" parent="0"/>\n')
        body = "".join(f"        {c}\n" for c in self.cells)
        tail = "      </root>\n    </mxGraphModel>\n  </diagram>\n</mxfile>\n"
        return head + body + tail

    def svg_text(self):
        head = (f'<svg xmlns="http://www.w3.org/2000/svg" width="{self.w}" height="{self.h}" viewBox="0 0 {self.w} {self.h}" '
                f'font-family="\'{FONT}\', Arial, Helvetica, sans-serif" role="img" aria-label="{xa(self.name)}">\n'
                f'<title>{escape(self.name)}</title>\n<rect width="{self.w}" height="{self.h}" fill="{BG}"/>\n')
        return head + "\n".join(self.svg) + "\n</svg>\n"

    def write(self, base):
        with open(base + ".drawio", "w", encoding="utf-8") as f:
            f.write(self.drawio())
        with open(base + ".svg", "w", encoding="utf-8") as f:
            f.write(self.svg_text())


def fmt(v):
    v = round(float(v), 2)
    return str(int(v)) if v.is_integer() else str(v)
