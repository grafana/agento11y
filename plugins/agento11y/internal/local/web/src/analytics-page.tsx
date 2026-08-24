import type { CSSProperties, ReactNode } from 'react';
import { PageHero, type PageHeroStat, PageShell } from './notices';
import { type AnalyticsTab, analyticsPath, isPlainLeftClick } from './routing';

export interface AnalyticsTabsProps {
  active: AnalyticsTab;
  onSelect: (tab: AnalyticsTab) => void;
}

export function AnalyticsTabs({ active, onSelect }: AnalyticsTabsProps) {
  return (
    <nav className="analytics-subtabs" aria-label="Analytics views">
      {(
        [
          ['overview', 'Overview'],
          ['skills', 'Tools'],
        ] as const
      ).map(([tab, label]) => (
        <a
          key={tab}
          href={analyticsPath(tab)}
          aria-current={active === tab ? 'page' : undefined}
          className={`analytics-subtab${active === tab ? ' analytics-subtab-active' : ''}`}
          onClick={(event) => {
            if (!isPlainLeftClick(event)) return;
            event.preventDefault();
            onSelect(tab);
          }}
        >
          {label}
        </a>
      ))}
    </nav>
  );
}

interface AnalyticsPageProps {
  children: ReactNode;
  stats: PageHeroStat[];
  tabs?: AnalyticsTabsProps;
  style?: CSSProperties;
}

export function AnalyticsPage({ children, stats, tabs, style }: AnalyticsPageProps) {
  return (
    <PageShell maxWidth={1400} style={style}>
      <PageHero
        title="Analytics"
        desc="Cost, tokens, tools, and workspaces across captured local sessions."
        stats={stats}
      />
      {tabs && <AnalyticsTabs {...tabs} />}
      {children}
    </PageShell>
  );
}
