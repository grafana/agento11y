import appCSS from 'virtual:theme-css-source';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import analyticsSource from '../internal/local/web/src/analytics.tsx?raw';
import conversationsSource from '../internal/local/web/src/conversations.tsx?raw';
import detailSource from '../internal/local/web/src/detail.tsx?raw';
import searchSource from '../internal/local/web/src/search.tsx?raw';
import {
  applyDocumentTheme,
  documentThemePreference,
  resolveThemePreference,
} from '../internal/local/web/src/settings-model';
import { type HistoryImport, SettingsAppearanceCard, SettingsView } from '../internal/local/web/src/settings-screen';
import settingsSource from '../internal/local/web/src/settings-screen.tsx?raw';
import type { ConfigResponse, Settings, ThemePreference } from '../internal/local/web/src/types';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function settings(overrides: Partial<Settings> = {}): Settings {
  return {
    theme: 'dark',
    endpoint: '',
    tenantId: '',
    otlpEndpoint: '',
    tokenSet: false,
    token: '',
    tokenCleared: false,
    otlpHeaders: '',
    otlpHeadersSet: false,
    otlpHeadersCleared: false,
    capture: '',
    tags: [],
    guards: 'off',
    guardTimeout: '',
    debug: false,
    autoUpdate: true,
    userId: '',
    localForward: false,
    ...overrides,
  };
}

function config(theme: ThemePreference, overrides: Partial<Settings> = {}): ConfigResponse {
  return {
    settings: settings({ theme, ...overrides }),
    preview: `AGENTO11Y_THEME=${theme}\n`,
    path: '/tmp/config.env',
    stackUrl: '',
    forwardStatus: null,
  };
}

const LIGHT_RULE =
  /:root\[data-theme='light'\],\s*:root\[data-theme='system'\]\[data-system-theme='light'\]\s*\{([^}]*)\}/g;

function required<T>(value: T | undefined, label: string): T {
  if (value === undefined) throw new Error(`Missing ${label}`);
  return value;
}

function lightDeclarations(): Record<string, string> {
  const matches = [...appCSS.matchAll(LIGHT_RULE)];
  expect(matches).toHaveLength(1);
  const block = required(matches[0]?.[1], 'light declaration block');
  return Object.fromEntries(
    [...block.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)].map((match) => [
      required(match[1], 'custom property name'),
      required(match[2], 'custom property value').trim(),
    ]),
  );
}

type Color = [number, number, number, number];

function parseColor(value: string, declarations: Record<string, string>): Color {
  const variable = value.match(/^var\((--[\w-]+)\)$/);
  if (variable) return parseColor(declarations[required(variable[1], 'custom property reference')] || '', declarations);
  if (/^#[\da-f]{3}$/i.test(value)) {
    const channel = (position: number) => Number.parseInt(value.slice(position, position + 1).repeat(2), 16) / 255;
    return [channel(1), channel(2), channel(3), 1];
  }
  if (/^#[\da-f]{6}$/i.test(value)) {
    return [
      Number.parseInt(value.slice(1, 3), 16) / 255,
      Number.parseInt(value.slice(3, 5), 16) / 255,
      Number.parseInt(value.slice(5, 7), 16) / 255,
      1,
    ];
  }
  const functional = value.match(/^rgba?\(([^)]+)\)$/i);
  if (functional) {
    const parts = required(functional[1], 'functional color body')
      .split(',')
      .map((part) => Number.parseFloat(part.trim()));
    return [
      required(parts[0], 'red channel') / 255,
      required(parts[1], 'green channel') / 255,
      required(parts[2], 'blue channel') / 255,
      parts[3] ?? 1,
    ];
  }
  throw new Error(`Unsupported CSS color: ${value}`);
}

function composite(foreground: Color, background: Color): Color {
  return [
    foreground[0] * foreground[3] + background[0] * (1 - foreground[3]),
    foreground[1] * foreground[3] + background[1] * (1 - foreground[3]),
    foreground[2] * foreground[3] + background[2] * (1 - foreground[3]),
    1,
  ];
}

function linearChannel(channel: number): number {
  return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
}

function luminance([r, g, b]: Color): number {
  return linearChannel(r) * 0.2126 + linearChannel(g) * 0.7152 + linearChannel(b) * 0.0722;
}

function contrastRatio(foreground: Color, background: Color): number {
  const foregroundLuminance = luminance(foreground);
  const backgroundLuminance = luminance(background);
  const light = Math.max(foregroundLuminance, backgroundLuminance);
  const dark = Math.min(foregroundLuminance, backgroundLuminance);
  return (light + 0.05) / (dark + 0.05);
}

function tokenContrast(
  declarations: Record<string, string>,
  foreground: string,
  background: string,
  underlay = '#FFFFFF',
): number {
  const underlayColor = parseColor(declarations[underlay] || underlay, declarations);
  const backgroundColor = composite(parseColor(declarations[background] || background, declarations), underlayColor);
  const foregroundColor = composite(parseColor(declarations[foreground] || foreground, declarations), backgroundColor);
  return contrastRatio(foregroundColor, backgroundColor);
}

function layeredTokenContrast(
  declarations: Record<string, string>,
  foreground: string,
  backgrounds: readonly string[],
): number {
  const colors = backgrounds.map((value) => parseColor(declarations[value] || value, declarations));
  let background = required(colors.pop(), 'opaque background layer');
  for (let index = colors.length - 1; index >= 0; index--) {
    background = composite(required(colors[index], 'background layer'), background);
  }
  const foregroundColor = composite(parseColor(declarations[foreground] || foreground, declarations), background);
  return contrastRatio(foregroundColor, background);
}

const history: HistoryImport = {
  agents: [],
  offers: [],
  run: null,
  error: null,
  start: vi.fn(async () => null),
  cancel: vi.fn(async () => undefined),
  dismiss: vi.fn(async () => undefined),
  reloadOffers: vi.fn(async () => undefined),
};

function viewProps(saved: ConfigResponse, onThemePreview = vi.fn()) {
  return {
    history,
    config: saved,
    configError: null,
    activeSettingsTab: 'local',
    onSelectTab: vi.fn(),
    onConfig: vi.fn(),
    onThemePreview,
  };
}

describe('light theme CSS contract', () => {
  it('uses one declaration list for fixed light and resolved system light', () => {
    expect([...appCSS.matchAll(LIGHT_RULE)]).toHaveLength(1);
    expect(appCSS.match(/:root\[data-theme='light'\]/g)).toHaveLength(1);
    expect(appCSS.match(/:root\[data-theme='system'\]\[data-system-theme='light'\]/g)).toHaveLength(1);
    expect(appCSS).not.toContain('@media (prefers-color-scheme: light)');
  });

  it('pins the handoff palette and elevation values', () => {
    const declarations = lightDeclarations();
    expect(declarations).toMatchObject({
      '--brand-orange': '#FF671D',
      '--brand-orange-text': '#BF3D12',
      '--brand-red': '#F55F3E',
      '--fg1': 'rgb(36,41,46)',
      '--fg2': 'rgba(36,41,46,0.74)',
      '--fg3': 'rgba(36,41,46,0.68)',
      '--fg-max': '#0B0C0E',
      '--fg-link': 'var(--brand-orange-text)',
      '--border-weak': 'rgba(36,41,46,0.12)',
      '--border-medium': 'rgba(36,41,46,0.22)',
      '--border-strong': 'rgba(36,41,46,0.32)',
      '--action-hover': 'rgba(36,41,46,0.08)',
      '--action-selected': 'rgba(36,41,46,0.06)',
      '--secondary-main': 'rgba(36,41,46,0.10)',
      '--secondary-border': 'rgba(36,41,46,0.32)',
      '--bg-canvas': '#F1F2F4',
      '--bg-primary': '#FFFFFF',
      '--bg-secondary': '#F5F6F8',
      '--surface-bg': '#FFFFFF',
      '--panel-bg': 'rgba(36,41,46,0.04)',
      '--control-bg': '#FFFFFF',
      '--row-hover': 'rgba(36,41,46,0.035)',
      '--group-bg': 'rgba(36,41,46,0.045)',
      '--group-hover': 'rgba(36,41,46,0.075)',
      '--inset-bg': '#FAFBFC',
      '--body-grid': 'none',
      '--inline-code-bg': 'rgba(36,41,46,0.05)',
      '--code-block-bg': '#FFFFFF',
      '--chart-grid': 'rgba(36,41,46,0.10)',
      '--scrollbar-thumb': 'rgba(36,41,46,0.18)',
      '--scrollbar-thumb-hover': 'rgba(36,41,46,0.30)',
      '--selection-bg': 'rgba(61,113,217,0.20)',
      '--modal-backdrop': 'rgba(36,41,46,0.35)',
      '--menu-shadow': '0 8px 24px rgba(36,41,46,0.16)',
      '--surface-shadow': '0 1px 2px rgba(36,41,46,0.06)',
      '--settings-preview-shadow': '0 2px 6px rgba(36,41,46,0.08)',
      '--settings-tab-shadow': '0 2px 6px rgba(36,41,46,0.10)',
      '--shadow-z2': '0 4px 10px rgba(36,41,46,0.18)',
      '--toggle-knob-shadow': '0 1px 2px rgba(36,41,46,0.35)',
      '--primary-main': '#3D71D9',
      '--primary-text': '#1F62E0',
      '--primary-border': '#3D71D9',
      '--info-main': '#3D71D9',
      '--info-text': '#1F62E0',
      '--info-border': '#3D71D9',
      '--info-transparent': 'rgba(61,113,217,0.10)',
      '--success-main': '#1B855E',
      '--success-text': '#0A6640',
      '--success-border': '#1B855E',
      '--success-transparent': 'rgba(27,133,94,0.12)',
      '--warning-main': '#FF9900',
      '--warning-text': '#8A5300',
      '--warning-border': '#FF9900',
      '--warning-transparent': 'rgba(255,153,0,0.14)',
      '--error-main': '#E0226E',
      '--error-text': '#B3054C',
      '--error-border': '#E0226E',
      '--error-transparent': 'rgba(224,34,110,0.10)',
      '--viz-blue': '#3274D9',
      '--viz-green': '#56A64B',
      '--viz-red': '#E02F44',
      '--viz-orange': '#FF780A',
      '--viz-purple': '#A352CC',
      '--viz-yellow': '#E0B400',
      '--agent-accent-text': 'var(--primary-text)',
      '--config-value-text': 'var(--success-text)',
      '--search-match-text': 'var(--warning-text)',
      '--model-opus': '#E5601A',
      '--model-sonnet': '#F27C0C',
      '--model-deepseek': '#3274D9',
      '--model-gpt': '#56A64B',
      '--model-fallback': '#6B7075',
      '--speaker-you-rule': 'linear-gradient(to right, rgba(191,61,18,0.45), rgba(191,61,18,0.06) 55%, transparent)',
      '--speaker-agent-rule':
        'linear-gradient(to right, rgba(50,116,217,0.45), rgba(50,116,217,0.06) 55%, transparent)',
      '--heat-0': 'rgba(36,41,46,0.06)',
      '--heat-1': 'rgba(255,103,29,0.25)',
      '--heat-2': 'rgba(255,103,29,0.48)',
      '--heat-3': 'rgba(255,103,29,0.72)',
      '--heat-4': '#FF671D',
      '--spark-orange': 'rgba(255,103,29,0.45)',
      '--spark-green': 'rgba(86,166,75,0.45)',
      '--spark-neutral': 'rgba(36,41,46,0.20)',
      '--spark-neutral-peak': 'rgba(36,41,46,0.42)',
    });
  });

  it('keeps light text and controls above the unrounded WCAG thresholds', () => {
    const declarations = lightDeclarations();
    const normalText = [
      ['primary on canvas', '--fg1', '--bg-canvas'],
      ['secondary on canvas', '--fg2', '--bg-canvas'],
      ['primary on card', '--fg1', '--bg-primary'],
      ['secondary on card', '--fg2', '--bg-primary'],
      ['primary on secondary surface', '--fg1', '--bg-secondary'],
      ['secondary on secondary surface', '--fg2', '--bg-secondary'],
      ['link on card', '--fg-link', '--bg-primary'],
      ['primary semantic on card', '--primary-text', '--bg-primary'],
      ['info semantic on card', '--info-text', '--bg-primary'],
      ['success semantic on card', '--success-text', '--bg-primary'],
      ['warning semantic on card', '--warning-text', '--bg-primary'],
      ['error semantic on card', '--error-text', '--bg-primary'],
      ['placeholder in input', '--placeholder-fg', '--control-bg'],
      ['control text in input', '--fg1', '--control-bg'],
      ['active pill label', '--primary-text', '--action-selected', '--bg-primary'],
      ['primary button label', '#FFFFFF', '--primary-main'],
      ['primary button hover label', '#FFFFFF', '--primary-shade'],
    ] as const;
    for (const [label, foreground, background, underlay] of normalText) {
      expect(tokenContrast(declarations, foreground, background, underlay), label).toBeGreaterThanOrEqual(4.5);
    }

    const renderedSmallText = [
      ['muted metadata on canvas', '--fg3', ['--bg-canvas']],
      ['muted metadata on cards', '--fg3', ['--surface-bg']],
      ['muted metadata on secondary surfaces', '--fg3', ['--bg-secondary']],
      ['muted metadata in config preview', '--fg3', ['--inset-bg']],
      ['muted metadata on translucent panels', '--fg3', ['--panel-bg', '--bg-canvas']],
      ['muted metadata on info surfaces', '--fg3', ['--info-transparent', '--bg-primary']],
      ['muted metadata on grouped rows', '--fg3', ['--group-bg', '--surface-bg']],
      ['muted metadata on selected rows', '--fg3', ['--row-selected', '--surface-bg']],
      ['config preview value', '--config-value-text', ['--inset-bg']],
      ['YOU speaker label', '--brand-orange-text', ['--bg-canvas']],
      ['AGENT and Reasoning labels', '--agent-accent-text', ['--bg-canvas']],
      ['search match count', '--search-match-text', ['--search-match-bg', '--surface-bg']],
      [
        'search match count on selected row',
        '--search-match-text',
        ['--search-match-bg', '--row-selected', '--surface-bg'],
      ],
    ] as const;
    for (const [label, foreground, backgrounds] of renderedSmallText) {
      expect(layeredTokenContrast(declarations, foreground, backgrounds), label).toBeGreaterThanOrEqual(4.5);
    }

    expect(tokenContrast(declarations, '--fg-max', '--bg-canvas'), 'large KPI text').toBeGreaterThanOrEqual(3);
  });

  it('uses contrast-safe text roles without dimming enabled labels', () => {
    expect(settingsSource).toContain("color: 'var(--config-value-text)'");
    expect(detailSource).toContain("colour: 'var(--agent-accent-text)'");
    expect(detailSource).toContain("color: 'var(--agent-accent-text)'");
    expect(searchSource).toContain("color: 'var(--search-match-text)'");

    expect(appCSS).toMatch(/input::placeholder\s*\{[^}]*opacity:\s*1;/);
    expect(appCSS).not.toMatch(/\.tools-mode-label span\s*\{[^}]*opacity:/);
    expect(analyticsSource).not.toContain('opacity: hidden ? 0.6 : 1');
    expect(conversationsSource).not.toContain('opacity: off ? 0.6 : 1');
    expect(searchSource).not.toContain("opacity: phase === 'loading' && hits.length > 0 ? 0.55 : 1");
    expect(searchSource).not.toContain('style={{ fontSize: 11, opacity: 0.7 }}');
  });
});

describe('document theme resolution', () => {
  it('uses dirty form, saved config, and the validated server stamp in order', () => {
    expect(resolveThemePreference('system', 'light', 'dark')).toBe('system');
    expect(resolveThemePreference(null, 'light', 'dark')).toBe('light');
    expect(resolveThemePreference(null, 'sepia', 'system')).toBe('system');
    expect(resolveThemePreference(null, undefined, 'sepia')).toBe('dark');
  });

  it('reads and writes only valid document attributes', () => {
    const root = document.createElement('html');
    root.setAttribute('data-theme', 'light');
    expect(documentThemePreference(root)).toBe('light');

    root.setAttribute('data-theme', 'sepia');
    expect(documentThemePreference(root)).toBe('dark');
    expect(applyDocumentTheme('system', root)).toBe('system');
    expect(root.getAttribute('data-theme')).toBe('system');
    expect(applyDocumentTheme('sepia', root)).toBe('dark');
    expect(root.getAttribute('data-theme')).toBe('dark');
  });
});

describe('SettingsAppearanceCard', () => {
  it('offers the handoff options and emits a theme preference', () => {
    const onChange = vi.fn();
    const { container } = render(<SettingsAppearanceCard theme="dark" onChange={onChange} />);

    expect(screen.getByText('Appearance')).toBeTruthy();
    expect(screen.getByText('Theme')).toBeTruthy();
    expect(container.textContent).toContain(
      'Applies to this viewer only. Match system follows your OS setting and switches without a reload.',
    );
    for (const name of ['Dark', 'Light', 'Match system']) expect(screen.getByRole('button', { name })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Dark' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: 'Dark' }).style.padding).toBe('7px 16px');

    fireEvent.click(screen.getByRole('button', { name: 'Light' }));
    expect(onChange).toHaveBeenCalledWith('light');
  });
});

describe('SettingsView theme preview', () => {
  it('adopts clean polls but holds the form theme across dirty polls', () => {
    const onThemePreview = vi.fn();
    const dark = config('dark');
    const light = config('light');
    const props = viewProps(dark, onThemePreview);
    const rendered = render(<SettingsView {...props} />);

    expect(screen.getByRole('button', { name: 'Dark' }).getAttribute('aria-pressed')).toBe('true');
    rendered.rerender(<SettingsView {...props} config={light} />);
    expect(screen.getByRole('button', { name: 'Light' }).getAttribute('aria-pressed')).toBe('true');
    expect(onThemePreview).toHaveBeenLastCalledWith(null);

    fireEvent.click(screen.getAllByRole('switch')[0] as HTMLElement);
    expect(onThemePreview).toHaveBeenLastCalledWith('light');
    rendered.rerender(<SettingsView {...props} config={dark} />);
    expect(screen.getByRole('button', { name: 'Light' }).getAttribute('aria-pressed')).toBe('true');
    expect(onThemePreview).toHaveBeenLastCalledWith('light');
  });

  it('Reset adopts the latest saved theme and unmount clears a preview', () => {
    const onThemePreview = vi.fn();
    const props = viewProps(config('dark'), onThemePreview);
    const rendered = render(<SettingsView {...props} />);

    fireEvent.click(screen.getByRole('button', { name: 'Light' }));
    expect(onThemePreview).toHaveBeenLastCalledWith('light');
    rendered.rerender(<SettingsView {...props} config={config('system')} />);

    fireEvent.click(screen.getByRole('button', { name: 'Reset' }));
    expect(screen.getByRole('button', { name: 'Match system' }).getAttribute('aria-pressed')).toBe('true');
    expect(onThemePreview).toHaveBeenLastCalledWith(null);

    fireEvent.click(screen.getByRole('button', { name: 'Dark' }));
    expect(onThemePreview).toHaveBeenLastCalledWith('dark');
    rendered.unmount();
    expect(onThemePreview).toHaveBeenLastCalledWith(null);
  });

  it('clears the dirty preview after a successful save', async () => {
    const savedLight = config('light');
    const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      if (String(input) === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(new Response(JSON.stringify(savedLight), { status: 200 }));
      }
      return Promise.resolve(new Response(JSON.stringify({ preview: savedLight.preview }), { status: 200 }));
    });
    vi.stubGlobal('fetch', fetchMock);
    const onThemePreview = vi.fn();
    const props = viewProps(config('dark'), onThemePreview);
    render(<SettingsView {...props} />);

    fireEvent.click(screen.getByRole('button', { name: 'Light' }));
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));

    await waitFor(() => expect(props.onConfig).toHaveBeenCalledWith(savedLight));
    expect(onThemePreview).toHaveBeenLastCalledWith(null);
    const put = fetchMock.mock.calls.find(([, init]) => init?.method === 'PUT');
    expect(JSON.parse(String(put?.[1]?.body)).settings.theme).toBe('light');
  });
});
