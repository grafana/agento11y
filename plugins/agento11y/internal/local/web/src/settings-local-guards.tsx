import { useCallback, useEffect, useState } from 'react';
import { Notice, SURFACE_BG, SurfaceCard } from './notices';
import { Mono } from './settings-model';
import { Icon } from './shell';
import type { GuardPack, GuardsFile } from './types';

function emptyGuards(): GuardsFile {
  return { exists: false, enabled: false, enforcing: 0, packs: [], rules: [] };
}

export function SettingsLocalGuardsCard({ onEnabledChange }: { onEnabledChange?: (enabled: boolean) => void }) {
  const [data, setData] = useState<GuardsFile | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null);

  const load = useCallback(() => {
    return fetch('/api/v1/guards')
      .then((r) =>
        r.ok
          ? (r.json() as Promise<GuardsFile>)
          : r.text().then((t) => Promise.reject(new Error(t.trim() || `HTTP ${r.status}`))),
      )
      .then((body) => {
        setData(body);
        setError(null);
      })
      .catch((err) => {
        setError(String(err.message || err));
        setData((prev) => prev ?? emptyGuards());
      });
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const putGuards = (body: { enabled?: boolean; packs?: Record<string, boolean> }, pendingKey: string) => {
    setPending(pendingKey);
    setError(null);
    fetch('/api/v1/guards', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
      .then((r) =>
        r.ok
          ? (r.json() as Promise<GuardsFile>)
          : r.text().then((t) => Promise.reject(new Error(t.trim() || `HTTP ${r.status}`))),
      )
      .then((next) => {
        setData(next);
        if (body.enabled !== undefined) onEnabledChange?.(next.enabled);
      })
      .catch((err) => setError(String(err.message || err)))
      .finally(() => setPending(null));
  };

  const packs = data?.packs || [];
  const compileErrors = (data?.errors || []).filter(Boolean);
  const enabled = !!data?.enabled;
  const busy = pending !== null;

  return (
    <SurfaceCard style={{ padding: '4px 20px 12px', marginBottom: 16, background: SURFACE_BG }}>
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'flex-start',
          gap: 32,
          padding: '16px 0 2px',
        }}
      >
        <div style={{ minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span
              style={{
                width: 18,
                height: 2,
                borderRadius: 999,
                background: 'var(--brand-orange)',
              }}
            />
            <span
              style={{
                fontSize: 11,
                fontWeight: 700,
                letterSpacing: '.08em',
                textTransform: 'uppercase',
                color: 'var(--fg3)',
              }}
            >
              Guards
            </span>
          </div>
          <div
            style={{
              fontSize: 12,
              lineHeight: 1.5,
              color: 'var(--fg3)',
              padding: '8px 0 0',
              maxWidth: 520,
            }}
          >
            Safety packs that block dangerous tool calls on this machine. Turn this on, then select which packs to
            enforce. They are saved in <Mono>{data?.path || 'guards.toml'}</Mono>.
          </div>
        </div>
        <Switch
          label="Enable guards"
          checked={enabled}
          disabled={busy}
          onToggle={() => {
            if (enabled) {
              const packsOff = Object.fromEntries(packs.map((pack) => [pack.id, false]));
              putGuards({ enabled: false, packs: packsOff }, '__enabled');
              return;
            }
            putGuards({ enabled: true }, '__enabled');
          }}
        />
      </div>
      {error && (
        <div style={{ padding: '4px 0 10px' }}>
          <Notice kind="error" title="Could not update guards">
            {error}
          </Notice>
        </div>
      )}
      {compileErrors.length > 0 && (
        <div style={{ padding: '4px 0 10px' }}>
          <Notice kind="warning" title="Some rules did not compile">
            {compileErrors.join('; ')}
          </Notice>
        </div>
      )}
      {enabled &&
        packs.map((pack) => (
          <PackRow
            key={pack.id}
            pack={pack}
            disabled={busy}
            onToggle={(on) => putGuards({ packs: { [pack.id]: on } }, pack.id)}
          />
        ))}
    </SurfaceCard>
  );
}

function PackRow({
  pack,
  disabled,
  onToggle,
}: {
  pack: GuardPack;
  disabled: boolean;
  onToggle: (enabled: boolean) => void;
}) {
  const [open, setOpen] = useState(false);
  const deny = pack.kind === 'deny';
  const preview = pack.preview || [];
  const previewId = `pack-preview-${pack.id}`;
  return (
    <div
      style={{
        padding: '16px 0',
        borderTop: '1px solid var(--border-weak)',
      }}
    >
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'flex-start',
          gap: 32,
        }}
      >
        <div style={{ minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <Icon name={deny ? 'ban' : 'shield'} size={14} style={{ color: 'var(--fg2)' }} />
            <div style={{ fontSize: 14, fontWeight: 500, color: 'var(--fg1)' }}>{pack.title}</div>
            {preview.length > 0 && (
              <button
                type="button"
                className="pack-preview-toggle"
                aria-expanded={open}
                aria-controls={previewId}
                aria-label={`View ${pack.title}`}
                onClick={() => setOpen((v) => !v)}
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 2,
                  border: 'none',
                  background: 'none',
                  padding: 0,
                  fontSize: 12,
                  fontWeight: 500,
                  cursor: 'pointer',
                }}
              >
                view
                <Icon name={open ? 'chevron' : 'cright'} size={12} />
              </button>
            )}
          </div>
          <div
            style={{
              fontSize: 12,
              lineHeight: 1.5,
              color: 'var(--fg3)',
              maxWidth: 460,
              marginTop: 4,
            }}
          >
            {pack.description}
          </div>
          {pack.detail && (
            <div
              style={{
                fontSize: 11.5,
                color: 'var(--fg3)',
                fontFamily: 'var(--fontFamilyMonospace)',
                marginTop: 6,
              }}
            >
              {pack.detail}
            </div>
          )}
        </div>
        <Switch
          label={pack.title}
          checked={pack.enabled}
          disabled={disabled}
          onToggle={() => onToggle(!pack.enabled)}
        />
      </div>
      {open && preview.length > 0 && (
        <ul
          id={previewId}
          style={{
            margin: '10px 0 0',
            padding: '10px 12px',
            maxHeight: 160,
            overflow: 'auto',
            listStyle: 'none',
            borderRadius: 8,
            background: 'var(--bg-canvas)',
            border: '1px solid var(--border-weak)',
          }}
        >
          {preview.map((item) => (
            <li
              key={item}
              style={{
                fontSize: 11.5,
                lineHeight: 1.6,
                color: 'var(--fg2)',
                fontFamily: 'var(--fontFamilyMonospace)',
              }}
            >
              {item}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Switch({
  label,
  checked,
  disabled,
  onToggle,
}: {
  label: string;
  checked: boolean;
  disabled: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-label={label}
      aria-checked={checked}
      disabled={disabled}
      onClick={onToggle}
      style={{
        position: 'relative',
        width: 38,
        height: 22,
        borderRadius: 9999,
        border: 'none',
        cursor: disabled ? 'not-allowed' : 'pointer',
        padding: 0,
        flexShrink: 0,
        opacity: disabled ? 0.55 : 1,
        background: checked ? 'var(--primary-main)' : 'var(--toggle-off-bg)',
        transition: 'background .15s',
      }}
    >
      <span
        style={{
          position: 'absolute',
          top: 3,
          left: 3,
          width: 16,
          height: 16,
          borderRadius: '50%',
          background: 'var(--toggle-knob-bg)',
          boxShadow: 'var(--toggle-knob-shadow)',
          transform: checked ? 'translateX(16px)' : 'translateX(0)',
          transition: 'transform .15s',
        }}
      />
    </button>
  );
}
