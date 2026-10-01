"""Render terminal output with SGR codes as an SVG terminal window, wrapping lines at COLS like a terminal.
Usage: ansi2svg.py TITLE COLS < in > out.svg (standard library only)."""
import html, re, sys
title, cols = sys.argv[1], int(sys.argv[2])
palette = {"fg": "#d4d4d4", "bg": "#1e1e1e", "31": "#f14c4c", "32": "#23d18b", "33": "#e5c07b", "36": "#29b8db", "dim": "#8b8b8b"}
tokens = re.split(r"(\x1b\[[0-9;]*m)", sys.stdin.read().rstrip("\n"))
# Wrap into screen lines of at most cols characters; escape codes take no room.
lines, cur, width = [], [], 0
for tok in tokens:
    if tok.startswith("\x1b["):
        cur.append(tok); continue
    for ch in tok:
        if ch == "\n":
            lines.append(cur); cur, width = [], 0; continue
        if width == cols:
            lines.append(cur); cur, width = [], 0
        cur.append(ch); width += 1
lines.append(cur)
CW, LH, PAD, TOP = 8.6, 19, 16, 42
maxw = max(sum(1 for t in l if not t.startswith("\x1b[")) for l in lines)
W, H = int(PAD * 2 + max(maxw, 40) * CW), int(TOP + PAD + len(lines) * LH)
out = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">',
       f'<rect width="{W}" height="{H}" rx="8" fill="{palette["bg"]}"/>',
       '<circle cx="20" cy="18" r="6" fill="#ff5f57"/><circle cx="40" cy="18" r="6" fill="#febc2e"/><circle cx="60" cy="18" r="6" fill="#28c840"/>',
       f'<text x="{W / 2:.0f}" y="23" fill="#8b8b8b" font-family="-apple-system, Segoe UI, Helvetica, Arial, sans-serif" font-size="13" text-anchor="middle">{html.escape(title)}</text>',
       f'<g font-family="SF Mono, Menlo, Consolas, DejaVu Sans Mono, Liberation Mono, monospace" font-size="14" fill="{palette["fg"]}">']
bold = dim = False
color = None
for i, line in enumerate(lines):
    y = TOP + PAD + i * LH - 4
    spans, text = [], ""
    def flush():
        global text
        if not text:
            return
        attrs = []
        fill = palette[color] if color else (palette["dim"] if dim else None)
        if fill: attrs.append('fill="%s"' % fill)
        if bold: attrs.append('font-weight="bold"')
        body = html.escape(text).replace(" ", "&#160;")
        spans.append("<tspan %s>%s</tspan>" % (" ".join(attrs), body) if attrs else body)
        text = ""
    for tok in line:
        m = re.fullmatch(r"\x1b\[([0-9;]*)m", tok)
        if not m:
            text += tok; continue
        flush()
        for code in (m.group(1) or "0").split(";"):
            if code == "0": bold = dim = False; color = None
            elif code == "1": bold = True
            elif code == "2": dim = True
            elif code == "22": bold = dim = False
            elif code == "39": color = None
            elif code in palette: color = code
    flush()
    out.append(f'<text x="{PAD}" y="{y}">{"".join(spans)}</text>')
out.append("</g></svg>")
print("\n".join(out))
