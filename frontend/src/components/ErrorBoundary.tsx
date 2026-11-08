import React from 'react';
import { AlertTriangle, RefreshCw } from 'lucide-react';

// Catches render errors in a subtree so one broken panel/tab can't unmount the
// whole app (React removes the entire tree on an uncaught render error).

interface ErrorBoundaryProps {
  /** Shown in the fallback so the user knows which area failed. */
  label: string;
  children: React.ReactNode;
}

interface ErrorBoundaryState {
  error: Error | null;
}

export class ErrorBoundary extends React.Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error(`[ErrorBoundary:${this.props.label}]`, error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="max-w-[1400px] mx-auto mt-8 px-6">
        <section className="bg-[var(--card-bg)] border border-[var(--accent-red)]/40 rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-3 font-mono text-xs">
          <h3 className="text-sm font-bold text-[var(--accent-red)] flex items-center gap-2">
            <AlertTriangle className="w-4 h-4" />
            {this.props.label} crashed
          </h3>
          <p className="text-[var(--text-muted)] break-all">{String(this.state.error)}</p>
          <button
            type="button"
            onClick={() => this.setState({ error: null })}
            className="self-start flex items-center gap-1.5 py-2 px-4 rounded-lg border border-white/10 bg-white/5 hover:bg-white/10 font-bold cursor-pointer text-[var(--text-main)]"
          >
            <RefreshCw className="w-3.5 h-3.5" />
            Retry
          </button>
        </section>
      </div>
    );
  }
}

export default ErrorBoundary;
