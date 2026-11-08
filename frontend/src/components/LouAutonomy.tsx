import { useCallback, useEffect, useState } from 'react';
import { ShieldCheck } from 'lucide-react';

// How much rope Lou gets (2026-08-25 desk review, fix #6). Lou had three bounded price
// knobs against nineteen validated lessons — it could watch a persona lose money for two
// months with no way to act. Rather than one more boolean, the escalation is explicit.
//
// The level is written to runs/lou/autonomy and read at Lou's NEXT nightly pass, so this
// control is a policy decision rather than a live switch — the panel says so, because a
// knob that looks instant but isn't is worse than no knob.

type Level = 'observe' | 'tune' | 'intervene';

const LEVELS: { id: Level; label: string; blurb: string }[] = [
  {
    id: 'observe',
    label: 'Observe',
    blurb: 'Analyses runs and writes lessons. Changes nothing.',
  },
  {
    id: 'tune',
    label: 'Tune',
    blurb: 'May also adjust bounded price and stop knobs — clamped, one step per day, safer-direction only.',
  },
  {
    id: 'intervene',
    label: 'Intervene',
    blurb: 'May also bench a losing persona into shadow mode and blacklist a repeat-loser contract family.',
  },
];

export default function LouAutonomy() {
  const [level, setLevel] = useState<Level | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await fetch('/api/lou/autonomy');
      if (!res.ok) return;
      const data = await res.json();
      if (data?.level) setLevel(data.level as Level);
    } catch {
      /* keep last-known */
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const choose = async (next: Level) => {
    if (next === level || saving) return;
    const previous = level;
    setLevel(next); // optimistic; reverted below if the write fails
    setSaving(true);
    setError(null);
    try {
      const res = await fetch('/api/lou/autonomy', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ level: next }),
      });
      if (!res.ok) throw new Error(await res.text());
    } catch {
      setLevel(previous);
      setError('Could not save — Lou keeps its current level.');
    } finally {
      setSaving(false);
    }
  };

  if (level === null) return null;

  const activeIdx = LEVELS.findIndex((l) => l.id === level);

  return (
    <section className="bg-[var(--card-bg)] border border-white/10 rounded-2xl p-5 flex flex-col gap-3">
      <div className="flex justify-between items-center flex-wrap gap-2">
        <h3 className="text-sm font-bold flex items-center gap-2">
          <ShieldCheck className="w-4 h-4 text-[var(--primary)]" />
          Lou — Autonomy
        </h3>
        <span className="text-[10px] font-mono text-[var(--text-muted)]">
          applies at the next nightly pass
        </span>
      </div>

      <div className="grid grid-cols-3 gap-2">
        {LEVELS.map((l, i) => {
          const active = l.id === level;
          return (
            <button
              key={l.id}
              onClick={() => choose(l.id)}
              disabled={saving}
              aria-pressed={active}
              className={`rounded-lg border px-3 py-2 text-left transition-colors disabled:opacity-60 ${
                active
                  ? 'border-[var(--primary)] bg-[var(--primary)]/10'
                  : 'border-white/10 hover:border-white/25'
              }`}
            >
              <div className="flex items-center gap-1.5">
                <span
                  className={`text-[10px] font-mono ${
                    i <= activeIdx ? 'text-[var(--primary)]' : 'text-[var(--text-muted)]'
                  }`}
                >
                  {'●'.repeat(i + 1)}
                </span>
                <span className={`text-xs font-bold ${active ? 'text-[var(--text-main)]' : 'text-[var(--text-muted)]'}`}>
                  {l.label}
                </span>
              </div>
            </button>
          );
        })}
      </div>

      <p className="text-[11px] text-[var(--text-muted)] leading-relaxed">
        {LEVELS[activeIdx]?.blurb}
      </p>
      <p className="text-[10px] font-mono text-[var(--text-muted)] leading-relaxed">
        Every level writes only to <span className="text-[var(--text-main)]">runs/lou/overrides.env</span>, which is
        sourced at launch — nothing here touches a running process. Undo with{' '}
        <span className="text-[var(--text-main)]">lou -rollback</span>. Lou never un-benches or un-blacklists on its
        own; reversing an intervention is a human decision.
      </p>
      {error && <p className="text-[11px] text-[var(--accent-red)]">{error}</p>}
    </section>
  );
}
