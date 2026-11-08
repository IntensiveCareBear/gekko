import React, { useEffect, useState, useCallback } from 'react';
import { Server, RefreshCw, CheckCircle2, XCircle, AlertTriangle, Wallet, Sparkles, Bird, Cloud, Newspaper, Landmark } from 'lucide-react';
import LedgerAudit from './LedgerAudit';
import LouLessons from './LouLessons';
import LouAutonomy from './LouAutonomy';

interface PaperAccount {
  starting_capital: number;
  cash: number;
  nav: number;
  realized_pnl: number;
  unrealized_pnl: number;
  open_positions: number;
  drawdown_pct: number;
}

// ─── Data contract (matches liveview /api/status) ────────────────────────────

interface ComponentStatus {
  component: string;
  reachable: boolean;
  status: string;
  uptime_seconds: number;
  // Array values (gordon's per-persona list) are filtered out of the card —
  // persona reliability lives on the Models tab, performance on Home.
  stats?: Record<string, number | string | boolean | unknown[]>;
  error?: string;
}

interface StatusResponse {
  generated_at: string;
  healthy_count: number;
  component_count: number;
  components: ComponentStatus[];
}

// ─── Formatting helpers ──────────────────────────────────────────────────────

const fmtUSD = (v: number) =>
  new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 }).format(v ?? 0);

function fmtUptime(seconds: number): string {
  if (!seconds || seconds < 0) return '—';
  const s = Math.floor(seconds);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d > 0) return `${d}d ${h}h ${m}m`;
  if (h > 0) return `${h}h ${m}m ${sec}s`;
  if (m > 0) return `${m}m ${sec}s`;
  return `${sec}s`;
}

function prettyKey(key: string): string {
  return key
    .replace(/_/g, ' ')
    .replace(/\b\w/g, c => c.toUpperCase());
}

// Relative "time ago" from a unix-seconds timestamp (0/undefined ⇒ never).
function fmtAgo(unixSeconds?: number): string {
  if (!unixSeconds || unixSeconds <= 0) return 'never';
  const secs = Math.max(0, Math.floor(Date.now() / 1000 - unixSeconds));
  if (secs < 60) return `${secs}s ago`;
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m ago`;
  return `${Math.floor(h / 24)}d ${h % 24}h ago`;
}

// Stat readers over a component's decoded stats map (JSON numbers arrive as
// number; missing keys read as sensible zero-values).
function sNum(stats: ComponentStatus['stats'], k: string): number {
  const v = stats?.[k];
  return typeof v === 'number' ? v : 0;
}
function sStr(stats: ComponentStatus['stats'], k: string): string {
  const v = stats?.[k];
  return typeof v === 'string' ? v : '';
}
function sBool(stats: ComponentStatus['stats'], k: string): boolean {
  return stats?.[k] === true;
}

// Keys the External APIs cards own — hidden from the generic per-component stat
// rows so hosted-backend/Twitter telemetry isn't duplicated on the gordon/kahuna
// cards. 'backend' (bare) is the backend name; 'backend_*' its budget/counters.
const EXTERNAL_API_PREFIXES = ['backend', 'twitter_', 'finnhub_', 'fred_', 'weather_'];

// A labelled budget bar (used / total) whose colour warms as it fills.
const BudgetBar: React.FC<{ used: number; total: number }> = ({ used, total }) => {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  const color = pct >= 90 ? 'var(--accent-red)' : pct >= 70 ? '#f59e0b' : 'var(--accent-green)';
  return (
    <div className="flex flex-col gap-1">
      <div className="flex justify-between items-center text-xs font-mono">
        <span className="text-[var(--text-muted)]">Daily budget</span>
        <span className="font-bold text-[var(--text-main)]">
          {used.toLocaleString()}{total > 0 ? ` / ${total.toLocaleString()}` : ' (unlimited)'}
        </span>
      </div>
      {total > 0 && (
        <div className="h-1.5 rounded-full bg-white/10 overflow-hidden">
          <div className="h-full rounded-full transition-all" style={{ width: `${pct}%`, background: color }} />
        </div>
      )}
    </div>
  );
};

// One key/value row inside an External APIs card.
const APIRow: React.FC<{ label: string; children: React.ReactNode; wrap?: boolean }> = ({ label, children, wrap }) => (
  <div className={`flex justify-between text-xs font-mono ${wrap ? 'items-start gap-4' : 'items-center'}`}>
    <span className="text-[var(--text-muted)] shrink-0">{label}</span>
    <span className={`font-bold text-[var(--text-main)] ${wrap ? 'text-right break-words min-w-0' : ''}`}>{children}</span>
  </div>
);

function fmtVal(key: string, v: number | string | boolean): React.ReactNode {
  if (typeof v === 'boolean') {
    return (
      <span className={`font-bold ${v ? 'text-[var(--accent-green)]' : 'text-[var(--text-muted)]'}`}>
        {v ? 'YES' : 'NO'}
      </span>
    );
  }
  if (typeof v === 'number') {
    if (/unix/i.test(key) && v > 0) {
      return (
        <span className="font-bold text-[var(--text-main)] text-right">
          {new Date(v * 1000).toLocaleString()} <span className="text-[var(--text-muted)]">({fmtAgo(v)})</span>
        </span>
      );
    }
    return <span className="font-bold text-[var(--text-main)]">{v.toLocaleString()}</span>;
  }
  return <span className="font-bold text-[var(--text-main)] text-right break-words min-w-0">{v}</span>;
}

// Human-friendly labels for the component IDs the backend reports.
const COMPONENT_LABELS: Record<string, string> = {
  liveview: 'Liveview (SSE / API Gateway)',
  'broker-worker': 'Broker Worker (Exchange Exec)',
  breen: 'Breen (Order Manager)',
  kahuna: 'Kahuna (Ingestion)',
  gordon: 'Gordon (Strategy Engine)',
  marketdata: 'Marketdata (Kalshi live, read-only)',
};

// ─── Component ───────────────────────────────────────────────────────────────

export const SystemStatus: React.FC = () => {
  const [data, setData] = useState<StatusResponse | null>(null);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const [account, setAccount] = useState<PaperAccount | null>(null);

  const fetchStatus = useCallback(async () => {
    try {
      const res = await fetch('/api/status');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: StatusResponse = await res.json();
      setData(json);
      setFetchError(null);
      setLastUpdated(new Date());
    } catch (err) {
      setFetchError(err instanceof Error ? err.message : 'unknown error');
    }

    // Paper-run account snapshot (one-line summary; full analytics on Home,
    // run management on Controls).
    try {
      const a = await fetch('/api/account');
      if (a.ok) setAccount(await a.json());
    } catch {
      /* keep last-known account */
    }
  }, []);

  useEffect(() => {
    fetchStatus();
    const interval = setInterval(fetchStatus, 3000);
    return () => clearInterval(interval);
  }, [fetchStatus]);

  // Aggregate headline numbers (e.g. events processed) across components.
  const headline = (() => {
    if (!data) return null;
    const get = (comp: string, key: string): number => {
      const c = data.components.find(x => x.component === comp);
      const v = c?.stats?.[key];
      return typeof v === 'number' ? v : 0;
    };
    return {
      eventsProcessed: get('kahuna', 'total_processed'),
      eventsQuarantined: get('kahuna', 'total_quarantined'),
      proposalsGenerated: get('gordon', 'proposals_generated'),
      ordersExecuted: get('breen', 'orders_executed'),
    };
  })();

  // ─── External APIs (hosted proposal backend + X/Twitter ingest) ────────────
  // Both are optional: backend stats appear only during an A/B run with a persona
  // on a hosted backend (e.g. Gemini); Twitter stats only when the live X source
  // is enabled. Cards render just when their host component reports the telemetry.
  const gordonComp = data?.components.find(c => c.component === 'gordon');
  const kahunaComp = data?.components.find(c => c.component === 'kahuna');

  // Human label for a backend id (extend as more hosted backends are added).
  const BACKEND_LABELS: Record<string, string> = { gemini: 'Gemini API' };

  const backendId = gordonComp ? sStr(gordonComp.stats, 'backend') : '';
  const backend = gordonComp && sNum(gordonComp.stats, 'backend_personas') > 0
    ? {
        reachable: gordonComp.reachable,
        name: BACKEND_LABELS[backendId] || backendId || 'Hosted backend',
        model: sStr(gordonComp.stats, 'backend_model') || backendId,
        personas: sNum(gordonComp.stats, 'backend_personas'),
        callsToday: sNum(gordonComp.stats, 'backend_calls_today'),
        maxRpd: sNum(gordonComp.stats, 'backend_max_rpd'),
        ok: sNum(gordonComp.stats, 'backend_primary_ok'),
        err: sNum(gordonComp.stats, 'backend_primary_err'),
        fallbacks: sNum(gordonComp.stats, 'backend_fallbacks'),
        lastOk: sNum(gordonComp.stats, 'backend_last_ok_unix'),
      }
    : null;

  const twitter = kahunaComp && kahunaComp.stats && 'twitter_live' in kahunaComp.stats
    ? {
        reachable: kahunaComp.reachable,
        live: sBool(kahunaComp.stats, 'twitter_live'),
        rateLimited: sBool(kahunaComp.stats, 'twitter_rate_limited'),
        budgetUsed: sNum(kahunaComp.stats, 'twitter_budget_used'),
        dailyBudget: sNum(kahunaComp.stats, 'twitter_daily_budget'),
        tweetsSeen: sNum(kahunaComp.stats, 'twitter_tweets_seen'),
        lastTweet: sNum(kahunaComp.stats, 'twitter_last_tweet_unix'),
        lastPoll: sNum(kahunaComp.stats, 'twitter_last_poll_unix'),
        pollInterval: sStr(kahunaComp.stats, 'twitter_poll_interval'),
        lastError: sStr(kahunaComp.stats, 'twitter_last_error'),
      }
    : null;

  const finnhub = kahunaComp && kahunaComp.stats && 'finnhub_live' in kahunaComp.stats
    ? {
        reachable: kahunaComp.reachable,
        live: sBool(kahunaComp.stats, 'finnhub_live'),
        rateLimited: sBool(kahunaComp.stats, 'finnhub_rate_limited'),
        articlesSeen: sNum(kahunaComp.stats, 'finnhub_articles_seen'),
        lastArticle: sNum(kahunaComp.stats, 'finnhub_last_article_unix'),
        lastPoll: sNum(kahunaComp.stats, 'finnhub_last_poll_unix'),
        pollInterval: sStr(kahunaComp.stats, 'finnhub_poll_interval'),
        categories: sStr(kahunaComp.stats, 'finnhub_categories'),
        maxCallsPerMin: sNum(kahunaComp.stats, 'finnhub_max_calls_per_min'),
        lastError: sStr(kahunaComp.stats, 'finnhub_last_error'),
      }
    : null;

  const fred = kahunaComp && kahunaComp.stats && 'fred_live' in kahunaComp.stats
    ? {
        reachable: kahunaComp.reachable,
        live: sBool(kahunaComp.stats, 'fred_live'),
        releasesSeen: sNum(kahunaComp.stats, 'fred_releases_seen'),
        series: sStr(kahunaComp.stats, 'fred_series'),
        pollInterval: sStr(kahunaComp.stats, 'fred_poll_interval'),
        calendarEvents: sNum(kahunaComp.stats, 'fred_calendar_events'),
        maxCallsPerMin: sNum(kahunaComp.stats, 'fred_max_calls_per_min'),
        lastRelease: sStr(kahunaComp.stats, 'fred_last_release'),
        lastReleaseAt: sNum(kahunaComp.stats, 'fred_last_release_unix'),
        lastPoll: sNum(kahunaComp.stats, 'fred_last_poll_unix'),
        lastError: sStr(kahunaComp.stats, 'fred_last_error'),
        calendarError: sStr(kahunaComp.stats, 'fred_calendar_error'),
      }
    : null;

  const weather = kahunaComp && kahunaComp.stats && 'weather_live' in kahunaComp.stats
    ? {
        reachable: kahunaComp.reachable,
        live: sBool(kahunaComp.stats, 'weather_live'),
        obsSeen: sNum(kahunaComp.stats, 'weather_obs_seen'),
        stations: sStr(kahunaComp.stats, 'weather_stations'),
        pollInterval: sStr(kahunaComp.stats, 'weather_poll_interval'),
        lastObs: sNum(kahunaComp.stats, 'weather_last_obs_unix'),
        lastPoll: sNum(kahunaComp.stats, 'weather_last_poll_unix'),
        lastError: sStr(kahunaComp.stats, 'weather_last_error'),
        // Per-station running local-day highs arrive as individual weather_high_<ICAO> keys.
        highs: Object.entries(kahunaComp.stats)
          .filter(([k, v]) => k.startsWith('weather_high_') && typeof v === 'number')
          .map(([k, v]) => ({ station: k.slice('weather_high_'.length), high: v as number }))
          .sort((a, b) => a.station.localeCompare(b.station)),
      }
    : null;
  const weatherHealthy = !!weather && weather.reachable && weather.live && !weather.lastError;

  // The hosted backend is "healthy" when reachable; degraded when every recent
  // primary call has been failing over to the local model.
  const backendFailingOver = !!backend && backend.err > 0 && backend.ok === 0;
  // Twitter is "healthy" when live, reachable, and not currently rate-limited or
  // erroring.
  const twitterHealthy = !!twitter && twitter.reachable && twitter.live && !twitter.rateLimited && !twitter.lastError;
  const finnhubHealthy = !!finnhub && finnhub.reachable && finnhub.live && !finnhub.rateLimited && !finnhub.lastError;
  // FRED numeric ingest is healthy when live and reachable; the calendar can lag
  // (heavier endpoint) without the source itself being unhealthy.
  const fredHealthy = !!fred && fred.reachable && fred.live && !fred.lastError;

  return (
    <div className="max-w-[1400px] mx-auto mt-8 px-6 pb-16 flex flex-col gap-6">
      <div className="flex justify-between items-center">
        <h2 className="text-lg font-bold flex items-center gap-2">
          <Server className="w-5 h-5 text-[var(--primary)]" />
          System
        </h2>
        <div className="flex items-center gap-3 text-xs font-mono text-[var(--text-muted)]">
          {data && (
            <span
              className={`px-3 py-1 rounded-full font-bold border ${
                data.healthy_count === data.component_count
                  ? 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                  : 'bg-amber-500/10 border-amber-500/30 text-amber-400'
              }`}
            >
              {data.healthy_count}/{data.component_count} HEALTHY
            </span>
          )}
          <button
            type="button"
            onClick={fetchStatus}
            className="flex items-center gap-1.5 px-3 py-1 rounded border border-white/10 hover:border-white/25 transition-colors cursor-pointer"
          >
            <RefreshCw className="w-3 h-3" />
            Refresh
          </button>
        </div>
      </div>

      {/* Backend-unreachable banner */}
      {fetchError && (
        <div className="bg-rose-500/10 border-2 border-rose-500/30 rounded-2xl p-5 flex items-center gap-3">
          <AlertTriangle className="w-6 h-6 text-rose-400 shrink-0" />
          <div className="font-mono text-xs">
            <p className="font-bold text-rose-300">Status backend unreachable ({fetchError}).</p>
            <p className="text-[var(--text-muted)] mt-1">
              The status page reads <code className="text-rose-300">/api/status</code> from the liveview
              service on <code className="text-rose-300">:8082</code>. Start the stack with{' '}
              <code className="text-rose-300">./run.sh</code> to populate live health data.
            </p>
          </div>
        </div>
      )}

      {/* Headline stat cards */}
      {headline && (
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-3">
          {[
            { label: 'Events Processed', value: headline.eventsProcessed, color: 'text-[var(--accent-green)]' },
            { label: 'Events Quarantined', value: headline.eventsQuarantined, color: 'text-[var(--accent-red)]' },
            { label: 'Proposals Generated', value: headline.proposalsGenerated, color: 'text-[var(--primary)]' },
            { label: 'Orders Executed', value: headline.ordersExecuted, color: 'text-[var(--text-main)]' },
          ].map((kpi, i) => (
            <div key={i} className="flex flex-col gap-1 bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl px-5 py-4 backdrop-blur-md">
              <span className="text-[10px] text-[var(--text-muted)] uppercase tracking-wider font-semibold">{kpi.label}</span>
              <span className={`text-2xl font-bold font-mono ${kpi.color}`}>{kpi.value.toLocaleString()}</span>
            </div>
          ))}
        </div>
      )}

      {/* Paper-run one-liner — full analytics live on the Home tab, run
          management (reset/exports) on the Controls tab. */}
      {account && (
        <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl px-5 py-3 shadow-lg backdrop-blur-md flex items-center gap-2 text-xs font-mono">
          <Wallet className="w-4 h-4 text-[var(--primary)] shrink-0" />
          <span className="text-[var(--text-muted)]">
            Paper run: NAV <span className="font-bold text-[var(--text-main)]">{fmtUSD(account.nav)}</span>
            {' '}· cash <span className="font-bold text-[var(--text-main)]">{fmtUSD(account.cash)}</span>
            {' '}· <span className="font-bold text-[var(--text-main)]">{account.open_positions}</span> open positions
            {' '}— analytics on the Home tab, run management on the Controls tab.
          </span>
        </section>
      )}

      {/* External APIs — hosted services the stack depends on (hosted proposal
          backend + X/Twitter ingest). Only render when at least one is active. */}
      {(backend || twitter || finnhub || fred || weather) && (
        <section className="flex flex-col gap-3">
          <h3 className="text-xs font-bold uppercase tracking-wider text-[var(--text-muted)] flex items-center gap-2">
            <Cloud className="w-4 h-4 text-[var(--primary)]" />
            External APIs
          </h3>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
            {/* Hosted proposal backend (e.g. Gemini) */}
            {backend && (
              <div className={`bg-[var(--card-bg)] border rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3 ${
                backend.reachable && !backendFailingOver ? 'border-[var(--border-color)]' : 'border-amber-500/40'
              }`}>
                <div className="flex justify-between items-start gap-2">
                  <div className="flex items-center gap-2">
                    <Sparkles className="w-4 h-4 text-[var(--primary)] shrink-0" />
                    <div className="flex flex-col">
                      <h4 className="text-sm font-bold leading-tight">{backend.name}</h4>
                      <span className="text-[10px] font-mono text-[var(--text-muted)]">{backend.model}</span>
                    </div>
                  </div>
                  <span className={`px-2 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider shrink-0 ${
                    !backend.reachable
                      ? 'bg-[var(--glow-red)] border-rose-500/30 text-rose-400'
                      : backendFailingOver
                        ? 'bg-amber-500/10 border-amber-500/30 text-amber-400'
                        : 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                  }`}>
                    {!backend.reachable ? 'down' : backendFailingOver ? 'failing over' : 'ok'}
                  </span>
                </div>
                <BudgetBar used={backend.callsToday} total={backend.maxRpd} />
                <div className="flex flex-col gap-2 border-t border-white/5 pt-2">
                  <APIRow label="Persona provider">
                    {backend.personas === 1 ? '1 persona' : `${backend.personas} personas`}
                  </APIRow>
                  <APIRow label="Primary calls OK">
                    <span className="text-[var(--accent-green)]">{backend.ok.toLocaleString()}</span>
                  </APIRow>
                  <APIRow label="Errors → fallback">
                    <span className={backend.err > 0 ? 'text-amber-400' : ''}>{backend.err.toLocaleString()}</span>
                    {' '}<span className="text-[var(--text-muted)] font-normal">({backend.fallbacks.toLocaleString()} to local)</span>
                  </APIRow>
                  <APIRow label="Last success">{fmtAgo(backend.lastOk)}</APIRow>
                </div>
              </div>
            )}

            {/* X / Twitter */}
            {twitter && (
              <div className={`bg-[var(--card-bg)] border rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3 ${
                twitterHealthy ? 'border-[var(--border-color)]' : 'border-amber-500/40'
              }`}>
                <div className="flex justify-between items-start gap-2">
                  <div className="flex items-center gap-2">
                    <Bird className="w-4 h-4 text-[var(--primary)] shrink-0" />
                    <div className="flex flex-col">
                      <h4 className="text-sm font-bold leading-tight">X / Twitter Ingest</h4>
                      <span className="text-[10px] font-mono text-[var(--text-muted)]">
                        {twitter.live ? `live · poll ${twitter.pollInterval}` : 'simulated'}
                      </span>
                    </div>
                  </div>
                  <span className={`px-2 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider shrink-0 ${
                    !twitter.reachable
                      ? 'bg-[var(--glow-red)] border-rose-500/30 text-rose-400'
                      : twitter.rateLimited
                        ? 'bg-amber-500/10 border-amber-500/30 text-amber-400'
                        : !twitter.live
                          ? 'bg-white/5 border-white/10 text-[var(--text-muted)]'
                          : 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                  }`}>
                    {!twitter.reachable ? 'down' : twitter.rateLimited ? 'rate limited' : !twitter.live ? 'simulated' : 'ok'}
                  </span>
                </div>
                <BudgetBar used={twitter.budgetUsed} total={twitter.dailyBudget} />
                <div className="flex flex-col gap-2 border-t border-white/5 pt-2">
                  <APIRow label="Tweets ingested">{twitter.tweetsSeen.toLocaleString()}</APIRow>
                  <APIRow label="Last tweet">{fmtAgo(twitter.lastTweet)}</APIRow>
                  <APIRow label="Last poll">{fmtAgo(twitter.lastPoll)}</APIRow>
                  {twitter.lastError ? (
                    <APIRow label="Last error">
                      <span className="text-rose-400 font-normal">{twitter.lastError}</span>
                    </APIRow>
                  ) : (
                    <APIRow label="Status">
                      <span className={twitter.rateLimited ? 'text-amber-400' : 'text-[var(--accent-green)]'}>
                        {twitter.rateLimited ? 'backing off' : 'healthy'}
                      </span>
                    </APIRow>
                  )}
                </div>
              </div>
            )}

            {/* Finnhub market news */}
            {finnhub && (
              <div className={`bg-[var(--card-bg)] border rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3 ${
                finnhubHealthy ? 'border-[var(--border-color)]' : 'border-amber-500/40'
              }`}>
                <div className="flex justify-between items-start gap-2">
                  <div className="flex items-center gap-2">
                    <Newspaper className="w-4 h-4 text-[var(--primary)] shrink-0" />
                    <div className="flex flex-col">
                      <h4 className="text-sm font-bold leading-tight">Finnhub News</h4>
                      <span className="text-[10px] font-mono text-[var(--text-muted)]">
                        {finnhub.live ? `live · poll ${finnhub.pollInterval}` : 'idle'}
                      </span>
                    </div>
                  </div>
                  <span className={`px-2 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider shrink-0 ${
                    !finnhub.reachable
                      ? 'bg-[var(--glow-red)] border-rose-500/30 text-rose-400'
                      : finnhub.rateLimited
                        ? 'bg-amber-500/10 border-amber-500/30 text-amber-400'
                        : !finnhub.live
                          ? 'bg-white/5 border-white/10 text-[var(--text-muted)]'
                          : 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                  }`}>
                    {!finnhub.reachable ? 'down' : finnhub.rateLimited ? 'rate limited' : !finnhub.live ? 'idle' : 'ok'}
                  </span>
                </div>
                <div className="flex flex-col gap-2 border-t border-white/5 pt-2">
                  <APIRow label="Articles ingested">{finnhub.articlesSeen.toLocaleString()}</APIRow>
                  <APIRow label="Categories">{finnhub.categories || '—'}</APIRow>
                  <APIRow label="Rate cap">{finnhub.maxCallsPerMin > 0 ? `≤ ${finnhub.maxCallsPerMin}/min (free: 60/min)` : '—'}</APIRow>
                  <APIRow label="Last article">{fmtAgo(finnhub.lastArticle)}</APIRow>
                  <APIRow label="Last poll">{fmtAgo(finnhub.lastPoll)}</APIRow>
                  {finnhub.lastError ? (
                    <APIRow label="Last error">
                      <span className="text-rose-400 font-normal">{finnhub.lastError}</span>
                    </APIRow>
                  ) : (
                    <APIRow label="Status">
                      <span className={finnhub.rateLimited ? 'text-amber-400' : 'text-[var(--accent-green)]'}>
                        {finnhub.rateLimited ? 'backing off' : 'healthy'}
                      </span>
                    </APIRow>
                  )}
                </div>
              </div>
            )}

            {/* FRED official macro data + econ calendar */}
            {fred && (
              <div className={`bg-[var(--card-bg)] border rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3 ${
                fredHealthy ? 'border-[var(--border-color)]' : 'border-amber-500/40'
              }`}>
                <div className="flex justify-between items-start gap-2">
                  <div className="flex items-center gap-2">
                    <Landmark className="w-4 h-4 text-[var(--primary)] shrink-0" />
                    <div className="flex flex-col">
                      <h4 className="text-sm font-bold leading-tight">FRED Macro Data</h4>
                      <span className="text-[10px] font-mono text-[var(--text-muted)]">
                        {fred.live ? `live · poll ${fred.pollInterval}` : 'idle'}
                      </span>
                    </div>
                  </div>
                  <span className={`px-2 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider shrink-0 ${
                    !fred.reachable
                      ? 'bg-[var(--glow-red)] border-rose-500/30 text-rose-400'
                      : !fred.live
                        ? 'bg-white/5 border-white/10 text-[var(--text-muted)]'
                        : 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                  }`}>
                    {!fred.reachable ? 'down' : !fred.live ? 'idle' : 'ok'}
                  </span>
                </div>
                <div className="flex flex-col gap-2 border-t border-white/5 pt-2">
                  <APIRow label="Series" wrap>{fred.series || '—'}</APIRow>
                  <APIRow label="Prints ingested">{fred.releasesSeen.toLocaleString()}</APIRow>
                  <APIRow label="Calendar events">
                    {fred.calendarError
                      ? <span className="text-amber-400 font-normal">error</span>
                      : fred.calendarEvents.toLocaleString()}
                  </APIRow>
                  <APIRow label="Last release">{fred.lastRelease || '—'}{fred.lastReleaseAt > 0 ? ` · ${fmtAgo(fred.lastReleaseAt)}` : ''}</APIRow>
                  <APIRow label="Last poll">{fmtAgo(fred.lastPoll)}</APIRow>
                  {fred.lastError ? (
                    <APIRow label="Last error">
                      <span className="text-rose-400 font-normal">{fred.lastError}</span>
                    </APIRow>
                  ) : fred.calendarError ? (
                    <APIRow label="Calendar">
                      <span className="text-amber-400 font-normal">{fred.calendarError}</span>
                    </APIRow>
                  ) : (
                    <APIRow label="Status">
                      <span className="text-[var(--accent-green)]">healthy</span>
                    </APIRow>
                  )}
                </div>
              </div>
            )}

            {/* Weather (METAR) numeric ingest for Kalshi high-temp markets */}
            {weather && (
              <div className={`bg-[var(--card-bg)] border rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3 ${
                weatherHealthy ? 'border-[var(--border-color)]' : 'border-amber-500/40'
              }`}>
                <div className="flex justify-between items-start gap-2">
                  <div className="flex items-center gap-2">
                    <Cloud className="w-4 h-4 text-[var(--primary)] shrink-0" />
                    <div className="flex flex-col">
                      <h4 className="text-sm font-bold leading-tight">Weather (METAR)</h4>
                      <span className="text-[10px] font-mono text-[var(--text-muted)]">
                        {weather.live ? `live · poll ${weather.pollInterval}` : 'idle'}
                      </span>
                    </div>
                  </div>
                  <span className={`px-2 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider shrink-0 ${
                    !weather.reachable
                      ? 'bg-[var(--glow-red)] border-rose-500/30 text-rose-400'
                      : !weather.live
                        ? 'bg-white/5 border-white/10 text-[var(--text-muted)]'
                        : 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                  }`}>
                    {!weather.reachable ? 'down' : !weather.live ? 'idle' : 'ok'}
                  </span>
                </div>
                <div className="flex flex-col gap-2 border-t border-white/5 pt-2">
                  <APIRow label="Stations" wrap>{weather.stations || '—'}</APIRow>
                  <APIRow label="Observations">{weather.obsSeen.toLocaleString()}</APIRow>
                  {weather.highs.length > 0 && (
                    <APIRow label="Day highs" wrap>
                      {weather.highs.map(h => `${h.station} ${h.high.toFixed(1)}°F`).join(' · ')}
                    </APIRow>
                  )}
                  <APIRow label="Last obs">{weather.lastObs > 0 ? fmtAgo(weather.lastObs) : '—'}</APIRow>
                  <APIRow label="Last poll">{fmtAgo(weather.lastPoll)}</APIRow>
                  {weather.lastError ? (
                    <APIRow label="Last error">
                      <span className="text-rose-400 font-normal">{weather.lastError}</span>
                    </APIRow>
                  ) : (
                    <APIRow label="Status">
                      <span className="text-[var(--accent-green)]">healthy</span>
                    </APIRow>
                  )}
                </div>
              </div>
            )}
          </div>
        </section>
      )}

      {/* Per-component health cards */}
      <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-5">
        {data?.components.map(comp => {
          const healthy = comp.reachable && comp.status === 'ok';
          return (
            <section
              key={comp.component}
              className={`bg-[var(--card-bg)] border rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4 ${
                healthy ? 'border-[var(--border-color)]' : 'border-rose-500/40'
              }`}
            >
              <div className="flex justify-between items-start gap-2">
                <div className="flex items-center gap-2">
                  {healthy ? (
                    <CheckCircle2 className="w-4 h-4 text-[var(--accent-green)] shrink-0" />
                  ) : (
                    <XCircle className="w-4 h-4 text-rose-400 shrink-0" />
                  )}
                  <h3 className="text-sm font-bold leading-tight">
                    {COMPONENT_LABELS[comp.component] ?? comp.component}
                  </h3>
                </div>
                <span
                  className={`px-2 py-0.5 rounded text-[9px] font-bold border uppercase tracking-wider shrink-0 ${
                    healthy
                      ? 'bg-[var(--glow-green)] border-[var(--accent-green)]/30 text-[var(--accent-green)]'
                      : 'bg-[var(--glow-red)] border-rose-500/30 text-rose-400'
                  }`}
                >
                  {comp.reachable ? comp.status : 'down'}
                </span>
              </div>

              <div className="flex justify-between items-center text-xs font-mono border-b border-white/5 pb-2">
                <span className="text-[var(--text-muted)]">Uptime</span>
                <span className="font-bold text-[var(--text-main)]">{fmtUptime(comp.uptime_seconds)}</span>
              </div>

              {comp.reachable && comp.stats ? (
                <div className="flex flex-col gap-2 font-mono text-xs">
                  {Object.entries(comp.stats)
                    .filter(([k, v]) => (v === null || typeof v !== 'object') && !EXTERNAL_API_PREFIXES.some(p => k.startsWith(p)))
                    .map(([k, v]) => (
                      <div key={k} className="flex justify-between items-start gap-4">
                        <span className="text-[var(--text-muted)] shrink-0">{prettyKey(k)}</span>
                        {fmtVal(k, v as number | string | boolean)}
                      </div>
                    ))}
                </div>
              ) : (
                <p className="text-[10px] font-mono text-rose-400/80 italic">
                  {comp.error || 'No health data — component offline.'}
                </p>
              )}
            </section>
          );
        })}
      </div>

      {/* Lou's lesson book + auto-tune audit trail (read-only) */}
      <LouAutonomy />
      <LouLessons />

      {/* Cryptographic ledger audit (moved here from the Strategy Stream's Pane D) */}
      <LedgerAudit />

      {lastUpdated && (
        <p className="text-[10px] font-mono text-[var(--text-muted)] text-right">
          Last updated {lastUpdated.toLocaleTimeString()} · auto-refresh 3s
        </p>
      )}

    </div>
  );
};

export default SystemStatus;
