import React, { useCallback, useEffect, useState } from 'react';
import { Users, Pencil, Save, RotateCcw, X, ArrowUp, ArrowDown, Minus, Pause } from 'lucide-react';
import GlossaryTooltip from './GlossaryTooltip';

// Promote/demote presets — a gordon's weight scales its share of the fleet's generate
// budget (gekko:gordon_priority): 1 = normal, <1 = slower, >1 = faster, 0 = paused
// (falls back to keyword rules). Mirrors the old Controls-tab Gordon Priority pane.
const LEVELS: { w: number; label: string; icon: React.ReactNode }[] = [
  { w: 0, label: 'Pause', icon: <Pause className="w-3 h-3" /> },
  { w: 0.5, label: 'Demote', icon: <ArrowDown className="w-3 h-3" /> },
  { w: 1, label: 'Normal', icon: <Minus className="w-3 h-3" /> },
  { w: 2, label: 'Promote', icon: <ArrowUp className="w-3 h-3" /> },
];

// Nearest preset for an arbitrary stored weight (so custom values still highlight one).
const levelFor = (w: number) =>
  LEVELS.reduce((best, l) => (Math.abs(l.w - w) < Math.abs(best.w - w) ? l : best), LEVELS[0]).w;

// ─── Data contracts ──────────────────────────────────────────────────────────

// /api/gordons — the roster + generated name + whether the operator has overridden it.
interface GordonMeta {
  id: string;
  name: string;
  overridden: boolean;
}

// /api/status → gordon component → stats.personas[] — the EFFECTIVE (live) identity +
// editable knobs the running gordon publishes. We seed the editor from these.
interface PersonaHealth {
  persona: string;
  reachable: boolean;
  provider?: string;
  model?: string;
  stance?: string;
  risk_tier?: string;
  match_threshold?: number;
  nudge?: string;
  recent_activity?: string[];
}

// /api/personas — the per-persona realized risk metrics (same feed as the Home scorecard).
interface PersonaStat {
  persona: string;
  trades: number;
  win_rate: number;
  realized_pnl: number;
  return_on_capital_pct: number;
  expectancy_usd?: number;
  sharpe?: number;
  sortino?: number;
  max_drawdown_pct?: number;
}

interface JoinedGordon {
  meta: GordonMeta;
  health?: PersonaHealth;
  stat?: PersonaStat;
}

const STANCES = ['bullish', 'neutral', 'bearish'];
const RISK_TIERS = ['conservative', 'balanced', 'aggressive'];

const fmtUSD = (n: number) => `${n >= 0 ? '+' : '−'}$${Math.abs(Math.round(n)).toLocaleString('en-US')}`;
const tone = (n: number) => (n >= 0 ? 'text-[var(--accent-green)]' : 'text-[var(--accent-red)]');
const fmtRatio = (n?: number) => (n === undefined || n === null ? '—' : n.toFixed(2));

// Editable draft — the fields a POST /api/gordons accepts.
interface Draft {
  display_name: string;
  nudge: string;
  stance: string;
  risk_tier: string;
  match_threshold: number;
  model: string;
  provider: string;
}

function seedDraft(g: JoinedGordon): Draft {
  const h: Partial<PersonaHealth> = g.health ?? {};
  // Fall back to sane defaults when the running gordon hasn't published a field yet
  // (e.g. an older binary predating these health keys), so the form never seeds a
  // blank/out-of-range knob.
  const thr = h.match_threshold;
  return {
    display_name: g.meta.overridden ? g.meta.name : '',
    nudge: h.nudge ?? '',
    stance: STANCES.includes(h.stance ?? '') ? h.stance! : 'neutral',
    risk_tier: RISK_TIERS.includes(h.risk_tier ?? '') ? h.risk_tier! : 'balanced',
    match_threshold: thr !== undefined && thr >= 0.35 && thr <= 0.8 ? thr : 0.5,
    model: h.model ?? '',
    provider: h.provider === 'gemini' ? 'gemini' : 'ollama',
  };
}

// ─── Activity feed ───────────────────────────────────────────────────────────

// Colour a log line by its bracketed tag so the feed is scannable at a glance:
// emitted proposals green, hard drops red, soft vetoes/skips amber, info muted.
const TAG_TONE: Record<string, string> = {
  PROPOSAL: 'text-[var(--accent-green)]',
  HOLDINGS: 'text-[var(--accent-green)]',
  ERROR: 'text-[var(--accent-red)]',
  'KAFKA ERR': 'text-[var(--accent-red)]',
  DECLINE: 'text-[var(--accent-red)]',
  PAUSED: 'text-[var(--accent-red)]',
  PREGATE: 'text-[var(--accent-amber,#d9a441)]',
  EDGE: 'text-[var(--accent-amber,#d9a441)]',
  SKEW: 'text-[var(--accent-amber,#d9a441)]',
  EXPIRY: 'text-[var(--accent-amber,#d9a441)]',
  FLOOR: 'text-[var(--accent-amber,#d9a441)]',
  STANCE: 'text-[var(--accent-amber,#d9a441)]',
  BLACKOUT: 'text-[var(--accent-amber,#d9a441)]',
  'RAG WARN': 'text-[var(--accent-amber,#d9a441)]',
};

// Split "15:04:05 [TAG] message" into its parts for styling. Lines without the
// expected shape fall through as a plain message.
function parseActivity(line: string): { time: string; tag: string; msg: string; tone: string } {
  const m = line.match(/^(\d{2}:\d{2}:\d{2})\s+\[([^\]]+)\]\s*(.*)$/);
  if (!m) return { time: '', tag: '', msg: line, tone: 'text-[var(--text-muted)]' };
  const tag = m[2];
  return { time: m[1], tag, msg: m[3], tone: TAG_TONE[tag] ?? 'text-[var(--text-muted)]' };
}

const ActivityFeed: React.FC<{ lines?: string[] }> = ({ lines }) => {
  if (!lines || lines.length === 0) {
    return (
      <div className="border-t border-white/5 pt-3">
        <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Recent activity</span>
        <p className="text-[11px] text-[var(--text-muted)] font-mono mt-1.5 italic">No activity yet — waiting on events.</p>
      </div>
    );
  }
  return (
    <div className="border-t border-white/5 pt-3">
      <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Recent activity</span>
      <ul className="mt-1.5 flex flex-col gap-0.5 max-h-40 overflow-y-auto font-mono text-[11px] leading-relaxed">
        {lines.map((line, i) => {
          const a = parseActivity(line);
          return (
            <li key={i} className="flex gap-2 whitespace-nowrap">
              {a.time && <span className="text-[var(--text-muted)] shrink-0 tabular-nums">{a.time}</span>}
              {a.tag && <span className={`${a.tone} font-bold shrink-0`}>{a.tag}</span>}
              <span className="text-[var(--text-main)] truncate" title={a.msg}>{a.msg}</span>
            </li>
          );
        })}
      </ul>
    </div>
  );
};

// ─── Metric tile ─────────────────────────────────────────────────────────────

const Tile: React.FC<{ label: string; value: string; cls?: string; termKey?: string }> = ({ label, value, cls, termKey }) => (
  <div className="flex flex-col gap-1">
    {termKey ? (
      <GlossaryTooltip termKey={termKey} className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">
        {label}
      </GlossaryTooltip>
    ) : (
      <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">{label}</span>
    )}
    <span className={`text-base font-mono font-bold ${cls ?? 'text-[var(--text-main)]'}`}>{value}</span>
  </div>
);

// ─── One gordon card ─────────────────────────────────────────────────────────

const GordonCard: React.FC<{
  g: JoinedGordon;
  weight: number;
  onSetWeight: (w: number) => void;
  onSaved: () => void;
}> = ({ g, weight, onSetWeight, onSaved }) => {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<Draft>(() => seedDraft(g));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const startEdit = () => {
    setDraft(seedDraft(g));
    setErr(null);
    setEditing(true);
  };

  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      const res = await fetch('/api/gordons', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ persona_id: g.meta.id, ...draft }),
      });
      if (!res.ok) throw new Error(await res.text());
      setEditing(false);
      onSaved();
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'save failed');
    } finally {
      setBusy(false);
    }
  };

  const resetToDefault = async () => {
    if (!window.confirm(`Reset ${g.meta.name} to its built-in defaults? This deletes all your saved edits for this gordon.`)) return;
    setBusy(true);
    setErr(null);
    try {
      const res = await fetch('/api/gordons/reset', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ persona_id: g.meta.id }),
      });
      if (!res.ok) throw new Error(await res.text());
      setEditing(false);
      onSaved();
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'reset failed');
    } finally {
      setBusy(false);
    }
  };

  const h = g.health;
  const s = g.stat;
  const reachable = h?.reachable ?? false;

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
      {/* Header */}
      <div className="flex items-start gap-3 flex-wrap">
        <span className={`mt-1.5 w-2 h-2 rounded-full shrink-0 ${reachable ? 'bg-[var(--accent-green)]' : 'bg-[var(--accent-red)]'}`} />
        <div className="flex flex-col">
          <span className="text-base font-bold text-[var(--text-main)] flex items-center gap-2">
            {g.meta.name}
            {g.meta.overridden && (
              <span className="text-[9px] px-1.5 py-0.5 rounded border border-[var(--primary)]/30 bg-[var(--primary)]/10 text-[var(--primary)] uppercase tracking-wider font-semibold" title="Has saved operator edits">
                edited
              </span>
            )}
          </span>
          <span className="text-[10px] font-mono text-[var(--text-muted)] mt-0.5">
            {g.meta.id}
            {h?.model && <> · <span className="text-[var(--text-main)]">{h.model}</span></>}
            {h?.provider && <> · {h.provider}</>}
            {h?.stance && <> · {h.stance}</>}
          </span>
        </div>
        {!editing && (
          <button
            type="button"
            onClick={startEdit}
            className="ml-auto flex items-center gap-1.5 text-[11px] font-semibold px-2.5 py-1.5 rounded-lg border border-white/10 bg-white/[0.03] text-[var(--text-muted)] hover:border-[var(--primary)]/40 hover:text-[var(--primary)] transition-colors cursor-pointer"
          >
            <Pencil className="w-3 h-3" /> Edit
          </button>
        )}
      </div>

      {/* Metric tiles */}
      <div className="grid grid-cols-3 sm:grid-cols-6 gap-4">
        <Tile label="Realized P&L" value={s ? fmtUSD(s.realized_pnl) : '—'} cls={s ? tone(s.realized_pnl) : undefined} termKey="PnL" />
        <Tile label="Return on cap." value={s ? `${s.return_on_capital_pct >= 0 ? '+' : '−'}${Math.abs(s.return_on_capital_pct).toFixed(1)}%` : '—'} cls={s ? tone(s.return_on_capital_pct) : undefined} termKey="Return on Capital" />
        <Tile label="Sharpe" value={fmtRatio(s?.sharpe)} termKey="Sharpe Ratio" />
        <Tile label="Sortino" value={fmtRatio(s?.sortino)} termKey="Sortino Ratio" />
        <Tile label="Win rate" value={s ? `${s.win_rate.toFixed(0)}%` : '—'} termKey="Win-Rate Variance" />
        <Tile label="Trades" value={s ? String(s.trades) : '—'} termKey="Total Trades" />
      </div>

      {/* Priority: promote / demote / pause this gordon's share of the LLM budget. */}
      <div className="flex items-center gap-3 border-t border-white/5 pt-3">
        <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold shrink-0">Priority</span>
        <div className="flex gap-1.5 flex-1">
          {LEVELS.map(l => {
            const sel = levelFor(weight) === l.w;
            return (
              <button
                key={l.w}
                type="button"
                onClick={() => onSetWeight(l.w)}
                title={l.label}
                className={`flex-1 flex items-center justify-center gap-1 text-[11px] font-bold py-1.5 rounded-md border transition-all cursor-pointer ${
                  sel
                    ? 'border-[var(--primary)] bg-[var(--primary)]/15 text-[var(--primary)]'
                    : 'border-white/10 text-[var(--text-muted)] hover:border-white/20'
                }`}
              >
                {l.icon}
                <span className="hidden sm:inline">{l.label}</span>
              </button>
            );
          })}
        </div>
      </div>

      {/* Live activity feed — what this gordon just did (proposals, declines, vetoes, skips). */}
      <ActivityFeed lines={h?.recent_activity} />

      {/* Editor */}
      {editing && (
        <div className="flex flex-col gap-3 border-t border-white/5 pt-4">
          <label className="flex flex-col gap-1">
            <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Personality prompt</span>
            <textarea
              value={draft.nudge}
              onChange={e => setDraft({ ...draft, nudge: e.target.value })}
              rows={3}
              maxLength={2000}
              className="w-full text-xs font-mono bg-white/[0.03] border border-white/10 rounded-lg p-2.5 text-[var(--text-main)] focus:border-[var(--primary)]/50 outline-none resize-y"
              placeholder="One-line personality instruction injected into the proposal prompt…"
            />
          </label>

          <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
            <label className="flex flex-col gap-1">
              <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Stance</span>
              <select
                value={draft.stance}
                onChange={e => setDraft({ ...draft, stance: e.target.value })}
                className="text-xs font-mono bg-white/[0.03] border border-white/10 rounded-lg px-2 py-1.5 text-[var(--text-main)] outline-none cursor-pointer"
              >
                {STANCES.map(v => <option key={v} value={v}>{v}</option>)}
              </select>
            </label>
            <label className="flex flex-col gap-1">
              <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Risk tier</span>
              <select
                value={draft.risk_tier}
                onChange={e => setDraft({ ...draft, risk_tier: e.target.value })}
                className="text-xs font-mono bg-white/[0.03] border border-white/10 rounded-lg px-2 py-1.5 text-[var(--text-main)] outline-none cursor-pointer"
              >
                {RISK_TIERS.map(v => <option key={v} value={v}>{v}</option>)}
              </select>
            </label>
            <label className="flex flex-col gap-1">
              <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">
                Match threshold <span className="text-[var(--text-main)]">{draft.match_threshold.toFixed(2)}</span>
              </span>
              <input
                type="range" min={0.35} max={0.80} step={0.01}
                value={draft.match_threshold}
                onChange={e => setDraft({ ...draft, match_threshold: parseFloat(e.target.value) })}
                className="w-full accent-[var(--primary)] cursor-pointer mt-2"
              />
            </label>
            <label className="flex flex-col gap-1">
              <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Display name</span>
              <input
                type="text"
                value={draft.display_name}
                onChange={e => setDraft({ ...draft, display_name: e.target.value })}
                maxLength={64}
                placeholder="(generated)"
                className="text-xs font-mono bg-white/[0.03] border border-white/10 rounded-lg px-2 py-1.5 text-[var(--text-main)] outline-none"
              />
            </label>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <label className="flex flex-col gap-1">
              <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Model</span>
              <input
                type="text"
                value={draft.model}
                onChange={e => setDraft({ ...draft, model: e.target.value })}
                maxLength={64}
                placeholder="e.g. qwen2.5:7b"
                className="text-xs font-mono bg-white/[0.03] border border-white/10 rounded-lg px-2 py-1.5 text-[var(--text-main)] outline-none"
              />
            </label>
            <label className="flex flex-col gap-1">
              <span className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] font-semibold">Backend</span>
              <select
                value={draft.provider}
                onChange={e => setDraft({ ...draft, provider: e.target.value })}
                className="text-xs font-mono bg-white/[0.03] border border-white/10 rounded-lg px-2 py-1.5 text-[var(--text-main)] outline-none cursor-pointer"
              >
                <option value="ollama">ollama</option>
                <option value="gemini">gemini</option>
              </select>
            </label>
          </div>
          <p className="text-[10px] text-[var(--text-muted)] font-mono">
            Prompt, stance, risk & threshold apply live. Model / backend changes take effect on the next <code className="text-[var(--text-main)]">./run.sh</code> restart.
          </p>

          {err && <p className="text-[11px] text-[var(--accent-red)] font-mono">⚠ {err}</p>}

          <div className="flex items-center gap-2 flex-wrap">
            <button
              type="button" onClick={save} disabled={busy}
              className="flex items-center gap-1.5 text-[11px] font-semibold px-3 py-1.5 rounded-lg border border-[var(--primary)]/40 bg-[var(--primary)]/15 text-[var(--primary)] hover:bg-[var(--primary)]/25 transition-colors cursor-pointer disabled:opacity-50"
            >
              <Save className="w-3 h-3" /> Save
            </button>
            <button
              type="button" onClick={() => setEditing(false)} disabled={busy}
              className="flex items-center gap-1.5 text-[11px] font-semibold px-3 py-1.5 rounded-lg border border-white/10 bg-white/[0.03] text-[var(--text-muted)] hover:text-[var(--text-main)] transition-colors cursor-pointer disabled:opacity-50"
            >
              <X className="w-3 h-3" /> Cancel
            </button>
            <button
              type="button" onClick={resetToDefault} disabled={busy}
              className="ml-auto flex items-center gap-1.5 text-[11px] font-semibold px-3 py-1.5 rounded-lg border border-[var(--accent-red)]/30 bg-[var(--accent-red)]/10 text-[var(--accent-red)] hover:bg-[var(--accent-red)]/20 transition-colors cursor-pointer disabled:opacity-50"
              title="Delete saved edits and revert to built-in defaults"
            >
              <RotateCcw className="w-3 h-3" /> Reset to default
            </button>
          </div>
        </div>
      )}
    </section>
  );
};

// ─── Scorecard container ─────────────────────────────────────────────────────

const GordonScorecard: React.FC = () => {
  const [gordons, setGordons] = useState<GordonMeta[]>([]);
  const [health, setHealth] = useState<Record<string, PersonaHealth>>({});
  const [stats, setStats] = useState<Record<string, PersonaStat>>({});
  const [priority, setPriority] = useState<{ fleet_rate: number; weights: Record<string, number> }>({ fleet_rate: 0, weights: {} });
  const [loaded, setLoaded] = useState(false);

  const fetchAll = useCallback(async () => {
    try {
      const [gRes, sRes, pRes, prRes] = await Promise.all([
        fetch('/api/gordons'),
        fetch('/api/status'),
        fetch('/api/personas'),
        fetch('/api/controls/priority'),
      ]);
      if (gRes.ok) {
        const data = await gRes.json();
        if (Array.isArray(data?.gordons)) setGordons(data.gordons);
      }
      if (sRes.ok) {
        const data = await sRes.json();
        const gordon = data?.components?.find((c: { component: string }) => c.component === 'gordon');
        const personas: PersonaHealth[] = Array.isArray(gordon?.stats?.personas) ? gordon.stats.personas : [];
        setHealth(Object.fromEntries(personas.map(p => [p.persona, p])));
      }
      if (pRes.ok) {
        const data = await pRes.json();
        if (Array.isArray(data)) setStats(Object.fromEntries((data as PersonaStat[]).map(p => [p.persona, p])));
      }
      if (prRes.ok) {
        const data = await prRes.json();
        setPriority({ fleet_rate: data?.fleet_rate ?? 0, weights: data?.weights ?? {} });
      }
    } catch {
      /* keep last-known */
    } finally {
      setLoaded(true);
    }
  }, []);

  useEffect(() => {
    fetchAll();
    const id = setInterval(fetchAll, 8000);
    return () => clearInterval(id);
  }, [fetchAll]);

  // Set one gordon's priority weight, preserving the rest of the fleet's weights.
  const setWeight = useCallback((id: string, w: number) => {
    setPriority(prev => {
      const next = { fleet_rate: prev.fleet_rate, weights: { ...prev.weights, [id]: w } };
      fetch('/api/controls/priority', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(next),
      }).catch(() => {});
      return next;
    });
  }, []);

  if (!loaded) return null;

  if (gordons.length === 0) {
    return (
      <p className="text-[11px] text-[var(--text-muted)] font-mono border border-dashed border-white/10 rounded-xl px-4 py-4">
        No gordons detected. They advertise themselves once running — check the fleet is up (needs the latest gordon build).
      </p>
    );
  }

  const joined: JoinedGordon[] = gordons.map(meta => ({
    meta,
    health: health[meta.id],
    stat: stats[meta.id],
  }));

  return (
    <section className="flex flex-col gap-4">
      <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
        <Users className="w-4 h-4 text-[var(--primary)]" />
        Fleet Scorecard
        <span className="ml-auto normal-case tracking-normal font-normal text-[10px] text-[var(--text-muted)]">
          {gordons.length} gordon{gordons.length === 1 ? '' : 's'} · edits persist across resets
        </span>
      </h3>
      {joined.map(g => (
        <GordonCard
          key={g.meta.id}
          g={g}
          weight={priority.weights[g.meta.id] ?? 1}
          onSetWeight={w => setWeight(g.meta.id, w)}
          onSaved={fetchAll}
        />
      ))}
    </section>
  );
};

export default GordonScorecard;
