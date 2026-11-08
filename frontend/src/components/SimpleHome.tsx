import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  TrendingUp, TrendingDown, Bot, ShieldCheck, ShieldAlert,
  Gauge, Target, Compass,
} from 'lucide-react';
import { computeWindowedPnL } from '../data/portfolioData';

// ─── The "simple" landing: a few high-level numbers + the handful of knobs that
// actually change how Gordon trades. Everything else lives behind Advanced mode.
//
// Knobs are the same backend-owned model controls the Advanced Controls tab
// writes (/api/controls/model); supervision is owned by App and passed in.

type Stance = 'bearish' | 'neutral' | 'bullish';
type RiskTier = 'conservative' | 'cautious' | 'balanced' | 'assertive' | 'aggressive';

interface ModelControls {
  stance: Stance;
  risk_tier: RiskTier;
  match_threshold: number;
  max_slippage_pct: number;
}

interface Account {
  starting_capital: number;
  cash: number;
  nav: number;
  open_positions: number;
  drawdown_pct: number;
  max_drawdown_pct: number;
}

interface ApiEquityPoint { timestamp_ms: number; nav: number; benchmark_nav?: number; }
interface BookMetrics { win_rate?: number; profit_factor?: number; trades?: number; }

const STANCES: Stance[] = ['bearish', 'neutral', 'bullish'];
const TIERS: RiskTier[] = ['conservative', 'cautious', 'balanced', 'assertive', 'aggressive'];

const STANCE_NOTE: Record<Stance, string> = {
  bearish: 'Leans toward selling. Strong buy signals still trade, but sized down.',
  neutral: 'No directional bias — takes whichever side the signal points.',
  bullish: 'Leans toward buying. Strong sell signals still trade, but sized down.',
};

const TIER_CAPS: Record<RiskTier, { size: number; exp: number }> = {
  conservative: { size: 1000, exp: 4000 },
  cautious: { size: 1750, exp: 6000 },
  balanced: { size: 2500, exp: 8000 },
  assertive: { size: 3750, exp: 12000 },
  aggressive: { size: 5000, exp: 16000 },
};

const usd = (n: number) => `$${Math.round(n).toLocaleString('en-US')}`;
const fmtSigned = (n: number) => `${n >= 0 ? '+' : '−'}$${Math.abs(Math.round(n)).toLocaleString('en-US')}`;
const fmtPct = (n: number) => `${n >= 0 ? '+' : '−'}${Math.abs(n).toFixed(2)}%`;
const tone = (n: number) => (n >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]');

// ─── Big equity sparkline for the hero ──────────────────────────────────────
// Shows the whole run's shape; autoscaled to the visible range so small moves
// are legible (a flat $100k account only wiggles by tens of dollars). Colour is
// driven by `positive` — the account's ACTUAL direction vs. starting capital —
// not the window's local slope, so the line never disagrees with the headline
// P&L (a green line over a red number reads as fake).
//
// The SPY buy-and-hold benchmark (benchmark_nav) is overlaid as a dashed blue
// line on the SAME y-scale — min/max span both series so the comparison is
// truthful (whichever is higher on screen is genuinely ahead).
const Sparkline: React.FC<{ equity: ApiEquityPoint[]; positive: boolean }> = ({ equity, positive }) => {
  const pts = equity.slice(-120);
  if (pts.length < 2) return null;
  const navs = pts.map(p => p.nav);
  const benches = pts.map(p => (typeof p.benchmark_nav === 'number' && p.benchmark_nav > 0 ? p.benchmark_nav : null));
  const hasBench = benches.some(b => b !== null);

  // Shared scale across both series so the two lines are directly comparable.
  const all = [...navs, ...benches.filter((b): b is number => b !== null)];
  const min = Math.min(...all), max = Math.max(...all);
  const span = max - min || 1;
  const W = 320, H = 72;
  const yOf = (v: number) => H - ((v - min) / span) * (H - 8) - 4;
  const xOf = (i: number) => (i / (navs.length - 1)) * W;

  const navCoords = navs.map((v, i) => [xOf(i), yOf(v)] as const);
  const navLine = navCoords.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ');
  const area = `${navCoords[0][0].toFixed(1)},${H} ${navLine} ${navCoords[navCoords.length - 1][0].toFixed(1)},${H}`;
  const [lx, ly] = navCoords[navCoords.length - 1];
  const stroke = positive ? 'var(--accent-green)' : 'var(--accent-red)';

  // Benchmark polyline; break the line wherever a sample lacks a benchmark value.
  const benchLine = benches
    .map((b, i) => (b === null ? null : `${xOf(i).toFixed(1)},${yOf(b).toFixed(1)}`))
    .filter((s): s is string => s !== null)
    .join(' ');

  return (
    <svg viewBox={`0 0 ${W} ${H}`} width="100%" height={H} preserveAspectRatio="none"
      className="max-w-[360px]" role="img" aria-label="Net asset value vs. SPY benchmark">
      <polygon points={area} fill={stroke} opacity={0.08} />
      {hasBench && (
        <polyline points={benchLine} fill="none" stroke="var(--chart-benchmark)" strokeWidth="1.5"
          strokeDasharray="5 3" strokeLinejoin="round" strokeLinecap="round" opacity={0.9} />
      )}
      <polyline points={navLine} fill="none" stroke={stroke} strokeWidth="2.5" strokeLinejoin="round" strokeLinecap="round" />
      <circle cx={lx} cy={ly} r="3.5" fill={stroke} />
    </svg>
  );
};

// ─── One high-level stat tile ───────────────────────────────────────────────
const Stat: React.FC<{ label: string; value: string; sub?: string; valueClass?: string }> =
  ({ label, value, sub, valueClass = 'text-[var(--text-main)]' }) => (
  <div className="flex flex-col gap-1 bg-white/[0.03] border border-white/[0.06] rounded-2xl px-5 py-4">
    <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">{label}</span>
    <span className={`text-2xl font-bold font-mono ${valueClass}`}>{value}</span>
    {sub && <span className="text-[11px] text-[var(--text-muted)] font-mono">{sub}</span>}
  </div>
);

interface Props {
  isUnsupervised: boolean;
  toggleSupervision: () => void;
}

const SimpleHome: React.FC<Props> = ({ isUnsupervised, toggleSupervision }) => {
  const [account, setAccount] = useState<Account | null>(null);
  const [equity, setEquity] = useState<ApiEquityPoint[]>([]);
  const [book, setBook] = useState<BookMetrics>({});
  const [live, setLive] = useState(false);

  // Model knobs (backend-owned). Debounced push on change, like ModelControls.
  const [c, setC] = useState<ModelControls>({ stance: 'neutral', risk_tier: 'balanced', match_threshold: 0.5, max_slippage_pct: 0.03 });
  const [saved, setSaved] = useState(true);
  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null);

  const fetchAll = useCallback(async () => {
    try {
      const [a, e, b] = await Promise.all([
        fetch('/api/account'), fetch('/api/equity_curve'), fetch('/api/book_metrics'),
      ]);
      if (a.ok) { const d = await a.json(); if (d && typeof d.nav === 'number') { setAccount(d); setLive(true); } }
      if (e.ok) { const d = await e.json(); if (Array.isArray(d)) setEquity(d); }
      if (b.ok) { const d = await b.json(); if (d && typeof d === 'object') setBook(d); }
    } catch { setLive(false); }
  }, []);

  useEffect(() => {
    fetchAll();
    const id = setInterval(fetchAll, 10000);
    return () => clearInterval(id);
  }, [fetchAll]);

  useEffect(() => {
    fetch('/api/controls/model')
      .then(r => (r.ok ? r.json() : null))
      .then(d => { if (d) setC({ stance: d.stance, risk_tier: d.risk_tier, match_threshold: d.match_threshold, max_slippage_pct: d.max_slippage_pct || 0.03 }); })
      .catch(() => {});
  }, []);

  const push = useCallback((next: ModelControls) => {
    setSaved(false);
    if (debounce.current) clearTimeout(debounce.current);
    debounce.current = setTimeout(() => {
      fetch('/api/controls/model', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(next),
      }).then(() => setSaved(true)).catch(() => setSaved(false));
    }, 350);
  }, []);
  const update = (patch: Partial<ModelControls>) =>
    setC(prev => { const next = { ...prev, ...patch }; push(next); return next; });

  const pnl = useMemo(() => {
    if (!account) return null;
    const mapped = equity.map(p => ({ timestampMs: p.timestamp_ms, nav: p.nav, date: '', drawdown: 0 }));
    return computeWindowedPnL(mapped, account.starting_capital, account.nav);
  }, [equity, account]);

  const caps = TIER_CAPS[c.risk_tier];
  const riskIdx = TIERS.indexOf(c.risk_tier);

  if (!account) {
    return (
      <div className="max-w-[1000px] mx-auto mt-24 px-6 text-center flex flex-col items-center gap-3">
        <Bot className="w-10 h-10 text-[var(--text-muted)] animate-pulse" />
        <p className="text-sm text-[var(--text-muted)] font-mono">
          Waiting for the trading stack… start it with <span className="text-[var(--text-main)]">./run.sh</span>.
        </p>
      </div>
    );
  }

  const totalPnl = account.nav - account.starting_capital;
  const totalPct = account.starting_capital ? (totalPnl / account.starting_capital) * 100 : 0;

  // Gekko's since-launch return minus SPY buy-and-hold over the same period, in
  // percentage points. Uses the latest benchmarked equity sample as SPY's mark.
  const vsSpy = (() => {
    const lastBench = [...equity].reverse().find(p => typeof p.benchmark_nav === 'number' && p.benchmark_nav! > 0);
    if (!lastBench || !account.starting_capital) return null;
    const spyPct = ((lastBench.benchmark_nav! - account.starting_capital) / account.starting_capital) * 100;
    return totalPct - spyPct;
  })();

  const today = pnl?.today ?? { delta: 0, pct: 0 };
  const TodayArrow = today.delta >= 0 ? TrendingUp : TrendingDown;
  const lockedMargin = account.nav - account.cash;

  return (
    <div className="max-w-[1000px] mx-auto mt-8 px-6 pb-20 flex flex-col gap-6">

      {/* ── Hero: the number that matters, plus its trend ────────────────── */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-3xl p-7 shadow-lg backdrop-blur-md">
        <div className="flex justify-between items-start flex-wrap gap-6">
          <div className="flex flex-col gap-1">
            <span className="text-[11px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">
              Account value · paper trading
            </span>
            <span className="text-5xl font-bold font-mono text-[var(--text-main)] tracking-tight">
              {usd(account.nav)}
            </span>
            <div className="flex items-center gap-3 mt-1">
              <span className={`text-sm font-bold font-mono flex items-center gap-1 ${tone(today.delta)}`}>
                <TodayArrow className="w-4 h-4" /> {fmtSigned(today.delta)} today
              </span>
              <span className="text-[var(--text-muted)]">·</span>
              <span className={`text-sm font-bold font-mono ${tone(totalPnl)}`}>
                {fmtSigned(totalPnl)} ({fmtPct(totalPct)}) all-time
              </span>
            </div>
          </div>
          <div className="flex flex-col items-end gap-2 flex-1 min-w-[240px]">
            <span className={`text-[10px] font-bold px-2.5 py-1 rounded-full border font-mono ${
              live ? 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                   : 'bg-rose-500/10 border-rose-500/30 text-rose-400'}`}>
              {live ? '● LIVE' : '○ OFFLINE'}
            </span>
            <Sparkline equity={equity} positive={totalPnl >= 0} />
            <div className="flex items-center gap-3 text-[10px] text-[var(--text-muted)] font-mono">
              <span className="flex items-center gap-1.5">
                <span className="inline-block w-3 h-0.5 rounded" style={{ background: totalPnl >= 0 ? 'var(--accent-green)' : 'var(--accent-red)' }} />
                Gekko
              </span>
              <span className="flex items-center gap-1.5">
                <span className="inline-block w-3 h-0 border-t-2 border-dashed" style={{ borderColor: 'var(--chart-benchmark)' }} />
                SPY hold{vsSpy !== null ? ` (${vsSpy >= 0 ? '+' : '−'}${Math.abs(vsSpy).toFixed(2)}%)` : ''}
              </span>
            </div>
          </div>
        </div>
      </section>

      {/* ── High-level stats ─────────────────────────────────────────────── */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <Stat label="Win rate"
          value={book.win_rate !== undefined ? `${book.win_rate.toFixed(0)}%` : '—'}
          sub={book.trades ? `${book.trades} settled trades` : 'no trades settled yet'} />
        <Stat label="Open positions" value={String(account.open_positions)}
          sub={`${usd(lockedMargin)} at work`} />
        <Stat label="Max drawdown"
          value={`−${Math.abs(account.max_drawdown_pct).toFixed(1)}%`}
          sub="worst peak-to-trough" valueClass="text-[var(--accent-red)]" />
        <Stat label="This week"
          value={pnl ? fmtSigned(pnl.week.delta) : '—'}
          sub={pnl ? fmtPct(pnl.week.pct) : ''}
          valueClass={tone(pnl?.week.delta ?? 0)} />
      </div>

      {/* ── The knobs that steer the fleet ───────────────────────────────── */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-3xl p-7 shadow-lg backdrop-blur-md flex flex-col gap-7">
        <div className="flex items-center gap-2">
          <Gauge className="w-4 h-4 text-[var(--primary)]" />
          <h3 className="text-sm font-bold text-[var(--text-main)]">How Gekko trades</h3>
          <span className="text-[10px] font-mono ml-auto text-[var(--text-muted)]">
            {saved ? 'applied live' : 'saving…'}
          </span>
        </div>

        {/* Knob 1 — Market stance */}
        <div className="flex flex-col gap-2.5">
          <div className="flex items-center gap-2">
            <Compass className="w-3.5 h-3.5 text-[var(--text-muted)]" />
            <span className="text-[13px] font-semibold text-[var(--text-main)]">Market stance</span>
          </div>
          <div className="flex gap-2">
            {STANCES.map(s => (
              <button key={s} onClick={() => update({ stance: s })}
                className={`flex-1 text-[13px] font-bold capitalize py-2.5 rounded-xl border transition-all ${
                  c.stance === s
                    ? 'border-[var(--primary)] bg-[var(--primary)]/15 text-[var(--primary)]'
                    : 'border-white/10 text-[var(--text-muted)] hover:border-white/25'}`}>
                {s}
              </button>
            ))}
          </div>
          <span className="text-[12px] text-[var(--text-muted)] leading-snug">{STANCE_NOTE[c.stance]}</span>
        </div>

        {/* Knob 2 — Risk appetite */}
        <div className="flex flex-col gap-2.5">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <Gauge className="w-3.5 h-3.5 text-[var(--text-muted)]" />
              <span className="text-[13px] font-semibold text-[var(--text-main)]">Risk appetite</span>
            </div>
            <span className="text-[12px] font-bold font-mono capitalize text-[var(--primary)]">{c.risk_tier}</span>
          </div>
          <input type="range" min={0} max={4} step={1} value={riskIdx}
            onChange={e => update({ risk_tier: TIERS[Number(e.target.value)] })}
            className="w-full accent-[var(--primary)]" />
          <div className="flex text-[9px] text-[var(--text-muted)] font-mono">
            {TIERS.map((t, i) => (
              <span key={t}
                className={`flex-1 capitalize ${i === 0 ? 'text-left' : i === TIERS.length - 1 ? 'text-right' : 'text-center'} ${
                  i === riskIdx ? 'text-[var(--primary)] font-bold' : ''}`}>
                {t}
              </span>
            ))}
          </div>
          <span className="text-[12px] text-[var(--text-muted)] leading-snug">
            Up to <b className="text-[var(--text-main)]">{usd(caps.size)}</b> per position,{' '}
            <b className="text-[var(--text-main)]">{usd(caps.exp)}</b> exposure per trade.
          </span>
        </div>

        {/* Knob 3 — Selectivity */}
        <div className="flex flex-col gap-2.5">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <Target className="w-3.5 h-3.5 text-[var(--text-muted)]" />
              <span className="text-[13px] font-semibold text-[var(--text-main)]">Selectivity</span>
            </div>
            <span className="text-[12px] font-bold font-mono text-[var(--primary)]">{c.match_threshold.toFixed(2)}</span>
          </div>
          <input type="range" min={0.35} max={0.8} step={0.05} value={c.match_threshold}
            onChange={e => update({ match_threshold: Math.round(Number(e.target.value) * 100) / 100 })}
            className="w-full accent-[var(--primary)]" />
          <div className="flex justify-between text-[10px] text-[var(--text-muted)] font-mono">
            <span>Trade often</span><span>High-conviction</span>
          </div>
          <span className="text-[12px] text-[var(--text-muted)] leading-snug">
            Higher = fewer, surer trades — only acts on news→market matches scoring ≥ {c.match_threshold.toFixed(2)}.
          </span>
        </div>
      </section>

      {/* ── The master switch: supervised vs. auto ───────────────────────── */}
      <section className={`rounded-3xl p-6 shadow-lg backdrop-blur-md border-2 flex items-center justify-between gap-4 flex-wrap transition-colors ${
        isUnsupervised ? 'bg-rose-500/[0.06] border-rose-500/30' : 'bg-[var(--primary)]/[0.05] border-[var(--primary)]/25'}`}>
        <div className="flex items-center gap-4">
          <div className={`p-3 rounded-2xl border ${
            isUnsupervised ? 'bg-rose-500/15 border-rose-500/30 text-rose-400'
                           : 'bg-[var(--primary)]/15 border-[var(--primary)]/30 text-[var(--primary)]'}`}>
            {isUnsupervised ? <ShieldAlert className="w-6 h-6" /> : <ShieldCheck className="w-6 h-6" />}
          </div>
          <div className="flex flex-col gap-0.5">
            <span className="text-[15px] font-bold text-[var(--text-main)]">
              {isUnsupervised ? 'Auto-trading (unsupervised)' : 'You approve every trade (supervised)'}
            </span>
            <span className="text-[12px] text-[var(--text-muted)] leading-snug max-w-md">
              {isUnsupervised
                ? 'Gekko routes proposals on its own. No confirmation prompts.'
                : 'Each proposal waits for your approval before it trades. Safe for unattended review.'}
            </span>
          </div>
        </div>
        <button type="button" onClick={toggleSupervision}
          className={`px-5 py-2.5 rounded-xl border font-bold text-[13px] transition-all cursor-pointer ${
            isUnsupervised
              ? 'bg-[var(--primary)]/10 hover:bg-[var(--primary)]/20 border-[var(--primary)]/40 text-[var(--primary)]'
              : 'bg-rose-500/10 hover:bg-rose-500/20 border-rose-500/40 text-rose-300'}`}>
          {isUnsupervised ? 'Switch to supervised' : 'Enable auto-trading'}
        </button>
      </section>

      <p className="text-center text-[11px] text-[var(--text-muted)] font-mono">
        Paper mode — no real money at risk. Full dashboards, streams &amp; per-Gordon metrics live in Advanced.
      </p>
    </div>
  );
};

export default SimpleHome;
