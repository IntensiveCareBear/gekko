import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Briefcase, Clock, SlidersHorizontal, ChevronDown } from 'lucide-react';

// ─── Data contracts (subset of the breen-published blobs) ────────────────────

interface Account {
  starting_capital: number;
  cash: number;
  nav: number;
  realized_pnl: number;
  unrealized_pnl: number;
  open_positions: number;
  drawdown_pct: number;
  max_drawdown_pct: number;
}

interface EquityPoint {
  timestamp_ms: number;
  nav: number;
}

interface ClosedTrade {
  realized_pnl: number;
  closed_at_ms: number;
}

interface BookMetrics {
  win_rate?: number;
  profit_factor?: number;
  trades?: number;
}

const fmtUSD = (n: number) => `${n >= 0 ? '+' : '−'}$${Math.abs(Math.round(n)).toLocaleString('en-US')}`;
const fmtUSDplain = (n: number) => `$${Math.round(n).toLocaleString('en-US')}`;
const fmtPct = (n: number) => `${n >= 0 ? '+' : '−'}${Math.abs(n).toFixed(2)}%`;
const tone = (n: number) => (n >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]');

// A curated, human-readable subset of breen's run_config knobs (the rest are shown
// under "all knobs"). Keys match breen/config.go Snapshot().
const KEY_KNOBS: { key: string; label: string; fmt?: (v: unknown) => string }[] = [
  { key: 'starting_capital', label: 'Starting capital', fmt: v => fmtUSDplain(Number(v)) },
  { key: 'dry_mode', label: 'Mode', fmt: v => (v ? 'DRY (paper)' : 'WET (live)') },
  { key: 'max_open_positions', label: 'Max open positions' },
  { key: 'max_total_deployed', label: 'Max deployed', fmt: v => fmtUSDplain(Number(v)) },
  // NOTE: these two are stored as percentage POINTS (10 = 10%, 15 = 15%), unlike the
  // *_exposure_pct knobs which are fractions — so don't scale them.
  { key: 'max_drawdown_halt_pct', label: 'Drawdown halt', fmt: v => `${Number(v)}%` },
  { key: 'stop_loss_pct', label: 'Stop loss', fmt: v => `${Number(v)}%` },
  { key: 'stop_loss_min_abs', label: 'Stop-loss floor', fmt: v => `$${Number(v).toFixed(2)}` },
  { key: 'per_contract_loss_limit', label: 'Per-contract loss cap', fmt: v => fmtUSDplain(Number(v)) },
  { key: 'consensus_enabled', label: 'Consensus', fmt: v => (v ? 'on' : 'off') },
  { key: 'adaptive_enabled', label: 'Adaptive weighting', fmt: v => (v ? 'on' : 'off') },
  { key: 'entry_require_live_book', label: 'Require live book', fmt: v => (v ? 'yes' : 'no') },
];

const fmtKnob = (v: unknown): string => {
  if (typeof v === 'boolean') return v ? 'on' : 'off';
  if (typeof v === 'number') return String(v);
  return String(v ?? '—');
};

const BreenOverview: React.FC = () => {
  const [account, setAccount] = useState<Account | null>(null);
  const [equity, setEquity] = useState<EquityPoint[]>([]);
  const [trades, setTrades] = useState<ClosedTrade[]>([]);
  const [book, setBook] = useState<BookMetrics>({});
  const [config, setConfig] = useState<Record<string, unknown>>({});
  const [showAllKnobs, setShowAllKnobs] = useState(false);

  const fetchAll = useCallback(async () => {
    try {
      const [aRes, eRes, tRes, bRes, cRes] = await Promise.all([
        fetch('/api/account'),
        fetch('/api/equity_curve'),
        fetch('/api/trades'),
        fetch('/api/book_metrics'),
        fetch('/api/run/config'),
      ]);
      if (aRes.ok) { const d = await aRes.json(); if (d && typeof d.nav === 'number') setAccount(d); }
      if (eRes.ok) { const d = await eRes.json(); if (Array.isArray(d)) setEquity(d); }
      if (tRes.ok) { const d = await tRes.json(); if (Array.isArray(d)) setTrades(d); }
      if (bRes.ok) { const d = await bRes.json(); if (d && typeof d === 'object') setBook(d); }
      if (cRes.ok) { const d = await cRes.json(); if (d && typeof d === 'object') setConfig(d); }
    } catch {
      /* keep last-known */
    }
  }, []);

  useEffect(() => {
    fetchAll();
    const id = setInterval(fetchAll, 10000);
    return () => clearInterval(id);
  }, [fetchAll]);

  // Last-24h window: NAV delta vs the equity sample nearest 24h ago, and closed-trade
  // activity within the window.
  const last24 = useMemo(() => {
    const now = Date.now();
    const cutoff = now - 24 * 60 * 60 * 1000;
    let navThen: number | null = null;
    if (equity.length > 0) {
      // First sample at/after the cutoff; else the oldest we have (run < 24h old).
      const atOrAfter = equity.find(p => p.timestamp_ms >= cutoff);
      navThen = (atOrAfter ?? equity[0]).nav;
    }
    const navNow = account?.nav ?? (equity.length ? equity[equity.length - 1].nav : null);
    const navDelta = navThen !== null && navNow !== null ? navNow - navThen : null;
    const navDeltaPct = navThen ? (navDelta! / navThen) * 100 : null;
    const recent = trades.filter(t => t.closed_at_ms >= cutoff);
    const realized24 = recent.reduce((s, t) => s + t.realized_pnl, 0);
    const wins24 = recent.filter(t => t.realized_pnl > 0).length;
    return { navDelta, navDeltaPct, closed: recent.length, realized24, wins24 };
  }, [equity, trades, account]);

  if (!account) return null;

  const totalPnl = account.nav - account.starting_capital;
  const totalPct = account.starting_capital ? (totalPnl / account.starting_capital) * 100 : 0;
  const deployed = account.nav - account.cash;
  const winRate = book.win_rate;

  // ── Breen's 2–3 sentence portfolio read (templated, deterministic) ──────────
  const overview = (() => {
    const dir = totalPnl >= 0 ? 'up' : 'down';
    const s1 = `The book stands at ${fmtUSDplain(account.nav)} — ${dir} ${fmtPct(totalPct).replace('+', '').replace('−', '')} (${fmtUSD(totalPnl)}) since launch, with ${fmtUSDplain(account.cash)} in cash and ${fmtUSDplain(deployed)} at work across ${account.open_positions} open position${account.open_positions === 1 ? '' : 's'}.`;
    const s2 = winRate !== undefined && book.trades
      ? `Across ${book.trades} settled trades the win rate is ${winRate.toFixed(0)}%${book.profit_factor !== undefined ? ` at a ${book.profit_factor.toFixed(2)} profit factor` : ''}; we're currently ${account.drawdown_pct > 0.05 ? `${(account.drawdown_pct).toFixed(1)}% off the peak` : 'at or near the equity high'} (worst drawdown ${account.max_drawdown_pct.toFixed(1)}%).`
      : `No trades have settled yet, so risk metrics are still warming up; worst drawdown so far is ${account.max_drawdown_pct.toFixed(1)}%.`;
    return `${s1} ${s2}`;
  })();

  const knobEntries = Object.entries(config);

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
      {/* Breen's portfolio read */}
      <div className="flex items-start gap-3">
        <div className="p-2 rounded-xl bg-[var(--primary)]/10 border border-[var(--primary)]/25 text-[var(--primary)] shrink-0">
          <Briefcase className="w-4 h-4" />
        </div>
        <div className="flex flex-col gap-1">
          <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-bold">Breen · Portfolio Read</span>
          <p className="text-sm leading-relaxed text-[var(--text-main)]">{overview}</p>
        </div>
      </div>

      {/* Last 24h */}
      <div className="flex items-center gap-3 border-t border-white/5 pt-4 flex-wrap">
        <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-bold shrink-0">
          <Clock className="w-3.5 h-3.5" /> Last 24h
        </div>
        <div className="flex gap-5 flex-wrap font-mono text-xs">
          <div className="flex flex-col">
            <span className="text-[9px] uppercase tracking-wider text-[var(--text-muted)]">NAV change</span>
            <span className={`text-sm font-bold ${last24.navDelta === null ? 'text-[var(--text-muted)]' : tone(last24.navDelta)}`}>
              {last24.navDelta === null ? '—' : `${fmtUSD(last24.navDelta)}${last24.navDeltaPct !== null ? ` (${fmtPct(last24.navDeltaPct)})` : ''}`}
            </span>
          </div>
          <div className="flex flex-col">
            <span className="text-[9px] uppercase tracking-wider text-[var(--text-muted)]">Realized (24h)</span>
            <span className={`text-sm font-bold ${tone(last24.realized24)}`}>{fmtUSD(last24.realized24)}</span>
          </div>
          <div className="flex flex-col">
            <span className="text-[9px] uppercase tracking-wider text-[var(--text-muted)]">Trades closed</span>
            <span className="text-sm font-bold text-[var(--text-main)]">
              {last24.closed}{last24.closed > 0 ? ` · ${last24.wins24}W` : ''}
            </span>
          </div>
        </div>
      </div>

      {/* Breen knobs (read-only) */}
      {knobEntries.length > 0 && (
        <div className="border-t border-white/5 pt-4 flex flex-col gap-3">
          <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-bold">
            <SlidersHorizontal className="w-3.5 h-3.5" /> Breen Knobs
            <span className="ml-2 normal-case tracking-normal font-normal text-[9px] text-[var(--text-muted)]">set at launch · restart to change</span>
          </div>
          <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-x-5 gap-y-2 font-mono text-xs">
            {KEY_KNOBS.filter(k => k.key in config).map(k => (
              <div key={k.key} className="flex flex-col">
                <span className="text-[9px] uppercase tracking-wider text-[var(--text-muted)] truncate">{k.label}</span>
                <span className="text-[var(--text-main)] font-semibold">{k.fmt ? k.fmt(config[k.key]) : fmtKnob(config[k.key])}</span>
              </div>
            ))}
          </div>
          <button
            type="button"
            onClick={() => setShowAllKnobs(v => !v)}
            className="self-start flex items-center gap-1 text-[10px] font-semibold text-[var(--text-muted)] hover:text-[var(--primary)] transition-colors cursor-pointer"
          >
            <ChevronDown className={`w-3 h-3 transition-transform ${showAllKnobs ? 'rotate-180' : ''}`} />
            {showAllKnobs ? 'Hide' : 'Show'} all {knobEntries.length} knobs
          </button>
          {showAllKnobs && (
            <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-x-5 gap-y-1.5 font-mono text-[11px] border-t border-white/5 pt-3">
              {knobEntries.sort((a, b) => a[0].localeCompare(b[0])).map(([k, v]) => (
                <div key={k} className="flex flex-col">
                  <span className="text-[9px] uppercase tracking-wider text-[var(--text-muted)] truncate" title={k}>{k}</span>
                  <span className="text-[var(--text-main)]">{fmtKnob(v)}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </section>
  );
};

export default BreenOverview;
