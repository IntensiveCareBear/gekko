import React, { useMemo } from 'react';
import { TrendingUp, TrendingDown } from 'lucide-react';
import { computeWindowedPnL, type EquityPoint, type PnLWindow } from '../data/portfolioData';

const fmtUSD = (n: number) =>
  `${n >= 0 ? '+' : '−'}$${Math.abs(Math.round(n)).toLocaleString('en-US')}`;
const fmtPct = (n: number) => `${n >= 0 ? '+' : '−'}${Math.abs(n).toFixed(2)}%`;

const tone = (n: number) => (n >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]');

// Sparkline of the most recent NAV samples, colored by net direction.
const Sparkline: React.FC<{ equity: EquityPoint[] }> = ({ equity }) => {
  const pts = equity.slice(-40);
  if (pts.length < 2) return null;
  const navs = pts.map(p => p.nav);
  const min = Math.min(...navs), max = Math.max(...navs);
  const span = max - min || 1;
  const W = 240, H = 56;
  const coords = navs.map((v, i) => {
    const x = (i / (navs.length - 1)) * W;
    const y = H - ((v - min) / span) * (H - 6) - 3;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });
  const up = navs[navs.length - 1] >= navs[0];
  const stroke = up ? 'var(--accent-green)' : 'var(--accent-red)';
  const [lastX, lastY] = coords[coords.length - 1].split(',');
  return (
    <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} className="max-w-[45%]" role="img" aria-label="NAV sparkline">
      <polyline points={coords.join(' ')} fill="none" stroke={stroke} strokeWidth="2" />
      <circle cx={lastX} cy={lastY} r="3" fill={stroke} />
    </svg>
  );
};

const WindowPill: React.FC<{ label: string; w: PnLWindow; sub?: string }> = ({ label, w, sub }) => (
  <div className="flex flex-col gap-1 bg-white/[0.03] border border-white/[0.06] rounded-xl px-4 py-3">
    <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">{label}</span>
    <span className={`text-xl font-bold font-mono ${tone(w.delta)}`}>{fmtUSD(w.delta)}</span>
    <span className="text-[11px] text-[var(--text-muted)] font-mono">{sub ?? fmtPct(w.pct)}</span>
  </div>
);

interface Props {
  equity: EquityPoint[];
  startingCapital: number;
  nav: number;
  drawdownPct: number;
  openPositions: number;
  lockedMargin: number;
}

const PnLHero: React.FC<Props> = ({ equity, startingCapital, nav, drawdownPct, openPositions, lockedMargin }) => {
  const pnl = useMemo(
    () => computeWindowedPnL(equity, startingCapital, nav),
    [equity, startingCapital, nav],
  );
  const today = pnl.today;
  const Arrow = today.delta >= 0 ? TrendingUp : TrendingDown;

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-6 shadow-lg backdrop-blur-md">
      <div className="flex justify-between items-start flex-wrap gap-3">
        <div>
          <div className="text-xs text-[var(--text-muted)] uppercase tracking-wider font-semibold mb-1">
            Today's P&amp;L · paper account
          </div>
          <div className="flex items-baseline gap-3">
            <span className={`text-4xl font-bold font-mono ${tone(today.delta)}`}>{fmtUSD(today.delta)}</span>
            <span className={`text-sm font-bold font-mono flex items-center gap-1 ${tone(today.delta)}`}>
              <Arrow className="w-4 h-4" /> {fmtPct(today.pct)}
            </span>
          </div>
        </div>
        <Sparkline equity={equity} />
      </div>

      <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 mt-6">
        <WindowPill label="This week" w={pnl.week} />
        <WindowPill label="Since launch" w={pnl.sinceLaunch} sub={`NAV $${Math.round(nav).toLocaleString('en-US')}`} />
        <div className="flex flex-col gap-1 bg-white/[0.03] border border-white/[0.06] rounded-xl px-4 py-3">
          <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">Max drawdown</span>
          <span className="text-xl font-bold font-mono text-[var(--accent-red)]">−{Math.abs(drawdownPct).toFixed(2)}%</span>
          <span className="text-[11px] text-[var(--text-muted)] font-mono">from peak</span>
        </div>
        <div className="flex flex-col gap-1 bg-white/[0.03] border border-white/[0.06] rounded-xl px-4 py-3">
          <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">Open positions</span>
          <span className="text-xl font-bold font-mono text-[var(--text-main)]">{openPositions}</span>
          <span className="text-[11px] text-[var(--text-muted)] font-mono">${Math.round(lockedMargin).toLocaleString('en-US')} at risk</span>
        </div>
      </div>
    </section>
  );
};

export default PnLHero;
