import React, { useCallback, useEffect, useRef, useState } from 'react';
import { SlidersHorizontal, Bot } from 'lucide-react';

type Stance = 'bearish' | 'neutral' | 'bullish';
type RiskTier = 'conservative' | 'cautious' | 'balanced' | 'assertive' | 'aggressive';

interface Controls {
  stance: Stance;
  risk_tier: RiskTier;
  match_threshold: number;
  max_slippage_pct: number; // fraction (0.03 = 3%); the UI shows percent
}

const STANCES: Stance[] = ['bearish', 'neutral', 'bullish'];
const TIERS: RiskTier[] = ['conservative', 'cautious', 'balanced', 'assertive', 'aggressive'];

const STANCE_NOTE: Record<Stance, string> = {
  bearish: 'Leans toward SELL/NO. Opposing (bullish) trades are still taken on strong signals, but sized down.',
  neutral: 'No directional bias — takes whichever side the signal implies.',
  bullish: 'Leans toward BUY/YES. Opposing (bearish) trades are still taken on strong signals, but sized down.',
};

const TIER_CAPS: Record<RiskTier, { size: number; exp: number }> = {
  conservative: { size: 1000, exp: 4000 },
  cautious: { size: 1750, exp: 6000 },
  balanced: { size: 2500, exp: 8000 },
  assertive: { size: 3750, exp: 12000 },
  aggressive: { size: 5000, exp: 16000 },
};

const usd = (n: number) => `$${n.toLocaleString('en-US')}`;

const ModelControls: React.FC = () => {
  const [c, setC] = useState<Controls>({ stance: 'neutral', risk_tier: 'balanced', match_threshold: 0.5, max_slippage_pct: 0.03 });
  const [saved, setSaved] = useState(true);
  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    fetch('/api/controls/model')
      .then(r => (r.ok ? r.json() : null))
      .then(d => {
        // Blobs written before the slippage knob existed lack the field — fall
        // back to the backend default rather than 0.
        if (d) setC({ stance: d.stance, risk_tier: d.risk_tier, match_threshold: d.match_threshold, max_slippage_pct: d.max_slippage_pct || 0.03 });
      })
      .catch(() => {});
  }, []);

  const push = useCallback((next: Controls) => {
    setSaved(false);
    if (debounce.current) clearTimeout(debounce.current);
    debounce.current = setTimeout(() => {
      fetch('/api/controls/model', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(next),
      }).then(() => setSaved(true)).catch(() => setSaved(false));
    }, 350);
  }, []);

  const update = (patch: Partial<Controls>) => {
    setC(prev => { const next = { ...prev, ...patch }; push(next); return next; });
  };

  const caps = TIER_CAPS[c.risk_tier];
  const riskIdx = TIERS.indexOf(c.risk_tier);
  const slipPct = c.max_slippage_pct * 100;

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
      <div className="flex items-center gap-2">
        <SlidersHorizontal className="w-4 h-4 text-[var(--primary)]" />
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] font-bold">Model Controls</h3>
        <span className="text-[10px] font-mono ml-auto text-[var(--text-muted)]">
          {saved ? 'applied live' : 'saving…'}
        </span>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
        {/* Stance */}
        <div className="flex flex-col gap-2">
          <span className="text-[11px] font-semibold text-[var(--text-main)]">Market stance</span>
          <div className="flex gap-1.5">
            {STANCES.map(s => (
              <button
                key={s}
                onClick={() => update({ stance: s })}
                className={`flex-1 text-[11px] font-bold capitalize py-1.5 rounded-md border transition-all ${
                  c.stance === s
                    ? 'border-[var(--primary)] bg-[var(--primary)]/15 text-[var(--primary)]'
                    : 'border-white/10 text-[var(--text-muted)] hover:border-white/20'
                }`}
              >
                {s}
              </button>
            ))}
          </div>
          <span className="text-[11px] text-[var(--text-muted)] leading-snug min-h-[44px]">{STANCE_NOTE[c.stance]}</span>
        </div>

        {/* Risk appetite */}
        <div className="flex flex-col gap-2">
          <span className="text-[11px] font-semibold text-[var(--text-main)]">Risk appetite</span>
          <input
            type="range" min={0} max={4} step={1} value={riskIdx}
            onChange={e => update({ risk_tier: TIERS[Number(e.target.value)] })}
            className="w-full accent-[var(--primary)]"
          />
          <div className="flex text-[9px] text-[var(--text-muted)] font-mono">
            {TIERS.map((t, i) => (
              <span key={t}
                className={`flex-1 capitalize ${i === 0 ? 'text-left' : i === TIERS.length - 1 ? 'text-right' : 'text-center'} ${
                  i === riskIdx ? 'text-[var(--primary)] font-bold' : ''}`}>
                {t}
              </span>
            ))}
          </div>
          <span className="text-[11px] text-[var(--text-muted)] leading-snug min-h-[44px]">
            Max position {usd(caps.size)} · max exposure {usd(caps.exp)} per trade.
          </span>
        </div>

        {/* Selectivity */}
        <div className="flex flex-col gap-2">
          <span className="text-[11px] font-semibold text-[var(--text-main)]">Selectivity</span>
          <input
            type="range" min={0.35} max={0.8} step={0.05} value={c.match_threshold}
            onChange={e => update({ match_threshold: Math.round(Number(e.target.value) * 100) / 100 })}
            className="w-full accent-[var(--primary)]"
          />
          <div className="flex justify-between text-[10px] text-[var(--text-muted)] font-mono">
            <span>Trade often</span><span className="text-[var(--primary)]">{c.match_threshold.toFixed(2)}</span><span>High-conviction</span>
          </div>
          <span className="text-[11px] text-[var(--text-muted)] leading-snug min-h-[44px]">
            Only acts on news→market matches scoring ≥ {c.match_threshold.toFixed(2)}. Higher = fewer, surer trades.
          </span>
        </div>

        {/* Max slippage (fleet-wide execution cap; stored as a fraction, shown as %) */}
        <div className="flex flex-col gap-2">
          <span className="text-[11px] font-semibold text-[var(--text-main)]">Max slippage</span>
          <input
            type="range" min={0.1} max={10} step={0.1} value={slipPct}
            onChange={e => update({ max_slippage_pct: Math.round(Number(e.target.value) * 10) / 1000 })}
            className="w-full accent-[var(--primary)]"
          />
          <div className="flex justify-between text-[10px] text-[var(--text-muted)] font-mono">
            <span>0.1%</span><span className="text-[var(--primary)]">{slipPct.toFixed(1)}%</span><span>10%</span>
          </div>
          <span className="text-[11px] text-[var(--text-muted)] leading-snug min-h-[44px]">
            Rejects trades whose estimated price impact exceeds {slipPct.toFixed(1)}%. Applies fleet-wide, to every persona.
          </span>
        </div>
      </div>

      <div className="flex items-start gap-2 bg-[var(--primary)]/10 border border-[var(--primary)]/20 rounded-xl px-4 py-2.5">
        <Bot className="w-4 h-4 text-[var(--primary)] mt-0.5 shrink-0" />
        <span className="text-[11px] text-[var(--text-main)] leading-snug">
          Gordon is <b className="capitalize">{c.stance}</b>, sizing <b className="capitalize">{c.risk_tier}</b> (≤ {usd(caps.exp)}/trade),
          acting only on matches ≥ <b>{c.match_threshold.toFixed(2)}</b>, skipping fills slipping &gt; <b>{slipPct.toFixed(1)}%</b>. Paper mode — proposals only.
        </span>
      </div>
    </section>
  );
};

export default ModelControls;
