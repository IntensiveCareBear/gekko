import { useCallback, useEffect, useState } from 'react';
import { BookOpen, RefreshCw, SlidersHorizontal } from 'lucide-react';

// Lou's lesson book (docs/lou-learning-loop-proposal.md): evidence-backed
// patterns distilled from past runs, plus the auto-tune audit trail. Read-only —
// lessons are written by the `lou` CLI; this panel just makes them visible.

interface LouEvidence {
  trades: number;
  wins: number;
  losses: number;
  pnl: number;
  win_rate_pct: number;
  runs: string[];
  detail?: string;
}

interface LouLesson {
  id: string;
  pattern: string;
  evidence: LouEvidence;
  confidence: number;
  suggested_action?: string;
  status: 'proposed' | 'validated' | 'retired';
  last_updated: string;
}

interface TuneChange {
  at: string;
  knob: string;
  old: number;
  new: number;
  lesson_id?: string;
  action: 'tune' | 'rollback';
}

interface LouResponse {
  lessons: LouLesson[];
  tuning: TuneChange[];
  overrides: Record<string, string>;
  generated_at?: string;
}

const fmtUSD = (v: number) =>
  new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 }).format(v ?? 0);

const STATUS_BADGE: Record<LouLesson['status'], string> = {
  validated: 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]',
  proposed: 'bg-amber-500/10 border-amber-500/30 text-amber-400',
  retired: 'bg-white/5 border-white/10 text-[var(--text-muted)]',
};

export default function LouLessons() {
  const [data, setData] = useState<LouResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [showRetired, setShowRetired] = useState(false);

  const load = useCallback(async () => {
    try {
      const res = await fetch('/api/lou/lessons');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setData(await res.json());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void load();
    const t = setInterval(() => void load(), 60_000); // Lou rewrites at most daily
    return () => clearInterval(t);
  }, [load]);

  const lessons = (data?.lessons ?? [])
    .filter((l) => showRetired || l.status !== 'retired')
    .sort((a, b) => {
      if ((a.status === 'validated') !== (b.status === 'validated')) return a.status === 'validated' ? -1 : 1;
      return b.confidence - a.confidence;
    });
  const retiredCount = (data?.lessons ?? []).filter((l) => l.status === 'retired').length;
  const overrides = Object.entries(data?.overrides ?? {});

  return (
    <section className="bg-[var(--card-bg)] border border-white/10 rounded-2xl p-5 flex flex-col gap-4">
      <div className="flex justify-between items-center">
        <h3 className="text-sm font-bold flex items-center gap-2">
          <BookOpen className="w-4 h-4 text-[var(--primary)]" />
          Lou — Lesson Book
          <span className="text-[10px] font-mono font-normal text-[var(--text-muted)]">
            advisory · learned from past runs
          </span>
        </h3>
        <div className="flex items-center gap-3 text-xs font-mono text-[var(--text-muted)]">
          {data?.generated_at && <span>updated {new Date(data.generated_at).toLocaleString()}</span>}
          {retiredCount > 0 && (
            <button
              onClick={() => setShowRetired((v) => !v)}
              className="px-2 py-0.5 rounded border border-white/10 hover:border-white/25 transition-colors cursor-pointer"
            >
              {showRetired ? 'hide' : 'show'} {retiredCount} retired
            </button>
          )}
          <button
            onClick={() => void load()}
            className="flex items-center gap-1.5 px-2 py-0.5 rounded border border-white/10 hover:border-white/25 transition-colors cursor-pointer"
          >
            <RefreshCw className="w-3 h-3" />
          </button>
        </div>
      </div>

      {error && (
        <p className="text-xs font-mono text-rose-400">Lesson endpoint unreachable ({error}).</p>
      )}

      {!error && lessons.length === 0 && (
        <p className="text-xs font-mono text-[var(--text-muted)]">
          No lessons yet — Lou hasn&apos;t run. Run <code className="text-[var(--text-main)]">cd lou && go run . -runs ../runs</code>{' '}
          after a trading run to distill the first lesson book.
        </p>
      )}

      {lessons.length > 0 && (
        <div className="flex flex-col gap-2 max-h-96 overflow-y-auto pr-1">
          {lessons.map((l) => (
            <div key={l.id} className="border border-white/5 rounded-lg p-3 flex flex-col gap-1.5">
              <div className="flex justify-between items-start gap-3">
                <p className="text-xs text-[var(--text-main)] leading-snug">{l.pattern}</p>
                <span
                  className={`px-2 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider shrink-0 ${STATUS_BADGE[l.status]}`}
                >
                  {l.status}
                </span>
              </div>
              <div className="flex items-center gap-3 text-[10px] font-mono text-[var(--text-muted)]">
                <span>
                  confidence{' '}
                  <span className="text-[var(--text-main)] font-bold">{(l.confidence * 100).toFixed(0)}%</span>
                </span>
                <span>{l.evidence.trades.toLocaleString()} trades</span>
                <span>{l.evidence.win_rate_pct.toFixed(0)}% win</span>
                <span className={l.evidence.pnl < 0 ? 'text-rose-400' : 'text-[var(--accent-green)]'}>
                  {fmtUSD(l.evidence.pnl)}
                </span>
                <span>{l.evidence.runs?.length ?? 0} runs</span>
              </div>
              {l.suggested_action && (
                <p className="text-[10px] font-mono text-amber-400/90">→ {l.suggested_action}</p>
              )}
            </div>
          ))}
        </div>
      )}

      {(overrides.length > 0 || (data?.tuning.length ?? 0) > 0) && (
        <div className="border-t border-white/5 pt-3 flex flex-col gap-2">
          <h4 className="text-[11px] font-bold flex items-center gap-1.5 text-[var(--text-muted)] uppercase tracking-wider">
            <SlidersHorizontal className="w-3 h-3" />
            Auto-tune (paper)
          </h4>
          {overrides.length > 0 && (
            <div className="flex flex-wrap gap-2">
              {overrides.map(([k, v]) => (
                <span
                  key={k}
                  className="px-2 py-0.5 rounded border border-[var(--primary)]/30 bg-[var(--primary)]/10 text-[10px] font-mono text-[var(--text-main)]"
                >
                  {k}={v}
                </span>
              ))}
            </div>
          )}
          <div className="flex flex-col gap-1">
            {(data?.tuning ?? []).slice(0, 6).map((c, i) => (
              <div key={i} className="flex justify-between items-center text-[10px] font-mono">
                <span className="text-[var(--text-muted)]">
                  {new Date(c.at).toLocaleString()} · {c.action === 'rollback' ? 'rollback' : c.lesson_id}
                </span>
                <span className="text-[var(--text-main)] font-bold">
                  {c.knob}: {c.old} → {c.new}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </section>
  );
}
