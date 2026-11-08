import React, { useState, useRef, useEffect } from 'react';
import { MessageSquare, Send, Bot, User, AlertTriangle } from 'lucide-react';

interface ChatMessage {
  role: 'user' | 'assistant';
  content: string;
}

// Build a compact snapshot of the live paper-trading run so the assistant can
// answer questions about "my portfolio". Fetches the same endpoints the
// Portfolio Analytics tab uses; resilient if the backend is unreachable.
async function buildPortfolioContext(): Promise<unknown> {
  try {
    const [accRes, trRes] = await Promise.all([fetch('/api/account'), fetch('/api/trades')]);
    const account = accRes.ok ? await accRes.json() : null;
    const trades = trRes.ok ? await trRes.json() : [];
    const recent = Array.isArray(trades)
      ? trades.slice(-10).map((t: any) => ({
          contract: t.contract_id, exchange: t.exchange, side: t.side,
          entry: t.entry_price, exit: t.exit_price, pnl: t.realized_pnl, return_pct: t.return_pct,
        }))
      : [];
    return { account, total_closed_trades: Array.isArray(trades) ? trades.length : 0, recent_trades: recent };
  } catch {
    return {};
  }
}

const STARTER_PROMPTS = [
  "What's my win rate and total PnL, and is that good?",
  'Explain my maximum drawdown in plain terms.',
  'Which exchange am I most exposed to?',
  "What does Gekko's breen do?",
];

export const AssistantChat: React.FC = () => {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: 'smooth' });
  }, [messages, loading]);

  const send = async (text: string) => {
    const trimmed = text.trim();
    if (!trimmed || loading) return;

    const nextMessages: ChatMessage[] = [...messages, { role: 'user', content: trimmed }];
    setMessages(nextMessages);
    setInput('');
    setLoading(true);
    setError(null);

    try {
      const portfolioContext = await buildPortfolioContext();
      const res = await fetch('/api/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: nextMessages,
          portfolio_context: portfolioContext,
        }),
      });
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data?.error || `HTTP ${res.status}`);
      }
      setMessages(prev => [...prev, { role: 'assistant', content: data.reply }]);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'unknown error');
    } finally {
      setLoading(false);
    }
  };

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    send(input);
  };

  return (
    <div className="max-w-[900px] mx-auto mt-8 px-6 pb-8 flex flex-col gap-4 h-[calc(100vh-8rem)]">
      <div className="flex items-center gap-2 shrink-0">
        <MessageSquare className="w-5 h-5 text-[var(--primary)]" />
        <h2 className="text-lg font-bold">Ask Gekko</h2>
        <span className="text-[10px] font-mono text-[var(--text-muted)] ml-auto">
          Local Ollama · informational only
        </span>
      </div>

      {/* Transcript */}
      <div
        ref={scrollRef}
        className="flex-1 overflow-y-auto flex flex-col gap-4 bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 backdrop-blur-md"
      >
        {messages.length === 0 && !loading && (
          <div className="flex flex-col gap-4 m-auto text-center max-w-md">
            <Bot className="w-10 h-10 text-[var(--primary)] mx-auto opacity-70" />
            <p className="text-sm text-[var(--text-muted)]">
              Ask about your portfolio (win rate, drawdown, allocation) or how Gekko works.
              Answers run on a local model and stay on your machine.
            </p>
            <div className="flex flex-col gap-2">
              {STARTER_PROMPTS.map((p, i) => (
                <button
                  key={i}
                  type="button"
                  onClick={() => send(p)}
                  className="text-xs text-left px-3 py-2 rounded-lg border border-white/10 bg-white/[0.03] hover:border-[var(--primary)]/50 hover:bg-white/[0.06] transition-all cursor-pointer text-[var(--text-main)]"
                >
                  {p}
                </button>
              ))}
            </div>
          </div>
        )}

        {messages.map((m, i) => (
          <div key={i} className={`flex gap-3 ${m.role === 'user' ? 'flex-row-reverse' : ''}`}>
            <div
              className={`w-7 h-7 rounded-full flex items-center justify-center shrink-0 border ${
                m.role === 'user'
                  ? 'bg-[var(--primary)]/15 border-[var(--primary)]/30 text-[var(--primary)]'
                  : 'bg-emerald-500/10 border-emerald-500/30 text-emerald-400'
              }`}
            >
              {m.role === 'user' ? <User className="w-3.5 h-3.5" /> : <Bot className="w-3.5 h-3.5" />}
            </div>
            <div
              className={`max-w-[80%] rounded-2xl px-4 py-2.5 text-sm leading-relaxed whitespace-pre-wrap ${
                m.role === 'user'
                  ? 'bg-[var(--primary)]/15 border border-[var(--primary)]/20 text-[var(--text-main)]'
                  : 'bg-white/[0.04] border border-white/10 text-[var(--text-main)]'
              }`}
            >
              {m.content}
            </div>
          </div>
        ))}

        {loading && (
          <div className="flex gap-3">
            <div className="w-7 h-7 rounded-full flex items-center justify-center shrink-0 border bg-emerald-500/10 border-emerald-500/30 text-emerald-400">
              <Bot className="w-3.5 h-3.5" />
            </div>
            <div className="bg-white/[0.04] border border-white/10 rounded-2xl px-4 py-3 flex items-center gap-1">
              <span className="w-1.5 h-1.5 rounded-full bg-[var(--text-muted)] animate-bounce [animation-delay:-0.3s]" />
              <span className="w-1.5 h-1.5 rounded-full bg-[var(--text-muted)] animate-bounce [animation-delay:-0.15s]" />
              <span className="w-1.5 h-1.5 rounded-full bg-[var(--text-muted)] animate-bounce" />
            </div>
          </div>
        )}
      </div>

      {/* Error banner */}
      {error && (
        <div className="shrink-0 bg-rose-500/10 border border-rose-500/30 rounded-xl p-3 flex items-center gap-2 font-mono text-xs">
          <AlertTriangle className="w-4 h-4 text-rose-400 shrink-0" />
          <span className="text-rose-300">
            Assistant unavailable: {error}. The chat needs the liveview service and a local Ollama
            model running (<code>./run.sh</code> + <code>ollama pull qwen2.5:1.5b</code>).
          </span>
        </div>
      )}

      {/* Composer */}
      <form onSubmit={onSubmit} className="shrink-0 flex gap-2">
        <input
          type="text"
          value={input}
          onChange={e => setInput(e.target.value)}
          placeholder="Ask about your portfolio or how Gekko works..."
          disabled={loading}
          className="flex-1 bg-[var(--card-bg)] border border-[var(--border-color)] rounded-xl px-4 py-3 text-sm outline-none focus:border-[var(--primary)]/60 transition-colors text-[var(--text-main)] placeholder:text-[var(--text-muted)] disabled:opacity-60"
        />
        <button
          type="submit"
          disabled={loading || !input.trim()}
          className="bg-[var(--primary)]/20 hover:bg-[var(--primary)]/30 border border-[var(--primary)]/40 text-[var(--primary)] font-bold px-5 rounded-xl transition-all cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed flex items-center gap-2"
        >
          <Send className="w-4 h-4" />
        </button>
      </form>
    </div>
  );
};

export default AssistantChat;
