#!/usr/bin/env python3
"""Weather sigma backtest: would a different sigma have made the predictive path pay?

weather_review.py scores only the buckets the live model picked, so it can't say what a
tighter sigma would have picked instead. This replays each station-day at the moment of its
first logged predictive pick, against EVERY bucket in that event at the Kalshi quote then
(the last hourly candle closed before the pick), and prices them with candidate sigmas.

  1. Calibration across all buckets: Brier and log loss of the model's P(YES) per sigma,
     against the market's mid. The model needs to beat the mid before any trade rule matters.
  2. Trading rule as live (edge 0.05..0.25, ask >= 0.05, one strike per family = the
     highest edge here), held to settlement at 1000 contracts with the taker fee.

mu and the running high come from the logged pick line (forecast + bias as priced live).
"LOO fit" is each station's RMSE of official - mu over its OTHER days (leave-one-out), so
that row is not fit on the day it trades. Cache: runs/weather/backtest_cache.json.
Usage: scripts/weather_sigma_backtest.py [--size 1000]
"""
import argparse, json, math, os, re, sys, time, urllib.error, urllib.request
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from weather_review import API, FEE, ROOT, parse_ticker  # noqa: E402

LINE = re.compile(r'^(\S+ \S+) \[WEATHER\] Weather\(model\): .+? predicted final high ([\d.-]+)°F .*?'
                  r'bucket "([^"]+)" fair [\d.]+ vs ask [\d.]+ .*?day-high so far ([\d.-]+)°F')
CACHE = os.path.join(ROOT, "runs/weather/backtest_cache.json")
MIN_EDGE, MAX_EDGE, MIN_ASK, MAX_ASK = 0.05, 0.25, 0.05, 0.98  # live gordon-weather gates
FLOOR, STATION_FLOOR = 3.0, {"KAUS": 4.0, "KLAX": 5.0}


def get(url):
    for wait in (1, 2, 4, 8, 16):
        try:
            return json.load(urllib.request.urlopen(url, timeout=15))
        except urllib.error.HTTPError as e:
            if e.code != 429:
                raise
            time.sleep(wait)
    raise RuntimeError("rate limited: " + url)


def ncdf(x):
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def yes_prob(m, mu, sigma, run_high):
    """P(YES) for one Kalshi bucket from Normal(mu, sigma) truncated below at the running
    high, integer settlement (mirrors gordon/weatherbias.go bucketYesProb)."""
    zr = (run_high - mu) / sigma
    den = 1 - ncdf(zr)

    def cdf(x):  # P(final < x | final >= run_high)
        if x <= run_high:
            return 0.0
        if den < 1e-9:
            return 1.0
        return (ncdf((x - mu) / sigma) - ncdf(zr)) / den
    st, lo, hi = m["strike_type"], m.get("floor_strike"), m.get("cap_strike")
    if st == "less":        # YES iff high < cap
        return cdf(hi - 0.5)
    if st == "greater":     # YES iff high > floor
        return 1 - cdf(lo + 0.5)
    return cdf(hi + 0.5) - cdf(lo - 0.5)  # between: floor <= high <= cap


def fee_pnl(y, ask, size):
    return size * ((1.0 if y else 0.0) - ask - math.ceil(FEE * size * ask * (1 - ask) * 100 - 1e-9) / 100 / size)


def load_days():
    """(station, date) -> first predictive pick: ts (UTC epoch), mu, running high, event."""
    days = {}
    for line in open(os.path.join(ROOT, "logs/gordon-weather.log"), errors="replace"):
        m = LINE.match(line)
        tk = m and parse_ticker(m[3])
        if not tk or (tk["station"], tk["date"]) in days:
            continue
        ts = datetime.strptime(m[1], "%Y/%m/%d %H:%M:%S").astimezone(timezone.utc).timestamp()
        days[(tk["station"], tk["date"])] = dict(ts=ts, mu=float(m[2]), run=float(m[4]), event=tk["family"])
    return days


def market_state(day, cache):
    """Every bucket of the event with its result and the quote at the pick time, or None
    while the event is unsettled."""
    ev = day["event"]
    if ev not in cache:
        ms = get(f"{API}/events/{ev}")["markets"]
        if not all(m.get("result") in ("yes", "no") for m in ms):
            return None
        rows = []
        for m in ms:
            s = int(day["ts"]) - 3 * 3600
            c = get(f"{API}/series/{ev.split('-')[0]}/markets/{m['ticker']}/candlesticks"
                    f"?start_ts={s}&end_ts={int(day['ts'])}&period_interval=60")["candlesticks"]
            c = [x for x in c if x["end_period_ts"] <= day["ts"] and x["yes_ask"].get("close_dollars")]
            if not c:
                continue
            q = c[-1]
            rows.append(dict(ticker=m["ticker"], strike_type=m["strike_type"], floor_strike=m.get("floor_strike"),
                             cap_strike=m.get("cap_strike"), y=m["result"] == "yes",
                             ask=float(q["yes_ask"]["close_dollars"]), bid=float(q["yes_bid"].get("close_dollars") or 0)))
        cache[ev] = dict(high=float(ms[0]["expiration_value"]), buckets=rows)
        json.dump(cache, open(CACHE, "w"))
    return cache[ev]


def selftest():
    b = lambda st, lo, hi: dict(strike_type=st, floor_strike=lo, cap_strike=hi)
    # the six NY Oct-13 buckets partition the line, so their probabilities sum to 1
    ladder = [b("less", None, 71), b("between", 71, 72), b("between", 73, 74), b("between", 75, 76),
              b("between", 77, 78), b("greater", 78, None)]
    assert abs(sum(yes_prob(x, 73.0, 2.0, 60.0) for x in ladder) - 1) < 1e-9
    # a running high above a bucket kills it
    assert yes_prob(b("less", None, 71), 73.0, 2.0, 72.0) == 0
    # 1000 at 0.50: fee 0.07*1000*0.25 = 17.50
    assert abs(fee_pnl(True, 0.5, 1000) - (500 - 17.5)) < 1e-9


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--size", type=int, default=1000)
    size = ap.parse_args().size
    selftest()
    try:
        cache = json.load(open(CACHE))
    except Exception:
        cache = {}
    bias = {r["station"]: r["sigma_f"] for r in json.load(open(os.path.join(ROOT, "runs/weather/bias.json")))}
    days = []
    for k, d in sorted(load_days().items()):
        st = market_state(d, cache)
        if st and st["buckets"]:
            days.append(dict(station=k[0], date=k[1], **d, **st))
    err = {}
    for d in days:
        err.setdefault(d["station"], []).append((d["date"], d["high"] - d["mu"]))

    def loo(d):
        e = [x for dt, x in err[d["station"]] if dt != d["date"]]
        return max(math.sqrt(sum(x * x for x in e) / len(e)), 0.75) if len(e) >= 3 else FLOOR

    variants = {
        "live (floor 3/4/5)": lambda d: max(bias.get(d["station"], FLOOR), FLOOR, STATION_FLOOR.get(d["station"], 0)),
        "table, no floor": lambda d: bias.get(d["station"], FLOOR),
        "LOO fit": loo,
        "fixed 1.5": lambda d: 1.5, "fixed 2.0": lambda d: 2.0, "fixed 2.5": lambda d: 2.5,
    }
    print(f"{len(days)} settled station-days, {sum(len(d['buckets']) for d in days)} buckets, "
          f"{days[0]['date']} .. {days[-1]['date']}\n")
    print("== Pick-time error (official high - mu) per station ==")
    for s, e in sorted(err.items()):
        x = [v for _, v in e]
        print(f"{s}  n {len(x):>3}  mean {sum(x)/len(x):+5.2f}F  rmse {math.sqrt(sum(v*v for v in x)/len(x)):4.2f}F  "
              f"live sigma {variants['live (floor 3/4/5)'](dict(station=s)):.1f}  table {bias.get(s, 0):.2f}")

    print("\n== 1. Calibration over every bucket (lower is better) ==")
    print(f"{'':<20}{'Brier':>8}{'logloss':>9}")
    allb = [(d, b) for d in days for b in d["buckets"]]
    mid = lambda b: (b["ask"] + b["bid"]) / 2 if b["bid"] > 0 else b["ask"] / 2
    clip = lambda p: min(max(p, 0.01), 0.99)
    for name, f in [("market mid", None)] + list(variants.items()):
        ps = [(clip(mid(b)) if f is None else clip(yes_prob(b, d["mu"], f(d), d["run"])), b["y"]) for d, b in allb]
        br = sum((p - y) ** 2 for p, y in ps) / len(ps)
        ll = -sum(math.log(p if y else 1 - p) for p, y in ps) / len(ps)
        print(f"{name:<20}{br:>8.4f}{ll:>9.4f}")

    print(f"\n== 2. Live trading rule, one strike per station-day, {size} contracts to settlement ==")
    print(f"{'':<20}{'trades':>7}{'wins':>6}{'mean ask':>10}{'P&L':>9}{'per trade':>11}")
    for name, f in variants.items():
        picks = []
        for d in days:
            best = None
            for b in d["buckets"]:
                e = yes_prob(b, d["mu"], f(d), d["run"]) - b["ask"]
                if MIN_ASK <= b["ask"] < MAX_ASK and MIN_EDGE <= e <= MAX_EDGE and (best is None or e > best[0]):
                    best = (e, b)
            if best:
                picks.append(best[1])
        pl = sum(fee_pnl(b["y"], b["ask"], size) for b in picks)
        n = len(picks)
        print(f"{name:<20}{n:>7}{sum(b['y'] for b in picks):>6}{(sum(b['ask'] for b in picks)/n if n else 0):>10.3f}"
              f"{pl:>9.0f}{(pl/n if n else 0):>11.1f}")


if __name__ == "__main__":
    main()
