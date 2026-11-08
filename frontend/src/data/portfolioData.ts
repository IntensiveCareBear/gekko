// Portfolio analytics data contracts + pure compute helpers.
//
// The dashboard is driven by the live paper-trading run (liveview /api/account,
// /api/trades, /api/equity_curve). This module keeps the shared TypeScript types
// and the framework-agnostic KPI/distribution calculators that operate on that
// data. (It previously also generated a demo dataset; that has been removed now
// that the analytics tab is live.)

// ─── Universal Data Contract Interfaces ─────────────────────────────────────

export interface EquityPoint {
  /** Unix milliseconds */
  timestampMs: number;
  /** Display label for the x-axis */
  date: string;
  /** Total Net Asset Value in USD */
  nav: number;
  /** Peak-to-trough drawdown as a positive percentage, e.g. 4.2 = 4.2% */
  drawdown: number;
  /** Buy-and-hold benchmark NAV (seed money in SPY). Absent if benchmark off/unanchored. */
  benchmarkNav?: number;
}

export interface TradeRecord {
  id: string;
  strategyId: string;
  contractId: string;
  exchange: 'kalshi' | 'polymarket';
  /** Resolution side: YES / NO (or EXIT for a sell-signal close) */
  side: string;
  entryPrice: number;
  exitPrice: number;
  /** Number of contracts / units */
  size: number;
  /** USD realized profit or loss */
  realizedPnL: number;
  /** Return as a percentage, e.g. 12.5 = 12.5% */
  returnPct: number;
  openedAtMs: number;
  closedAtMs: number;
  status: string;
}

export interface AssetAllocation {
  name: string;
  value: number;
  fill: string;
}

export interface PnLBin {
  label: string;
  count: number;
  totalPnL: number;
  /** Positive bins: true */
  profitable: boolean;
}

export interface PortfolioKPIs {
  totalTrades: number;
  winRate: number;
  profitFactor: number;
  avgWin: number;
  avgLoss: number;
  totalPnL: number;
  maxDrawdownPct: number;
  sharpeApprox: number;
}

// ─── Windowed P&L (for the hero card) ───────────────────────────────────────

export interface PnLWindow {
  /** Absolute USD change over the window */
  delta: number;
  /** Percentage change over the window's starting NAV */
  pct: number;
}

export interface WindowedPnL {
  today: PnLWindow;
  week: PnLWindow;
  sinceLaunch: PnLWindow;
}

function windowFrom(startNav: number, currentNav: number): PnLWindow {
  const delta = currentNav - startNav;
  const pct = startNav > 0 ? (delta / startNav) * 100 : 0;
  return { delta: Math.round(delta * 100) / 100, pct: Math.round(pct * 100) / 100 };
}

// navAtOrBefore returns the NAV of the latest equity sample at/just before cutoff,
// or the earliest sample's NAV when every sample is newer than the cutoff.
function navAtOrBefore(equity: EquityPoint[], cutoffMs: number): number {
  let nav = equity[0]?.nav ?? 0;
  for (const p of equity) {
    if (p.timestampMs <= cutoffMs) nav = p.nav;
    else break;
  }
  return nav;
}

// computeWindowedPnL derives today / this-week / since-launch P&L from the equity
// curve and account. "Today" is measured from local midnight, "week" from 7d ago,
// and "since launch" from starting capital.
export function computeWindowedPnL(equity: EquityPoint[], startingCapital: number, currentNav: number): WindowedPnL {
  const now = Date.now();
  const midnight = new Date(); midnight.setHours(0, 0, 0, 0);
  const weekAgo = now - 7 * 86_400_000;

  const navToday = equity.length ? navAtOrBefore(equity, midnight.getTime()) : currentNav;
  const navWeek = equity.length ? navAtOrBefore(equity, weekAgo) : currentNav;

  return {
    today: windowFrom(navToday, currentNav),
    week: windowFrom(navWeek, currentNav),
    sinceLaunch: windowFrom(startingCapital, currentNav),
  };
}

// ─── PnL Distribution Bins ────────────────────────────────────────────────────

export function generatePnLBins(trades: TradeRecord[]): PnLBin[] {
  const bins: { label: string; min: number; max: number; profitable: boolean }[] = [
    { label: '< -$500', min: -Infinity, max: -500, profitable: false },
    { label: '-$200 to -$500', min: -500, max: -200, profitable: false },
    { label: '-$50 to -$200', min: -200, max: -50, profitable: false },
    { label: 'Break-even', min: -50, max: 50, profitable: true },
    { label: '+$50 to +$200', min: 50, max: 200, profitable: true },
    { label: '+$200 to +$500', min: 200, max: 500, profitable: true },
    { label: '> +$500', min: 500, max: Infinity, profitable: true },
  ];

  return bins.map(bin => {
    const matching = trades.filter(t => t.realizedPnL >= bin.min && t.realizedPnL < bin.max);
    return {
      label: bin.label,
      count: matching.length,
      totalPnL: Math.round(matching.reduce((s, t) => s + t.realizedPnL, 0) * 100) / 100,
      profitable: bin.profitable,
    };
  });
}

// ─── Summary KPIs ─────────────────────────────────────────────────────────────

export function computeKPIs(trades: TradeRecord[], equityCurve: EquityPoint[]): PortfolioKPIs {
  const wins = trades.filter(t => t.realizedPnL > 0);
  const losses = trades.filter(t => t.realizedPnL <= 0);

  const grossProfit = wins.reduce((s, t) => s + t.realizedPnL, 0);
  const grossLoss = Math.abs(losses.reduce((s, t) => s + t.realizedPnL, 0));

  const avgWin = wins.length > 0 ? grossProfit / wins.length : 0;
  const avgLoss = losses.length > 0 ? grossLoss / losses.length : 0;
  const profitFactor = grossLoss > 0 ? grossProfit / grossLoss : grossProfit > 0 ? 99 : 0;
  const totalPnL = trades.reduce((s, t) => s + t.realizedPnL, 0);
  const maxDrawdownPct = equityCurve.reduce((m, p) => Math.max(m, p.drawdown), 0);
  const winRate = trades.length > 0 ? (wins.length / trades.length) * 100 : 0;

  // Rough annualized Sharpe from NAV step-to-step returns.
  const returns: number[] = [];
  for (let i = 1; i < equityCurve.length; i++) {
    const prev = equityCurve[i - 1].nav;
    if (prev > 0) returns.push((equityCurve[i].nav - prev) / prev);
  }
  let sharpeApprox = 0;
  if (returns.length > 1) {
    const meanRet = returns.reduce((s, r) => s + r, 0) / returns.length;
    const variance = returns.reduce((s, r) => s + (r - meanRet) ** 2, 0) / returns.length;
    if (variance > 0) sharpeApprox = (meanRet / Math.sqrt(variance)) * Math.sqrt(252);
  }

  return {
    totalTrades: trades.length,
    winRate: Math.round(winRate * 100) / 100,
    profitFactor: Math.round(profitFactor * 100) / 100,
    avgWin: Math.round(avgWin * 100) / 100,
    avgLoss: Math.round(avgLoss * 100) / 100,
    totalPnL: Math.round(totalPnL * 100) / 100,
    maxDrawdownPct: Math.round(maxDrawdownPct * 100) / 100,
    sharpeApprox: Math.round(sharpeApprox * 100) / 100,
  };
}
