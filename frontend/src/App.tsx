import { useState, useEffect } from 'react';
import { Home, Activity, Server, Sparkles, Bug, Cpu, SlidersHorizontal, ChevronLeft, LayoutGrid } from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import SimpleHome from './components/SimpleHome';
import Dashboard from './components/Dashboard';
import PortfolioAnalytics from './components/PortfolioAnalytics';
import SystemStatus from './components/SystemStatus';
import SystemMetrics from './components/SystemMetrics';
import GordonScorecard from './components/GordonScorecard';
import AssistantChat from './components/AssistantChat';
import DebugConsole from './components/DebugConsole';
import Controls from './components/Controls';
import ErrorBoundary from './components/ErrorBoundary';

export type Theme =
  | 'modern' | 'light' | 'bladerunner' | 'synth84' | 'royal'
  | 'django' | 'everforest' | 'jackiebrown' | 'letterpress' | 'mellifluous'
  | 'belafonte' | 'bluedolphin' | 'cyberdyne'
  | 'base16icy' | 'batman' | 'darkmatrix' | 'emeraldsynth'
  | 'hax0rblue' | 'laser' | 'liquidcarbon' | 'miamiheat'
  | 'neonpurple' | 'ocean' | 'sleepyhollow' | 'kanagawalotus'
  | 'blackmesa' | 'aperture';

type View = 'strategy' | 'portfolio' | 'controls' | 'status' | 'metrics' | 'assistant' | 'debug';

const TABS: { id: View; label: string; Icon: LucideIcon }[] = [
  { id: 'portfolio', label: 'HOME', Icon: Home },
  { id: 'strategy', label: 'BREEN', Icon: Activity },
  { id: 'controls', label: 'CONTROLS', Icon: SlidersHorizontal },
  { id: 'status', label: 'SYSTEM', Icon: Server },
  { id: 'metrics', label: 'GORDONS', Icon: Cpu },
  { id: 'debug', label: 'DEBUG', Icon: Bug },
];

function App() {
  const [view, setView] = useState<View>('portfolio');

  // Simple vs. Advanced. Simple is the default landing: a few knobs + high-level
  // numbers. Advanced reveals the full tabbed dashboard (streams, per-Gordon
  // metrics, debug, run management). Persisted so power users stay in Advanced.
  const [advanced, setAdvanced] = useState<boolean>(
    () => localStorage.getItem('gekko_advanced') === '1'
  );
  useEffect(() => {
    localStorage.setItem('gekko_advanced', advanced ? '1' : '0');
  }, [advanced]);

  // One-time cleanup of removed knobs: WET/DRY and RAG depth never reached the
  // backend; max slippage moved into /api/controls/model (backend-owned).
  useEffect(() => {
    localStorage.removeItem('gekko_wet_dry');
    localStorage.removeItem('gekko_rag_depth');
    localStorage.removeItem('gekko_max_slippage');
  }, []);

  // Supervision mode (auto_approve) is backend state owned by breen. App polls it
  // so the nav badge stays truthful on every tab; the Controls tab flips it.
  const [isUnsupervised, setIsUnsupervised] = useState<boolean>(false);
  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const res = await fetch('/api/controls');
        if (res.ok) {
          const d = await res.json();
          if (alive && typeof d?.auto_approve === 'boolean') setIsUnsupervised(d.auto_approve);
        }
      } catch {
        /* backend offline — keep last-known value */
      }
    };
    poll();
    const interval = setInterval(poll, 5000);
    return () => { alive = false; clearInterval(interval); };
  }, []);

  const toggleSupervision = async () => {
    // Optimistic flip; backend (breen) is the source of truth on next poll.
    setIsUnsupervised(prev => !prev);
    try {
      await fetch('/api/controls/toggle', { method: 'POST' });
    } catch {
      /* backend offline — optimistic local toggle already applied */
    }
  };

  // Theme lives here (App never unmounts) so it persists across tab switches and
  // survives reloads. Previously it lived in Dashboard, which remounts per tab and
  // reset the theme to default whenever you returned to the Strategy Stream.
  const [theme, setTheme] = useState<Theme>(
    () => (localStorage.getItem('gekko_theme') as Theme) || 'modern'
  );

  useEffect(() => {
    document.body.className = '';
    const cls = document.body.classList;
    if (theme === 'light') cls.add('theme-light');
    else if (theme === 'bladerunner') cls.add('theme-bladerunner');
    else if (theme === 'synth84') cls.add('theme-synth84');
    else if (theme === 'royal') cls.add('theme-royal');
    else if (theme === 'django') cls.add('theme-django');
    else if (theme === 'jackiebrown') cls.add('theme-jackiebrown');
    else if (theme === 'mellifluous') cls.add('theme-mellifluous');
    else if (theme === 'belafonte') cls.add('theme-belafonte');
    else if (theme === 'bluedolphin') cls.add('theme-bluedolphin');
    else if (theme === 'cyberdyne') cls.add('theme-cyberdyne');
    else if (theme === 'base16icy') cls.add('theme-base16icy');
    else if (theme === 'batman') cls.add('theme-batman');
    else if (theme === 'darkmatrix') cls.add('theme-darkmatrix');
    else if (theme === 'emeraldsynth') cls.add('theme-emeraldsynth');
    else if (theme === 'hax0rblue') cls.add('theme-hax0rblue');
    else if (theme === 'laser') cls.add('theme-laser');
    else if (theme === 'liquidcarbon') cls.add('theme-liquidcarbon');
    else if (theme === 'miamiheat') cls.add('theme-miamiheat');
    else if (theme === 'neonpurple') cls.add('theme-neonpurple');
    else if (theme === 'ocean') cls.add('theme-ocean');
    else if (theme === 'sleepyhollow') cls.add('theme-sleepyhollow');
    else if (theme === 'blackmesa') cls.add('theme-blackmesa');
    // Light-background themes also carry .theme-light so the light-mode
    // compatibility overrides apply; their own class (declared after .theme-light
    // in index.css) then wins the palette variables.
    else if (theme === 'everforest') cls.add('theme-light', 'theme-everforest');
    else if (theme === 'letterpress') cls.add('theme-light', 'theme-letterpress');
    else if (theme === 'kanagawalotus') cls.add('theme-light', 'theme-kanagawalotus');
    else if (theme === 'aperture') cls.add('theme-light', 'theme-aperture');
    localStorage.setItem('gekko_theme', theme);
  }, [theme]);

  // ── Simple mode: minimal chrome + the SimpleHome landing ──────────────────
  if (!advanced) {
    return (
      <>
        <nav className="sticky top-0 z-50 h-11 border-b border-[var(--border-color)] bg-[var(--bg-color)] px-6 flex items-center gap-3 text-xs font-mono">
          <span className="font-bold tracking-tight text-[var(--text-main)] flex items-center gap-2">
            <span className="w-2 h-2 rounded-full bg-[var(--accent-green)] animate-pulse-live shadow-[0_0_10px_var(--accent-green)]" />
            GEKKO
          </span>
          <span
            className={`ml-auto self-center px-2.5 py-1 rounded-full font-bold border text-[10px] ${
              isUnsupervised
                ? 'bg-rose-500/10 border-rose-500/30 text-rose-400'
                : 'bg-indigo-500/10 border-indigo-500/30 text-indigo-400'
            }`}
          >
            {isUnsupervised ? 'UNSUPERVISED (AUTO)' : 'SUPERVISED'}
          </span>
          <button
            type="button"
            onClick={() => setAdvanced(true)}
            className="self-center flex items-center gap-1.5 px-3 py-1.5 rounded-full font-bold border border-white/10 text-[var(--text-muted)] hover:text-[var(--text-main)] hover:border-white/25 transition-all cursor-pointer"
          >
            <LayoutGrid className="w-3.5 h-3.5" />
            ADVANCED
          </button>
        </nav>
        <ErrorBoundary label="Home">
          <SimpleHome isUnsupervised={isUnsupervised} toggleSupervision={toggleSupervision} />
        </ErrorBoundary>
      </>
    );
  }

  return (
    <>
      {/* Global view switcher — topmost sticky bar (above the Dashboard header) */}
      <nav className="sticky top-0 z-50 h-11 border-b border-[var(--border-color)] bg-[var(--bg-color)] px-6 flex gap-1 text-xs font-mono">
        {/* Back to the simplified landing */}
        <button
          type="button"
          onClick={() => setAdvanced(false)}
          className="self-center flex items-center gap-1 px-2.5 py-1.5 rounded-full font-bold border border-white/10 text-[var(--text-muted)] hover:text-[var(--text-main)] hover:border-white/25 transition-all cursor-pointer mr-1"
          title="Back to simple view"
        >
          <ChevronLeft className="w-3.5 h-3.5" />
          SIMPLE
        </button>
        <span className="self-center h-5 w-px bg-[var(--border-color)] mr-2" />

        {/* Assistant — first and visually prominent (accent pill) */}
        <button
          type="button"
          onClick={() => setView('assistant')}
          className={`self-center flex items-center gap-1.5 px-3.5 py-1.5 rounded-full font-bold border transition-all cursor-pointer ${
            view === 'assistant'
              ? 'bg-[var(--primary)]/25 border-[var(--primary)]/60 text-[var(--primary)] shadow-[0_0_12px_var(--primary)]'
              : 'bg-[var(--primary)]/10 border-[var(--primary)]/40 text-[var(--primary)] hover:bg-[var(--primary)]/20 shadow-[0_0_8px_rgba(99,102,241,0.25)]'
          }`}
        >
          <Sparkles className="w-3.5 h-3.5" />
          ASK GEKKO
        </button>

        {/* Divider */}
        <span className="self-center h-5 w-px bg-[var(--border-color)] mx-2" />

        {TABS.map(({ id, label, Icon }) => (
          <button
            key={id}
            type="button"
            onClick={() => setView(id)}
            className={`flex items-center gap-1.5 px-4 py-2.5 border-b-2 transition-all cursor-pointer font-bold ${
              view === id
                ? 'border-indigo-500 text-indigo-400'
                : 'border-transparent text-slate-400 hover:text-slate-100'
            }`}
          >
            <Icon className="w-3.5 h-3.5" />
            {label}
          </button>
        ))}

        {/* Supervision badge — the one safety-critical state, visible on every tab.
            The toggle itself lives in the Controls tab. */}
        <span
          className={`ml-auto self-center px-2.5 py-1 rounded-full font-bold border text-[10px] ${
            isUnsupervised
              ? 'bg-rose-500/10 border-rose-500/30 text-rose-400'
              : 'bg-indigo-500/10 border-indigo-500/30 text-indigo-400'
          }`}
        >
          {isUnsupervised ? 'UNSUPERVISED (AUTO)' : 'SUPERVISED'}
        </span>
      </nav>

      {view === 'strategy' && (
        <ErrorBoundary label="Breen">
          <Dashboard theme={theme} />
        </ErrorBoundary>
      )}
      {view === 'portfolio' && (
        <ErrorBoundary label="Portfolio Analytics">
          <PortfolioAnalytics />
        </ErrorBoundary>
      )}
      {view === 'controls' && (
        <ErrorBoundary label="Controls">
          <Controls
            isUnsupervised={isUnsupervised}
            toggleSupervision={toggleSupervision}
            theme={theme}
            setTheme={setTheme}
          />
        </ErrorBoundary>
      )}
      {view === 'assistant' && (
        <ErrorBoundary label="Assistant">
          <AssistantChat />
        </ErrorBoundary>
      )}
      {view === 'metrics' && (
        <ErrorBoundary label="Gordons">
          <div className="max-w-[1400px] mx-auto mt-8 px-6 pb-16 flex flex-col gap-6">
            <div className="flex items-center gap-3">
              <Cpu className="w-5 h-5 text-[var(--primary)]" />
              <h2 className="text-lg font-bold text-[var(--text-main)]">Gordons</h2>
            </div>
            <GordonScorecard />
          </div>
        </ErrorBoundary>
      )}
      {view === 'debug' && (
        <ErrorBoundary label="Debug Console">
          <DebugConsole />
        </ErrorBoundary>
      )}
      {view === 'status' && (
        <ErrorBoundary label="System">
          <SystemStatus />
          <SystemMetrics theme={theme} />
        </ErrorBoundary>
      )}
    </>
  );
}

export default App;
