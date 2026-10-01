"""Animate a timed terminal recording as a looping SVG (standard library only).
Input: JSON [[seconds, text], ...] of writes that use only SGR codes, "\r\x1b[K" and newlines.
Usage: cast2svg.py TITLE COLS PROMPT < rec.json > out.svg"""
import html, json, re, sys
title, cols, prompt = sys.argv[1], int(sys.argv[2]), sys.argv[3]
writes = json.load(sys.stdin)
LEAD, HOLD = 0.8, 4.0  # seconds before the output starts, and on the last screen before the loop restarts
palette = {"fg": "#d4d4d4", "bg": "#1e1e1e", "31": "#f14c4c", "32": "#23d18b", "33": "#e5c07b", "36": "#29b8db", "dim": "#8b8b8b"}

def wrap(line):
    """Split one logical line into screen rows of at most cols characters; escape codes take no room."""
    rows, cur, width = [], "", 0
    for tok in re.split(r"(\x1b\[[0-9;]*m)", line):
        if tok.startswith("\x1b["):
            cur += tok; continue
        for ch in tok:
            if width == cols:
                rows.append(cur); cur, width = "", 0
            cur += ch; width += 1
    return rows + [cur]

# Replay: finished lines with the time they ended, and the current line's text over time.
finished, current, states = [], "", []
for t, text in writes:
    t += LEAD
    i = 0
    while i < len(text):
        if text.startswith("\r\x1b[K", i):
            current = ""; i += 4; continue
        ch = text[i]
        if ch == "\n":
            finished.append((t, current)); current = ""
        elif ch != "\r":
            current += ch
        i += 1
    states.append((t, len(finished), current))
end = writes[-1][0] + LEAD + HOLD
redact = lambda s: re.sub(r"/(?:var|private|tmp)/\S*/claude", "…/claude", s)

rows = [(0.0, "\x1b[36m$\x1b[39m " + prompt)]
row_of_line = []
for t, line in finished:
    row_of_line.append(len(rows))
    rows += [(t, r) for r in wrap(redact(line))]
# The current line sits under the rows finished so far; it changes, so each state gets its own element.
status = []
for k, (t, n, cur) in enumerate(states):
    t_next = states[k + 1][0] if k + 1 < len(states) else end
    if cur and t_next > t:
        row = 1 + sum(len(wrap(redact(l))) for _, l in finished[:n])
        if status and status[-1][2] == cur and status[-1][3] == row and abs(status[-1][1] - t) < 1e-9:
            status[-1][1] = t_next
        else:
            status.append([t, t_next, cur, row])

CW, LH, PAD, TOP = 8.6, 19, 16, 42
total_rows = max(len(rows), max((s[3] + 1 for s in status), default=0))
W, H = int(PAD * 2 + cols * CW), int(TOP + PAD + total_rows * LH)

def spans(text):
    out, bold, dim, color = [], False, False, None
    for tok in re.split(r"(\x1b\[[0-9;]*m)", text):
        m = re.fullmatch(r"\x1b\[([0-9;]*)m", tok)
        if m:
            for code in (m.group(1) or "0").split(";"):
                if code == "0": bold = dim = False; color = None
                elif code == "1": bold = True
                elif code == "2": dim = True
                elif code == "22": bold = dim = False
                elif code == "39": color = None
                elif code in palette: color = code
            continue
        if not tok:
            continue
        attrs = []
        fill = palette[color] if color else (palette["dim"] if dim else None)
        if fill: attrs.append('fill="%s"' % fill)
        if bold: attrs.append('font-weight="bold"')
        body = html.escape(tok).replace(" ", "&#160;")
        out.append("<tspan %s>%s</tspan>" % (" ".join(attrs), body) if attrs else body)
    return "".join(out)

def visible(start, stop):
    """A discrete opacity animation: shown from start to stop in each loop of `end` seconds."""
    a, b = start / end, stop / end
    if a <= 0 and b >= 1:
        return ""
    if a <= 0:
        values, times = "1;0", "0;%.5f" % b
    elif b >= 1:
        values, times = "0;1", "0;%.5f" % a
    else:
        values, times = "0;1;0", "0;%.5f;%.5f" % (a, b)
    return '<animate attributeName="opacity" calcMode="discrete" values="%s" keyTimes="%s" dur="%.2fs" repeatCount="indefinite"/>' % (values, times, end)

out = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">',
       f'<rect width="{W}" height="{H}" rx="8" fill="{palette["bg"]}"/>',
       '<circle cx="20" cy="18" r="6" fill="#ff5f57"/><circle cx="40" cy="18" r="6" fill="#febc2e"/><circle cx="60" cy="18" r="6" fill="#28c840"/>',
       f'<text x="{W / 2:.0f}" y="23" fill="#8b8b8b" font-family="-apple-system, Segoe UI, Helvetica, Arial, sans-serif" font-size="13" text-anchor="middle">{html.escape(title)}</text>',
       f'<g font-family="SF Mono, Menlo, Consolas, DejaVu Sans Mono, Liberation Mono, monospace" font-size="14" fill="{palette["fg"]}">']
for i, (t, text) in enumerate(rows):
    y = TOP + PAD + i * LH - 4
    anim = visible(t, end)
    out.append(f'<text x="{PAD}" y="{y}"' + (' opacity="0">' + anim if anim else ">") + spans(text) + "</text>")
for start, stop, cur, row in status:
    y = TOP + PAD + row * LH - 4
    anim = visible(start, stop)
    out.append(f'<text x="{PAD}" y="{y}"' + (' opacity="0">' + anim if anim else ">") + spans(redact(cur)) + "</text>")
out.append("</g></svg>")
print("\n".join(out))
print(f"{len(rows)} rows, {len(status)} status states, loop {end:.1f}s", file=sys.stderr)
