import React, { useEffect, useRef, useState } from 'react';
import { Cpu, AlertTriangle, Timer, Layers, Activity, Zap } from 'lucide-react';
import {
  AreaChart, Area, XAxis, YAxis, CartesianGrid, Tooltip as ReTooltip, ResponsiveContainer, ReferenceLine,
} from 'recharts';
import SystemTelemetry from './SystemTelemetry';
import { PERSONA_LABEL } from '../data/personas';

import type { Theme } from '../App';

// ─── Data contract (the gordon component of liveview /api/status) ─────────────

interface PersonaHealth {
  persona: string;
  reachable: boolean;
  proposals_generated: number;
  llm_proposals?: number;
  llm_fallbacks?: number;
}

interface ComponentStatus {
  component: string;
  reachable: boolean;
  status: string;
  stats?: Record<string, number | string | boolean | PersonaHealth[]>;
}

interface StatusResponse {
  components: ComponentStatus[];
}

// num safely reads a numeric stat (values arrive as number | string | boolean | array).
const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

// priorityBadge maps a gordon's priority weight (gekko:gordon_priority, set on the
// Controls tab) to a read-only badge. Weight 1 / unset = normal ⇒ no badge.
function priorityBadge(w: number | undefined): { label: string; cls: string } | null {
  if (w === undefined || w === 1) return null;
  if (w === 0) return { label: 'Paused', cls: 'bg-rose-500/10 border-rose-500/30 text-rose-400' };
  if (w < 1) return { label: 'Demoted', cls: 'bg-amber-500/10 border-amber-500/30 text-amber-400' };
  return { label: 'Promoted', cls: 'bg-[var(--primary)]/10 border-[var(--primary)]/30 text-[var(--primary)]' };
}

// ratioColor grades the fallback ratio: low is healthy (LLM doing the work), high is bad.
function ratioColor(pct: number): string {
  if (pct >= 50) return 'var(--accent-red)';
  if (pct >= 20) return 'var(--accent-amber, #f59e0b)';
  return 'var(--accent-green)';
}

// A single sampled point on the fallback-ratio timeline.
interface RatioSample {
  t: number;   // wall-clock ms, for the tooltip
  label: string; // hh:mm:ss for the axis
  pct: number; // cumulative fallback ratio (running total)
  instPct: number | null; // fallback ratio of just the decisions since the last sample
}

// Keep ~10 minutes of history at the 3s poll cadence.
const MAX_SAMPLES = 200;

// Rolling window for the per-persona fallback rate. The cumulative ratio barely
// moves once history piles up, so we grade each persona over just the recent
// window instead — recent fallbacks show up instead of being diluted to ~0%.
const PERSONA_WINDOW_MS = 120_000; // 2 minutes

const SystemMetrics: React.FC<{ theme: Theme }> = ({ theme }) => {
  const [gordon, setGordon] = useState<ComponentStatus | null>(null);
  const [reachable, setReachable] = useState<boolean | null>(null);
  const [history, setHistory] = useState<RatioSample[]>([]);
  // Guard against double-counting the same backend snapshot into the timeline.
  const lastDecidedRef = useRef<number>(-1);
  // Previous cumulative counts, so we can derive the per-interval (instantaneous) rate.
  const prevCountsRef = useRef<{ proposals: number; fallbacks: number } | null>(null);
  // Rolling window of per-persona cumulative counts, for the windowed fallback rate.
  const personaHistRef = useRef<{ t: number; counts: Record<string, { p: number; f: number }> }[]>([]);
  const [personaRates, setPersonaRates] = useState<Record<string, { pct: number; dF: number; dD: number }>>({});
  // Read-only priority weights (the knob itself lives on the Controls tab): shown
  // as badges here so promote/demote decisions keep their fallback-rate context.
  const [priorityWeights, setPriorityWeights] = useState<Record<string, number>>({});

  useEffect(() => {
    let alive = true;
    const fetchWeights = () => {
      fetch('/api/controls/priority')
        .then(r => (r.ok ? r.json() : null))
        .then(d => { if (d && alive) setPriorityWeights(d.weights ?? {}); })
        .catch(() => {});
    };
    fetchWeights();
    const iv = setInterval(fetchWeights, 15000);
    return () => { alive = false; clearInterval(iv); };
  }, []);

  useEffect(() => {
    let alive = true;
    const fetchStatus = async () => {
      try {
        const res = await fetch('/api/status');
        if (!res.ok) throw new Error(String(res.status));
        const data: StatusResponse = await res.json();
        if (!alive) return;
        const g = data.components?.find(c => c.component === 'gordon') ?? null;
        setGordon(g);
        setReachable(!!g?.reachable);

        // Sample the fallback ratio into the rolling timeline. Only append when
        // the decision count actually moved, so an idle fleet doesn't paint a
        // flat line of duplicate snapshots.
        const gStats = g?.stats ?? {};
        const gProposals = num(gStats['llm_proposals']);
        const gFallbacks = num(gStats['llm_fallbacks']);
        const gDecided = gProposals + gFallbacks;
        if (gDecided > 0 && gDecided !== lastDecidedRef.current) {
          lastDecidedRef.current = gDecided;
          const pct = (gFallbacks / gDecided) * 100;
          // Instantaneous rate: fallback share of only the decisions added since the
          // previous sample. Null on the first sample (no prior counts to diff against).
          const prev = prevCountsRef.current;
          const dDecided = prev ? gDecided - (prev.proposals + prev.fallbacks) : 0;
          const instPct = prev && dDecided > 0
            ? ((gFallbacks - prev.fallbacks) / dDecided) * 100
            : null;
          prevCountsRef.current = { proposals: gProposals, fallbacks: gFallbacks };
          const now = new Date();
          setHistory(prev2 => [
            ...prev2.slice(-(MAX_SAMPLES - 1)),
            { t: now.getTime(), label: now.toLocaleTimeString(), pct, instPct },
          ]);
        }

        // Per-persona fallback rate over a rolling window: snapshot each persona's
        // cumulative counts, then grade on the delta vs. the oldest snapshot still
        // inside the window (rather than all-time, which barely moves).
        const pArr = Array.isArray(gStats['personas']) ? (gStats['personas'] as PersonaHealth[]) : [];
        if (pArr.length > 0) {
          const snap: Record<string, { p: number; f: number }> = {};
          for (const p of pArr) snap[p.persona] = { p: num(p.llm_proposals), f: num(p.llm_fallbacks) };
          const nowMs = Date.now();
          const hist = personaHistRef.current;
          hist.push({ t: nowMs, counts: snap });
          // Keep the front pinned to the newest snapshot at/just before the window edge.
          while (hist.length > 2 && hist[1].t < nowMs - PERSONA_WINDOW_MS) hist.shift();
          const baseline = hist[0].counts;
          const rates: Record<string, { pct: number; dF: number; dD: number }> = {};
          for (const persona of Object.keys(snap)) {
            const b = baseline[persona] ?? { p: 0, f: 0 };
            const dF = snap[persona].f - b.f;
            const dP = snap[persona].p - b.p;
            const dD = dF + dP;
            // Guard against counter resets (backend restart) producing negative deltas.
            rates[persona] = dD > 0 && dF >= 0
              ? { pct: (dF / dD) * 100, dF, dD }
              : { pct: 0, dF: 0, dD: 0 };
          }
          setPersonaRates(rates);
        }
      } catch {
        if (alive) setReachable(false);
      }
    };
    fetchStatus();
    const interval = setInterval(fetchStatus, 3000);
    return () => { alive = false; clearInterval(interval); };
  }, []);

  const stats = gordon?.stats ?? {};
  const llmProposals = num(stats['llm_proposals']);
  const llmFallbacks = num(stats['llm_fallbacks']);
  const decided = llmProposals + llmFallbacks;
  // Fallback ratio = fallbacks / all decisions. Correctly a ratio-of-sums (the
  // backend sums raw counts across personas; we divide here).
  const fallbackPct = decided > 0 ? (llmFallbacks / decided) * 100 : 0;
  const llmPct = decided > 0 ? (llmProposals / decided) * 100 : 0;

  const slotWaits = num(stats['llm_slot_waits']);
  const slotTimeouts = num(stats['llm_slot_timeouts']);
  const slotFailOpen = num(stats['llm_slot_fail_open']);
  const slotAvgWaitMs = num(stats['llm_slot_avg_wait_ms']);
  // Numeric pre-gate: events vetoed before the LLM call (drawdown halt / family
  // loss-limit / cooldown / near-expiry), i.e. Ollama slots the fleet never spent.
  const pregateSkips = num(stats['pregate_skips']);

  const personas = Array.isArray(stats['personas']) ? (stats['personas'] as PersonaHealth[]) : [];

  // Auto-zoom the chart's Y range to the observed band (with padding) instead of a
  // fixed 0–100. The fallback ratio is a cumulative figure that barely moves, so a
  // full-scale axis would flatten every wiggle against the floor.
  const yDomain = ((): [number, number] => {
    if (history.length === 0) return [0, 100];
    const pcts = history.map(h => h.pct);
    const lo = Math.min(...pcts);
    const hi = Math.max(...pcts);
    const pad = Math.max((hi - lo) * 0.25, 1);
    let dMin = Math.max(0, Math.floor(lo - pad));
    let dMax = Math.min(100, Math.ceil(hi + pad));
    if (dMax - dMin < 4) dMax = Math.min(100, dMin + 4); // keep a minimum visible span
    return [dMin, dMax];
  })();
  // Only draw the 20% target line when it actually falls inside the zoomed view.
  const showTarget = 20 >= yDomain[0] && 20 <= yDomain[1];

  // Instantaneous series: per-interval rate only (drop the first, null-seeded sample).
  const instHistory = history.filter(h => h.instPct !== null);

  return (
    <div className="max-w-[1400px] mx-auto mt-8 px-6 pb-16 flex flex-col gap-6">
      <div className="flex items-center gap-3">
        <Cpu className="w-5 h-5 text-[var(--primary)]" />
        <h2 className="text-lg font-bold text-[var(--text-main)]">Model Reliability</h2>
        <span className={`ml-auto text-[10px] font-mono px-2 py-1 rounded-full border ${
          reachable === null ? 'text-[var(--text-muted)] border-white/10'
            : reachable ? 'text-[var(--accent-green)] border-[var(--accent-green)]/40'
            : 'text-[var(--accent-red)] border-[var(--accent-red)]/40'
        }`}>
          {reachable === null ? 'LOADING' : reachable ? 'GORDON LIVE' : 'GORDON UNREACHABLE'}
        </span>
      </div>

      {/* Headline: LLM fallback ratio */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-6 shadow-lg backdrop-blur-md">
        <div className="flex flex-col md:flex-row md:items-center gap-6">
          <div className="flex flex-col items-center justify-center md:w-64 shrink-0">
            <span className="text-xs uppercase tracking-wider text-[var(--text-muted)] font-bold flex items-center gap-1.5">
              <AlertTriangle className="w-3.5 h-3.5" /> LLM Fallback Ratio
            </span>
            <span className="text-6xl font-black font-mono mt-2" style={{ color: ratioColor(fallbackPct) }}>
              {decided > 0 ? `${fallbackPct.toFixed(1)}%` : '—'}
            </span>
            <span className="text-[11px] text-[var(--text-muted)] font-mono mt-1">
              {decided > 0
                ? `${llmFallbacks.toLocaleString()} of ${decided.toLocaleString()} decisions used rules`
                : 'awaiting decisions'}
            </span>
          </div>

          <div className="flex-1 flex flex-col gap-3">
            {/* LLM vs rules split bar. The LLM slice gets a fixed distinct hue so the
                split is always readable — the fallback slice keeps its severity tint,
                which is green when healthy and would otherwise vanish against a green LLM slice. */}
            <div className="w-full h-4 rounded-full overflow-hidden bg-white/5 flex">
              <div className="h-full transition-all" style={{ width: `${llmPct}%`, backgroundColor: 'var(--primary)' }} />
              <div className="h-full transition-all" style={{ width: `${fallbackPct}%`, backgroundColor: ratioColor(fallbackPct) }} />
            </div>
            <div className="flex justify-between text-[11px] font-mono text-[var(--text-muted)]">
              <span className="flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full" style={{ backgroundColor: 'var(--primary)' }} />
                LLM decided {llmProposals.toLocaleString()} ({llmPct.toFixed(1)}%)
              </span>
              <span className="flex items-center gap-1.5">
                Rules fallback {llmFallbacks.toLocaleString()} ({fallbackPct.toFixed(1)}%)
                <span className="w-2 h-2 rounded-full" style={{ backgroundColor: ratioColor(fallbackPct) }} />
              </span>
            </div>
            <p className="text-[11px] text-[var(--text-muted)] leading-relaxed border-t border-white/5 pt-3">
              A high ratio means the Ollama pipeline is timing out or erroring and the fleet is
              trading on the keyword rules parser instead of the model. Lower is better — target
              well under 20%. Watch the concurrency-limiter stats below if this climbs.
            </p>
            {pregateSkips > 0 && (
              <p className="text-[11px] font-mono text-[var(--text-muted)]">
                <span className="text-[var(--accent-green)] font-bold">{pregateSkips.toLocaleString()}</span> proposals vetoed pre-LLM (drawdown halt / loss-limit / cooldown / near-expiry) — Ollama slots saved, so the ratio above reflects only calls actually worth making.
              </p>
            )}
          </div>
        </div>
      </section>

      {/* Fallback ratio over time */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3">
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
          <Activity className="w-4 h-4 text-[var(--primary)]" />
          Fallback Ratio Over Time
        </h3>
        {history.length < 2 ? (
          <div className="h-[200px] flex items-center justify-center text-[11px] text-[var(--text-muted)] font-mono">
            {reachable === false ? 'gordon unreachable' : 'collecting samples…'}
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={200}>
            <AreaChart data={history} margin={{ top: 4, right: 16, bottom: 0, left: 0 }}>
              <defs>
                <linearGradient id="fbGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor="var(--accent-red)" stopOpacity={0.28} />
                  <stop offset="95%" stopColor="var(--accent-red)" stopOpacity={0.02} />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" stroke="rgba(255,255,255,0.04)" vertical={false} />
              <XAxis
                dataKey="label"
                tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }}
                tickLine={false} axisLine={false} minTickGap={40}
              />
              <YAxis
                domain={yDomain}
                width={38}
                allowDecimals={false}
                tickFormatter={(v: number) => `${v}%`}
                tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }}
                tickLine={false} axisLine={false}
              />
              {showTarget && (
                <ReferenceLine y={20} stroke="var(--accent-amber, #f59e0b)" strokeDasharray="4 3" strokeOpacity={0.5} />
              )}
              <ReTooltip content={<RatioTooltip />} cursor={{ stroke: 'rgba(255,255,255,0.07)', strokeWidth: 1 }} />
              <Area
                type="monotone" dataKey="pct" stroke="var(--accent-red)" strokeWidth={2}
                fill="url(#fbGrad)" dot={false} activeDot={{ r: 4, strokeWidth: 0 }} isAnimationActive={false}
              />
            </AreaChart>
          </ResponsiveContainer>
        )}
        <p className="text-[11px] text-[var(--text-muted)] leading-relaxed">
          Live sample of the rules-fallback share each poll (~3s), auto-zoomed to the observed
          band. The dashed line (when in view) marks the 20% target ceiling — sustained time above
          it means the Ollama pipeline is degrading.
        </p>
      </section>

      {/* Instantaneous (per-poll) fallback rate — livelier than the cumulative curve */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3">
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
          <Zap className="w-4 h-4 text-[var(--primary)]" />
          Fallback Rate (Per Interval)
        </h3>
        {instHistory.length < 2 ? (
          <div className="h-[200px] flex items-center justify-center text-[11px] text-[var(--text-muted)] font-mono">
            {reachable === false ? 'gordon unreachable' : 'collecting samples…'}
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={200}>
            <AreaChart data={instHistory} margin={{ top: 4, right: 16, bottom: 0, left: 0 }}>
              <defs>
                <linearGradient id="fbInstGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor="var(--primary)" stopOpacity={0.3} />
                  <stop offset="95%" stopColor="var(--primary)" stopOpacity={0.02} />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" stroke="rgba(255,255,255,0.04)" vertical={false} />
              <XAxis
                dataKey="label"
                tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }}
                tickLine={false} axisLine={false} minTickGap={40}
              />
              <YAxis
                domain={[0, 100]}
                width={38}
                allowDecimals={false}
                tickFormatter={(v: number) => `${v}%`}
                tick={{ fill: 'var(--text-muted)', fontSize: 9, fontFamily: 'var(--font-mono)' }}
                tickLine={false} axisLine={false}
              />
              <ReferenceLine y={20} stroke="var(--accent-amber, #f59e0b)" strokeDasharray="4 3" strokeOpacity={0.5} />
              <ReTooltip content={<RatioTooltip dataKey="instPct" />} cursor={{ stroke: 'rgba(255,255,255,0.07)', strokeWidth: 1 }} />
              <Area
                type="monotone" dataKey="instPct" stroke="var(--primary)" strokeWidth={2}
                fill="url(#fbInstGrad)" dot={false} activeDot={{ r: 4, strokeWidth: 0 }} isAnimationActive={false}
              />
            </AreaChart>
          </ResponsiveContainer>
        )}
        <p className="text-[11px] text-[var(--text-muted)] leading-relaxed">
          Fallback share of only the decisions made since the previous poll (~3s). Full 0–100%
          scale — this reacts immediately to a stall or recovery instead of being smoothed by the
          running total above.
        </p>
      </section>

      {/* Concurrency limiter stats — the "why" behind fallbacks */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
          <Timer className="w-4 h-4 text-[var(--primary)]" />
          LLM Concurrency Limiter
        </h3>
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 font-mono">
          <Stat label="Slot Waits" value={slotWaits.toLocaleString()} />
          <Stat label="Avg Wait" value={`${Math.round(slotAvgWaitMs)} ms`} />
          <Stat label="Give-ups (timeouts)" value={slotTimeouts.toLocaleString()}
            danger={slotTimeouts > 0} />
          <Stat label="Fail-open (Redis down)" value={slotFailOpen.toLocaleString()}
            danger={slotFailOpen > 0} />
        </div>
        <p className="text-[11px] text-[var(--text-muted)] leading-relaxed">
          Generate calls wait here for a fleet-wide slot instead of piling into Ollama and timing
          out. Sustained give-ups mean the fleet wants more throughput than Ollama can serve — raise
          <code className="mx-1 text-[var(--text-main)]">OLLAMA_MAX_CONCURRENT</code> (and Ollama's
          <code className="mx-1 text-[var(--text-main)]">OLLAMA_NUM_PARALLEL</code>) or lower the model size.
        </p>
      </section>

      {/* Per-persona breakdown */}
      {personas.length > 0 && (
        <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3">
          <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
            <Layers className="w-4 h-4 text-[var(--primary)]" />
            Per-Persona
            <span className="ml-auto normal-case tracking-normal font-normal text-[10px] text-[var(--text-muted)]">
              last {Math.round(PERSONA_WINDOW_MS / 60000)}m
            </span>
          </h3>
          <div className="flex flex-col gap-2 font-mono text-xs">
            {personas.map((p, i) => {
              const r = personaRates[p.persona] ?? { pct: 0, dF: 0, dD: 0 };
              const pPct = r.pct;
              const badge = priorityBadge(priorityWeights[p.persona]);
              return (
                <div key={i} className="flex items-center gap-3 py-1">
                  <span className={`w-2 h-2 rounded-full shrink-0 ${p.reachable ? 'bg-[var(--accent-green)]' : 'bg-[var(--accent-red)]'}`} />
                  <span className="w-28 text-[var(--text-main)] truncate">{PERSONA_LABEL[p.persona] ?? p.persona}</span>
                  <span className="w-20 shrink-0">
                    {badge && (
                      <span className={`px-1.5 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider ${badge.cls}`} title="Set on the Controls tab (Gordon Priority)">
                        {badge.label}
                      </span>
                    )}
                  </span>
                  <div className="flex-1 h-2 rounded-full overflow-hidden bg-white/5">
                    <div className="h-full" style={{ width: `${pPct}%`, backgroundColor: ratioColor(pPct) }} />
                  </div>
                  <span className="w-16 text-right" style={{ color: ratioColor(pPct) }}>
                    {r.dD > 0 ? `${pPct.toFixed(0)}%` : '—'}
                  </span>
                  <span className="w-24 text-right text-[var(--text-muted)]">
                    {r.dF.toLocaleString()}/{r.dD.toLocaleString()}
                  </span>
                </div>
              );
            })}
          </div>
        </section>
      )}

      {/* Live RAG/inference telemetry (SSE) */}
      <SystemTelemetry theme={theme} />
    </div>
  );
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const RatioTooltip = ({ active, payload, dataKey = 'pct' }: any) => {
  if (!active || !payload?.length) return null;
  const p = payload[0].payload as RatioSample;
  const val = dataKey === 'instPct' ? p.instPct : p.pct;
  if (val === null || val === undefined) return null;
  return (
    <div className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-lg p-2.5 shadow-lg font-mono">
      <div className="text-[9px] text-[var(--text-muted)]">{p.label}</div>
      <div className="text-sm font-bold" style={{ color: ratioColor(val) }}>{val.toFixed(1)}%</div>
    </div>
  );
};

const Stat: React.FC<{ label: string; value: string; danger?: boolean }> = ({ label, value, danger }) => (
  <div className="bg-white/[0.03] border border-white/5 rounded-xl px-3 py-2.5 flex flex-col gap-1">
    <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)]">{label}</span>
    <span className={`text-lg font-bold ${danger ? 'text-[var(--accent-red)]' : 'text-[var(--text-main)]'}`}>{value}</span>
  </div>
);

export default SystemMetrics;
