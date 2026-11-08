import React from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import glossaryData from '../data/glossary.json';

// Define entry schema mapping
interface GlossaryEntry {
  term: string;
  definition: string;
  category: string;
}

type GlossaryData = Record<string, GlossaryEntry>;
const glossary = glossaryData as GlossaryData;

interface GlossaryTooltipProps {
  termKey: string;
  children: React.ReactNode;
  className?: string;
}

export const GlossaryTooltip: React.FC<GlossaryTooltipProps> = ({
  termKey,
  children,
  className = '',
}) => {
  const entry = glossary[termKey];

  // If the term key does not exist, degrade gracefully to rendering children
  if (!entry) {
    return <span className={className}>{children}</span>;
  }

  return (
    <Tooltip.Provider delayDuration={300}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span
            className={`cursor-help border-b border-dashed border-slate-500/40 hover:border-indigo-400/80 transition-colors duration-150 inline-flex items-center ${className}`}
          >
            {children}
          </span>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content
            className="z-50 max-w-xs bg-[var(--card-bg)] border border-[var(--border-color)] p-3 rounded-lg shadow-md backdrop-blur-md text-xs text-[var(--text-main)] select-none animate-in fade-in-0 zoom-in-95 duration-200 ease-out"
            sideOffset={6}
          >
            <div className="flex flex-col gap-1 font-sans">
              <div className="flex justify-between items-center gap-2 mb-0.5">
                <span className="text-[10px] font-semibold text-[var(--primary)] uppercase tracking-wider">
                  {entry.category}
                </span>
              </div>
              <span className="text-sm font-bold text-[var(--text-main)]">
                {entry.term}
              </span>
              <p className="text-[var(--text-muted)] text-xs leading-normal font-normal">
                {entry.definition}
              </p>
            </div>
            <Tooltip.Arrow className="fill-[var(--card-bg)] stroke-[var(--border-color)] stroke-1" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  );
};

export default GlossaryTooltip;
