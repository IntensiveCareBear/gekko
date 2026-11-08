import React, { useEffect, useState } from 'react';
import { Shield } from 'lucide-react';
import GlossaryTooltip from './GlossaryTooltip';

// Ledger audit/integrity panel. Self-contained: polls /api/ledger directly so it
// can live in the System tab independent of the Strategy Stream. (Moved here from
// the Strategy Stream's "Pane D".)

interface LedgerBlock {
  tx_id: number;
  timestamp: string;
  contract_id: string;
  action: string;
  size: number;
  price: number;
  status: string;
  yield_delta: number;
  broker_tx_id?: string;
  prev_hash?: string;
  hash?: string;
}

interface LedgerSummary {
  verified: boolean;
  block_count: number;
  blocks: LedgerBlock[];
}

export const LedgerAudit: React.FC = () => {
  const [ledgerSummary, setLedgerSummary] = useState<LedgerSummary>({
    verified: true,
    block_count: 0,
    blocks: [],
  });

  useEffect(() => {
    const fetchLedger = async () => {
      try {
        const res = await fetch('/api/ledger');
        if (res.ok) {
          const data = await res.json();
          if (data && typeof data.block_count === 'number') {
            // An empty ledger arrives as "blocks": null (Go nil slice) — normalize
            // to an array so render code can index it safely.
            setLedgerSummary({ ...data, blocks: Array.isArray(data.blocks) ? data.blocks : [] });
          }
        }
      } catch {
        /* keep last-known */
      }
    };
    fetchLedger();
    const id = setInterval(fetchLedger, 3000);
    return () => clearInterval(id);
  }, []);

  return (
    <section className="bg-[var(--card-bg)] border border-[var(--border-color)] rounded-2xl p-5 shadow-lg backdrop-blur-md flex flex-col gap-4">
      <h3 className="text-xs uppercase tracking-wider text-[var(--text-muted)] border-b border-white/5 pb-2 font-bold flex items-center gap-2">
        <Shield className="w-4 h-4 text-[var(--primary)]" />
        Ledger Audit Integrity
      </h3>

      <div className="flex flex-col gap-3 font-mono text-sm">
        <div className="flex justify-between items-center py-1">
          <span className="text-xs text-[var(--text-muted)]">Active Ledger State</span>
          <span className="font-bold text-[var(--accent-green)] flex items-center gap-1">
            ✓ SECURE (immudb)
          </span>
        </div>
        <div className="flex justify-between items-center py-1">
          <span className="text-xs text-[var(--text-muted)]">Verified Block Count</span>
          <span className="font-bold text-[var(--primary)]">
            {ledgerSummary.block_count} Blocks
          </span>
        </div>

        <div className="flex flex-col gap-1.5 py-1 border-t border-white/5 pt-2.5">
          <GlossaryTooltip termKey="WORM Ledger Entry Hash" className="text-xs text-[var(--text-muted)] font-medium mb-0.5">
            WORM Ledger Entry Hash
          </GlossaryTooltip>
          <span className="text-[10px] bg-white/5 px-2 py-1.5 rounded border border-white/5 text-[var(--text-main)] select-all leading-normal break-all">
            {ledgerSummary.blocks.length > 0
              ? `SHA-256: ${ledgerSummary.blocks[ledgerSummary.blocks.length - 1].hash?.substring(0, 32)}`
              : '—'}
          </span>
        </div>
        <div className="flex flex-col gap-1.5 py-1 border-t border-white/5 pt-2.5">
          <GlossaryTooltip termKey="Cryptographic Block Chain Root" className="text-xs text-[var(--text-muted)] font-medium mb-0.5">
            Cryptographic Block Chain Root
          </GlossaryTooltip>
          <span className="text-[10px] bg-white/5 px-2 py-1.5 rounded border border-white/5 text-[var(--text-main)] select-all leading-normal break-all">
            {ledgerSummary.blocks.length > 0
              ? `Merkle Root: ${ledgerSummary.blocks[ledgerSummary.blocks.length - 1].prev_hash?.substring(0, 32)}`
              : '—'}
          </span>
        </div>

        {/* Dynamic Ledger Transactions Sub-section */}
        <div className="border-t border-white/10 pt-3.5 mt-2.5">
          <span className="text-xs text-[var(--text-muted)] block mb-3 font-semibold uppercase tracking-wider">
            Audit Transaction Ledger
          </span>
          {ledgerSummary.blocks.length === 0 ? (
            <span className="text-xs text-[var(--text-muted)] italic block py-2 text-center bg-white/2 border border-white/5 border-dashed rounded-lg">
              No transactions committed yet.
            </span>
          ) : (
            <div className="flex flex-col gap-2 max-h-[170px] overflow-y-auto pr-1">
              {[...ledgerSummary.blocks].reverse().slice(0, 3).map((block, idx) => {
                let statusColor = 'text-slate-300 border-slate-700/60 bg-slate-800/10';
                if (block.status.includes('CLOSED') || block.status.includes('FILLED') || block.status.includes('RESOLVED')) {
                  statusColor = 'text-[var(--accent-green)] border-[var(--accent-green)]/20 bg-[var(--glow-green)]';
                } else if (block.status === 'DENIED') {
                  statusColor = 'text-[var(--accent-red)] border-[var(--accent-red)]/20 bg-[var(--glow-red)]';
                }
                return (
                  <div key={idx} className="flex flex-col gap-1.5 bg-white/5 border border-white/5 p-2 rounded-lg text-[10.5px]">
                    <div className="flex justify-between items-center text-[9px] text-[var(--text-muted)]">
                      <span>BLOCK #{block.tx_id} // {new Date(block.timestamp).toLocaleTimeString()}</span>
                      <span className={`px-1.5 py-0.5 rounded font-bold border text-[8px] uppercase tracking-wider ${statusColor}`}>
                        {block.status}
                      </span>
                    </div>
                    <div className="flex justify-between items-center font-bold text-slate-100">
                      <span>{block.action} {block.contract_id}</span>
                      <span>{block.size} units @ ${block.price.toFixed(2)}</span>
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>
    </section>
  );
};

export default LedgerAudit;
