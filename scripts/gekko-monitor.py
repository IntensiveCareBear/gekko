#!/usr/bin/env python3
"""
gekko-monitor — a tactical HUD for the Gekko stack.

A fighter-cockpit style terminal display. Pulls from the liveview API
(default http://localhost:8082) and renders framed instrument panels:
account P&L, a caution-annunciator strip, component health, breen's
consensus engine, the Gordon persona fleet (LLM stats + skip-reason
histograms), kahuna data feeds, and a live decision/target log.

On a wide terminal (>= 140 cols) panels pair up side by side and the
display grows an equity chart, position board, trade tape and daily P&L;
the target log stretches to fill whatever height is left, so a portrait
monitor gets used top to bottom.

Usage:
    scripts/gekko-monitor.py                 # refresh every 5s
    scripts/gekko-monitor.py -n 2            # refresh every 2s
    scripts/gekko-monitor.py --once          # one frame, then exit
    scripts/gekko-monitor.py --host http://localhost:8082
    scripts/gekko-monitor.py --feed          # longer target log
    scripts/gekko-monitor.py --no-color
    scripts/gekko-monitor.py --width 160     # cap the display width
    scripts/gekko-monitor.py --theme bladerunner   # amber ESPER terminal

No dependencies beyond the Python 3 standard library.
"""
import argparse
import contextlib
import io
import json
import os
import re
import sys
import time
import urllib.request
from collections import Counter, defaultdict
from datetime import datetime

# ── ANSI ─────────────────────────────────────────────────────────────────────
RESET = "\033[0m"
BOLD = "\033[1m"
DIM = "\033[2m"
REV = "\033[7m"
BLINK = "\033[5m"


def c(code):
    return f"\033[{code}m"


# Phosphor palette — HUD green primary, amber caution, red warning.
GREEN, AMBER, RED = c(92), c(93), c(91)
DGREEN = c(32)          # dim green chrome
CYAN, MAG, WHT, GRY = c(96), c(95), c(97), c(90)
BLK = c(30)

USE_COLOR = sys.stdout.isatty()
WIDTH_CAP = 240
TWO_COL = 140   # at or above this width, row() puts panels side by side
_W = None       # width override while row() renders a column

ANSI_RE = re.compile(r"\033\[[0-9;]*m")


# Themes remap the palette above at paint time, so every panel follows along.
THEMES = {
    "hud": {},
    # LA 2019: sodium-amber phosphor, teal glass, neon-pink alarms.
    "bladerunner": {
        GREEN: c("38;5;214"), DGREEN: c("38;5;130"), AMBER: c("38;5;44"),
        RED: c("38;5;198"), CYAN: c("38;5;80"), MAG: c("38;5;205"),
        WHT: c("38;5;223"), GRY: c("38;5;137"),
    },
}
TAGLINES = {
    "hud": ("  TACTICAL TRADING DISPLAY", "ctrl-c to disengage"),
    "bladerunner": ("  ESPER · TYRELL CORP · NEXUS-6 TRADING UNIT", "ctrl-c · like tears in rain"),
}
THEME = "hud"


def paint(s, *codes):
    if not USE_COLOR or not codes:
        return s
    remap = THEMES[THEME]
    return "".join(remap.get(x, x) for x in codes) + s + RESET


def strip_ansi(s):
    return ANSI_RE.sub("", s)


def vis(s):
    return len(strip_ansi(s))


def pad(s, w):
    return s + " " * max(0, w - vis(s))


def box_width():
    if _W:
        return _W
    try:
        cols = os.get_terminal_size().columns
    except OSError:
        cols = 100
    return max(64, min(cols, WIDTH_CAP))


# ── formatting ───────────────────────────────────────────────────────────────
def money(x, width=0):
    try:
        x = float(x)
    except (TypeError, ValueError):
        x = 0.0
    s = f"{'+' if x >= 0 else '-'}${abs(x):,.2f}"
    if width:
        s = s.rjust(width)
    return paint(s, GREEN if x >= 0 else RED)


def pct(x, good_high=True, width=0):
    try:
        x = float(x)
    except (TypeError, ValueError):
        x = 0.0
    s = f"{x:+.2f}%"
    if width:
        s = s.rjust(width)
    up = x >= 0
    col = (GREEN if up else RED) if good_high else (RED if up else GREEN)
    return paint(s, col)


def gauge(frac, width=18, col=GREEN, danger=False):
    """HUD tape gauge with end caps and a moving fill."""
    frac = max(0.0, min(1.0, frac))
    filled = int(round(frac * width))
    fillcol = RED if danger else col
    body = paint("▮" * filled, fillcol) + paint("▯" * (width - filled), DGREEN)
    return paint("▐", DGREEN) + body + paint("▌", DGREEN)


def ago(unix):
    if not unix:
        return paint("--:--", GRY)
    d = max(0, time.time() - float(unix))
    if d < 60:
        s = f"T-{int(d)}s"
    elif d < 3600:
        s = f"T-{int(d/60)}m"
    elif d < 86400:
        s = f"T-{d/3600:.1f}h"
    else:
        s = f"T-{d/86400:.1f}d"
    col = GREEN if d < 300 else (AMBER if d < 1800 else RED)
    return paint(s, col)


def dur(seconds):
    seconds = int(seconds or 0)
    d, seconds = divmod(seconds, 86400)
    h, seconds = divmod(seconds, 3600)
    m, _ = divmod(seconds, 60)
    if d:
        return f"{d}d{h:02d}h"
    if h:
        return f"{h}h{m:02d}m"
    return f"{m}m"


def tile(label, state):
    """Annunciator light. state: 'ok' | 'caution' | 'warn' | 'off'."""
    col = {"ok": GREEN, "caution": AMBER, "warn": RED, "off": GRY}[state]
    if state == "off":
        return paint(f" {label} ", GRY, DIM)
    return paint(f" {label} ", BLK, REV, col)


# ── framed panels ────────────────────────────────────────────────────────────
def panel(title, lines, accent=DGREEN, tab=GREEN):
    W = box_width()
    ttl = paint(f" {title} ", BOLD, tab)
    dashes = W - 5 - vis(ttl)
    out = [
        paint("┌─", accent) + paint("┤", accent) + ttl + paint("├", accent)
        + paint("─" * max(0, dashes), accent) + paint("┐", accent)
    ]
    inner = W - 4
    for ln in lines:
        out.append(paint("│ ", accent) + pad(ln, inner) + paint(" │", accent))
    out.append(paint("└" + "─" * (W - 2) + "┘", accent))
    return "\n".join(out)


def rule(ch="─"):
    return paint(ch * box_width(), DGREEN)


# ── data fetch ───────────────────────────────────────────────────────────────
def fetch(host, path, default=None):
    try:
        req = urllib.request.Request(host + path, headers={"Accept": "application/json"})
        with urllib.request.urlopen(req, timeout=4) as r:
            return json.load(r)
    except Exception:
        return default


def snapshot(host):
    return {
        "status": fetch(host, "/api/status", {}),
        "account": fetch(host, "/api/account", {}),
        "personas": fetch(host, "/api/personas", []),
        "gordons": fetch(host, "/api/gordons", {}),
        "book": fetch(host, "/api/book_metrics", {}),
        "ledger": fetch(host, "/api/ledger/stats", {}),
        "trades": fetch(host, "/api/trades", []),
        "positions": fetch(host, "/api/positions", []),
        "curve": fetch(host, "/api/equity_curve", []),
    }


def comp_by_name(status):
    return {x.get("component"): x for x in status.get("components", [])}


# ── HUD banner + master caution ──────────────────────────────────────────────
def render_banner(acct, status):
    W = box_width()
    breen = comp_by_name(status).get("breen", {}).get("stats", {})
    mode = "PAPER" if breen.get("dry_mode") else paint("LIVE", BOLD, RED)
    appr = "AUTO" if breen.get("auto_approve") else "MAN"
    left = paint("◄", DGREEN) + paint("⊕", BOLD, GREEN) + paint("►", DGREEN)
    title = paint("G E K K O", BOLD, GREEN) + paint(TAGLINES[THEME][0], DGREEN)
    flags = f"{paint('MODE', GRY)} {mode}  {paint('APPR', GRY)} {appr}"
    line = f"{left}  {title}"
    inner = W - 4  # "║ " + content + " ║"
    body = pad(line, inner - vis(flags)) + flags
    print(paint("╔" + "═" * (W - 2) + "╗", GREEN))
    print(paint("║ ", GREEN) + pad(body, inner) + paint(" ║", GREEN))
    print(paint("╚" + "═" * (W - 2) + "╝", GREEN))


def render_annunciators(snap):
    status = snap["status"]
    acct = snap["account"] or {}
    comps = comp_by_name(status)
    g = comps.get("gordon", {}).get("stats", {})
    k = comps.get("kahuna", {}).get("stats", {})

    # EXEC — every core component reachable?
    core_ok = all(
        comps.get(n, {}).get("reachable") and comps.get(n, {}).get("status") == "ok"
        for n in ("breen", "broker-worker", "gordon")
    )
    exec_state = "ok" if core_ok else "warn"

    # RISK — drawdown thresholds
    dd = float(acct.get("drawdown_pct", 0) or 0)
    risk_state = "warn" if dd > 5 else ("caution" if dd > 1 else "ok")

    # P&L — net vs start
    nav = float(acct.get("nav", 0) or 0)
    start = float(acct.get("starting_capital", 0) or 0)
    pnl_state = "ok" if nav >= start else "caution"

    # LLM — backend errors / fallbacks
    fb = g.get("llm_fallbacks", 0) or 0
    prop = g.get("llm_proposals", 1) or 1
    llm_state = "warn" if g.get("backend_primary_err", 0) else (
        "caution" if fb / max(prop, 1) > 0.05 else "ok")

    # FEED — any data source rate-limited or dark
    src_bad = any(k.get(f"{s}_rate_limited") for s in ("twitter", "reddit", "finnhub", "fred", "weather"))
    src_dark = any(k.get(f"{s}_live") is False for s in ("twitter", "reddit", "finnhub", "fred", "weather"))
    feed_state = "warn" if src_dark else ("caution" if src_bad else "ok")

    # SLOTS — llm slot timeouts
    slot_state = "caution" if g.get("llm_slot_timeouts", 0) else "ok"

    tiles = [
        tile("EXEC", exec_state),
        tile("P&L", pnl_state),
        tile("RISK", risk_state),
        tile("LLM", llm_state),
        tile("FEED", feed_state),
        tile("SLOT", slot_state),
    ]
    label = paint(" CAUTION ", BLK, REV, AMBER) if any(
        s != "ok" for s in (exec_state, risk_state, pnl_state, llm_state, feed_state, slot_state)
    ) else paint(" SYS OK ", BLK, REV, GREEN)
    print("  " + label + "   " + " ".join(tiles))


# ── panels ───────────────────────────────────────────────────────────────────
def panel_account(acct):
    nav = float(acct.get("nav", 0) or 0)
    start = float(acct.get("starting_capital", 0) or 0)
    cash = float(acct.get("cash", 0) or 0)
    rpnl = float(acct.get("realized_pnl", 0) or 0)
    upnl = float(acct.get("unrealized_pnl", 0) or 0)
    dd = float(acct.get("drawdown_pct", 0) or 0)
    maxdd = float(acct.get("max_drawdown_pct", 0) or 0)
    openpos = acct.get("open_positions", 0)
    ret = ((nav - start) / start * 100) if start else 0.0
    deployed = (1 - cash / nav) if nav else 0

    lines = [
        f"{paint('NAV', GRY)}  {paint(f'${nav:>12,.2f}', BOLD, WHT)}   "
        f"{paint('RET', GRY)} {pct(ret, width=8)}   "
        f"{paint('OPEN', GRY)} {paint(str(openpos), BOLD, AMBER)}",
        f"{paint('RPL', GRY)}  {money(rpnl, 13)}   "
        f"{paint('UPL', GRY)} {money(upnl, 8)}   "
        f"{paint('CASH', GRY)} {paint(f'${cash:,.0f}', WHT)}",
        f"{paint('DEPLOY', GRY)} {gauge(deployed, 16)} {deployed*100:>4.0f}%   "
        f"{paint('DD', GRY)} {pct(-dd, good_high=False)}  {paint('MAXDD', GRY)} {pct(-maxdd, good_high=False)}",
    ]
    print(panel("FLIGHT · ACCOUNT", lines, tab=GREEN))


def panel_health(status):
    comps = status.get("components", [])
    healthy = status.get("healthy_count", 0)
    cells = []
    for cp in comps:
        ok = cp.get("reachable") and cp.get("status") == "ok"
        dot = paint("●", GREEN) if ok else paint("◌", RED)
        name = cp.get("component", "?").upper()[:6]
        cells.append(f"{dot} {paint(name, WHT if ok else RED)} {paint(dur(cp.get('uptime_seconds',0)), GRY)}")
    # two rows of three
    lines = []
    for i in range(0, len(cells), 3):
        lines.append("   ".join(cells[i:i + 3]))
    lines.append(paint(f"SYSTEMS {healthy}/{len(comps)} NOMINAL", GRY))
    print(panel("SYSTEMS", lines))


def panel_breen(status, ledger):
    b = comp_by_name(status).get("breen", {}).get("stats", {})
    if not b:
        return
    lines = [
        f"{paint('POS', GRY)} {paint(str(b.get('active_positions',0)), BOLD, AMBER):>3}   "
        f"{paint('ORDERS', GRY)} {paint(str(b.get('consensus_orders',0)), GREEN)}   "
        f"{paint('SKIP', GRY)} {b.get('consensus_skipped',0)}   "
        f"{paint('REJECT', GRY)} {paint(str(b.get('proposals_rejected',0)), RED)}",
        f"{paint('EXEC', GRY)} {b.get('orders_executed',0)}   "
        f"{paint('LEDGER', GRY)} {b.get('ledger_blocks',0)} blk   "
        f"{paint('B/S', GRY)} {paint(str(ledger.get('buys',0)), GREEN)}/{paint(str(ledger.get('sells',0)), RED)}   "
        f"{paint('NOQUOTE', GRY)} {b.get('no_quote_marks',0)}",
    ]
    print(panel("BREEN · WEAPONS/EXEC", lines))


def panel_llm(status):
    g = comp_by_name(status).get("gordon", {}).get("stats", {})
    if not g:
        return
    calls = g.get("backend_calls_today", 0)
    rpd = g.get("backend_max_rpd", 0) or 1
    frac = calls / rpd
    lat = g.get("avg_latency_ms", 0) or 0
    latcol = GREEN if lat < 1500 else (AMBER if lat < 4000 else RED)
    fb = g.get("llm_fallbacks", 0)
    lines = [
        f"{paint('BACKEND', GRY)} {paint(g.get('backend',''), BOLD, WHT)}:{g.get('backend_model','')}  "
        f"{paint('PING', GRY)} {ago(g.get('backend_last_ok_unix'))}",
        f"{paint('QUOTA', GRY)}  {gauge(frac, 20, danger=frac>0.85)} "
        f"{paint(f'{calls}/{rpd}', WHT)} rpd",
        f"{paint('PROP', GRY)} {paint(str(g.get('llm_proposals',0)), WHT)}   "
        f"{paint('FALLBK', GRY)} {paint(str(fb), RED if fb else GRY)}   "
        f"{paint('LAT', GRY)} {paint(f'{lat:.0f}ms', latcol)}   "
        f"{paint('EVT', GRY)} {g.get('events_consumed',0)}   "
        f"{paint('LAG', GRY)} {g.get('kafka_offset_lag',0)}",
    ]
    tos = g.get("llm_slot_timeouts", 0)
    waits = g.get("llm_slot_waits", 0)
    if tos or waits or g.get("llm_slot_fail_open"):
        lines.append(
            f"{paint('SLOTS', GRY)} waits {waits}  wait-avg {g.get('llm_slot_avg_wait_ms',0)}ms  "
            f"timeout {paint(str(tos), RED if tos else GRY)}  fail-open {g.get('llm_slot_fail_open',0)}"
        )
    print(panel("LLM · POWERPLANT", lines))


SKIP_TAGS = ["PREGATE", "FLOOR", "CEIL", "EDGE", "VERIFY", "SKEW", "HOLDINGS"]
TAG_COLORS = {
    "PREGATE": GRY, "FLOOR": CYAN, "CEIL": CYAN, "EDGE": AMBER,
    "VERIFY": MAG, "SKEW": AMBER, "HOLDINGS": WHT, "INFO": DGREEN,
}
STANCE_COLORS = {"bull": GREEN, "bullish": GREEN, "bear": RED, "bearish": RED, "neutral": WHT}


def panel_gordons(status, gordons):
    g = comp_by_name(status).get("gordon", {}).get("stats", {})
    if not g:
        return
    names = {row.get("id"): row.get("name", "") for row in gordons.get("gordons", [])}
    lines = []
    personas = g.get("personas", [])
    for i, p in enumerate(personas):
        pid = p.get("persona", "?")
        name = names.get(pid, "")
        stance = p.get("stance", "")
        risk = p.get("risk_tier", "")
        reach = paint("◆", GREEN) if p.get("reachable") else paint("◇", RED)
        gen = p.get("proposals_generated", 0)
        seen = p.get("llm_proposals", 0)
        skips = p.get("pregate_skips", 0)
        fb = p.get("llm_fallbacks", 0)
        hit = (gen / seen * 100) if seen else 0

        callsign = paint(pid.upper().ljust(13), BOLD, WHT)
        stcol = STANCE_COLORS.get(stance.lower(), WHT)
        lines.append(
            f"{reach} {callsign} {paint(name, DGREEN)}  "
            f"{paint(stance.upper(), stcol)}/{paint(risk.upper(), MAG)}"
        )
        lines.append(
            f"   {paint(p.get('provider','')+':'+p.get('model','?'), GRY)}  "
            f"{paint('FIRE', GRY)} {paint(str(gen), GREEN)}  {paint('SEEN', GRY)} {seen}  "
            f"{paint('HIT', GRY)} {hit:>4.1f}%  {paint('PREGATE', GRY)} {paint(str(skips), AMBER)}  "
            f"{paint('FB', GRY)} {paint(str(fb), RED if fb else GRY)}"
        )
        tagc = Counter()
        for a in p.get("recent_activity", []) or []:
            m = re.search(r"\[([A-Z]+)\]", a)
            if m:
                tagc[m.group(1)] += 1
        if tagc:
            parts = []
            for t in SKIP_TAGS:
                if tagc.get(t):
                    parts.append(paint(f"{t}·{tagc[t]}", TAG_COLORS.get(t, WHT)))
            for t, n in tagc.items():
                if t not in SKIP_TAGS and t != "INFO":
                    parts.append(paint(f"{t}·{n}", WHT))
            if parts:
                lines.append("   " + paint("VETO ", GRY) + " ".join(parts))
        if i != len(personas) - 1:
            lines.append(paint("─" * (box_width() - 4), DGREEN))
    print(panel("GORDON · SQUADRON", lines))


def panel_personas_pnl(personas):
    if not personas:
        return
    lines = [paint(f"{'CALLSIGN':<14}{'TRD':>4}{'WIN':>6}{'REALIZED':>13}{'EXPCT':>10}{'PF':>6}", GRY)]
    for p in sorted(personas, key=lambda x: x.get("realized_pnl", 0), reverse=True):
        wr = p.get("win_rate", 0)
        try:
            wr = float(wr) * 100 if float(wr) <= 1 else float(wr)
        except (TypeError, ValueError):
            wr = 0
        pf = p.get("profit_factor", 0)
        if (pf in (None, 0)) and p.get("losses", 0) == 0 and p.get("wins", 0):
            pfs = "∞"
        else:
            pfs = f"{float(pf or 0):.2f}"
        lines.append(
            f"{p.get('persona','?').upper():<14}"
            f"{p.get('trades',0):>4}"
            f"{wr:>5.0f}% "
            + money(p.get("realized_pnl", 0), 12)
            + " " + money(p.get("expectancy_usd", 0), 9)
            + f"  {pfs:>4}"
        )
    print(panel("SCOREBOARD · PERSONA P&L", lines))


def panel_book(book):
    bands = book.get("entry_bands", [])
    if not bands or not book.get("trades"):
        return
    lines = []
    maxn = max((b.get("trades", 0) for b in bands), default=0) or 1
    barw = max(10, box_width() - 52)
    for band in bands:
        n = band.get("trades", 0)
        if not n:
            continue
        wr = band.get("win_rate", 0)
        wr = wr * 100 if wr <= 1 else wr
        fill = round(n / maxn * barw)
        spark = paint("▰" * fill, CYAN) + paint("▱" * (barw - fill), DGREEN)
        lines.append(
            f"{band.get('label',''):>10} {spark} "
            f"{paint(f'{n}tr', WHT)}  win {wr:>3.0f}%  {money(band.get('realized_pnl',0))}"
        )
    if lines:
        print(panel("BALLISTICS · ENTRY BANDS", lines))


def panel_sources(status):
    k = comp_by_name(status).get("kahuna", {}).get("stats", {})
    if not k:
        return
    sources = ["twitter", "reddit", "finnhub", "fred", "weather"]
    cells = []
    for s in sources:
        live = k.get(f"{s}_live")
        rl = k.get(f"{s}_rate_limited")
        dot = paint("●", GREEN) if live and not rl else (paint("◐", AMBER) if live else paint("◌", RED))
        cells.append(f"{dot} {paint(s.upper()[:4], WHT)} {ago(k.get(f'{s}_last_poll_unix'))}")
    lines = []
    for i in range(0, len(cells), 3):
        lines.append("   ".join(cells[i:i + 3]))
    tb, tdb = k.get("twitter_budget_used"), k.get("twitter_daily_budget")
    extras = []
    if tdb:
        frac = (tb or 0) / tdb
        extras.append(f"{paint('TWTR', GRY)} {gauge(frac, 10)} {tb}/{tdb}")
    if k.get("twitter_last_error"):
        extras.append(paint(f"ERR {k['twitter_last_error']}", AMBER))
    q, proc = k.get("total_quarantined"), k.get("total_processed")
    if proc is not None:
        extras.append(f"{paint('PROC', GRY)} {proc}  {paint('QUAR', GRY)} {paint(str(q), RED if q else GRY)}")
    if extras:
        lines.append("  ".join(extras))
    print(panel("KAHUNA · SENSORS", lines))


def panel_feed(status, limit=10):
    g = comp_by_name(status).get("gordon", {}).get("stats", {})
    rows = []
    for p in g.get("personas", []):
        pid = p.get("persona", "?")[:8]
        for a in p.get("recent_activity", []) or []:
            rows.append((a[:8], pid, a))
    rows.sort(key=lambda r: r[0], reverse=True)
    seen, lines = set(), []
    W = box_width() - 4
    for ts, pid, a in rows:
        if a in seen:
            continue
        seen.add(a)
        m = re.search(r"\[([A-Z]+)\]", a)
        tagcol = TAG_COLORS.get(m.group(1) if m else "", WHT)
        body = re.sub(r"^\d{2}:\d{2}:\d{2}\s*", "", a)
        body = re.sub(r"\[([A-Z]+)\]", lambda mm: paint(mm.group(0), tagcol), body, count=1)
        body = re.sub(r"(POLY-\d{6})\d+", r"\1…", body)
        line = f"{paint('►', GREEN)} {paint(ts[:5], GRY)} {paint(pid.ljust(8)[:8], DIM)} {body}"
        if vis(line) > W:
            line = truncate_visible(line, W - 1) + paint("…", GRY)
        lines.append(line)
        if len(lines) >= limit:
            break
    if lines:
        print(panel("TARGET LOG · LIVE DECISIONS", lines))


def truncate_visible(s, width):
    out, seen, i = [], 0, 0
    while i < len(s) and seen < width:
        m = ANSI_RE.match(s, i)
        if m:
            out.append(m.group(0))
            i = m.end()
            continue
        out.append(s[i])
        seen += 1
        i += 1
    return "".join(out) + (RESET if USE_COLOR else "")


# ── tall-screen panels ───────────────────────────────────────────────────────
BRAILLE = ((0x01, 0x02, 0x04, 0x40), (0x08, 0x10, 0x20, 0x80))


def panel_equity(curve, start, height=14):
    """NAV over the run as a braille line chart; green above starting capital, red below."""
    navs = [float(p.get("nav", 0) or 0) for p in curve]
    if len(navs) < 2:
        return
    start = start or navs[0]
    pw = box_width() - 4 - 12
    n = pw * 2
    vals = [navs[i * (len(navs) - 1) // (n - 1)] for i in range(n)]
    lo, hi = min(vals + [start]), max(vals + [start])
    span = (hi - lo) or 1.0
    lo, hi = lo - span * 0.03, hi + span * 0.03
    H = height * 4

    def Y(v):
        return min(H - 1, max(0, round((hi - v) / (hi - lo) * (H - 1))))

    grid = [[0] * pw for _ in range(height)]
    prev = Y(vals[0])
    for i, v in enumerate(vals):
        y = Y(v)
        for yy in range(min(y, prev), max(y, prev) + 1):
            grid[yy // 4][i // 2] |= BRAILLE[i % 2][yy % 4]
        prev = y
    base = Y(start) // 4
    label_at = {0: hi, height - 1: lo, base: start, height // 2: (hi + lo) / 2}

    lines = []
    for r in range(height):
        lab = paint(f"${label_at[r]:>9,.0f} ┤", GRY) if r in label_at else paint(" " * 10 + " │", DGREEN)
        cells = []
        for cx in range(pw):
            b = grid[r][cx]
            if b:
                cells.append(paint(chr(0x2800 + b), GREEN if vals[2 * cx] >= start else RED))
            elif r == base:
                cells.append(paint("┄", GRY))
            else:
                cells.append(" ")
        lines.append(lab + "".join(cells))
    t0, t1 = curve[0].get("timestamp_ms", 0), curve[-1].get("timestamp_ms", 0)
    fmt = lambda ms: datetime.fromtimestamp(ms / 1000).strftime("%m-%d %H:%M")
    mid = fmt((t0 + t1) / 2)
    axis = fmt(t0) + " " * max(1, pw // 2 - 11 - len(mid) // 2) + mid
    axis = axis + " " * max(1, pw - len(axis) - 11) + fmt(t1)
    lines.append(paint(" " * 11 + "└" + "─" * pw, DGREEN))
    lines.append(" " * 12 + paint(axis, GRY))
    chg = navs[-1] - start
    print(panel(f"TELEMETRY · EQUITY  ${navs[-1]:,.0f}  {'+' if chg >= 0 else '-'}${abs(chg):,.0f}", lines))


def panel_daily(trades, days=14):
    by = defaultdict(lambda: [0.0, 0])
    for t in trades:
        if not t.get("closed_at_ms"):
            continue
        d = datetime.fromtimestamp(t["closed_at_ms"] / 1000).date()
        by[d][0] += float(t.get("realized_pnl", 0) or 0)
        by[d][1] += 1
    keys = sorted(by)[-days:]
    if not keys:
        return
    peak = max(abs(by[k][0]) for k in keys) or 1
    half = max(6, (box_width() - 4 - 30) // 2)
    lines = []
    for k in keys:
        v, n = by[k]
        L = round(abs(v) / peak * half)
        left = " " * (half - L) + paint("█" * L, RED) if v < 0 else " " * half
        right = paint("█" * L, GREEN) if v >= 0 else ""
        lines.append(f"{k:%a %m-%d} {left}{paint('│', GRY)}{pad(right, half)} {money(v, 11)} {n:>3}tr")
    print(panel("DAMAGE REPORT · DAILY P&L", lines))


def until(iso):
    """Countdown to an RFC3339 timestamp (Go emits nanoseconds; Python 3.9 wants micro)."""
    try:
        iso = re.sub(r"(\.\d{6})\d+", r"\1", iso.replace("Z", "+00:00"))
        d = datetime.fromisoformat(iso).timestamp() - time.time()
    except (AttributeError, ValueError):
        return paint("--", GRY)
    if d <= 0:
        return paint("EXPIRED", RED)
    s = f"T+{dur(d)}"
    return paint(s, RED if d < 3600 else (AMBER if d < 6 * 3600 else GREEN))


def panel_positions(positions):
    W = box_width() - 4
    fixed = 16 + 7 + 8 + 11 + 12 + 10 + 10 + 2
    cw = max(16, W - fixed)
    lines = [paint(f"{'CONTRACT':<{cw}} {'PERSONA':<15}{'ENTRY':>7}{'SIZE':>8}{'COST':>11}"
                   f"{'BEST/WORST':>12}{'FEES':>10}{'EXPIRY':>10}", GRY)]
    for p in positions:
        e, sz = float(p.get("entry_price", 0) or 0), float(p.get("size", 0) or 0)
        best, worst = float(p.get("best_mark", e) or e), float(p.get("worst_mark", e) or e)
        fees = float(p.get("entry_fee_usd", 0) or 0) + float(p.get("entry_spread_cost_usd", 0) or 0)
        bw = f"{best:.2f}/{worst:.2f}"
        lines.append(
            f"{paint(p.get('contract_id', '?')[:cw].ljust(cw), BOLD, WHT)} "
            f"{paint(p.get('persona', '?')[:14].upper().ljust(15), DGREEN)}"
            f"{e:>7.2f}{sz:>8,.0f}{paint(f'${e * sz:,.0f}'.rjust(11), WHT)}"
            f"{paint(bw.rjust(12), GREEN if best > e else (RED if worst < e else GRY))}"
            f"{paint(f'${fees:,.2f}'.rjust(10), AMBER)}"
            + pad("", 10 - vis(until(p.get("expiry")))) + until(p.get("expiry"))
        )
    if len(lines) == 1:
        lines.append(paint("no open positions — weapons cold", GRY))
    print(panel(f"TARGETS LOCKED · OPEN POSITIONS {len(positions)}", lines))


def panel_tape(trades, limit=15):
    W = box_width() - 4
    fixed = 12 + 15 + 13 + 12 + 8 + 12 + 9
    cw = max(16, W - fixed)
    lines = [paint(f"{'CLOSED':<12}{'CONTRACT':<{cw}}{'PERSONA':<15}{'EXIT':<13}{'ENTRY→EXIT':>12}"
                   f"{'SIZE':>8}{'P&L':>12}{'RET':>9}", GRY)]
    recent = sorted(trades, key=lambda t: t.get("closed_at_ms", 0), reverse=True)[:limit]
    for t in recent:
        pnl = float(t.get("realized_pnl", 0) or 0)
        ret = float(t.get("return_pct", 0) or 0)
        when = datetime.fromtimestamp(t.get("closed_at_ms", 0) / 1000).strftime("%m-%d %H:%M")
        px = f"{float(t.get('entry_price', 0) or 0):.2f}→{float(t.get('exit_price', 0) or 0):.2f}"
        lines.append(
            f"{paint(when.ljust(12), GRY)}{t.get('contract_id', '?')[:cw - 1].ljust(cw)}"
            f"{paint(t.get('persona', '?')[:14].upper().ljust(15), DGREEN)}"
            f"{paint(str(t.get('status', ''))[:12].ljust(13), GREEN if pnl > 0 else RED)}"
            f"{px:>12}{float(t.get('size', 0) or 0):>8,.0f}{money(pnl, 12)}{pct(ret, width=9)}"
        )
    if len(lines) > 1:
        print(panel("GUN CAMERA · TRADE TAPE", lines))


def capture(fn, *a):
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        fn(*a)
    out = buf.getvalue().rstrip("\n")
    return out.split("\n") if out else []


def row(*parts):
    """Render (fn, *args) panels side by side when wide enough, else stacked."""
    global _W
    full = box_width()
    if full < TWO_COL:
        for fn, *a in parts:
            fn(*a)
        return
    w = (full - len(parts) + 1) // len(parts)
    _W = w
    try:
        blocks = [b for b in (capture(fn, *a) for fn, *a in parts) if b]
    finally:
        _W = None
    if not blocks:
        return
    h = max(len(b) for b in blocks)
    blank = paint("│", DGREEN) + " " * (w - 2) + paint("│", DGREEN)
    blocks = [b[:-1] + [blank] * (h - len(b)) + b[-1:] for b in blocks]
    for line in zip(*blocks):
        print(" ".join(line))


def term_rows():
    try:
        return os.get_terminal_size().lines
    except OSError:
        return 0


# ── frame ────────────────────────────────────────────────────────────────────
def draw(host, feed_mode):
    snap = snapshot(host)
    status = snap["status"] or {}
    if not status.get("components"):
        print(panel("NO SIGNAL", [
            paint(f"cannot reach liveview at {host}", RED),
            paint("is the stack up?  ./run.sh", GRY),
        ], accent=RED, tab=RED))
        return

    acct = snap["account"] or {}
    rows = term_rows()
    head = capture(lambda: (
        render_banner(acct, status),
        render_annunciators(snap),
        print(),
        row((panel_account, acct), (panel_health, status)),
        panel_equity(snap["curve"] or [], float(acct.get("starting_capital", 0) or 0),
                     min(40, max(10, rows // 6)) if rows else 14),
        row((panel_breen, status, snap["ledger"] or {}), (panel_llm, status)),
        row((panel_gordons, status, snap["gordons"] or {}), (panel_personas_pnl, snap["personas"] or [])),
        panel_positions(snap["positions"] or []),
        row((panel_daily, snap["trades"] or []), (panel_book, snap["book"] or {})),
        panel_tape(snap["trades"] or [], max(15, rows // 8)),
        panel_sources(status),
    ))
    print("\n".join(head))
    room = rows - len(head) - 3
    panel_feed(status, limit=max(20 if feed_mode else 8, room))

    now = datetime.now().strftime("%H:%M:%S")
    print(paint(f"  ▪ {now}  {host}  ·  {TAGLINES[THEME][1]}", GRY), end="")


def main():
    global USE_COLOR, WIDTH_CAP, THEME
    ap = argparse.ArgumentParser(description="Tactical HUD for the Gekko stack.")
    ap.add_argument("--host", default=os.environ.get("GEKKO_HOST", "http://localhost:8082"))
    ap.add_argument("-n", "--interval", type=float, default=5.0, help="refresh seconds")
    ap.add_argument("--once", action="store_true", help="draw one frame and exit")
    ap.add_argument("--feed", action="store_true", help="longer target log")
    ap.add_argument("--no-color", action="store_true")
    ap.add_argument("--theme", choices=sorted(THEMES), default="hud")
    ap.add_argument("--width", type=int, default=WIDTH_CAP, help="max display width")
    args = ap.parse_args()

    WIDTH_CAP = args.width
    THEME = args.theme
    if args.no_color:
        USE_COLOR = False

    if args.once:
        draw(args.host, args.feed)
        print()
        return

    try:
        sys.stdout.write("\033[?1049h\033[?25l")  # alt screen, hide cursor
        while True:
            # Build the whole frame first and overwrite in place: no clear, no flicker.
            frame = capture(draw, args.host, args.feed)
            rows = term_rows() or len(frame)
            sys.stdout.write("\033[H" + "\n".join(ln + "\033[K" for ln in frame[:rows]) + "\033[J")
            sys.stdout.flush()
            time.sleep(args.interval)
    except KeyboardInterrupt:
        pass
    finally:
        sys.stdout.write("\033[?25h\033[?1049l")  # restore cursor + screen


if __name__ == "__main__":
    main()
