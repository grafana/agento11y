import type { CSSProperties, KeyboardEvent } from 'react';
import { useEffect, useId, useRef, useState } from 'react';
import { Icon } from './shell';

export type ResumeAgent = 'claude-code' | 'pi';

interface Feedback {
  kind: 'copied' | 'manual';
  target: string;
}

interface ResumeActionProps {
  conversationId: string;
  agent: ResumeAgent | null;
  /** null when the recorded metadata cannot produce a command. */
  command: string | null;
  /** ID of the element that explains why command is null. */
  describedBy?: string;
  buttonStyle: CSSProperties;
}

/**
 * Copies a resume command to the clipboard and shows it in a panel for manual
 * copying. The viewer cannot observe the terminal, so it never claims that
 * the session resumed.
 */
export function ResumeAction({ conversationId, agent, command, describedBy, buttonStyle }: ResumeActionProps) {
  // A clipboard result or panel belongs to one session and one command.
  const target = command == null ? null : `${conversationId}\u0000${command}`;
  const [feedback, setFeedback] = useState<Feedback | null>(null);
  const [pending, setPending] = useState(false);
  const seqRef = useRef(0);
  const pendingRef = useRef(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const fieldRef = useRef<HTMLTextAreaElement>(null);
  const panelId = useId();
  const messageId = useId();

  useEffect(() => {
    seqRef.current++;
    pendingRef.current = false;
    setPending(false);
    setFeedback((current) => (current && current.target === target ? current : null));
    return () => {
      seqRef.current++;
    };
  }, [target]);

  const shown = feedback && feedback.target === target ? feedback : null;

  useEffect(() => {
    if (shown?.kind !== 'manual') return;
    fieldRef.current?.focus();
    fieldRef.current?.select();
  }, [shown]);

  const copy = async () => {
    if (command == null || target == null || pendingRef.current) return;
    const seq = ++seqRef.current;
    const settle = (kind: Feedback['kind']) => {
      if (seqRef.current !== seq) return;
      pendingRef.current = false;
      setPending(false);
      setFeedback({ kind, target });
    };
    pendingRef.current = true;
    setPending(true);
    try {
      // Called before the first await: Firefox and Safari require transient
      // user activation for clipboard writes.
      await navigator.clipboard.writeText(command);
      settle('copied');
    } catch {
      settle('manual');
    }
  };

  const dismiss = () => {
    setFeedback(null);
    triggerRef.current?.focus();
  };
  const onEscape = (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || !shown) return;
    event.preventDefault();
    event.stopPropagation();
    dismiss();
  };

  const disabled = command == null;
  const message =
    shown?.kind === 'copied'
      ? 'Command copied. Paste it into your terminal.'
      : shown?.kind === 'manual'
        ? 'Could not copy. Select and copy the command below.'
        : '';
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: Child controls own focus; the wrapper handles Escape.
    <div role="presentation" style={{ display: 'inline-flex' }} onKeyDown={onEscape}>
      <button
        ref={triggerRef}
        type="button"
        disabled={disabled}
        aria-disabled={pending || undefined}
        aria-busy={pending || undefined}
        aria-describedby={disabled ? describedBy : undefined}
        aria-controls={shown ? panelId : undefined}
        onClick={copy}
        style={{
          ...buttonStyle,
          cursor: disabled ? 'not-allowed' : pending ? 'progress' : 'pointer',
          opacity: disabled ? 0.5 : 1,
        }}
        onMouseEnter={(event) => {
          if (!disabled) event.currentTarget.style.background = 'var(--action-hover)';
        }}
        onMouseLeave={(event) => (event.currentTarget.style.background = 'transparent')}
      >
        <Icon name="copy" size={12} />
        Copy resume command
      </button>
      {/* Mounted before any feedback: screen readers announce changes to an
          existing live region, not a region inserted with its text. */}
      <div role="status" className="sr-only">
        {message}
      </div>
      {shown && command != null && (
        <section
          id={panelId}
          aria-label="Resume command"
          style={{
            position: 'absolute',
            zIndex: 30,
            // The wrapper is not positioned, so this anchors to the sticky
            // toolbar. Anchoring to the button would push the panel off the
            // left edge once the actions wrap there.
            top: '100%',
            right: 16,
            width: 'min(520px, calc(100% - 32px))',
            padding: 12,
            display: 'flex',
            flexDirection: 'column',
            gap: 8,
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            background: 'var(--bg-secondary)',
            boxShadow: 'var(--shadow-z2)',
            fontSize: 12,
            color: 'var(--fg2)',
          }}
        >
          <div id={messageId} aria-hidden="true" style={{ color: 'var(--fg1)', fontWeight: 500 }}>
            {message}
          </div>
          <textarea
            ref={fieldRef}
            readOnly
            value={command}
            aria-label="Resume command"
            aria-describedby={messageId}
            spellCheck={false}
            rows={3}
            onFocus={(event) => event.currentTarget.select()}
            style={{
              width: '100%',
              boxSizing: 'border-box',
              resize: 'none',
              padding: 8,
              border: '1px solid var(--border-weak)',
              borderRadius: 2,
              background: 'var(--bg-canvas)',
              color: 'var(--fg-max)',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 12,
              lineHeight: 1.45,
              wordBreak: 'break-all',
              overflowY: 'auto',
            }}
          />
          <div>Run on the original machine with your usual agent profile and sandbox.</div>
          {agent === 'pi' && <div>If Pi offers to fork, cancel to avoid creating a new session.</div>}
          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <button type="button" onClick={dismiss} style={{ ...buttonStyle, height: 26 }}>
              Dismiss
            </button>
          </div>
        </section>
      )}
    </div>
  );
}
