import React, { useCallback, useEffect, useState } from 'react';
import { Target } from 'lucide-react';
import { PERSONA_LABEL } from '../data/personas';

interface CalibrationBin {
  lo: number;
  hi: number;
  midpoint: number;
  trades: number;
  wins: number;
  win_rate: number;
  adjusted: number;
  gap_pct: number;
  thin: boolean;
}

interface PersonaCalibration {
  persona: string;
  trades: number;
  win_rate: number;
  bins: CalibrationBin[];
}

interface CalibrationData {
  bins?: number;
  min_samples?: number;
  // false = the table is only being watched; true = consensus and the mid-band gate
  // are reasoning on it. The same numbers mean different things either way.
  enabled?: boolean;
  personas?: PersonaCalibration[];
  updated_at_ms?: number;
}

// A bin's colour is its GAP — how far the realized win rate sits from what the persona
// claimed — not its win rate. A 30% bucket delivering 30% is perfectly calibrated and
// should read as neutral; a 90% bucket delivering 30% is the thing worth seeing.
const gapTone = (gap: number, thin: boolean) => {
  if (thin) return 'text-[var(--text-muted)]';
  if (gap <= -25) return 'text-[var(--accent-red)] font-bold';
  if (gap <= -10) return 'text-[var(--accent-red)]';
  if (gap >= 10) return 'text-[var(--accent-green)]';
  return 'text-[var(--text-main)]';
};

const label = (persona: string) => PERSONA_LABEL[persona] ?? persona;

// CalibrationTable renders breen's confidence-calibration table: for each persona, the
// win rate its stated-confidence buckets actually delivered. Display-only — nothing
// trades off it yet (breen-intelligence Phase 0). It answers one question before any
// code acts on it: is the fleet's confidence miscalibrated enough to be worth
// correcting? Flat rows across the buckets mean conviction carries no information, and
// the calibrated-confidence work is not worth building.
const CalibrationTable: React.FC = () => {
  const [data, setData] = useState<CalibrationData | null>(null);
  const [loaded, setLoaded] = useState(false);

  const fetchCalibration = useCallback(async () => {
    try {
      const res = await fetch('/api/calibration');
      if (res.ok) {
        const json = await res.json();
        if (json && typeof json === 'object') setData(json as CalibrationData);
      }
    } catch {
      /* keep last-known */
    } finally {
      setLoaded(true);
    }
  }, []);

  useEffect(() => {
    fetchCalibration();
    const id = setInterval(fetchCalibration, 15000);
    return () => clearInterval(id);
  }, [fetchCalibration]);

  const personas = (data?.personas ?? []).filter(p => p.trades > 0);
  if (!loaded || personas.length === 0) return null;

  const binCount = data?.bins ?? personas[0].bins.length;
  const headers = personas[0].bins.map(b => `${b.lo.toFixed(1)}–${b.hi.toFixed(1)}`);

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md">
      <div className="flex items-center gap-2 mb-1 flex-wrap">
        <Target className="w-4 h-4 text-[var(--primary)]" />
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] font-bold">Confidence Calibration</h3>
        <span
          className={`text-[9px] uppercase tracking-wider font-bold px-1.5 py-0.5 rounded ${
            data?.enabled
              ? 'bg-[var(--primary)]/15 text-[var(--primary)]'
              : 'bg-white/5 text-[var(--text-muted)]'
          }`}
          title={
            data?.enabled
              ? 'consensus and the mid-band gate are reasoning on these numbers'
              : 'recorded and displayed only — no trading decision reads this yet'
          }
        >
          {data?.enabled ? 'steering' : 'watching'}
        </span>
        <span className="text-[10px] text-[var(--text-muted)] ml-auto font-mono">
          {binCount} bins · shrunk below {data?.min_samples ?? 20} trades
        </span>
      </div>
      <p className="text-[10px] text-[var(--text-muted)] mb-4 font-mono leading-relaxed">
        Realized win rate inside each stated-confidence bucket. Red = the persona claimed more than it delivered.
        Greyed cells are too thin to believe and are pulled toward the persona&apos;s overall rate.
      </p>

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="text-[10px] uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5">
              <th className="text-left font-semibold py-2">Persona</th>
              <th className="text-right font-semibold py-2">Trades</th>
              <th className="text-right font-semibold py-2">Overall</th>
              {headers.map(h => (
                <th key={h} className="text-right font-semibold py-2">{h}</th>
              ))}
            </tr>
          </thead>
          <tbody className="font-mono">
            {personas.map(p => (
              <tr key={p.persona} className="border-b border-white/[0.04]">
                <td className="py-1.5 text-[var(--text-main)] whitespace-nowrap">{label(p.persona)}</td>
                <td className="text-right py-1.5 text-[var(--text-muted)]">{p.trades}</td>
                <td className="text-right py-1.5 text-[var(--text-muted)]">{p.win_rate.toFixed(0)}%</td>
                {p.bins.map(b => (
                  <td
                    key={`${p.persona}-${b.lo}`}
                    className={`text-right py-1.5 ${gapTone(b.gap_pct, b.thin)}`}
                    title={
                      b.trades === 0
                        ? 'no trades in this confidence bucket'
                        : `${b.wins}/${b.trades} won · raw ${b.win_rate.toFixed(0)}% · claimed ${(b.midpoint * 100).toFixed(0)}% · gap ${b.gap_pct.toFixed(0)}pts`
                    }
                  >
                    {b.trades === 0 ? '·' : `${b.adjusted.toFixed(0)}%`}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
};

export default CalibrationTable;
