import React, { useCallback, useEffect, useState } from 'react';
import { SlidersHorizontal, RefreshCw, Settings, Palette, Wallet, RotateCcw, Download, AlertTriangle } from 'lucide-react';
import GlossaryTooltip from './GlossaryTooltip';
import ModelControls from './ModelControls';

type Theme =
  | 'modern' | 'light' | 'bladerunner' | 'synth84' | 'royal'
  | 'django' | 'everforest' | 'jackiebrown' | 'letterpress' | 'mellifluous'
  | 'belafonte' | 'bluedolphin' | 'cyberdyne'
  | 'blackmesa' | 'aperture'
  | 'base16icy' | 'batman' | 'darkmatrix' | 'emeraldsynth'
  | 'hax0rblue' | 'laser' | 'liquidcarbon' | 'miamiheat'
  | 'neonpurple' | 'ocean' | 'sleepyhollow' | 'kanagawalotus';

interface PaperAccount {
  starting_capital: number;
  cash: number;
  nav: number;
  realized_pnl: number;
  unrealized_pnl: number;
  open_positions: number;
  drawdown_pct: number;
}

interface ControlsProps {
  // Supervision (auto_approve) is backend state owned by breen; App polls it for
  // the nav badge and hands the toggle down here.
  isUnsupervised: boolean;
  toggleSupervision: () => void;
  theme: Theme;
  setTheme: React.Dispatch<React.SetStateAction<Theme>>;
}

const fmtUSD = (v: number) =>
  new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 }).format(v ?? 0);

export const Controls: React.FC<ControlsProps> = ({
  isUnsupervised, toggleSupervision,
  theme, setTheme,
}) => {
  const [account, setAccount] = useState<PaperAccount | null>(null);
  const [resetting, setResetting] = useState(false);
  const [resetDialogOpen, setResetDialogOpen] = useState(false);
  const [resetNote, setResetNote] = useState('');
  const [resetError, setResetError] = useState<string | null>(null);

  const fetchAccount = useCallback(async () => {
    try {
      const a = await fetch('/api/account');
      if (a.ok) setAccount(await a.json());
    } catch {
      /* keep last-known account */
    }
  }, []);

  useEffect(() => {
    fetchAccount();
    const interval = setInterval(fetchAccount, 10000);
    return () => clearInterval(interval);
  }, [fetchAccount]);

  // Downloads a zip of the full run (settings, trades, equity, run logs + note)
  // so it can be dropped into any analysis tool before the run is wiped.
  const downloadRunBundle = async (note: string) => {
    const res = await fetch('/api/run/bundle', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ note }),
    });
    if (!res.ok) throw new Error(`bundle export failed (HTTP ${res.status})`);
    const blob = await res.blob();
    const filename = res.headers.get('Content-Disposition')?.match(/filename="([^"]+)"/)?.[1]
      ?? `gekko-run-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.zip`;
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };

  const confirmReset = async (withExport: boolean) => {
    setResetting(true);
    setResetError(null);
    if (withExport) {
      // Liquidate open positions first (bounded wait) so their closing trades are
      // in the export. Reset liquidates any leftovers server-side regardless.
      try {
        await fetch('/api/run/liquidate', { method: 'POST' });
        for (let i = 0; i < 16; i++) {
          await new Promise(res => setTimeout(res, 500));
          const a = await fetch('/api/account');
          if (a.ok && (await a.json()).open_positions === 0) break;
        }
      } catch {
        /* breen offline or slow — proceed; reset handles leftovers */
      }
      try {
        await downloadRunBundle(resetNote);
      } catch (err) {
        // Keep the dialog open so the run isn't wiped without its export.
        setResetError(err instanceof Error ? err.message : 'export failed');
        setResetting(false);
        return;
      }
    }
    try {
      await fetch('/api/run/reset', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ note: resetNote }),
      });
    } catch {
      /* ignore — next poll reflects state */
    }
    setResetDialogOpen(false);
    setResetNote('');
    setResetting(false);
    setTimeout(fetchAccount, 600);
  };

  return (
    <div className="max-w-[1400px] mx-auto mt-8 px-6 pb-16 flex flex-col gap-6">
      <h2 className="text-lg font-bold flex items-center gap-2">
        <SlidersHorizontal className="w-5 h-5 text-[var(--primary)]" />
        Controls
      </h2>

      {/* Live model controls (moved here from the Home tab). Max slippage lives
          inside as a real backend knob — the old display-only slider is gone. */}
      <ModelControls />

      {/* Executive Controls (moved here from the System tab; the dead WET/DRY and
          RAG-depth knobs were removed — they never reached the backend) */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
          <Settings className="w-4 h-4 text-[var(--primary)]" />
          Executive Controls
        </h3>

        <div className="grid grid-cols-1 md:grid-cols-2 gap-x-8 gap-y-4 text-xs font-mono">
          {/* Supervised / Unsupervised Toggle */}
          <div className="flex flex-col gap-2">
            <div className="flex justify-between items-center">
              <GlossaryTooltip termKey="Supervision Control Mode" className="text-[var(--text-muted)] font-medium">
                Supervision Control Mode
              </GlossaryTooltip>
              <span className={`font-bold px-2 py-0.5 rounded text-[10px] ${!isUnsupervised ? 'bg-indigo-500/10 text-indigo-400 border border-indigo-500/20' : 'bg-rose-500/10 text-rose-400 border border-rose-500/20'}`}>
                {!isUnsupervised ? 'SUPERVISED (MANUAL)' : 'UNSUPERVISED (AUTO)'}
              </span>
            </div>
            <button
              type="button"
              onClick={toggleSupervision}
              className={`w-full py-2.5 px-4 rounded-lg border font-bold flex items-center justify-center gap-2 transition-all cursor-pointer
                ${!isUnsupervised
                  ? 'bg-indigo-500/10 hover:bg-indigo-500/15 border-indigo-500/40 text-indigo-300'
                  : 'bg-rose-500/10 hover:bg-rose-500/15 border-rose-500/40 text-rose-300'
                }`}
            >
              <RefreshCw className="w-3.5 h-3.5" />
              Switch to {!isUnsupervised ? 'Unsupervised (Auto)' : 'Supervised (Manual)'}
            </button>
          </div>
        </div>
      </section>

      {/* Paper Trading Run management (moved here from the System tab; the account
          analytics live on the Home tab) */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
        <div className="flex justify-between items-center border-b border-white/5 pb-2">
          <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] font-bold flex items-center gap-2">
            <Wallet className="w-4 h-4 text-[var(--primary)]" />
            Paper Trading Run
          </h3>
          <div className="flex items-center gap-2">
            {/* Download run-log exports */}
            {[
              { file: 'trades.csv', label: 'Trades CSV' },
              { file: 'equity.csv', label: 'Equity CSV' },
              { file: 'run_summary.txt', label: 'Summary' },
            ].map(d => (
              <a
                key={d.file}
                href={`/api/run/export?file=${d.file}`}
                className="flex items-center gap-1 px-2.5 py-1 rounded-md border border-white/10 bg-white/[0.03] hover:border-[var(--primary)]/50 hover:bg-white/[0.06] text-[var(--text-muted)] hover:text-[var(--text-main)] font-bold text-[10px] font-mono transition-all"
              >
                <Download className="w-3 h-3" />
                {d.label}
              </a>
            ))}
            <button
              type="button"
              onClick={() => { setResetError(null); setResetDialogOpen(true); }}
              disabled={resetting}
              className="flex items-center gap-1.5 px-3 py-1 rounded-md border border-rose-500/40 bg-rose-500/10 hover:bg-rose-500/20 text-rose-300 font-bold text-[10px] font-mono transition-all cursor-pointer disabled:opacity-50"
            >
              <RotateCcw className="w-3 h-3" />
              {resetting ? 'Resetting…' : `Reset run ($${(account?.starting_capital ?? 100000).toLocaleString()})`}
            </button>
          </div>
        </div>

        <p className="text-[10px] font-mono text-[var(--text-muted)]">
          {account
            ? <>Current NAV <span className="text-[var(--text-main)] font-bold">{fmtUSD(account.nav)}</span> · {account.open_positions} open positions · started at ${(account.starting_capital ?? 100000).toLocaleString()}. </>
            : <>No account data — start the stack with ./run.sh. </>}
          For an unattended run, supervision must be{' '}
          <span className={isUnsupervised ? 'text-[var(--accent-green)]' : 'text-amber-400'}>
            {isUnsupervised ? 'UNSUPERVISED (auto) ✓' : 'switched to UNSUPERVISED above'}
          </span>. Watch progress in the Home tab.
        </p>
      </section>

      {/* Appearance (theme selector) */}
      <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
        <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
          <Palette className="w-4 h-4 text-[var(--primary)]" />
          Appearance
        </h3>
        <div className="flex justify-between items-center text-xs font-mono">
          <span className="text-[var(--text-muted)] uppercase tracking-wider font-semibold">Interface Theme</span>
          <select
            value={theme}
            onChange={(e) => setTheme(e.target.value as Theme)}
            className="bg-white/5 border border-[var(--border-color)] text-[var(--text-main)] py-1.5 px-3 rounded-md outline-none cursor-pointer hover:border-[var(--primary)] transition-all"
          >
            <option value="modern" className="bg-slate-950 text-slate-100">CYBER MODERN (DARK)</option>
            <option value="light" className="bg-slate-950 text-slate-100">CLEAN LIGHT</option>
            <option value="bladerunner" className="bg-slate-950 text-slate-100">RETRO BR-1982</option>
            <option value="blackmesa" className="bg-slate-950 text-slate-100">BLACK MESA (HALF-LIFE)</option>
            <option value="aperture" className="bg-slate-950 text-slate-100">APERTURE SCIENCE (PORTAL)</option>
            <option value="synth84" className="bg-slate-950 text-slate-100">SYNTHWAVE '84</option>
            <option value="royal" className="bg-slate-950 text-slate-100">HIS MAJESTY ROYAL</option>
            <option value="django" className="bg-slate-950 text-slate-100">DJANGO SMOOTH</option>
            <option value="jackiebrown" className="bg-slate-950 text-slate-100">JACKIE BROWN</option>
            <option value="mellifluous" className="bg-slate-950 text-slate-100">MELLIFLUOUS</option>
            <option value="everforest" className="bg-slate-950 text-slate-100">EVERFOREST LIGHT</option>
            <option value="letterpress" className="bg-slate-950 text-slate-100">LETTERPRESS</option>
            <option value="belafonte" className="bg-slate-950 text-slate-100">BELAFONTE NIGHT</option>
            <option value="bluedolphin" className="bg-slate-950 text-slate-100">BLUE DOLPHIN</option>
            <option value="cyberdyne" className="bg-slate-950 text-slate-100">CYBERDYNE</option>
            <option value="base16icy" className="bg-slate-950 text-slate-100">BASE16 ICY</option>
            <option value="batman" className="bg-slate-950 text-slate-100">BATMAN</option>
            <option value="darkmatrix" className="bg-slate-950 text-slate-100">DARKMATRIX</option>
            <option value="emeraldsynth" className="bg-slate-950 text-slate-100">EMERALD SYNTH</option>
            <option value="hax0rblue" className="bg-slate-950 text-slate-100">HAX0R BLUE</option>
            <option value="laser" className="bg-slate-950 text-slate-100">LASER</option>
            <option value="liquidcarbon" className="bg-slate-950 text-slate-100">LIQUID CARBON</option>
            <option value="miamiheat" className="bg-slate-950 text-slate-100">MIAMI HEAT</option>
            <option value="neonpurple" className="bg-slate-950 text-slate-100">NEON PURPLE</option>
            <option value="ocean" className="bg-slate-950 text-slate-100">OCEAN</option>
            <option value="sleepyhollow" className="bg-slate-950 text-slate-100">SLEEPY HOLLOW</option>
            <option value="kanagawalotus" className="bg-slate-950 text-slate-100">KANAGAWA LOTUS (LIGHT)</option>
          </select>
        </div>
      </section>

      {/* Reset-run dialog: export the run as a zip (with a commit-style note), then reset. */}
      {resetDialogOpen && (
        <div
          className="fixed inset-0 z-50 bg-black/60 backdrop-blur-sm flex items-center justify-center p-6"
          onClick={() => { if (!resetting) setResetDialogOpen(false); }}
        >
          <div
            className="w-full max-w-md bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-6 shadow-2xl flex flex-col gap-4"
            onClick={e => e.stopPropagation()}
          >
            <h3 className="text-sm font-bold flex items-center gap-2">
              <RotateCcw className="w-4 h-4 text-rose-400" />
              Reset paper run
            </h3>
            <p className="text-xs font-mono text-[var(--text-muted)] leading-relaxed">
              This liquidates all open positions at their current marks, clears trades and the
              equity curve, and restores cash to{' '}
              <span className="text-[var(--text-main)] font-bold">
                ${(account?.starting_capital ?? 100000).toLocaleString()}
              </span>
              . A zip with the run's settings, trades (including the liquidations), equity curve
              and logs is downloaded first so you can load it into any analysis tool.
            </p>
            <div className="flex flex-col gap-1.5">
              <label htmlFor="reset-note" className="text-[10px] font-mono text-[var(--text-muted)] uppercase tracking-wider font-semibold">
                Run note (optional)
              </label>
              <textarea
                id="reset-note"
                value={resetNote}
                onChange={e => setResetNote(e.target.value)}
                placeholder="What was this run testing? e.g. consensus on, 6% category cap, deepseek persona…"
                rows={3}
                autoFocus
                className="w-full bg-white/5 border border-[var(--border-color)] rounded-lg px-3 py-2 text-xs font-mono text-[var(--text-main)] placeholder:text-[var(--text-muted)]/50 outline-none focus:border-[var(--primary)] resize-none transition-colors"
              />
              <p className="text-[10px] font-mono text-[var(--text-muted)]">
                Saved as run_note.txt in the zip and in the server-side archive.
              </p>
            </div>
            {resetError && (
              <div className="flex items-start gap-2 bg-rose-500/10 border border-rose-500/30 rounded-lg px-3 py-2">
                <AlertTriangle className="w-3.5 h-3.5 text-rose-400 shrink-0 mt-0.5" />
                <p className="text-[10px] font-mono text-rose-300">
                  Export failed ({resetError}). The run was NOT reset. Retry, or reset without the export.
                </p>
              </div>
            )}
            <div className="flex justify-end items-center gap-2 pt-1">
              <button
                type="button"
                onClick={() => setResetDialogOpen(false)}
                disabled={resetting}
                className="px-3 py-1.5 rounded-md border border-white/10 hover:border-white/25 text-[var(--text-muted)] hover:text-[var(--text-main)] font-bold text-[10px] font-mono transition-all cursor-pointer disabled:opacity-50"
              >
                Cancel
              </button>
              {resetError && (
                <button
                  type="button"
                  onClick={() => confirmReset(false)}
                  disabled={resetting}
                  className="px-3 py-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 hover:bg-amber-500/20 text-amber-300 font-bold text-[10px] font-mono transition-all cursor-pointer disabled:opacity-50"
                >
                  Reset without export
                </button>
              )}
              <button
                type="button"
                onClick={() => confirmReset(true)}
                disabled={resetting}
                className="flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-rose-500/40 bg-rose-500/10 hover:bg-rose-500/20 text-rose-300 font-bold text-[10px] font-mono transition-all cursor-pointer disabled:opacity-50"
              >
                <Download className="w-3 h-3" />
                {resetting ? 'Liquidating & exporting…' : 'Export zip & reset'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default Controls;
