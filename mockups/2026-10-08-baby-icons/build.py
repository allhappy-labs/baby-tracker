#!/usr/bin/env python3
"""Generates svg/<id>.svg (standalone, template-ready) and index.html.

Run: python3 build.py   (then render index.html with headless Chrome, see README)
Every element is self-contained black art; the app tints by alpha only.
"""
import math, pathlib

HERE = pathlib.Path(__file__).parent
K = 'fill="#000"'
S = 'fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round"'

# ---- shared baby base ------------------------------------------------------
# Strokes: 2.2 head + curl, 1.8 secondary details, 1.2 blush. No partial opacity.
CX, CY, R = 12, 13.6, 7.9          # head; outline stroke 2.2 -> outer r 9.0

def f(v):
    s = f"{v:.2f}".rstrip("0").rstrip(".")
    return s if s not in ("-0", "") else "0"

def base(mouth=True, ears=True, dx=0.0, open_chin=False, blush=False):
    x = lambda v: f(v + dx)
    if open_chin:   # head outline opened at the chin so a pacifier ring can sit in the gap
        face = f'<path d="M{x(15.8)} 20.53A7.9 7.9 0 1 0 {x(8.2)} 20.53" {S} stroke-width="2.2"/>'
    else:
        face = f'<circle cx="{x(CX)}" cy="{CY}" r="{R}" {S} stroke-width="2.2"/>'
    ear = (f'<path d="M{x(3.9)} 11.9a1.75 1.75 0 0 0 0 3.5z" {K}/>'
           f'<path d="M{x(20.1)} 11.9a1.75 1.75 0 0 1 0 3.5z" {K}/>')
    eyes = (f'<ellipse cx="{x(9.1)}" cy="13.9" rx="1.15" ry="1.4" {K}/>'
            f'<ellipse cx="{x(14.9)}" cy="13.9" rx="1.15" ry="1.4" {K}/>')
    # blush: short opaque dashes, >=0.4 inside the head stroke
    bl = f'<path d="M{x(6.9)} 16.5h1.3M{x(15.8)} 16.5h1.3" {S} stroke-width="1.2"/>'
    smile = f'<path d="M{x(10.5)} 17.2q1.5 1.25 3 0" {S} stroke-width="1.8"/>'
    return face + (ear if ears else "") + eyes + (bl if blush else "") + (smile if mouth else "")

def circ(cx, cy, r, cw=True):
    """Circle as path subpath; cw=True -> clockwise on screen (for nonzero holes)."""
    sw = 1 if cw else 0
    return (f"M{f(cx-r)} {f(cy)}a{f(r)} {f(r)} 0 1 {sw} {f(2*r)} 0"
            f"a{f(r)} {f(r)} 0 1 {sw} {f(-2*r)} 0z")

# ---- features ---------------------------------------------------------------
# boy.1: one fat open baby curl springing from the crown (open hook, not a ball)
CURL = (f'<path d="M11.7 5.4C10.9 3.2 12.2 1.3 14.3 1.3c1.7 0 2.6 1.5 1.9 2.9" '
        f'{S} stroke-width="2.2"/>')

def spiky_hair():
    # filled hair cap: spiky crown outside the head, zig-zag fringe on the forehead
    pts = []
    a0, a1 = 196, 344                       # degrees, measured with y up
    n = 9
    for i in range(n + 1):
        a = math.radians(a0 + (a1 - a0) * i / n)
        r = 9.4 if i % 2 else 8.6
        if i in (0, n):
            r = 8.6
        pts.append((CX + r * math.cos(a), CY + r * math.sin(a)))
    top = " ".join(f"{f(x)} {f(y)}" for x, y in pts)
    fringe = "17.8 10.6 16.4 8.6 14.8 10.1 13.2 8.3 11.6 9.9 10 8.2 8.4 9.8 7 8.4 6 10.4"
    return f'<path d="M{top} L{fringe}Z" {K} stroke="#000" stroke-width=".6" stroke-linejoin="round"/>'

# boy.3: whole glyph shifted left by CAP_DX so the brim does not drag the weight right
CAP_DX = -0.6
def cap(dx=CAP_DX):
    x = lambda v: f(v + dx)
    return (f'<path d="M{x(3.9)} 10.2C{x(4.3)} 5.6 {x(7.6)} 3.3 {x(12)} 3.3s7.7 2.3 8.1 6.9z" {K}/>'
            f'<circle cx="{x(12)}" cy="3.1" r="1.25" {K}/>'
            f'<path d="M{x(11)} 9.1h11.3a1.25 1.25 0 0 1 0 2.5H{x(11)}z" {K}/>')

# boy.4: small oval shield over the mouth, ring hanging in the opened chin
PACI = (f'<ellipse cx="12" cy="16.85" rx="2.4" ry="1.1" {K}/>'
        f'<circle cx="12" cy="20.7" r="1.5" {S} stroke-width="1.5"/>')

def xf(pts, cx, cy, ang, sc):
    a = math.radians(ang); c, s_ = math.cos(a), math.sin(a)
    return [(cx + sc*(x*c - y*s_), cy + sc*(x*s_ + y*c)) for x, y in pts]

def bow(cx, cy, ang, sc):
    out = ""
    for m in (1, -1):
        P = [(0,0),(-5.2*m,-2.7),(-6.4*m,-3.2),(-7.1*m,-2.3),(-7.1*m,-1.2),
             (-7.1*m,1.2),(-7.1*m,2.3),(-6.4*m,3.2),(-5.2*m,2.7)]
        q = xf(P, cx, cy, ang, sc)
        g = lambda i: f"{f(q[i][0])} {f(q[i][1])}"
        out += (f'<path d="M{g(0)}L{g(1)}C{g(2)} {g(3)} {g(4)}L{g(5)}C{g(6)} {g(7)} {g(8)}Z" {K}/>')
    out += f'<circle cx="{f(cx)}" cy="{f(cy)}" r="{f(2.0*sc)}" {K}/>'
    return out

BOW = bow(15.5, 5.5, -20, 0.9)

def teardrop(tx, ty, cx, cy, r):
    d = math.hypot(cx-tx, cy-ty); base_ = math.atan2(ty-cy, tx-cx); off = math.acos(r/d)
    p1 = (cx + r*math.cos(base_+off), cy + r*math.sin(base_+off))
    p2 = (cx + r*math.cos(base_-off), cy + r*math.sin(base_-off))
    return (f'<path d="M{f(tx)} {f(ty)}L{f(p1[0])} {f(p1[1])}A{r} {r} 0 1 1 {f(p2[0])} {f(p2[1])}Z" {K}/>')

PART_L = f'<path d="M11.3 4.9C7.6 5.2 5 7.6 4.4 10.9c2.4-.4 5.3-2.2 6.9-4.4z" {K}/>'
PART_R = f'<path d="M12.7 4.9c3.7.3 6.3 2.7 6.9 6-2.4-.4-5.3-2.2-6.9-4.4z" {K}/>'
PIGTAILS = (PART_L + PART_R
            + teardrop(5.2, 6.6, 3.0, 9.1, 2.1) + teardrop(18.8, 6.6, 21.0, 9.1, 2.1))

def flower(cx, cy, d=2.5, r=1.3, core=1.6, hole=0.8):
    # one compound path, nonzero winding: petals + core clockwise, centre hole anticlockwise
    sub = ""
    for i in range(5):
        a = math.radians(-90 + 72 * i)
        sub += circ(cx + d*math.cos(a), cy + d*math.sin(a), r)
    sub += circ(cx, cy, core) + circ(cx, cy, hole, cw=False)
    return f'<path d="{sub}" {K} fill-rule="nonzero"/>'

HEADBAND = (f'<path d="M4.6 10.4C7 7.4 17 7.4 19.4 10.4" {S} stroke-width="1.8"/>'
            + flower(5.2, 5.6))

# girl.4: big bun sitting on the crown, filled hair-tie pinch, short parted fringe
BUN = (f'<circle cx="12" cy="3" r="2.9" {K}/>'
       f'<path d="M9.1 4.2Q12 5 14.9 4.2V5.9Q12 5.2 9.1 5.9z" {K}/>'
       f'<path d="M11.3 5C8 5.2 5.6 7.6 5 10.3 7.5 10 10.4 8.4 11.3 6.3z" {K}/>'
       f'<path d="M12.7 5c3.3.2 5.7 2.6 6.3 5.3-2.5-.3-5.4-1.9-6.3-4z" {K}/>')

# neutral: three newborn hair strands fanning from the crown (no curl, no accessory)
WISP = f'<path d="M12 5.4V2.9M10.2 5.7 9.4 3.5M13.8 5.7l.8-2.2" {S} stroke-width="1.8"/>'

ICONS = {
    "boy.1":  ("Curl",             "#6FB2FF", base() + CURL),
    "boy.2":  ("Spiky fringe",     "#4FD8B0", base() + spiky_hair()),
    "boy.3":  ("Cap with brim",    "#FFD84D", base(dx=CAP_DX) + cap()),
    "boy.4":  ("Pacifier",         "#8A84FF", base(mouth=False, open_chin=True) + PACI),
    "girl.1": ("Big bow",          "#FF86B8", base() + BOW),
    "girl.2": ("Pigtail puffs",    "#FFB35C", base() + PIGTAILS),
    "girl.3": ("Headband + flower","#D89BFF", base() + HEADBAND),
    "girl.4": ("Top-knot bun",     "#FF6B6B", base() + BUN),
    "neutral":("Newborn wisps",    "#9AA4C9", base() + WISP),
}

def svg(body):
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">{body}</svg>\n'

def main():
    d = HERE / "svg"; d.mkdir(exist_ok=True)
    for k, (_, _, body) in ICONS.items():
        (d / f"{k}.svg").write_text(svg(body))
    ic = lambda k, px: (f'<span class="ic" style="width:{px}px;height:{px}px;background:{ICONS[k][1]};'
                        f'-webkit-mask-image:url(svg/{k}.svg);mask-image:url(svg/{k}.svg)"></span>')
    cells = "".join(
        f'<div class="cell">{ic(k,96)}<div class="sm">{ic(k,32)}{ic(k,28)}</div>'
        f'<b>{k}</b><i>{n}<br>{t}</i></div>' for k, (n, t, _) in ICONS.items())
    hdr = lambda k, name: f'<div class="hdr">{ic(k,32)}<span>{name}</span><span class="chev">⌄</span></div>'
    headers = ("".join(hdr(k, n) for k, n in [("boy.1","Max"),("boy.2","Leo")]),
               "".join(hdr(k, n) for k, n in [("girl.1","Sophia"),("girl.2","Mia")]),
               hdr("neutral", "Mira"))
    names = {"boy.1":"Max","boy.2":"Leo","boy.3":"Noah","boy.4":"Theo","girl.1":"Sophia",
             "girl.2":"Mia","girl.3":"Ava","girl.4":"Lily","neutral":"Mira"}
    rows = "".join(f'<div class="row">{ic(k,28)}<span>{names[k]}</span><em>{k}</em></div>' for k in ICONS)
    raw = "".join(f'<img src="svg/{k}.svg" width="32" height="32">' for k in ICONS)
    html = f"""<!doctype html><meta charset="utf-8"><title>Baby icons v2</title>
<style>
body{{background:#0D1224;color:#F3F5FD;font:14px -apple-system,system-ui;margin:0;padding:16px 20px}}
h1{{font-size:18px;margin:0 0 4px}} h2{{font-size:13px;color:#9AA4C9;font-weight:600;margin:16px 0 8px;text-transform:uppercase;letter-spacing:.05em}}
p{{margin:0;color:#9AA4C9;font-size:12px}}
.ic{{display:inline-block;-webkit-mask-size:contain;mask-size:contain;-webkit-mask-repeat:no-repeat;mask-repeat:no-repeat;vertical-align:middle}}
.grid{{display:grid;grid-template-columns:repeat(5,124px);gap:10px 8px}}
.cell{{text-align:center}} .cell .sm{{display:flex;gap:14px;justify-content:center;align-items:center;margin:6px 0 2px}}
.cell b{{display:block;font-size:12px}} .cell i{{display:block;font-style:normal;color:#9AA4C9;font-size:11px}}
.cards{{display:flex;gap:10px}}
.card{{background:#151B33;border-radius:14px;padding:6px 12px;flex:1}}
.hdr{{display:flex;align-items:center;gap:8px;font:700 22px -apple-system,system-ui;height:44px}}
.chev{{color:#9AA4C9;font-size:15px;font-weight:600;margin-top:-6px}}
.rows{{display:grid;grid-template-columns:repeat(3,1fr);gap:0 10px;background:#151B33;border-radius:14px;padding:4px 12px}}
.row{{display:flex;align-items:center;gap:10px;height:44px;font-size:17px;border-bottom:1px solid #232B4A}}
.row em{{margin-left:auto;color:#9AA4C9;font-size:11px;font-style:normal}}
.raw{{background:#F3F5FD;border-radius:10px;padding:6px;display:inline-flex;gap:10px}}
</style>
<h1>Baby icons — redraw v2</h1>
<p>Rendered from the shipped files svg/&lt;id&gt;.svg as alpha masks (= template tint). 96&nbsp;px, 32&nbsp;pt, 28&nbsp;pt.</p>
<div class="grid">{cells}</div>
<h2>Header · 32 pt</h2>
<div class="cards"><div class="card">{headers[0]}</div><div class="card">{headers[1]}</div><div class="card">{headers[2]}</div></div>
<h2>List rows · 28 pt</h2>
<div class="rows">{rows}</div>
<h2>Raw SVG files (untinted, &lt;img&gt;)</h2>
<div class="raw">{raw}</div>
"""
    (HERE / "index.html").write_text(html)

if __name__ == "__main__":
    main()
