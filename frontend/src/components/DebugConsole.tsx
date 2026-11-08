import { useState, useEffect, useRef, useCallback } from 'react';
import { Terminal, Play, Pause, Trash2, ChevronsDown } from 'lucide-react';

interface LogLine {
  service: string;
  level: string;
  ts: string;
  msg: string;
  raw: string;
}

interface DisplayLine extends LogLine {
  id: string;
  count: number;
}

const MAX_BYTES = 3 * 1024 * 1024;

// Stable colour per service name, expressed as a CSS color string.
const PALETTE = [
  '#7dd3fc', // sky-300
  '#6ee7b7', // emerald-300
  '#c4b5fd', // violet-300
  '#fcd34d', // amber-300
  '#f9a8d4', // pink-300
  '#5eead4', // teal-300
  '#fdba74', // orange-300
  '#bef264', // lime-300
];
function serviceHue(service: string): string {
  let h = 0;
  for (const c of service) h = (Math.imul(h, 31) + c.charCodeAt(0)) | 0;
  return PALETTE[Math.abs(h) % PALETTE.length];
}

// 3-char level abbreviation + colour.
const LEVEL_META: Record<string, { abbr: string; color: string }> = {
  ERROR:      { abbr: 'ERR', color: '#f87171' },
  WARN:       { abbr: 'WRN', color: '#fbbf24' },
  WARNING:    { abbr: 'WRN', color: '#fbbf24' },
  INFO:       { abbr: 'INF', color: '#60a5fa' },
  EXECUTE:    { abbr: 'EXE', color: '#34d399' },
  'DRY RUN':  { abbr: 'DRY', color: '#a78bfa' },
  SETTLEMENT: { abbr: 'STL', color: '#22d3ee' },
  PROPOSAL:   { abbr: 'PRO', color: '#818cf8' },
};
function levelMeta(level: string) {
  return LEVEL_META[level] ?? LEVEL_META[level?.toUpperCase()] ?? { abbr: '···', color: '#6b7280' };
}

function shortTime(ts: string): string {
  return ts.length >= 19 ? ts.slice(11) : ts;
}

function estimateBytes(lines: DisplayLine[]): number {
  let n = 0;
  for (const l of lines) n += l.raw.length + 60;
  return n;
}

let seq = 0;

export default function DebugConsole() {
  const [lines, setLines] = useState<DisplayLine[]>([]);
  const [live, setLive] = useState(true);
  const [knownServices, setKnownServices] = useState<string[]>([]);
  const [hiddenServices, setHiddenServices] = useState<Set<string>>(new Set());

  const containerRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const queueRef = useRef<LogLine[]>([]);
  const liveRef = useRef(true);
  liveRef.current = live;

  // Drain queue into state every 100 ms.
  useEffect(() => {
    const id = setInterval(() => {
      if (queueRef.current.length === 0) return;
      const batch = queueRef.current.splice(0);
      setLines(prev => {
        let next = [...prev];
        for (const line of batch) {
          const last = next[next.length - 1];
          if (
            last &&
            last.service === line.service &&
            last.level === line.level &&
            last.msg === line.msg
          ) {
            next[next.length - 1] = { ...last, count: last.count + 1 };
          } else {
            next.push({ ...line, id: `l${seq++}`, count: 1 });
          }
        }
        if (estimateBytes(next) > MAX_BYTES) {
          next = next.slice(Math.floor(next.length * 0.35));
        }
        return next;
      });
      setKnownServices(prev => {
        const next = new Set(prev);
        for (const l of batch) next.add(l.service);
        if (next.size === prev.length) return prev;
        return Array.from(next).sort();
      });
    }, 100);
    return () => clearInterval(id);
  }, []);

  // Initial snapshot.
  useEffect(() => {
    fetch('/api/logs/snapshot')
      .then(r => r.json())
      .then((data: LogLine[]) => { queueRef.current.push(...data); })
      .catch(() => {});
  }, []);

  // SSE stream.
  useEffect(() => {
    const es = new EventSource('/api/logs/stream');
    es.onmessage = (e) => {
      try { queueRef.current.push(JSON.parse(e.data)); } catch {}
    };
    return () => es.close();
  }, []);

  // Auto-scroll when live.
  useEffect(() => {
    if (live) bottomRef.current?.scrollIntoView({ behavior: 'instant' });
  }, [lines, live]);

  const onScroll = useCallback(() => {
    const el = containerRef.current;
    if (!el || !liveRef.current) return;
    if (el.scrollHeight - el.scrollTop - el.clientHeight > 80) setLive(false);
  }, []);

  const jumpToBottom = () => {
    setLive(true);
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  };

  const toggleService = (svc: string) =>
    setHiddenServices(prev => {
      const next = new Set(prev);
      next.has(svc) ? next.delete(svc) : next.add(svc);
      return next;
    });

  const visible = lines.filter(l => !hiddenServices.has(l.service));
  const bytes = estimateBytes(lines);

  return (
    <div
      className="relative flex flex-col bg-[#070b12]"
      style={{ height: 'calc(100vh - 2.75rem)', fontFamily: 'var(--font-mono)', fontSize: '12px' }}
    >
      {/* Toolbar */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-1.5 border-b border-slate-800 shrink-0 bg-[#090e18]">
        <Terminal className="w-3.5 h-3.5 text-[var(--primary)] shrink-0" />
        <span className="font-bold text-slate-300 tracking-widest text-[11px] shrink-0">DEBUG</span>

        <span className="text-slate-600 text-[10px] tabular-nums shrink-0">
          {visible.length.toLocaleString()} lines
          {bytes > 512 * 1024 ? ` · ${(bytes / 1024 / 1024).toFixed(1)} MB` : ''}
        </span>

        {/* Service toggles — single scrollable row, never wraps */}
        <div className="flex gap-x-3 text-[10px] overflow-x-auto shrink" style={{ scrollbarWidth: 'none' }}>
          {knownServices.map(svc => (
            <button
              key={svc}
              onClick={() => toggleService(svc)}
              className="cursor-pointer whitespace-nowrap transition-opacity hover:opacity-70 shrink-0"
              style={{
                color: hiddenServices.has(svc) ? '#374151' : serviceHue(svc),
                textDecoration: hiddenServices.has(svc) ? 'line-through' : 'none',
              }}
            >
              {svc}
            </button>
          ))}
        </div>

        <div className="ml-auto flex items-center gap-1.5 shrink-0">
          <button
            onClick={() => live ? setLive(false) : jumpToBottom()}
            className="flex items-center gap-1 px-2 py-0.5 rounded text-[10px] font-bold border transition-colors cursor-pointer"
            style={live
              ? { background: 'rgba(52,211,153,0.12)', borderColor: 'rgba(52,211,153,0.35)', color: '#34d399' }
              : { background: 'rgba(51,65,85,0.4)', borderColor: '#475569', color: '#94a3b8' }
            }
          >
            {live ? <Play className="w-2.5 h-2.5 fill-current" /> : <Pause className="w-2.5 h-2.5" />}
            {live ? 'LIVE' : 'PAUSED'}
          </button>
          <button
            onClick={() => setLines([])}
            className="flex items-center gap-1 px-2 py-0.5 rounded text-[10px] font-bold border border-slate-700 text-slate-500 hover:border-red-700 hover:text-red-400 transition-colors cursor-pointer"
          >
            <Trash2 className="w-2.5 h-2.5" />
            CLEAR
          </button>
        </div>
      </div>

      {/* Log lines */}
      <div
        ref={containerRef}
        onScroll={onScroll}
        className="flex-1 overflow-y-auto overflow-x-auto px-2 py-1"
        style={{ scrollbarWidth: 'thin', scrollbarColor: '#1e293b transparent' }}
      >
        {visible.length === 0 ? (
          <div className="flex items-center justify-center h-24 text-slate-700 text-[11px]">
            {lines.length === 0 ? 'Waiting for logs…' : 'All services hidden'}
          </div>
        ) : (
          visible.map(line => {
            const { abbr, color: lvlColor } = levelMeta(line.level);
            const svcColor = serviceHue(line.service);
            const isError = line.level === 'ERROR';
            return (
              <div
                key={line.id}
                className="flex gap-2 hover:bg-white/[0.03] leading-5 rounded-sm"
                style={isError ? { background: 'rgba(127,29,29,0.15)' } : undefined}
              >
                {/* Time */}
                <span className="shrink-0 text-slate-600 select-none w-[4.5rem] text-right">
                  {shortTime(line.ts)}
                </span>

                {/* Service — colored, truncated with ellipsis */}
                <span
                  className="shrink-0 w-[9rem] truncate"
                  title={line.service}
                  style={{ color: svcColor }}
                >
                  {line.service}
                </span>

                {/* Level — 3-char fixed */}
                <span
                  className="shrink-0 w-[2.25rem] font-bold"
                  style={{ color: lvlColor }}
                >
                  {line.level ? abbr : '   '}
                </span>

                {/* Message */}
                <span className="text-slate-300 break-all min-w-0">
                  {line.msg || line.raw}
                  {line.count > 1 && (
                    <span className="ml-2 text-slate-600 tabular-nums">×{line.count}</span>
                  )}
                </span>
              </div>
            );
          })
        )}
        <div ref={bottomRef} className="h-px" />
      </div>

      {/* Jump-to-live FAB */}
      {!live && (
        <button
          onClick={jumpToBottom}
          className="absolute bottom-5 right-5 flex items-center gap-1.5 px-3 py-1.5 rounded-full text-[11px] font-bold text-white shadow-xl cursor-pointer"
          style={{ background: 'var(--primary)' }}
        >
          <ChevronsDown className="w-3.5 h-3.5" />
          Jump to live
        </button>
      )}
    </div>
  );
}
