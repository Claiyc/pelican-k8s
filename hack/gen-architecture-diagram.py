#!/usr/bin/env python3
"""Generate the README architecture diagram (light and dark SVGs).

Usage: python3 hack/gen-architecture-diagram.py

The SVGs are written to docs/images, next to the README that shows them.
"""
from pathlib import Path

OUT = Path(__file__).resolve().parent.parent / "docs" / "images"
SANS = "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, sans-serif"
MONO = "ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace"

THEMES = {
 "light": dict(bg="#ffffff", frame="#f6f8fa", frameStroke="#d0d7de", text="#1f2328", muted="#59636e",
   box="#ffffff", line="#59636e",
   panel=("#eef2f6","#57606a"), gw=("#eaf2ff","#2563eb"), cr=("#f3f4f6","#4b5563"),
   op=("#f3edff","#7c3aed"), ag=("#e7f7f4","#0f9488"), sh=("#fff1e7","#e8590c"),
   pvc=("#f3f4f6","#6b7280"), podfill="#ffffff"),
 "dark": dict(bg="#0d1117", frame="#151b23", frameStroke="#3d444d", text="#e6edf3", muted="#9198a1",
   box="#0d1117", line="#9198a1",
   panel=("#1b222c","#8b949e"), gw=("#10213f","#58a6ff"), cr=("#1b1f26","#9ca3af"),
   op=("#221a3a","#a78bfa"), ag=("#0d2a28","#2dd4bf"), sh=("#33200f","#fb923c"),
   pvc=("#1b1f26","#9ca3af"), podfill="#0d1117"),
}

def gen(t):
    T = THEMES[t]
    o = []
    W, H = 1000, 560
    def a(s): o.append(s)
    def marker(name, color):
        a(f'<marker id="{name}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,1 L9,5 L0,9 z" fill="{color}"/></marker>')
    def box(x,y,w,h,col,title,sub=None,sub2=None,mono=False,dash=False,r=10,tsize=17):
        fill, stroke = col
        d = ' stroke-dasharray="6 4"' if dash else ''
        a(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{r}" fill="{fill}" stroke="{stroke}" stroke-width="1.6"{d}/>')
        cx = x + w/2
        lines = 1 + (sub is not None) + (sub2 is not None)
        ty = y + h/2 - (lines-1)*10 + 6
        fam = MONO if mono else SANS
        a(f'<text x="{cx}" y="{ty}" text-anchor="middle" font-family="{fam}" font-size="{tsize}" font-weight="600" fill="{T["text"]}">{title}</text>')
        if sub: a(f'<text x="{cx}" y="{ty+21}" text-anchor="middle" font-family="{SANS}" font-size="12.5" fill="{T["muted"]}">{sub}</text>')
        if sub2: a(f'<text x="{cx}" y="{ty+38}" text-anchor="middle" font-family="{SANS}" font-size="12.5" fill="{T["muted"]}">{sub2}</text>')
    def label(x,y,s,anchor="middle",color=None,size=12.5,weight=500,italic=False):
        st = ' font-style="italic"' if italic else ''
        a(f'<text x="{x}" y="{y}" text-anchor="{anchor}" font-family="{SANS}" font-size="{size}" font-weight="{weight}" fill="{color or T["muted"]}"{st}>{s}</text>')
    def pill(x,y,s,col):
        w = len(s)*6.6+18
        a(f'<rect x="{x}" y="{y-13}" width="{w}" height="19" rx="9.5" fill="{T["bg"]}" stroke="{col}" stroke-width="1"/>')
        a(f'<text x="{x+w/2}" y="{y+1}" text-anchor="middle" font-family="{SANS}" font-size="11" font-weight="600" fill="{col}">{s}</text>')
    def path(d,color,m_end=True,m_start=False,dash=False,width=1.6):
        ms = f' marker-start="url(#m-{color[1:]})"' if m_start else ''
        me = f' marker-end="url(#m-{color[1:]})"' if m_end else ''
        dd = ' stroke-dasharray="5 4"' if dash else ''
        a(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="{width}"{dd}{ms}{me}/>')

    a(f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-labelledby="title desc">')
    a('<title id="title">pelican-k8s architecture</title>')
    a('<desc id="desc">The Panel calls the gateway over the Wings API. The gateway writes spec and status to the GameServer custom resource and proxies console, files and SFTP to the agent pod. The operator watches the GameServer and reconciles the agent pod and the game pod. The agent drives the shim in the game pod over the shim protocol; the shim runs the egg entrypoint. Both pods share the server PVC.</desc>')
    a('<defs>')
    # dict.fromkeys drops duplicate colours and keeps the order, so the output is reproducible.
    for c in dict.fromkeys([T["line"], T["gw"][1], T["op"][1], T["sh"][1], T["ag"][1], T["cr"][1]]):
        marker("m-"+c[1:], c)
    a('</defs>')
    a(f'<rect width="{W}" height="{H}" rx="14" fill="{T["bg"]}"/>')

    # cluster frame
    a(f'<rect x="236" y="22" width="744" height="516" rx="14" fill="{T["frame"]}" stroke="{T["frameStroke"]}" stroke-width="1.4" stroke-dasharray="7 5"/>')
    label(256, 46, "KUBERNETES CLUSTER", anchor="start", size=11.5, weight=700)

    # Panel (outside)
    box(24, 72, 168, 80, T["panel"], "Pelican Panel", "unmodified")
    # gateway
    box(290, 72, 220, 80, T["gw"], "gateway", "looks like one Wings node")
    # GameServer CR
    box(730, 72, 220, 80, T["cr"], "GameServer", "custom resource", mono=True, tsize=16)
    # operator
    box(730, 206, 220, 64, T["op"], "operator", "the only reconciler")

    # Panel -> gateway
    path("M192,112 L288,112", T["gw"][1])
    label(240, 103, "Wings API", color=T["text"])
    # gateway -> CR
    path("M510,112 L728,112", T["cr"][1])
    label(619, 103, "spec · status", color=T["text"])
    # CR <-> operator
    path("M840,154 L840,204", T["op"][1], m_start=True)
    label(852, 184, "watch", anchor="start", color=T["text"])

    # agent pod
    ax, ay, aw, ah = 276, 330, 258, 132
    a(f'<rect x="{ax}" y="{ay}" width="{aw}" height="{ah}" rx="12" fill="{T["podfill"]}" stroke="{T["ag"][1]}" stroke-width="1.4"/>')
    label(ax+14, ay+22, "AGENT POD", anchor="start", size=11, weight=700, color=T["ag"][1])
    pill(ax+aw-112, ay+20, "always running", T["ag"][1])
    box(ax+14, ay+36, aw-28, 82, T["ag"], "agent", "Wings as a library", "files · SFTP · console · backups")

    # game pod
    gx, gy, gw_, gh = 604, 330, 346, 132
    a(f'<rect x="{gx}" y="{gy}" width="{gw_}" height="{gh}" rx="12" fill="{T["podfill"]}" stroke="{T["sh"][1]}" stroke-width="1.4" stroke-dasharray="6 4"/>')
    label(gx+14, gy+22, "GAME POD", anchor="start", size=11, weight=700, color=T["sh"][1])
    pill(gx+gw_-106, gy+20, "only while on", T["sh"][1])
    box(gx+14, gy+36, 146, 82, T["sh"], "shim", "PID 1 · PTY, signals,", "exit codes, stats")
    box(gx+186, gy+36, 146, 82, ("transparent", T["sh"][1]), "egg", "entrypoint", "unmodified image", dash=True)
    path(f"M{gx+160},{gy+77} L{gx+184},{gy+77}", T["sh"][1])

    # gateway -> agent (data path)
    path("M372,152 L372,364", T["gw"][1], width=1.8)
    label(384, 190, "proxied data path", anchor="start", color=T["text"])
    label(384, 207, "console · files · SFTP", anchor="start")

    # agent <-> shim
    path(f"M{ax+aw-14},{gy+77} L{gx+12},{gy+77}", T["ag"][1], m_start=True)
    label((ax+aw-14+gx+14)/2, gy+68, "shim", color=T["text"])
    label((ax+aw-14+gx+14)/2, gy+96, "protocol", color=T["text"])

    # operator -> pods (reconcile)
    path("M730,238 L476,238 L476,328", T["op"][1], dash=True)
    path("M840,270 L840,328", T["op"][1], dash=True)
    label(600, 229, "reconciles", color=T["text"])
    label(852, 303, "creates per run", anchor="start", color=T["text"])

    # PVC
    px, py, pw = 520, 494, 170
    ry = 6
    fill, stroke = T["pvc"]
    a(f'<path d="M{px},{py} L{px},{py+26} A{pw/2},{ry} 0 0 0 {px+pw},{py+26} L{px+pw},{py} A{pw/2},{ry} 0 0 0 {px},{py} z" fill="{fill}" stroke="{stroke}" stroke-width="1.4"/>')
    a(f'<ellipse cx="{px+pw/2}" cy="{py}" rx="{pw/2}" ry="{ry}" fill="{fill}" stroke="{stroke}" stroke-width="1.4"/>')
    label(px+pw/2, py+22, "PVC · server files", color=T["text"], size=12.5, weight=600)
    path(f"M405,462 L405,{py+13} L{px-2},{py+13}", T["line"], m_end=False, width=1.3)
    path(f"M777,462 L777,{py+13} L{px+pw+2},{py+13}", T["line"], m_end=False, width=1.3)

    a('</svg>')
    return "\n".join(o)

for t in THEMES:
    (OUT / f"architecture-{t}.svg").write_text(gen(t) + "\n")
