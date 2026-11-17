#!/usr/bin/env python3
"""Generates the Iris app icon as SVG (hyad night style, same iris geometry
as IrisShutter.swift). Render to PNGs with render_icon.js; build-app.sh turns
the iconset into AppIcon.icns with iconutil.

Usage: make_icon.py OUT.svg [--small]
"""
import math
import sys

# hyad night tokens
SKY, SKY_RAISED, SKY_HIGH = "#0a0e13", "#10161d", "#18212b"
NEATLINE, NEATLINE_STRONG = "#27323f", "#66788c"
STARLIGHT, CLASS_A = "#ece7dd", "#bdd1ff"


def iris_paths(cx, cy, R, op, n):
    """Blade and seam paths: port of IrisGeometry (IrisShutter.swift)."""
    step = 2 * math.pi / n
    ap = R * 1.45 * op
    spin = op * step * 1.2
    tw = math.radians(115 - 55 * op)
    rim = R * 1.06

    def seam(i):
        a = spin + i * step
        e = a + tw
        p0 = (cx + ap * math.cos(a), cy + ap * math.sin(a))
        p1 = (cx + rim * math.cos(e), cy + rim * math.sin(e))
        c1 = (p0[0] + 0.55 * R * math.cos(a + math.pi / 2), p0[1] + 0.55 * R * math.sin(a + math.pi / 2))
        c2 = (p1[0] - 0.35 * R * math.cos(e), p1[1] - 0.35 * R * math.sin(e))
        return p0, c1, c2, p1

    f = lambda p: f"{p[0]:.2f},{p[1]:.2f}"
    blades, seams = [], []
    for i in range(n):
        p0, c1, c2, p1 = seam(i)
        q0, d1, d2, q1 = seam(i + 1)
        blades.append(f"M{f(p0)} C{f(c1)} {f(c2)} {f(p1)} A{rim:.2f},{rim:.2f} 0 0 1 {f(q1)} "
                      f"C{f(d2)} {f(d1)} {f(q0)} Z")
        seams.append(f"M{f(p0)} C{f(c1)} {f(c2)} {f(p1)}")
    return blades, seams


def icon_svg(small=False):
    S = 1024
    # macOS icon grid: 824px body centred, continuous-corner radius ~185px.
    body, inset, radius = 824, 100, 185
    cx = cy = S / 2
    n = 6 if small else 9
    R = 318 if small else 286            # blade area radius
    ring = 50 if small else 42
    op = 0.30 if small else 0.34         # partly open: the light is the brand
    seam_w = 16 if small else 6
    blades, seams = iris_paths(cx, cy, R, op, n)

    out = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{S}" height="{S}" viewBox="0 0 {S} {S}">',
           '<defs>',
           f'<clipPath id="disc"><circle cx="{cx}" cy="{cy}" r="{R}"/></clipPath>',
           f'<clipPath id="body"><rect x="{inset}" y="{inset}" width="{body}" height="{body}" rx="{radius}"/></clipPath>',
           '</defs>',
           # Plate: flat sky, 1px-style neatline frame (scaled), no gradients.
           f'<g clip-path="url(#body)"><rect x="{inset}" y="{inset}" width="{body}" height="{body}" fill="{SKY}"/>']
    if not small:
        # Registration marks in the plate corners (hyad Plate signature).
        k, m = 26, inset + 92
        for x, y, dx, dy in [(m, m, 1, 1), (S - m, m, -1, 1), (m, S - m, 1, -1), (S - m, S - m, -1, -1)]:
            out.append(f'<path d="M{x - k} {y} H{x + k} M{x} {y - k} V{y + k}" stroke="{NEATLINE_STRONG}" stroke-width="4" fill="none"/>')
    out.append('</g>')
    out.append(f'<rect x="{inset + 2}" y="{inset + 2}" width="{body - 4}" height="{body - 4}" rx="{radius - 2}" fill="none" stroke="{NEATLINE}" stroke-width="4"/>')
    # Ring band.
    out.append(f'<circle cx="{cx}" cy="{cy}" r="{R + ring / 2}" fill="none" stroke="{SKY_RAISED}" stroke-width="{ring}"/>')
    # Behind the blades: the light (flat class-A disc + halo), as when access is granted.
    out.append(f'<g clip-path="url(#disc)"><rect width="{S}" height="{S}" fill="{SKY}"/>'
               f'<circle cx="{cx}" cy="{cy}" r="{R}" fill="{CLASS_A}" fill-opacity="0.14"/>'
               f'<circle cx="{cx}" cy="{cy}" r="{R * 0.40:.1f}" fill="{CLASS_A}" fill-opacity="0.22"/>'
               f'<circle cx="{cx}" cy="{cy}" r="{R * (0.24 if small else 0.2):.1f}" fill="{CLASS_A}"/>')
    # Blades: two steel tones so the shutter reads at a glance.
    for i, b in enumerate(blades):
        out.append(f'<path d="{b}" fill="{"#2a3644" if i % 2 == 0 else "#1c2530"}"/>')
    for s in seams:
        out.append(f'<path d="{s}" fill="none" stroke="#8796a8" stroke-width="{seam_w}" stroke-linecap="round"/>')
    out.append('</g>')
    # Ring edges + chart ticks at the cardinal points.
    out.append(f'<circle cx="{cx}" cy="{cy}" r="{R}" fill="none" stroke="{NEATLINE_STRONG}" stroke-width="{seam_w + 1}"/>')
    out.append(f'<circle cx="{cx}" cy="{cy}" r="{R + ring}" fill="none" stroke="{NEATLINE}" stroke-width="{seam_w}"/>')
    mid = R + ring / 2
    for a in (0, math.pi / 2, math.pi, 3 * math.pi / 2):
        x1, y1 = cx + (mid - ring * 0.35) * math.cos(a), cy + (mid - ring * 0.35) * math.sin(a)
        x2, y2 = cx + (mid + ring * 0.35) * math.cos(a), cy + (mid + ring * 0.35) * math.sin(a)
        out.append(f'<path d="M{x1:.1f} {y1:.1f} L{x2:.1f} {y2:.1f}" stroke="{NEATLINE_STRONG}" stroke-width="{seam_w + 1}"/>')
    out.append('</svg>')
    return "\n".join(out)


if __name__ == "__main__":
    open(sys.argv[1], "w").write(icon_svg(small="--small" in sys.argv))
