#!/usr/bin/env python3
"""Gordon-default review: score every LLM fair that reached gordon's edge check against the
Kalshi settlement. Answers one question: does the LLM fair carry any edge over the book?

Sources: logs/gordon.log (+ rotated/archived copies). Two line kinds reach the edge check:
  skipped: `[EDGE] BUY T skipped — fair F vs book B/A: ...` (and the `fair F > 2.0× ask A`
           implausible variant, BUY only, no bid)
  taken:   `🟢 [gordon-default] BUY N T @ P | ... model=...+kalshi-match` — passed the check.
           Since 2026-10-19 the line carries `| fair F |`; older taken rows have ask (= fill P)
           but no fair, so they are in the P&L tables, not the calibration/edge-band ones.
Fair is P(YES) on both sides. A BUY is scored as YES bought at the ask; a SELL as NO bought
at 1-bid (the side the model wanted, at the touch), held to settlement, with the Kalshi taker
fee ceil(0.07*C*P*(1-P)) on the one entry leg (settlement is fee-free).
Settlements: Kalshi public market endpoint, cached in runs/gordon/settlements.json
(finalized only; POLY-* and unsettled tickers are skipped).
Repeat evaluations of a ticker are all counted (n); `tk` is the distinct-ticker count.
Usage: scripts/gordon_review.py [--size 1000] [--since 2026-10-09]
"""
import argparse, collections, glob, json, math, os, re, statistics
from concurrent.futures import ThreadPoolExecutor

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
FEE = 0.07
CACHE = os.path.join(ROOT, "runs/gordon/settlements.json")
API = os.environ.get("KALSHI_API_BASE", "https://api.elections.kalshi.com/trade-api/v2")

TS = r'^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) '
SKIP = re.compile(TS + r'\[EDGE\] (BUY|SELL) (\S+) skipped — fair ([\d.]+) vs book ([\d.]+)/([\d.]+)')
IMPL = re.compile(TS + r'\[EDGE\] (BUY) (\S+) skipped — fair ([\d.]+) > [\d.]+× ask ([\d.]+)')
TAKEN = re.compile(TS + r'\S+ \[gordon-default\] BUY\s+\d+\s+(\S+)\s+@ ([\d.]+) (?:.*?\| fair ([\d.]+) )?.*model=\S*\+kalshi-match')


def parse(line):
    """One log line -> proposal dict, or None."""
    if m := SKIP.match(line):
        return dict(ts=m[1], act=m[2], t=m[3], fair=float(m[4]), bid=float(m[5]), ask=float(m[6]), kind="skipped")
    if m := IMPL.match(line):
        return dict(ts=m[1], act=m[2], t=m[3], fair=float(m[4]), bid=None, ask=float(m[5]), kind="implausible")
    if m := TAKEN.match(line):
        return dict(ts=m[1], act="BUY", t=m[2], fair=float(m[4]) if m[4] else None, bid=None, ask=float(m[3]), kind="taken")
    return None


def selftest():
    r = parse("2026/10/19 13:11:19 [EDGE] BUY KXETHD-26OCT2017-T2439.99 skipped — fair 0.25 vs book "
              "0.19/0.21: edge +0.040 < required 0.090 (min 0.07 + spread)\n")
    assert r == dict(ts="2026/10/19 13:11:19", act="BUY", t="KXETHD-26OCT2017-T2439.99", fair=0.25,
                     bid=0.19, ask=0.21, kind="skipped"), r
    r = parse("2026/09/30 21:11:11 [EDGE] BUY KXU3-26OCT-T4.9 skipped — fair 0.12 > 2.0× ask 0.05: implausible")
    assert (r["kind"], r["fair"], r["ask"], r["bid"]) == ("implausible", 0.12, 0.05, None), r
    r = parse("2026/10/19 17:16:38 🟢 [gordon-default] BUY  1550 KXETHD-26OCT2017-T2439.99 @ 0.23  | conf 0.75 | "
              "Slip: 0.94% (Slices: 0) | model=qwen2.5:32b+kalshi-match | signals: momentum -18.9% (552µs)")
    assert (r["kind"], r["t"], r["ask"], r["fair"]) == ("taken", "KXETHD-26OCT2017-T2439.99", 0.23, None), r
    r = parse("2026/10/20 09:00:00 🟢 [gordon-default] BUY  1550 KXETHD-26OCT2017-T2439.99 @ 0.23  | conf 0.75 | "
              "fair 0.310 | Slip: 0.94% (Slices: 0) | model=qwen2.5:32b+kalshi-match (552µs)")
    assert (r["kind"], r["ask"], r["fair"]) == ("taken", 0.23, 0.31), r
    assert parse("2026/10/19 17:16:38 🟢 [gordon-default] BUY  10 FOO @ 0.23  | model=qwen2.5:32b") is None
    # fee: 1000 ctr at 0.50 -> 0.07*1000*0.25 = 17.50 exactly; 1 ctr at 0.50 -> ceil(1.75c) = 2c
    assert pnl(True, 0.5, 1000) == 1000 * 0.5 - 17.5 and abs(pnl(False, 0.5, 1) - (-0.52)) < 1e-9


def series(t):
    s = t.split("-")[0]
    if s in ("KXBTCD", "KXETHD", "KXWTI", "KXBRENTW"):
        return s
    for p in ("KXCPI", "KXHIGH"):
        if s.startswith(p):
            return p + "*"
    return "other"


def fetch(t):
    import time, urllib.error, urllib.request
    for i in range(6):
        try:
            m = json.load(urllib.request.urlopen(f"{API}/markets/{t}", timeout=10))["market"]
            break
        except urllib.error.HTTPError as e:
            if e.code != 429:
                return t, None
            time.sleep(0.5 * 2 ** i)  # public API rate limit
        except Exception:
            return t, None
    else:
        return t, None
    if m.get("status") in ("finalized", "settled") and m.get("result") in ("yes", "no"):
        return t, m["result"] == "yes"
    return t, None


def settlements(tickers):
    """ticker -> yes? Finalized results are immutable, so cache them."""
    try:
        cache = json.load(open(CACHE))
    except Exception:
        cache = {}
    todo = [t for t in tickers if t not in cache and t.startswith("KX")]
    with ThreadPoolExecutor(8) as ex:  # misses (unsettled, errors) are refetched next run
        for t, y in ex.map(fetch, todo):
            if y is not None:
                cache[t] = y
    os.makedirs(os.path.dirname(CACHE), exist_ok=True)
    json.dump(cache, open(CACHE, "w"), sort_keys=True)
    return cache


def pnl(y, price, size):
    """Hold-to-settlement P&L of buying `size` at `price`, paying y=1. One fee leg."""
    fee = math.ceil(round(FEE * size * price * (1 - price) * 100, 6)) / 100
    return size * ((1.0 if y else 0.0) - price) - fee


def leg(r):
    """(won?, entry price) for the side the proposal wanted."""
    return (r["y"], r["ask"]) if r["act"] == "BUY" else (not r["y"], 1 - r["bid"])


def edge(r):
    return r["fair"] - r["ask"] if r["act"] == "BUY" else r["bid"] - r["fair"]


def table(title, groups, size):
    print(f"== {title} ==")
    print(f"{'':<14}{'n':>5}{'tk':>5}{'fair':>7}{'price':>7}{'won':>7}{'EV/ctr':>8}{'P&L':>10}")
    for name, b in groups:
        if not b:
            continue
        fs = [r["fair"] for r in b if r["fair"] is not None]
        legs = [leg(r) for r in b]
        pl = sum(pnl(w, p, size) for w, p in legs)
        print(f"{name:<14}{len(b):>5}{len({r['t'] for r in b}):>5}"
              f"{(f'{statistics.mean(fs):.3f}' if fs else '-'):>7}{statistics.mean(p for _, p in legs):>7.3f}"
              f"{sum(w for w, _ in legs)/len(b):>7.3f}{pl/size/len(b):>+8.3f}{pl:>+10.0f}"
              f"{'  thin' if len(b) < 30 else ''}")
    print()


def main():
    selftest()
    ap = argparse.ArgumentParser()
    ap.add_argument("--size", type=int, default=1000)
    ap.add_argument("--since", default="", help="YYYY-MM-DD, only proposals on/after this date")
    a = ap.parse_args()
    since = a.since.replace("-", "/")

    files = sorted(glob.glob(os.path.join(ROOT, "logs/gordon.log*")) + glob.glob(os.path.join(ROOT, "runs/archive-*/**/gordon.log*"), recursive=True))
    seen, props = set(), []
    for f in files:
        for line in open(f, errors="replace"):
            if line in seen:  # archived copies can overlap the live log
                continue
            seen.add(line)
            if (r := parse(line)) and r["ts"] >= since:
                props.append(r)

    res = settlements({r["t"] for r in props})
    rows = [dict(r, y=res[r["t"]]) for r in props if r["t"] in res]
    first = list({(r["t"], r["act"]): r for r in reversed(rows)}.values())
    print(f"{len(props)} proposals parsed from {len(files)} log file(s); {len(rows)} on settled Kalshi markets "
          f"({len({r['t'] for r in rows})} tickers). Size {a.size}, fee ceil({FEE}*C*P*(1-P)).\n"
          f"BUY = YES at ask, SELL = NO at 1-bid, held to settlement. Taken rows before 2026-10-19 carry no fair.\n")
    if not rows:
        return

    cal = [r for r in rows if r["fair"] is not None]
    print("== 1. Calibration: LLM fair (P(YES)) vs realized YES rate, all sides ==")
    print(f"{'fair band':<11}{'n':>5}{'tk':>5}{'mean fair':>11}{'YES rate':>10}{'mean ask':>10}")
    for i in range(10):
        lo, hi = i / 10, (i + 1) / 10
        b = [r for r in cal if lo <= r["fair"] < hi or (i == 9 and r["fair"] == 1)]
        if b:
            print(f"{lo:.1f}-{hi:.1f}    {len(b):>5}{len({r['t'] for r in b}):>5}{statistics.mean(r['fair'] for r in b):>11.3f}"
                  f"{sum(r['y'] for r in b)/len(b):>10.3f}{statistics.mean(r['ask'] for r in b):>10.3f}"
                  f"{'  thin' if len(b) < 30 else ''}")
    wb = [r for r in cal if r["bid"] is not None]
    if wb:
        brier = lambda p: statistics.mean((p(r) - r["y"]) ** 2 for r in wb)
        print(f"Brier on {len(wb)} rows with a full book: LLM fair {brier(lambda r: r['fair']):.4f}  "
              f"book mid {brier(lambda r: (r['bid'] + r['ask']) / 2):.4f}  (lower is better; LLM must beat mid to have edge)")
    print()

    order = ["KXBTCD", "KXETHD", "KXWTI", "KXBRENTW", "KXCPI*", "KXHIGH*", "other"]
    for act in ("BUY", "SELL"):
        g = [r for r in rows if r["act"] == act]
        table(f"2. {act}: EV if every proposal were taken, by series", [(s, [r for r in g if series(r["t"]) == s]) for s in order]
              + [("all", g), ("all, 1st/tk", [r for r in first if r["act"] == act])], a.size)

    bands = [(-9, 0), (0, .03), (.03, .07), (.07, .10), (.10, .15), (.15, .25), (.25, 9)]
    table("3. EV by LLM edge (BUY fair-ask, SELL bid-fair), rows with a logged fair",
          [(f"{max(lo,-1):+.2f}..{min(hi,1):+.2f}", [r for r in cal if lo <= edge(r) < hi]) for lo, hi in bands], a.size)

    table("4. Taken vs skipped", [(f"{k} {act}", [r for r in rows if r["kind"] == k and r["act"] == act])
                                  for k in ("taken", "skipped", "implausible") for act in ("BUY", "SELL")], a.size)
    unsettled = len({r["t"] for r in props} - set(res))
    print(f"{unsettled} tickers not scored (POLY-*, unsettled, or fetch failed).")


if __name__ == "__main__":
    main()
