import type { Theme } from '../App';
import React, { useState, useEffect } from 'react';
import {
  Server, Clock, AlertTriangle,
  ChevronRight, ExternalLink, User, ArrowUpDown
} from 'lucide-react';
import GlossaryTooltip from './GlossaryTooltip';
import BreenOverview from './BreenOverview';
import { PERSONA_LABEL } from '../data/personas';

// Colour a persona badge by its stance so the A/B fleet is scannable at a
// glance. Matches on substring so every bull/bear/neutral variant is covered;
// anything else (custom personas) falls back to violet.
const personaBadgeClasses = (id: string): string => {
  const s = id.toLowerCase();
  if (s.includes('bull')) return 'bg-emerald-500/10 border-emerald-500/30 text-emerald-300';
  if (s.includes('bear')) return 'bg-rose-500/10 border-rose-500/30 text-rose-300';
  if (s.includes('neutral')) return 'bg-slate-500/10 border-slate-400/30 text-slate-300';
  return 'bg-violet-500/10 border-violet-500/30 text-violet-300';
};

// Types and Schemas
interface IcebergOrder {
  contract_id: string;
  action: string;
  size: number;
  price: number;
  delay_ms: number;
  timestamp: string;
}

interface StrategyProposal {
  contract_id: string;
  action: string;
  average_price: number;
  total_size: number;
  slippage_pct: number;
  limit_breached: boolean;
  breach_reason?: string;
  iceberg_orders?: IcebergOrder[];
  timestamp: string;
  event_hash: string;
  processing_time: string;
  proposal_text?: string;
  slice_boundaries?: number[];
  persona?: string;
  source_id?: string;
  source_url?: string;
  model_used?: string;
  embed_latency_ms?: number;
  qdrant_latency_ms?: number;
  llm_latency_ms?: number;
}

interface DashboardProps {
  // Theme is owned by App (persists across tabs); Dashboard only reads it for
  // its chart colours and header badges. The selector lives in the Controls tab.
  theme: Theme;
}

export const Dashboard: React.FC<DashboardProps> = ({ theme }) => {
  // Supervision mode is backend state (auto_approve in Redis) — read here to hide
  // the manual approval UI in auto mode; the toggle UI lives in the Controls tab.
  const [isUnsupervised, setIsUnsupervised] = useState<boolean>(false);

  // Max slippage cap (percent, e.g. 3.0) — the same backend value Gordon enforces
  // (gekko:model_controls), polled below; used to colour slippage figures.
  const [maxSlippage, setMaxSlippage] = useState<number>(3.0);

  // Live proposal stream state (fed by the SSE link below).
  const [proposals, setProposals] = useState<StrategyProposal[]>([]);
  const [connectionStatus, setConnectionStatus] = useState<'CONNECTING' | 'LIVE' | 'OFFLINE'>('CONNECTING');
  const [pendingProposal, setPendingProposal] = useState<StrategyProposal | null>(null);
  const [actionLoading, setActionLoading] = useState<boolean>(false);

  // Persona filter for the live feed ('all' or a persona id). Filtering happens
  // at render time only, so the underlying buffer keeps its dedup + 50-cap.
  const [personaFilter, setPersonaFilter] = useState<string>('all');

  // Action filter for the live feed ('all', 'BUY', or 'SELL'). Same render-time
  // filtering as personaFilter — the underlying buffer is untouched.
  const [actionFilter, setActionFilter] = useState<'all' | 'BUY' | 'SELL'>('all');

  // Generated "Firstname Gordon" display names, keyed by persona id, from /api/gordons
  // (same source the Gordons tab uses, so display-name overrides are respected). Falls
  // back to the persona label then the raw id.
  const [gordonNames, setGordonNames] = useState<Record<string, string>>({});
  const nameFor = (id: string) => gordonNames[id] ?? PERSONA_LABEL[id] ?? id;


  // Server-Sent Events link listener
  useEffect(() => {
    setConnectionStatus('CONNECTING');

    // Seed from liveview's recent-proposals buffer: SSE only delivers NEW
    // proposals, and the entry gates can keep those minutes apart — without
    // this backfill a fresh page load sits on the empty state until the next
    // proposal happens to fire.
    fetch('/api/proposals')
      .then(res => (res.ok ? res.json() : []))
      .then((data: StrategyProposal[]) => {
        if (Array.isArray(data) && data.length > 0) {
          setProposals(prev => (prev.length === 0 ? data.slice(0, 50) : prev));
        }
      })
      .catch(() => { /* backlog is best-effort; SSE remains the live source */ });

    const sse = new EventSource('/api/stream');

    sse.onopen = () => {
      setConnectionStatus('LIVE');
    };

    sse.onerror = () => {
      setConnectionStatus('OFFLINE');
    };

    sse.onmessage = (event) => {
      try {
        const payload: StrategyProposal = JSON.parse(event.data);
        handleNewProposal(payload);
      } catch (err) {
        console.error("Failed to parse SSE payload", err);
      }
    };

    return () => {
      sse.close();
    };
  }, []);

  // Ingest state polling for positions, ledger summary, pending proposals, and controls (every 2.5s)
  useEffect(() => {
    if (connectionStatus === 'OFFLINE') return;

    const fetchBackendState = async () => {
      try {
        // 1. Fetch pending proposals
        const pendingRes = await fetch('/api/proposals/pending');
        if (pendingRes.ok) {
          const data = await pendingRes.json();
          if (data && data.contract_id) {
            setPendingProposal(data);
          } else {
            setPendingProposal(null);
          }
        } else {
          setPendingProposal(null);
        }

        // 2. Fetch executive controls supervision mode status (hides the manual
        // approval banner in unsupervised/auto mode).
        const controlsRes = await fetch('/api/controls');
        if (controlsRes.ok) {
          const controlsData = await controlsRes.json();
          if (controlsData && typeof controlsData.auto_approve === 'boolean') {
            setIsUnsupervised(controlsData.auto_approve);
          }
        }

        // 3. Fetch the model controls for the live slippage cap (fraction → percent).
        const modelRes = await fetch('/api/controls/model');
        if (modelRes.ok) {
          const modelData = await modelRes.json();
          const slip = Number(modelData?.max_slippage_pct);
          if (Number.isFinite(slip) && slip > 0) setMaxSlippage(slip * 100);
        }

        // 4. Generated gordon names (id → "Firstname Gordon"), so the proposal author
        // shows the gordon's name instead of its raw persona id.
        const gordonsRes = await fetch('/api/gordons');
        if (gordonsRes.ok) {
          const data = await gordonsRes.json();
          if (Array.isArray(data?.gordons)) {
            setGordonNames(Object.fromEntries(
              data.gordons.map((g: { id: string; name: string }) => [g.id, g.name]),
            ));
          }
        }
      } catch (err) {
        console.error("Failed to fetch backend telemetry", err);
      }
    };

    fetchBackendState();
    const interval = setInterval(fetchBackendState, 2500);
    return () => clearInterval(interval);
  }, [connectionStatus]);

  const handleNewProposal = (p: StrategyProposal) => {
    setProposals(prev => {
      // A proposal broadcast between the backlog fetch and SSE connect would
      // arrive twice; same event_hash + timestamp identifies the duplicate.
      if (prev.some(q => q.event_hash === p.event_hash && q.timestamp === p.timestamp)) {
        return prev;
      }
      const next = [p, ...prev];
      return next.slice(0, 50);
    });
  };

  // Dispatch manual approval/denials to Go backend
  const handleApprovalAction = async (approve: boolean) => {
    if (!pendingProposal) return;
    setActionLoading(true);

    if (connectionStatus !== 'OFFLINE') {
      try {
        const endpoint = approve ? '/api/proposals/approve' : '/api/proposals/deny';
        const res = await fetch(endpoint, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ event_hash: pendingProposal.event_hash })
        });
        if (res.ok) {
          // Order routed; the resulting position/ledger updates surface in the
          // Home and System tabs. Just clear the pending banner here.
          setPendingProposal(null);
        }
      } catch (err) {
        console.error("Failed to post order action", err);
      } finally {
        setActionLoading(false);
      }
    } else {
      // Backend offline — can't route the action; surface it and bail (no local fakery).
      console.warn("Cannot action proposal: backend offline.");
      setActionLoading(false);
    }
  };

  // Distinct personas present in the current buffer (sorted, attributed only),
  // used to build the filter chips. Only worth showing when >1 persona appears.
  const personasInStream = Array.from(
    new Set(proposals.map(p => p.persona).filter((id): id is string => !!id))
  ).sort();

  const visibleProposals = proposals
    .filter(p => personaFilter === 'all' || (p.persona || '') === personaFilter)
    .filter(p => actionFilter === 'all' || p.action.toUpperCase() === actionFilter);

  return (
    <div className="min-h-screen text-[var(--text-main)] transition-colors duration-300 relative pb-12">
      {theme === 'bladerunner' && (
        <div className="fixed inset-0 pointer-events-none z-50 bg-[linear-gradient(rgba(18,16,16,0)_50%,rgba(0,0,0,0.25)_50%)] bg-[size:100%_4px]" />
      )}

      {/* Main Header */}
      <header className="sticky top-11 z-40 border-b border-[var(--border-color)] bg-[var(--bg-color)]/95 backdrop-blur-md px-6 py-4 flex justify-between items-center transition-all">
        <div className="flex items-center gap-3">
          <div className="w-2.5 h-2.5 rounded-full bg-[var(--accent-green)] animate-pulse-live shadow-[0_0_10px_var(--accent-green)]" />
          <h1 className="text-xl font-bold tracking-tight bg-gradient-to-r from-white to-[var(--text-muted)] bg-clip-text text-transparent">
            BREEN // STRATEGY STREAM
            {theme === 'bladerunner' && <span className="text-[10px] text-[var(--primary)] ml-2 font-mono">// BR-SCAN-99</span>}
            {theme === 'synth84' && <span className="text-[10px] text-[var(--accent-green)] ml-2 font-mono">// SYNTH-84</span>}
            {theme === 'royal' && <span className="text-[10px] text-[var(--primary)] ml-2 font-mono">// ROYAL COURT</span>}
          </h1>
        </div>

        <div className="flex items-center gap-4 text-xs font-mono">
          <div className={`px-3 py-1 rounded-full font-bold border transition-colors flex items-center gap-1.5 
            ${connectionStatus === 'LIVE' 
              ? 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]' 
              : connectionStatus === 'CONNECTING' 
                ? 'bg-amber-500/10 border-amber-500/20 text-amber-500' 
                : 'bg-rose-500/10 border-rose-500/20 text-rose-500'
            }`}
          >
            <span className={`w-1.5 h-1.5 rounded-full ${connectionStatus === 'LIVE' ? 'bg-[var(--accent-green)]' : connectionStatus === 'CONNECTING' ? 'bg-amber-500 animate-pulse' : 'bg-rose-500'}`} />
            {connectionStatus === 'LIVE' ? 'STREAM LIVE' : connectionStatus === 'CONNECTING' ? 'CONNECTING SSE' : 'OFFLINE'}
          </div>
        </div>
      </header>

      {/* Main Frame — proposals feed. Quantitative analytics now live on the Home
          tab; the ledger audit integrity panel moved to the System tab. */}
      <main className="max-w-[1400px] mx-auto mt-8 px-6 flex flex-col gap-8">

        {/* Breen's portfolio read + last-24h summary + knobs */}
        <BreenOverview />

        {/* Active Pending approval banner + Proposals feed */}
        <div className="flex flex-col gap-6">

          {/* Dynamic Active Pending Proposal Banner — hidden in unsupervised (auto) mode */}
          {!isUnsupervised && pendingProposal && (
            <div className="bg-amber-500/10 border-2 border-amber-500/30 rounded-2xl p-6 shadow-lg backdrop-blur-md relative overflow-hidden flex flex-col sm:flex-row justify-between items-center gap-4 animate-in slide-in-from-top-4 duration-300">
              <div className="flex items-center gap-4">
                <div className="p-3 bg-amber-500/20 rounded-xl text-amber-500 border border-amber-500/30 animate-pulse">
                  <AlertTriangle className="w-6 h-6" />
                </div>
                <div className="flex flex-col gap-1">
                  <span className="text-[10px] font-bold tracking-wider uppercase text-amber-400 font-mono">
                    🚨 Executive Order Pending Approval
                  </span>
                  <h4 className="text-sm font-bold text-slate-100 font-mono">
                    {pendingProposal.action} {pendingProposal.total_size.toLocaleString()} units of {pendingProposal.contract_id} @ ${pendingProposal.average_price.toFixed(4)}
                  </h4>
                  <p className="text-xs text-slate-300 leading-normal max-w-xl">
                    {pendingProposal.proposal_text}
                  </p>
                </div>
              </div>

              <div className="flex gap-3 w-full sm:w-auto font-mono text-xs">
                <button
                  type="button"
                  disabled={actionLoading}
                  onClick={() => handleApprovalAction(true)}
                  className="flex-1 sm:flex-initial bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 text-white font-bold py-2.5 px-5 rounded-xl transition-all cursor-pointer shadow-lg shadow-emerald-700/20"
                >
                  {actionLoading ? 'Approving...' : 'APPROVE ORDER'}
                </button>
                <button
                  type="button"
                  disabled={actionLoading}
                  onClick={() => handleApprovalAction(false)}
                  className="flex-1 sm:flex-initial bg-rose-600 hover:bg-rose-500 disabled:opacity-50 text-white font-bold py-2.5 px-5 rounded-xl transition-all cursor-pointer shadow-lg shadow-rose-700/20"
                >
                  {actionLoading ? 'Denying...' : 'DENY ORDER'}
                </button>
              </div>
            </div>
          )}

          <div className="flex justify-between items-center">
            <h2 className="text-lg font-bold flex items-center gap-2">
              <Server className="w-5 h-5 text-[var(--primary)] animate-pulse" />
              Real-Time Signal Proposals
            </h2>
            <span className="text-xs font-mono text-[var(--text-muted)]">
              Showing latest {visibleProposals.length} events
            </span>
          </div>

          {/* Action filter (BUY/SELL/All) — always available since both actions can
              appear regardless of persona count. */}
          <div className="flex items-center gap-2 flex-wrap">
            <span className="text-[10px] uppercase font-mono text-[var(--text-muted)] mr-1 flex items-center gap-1">
              <ArrowUpDown className="w-3 h-3" /> Action
            </span>
            {(['all', 'BUY', 'SELL'] as const).map(a => {
              const active = actionFilter === a;
              const label = a === 'all' ? 'All' : a;
              return (
                <button
                  key={a}
                  type="button"
                  onClick={() => setActionFilter(a)}
                  className={`px-3 py-1 rounded-full text-xs font-mono border transition-colors cursor-pointer
                    ${active
                      ? a === 'BUY'
                        ? 'bg-[var(--glow-green)] border-[var(--accent-green)]/50 text-[var(--accent-green)] font-bold'
                        : a === 'SELL'
                          ? 'bg-[var(--glow-red)] border-[var(--accent-red)]/50 text-[var(--accent-red)] font-bold'
                          : 'bg-[var(--primary)]/15 border-[var(--primary)]/50 text-[var(--primary)] font-bold'
                      : 'bg-white/5 border-white/10 text-[var(--text-muted)] hover:text-[var(--text-main)] hover:border-white/20'
                    }`}
                >
                  {label}
                </button>
              );
            })}
          </div>

          {/* Persona filter — only shown once the buffer holds >1 persona. */}
          {personasInStream.length > 1 && (
            <div className="flex items-center gap-2 flex-wrap">
              <span className="text-[10px] uppercase font-mono text-[var(--text-muted)] mr-1 flex items-center gap-1">
                <User className="w-3 h-3" /> Persona
              </span>
              {['all', ...personasInStream].map(pid => {
                const active = personaFilter === pid;
                const label = pid === 'all' ? 'All' : nameFor(pid);
                return (
                  <button
                    key={pid}
                    type="button"
                    onClick={() => setPersonaFilter(pid)}
                    className={`px-3 py-1 rounded-full text-xs font-mono border transition-colors cursor-pointer
                      ${active
                        ? 'bg-[var(--primary)]/15 border-[var(--primary)]/50 text-[var(--primary)] font-bold'
                        : 'bg-white/5 border-white/10 text-[var(--text-muted)] hover:text-[var(--text-main)] hover:border-white/20'
                      }`}
                  >
                    {label}
                  </button>
                );
              })}
            </div>
          )}

          <div className="flex flex-col gap-6">
            {visibleProposals.length === 0 ? (
              <div className="text-center py-16 border-2 border-dashed border-[var(--border-color)] rounded-2xl bg-white/5 flex flex-col items-center gap-3">
                <Clock className="w-10 h-10 text-[var(--text-muted)] animate-spin-slow" />
                <p className="text-sm text-[var(--text-muted)]">
                  {proposals.length === 0
                    ? 'Awaiting ingestion events from Kahuna Kafka pipeline...'
                    : personaFilter === 'all' && actionFilter === 'all'
                      ? 'No proposals in the current buffer.'
                      : `No ${actionFilter === 'all' ? '' : `${actionFilter} `}proposals${personaFilter === 'all' ? '' : ` from ${nameFor(personaFilter)}`} in the current buffer.`}
                </p>
              </div>
            ) : (
              visibleProposals.map((p, idx) => {
                const isBuy = p.action.toUpperCase() === "BUY";
                const isBreached = p.limit_breached;
                const formattedTime = new Date(p.timestamp).toLocaleTimeString([], { hour12: false }) + '.' + 
                  String(new Date(p.timestamp).getMilliseconds()).padStart(3, '0');
                
                let sourceDotColor = 'bg-slate-400';
                const srcLower = (p.source_id || '').toLowerCase();
                if (srcLower.includes('reuters')) sourceDotColor = 'bg-amber-500';
                else if (srcLower.includes('twitter')) sourceDotColor = 'bg-sky-400';
                else if (srcLower.includes('kalshi')) sourceDotColor = 'bg-blue-500';
                else if (srcLower.includes('polymarket')) sourceDotColor = 'bg-purple-500';

                return (
                  <div 
                    key={p.event_hash + idx}
                    className={`bg-[var(--card-bg)] border rounded-2xl p-6 shadow-md transition-all duration-200 backdrop-blur-md relative overflow-hidden flex flex-col gap-4
                      ${isBreached 
                        ? 'border-[var(--accent-red)]/40 shadow-[var(--accent-red)]/5 hover:border-[var(--accent-red)]' 
                        : 'border-[var(--border-color)] hover:border-[var(--primary)]/50 hover:shadow-[var(--primary)]/5'
                      }`}
                  >
                    {/* Top Action & Status Row */}
                    <div className="flex justify-between items-start gap-4 flex-wrap">
                      <div className="flex items-center gap-3 flex-wrap">
                        <span className={`px-3 py-1.5 rounded-lg text-xs font-bold font-mono tracking-wider border 
                          ${isBuy 
                            ? 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]' 
                            : 'bg-[var(--glow-red)] border-[var(--accent-red)]/30 text-[var(--accent-red)]'
                          }`}
                        >
                          {p.action}
                        </span>
                        
                        <span className="font-mono text-sm font-bold bg-white/5 border border-white/10 px-3 py-1.5 rounded-lg text-indigo-200">
                          {p.contract_id}
                        </span>

                        {p.persona && (
                          <span className={`inline-flex items-center gap-1.5 text-xs font-mono px-3 py-1.5 rounded-lg border ${personaBadgeClasses(p.persona)}`}>
                            <User className="w-3 h-3" />
                            {nameFor(p.persona)}
                          </span>
                        )}

                        {p.source_id && (
                          p.source_url ? (
                            <a 
                              href={p.source_url} 
                              target="_blank" 
                              rel="noreferrer"
                              className="inline-flex items-center gap-1.5 text-xs bg-white/5 hover:bg-white/10 border border-white/10 px-3 py-1.5 rounded-lg font-mono transition-colors text-[var(--text-main)]"
                            >
                              <span className={`w-1.5 h-1.5 rounded-full ${sourceDotColor}`} />
                              {p.source_id}
                              <ExternalLink className="w-3 h-3 text-[var(--text-muted)]" />
                            </a>
                          ) : (
                            <span className="inline-flex items-center gap-1.5 text-xs bg-white/5 border border-white/10 px-3 py-1.5 rounded-lg font-mono text-[var(--text-main)]">
                              <span className={`w-1.5 h-1.5 rounded-full ${sourceDotColor}`} />
                              {p.source_id}
                            </span>
                          )
                        )}
                      </div>

                      <span className={`px-3 py-1 rounded-md text-[10px] font-bold font-mono uppercase tracking-wider border
                        ${isBreached 
                          ? 'bg-[var(--glow-red)] border-[var(--accent-red)]/30 text-[var(--accent-red)]' 
                          : 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                        }`}
                      >
                        {isBreached ? '⚠️ LIMIT BREACHED' : '✓ RISK OK'}
                      </span>
                    </div>

                    {/* Numeric Statistics Grid */}
                    <div className="grid grid-cols-2 sm:grid-cols-4 gap-4 bg-white/2 border border-white/5 p-4 rounded-xl font-mono text-xs">
                      <div className="flex flex-col gap-1">
                        <span className="text-[10px] uppercase text-[var(--text-muted)]">Total Size</span>
                        <span className="font-bold text-[var(--text-main)]">{p.total_size.toLocaleString()} units</span>
                      </div>
                      <div className="flex flex-col gap-1">
                        <span className="text-[10px] uppercase text-[var(--text-muted)]">Avg price</span>
                        <span className="font-bold text-[var(--text-main)]">${p.average_price.toFixed(4)}</span>
                      </div>
                      <div className="flex flex-col gap-1">
                        <GlossaryTooltip termKey="Slippage" className="text-[10px] uppercase text-[var(--text-muted)] font-semibold">
                          Slippage
                        </GlossaryTooltip>
                        <span className={`font-bold ${p.slippage_pct > maxSlippage / 100 ? 'text-[var(--accent-red)]' : p.slippage_pct > (maxSlippage * 0.75) / 100 ? 'text-amber-500' : 'text-[var(--accent-green)]'}`}>
                          {(p.slippage_pct * 100).toFixed(4)}%
                        </span>
                      </div>
                      <div className="flex flex-col gap-1">
                        <span className="text-[10px] uppercase text-[var(--text-muted)]">Latency</span>
                        <span className="font-bold text-[var(--primary)]">{p.processing_time}</span>
                      </div>
                    </div>

                    {/* Trading Rationale Block */}
                    <div className="bg-[var(--primary)]/[0.04] border-l-2 border-[var(--primary)] px-4 py-3 rounded-r-xl text-sm leading-relaxed text-slate-200">
                      <strong className="text-xs uppercase text-[var(--primary)] block mb-1 font-semibold">
                        Trading Rationale
                      </strong>
                      {p.proposal_text}
                      {isBreached && p.breach_reason && (
                        <div className="mt-2 text-xs font-semibold text-[var(--accent-red)] flex items-center gap-1 border-t border-red-500/10 pt-1.5">
                          <AlertTriangle className="w-3.5 h-3.5" />
                          Ledger Audit Breach Reason: {p.breach_reason}
                        </div>
                      )}
                    </div>

                    {/* Slice Boundaries Display (if available) */}
                    {p.slice_boundaries && p.slice_boundaries.length > 0 && (
                      <div className="flex items-center gap-2 flex-wrap font-mono text-[10px] text-[var(--text-muted)] bg-white/2 border border-white/5 px-3 py-2 rounded-lg">
                        <span className="font-semibold uppercase text-slate-400">Slice Boundaries:</span>
                        {p.slice_boundaries.map((val, idx) => (
                          <React.Fragment key={idx}>
                            <span className="bg-white/5 border border-white/10 px-2 py-0.5 rounded text-slate-100 font-bold">
                              {val}
                            </span>
                            {idx < (p.slice_boundaries?.length || 0) - 1 && (
                              <ChevronRight className="w-3 h-3 text-slate-600" />
                            )}
                          </React.Fragment>
                        ))}
                      </div>
                    )}

                    {/* Iceberg Orders execution schedule */}
                    {p.iceberg_orders && p.iceberg_orders.length > 0 && (
                      <div className="flex flex-col gap-2 font-mono text-[10px] text-[var(--text-muted)] border-t border-white/5 pt-3">
                        <div className="flex justify-between items-center">
                          <GlossaryTooltip termKey="Iceberg Execution Timeline" className="font-semibold uppercase tracking-wider text-slate-400">
                            Iceberg Execution Timeline ({p.iceberg_orders.length} chunks)
                          </GlossaryTooltip>
                          <span className="text-[var(--text-muted)]">
                            Execution delay: {Math.max(...p.iceberg_orders.map(o => o.delay_ms))} ms
                          </span>
                        </div>
                        
                        {/* Timeline Bar */}
                        <div className="h-4 bg-white/5 border border-white/10 rounded-full relative flex items-center px-1">
                          {p.iceberg_orders.map((o, idx) => {
                            const maxDelay = Math.max(...(p.iceberg_orders?.map(x => x.delay_ms) || [1]));
                            const leftPct = maxDelay > 0 ? (o.delay_ms / maxDelay) * 94 : 0;
                            return (
                              <div 
                                key={idx}
                                style={{ left: `calc(${leftPct}% + 4px)` }}
                                className="absolute w-2 h-2 rounded-full bg-[var(--primary)] hover:scale-125 transition-transform cursor-pointer shadow-[0_0_5px_var(--primary)]"
                                title={`Chunk Size: ${o.size} / Delay: +${o.delay_ms}ms`}
                              />
                            );
                          })}
                        </div>
                      </div>
                    )}

                    {/* Interactive Action Buttons — hidden in unsupervised (auto) mode,
                        where breen auto-executes proposals without approval */}
                    {isUnsupervised ? null : pendingProposal && pendingProposal.event_hash === p.event_hash ? (
                      <div className="flex gap-3 w-full border-t border-white/5 pt-3 mt-2 font-mono text-xs">
                        <button
                          type="button"
                          disabled={actionLoading}
                          onClick={() => handleApprovalAction(true)}
                          className="flex-1 bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 text-white font-bold py-2 px-4 rounded-xl transition-all cursor-pointer shadow-lg shadow-emerald-700/20 flex items-center justify-center gap-1.5"
                        >
                          APPROVE ORDER
                        </button>
                        <button
                          type="button"
                          disabled={actionLoading}
                          onClick={() => handleApprovalAction(false)}
                          className="flex-1 bg-rose-600 hover:bg-rose-500 disabled:opacity-50 text-white font-bold py-2 px-4 rounded-xl transition-all cursor-pointer shadow-lg shadow-rose-700/20 flex items-center justify-center gap-1.5"
                        >
                          DENY ORDER
                        </button>
                      </div>
                    ) : !pendingProposal && !isBreached ? (
                      <div className="flex gap-3 w-full border-t border-white/5 pt-3 mt-2 font-mono text-xs">
                        <button
                          type="button"
                          onClick={async () => {
                            setPendingProposal(p);
                            if (connectionStatus !== 'OFFLINE') {
                              try {
                                await fetch('/api/proposals/pending', {
                                  method: 'POST',
                                  headers: {
                                    'Content-Type': 'application/json'
                                  },
                                  body: JSON.stringify(p)
                                });
                              } catch (err) {
                                console.error("Failed to post pending proposal to server", err);
                              }
                            }
                          }}
                          className="w-full bg-amber-500/10 hover:bg-amber-500/15 border border-amber-500/35 text-amber-300 font-bold py-2 px-4 rounded-xl transition-all cursor-pointer flex items-center justify-center gap-1.5"
                        >
                          ⚡ Action/Approve Proposal
                        </button>
                      </div>
                    ) : null}

                    {/* Footer Row (Timestamp and Event Hash) */}
                    <div className="flex justify-between items-center text-[9px] font-mono text-[var(--text-muted)] border-t border-white/5 pt-3 mt-2">
                      <span>TIMESTAMP: {formattedTime}</span>
                      <span className="select-all">EVENT HASH: {p.event_hash.substring(0, 16).toUpperCase()}...</span>
                    </div>

                  </div>
                );
              })
            )}
          </div>
        </div>

      </main>
    </div>
  );
};

export default Dashboard;
