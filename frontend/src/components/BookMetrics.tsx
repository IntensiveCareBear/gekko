import React, { useCallback, useEffect, useState } from 'react';
import { Gauge } from 'lucide-react';
import GlossaryTooltip from './GlossaryTooltip';

interface EntryBand {
  label: string;
  trades: number;
  win_rate: number;
  realized_pnl: number;
}

interface BookMetricsData {
  expectancy_usd?: number;
  profit_factor?: number;
  sharpe?: number;
  sortino?: number;
  max_drawdown_usd?: number;
  max_drawdown_pct?: number;
  samples?: number;
  trades: number;
  wins: number;
  losses: number;
  win_rate: number;
  entry_bands?: EntryBand[];
  // Cost decomposition (breen-intelligence Phase 0). spread_cost_usd is already inside
  // realized_pnl — it explains the number, it does not adjust it. Fees are the one real
  // adjustment: the paper account never charged them.
  spread_cost_usd?: number;
  fee_usd?: number;
  realized_pnl?: number;
  net_pnl_after_fees?: number;
  avg_mfe_pct?: number;
  avg_mae_pct?: number;
  excursion_samples?: number;
}

const fmtUSD = (n: number) => `${n >= 0 ? '+' : '−'}$${Math.abs(Math.round(n)).toLocaleString('en-US')}`;
const tone = (n: number) => (n >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]');
const fmtRatio = (n?: number) => (n === undefined || n === null ? '—' : n.toFixed(2));

const Tile: React.FC<{ label: string; value: string; cls?: string; termKey?: string }> = ({ label, value, cls, termKey }) => (
  <div className="flex flex-col gap-1">
    {termKey ? (
      <GlossaryTooltip termKey={termKey} className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">
        {label}
      </GlossaryTooltip>
    ) : (
      <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">{label}</span>
    )}
    <span className={`text-lg font-mono font-bold ${cls ?? 'text-[var(--text-main)]'}`}>{value}</span>
  </div>
);

// BookMetrics renders breen's own (whole-account) risk-adjusted scorecard, the
// counterpart to the per-persona (Gordon) scorecard: expectancy, profit factor,
// per-trade Sharpe/Sortino, max drawdown, plus win-rate by entry-price band — the
// lens the post-mortems used to find the money-losing 0.50–0.65 band.
const BookMetrics: React.FC = () => {
  const [m, setM] = useState<BookMetricsData | null>(null);
  const [loaded, setLoaded] = useState(false);

  const fetchMetrics = useCallback(async () => {
    try {
      const res = await fetch('/api/book_metrics');
      if (res.ok) {
        const data = await res.json();
        if (data && typeof data === 'object' && 'trades' in data) setM(data as BookMetricsData);
      }
    } catch {
      /* keep last-known */
    } finally {
      setLoaded(true);
    }
  }, []);

  useEffect(() => {
    fetchMetrics();
    const id = setInterval(fetchMetrics, 8000);
    return () => clearInterval(id);
  }, [fetchMetrics]);

  // Hidden until the book has at least one closed trade.
  if (!loaded || !m || m.trades === 0) return null;

  const gated = (m.samples ?? 0) < 20 || m.sharpe === undefined;
  const bands = (m.entry_bands ?? []).filter(b => b.trades > 0);
  // Trades closed before Phase 0 carry no excursion data; averaging over them would
  // report a narrower range than the book actually saw.
  const hasExcursions = (m.excursion_samples ?? 0) > 0;

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md">
      <div className="flex items-center gap-2 mb-4">
        <Gauge className="w-4 h-4 text-[var(--primary)]" />
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] font-bold">Book Metrics</h3>
        <span className="text-[10px] text-[var(--text-muted)] ml-auto font-mono">breen · whole account · {m.trades} trades</span>
      </div>

      <div className="grid grid-cols-3 sm:grid-cols-6 gap-4 mb-5">
        <Tile label="Expectancy" value={m.expectancy_usd === undefined ? '—' : fmtUSD(m.expectancy_usd)} cls={tone(m.expectancy_usd ?? 0)} termKey="Expectancy" />
        <Tile label="Profit factor" value={fmtRatio(m.profit_factor)} termKey="Profit Factor" />
        <Tile label="Sharpe" value={fmtRatio(m.sharpe)} termKey="Sharpe Ratio" />
        <Tile label="Sortino" value={fmtRatio(m.sortino)} termKey="Sortino Ratio" />
        <Tile label="Max DD" value={m.max_drawdown_usd ? fmtUSD(-Math.abs(m.max_drawdown_usd)) : '$0'} cls="text-[var(--accent-red)]" termKey="Drawdown" />
        <Tile label="Win rate" value={`${m.win_rate.toFixed(0)}%`} termKey="Win-Rate Variance" />
      </div>

      {gated && (
        <p className="text-[10px] text-[var(--text-muted)] mb-4 font-mono">Sharpe/Sortino shown once the book clears 20 trades ({m.samples ?? m.trades} so far).</p>
      )}

      {/* Cost & excursion row: what the run actually paid to trade, and how far the
          average position ran in each direction before it closed. */}
      <div className="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-5 pt-4 border-t border-white/5">
        <Tile label="Spread paid" value={`$${Math.round(m.spread_cost_usd ?? 0).toLocaleString('en-US')}`} cls="text-[var(--text-muted)]" termKey="Spread Paid" />
        <Tile label="Fees (modelled)" value={`$${Math.round(m.fee_usd ?? 0).toLocaleString('en-US')}`} cls="text-[var(--text-muted)]" termKey="Fees (modelled)" />
        <Tile
          label="Avg MFE"
          value={hasExcursions ? `${(m.avg_mfe_pct ?? 0).toFixed(0)}%` : '—'}
          cls="text-[var(--accent-green)]"
          termKey="Avg MFE"
        />
        <Tile
          label="Avg MAE"
          value={hasExcursions ? `${(m.avg_mae_pct ?? 0).toFixed(0)}%` : '—'}
          cls="text-[var(--accent-red)]"
          termKey="Avg MAE"
        />
      </div>
      <p className="text-[10px] text-[var(--text-muted)] mb-4 font-mono leading-relaxed">
        Spread is already inside realized P&amp;L (buys fill at the ask, mark at the mid) — it explains the number, it doesn&apos;t adjust it.
        Fees are modelled but not charged to cash: net of them the book is {fmtUSD(m.net_pnl_after_fees ?? m.realized_pnl ?? 0)}.
        {hasExcursions ? '' : ' Excursions fill in as trades opened under the new build close.'}
      </p>

      {bands.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5">
                <th className="text-left font-semibold py-2">Entry band</th>
                <th className="text-right font-semibold py-2">Trades</th>
                <th className="text-right font-semibold py-2">Win rate</th>
                <th className="text-right font-semibold py-2">Realized P&amp;L</th>
              </tr>
            </thead>
            <tbody className="font-mono">
              {bands.map(b => (
                <tr key={b.label} className="border-b border-white/[0.04]">
                  <td className="py-1.5 text-[var(--text-main)]">{b.label}</td>
                  <td className="text-right py-1.5 text-[var(--text-muted)]">{b.trades}</td>
                  <td className="text-right py-1.5 text-[var(--text-main)]">{b.win_rate.toFixed(0)}%</td>
                  <td className={`text-right py-1.5 ${tone(b.realized_pnl)}`}>{fmtUSD(b.realized_pnl)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
};

export default BookMetrics;
