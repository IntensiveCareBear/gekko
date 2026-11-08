import React, { useState, useMemo, useEffect, useCallback } from 'react';
import {
  ComposedChart, Area, Line, Bar, XAxis, YAxis, CartesianGrid, Tooltip as ReTooltip,
  ResponsiveContainer, PieChart, Pie, Cell, Label,
} from 'recharts';
import { TrendingUp, BarChart2, PieChart as PieIcon, List, ShieldCheck, Clock } from 'lucide-react';
import GlossaryTooltip from './GlossaryTooltip';
import PnLHero from './PnLHero';
import BookMetrics from './BookMetrics';
import CalibrationTable from './CalibrationTable';
import {
  computeKPIs, generatePnLBins,
  type EquityPoint, type TradeRecord, type AssetAllocation, type PnLBin, type PortfolioKPIs,
} from '../data/portfolioData';

// ─── Live API shapes (from liveview /api/*) ──────────────────────────────────

interface ApiAccount {
  starting_capital: number;
  cash: number;
  nav: number;
  realized_pnl: number;
  unrealized_pnl: number;
  open_positions: number;
  peak_nav: number;
  drawdown_pct: number;
  max_drawdown_pct?: number;
}
interface ApiTrade {
  contract_id: string; exchange: string; side: string;
  entry_price: number; exit_price: number; size: number;
  realized_pnl: number; return_pct: number;
  opened_at_ms: number; closed_at_ms: number; status: string;
}
interface ApiEquityPoint {
  timestamp_ms: number; nav: number; cash: number;
  realized_pnl: number; unrealized_pnl: number; open_positions: number; drawdown_pct: number;
  benchmark_nav?: number;
}
interface ApiPosition { contract_id: string; entry_price: number; size: number; }

function inferExchange(contractID: string): 'kalshi' | 'polymarket' {
  const c = contractID.toLowerCase();
  return c.includes('btc') || c.includes('eth') || c.includes('poly') ? 'polymarket' : 'kalshi';
}

// ─── Time-range filter helper ─────────────────────────────────────────────────

type Range = '1D' | '1W' | '1M' | 'YTD' | 'All';

function filterByRange(points: EquityPoint[], range: Range): EquityPoint[] {
  if (range === 'All') return points;
  const now = points[points.length - 1]?.timestampMs ?? Date.now();
  const cutoff: Record<Range, number> = {
    '1D': now - 86_400_000,
    '1W': now - 7 * 86_400_000,
    '1M': now - 30 * 86_400_000,
    'YTD': new Date(new Date(now).getFullYear(), 0, 1).getTime(),
    All: 0,
  };
  return points.filter(p => p.timestampMs >= cutoff[range]);
}

// ─── Shared tooltip style ─────────────────────────────────────────────────────

const tooltipStyle: React.CSSProperties = {
  background: 'var(--card-bg)',
  border: '1px solid var(--border-color)',
  borderRadius: 10,
  fontSize: 11,
  fontFamily: 'var(--font-mono, monospace)',
  color: 'var(--text-main)',
  boxShadow: '0 4px 20px rgba(0,0,0,0.25)',
  backdropFilter: 'blur(8px)',
};

const fmt = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 });
const fmtExact = (v: number) =>
  new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(v);

// ─── Section header + KPI pill ───────────────────────────────────────────────

const SectionHeader: React.FC<{ icon: React.ReactNode; label: string }> = ({ icon, label }) => (
  <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2 mb-4">
    <span className="text-[var(--primary)]">{icon}</span>
    {label}
  </h3>
);

const KPIPill: React.FC<{ label: string; value: string; color?: string; termKey?: string }> = ({
  label, value, color = 'text-[var(--text-main)]', termKey,
}) => (
  <div className="flex flex-col gap-1 bg-white/[0.03] border border-white/[0.06] rounded-xl px-4 py-3">
    {termKey ? (
      <GlossaryTooltip termKey={termKey} className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">
        {label}
      </GlossaryTooltip>
    ) : (
      <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">{label}</span>
    )}
    <span className={`text-base font-bold font-mono ${color}`}>{value}</span>
  </div>
);

// ─── Chart 1: Equity Curve + Drawdown ────────────────────────────────────────

const EquityCurveChart: React.FC<{ equity: EquityPoint[]; kpis: PortfolioKPIs }> = ({ equity, kpis }) => {
  const [range, setRange] = useState<Range>('All');
  const data = useMemo(() => filterByRange(equity, range), [equity, range]);

  // Adaptive axis scaling so labels aren't all identical when NAV/drawdown move
  // only slightly (e.g. all rounding to "$100k" / "0.0%").
  const axis = useMemo(() => {
    if (data.length === 0) {
      return {
        navDomain: ['auto', 'auto'] as const,
        ddDomain: [0, 'auto'] as const,
        navFmt: (v: number) => `$${(v / 1000).toFixed(0)}k`,
        ddFmt: (v: number) => `${v.toFixed(1)}%`,
      };
    }
    // Include the benchmark line in the NAV axis domain so it stays in view.
    const navs = data.flatMap(d =>
      typeof d.benchmarkNav === 'number' && d.benchmarkNav > 0 ? [d.nav, d.benchmarkNav] : [d.nav],
    );
    const navMin = Math.min(...navs);
    const navMax = Math.max(...navs);
    const navPad = Math.max((navMax - navMin) * 0.1, 1);
    const spanK = (navMax - navMin) / 1000;
    const navDec = spanK >= 10 ? 0 : spanK >= 1 ? 1 : 2;

    const ddMax = Math.max(0, ...data.map(d => d.drawdown));
    const ddDec = ddMax >= 1 ? 1 : ddMax >= 0.1 ? 2 : 3;

    return {
      navDomain: [navMin - navPad, navMax + navPad] as [number, number],
      ddDomain: [0, Math.max(ddMax * 1.25, 0.1)] as [number, number],
      navFmt: (v: number) => `$${(v / 1000).toFixed(navDec)}k`,
      ddFmt: (v: number) => `${v.toFixed(ddDec)}%`,
    };
  }, [data]);

  // Latest NAV-vs-benchmark comparison (buy-and-hold SPY with the same seed money).
  const benchmark = useMemo(() => {
    const withBench = data.filter(d => typeof d.benchmarkNav === 'number' && d.benchmarkNav! > 0);
    if (withBench.length === 0) return null;
    const last = withBench[withBench.length - 1];
    const benchNav = last.benchmarkNav!;
    const deltaPct = benchNav > 0 ? (last.nav / benchNav - 1) * 100 : 0;
    return { benchNav, deltaPct };
  }, [data]);

  const CustomTooltip = ({ active, payload, label }: any) => {
    if (!active || !payload?.length) return null;
    const nav = payload.find((p: any) => p.dataKey === 'nav')?.value ?? 0;
    const dd = payload.find((p: any) => p.dataKey === 'drawdown')?.value ?? 0;
    const bench = payload.find((p: any) => p.dataKey === 'benchmarkNav')?.value;
    const ts = payload[0]?.payload?.timestampMs;
    return (
      <div style={tooltipStyle} className="p-3 flex flex-col gap-1">
        <span className="text-[var(--text-muted)] text-[10px]">{label}</span>
        {ts && <span className="text-[9px] text-[var(--text-muted)]">Unix: {ts}</span>}
        <span className="text-[var(--accent-green)] font-bold">NAV: {fmtExact(nav)}</span>
        {typeof bench === 'number' && bench > 0 && (
          <span className="text-[var(--primary)]">SPY hold: {fmtExact(bench)}</span>
        )}
        <span className="text-[var(--accent-red)]">Drawdown: {dd.toFixed(2)}%</span>
      </div>
    );
  };

  const navColor = 'var(--accent-green)';
  const ddColor = 'var(--accent-red)';
  // SPY benchmark uses a dedicated blue rather than --primary: in several themes
  // --primary is a pink/red that's near-identical to --accent-red (drawdown),
  // making the two lines indistinguishable. --chart-benchmark is a distinct hue
  // from both the green NAV area and the red drawdown line across every theme.
  const benchColor = 'var(--chart-benchmark)';

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md">
      <div className="flex justify-between items-center mb-1">
        <SectionHeader icon={<TrendingUp className="w-4 h-4" />} label="Chart 1 // Cumulative Equity Curve & Drawdown" />
        <div className="flex gap-1 font-mono text-[10px] mb-4">
          {(['1D', '1W', '1M', 'YTD', 'All'] as Range[]).map(r => (
            <button
              key={r}
              type="button"
              onClick={() => setRange(r)}
              className={`px-2.5 py-1 rounded-md border transition-all cursor-pointer font-bold ${
                range === r
                  ? 'bg-[var(--primary)]/20 border-[var(--primary)]/40 text-[var(--primary)]'
                  : 'bg-white/[0.03] border-white/10 text-[var(--text-muted)] hover:border-white/20'
              }`}
            >
              {r}
            </button>
          ))}
        </div>
      </div>

      {/* Curve-quality KPIs (P&L / drawdown live in the hero; win-rate with the
          distribution chart — kept here only what's specific to the equity curve). */}
      <div className="grid grid-cols-2 gap-3 mb-5">
        <KPIPill label="Sharpe (annl.)" value={kpis.sharpeApprox.toFixed(2)} color="text-[var(--primary)]" termKey="Sharpe Ratio" />
        {benchmark && (
          <KPIPill
            label="vs SPY"
            value={`${benchmark.deltaPct >= 0 ? '+' : ''}${benchmark.deltaPct.toFixed(2)}%`}
            color={benchmark.deltaPct >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]'}
            termKey="vs SPY"
          />
        )}
      </div>

      <ResponsiveContainer width="100%" height={280}>
        <ComposedChart data={data} margin={{ top: 4, right: 16, bottom: 0, left: 8 }}>
          <defs>
            <linearGradient id="navGrad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="5%" stopColor={navColor} stopOpacity={0.25} />
              <stop offset="95%" stopColor={navColor} stopOpacity={0.02} />
            </linearGradient>
            <linearGradient id="ddGrad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="5%" stopColor={ddColor} stopOpacity={0.35} />
              <stop offset="95%" stopColor={ddColor} stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid strokeDasharray="3 3" stroke="rgba(255,255,255,0.04)" vertical={false} />
          <XAxis
            dataKey="date"
            tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }}
            tickLine={false}
            axisLine={false}
            interval="preserveStartEnd"
            minTickGap={40}
          />
          <YAxis
            yAxisId="nav"
            orientation="left"
            tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }}
            tickLine={false}
            axisLine={false}
            domain={axis.navDomain}
            tickFormatter={axis.navFmt}
            width={56}
            allowDecimals
          />
          <YAxis
            yAxisId="dd"
            orientation="right"
            tick={{ fill: 'rgba(244,63,94,0.7)', fontSize: 9, fontFamily: 'var(--font-mono)' }}
            tickLine={false}
            axisLine={false}
            domain={axis.ddDomain}
            tickFormatter={axis.ddFmt}
            width={44}
            allowDecimals
            reversed
          />
          <ReTooltip content={<CustomTooltip />} cursor={{ stroke: 'rgba(255,255,255,0.07)', strokeWidth: 1 }} />
          <Area yAxisId="nav" type="monotone" dataKey="nav" stroke={navColor} strokeWidth={2} fill="url(#navGrad)" dot={false} activeDot={{ r: 4, strokeWidth: 0 }} isAnimationActive={false} />
          {benchmark && (
            <Line yAxisId="nav" type="monotone" dataKey="benchmarkNav" stroke={benchColor} strokeWidth={1.5} dot={false} activeDot={{ r: 3, strokeWidth: 0 }} strokeDasharray="5 3" isAnimationActive={false} connectNulls />
          )}
          <Area yAxisId="dd" type="monotone" dataKey="drawdown" stroke={ddColor} strokeWidth={1.5} fill="url(#ddGrad)" dot={false} activeDot={{ r: 3, strokeWidth: 0 }} strokeDasharray="4 2" isAnimationActive={false} />
        </ComposedChart>
      </ResponsiveContainer>

      <div className="flex gap-5 mt-3 text-[10px] font-mono text-[var(--text-muted)]">
        <span className="flex items-center gap-1.5">
          <span className="w-3 h-0.5 rounded" style={{ background: navColor }} />
          <GlossaryTooltip termKey="NAV">NAV (Portfolio Value)</GlossaryTooltip>
        </span>
        <span className="flex items-center gap-1.5">
          <span className="w-3 h-px rounded border-t border-dashed" style={{ borderColor: ddColor }} />
          <GlossaryTooltip termKey="Drawdown">Drawdown %</GlossaryTooltip>
        </span>
        {benchmark && (
          <span className="flex items-center gap-1.5">
            <span className="w-3 h-px rounded border-t border-dashed" style={{ borderColor: benchColor }} />
            SPY buy &amp; hold
          </span>
        )}
      </div>
    </section>
  );
};

// ─── Chart 2: PnL Distribution + Edge KPIs ───────────────────────────────────

const PnLDistributionChart: React.FC<{ bins: PnLBin[]; kpis: PortfolioKPIs }> = ({ bins, kpis }) => {
  const CustomTooltip = ({ active, payload }: any) => {
    if (!active || !payload?.length) return null;
    const d = payload[0].payload;
    return (
      <div style={tooltipStyle} className="p-3 flex flex-col gap-1">
        <span className="font-bold text-[var(--text-main)]">{d.label}</span>
        <span className="text-[var(--text-muted)]">Trades: <b className="text-white">{d.count}</b></span>
        <span className={d.totalPnL >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]'}>
          Total PnL: {fmtExact(d.totalPnL)}
        </span>
      </div>
    );
  };

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md">
      <SectionHeader icon={<BarChart2 className="w-4 h-4" />} label="Chart 2 // PnL Distribution & Win-Rate Variance" />

      <div className="flex flex-col lg:flex-row gap-5">
        <div className="flex-1 min-w-0">
          <ResponsiveContainer width="100%" height={260}>
            <ComposedChart data={bins} layout="vertical" margin={{ top: 0, right: 12, bottom: 0, left: 4 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="rgba(255,255,255,0.04)" horizontal={false} />
              <XAxis type="number" dataKey="count" tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }} tickLine={false} axisLine={false} allowDecimals={false} />
              <YAxis type="category" dataKey="label" width={108} tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }} tickLine={false} axisLine={false} />
              <ReTooltip content={<CustomTooltip />} cursor={{ fill: 'rgba(255,255,255,0.03)' }} />
              <Bar dataKey="count" radius={[0, 4, 4, 0]} maxBarSize={20} isAnimationActive={false}>
                {bins.map((bin, idx) => (
                  <Cell key={idx} fill={bin.profitable ? 'var(--accent-green)' : 'var(--accent-red)'} fillOpacity={0.8} />
                ))}
              </Bar>
            </ComposedChart>
          </ResponsiveContainer>
        </div>

        <div className="flex flex-col gap-3 lg:w-52 shrink-0">
          <KPIPill label="Profit Factor" value={kpis.profitFactor.toFixed(2)} color="text-[var(--primary)]" termKey="Profit Factor" />
          <KPIPill label="Win Rate" value={`${kpis.winRate}%`} color="text-[var(--accent-green)]" termKey="Win-Rate Variance" />
          <KPIPill label="Avg Winning Trade" value={fmtExact(kpis.avgWin)} color="text-[var(--accent-green)]" termKey="Average Win/Loss" />
          <KPIPill label="Avg Losing Trade" value={fmtExact(-kpis.avgLoss)} color="text-[var(--accent-red)]" termKey="Average Win/Loss" />
          <KPIPill label="Total Trades" value={String(kpis.totalTrades)} termKey="Total Trades" />
        </div>
      </div>
    </section>
  );
};

// ─── Chart 3: Asset Allocation Donut ─────────────────────────────────────────

const AssetAllocationChart: React.FC<{
  exchangeSlices: AssetAllocation[];
  capitalSlices: AssetAllocation[];
  nav: number;
}> = ({ exchangeSlices, capitalSlices, nav }) => {
  const DonutTooltip = ({ active, payload }: any) => {
    if (!active || !payload?.length) return null;
    const d = payload[0].payload;
    return (
      <div style={tooltipStyle} className="p-3 flex flex-col gap-1">
        <span className="font-bold text-[var(--text-main)]">{d.name}</span>
        <span className="text-[var(--text-muted)]">{fmt.format(d.value)}</span>
      </div>
    );
  };

  const CenterLabel = ({ viewBox }: any) => {
    const { cx, cy } = viewBox;
    return (
      <g>
        <text x={cx} y={cy - 8} textAnchor="middle" style={{ fill: '#9ca3af' }} fontSize={9} fontFamily="var(--font-mono)">FUND EQUITY</text>
        <text x={cx} y={cy + 10} textAnchor="middle" style={{ fill: '#f3f4f6' }} fontSize={13} fontWeight="bold" fontFamily="var(--font-mono)">{fmt.format(nav)}</text>
      </g>
    );
  };

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md">
      <SectionHeader icon={<PieIcon className="w-4 h-4" />} label="Chart 3 // Asset Class & Exchange Allocation" />

      <div className="flex flex-col sm:flex-row gap-6 items-center">
        <div className="w-full sm:w-auto flex-1 min-w-0">
          <ResponsiveContainer width="100%" height={240}>
            <PieChart>
              <Pie data={exchangeSlices} dataKey="value" nameKey="name" cx="50%" cy="50%" outerRadius={90} innerRadius={62} paddingAngle={3} stroke="none" isAnimationActive={false}>
                {exchangeSlices.map((entry, idx) => (<Cell key={idx} fill={entry.fill} fillOpacity={0.85} />))}
                <Label content={<CenterLabel />} position="center" />
              </Pie>
              <Pie data={capitalSlices} dataKey="value" nameKey="name" cx="50%" cy="50%" outerRadius={56} innerRadius={36} paddingAngle={2} stroke="none" isAnimationActive={false}>
                {capitalSlices.map((entry, idx) => (<Cell key={idx} fill={entry.fill} fillOpacity={0.7} />))}
              </Pie>
              <ReTooltip content={<DonutTooltip />} />
            </PieChart>
          </ResponsiveContainer>
        </div>

        <div className="flex flex-col gap-3 font-mono text-xs shrink-0 sm:w-44">
          <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold border-b border-white/5 pb-1.5">Exchange Exposure</span>
          {exchangeSlices.map((s, i) => (
            <div key={i} className="flex justify-between items-center gap-3">
              <span className="flex items-center gap-1.5 text-[var(--text-muted)]">
                <span className="w-2.5 h-2.5 rounded-sm shrink-0" style={{ background: s.fill }} />
                {s.name}
              </span>
              <span className="font-bold text-[var(--text-main)] text-right">{fmt.format(s.value)}</span>
            </div>
          ))}
          <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold border-b border-white/5 pb-1.5 mt-2">Capital Split</span>
          {capitalSlices.map((s, i) => (
            <div key={i} className="flex justify-between items-center gap-3">
              <span className="flex items-center gap-1.5 text-[var(--text-muted)]">
                <span className="w-2.5 h-2.5 rounded-sm shrink-0" style={{ background: s.fill }} />
                <GlossaryTooltip termKey={s.name} className="text-[var(--text-muted)]">{s.name}</GlossaryTooltip>
              </span>
              <span className="font-bold text-[var(--text-main)] text-right">{fmt.format(s.value)}</span>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
};

// ─── Chart 4: Closed Position Ledger ─────────────────────────────────────────

const PAGE_SIZE = 15;

const ClosedPositionLedger: React.FC<{ trades: TradeRecord[] }> = ({ trades }) => {
  const [page, setPage] = useState(0);
  const [selected, setSelected] = useState<TradeRecord | null>(null);

  const sorted = useMemo(() => [...trades].sort((a, b) => b.closedAtMs - a.closedAtMs), [trades]);
  const totalPages = Math.max(1, Math.ceil(sorted.length / PAGE_SIZE));
  const rows = sorted.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md">
      <div className="flex justify-between items-center">
        <SectionHeader icon={<List className="w-4 h-4" />} label="Chart 4 // Closed Position Ledger" />
        <span className="text-[10px] font-mono text-[var(--text-muted)] mb-4">{sorted.length} records</span>
      </div>

      {/* Settlement record modal */}
      {selected && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm" onClick={() => setSelected(null)}>
          <div className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-6 max-w-lg w-full mx-4 shadow-2xl" onClick={e => e.stopPropagation()}>
            <div className="flex justify-between items-center mb-4">
              <h4 className="text-sm font-bold flex items-center gap-2 font-mono">
                <ShieldCheck className="w-4 h-4 text-[var(--accent-green)]" />
                Settlement Record
              </h4>
              <button type="button" className="text-[var(--text-muted)] hover:text-white text-lg leading-none cursor-pointer" onClick={() => setSelected(null)}>×</button>
            </div>
            <div className="grid grid-cols-2 gap-3 font-mono text-xs">
              <div><span className="text-[var(--text-muted)] block">Contract</span><span className="font-bold">{selected.contractId}</span></div>
              <div><span className="text-[var(--text-muted)] block">Resolution</span><span className="font-bold">{selected.side}</span></div>
              <div><span className="text-[var(--text-muted)] block">Entry → Exit</span><span className="font-bold">${selected.entryPrice.toFixed(4)} → ${selected.exitPrice.toFixed(4)}</span></div>
              <div><span className="text-[var(--text-muted)] block">Size</span><span className="font-bold">{selected.size}</span></div>
              <div><span className="text-[var(--text-muted)] block">Realized PnL</span><span className={`font-bold ${selected.realizedPnL >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]'}`}>{fmtExact(selected.realizedPnL)}</span></div>
              <div><span className="text-[var(--text-muted)] block">Return</span><span className={`font-bold ${selected.returnPct >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]'}`}>{selected.returnPct.toFixed(2)}%</span></div>
              <div><span className="text-[var(--text-muted)] block">Opened</span><span className="font-bold">{new Date(selected.openedAtMs).toLocaleString()}</span></div>
              <div><span className="text-[var(--text-muted)] block">Closed</span><span className="font-bold">{new Date(selected.closedAtMs).toLocaleString()}</span></div>
            </div>
            <p className="text-[var(--text-muted)] text-[10px] border-t border-white/5 pt-3 mt-3">
              Recorded to the immutable WORM ledger ({selected.status}). Paper-trading run — no real funds moved.
            </p>
          </div>
        </div>
      )}

      <div className="overflow-x-auto -mx-1">
        <table className="w-full text-[10.5px] font-mono border-collapse">
          <thead>
            <tr className="border-b border-white/5">
              {([
                { label: 'Contract' },
                { label: 'Exchange' },
                { label: 'Side', termKey: 'Contract Side' },
                { label: 'Entry' },
                { label: 'Exit' },
                { label: 'PnL ($)', termKey: 'PnL' },
                { label: 'Return', termKey: 'Realized Return' },
                { label: 'Closed' },
                { label: 'Audit', termKey: 'WORM Ledger Entry Hash' },
              ] as { label: string; termKey?: string }[]).map(h => (
                <th key={h.label} className="text-left py-2 px-2 text-[var(--text-muted)] font-semibold uppercase tracking-wider whitespace-nowrap">
                  {h.termKey ? (
                    <GlossaryTooltip termKey={h.termKey} className="text-[var(--text-muted)] font-semibold uppercase tracking-wider">{h.label}</GlossaryTooltip>
                  ) : h.label}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr><td colSpan={9} className="py-6 text-center text-[var(--text-muted)] italic">No closed trades yet — positions settle at expiry.</td></tr>
            )}
            {rows.map((t, idx) => {
              const win = t.realizedPnL >= 0;
              return (
                <tr key={t.id} className={`border-b border-white/[0.04] transition-colors ${idx % 2 === 0 ? 'bg-white/[0.015]' : ''} hover:bg-white/[0.04]`}>
                  <td className="py-2 px-2 text-slate-100 font-bold whitespace-nowrap">{t.contractId}</td>
                  <td className="py-2 px-2">
                    <span className={`px-1.5 py-0.5 rounded text-[9px] font-bold border ${t.exchange === 'kalshi' ? 'bg-blue-500/10 border-blue-500/20 text-blue-400' : 'bg-purple-500/10 border-purple-500/20 text-purple-400'}`}>
                      {t.exchange.toUpperCase()}
                    </span>
                  </td>
                  <td className="py-2 px-2">
                    <span className={`px-1.5 py-0.5 rounded text-[9px] font-bold border ${t.side === 'YES' ? 'bg-[var(--glow-green)] border-[var(--accent-green)]/20 text-[var(--accent-green)]' : 'bg-[var(--glow-red)] border-[var(--accent-red)]/20 text-[var(--accent-red)]'}`}>
                      {t.side}
                    </span>
                  </td>
                  <td className="py-2 px-2 text-[var(--text-muted)]">${t.entryPrice.toFixed(4)}</td>
                  <td className="py-2 px-2 text-[var(--text-muted)]">${t.exitPrice.toFixed(4)}</td>
                  <td className={`py-2 px-2 font-bold ${win ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]'}`}>{win ? '+' : ''}{fmtExact(t.realizedPnL)}</td>
                  <td className={`py-2 px-2 font-bold ${win ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]'}`}>{win ? '+' : ''}{t.returnPct.toFixed(2)}%</td>
                  <td className="py-2 px-2 text-[var(--text-muted)] whitespace-nowrap">{new Date(t.closedAtMs).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })}</td>
                  <td className="py-2 px-2">
                    <button type="button" onClick={() => setSelected(t)} className="flex items-center gap-1 text-[var(--primary)] hover:text-white transition-colors cursor-pointer text-[9px] font-bold border border-[var(--primary)]/20 hover:border-[var(--primary)]/60 bg-[var(--primary)]/5 hover:bg-[var(--primary)]/10 px-2 py-0.5 rounded" title="View settlement record">
                      <ShieldCheck className="w-2.5 h-2.5" /> WORM
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <div className="flex justify-between items-center mt-4 font-mono text-xs text-[var(--text-muted)]">
        <span>Page {page + 1} / {totalPages}</span>
        <div className="flex gap-2">
          <button type="button" disabled={page === 0} onClick={() => setPage(p => p - 1)} className="px-3 py-1 rounded border border-white/10 hover:border-white/25 disabled:opacity-30 cursor-pointer transition-colors">← Prev</button>
          <button type="button" disabled={page >= totalPages - 1} onClick={() => setPage(p => p + 1)} className="px-3 py-1 rounded border border-white/10 hover:border-white/25 disabled:opacity-30 cursor-pointer transition-colors">Next →</button>
        </div>
      </div>
    </section>
  );
};

// ─── Main Portfolio Analytics Container (live data) ──────────────────────────

export const PortfolioAnalytics: React.FC = () => {
  const [account, setAccount] = useState<ApiAccount | null>(null);
  const [trades, setTrades] = useState<TradeRecord[]>([]);
  const [equity, setEquity] = useState<EquityPoint[]>([]);
  const [positions, setPositions] = useState<ApiPosition[]>([]);
  const [loaded, setLoaded] = useState(false);

  const fetchAll = useCallback(async () => {
    try {
      const [accRes, trRes, eqRes, posRes] = await Promise.all([
        fetch('/api/account'), fetch('/api/trades'), fetch('/api/equity_curve'), fetch('/api/positions'),
      ]);
      if (accRes.ok) setAccount(await accRes.json());
      if (trRes.ok) {
        const raw: ApiTrade[] = await trRes.json();
        setTrades(raw.map((t, i) => ({
          id: `${t.contract_id}-${t.closed_at_ms}-${i}`,
          strategyId: 'AUTO',
          contractId: t.contract_id,
          exchange: (t.exchange === 'polymarket' ? 'polymarket' : 'kalshi'),
          side: t.side,
          entryPrice: t.entry_price,
          exitPrice: t.exit_price,
          size: t.size,
          realizedPnL: t.realized_pnl,
          returnPct: t.return_pct,
          openedAtMs: t.opened_at_ms,
          closedAtMs: t.closed_at_ms,
          status: t.status,
        })));
      }
      if (eqRes.ok) {
        const raw: ApiEquityPoint[] = await eqRes.json();
        setEquity(raw.map(p => ({
          timestampMs: p.timestamp_ms,
          date: new Date(p.timestamp_ms).toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }),
          nav: p.nav,
          drawdown: p.drawdown_pct,
          benchmarkNav: p.benchmark_nav,
        })));
      }
      if (posRes.ok) {
        const raw = await posRes.json();
        if (Array.isArray(raw)) setPositions(raw);
      }
    } catch {
      /* leave last-known values; container shows them or the empty state */
    } finally {
      setLoaded(true);
    }
  }, []);

  useEffect(() => {
    fetchAll();
    const id = setInterval(fetchAll, 8000);
    return () => clearInterval(id);
  }, [fetchAll]);

  const kpis = useMemo(() => {
    const base = computeKPIs(trades, equity);
    if (account) {
      base.totalPnL = Math.round((account.nav - account.starting_capital) * 100) / 100;
    }
    return base;
  }, [trades, equity, account]);

  const bins = useMemo(() => generatePnLBins(trades), [trades]);

  const allocation = useMemo(() => {
    const cash = account?.cash ?? 0;
    // Open exposure split by exchange (from live open positions).
    let kalshi = 0, poly = 0;
    for (const p of positions) {
      const notional = p.entry_price * p.size;
      if (inferExchange(p.contract_id) === 'polymarket') poly += notional; else kalshi += notional;
    }
    const locked = kalshi + poly;
    const exchangeSlices: AssetAllocation[] = [
      { name: 'Kalshi', value: Math.round(kalshi), fill: 'var(--primary)' },
      { name: 'Polymarket', value: Math.round(poly), fill: '#a78bfa' },
    ];
    const capitalSlices: AssetAllocation[] = [
      { name: 'Cash Balance', value: Math.round(cash), fill: 'var(--accent-green)' },
      { name: 'Locked Margin', value: Math.round(locked), fill: 'var(--accent-red)' },
    ];
    return { exchangeSlices, capitalSlices };
  }, [account, positions]);

  const nav = account?.nav ?? 0;
  const startingCapital = account?.starting_capital ?? 0;

  if (!loaded) {
    return (
      <div className="max-w-[1400px] mx-auto mt-20 flex flex-col items-center gap-3 text-[var(--text-muted)]">
        <Clock className="w-8 h-8 animate-spin-slow" />
        <p className="text-sm font-mono">Loading paper-run data…</p>
      </div>
    );
  }

  const runStarted = startingCapital > 0;

  return (
    <div className="max-w-[1400px] mx-auto mt-8 px-6 pb-16 flex flex-col gap-8">
      {!runStarted && (
        <div className="bg-amber-500/10 border border-amber-500/30 rounded-2xl p-4 font-mono text-xs text-amber-300 flex items-center gap-2">
          <Clock className="w-4 h-4 shrink-0" />
          No active paper run detected. Start the stack with <code>./run.sh</code> (breen seeds the $100k account).
        </div>
      )}
      {runStarted && trades.length === 0 && (
        <div className="bg-[var(--primary)]/10 border border-[var(--primary)]/30 rounded-2xl p-4 font-mono text-xs text-[var(--primary)] flex items-center gap-2">
          <Clock className="w-4 h-4 shrink-0" />
          Run is live — no positions have settled yet. The equity curve and trade ledger fill in as contracts reach expiry.
        </div>
      )}

      {/* P&L hero — model controls moved to the Controls tab */}
      <PnLHero
        equity={equity}
        startingCapital={startingCapital}
        nav={nav}
        drawdownPct={account?.max_drawdown_pct ?? account?.drawdown_pct ?? 0}
        openPositions={account?.open_positions ?? positions.length}
        lockedMargin={allocation.capitalSlices.find(s => s.name === 'Locked Margin')?.value ?? 0}
      />
      <BookMetrics />
      <CalibrationTable />

      <EquityCurveChart equity={equity} kpis={kpis} />

      <div className="grid grid-cols-1 xl:grid-cols-2 gap-8">
        <PnLDistributionChart bins={bins} kpis={kpis} />
        <AssetAllocationChart exchangeSlices={allocation.exchangeSlices} capitalSlices={allocation.capitalSlices} nav={nav} />
      </div>

      <ClosedPositionLedger trades={trades} />
    </div>
  );
};

export default PortfolioAnalytics;
