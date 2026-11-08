import React, { useEffect, useRef, useState } from 'react';
import { Activity } from 'lucide-react';
import GlossaryTooltip from './GlossaryTooltip';

import type { Theme } from '../App';

interface LatencyPoint {
  embed: number;
  qdrant: number;
  llm: number;
}

// Only the proposal fields the telemetry pane reads off the SSE stream.
interface TelProposal {
  limit_breached?: boolean;
  model_used?: string;
  embed_latency_ms?: number;
  qdrant_latency_ms?: number;
  llm_latency_ms?: number;
}

const seriesColor = (theme: Theme, series: 'embed' | 'qdrant' | 'llm'): string => {
  switch (theme) {
    case 'synth84':
      return series === 'embed' ? '#ff007f' : series === 'qdrant' ? '#00ffff' : '#9d4edd';
    case 'bladerunner':
      return series === 'embed' ? '#ffb000' : series === 'qdrant' ? '#b8860b' : '#ffd700';
    case 'royal':
      return series === 'embed' ? '#d4af37' : series === 'qdrant' ? '#2ecc71' : '#3498db';
    default:
      return series === 'embed' ? '#fb7185' : series === 'qdrant' ? '#60a5fa' : '#a78bfa';
  }
};

// SystemTelemetry is the live RAG/inference telemetry pane (moved here from the
// Strategy Stream). It subscribes to /api/stream and derives metrics from the real
// proposal payloads gordon emits (latency breakdown, ingestion rate, breach rate).
const SystemTelemetry: React.FC<{ theme: Theme }> = ({ theme }) => {
  const [latencyHistory, setLatencyHistory] = useState<LatencyPoint[]>([]);
  const [totProposals, setTotProposals] = useState(0);
  const [breachedProposals, setBreachedProposals] = useState(0);
  const [eps, setEps] = useState(0);
  const [bandwidth, setBandwidth] = useState(0);
  const [activeReasoner, setActiveReasoner] = useState('—');
  const [connectionStatus, setConnectionStatus] = useState<'CONNECTING' | 'LIVE' | 'OFFLINE'>('CONNECTING');
  const eventTimesRef = useRef<number[]>([]);
  const eventBytesRef = useRef<number>(0);

  useEffect(() => {
    setConnectionStatus('CONNECTING');
    const sse = new EventSource('/api/stream');
    sse.onopen = () => setConnectionStatus('LIVE');
    sse.onerror = () => setConnectionStatus('OFFLINE');
    sse.onmessage = (event) => {
      try {
        const p: TelProposal = JSON.parse(event.data);
        setTotProposals(prev => prev + 1);
        if (p.limit_breached) setBreachedProposals(prev => prev + 1);
        if (p.model_used) setActiveReasoner(p.model_used);
        if (p.embed_latency_ms || p.qdrant_latency_ms || p.llm_latency_ms) {
          setLatencyHistory(prev =>
            [...prev, { embed: p.embed_latency_ms || 0, qdrant: p.qdrant_latency_ms || 0, llm: p.llm_latency_ms || 0 }].slice(-20),
          );
        }
        eventTimesRef.current.push(Date.now());
        eventBytesRef.current += new Blob([event.data]).size;
      } catch {
        /* ignore malformed payloads */
      }
    };
    return () => sse.close();
  }, []);

  useEffect(() => {
    const t = setInterval(() => {
      const now = Date.now();
      eventTimesRef.current = eventTimesRef.current.filter(x => now - x < 10000);
      setEps(eventTimesRef.current.length / 10);
      setBandwidth((eventBytesRef.current / 1024) / 10);
      eventBytesRef.current = 0;
    }, 1000);
    return () => clearInterval(t);
  }, []);

  const okRate = totProposals > 0
    ? (((totProposals - breachedProposals) / totProposals) * 100).toFixed(1) + '%'
    : '100%';
  const last = latencyHistory[latencyHistory.length - 1];

  const renderLatencyChart = () => {
    if (latencyHistory.length === 0) {
      return <span className="text-[10px] text-[var(--text-muted)] font-mono italic">Awaiting proposals…</span>;
    }
    const width = 310, height = 150, padding = 20;
    let maxVal = 100;
    latencyHistory.forEach(pt => {
      const m = Math.max(pt.embed, pt.qdrant, pt.llm);
      if (m > maxVal) maxVal = m;
    });
    maxVal = Math.ceil(maxVal * 1.15);
    const n = latencyHistory.length;
    const getX = (idx: number) => (n > 1 ? padding + (idx / (n - 1)) * (width - padding * 2) : width / 2);
    const getY = (val: number) => height - padding - (val / maxVal) * (height - padding * 2);
    const pathFor = (key: keyof LatencyPoint) =>
      latencyHistory.map((pt, idx) => `${idx === 0 ? 'M' : 'L'} ${getX(idx)} ${getY(pt[key])}`).join(' ');

    return (
      <svg width="100%" height="100%" viewBox={`0 0 ${width} ${height}`} className="overflow-visible font-mono">
        {[0, 0.5, 1].map((ratio, i) => {
          const yVal = height - padding - ratio * (height - padding * 2);
          return (
            <g key={i}>
              <line x1={padding} y1={yVal} x2={width - padding} y2={yVal} className="stroke-white/5" strokeDasharray="3,3" />
              <text x={padding - 5} y={yVal + 3} textAnchor="end" className="fill-[var(--text-muted)] text-[8px]">
                {Math.round(ratio * maxVal)}ms
              </text>
            </g>
          );
        })}
        <path d={pathFor('embed')} fill="none" stroke={seriesColor(theme, 'embed')} strokeWidth="1.5" strokeLinecap="round" />
        <path d={pathFor('qdrant')} fill="none" stroke={seriesColor(theme, 'qdrant')} strokeWidth="1.5" strokeLinecap="round" />
        <path d={pathFor('llm')} fill="none" stroke={seriesColor(theme, 'llm')} strokeWidth="1.8" strokeLinecap="round" />
        <circle cx={getX(n - 1)} cy={getY(latencyHistory[n - 1].embed)} r="3" fill={seriesColor(theme, 'embed')} stroke="#fff" strokeWidth="0.5" />
        <circle cx={getX(n - 1)} cy={getY(latencyHistory[n - 1].qdrant)} r="3" fill={seriesColor(theme, 'qdrant')} stroke="#fff" strokeWidth="0.5" />
        <circle cx={getX(n - 1)} cy={getY(latencyHistory[n - 1].llm)} r="3" fill={seriesColor(theme, 'llm')} stroke="#fff" strokeWidth="0.5" />
      </svg>
    );
  };

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
      <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
        <Activity className="w-4 h-4 text-[var(--primary)]" />
        System Telemetry
        <span className={`ml-auto text-[10px] ${connectionStatus === 'LIVE' ? 'text-[var(--accent-green)]' : connectionStatus === 'OFFLINE' ? 'text-[var(--accent-red)]' : 'text-[var(--text-muted)]'}`}>
          {connectionStatus}
        </span>
      </h3>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-x-8 gap-y-3 font-mono text-sm">
        <div className="flex justify-between items-center py-1">
          <span className="text-xs text-[var(--text-muted)]">Proposals Ingested</span>
          <span className="font-bold text-[var(--text-main)]">{totProposals}</span>
        </div>
        <div className="flex justify-between items-center py-1">
          <span className="text-xs text-[var(--text-muted)]">Limits Breached</span>
          <span className="font-bold text-[var(--accent-red)]">{breachedProposals}</span>
        </div>
        <div className="flex justify-between items-center py-1">
          <span className="text-xs text-[var(--text-muted)]">Risk Safety Rate</span>
          <span className="font-bold text-[var(--accent-green)]">{okRate}</span>
        </div>
        <div className="flex justify-between items-center py-1">
          <GlossaryTooltip termKey="Ollama Inference Duration" className="text-xs text-[var(--text-muted)] font-medium">
            Ollama Inference Duration
          </GlossaryTooltip>
          <span className="font-bold text-indigo-400">{last?.llm || 0} ms</span>
        </div>
        <div className="flex justify-between items-center py-1">
          <span className="text-xs text-[var(--text-muted)]">Ingestion Speed</span>
          <span className="font-bold text-[var(--text-main)]">{eps.toFixed(1)} EPS</span>
        </div>
        <div className="flex justify-between items-center py-1">
          <span className="text-xs text-[var(--text-muted)]">Bandwidth Ingest</span>
          <span className="font-bold text-[var(--text-main)]">{bandwidth.toFixed(2)} KB/s</span>
        </div>
      </div>

      <div className="border-t border-white/5 pt-3">
        <span className="text-xs text-[var(--text-muted)] block mb-3 font-semibold uppercase tracking-wider">
          Inference Latency Breakdown
        </span>
        <div className="h-[150px] w-full flex items-center justify-center">{renderLatencyChart()}</div>
        <div className="flex flex-col gap-1.5 text-[10px] mt-4 border-t border-white/5 pt-2 text-[var(--text-muted)]">
          <div className="flex justify-between items-center">
            <span className="flex items-center gap-1.5"><span className="w-2 h-2 rounded-full" style={{ backgroundColor: seriesColor(theme, 'embed') }} />Embedding (nomic-embed-text)</span>
            <span className="font-bold text-[var(--text-main)]">{last?.embed || 0} ms</span>
          </div>
          <div className="flex justify-between items-center">
            <span className="flex items-center gap-1.5"><span className="w-2 h-2 rounded-full" style={{ backgroundColor: seriesColor(theme, 'qdrant') }} />Qdrant Vector Retrieval</span>
            <span className="font-bold text-[var(--text-main)]">{last?.qdrant || 0} ms</span>
          </div>
          <div className="flex justify-between items-center">
            <span className="flex items-center gap-1.5"><span className="w-2 h-2 rounded-full" style={{ backgroundColor: seriesColor(theme, 'llm') }} />Ollama Reasoner ({activeReasoner})</span>
            <span className="font-bold text-[var(--text-main)]">{last?.llm || 0} ms</span>
          </div>
        </div>
      </div>
    </section>
  );
};

export default SystemTelemetry;
