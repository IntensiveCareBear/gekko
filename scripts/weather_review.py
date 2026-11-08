#!/usr/bin/env python3
"""Weather desk review: score every predictive weather pick against the realized high.

  1. Cheap-ticket EV: realized hit rate vs the model's `fair`, and per-contract P&L at the
     logged ask, bucketed by ask band.
  2. One-strike-per-family: what breen's rejected buckets would have settled at, against
     the strike it let through.

Sources: logs/gordon-weather.log ([WEATHER] model lines), logs/breen.log (rejections),
Kalshi's public market endpoint for the settled result and official high (cached in
runs/weather/settlements.json; unfinalized markets are skipped). history.jsonl is NOT used as
truth: its METAR-derived "realized_high" under-reads the official high (see section 3).
Usage: scripts/weather_review.py [--size 1000]
"""
import argparse, collections, json, math, os, re, statistics
from datetime import datetime

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
STATION = {"NY": "KNYC", "MIA": "KMIA", "PHIL": "KPHL", "DEN": "KDEN", "LAX": "KLAX", "AUS": "KAUS", "CHI": "KMDW"}
MON = {m: i + 1 for i, m in enumerate("JAN FEB MAR APR MAY JUN JUL AUG SEP OCT NOV DEC".split())}
FEE = 0.07  # Kalshi taker fee rate: fee/contract = FEE * p * (1 - p)

MODEL = re.compile(r'^(\S+ \S+) \[WEATHER\] Weather\(model\): (.+?) predicted final high ([\d.-]+)°F .*?bucket "([^"]+)" fair ([\d.]+) vs ask ([\d.]+)')
REJECT = re.compile(r'^(\S+ \S+) \[RISK\] Rejected BUY (\S+) \(persona=weather\): family \S+ already holds a different strike')
TICKER = re.compile(r'^KXHIGH([A-Z]+)-(\d{2})([A-Z]{3})(\d{2})-([BT])([\d.]+)$')


def parse_ticker(t):
    m = TICKER.match(t)
    if not m or m.group(1) not in STATION:
        return None
    return dict(station=STATION[m.group(1)], date=f"20{m.group(2)}-{MON[m.group(3)]:02d}-{m.group(4)}",
                kind=m.group(5), x=float(m.group(6)), family=t.rsplit("-", 1)[0])


def load_history():
    h = {}
    for line in open(os.path.join(ROOT, "runs/weather/history.jsonl")):
        d = json.loads(line)
        h[(d["station"], d["date"])] = d["realized_high"]
    return h


CACHE = os.path.join(ROOT, "runs/weather/settlements.json")
API = os.environ.get("KALSHI_API_BASE", "https://api.elections.kalshi.com/trade-api/v2")


def settlements(tickers):
    """ticker -> (yes?, official high). Finalized results are immutable, so cache them."""
    import urllib.request
    try:
        cache = json.load(open(CACHE))
    except Exception:
        cache = {}
    for t in tickers:
        if t in cache:
            continue
        try:
            m = json.load(urllib.request.urlopen(f"{API}/markets/{t}", timeout=10))["market"]
        except Exception:
            continue
        if m.get("status") in ("finalized", "settled") and m.get("result") in ("yes", "no"):
            cache[t] = [m["result"] == "yes", float(m["expiration_value"])]
    json.dump(cache, open(CACHE, "w"))
    return cache


def pnl(y, ask, size):
    return size * ((1.0 if y else 0.0) - ask - FEE * ask * (1 - ask))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--size", type=int, default=1000)
    size = ap.parse_args().size
    hist = load_history()

    picks = {}  # ticker -> first sighting
    for line in open(os.path.join(ROOT, "logs/gordon-weather.log"), errors="replace"):
        m = MODEL.match(line)
        if m and m.group(4) not in picks:
            picks[m.group(4)] = dict(ts=m.group(1), mu=float(m.group(3)), fair=float(m.group(5)), ask=float(m.group(6)))
    rejected = collections.OrderedDict()
    for line in open(os.path.join(ROOT, "logs/breen.log"), errors="replace"):
        m = REJECT.match(line)
        if m:
            rejected.setdefault(m.group(2), m.group(1))

    filled = set()
    for line in open(os.path.join(ROOT, "runs/trades.jsonl")):
        d = json.loads(line)
        if d.get("persona") == "weather":
            filled.add(d["contract_id"])
    try:
        import urllib.request
        for p in json.load(urllib.request.urlopen("http://localhost:8082/api/positions", timeout=3)):
            if p.get("persona") == "weather":
                filled.add(p["contract_id"])
    except Exception:
        pass

    res = settlements([t for t in picks if parse_ticker(t)])
    rows = []
    for t, p in picks.items():
        tk = parse_ticker(t)
        if not tk or t not in res:
            continue
        y, high = res[t]
        rows.append(dict(t=t, tk=tk, y=y, high=high, filled=t in filled, rej=t in rejected, **p))

    print(f"Scored {len(rows)} predictive picks on settled days ({sum(r['filled'] for r in rows)} filled, "
          f"{sum(r['rej'] for r in rows)} breen-rejected), size {size}, fee {FEE}*p*(1-p)\n")

    print("== 1. Hit rate and EV by ask band (every pick, as if all were taken) ==")
    bands = [(0, .05), (.05, .10), (.10, .20), (.20, 1.01)]
    print(f"{'ask band':<11}{'n':>4}{'mean fair':>11}{'hit rate':>10}{'EV/ctr':>9}{'P&L':>10}  note")
    for lo, hi in bands:
        b = [r for r in rows if lo <= r["ask"] < hi]
        if not b:
            continue
        hit = sum(r["y"] for r in b) / len(b)
        pl = sum(pnl(r["y"], r["ask"], size) for r in b)
        print(f"{lo:.2f}-{min(hi,1):.2f}  {len(b):>4}{statistics.mean(r['fair'] for r in b):>11.3f}{hit:>10.3f}"
              f"{pl/size/len(b):>9.3f}{pl:>10.0f}  {'thin sample' if len(b) < 30 else ''}")
    pl = sum(pnl(r["y"], r["ask"], size) for r in rows)
    print(f"{'all':<11}{len(rows):>4}{statistics.mean(r['fair'] for r in rows):>11.3f}"
          f"{sum(r['y'] for r in rows)/len(rows):>10.3f}{pl/size/len(rows):>9.3f}{pl:>10.0f}\n")

    print("== Forecast error per station (predicted final high vs OFFICIAL high, first sighting) ==")
    by = collections.defaultdict(list)
    for r in rows:
        by[(r["tk"]["station"], r["tk"]["date"])].append(r["mu"] - r["high"])
    st = collections.defaultdict(list)
    for (s, _), v in by.items():
        st[s].append(statistics.mean(v))
    for s, v in sorted(st.items()):
        print(f"{s}  days {len(v):>2}  mean err {statistics.mean(v):+.2f}F  mean |err| {statistics.mean(abs(x) for x in v):.2f}F")

    print("\n== 2. One-strike-per-family rejections ==")
    rr = [r for r in rows if r["rej"] and not r["filled"]]
    fl = [r for r in rows if r["filled"]]
    for name, grp in (("rejected (not taken)", rr), ("filled", fl)):
        if grp:
            print(f"{name:<22} n {len(grp):>3}  hit {sum(r['y'] for r in grp)/len(grp):.3f}  "
                  f"mean fair {statistics.mean(r['fair'] for r in grp):.3f}  "
                  f"P&L if taken {sum(pnl(r['y'], r['ask'], size) for r in grp):>8.0f}")
    fam = collections.defaultdict(lambda: dict(f=[], r=[]))
    for r in rows:
        if r["filled"]:
            fam[r["tk"]["family"]]["f"].append(r)
        elif r["rej"]:
            fam[r["tk"]["family"]]["r"].append(r)
    both = {k: v for k, v in fam.items() if v["f"] and v["r"]}
    better = sum(1 for v in both.values()
                 if sum(pnl(x["y"], x["ask"], size) for x in v["r"]) > sum(pnl(x["y"], x["ask"], size) for x in v["f"]))
    print(f"families with both a fill and a rejection: {len(both)}; rejected side would have beaten the fill in {better}")
    for k, v in sorted(both.items()):
        f = sum(pnl(x["y"], x["ask"], size) for x in v["f"])
        r = sum(pnl(x["y"], x["ask"], size) for x in v["r"])
        print(f"  {k:<22} filled {f:>7.0f}  rejected-if-taken {r:>7.0f}  (high {v['f'][0]['high']})")

    print("\n== 4. By model edge (fair - ask): is the model worst where it disagrees with the market most? ==")
    print(f"{'edge':<11}{'n':>4}{'mean fair':>11}{'hit rate':>10}{'EV/ctr':>9}{'P&L':>10}")
    for lo, hi in [(0, .06), (.06, .10), (.10, .15), (.15, 1)]:
        b = [r for r in rows if lo <= r["fair"] - r["ask"] < hi]
        if b:
            pl = sum(pnl(r["y"], r["ask"], size) for r in b)
            print(f"{lo:.2f}-{min(hi,1):.2f}  {len(b):>4}{statistics.mean(r['fair'] for r in b):>11.3f}"
                  f"{sum(r['y'] for r in b)/len(b):>10.3f}{pl/size/len(b):>9.3f}{pl:>10.0f}")
    print(f"settled days in sample: {len({(r['tk']['station'], r['tk']['date']) for r in rows})} station-days over "
          f"{len({r['tk']['date'] for r in rows})} calendar days")

    print("\n== 3. history.jsonl realized_high vs official high (feeds the bias model) ==")
    off = {(r["tk"]["station"], r["tk"]["date"]): r["high"] for r in rows}
    d = collections.defaultdict(list)
    for (s_, dt), o in off.items():
        if (s_, dt) in hist:
            d[s_].append(hist[(s_, dt)] - o)
    for s_, v in sorted(d.items()):
        print(f"{s_}  days {len(v):>2}  history minus official: mean {statistics.mean(v):+.2f}F  min {min(v):+.1f}  max {max(v):+.1f}")

    ledger_section(res, size)


def ledger_section(res, size):
    """5. Fit check on the forecast that actually traded (runs/weather/pick_forecasts.jsonl).
    Uses the FIRST ledger row per station-date (the forecast the first pick priced with)
    against the official high from Kalshi, and compares the implied bias with the table."""
    path = os.path.join(ROOT, "runs/weather/pick_forecasts.jsonl")
    print("\n== 5. Bias on the forecast actually priced (pick_forecasts.jsonl) ==")
    if not os.path.exists(path):
        print("no ledger yet (written once a weather observation prices with a cached forecast; "
              "needs gordon built from 8a6ff18 or later)")
        return
    off = {}
    for t, (_, high) in res.items():
        tk = parse_ticker(t)
        if tk:
            off[(tk["station"], tk["date"])] = high
    first = {}
    for line in open(path):
        try:
            r = json.loads(line)
        except ValueError:
            continue
        first.setdefault((r["station"], r["date"]), r)
    by = collections.defaultdict(list)
    table = collections.defaultdict(float)
    for k, r in first.items():
        table[k[0]] = r["bias_f"]
        if k in off:
            by[k[0]].append(off[k] - r["forecast_high"])
    print(f"{len(first)} station-days in ledger, {sum(len(v) for v in by.values())} with a settled official high")
    print(f"{'station':<7}{'n':>3}{'pick-time bias':>16}{'table bias':>12}{'stdev':>8}   (official - forecast)")
    for s_, v in sorted(by.items()):
        sd = statistics.stdev(v) if len(v) > 1 else float("nan")
        print(f"{s_:<7}{len(v):>3}{statistics.mean(v):>+16.2f}{table[s_]:>+12.2f}{sd:>8.2f}"
              f"{'  thin' if len(v) < 10 else ''}")


if __name__ == "__main__":
    main()
