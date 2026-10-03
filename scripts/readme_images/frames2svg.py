"""Animate a timed recording of a live terminal (the dashboard's redraws) as a looping SVG, or draw one moment of it
(standard library only).

Input: JSON [[seconds, text], ...] from TestRecordLiveRun. A small terminal replays it: SGR styles (8, 256 and 24-bit
colors), newlines, cursor up (ESC [ n A), erase to the end of the screen (CR ESC [ J), erase the line (CR ESC [ K);
other modes (the cursor's visibility, synchronized output) are ignored. Each change of the last ROWS screen rows becomes
a frame, shown for as long as it was on screen, at most FPS frames a second; the loop holds the last frame for a few
seconds. Lines that repeat across frames are defined once and reused, so a long recording stays small.

Usage: frames2svg.py TITLE COLS ROWS PROMPT [--from SECONDS] [--fps N] [--still SECONDS|last] < rec.json > out.svg
  --from   starts the animation there (the screen as it was then): skip the checks before the first run
  --still  draws the screen at that moment (or the last), without animation; ROWS 0 is every line"""
import html, json, re, sys

args = sys.argv[1:]
opts = {}
while len(args) > 4:
    opts[args[-2]] = args[-1]
    args = args[:-2]
title, cols, rows_shown, prompt = args[0], int(args[1]), int(args[2]), args[3]
start_at = float(opts.get("--from", 0))
fps = float(opts.get("--fps", 5))
still = opts.get("--still")
HOLD = 4.0
writes = json.load(sys.stdin)
palette = {"fg": "#d4d4d4", "bg": "#1e1e1e", "31": "#f14c4c", "32": "#23d18b", "33": "#e5c07b", "34": "#3b8eea",
           "35": "#d670d6", "36": "#29b8db", "dim": "#8b8b8b"}
home = re.compile(r"/(?:Users|home)/[^/\s]+")
temp = re.compile(r"/(?:var|private|tmp)/\S*/(claude|data)\b")


def redact(s):
    return home.sub("~", temp.sub(r"…/\1", s))


def xterm256(n):
    if n < 16:
        return palette.get(str(30 + n % 8), palette["fg"])
    if n < 232:
        n -= 16
        level = lambda v: 0 if v == 0 else 55 + v * 40
        return "#%02x%02x%02x" % (level(n // 36), level(n // 6 % 6), level(n % 6))
    return "#%02x%02x%02x" % ((8 + (n - 232) * 10,) * 3)


# The terminal: rows of text with their SGR codes, and the cursor's row. Lines are kept whole (a line wider than the
# terminal is cut when drawn: the dashboard never writes one).
screen, cur = ["\x1b[36m$\x1b[39m " + prompt, ""], 1
token = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\r|\n|[^\x1b\r\n]+|\x1b")


def feed(text):
    global screen, cur
    for tok in token.findall(text):
        if tok == "\n":
            cur += 1
            if cur == len(screen):
                screen.append("")
        elif tok == "\r":
            pass
        elif tok.startswith("\x1b["):
            kind, params = tok[-1], tok[2:-1]
            if kind == "A":
                cur = max(cur - int(params or 1), 0)
            elif kind == "J":
                screen = screen[:cur] + [""]
            elif kind == "K":
                screen[cur] = ""
            elif kind == "m":
                screen[cur] += tok
        elif tok != "\x1b":
            screen[cur] += tok


def view():
    lines = list(screen)
    while len(lines) > 1 and lines[-1] == "":
        lines.pop()
    if rows_shown > 0:
        lines = lines[-rows_shown:]
    return tuple(redact(l) for l in lines)


frames = []  # (time, lines)
for t, text in writes:
    if still not in (None, "last") and t > float(still):
        break  # the screen at that moment, not a frame merged with later ones
    feed(text)
    if t < start_at:
        continue
    v = view()
    t = t - start_at
    if frames and frames[-1][1] == v:
        continue
    if frames and t - frames[-1][0] < 1 / fps:  # too soon: this state replaces the last one
        frames[-1] = (frames[-1][0], v)
        continue
    frames.append((t, v))
if still is not None:
    frames = [(0, view())]

CW, LH, PAD, TOP = 8.6, 19, 16, 42
height_rows = max(len(f[1]) for f in frames)
W, H = int(PAD * 2 + cols * CW), int(TOP + PAD + height_rows * LH)


def spans(text):
    out, bold, dim, color = [], False, False, None
    for tok in re.split(r"(\x1b\[[0-9;]*m)", text):
        m = re.fullmatch(r"\x1b\[([0-9;]*)m", tok)
        if m:
            codes = (m.group(1) or "0").split(";")
            if codes[:2] == ["38", "2"] and len(codes) == 5:
                color = "#%02x%02x%02x" % tuple(int(c) for c in codes[2:])
                continue
            if codes[:2] == ["38", "5"] and len(codes) == 3:
                color = xterm256(int(codes[2]))
                continue
            for code in codes:
                if code == "0": bold = dim = False; color = None
                elif code == "1": bold = True
                elif code == "2": dim = True
                elif code == "22": bold = dim = False
                elif code == "39": color = None
                elif code in palette: color = palette[code]
            continue
        if not tok:
            continue
        attrs = []
        fill = color if color else (palette["dim"] if dim else None)
        if fill: attrs.append('fill="%s"' % fill)
        if bold: attrs.append('font-weight="bold"')
        body = html.escape(tok).replace(" ", "&#160;")
        out.append("<tspan %s>%s</tspan>" % (" ".join(attrs), body) if attrs else body)
    return "".join(out)


ids = {}
for _, lines in frames:
    for line in lines:
        if line and line not in ids:
            ids[line] = "l%d" % len(ids)
end = frames[-1][0] + HOLD if len(frames) > 1 else 1
out = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">']
if len(frames) > 1:
    css = []
    for i, (t, _) in enumerate(frames):
        t_next = frames[i + 1][0] if i + 1 < len(frames) else end
        a, b = 100 * t / end, 100 * t_next / end
        if i == 0:
            css.append(f"@keyframes a{i}{{0%{{opacity:1}} {b:.3f}%{{opacity:0}} 100%{{opacity:0}}}}")
        else:
            css.append(f"@keyframes a{i}{{0%{{opacity:0}} {a:.3f}%{{opacity:1}} {b:.3f}%{{opacity:0}} 100%{{opacity:0}}}}")
        css.append(f".f{i}{{opacity:0;animation:a{i} {end:.2f}s step-end infinite}}")
    out.append("<style>" + "\n".join(css) + "</style>")
out += [f'<rect width="{W}" height="{H}" rx="8" fill="{palette["bg"]}"/>',
        '<circle cx="20" cy="18" r="6" fill="#ff5f57"/><circle cx="40" cy="18" r="6" fill="#febc2e"/><circle cx="60" cy="18" r="6" fill="#28c840"/>',
        f'<text x="{W / 2:.0f}" y="23" fill="#8b8b8b" font-family="-apple-system, Segoe UI, Helvetica, Arial, sans-serif" font-size="13" text-anchor="middle">{html.escape(title)}</text>',
        f'<g font-family="SF Mono, Menlo, Consolas, DejaVu Sans Mono, Liberation Mono, monospace" font-size="14" fill="{palette["fg"]}">']
if len(frames) > 1:
    out.append("<defs>")
    for line, i in ids.items():
        out.append(f'<text id="{i}" x="0" y="0">{spans(line)}</text>')
    out.append("</defs>")
    for n, (_, lines) in enumerate(frames):
        out.append(f'<g class="f{n}">')
        for k, line in enumerate(lines):
            if line:
                out.append(f'<use href="#{ids[line]}" x="{PAD}" y="{TOP + PAD + k * LH - 4}"/>')
        out.append("</g>")
else:
    for k, line in enumerate(frames[0][1]):
        out.append(f'<text x="{PAD}" y="{TOP + PAD + k * LH - 4}">{spans(line)}</text>')
out.append("</g></svg>")
print("\n".join(out))
print(f"{len(frames)} frame(s), {len(ids)} distinct lines, loop {end:.1f}s", file=sys.stderr)
