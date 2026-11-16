import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import { useTranslation } from '../i18n';
import type { ChargePointInfo, OcppEvent } from '../types';
import { subscribeOcpp } from '../utils/ocppBus';
import {
  caretContext, childFields, jsonError, leafName, placeholderFor, tokenize, validatePayload,
  type ConsoleAction, type ConsoleField, type Warning,
} from '../utils/ocppConsole';

const MAX_LINES = 100;
const HISTORY_KEY = 'grosz.ocppConsole.history';
const HISTORY_MAX = 20;
const NOISY = ['Heartbeat', 'MeterValues'];

type Line =
  | { seq: number; kind: 'ocpp'; event: OcppEvent }
  | { seq: number; kind: 'result'; at: string; cpId: string; action: string; response?: unknown; error?: string; durationMs?: number };

interface HistoryEntry { action: string; payload: string }

interface Suggestion { label: string; detail: string; field?: ConsoleField; value?: string }

function loadHistory(): HistoryEntry[] {
  try {
    const parsed = JSON.parse(localStorage.getItem(HISTORY_KEY) ?? '[]');
    return Array.isArray(parsed) ? parsed.filter((h) => typeof h?.action === 'string' && typeof h?.payload === 'string') : [];
  } catch { return []; }
}

function saveHistory(entries: HistoryEntry[]): void {
  try { localStorage.setItem(HISTORY_KEY, JSON.stringify(entries)); } catch { /* storage unavailable */ }
}

// Wall clock for event handlers (kept out of render for the purity lint).
const nowMs = () => Date.now();

const pretty = (value: unknown) => JSON.stringify(value, null, 2) ?? '';

// Json renders JSON (or near-JSON) text with token colour classes.
function Json({ text }: { text: string }) {
  return <>{tokenize(text).map((tok, i) => tok.type === 'space' || tok.type === 'punct'
    ? tok.text
    : <span key={i} className={`tok-${tok.type}`}>{tok.text}</span>)}</>;
}

export function OcppConsole({ open, onClose, timezone, chargePoints, defaultCpId }: {
  open: boolean;
  onClose: () => void;
  timezone: string;
  chargePoints: ChargePointInfo[];
  defaultCpId?: string;
}) {
  const { t } = useTranslation();
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (open && !dialog.open) dialog.showModal();
    if (!open && dialog.open) dialog.close();
    if (!open) return;
    const root = document.documentElement;
    const previous = root.style.overflow;
    root.style.overflow = 'hidden';
    return () => { root.style.overflow = previous; };
  }, [open]);
  return <dialog ref={ref} className="ocpp-console" aria-labelledby="ocpp-console-title"
    onCancel={(e) => { e.preventDefault(); onClose(); }} onClose={() => { if (open) onClose(); }}>
    {open && <ConsoleBody onClose={onClose} timezone={timezone} chargePoints={chargePoints} defaultCpId={defaultCpId} t={t} />}
  </dialog>;
}

function ConsoleBody({ onClose, timezone, chargePoints, defaultCpId, t }: {
  onClose: () => void;
  timezone: string;
  chargePoints: ChargePointInfo[];
  defaultCpId?: string;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  const [lines, setLines] = useState<Line[]>([]);
  const [loading, setLoading] = useState(true);
  const [filter, setFilter] = useState('');
  const [hidden, setHidden] = useState<string[]>([]);
  const [expanded, setExpanded] = useState<number[]>([]);
  const [unseen, setUnseen] = useState(false);
  const [actions, setActions] = useState<ConsoleAction[]>([]);
  const [cpId, setCpId] = useState(() => {
    const ids = chargePoints.map((cp) => cp.id);
    if (defaultCpId && ids.includes(defaultCpId)) return defaultCpId;
    return chargePoints.find((cp) => cp.connected)?.id ?? ids[0] ?? defaultCpId ?? '';
  });
  const [actionName, setActionName] = useState('');
  const [loadedAction, setLoadedAction] = useState('');
  const [text, setText] = useState('{}');
  const [cursor, setCursor] = useState({ caret: 0, top: 0, left: 0, lineHeight: 18, above: false });
  const [focused, setFocused] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  const [suggestIndex, setSuggestIndex] = useState(0);
  const [sendingSince, setSendingSince] = useState<number | null>(null);
  const [elapsed, setElapsed] = useState(0);
  const [history, setHistory] = useState<HistoryEntry[]>(loadHistory);
  const [historyIndex, setHistoryIndex] = useState(-1);
  const seq = useRef(0);
  const seen = useRef(new Set<number>());
  const stick = useRef(true);
  const scrollRef = useRef<HTMLDivElement>(null);
  const taRef = useRef<HTMLTextAreaElement>(null);
  const hlRef = useRef<HTMLPreElement>(null);
  const measureRef = useRef<HTMLSpanElement>(null);
  const pendingCaret = useRef<{ caret: number; focus: boolean } | null>(null);

  const append = (added: Line[]) => {
    if (!added.length) return;
    setLines((prev) => [...prev, ...added].slice(-MAX_LINES));
    if (!stick.current) setUnseen(true);
  };
  const ocppLine = (event: OcppEvent): Line | null => {
    if (event.id > 0) {
      if (seen.current.has(event.id)) return null;
      seen.current.add(event.id);
    }
    return { seq: ++seq.current, kind: 'ocpp', event };
  };

  // Live messages from the app's SSE stream, then the recent backlog.
  useEffect(() => {
    let cancelled = false;
    const unsubscribe = subscribeOcpp((event) => {
      const line = ocppLine(event);
      if (line) append([line]);
    });
    fetch(`/api/events?n=${MAX_LINES}`)
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (cancelled || !d) return;
        const backlog = ((d.events || []) as OcppEvent[]).slice().reverse()
          .map(ocppLine).filter((l): l is Line => l !== null);
        setLines((prev) => [...backlog, ...prev].slice(-MAX_LINES));
      })
      .catch(() => {})
      .finally(() => { if (!cancelled) setLoading(false); });
    fetch('/api/ocpp/console/actions')
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => { if (!cancelled && Array.isArray(d?.actions)) setActions(d.actions); })
      .catch(() => {});
    return () => { cancelled = true; unsubscribe(); };
  }, []);

  // Keep the scrollback pinned to the bottom unless the user scrolled up.
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [lines, filter, hidden]);

  // Restore the caret after programmatic edits (suggestions, history).
  useLayoutEffect(() => {
    const ta = taRef.current;
    const pending = pendingCaret.current;
    if (ta && pending) {
      if (pending.focus) ta.focus();
      ta.setSelectionRange(pending.caret, pending.caret);
      if (!pending.focus) { ta.scrollTop = 0; ta.scrollLeft = 0; }
      pendingCaret.current = null;
    }
  }, [text]);

  useEffect(() => {
    if (sendingSince === null) return;
    const id = window.setInterval(() => setElapsed(Math.round((nowMs() - sendingSince) / 1000)), 500);
    return () => window.clearInterval(id);
  }, [sendingSince]);

  const timeFmt = useMemo(() => new Intl.DateTimeFormat('en-GB', {
    timeZone: timezone, hour: '2-digit', minute: '2-digit', second: '2-digit', fractionalSecondDigits: 3, hourCycle: 'h23',
  }), [timezone]);
  const fmtTime = (iso: string) => { try { return timeFmt.format(new Date(iso)); } catch { return iso; } };

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
    if (stick.current && unseen) setUnseen(false);
  };
  const scrollToBottom = () => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
    stick.current = true;
    setUnseen(false);
  };

  const multiBox = new Set(lines.map((l) => (l.kind === 'ocpp' ? l.event.chargeBox : l.cpId))).size > 1 || chargePoints.length > 1;
  const needle = filter.trim().toLowerCase();
  const visible = lines.filter((l) => {
    const action = l.kind === 'ocpp' ? l.event.action : l.action;
    if (hidden.includes(action)) return false;
    if (!needle) return true;
    const hay = l.kind === 'ocpp'
      ? `${l.event.action} ${l.event.chargeBox} ${JSON.stringify(l.event.payload)}`
      : `${l.action} ${l.cpId} ${l.error ?? ''} ${JSON.stringify(l.response ?? '')}`;
    return hay.toLowerCase().includes(needle);
  });

  // Editor state derived from the text.
  const actionDef = actions.find((a) => a.action === actionName);
  const error = jsonError(text);
  const warnings: Warning[] = actionDef && !error ? validatePayload(JSON.parse(text), actionDef.fields) : [];
  const unknownAction = actionName.trim() !== '' && actions.length > 0 && !actionDef;
  const ctx = focused && !dismissed && actionDef ? caretContext(text, cursor.caret) : null;
  const suggestions: Suggestion[] = (() => {
    if (!ctx || !actionDef) return [];
    const prefix = ctx.partial.toLowerCase();
    if (ctx.mode === 'key') {
      return childFields(actionDef.fields, ctx.path)
        .filter((f) => !ctx.existing.includes(leafName(f.path)) && leafName(f.path).toLowerCase().startsWith(prefix))
        .map((f) => ({
          label: leafName(f.path), field: f,
          detail: [f.type, f.required ? t('console.required') : '', f.enum?.join(' | ') ?? ''].filter(Boolean).join(' · '),
        }));
    }
    const path = ctx.path ? `${ctx.path}.${ctx.key}` : ctx.key;
    const field = actionDef.fields.find((f) => f.path === path);
    return (field?.enum ?? [])
      .filter((v) => v.toLowerCase().startsWith(prefix))
      .map((v) => ({ label: v, value: v, detail: leafName(path ?? '') }));
  })();
  const activeIndex = Math.min(suggestIndex, Math.max(0, suggestions.length - 1));

  const updateCursor = (ta: HTMLTextAreaElement) => {
    const caret = ta.selectionStart;
    const before = ta.value.slice(0, caret).split('\n');
    const style = getComputedStyle(ta);
    const lineHeight = parseFloat(style.lineHeight) || 18;
    const padTop = parseFloat(style.paddingTop) || 0;
    const padLeft = parseFloat(style.paddingLeft) || 0;
    const charWidth = (measureRef.current?.getBoundingClientRect().width ?? 80) / 10;
    // Keep the list inside the editor even when the caret line is scrolled away.
    const top = Math.max(lineHeight, Math.min(padTop + before.length * lineHeight - ta.scrollTop + 2, ta.clientHeight));
    const rawLeft = padLeft + before[before.length - 1].length * charWidth - ta.scrollLeft;
    const left = Math.max(0, Math.min(rawLeft, ta.clientWidth - 250));
    // Open upwards when the caret is in the lower half of the editor.
    const above = top > ta.clientHeight * 0.55;
    setCursor((c) => (c.caret === caret && c.top === top && c.left === left && c.above === above
      ? c : { caret, top, left, lineHeight, above }));
  };

  const replaceText = (next: string, caret: number, focus = true) => {
    pendingCaret.current = { caret, focus };
    setText(next);
    setCursor((c) => ({ ...c, caret }));
  };

  const accept = (s: Suggestion) => {
    if (!ctx) return;
    let rest = text.slice(cursor.caret);
    const inString = text[ctx.replaceFrom] === '"';
    if (inString && rest.startsWith('"')) rest = rest.slice(1);
    let insertion: string;
    let caret: number;
    if (ctx.mode === 'key' && s.field) {
      const ph = placeholderFor(s.field);
      insertion = `"${s.label}": ${ph.text}`;
      caret = ctx.replaceFrom + insertion.length - ph.caretBack;
    } else {
      insertion = `"${s.value}"`;
      caret = ctx.replaceFrom + insertion.length;
    }
    setSuggestIndex(0);
    replaceText(text.slice(0, ctx.replaceFrom) + insertion + rest, caret);
  };

  const loadTemplate = (name: string) => {
    const def = actions.find((a) => a.action === name);
    if (!def) return;
    setLoadedAction(name);
    replaceText(pretty(def.template ?? {}), 0, false);
  };

  const recallHistory = (index: number) => {
    const entry = history[index];
    if (!entry) return;
    setHistoryIndex(index);
    setActionName(entry.action);
    setLoadedAction(entry.action);
    replaceText(entry.payload, entry.payload.length);
  };

  const connected = chargePoints.some((cp) => cp.id === cpId);
  const canSend = sendingSince === null && !error && !!actionDef && !!cpId;
  const send = async () => {
    if (!canSend) return;
    const payload = JSON.parse(text);
    const started = nowMs();
    setSendingSince(started);
    setElapsed(0);
    const entry = { action: actionName, payload: text };
    const nextHistory = [entry, ...history.filter((h) => h.action !== entry.action || h.payload !== entry.payload)].slice(0, HISTORY_MAX);
    setHistory(nextHistory);
    setHistoryIndex(-1);
    saveHistory(nextHistory);
    const result: Omit<Extract<Line, { kind: 'result' }>, 'seq' | 'kind' | 'at'> = { cpId, action: actionName };
    try {
      const r = await fetch(`/api/charger/${encodeURIComponent(cpId)}/ocpp`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: actionName, payload }),
      });
      if ((r.headers.get('content-type') ?? '').includes('json')) {
        const d = await r.json();
        Object.assign(result, { response: d.response, error: d.error || (r.ok ? undefined : `HTTP ${r.status}`), durationMs: d.durationMs });
      } else {
        result.error = (await r.text()).trim() || `HTTP ${r.status}`;
      }
    } catch (e) {
      result.error = t('console.requestFailed', { error: (e as Error).message });
    }
    result.durationMs ??= nowMs() - started;
    stick.current = true;
    append([{ seq: ++seq.current, kind: 'result', at: new Date().toISOString(), ...result }]);
    setSendingSince(null);
  };

  const onEditorKey = (e: ReactKeyboardEvent<HTMLTextAreaElement>) => {
    if (suggestions.length) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        const delta = e.key === 'ArrowDown' ? 1 : -1;
        setSuggestIndex((activeIndex + delta + suggestions.length) % suggestions.length);
        return;
      }
      if ((e.key === 'Enter' && !e.ctrlKey && !e.metaKey) || e.key === 'Tab') {
        e.preventDefault();
        accept(suggestions[activeIndex]);
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        setDismissed(true);
        return;
      }
    }
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); void send(); return; }
    if (e.altKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown') && history.length) {
      e.preventDefault();
      const next = e.key === 'ArrowUp' ? Math.min(historyIndex + 1, history.length - 1) : Math.max(historyIndex - 1, 0);
      recallHistory(next);
    }
  };

  const jsonStatus = error
    ? t('console.jsonError', { line: error.line, column: error.column, message: error.message })
    : '';
  const warningText = (w: Warning) => {
    switch (w.kind) {
      case 'unknown': return t('console.warnUnknown', { path: w.path });
      case 'missing': return t('console.warnMissing', { path: w.path });
      case 'enum': return t('console.warnEnum', { path: w.path, values: w.values.join(', ') });
      case 'type': return t('console.warnType', { path: w.path, type: w.type });
    }
  };

  return <div className="con-body">
    <header className="con-head">
      <h2 id="ocpp-console-title">{t('console.title')}</h2>
      <button type="button" className="con-close" onClick={onClose} aria-label={t('common.close')} title={t('common.close')}>×</button>
    </header>
    <p className="con-warning" role="note">{t('console.warning')}</p>
    <div className="con-toolbar">
      <input type="search" value={filter} onChange={(e) => setFilter(e.target.value)}
        placeholder={t('console.filterPlaceholder')} aria-label={t('console.filter')} />
      {NOISY.map((action) => <button key={action} type="button" className="con-chip" aria-pressed={hidden.includes(action)}
        aria-label={t('console.hideAction', { action })} title={t('console.hideAction', { action })}
        onClick={() => setHidden((h) => (h.includes(action) ? h.filter((a) => a !== action) : [...h, action]))}>
        {action}
      </button>)}
    </div>
    <div className="con-scroll-wrap">
      <div className="con-scroll" ref={scrollRef} onScroll={onScroll} role="log" aria-live="polite" aria-label={t('console.title')}>
        {loading && !lines.length && <p className="con-empty">{t('common.loading')}</p>}
        {!loading && !visible.length && <p className="con-empty">{t('console.empty')}</p>}
        <ol className="con-lines">
          {visible.map((line) => {
            const open = expanded.includes(line.seq);
            const toggle = () => setExpanded((x) => (open ? x.filter((s) => s !== line.seq) : [...x, line.seq]));
            if (line.kind === 'result') {
              const ok = !line.error;
              return <li key={line.seq} className={`con-line con-result ${ok ? 'ok' : 'err'}`}>
                <div className="con-row">
                  <span className="con-time">{fmtTime(line.at)}</span>
                  <span className="con-dir" aria-hidden="true">{ok ? '✓' : '✗'}</span>
                  {multiBox && <span className="con-cb">{line.cpId}</span>}
                  <span className="con-action">{line.action}</span>
                  <span className="con-summary">{ok
                    ? t('console.responseIn', { ms: line.durationMs ?? 0 })
                    : t('console.errorIn', { ms: line.durationMs ?? 0, error: line.error ?? '' })}</span>
                </div>
                {ok && line.response !== undefined && <pre className="con-json"><Json text={pretty(line.response)} /></pre>}
              </li>;
            }
            const ev = line.event;
            const send = ev.direction === 'send';
            return <li key={line.seq} className={`con-line ${send ? 'send' : 'recv'}`}>
              <button type="button" className="con-row" aria-expanded={open} onClick={toggle}>
                <span className="con-time">{fmtTime(ev.timestamp)}</span>
                <span className="con-dir" aria-label={send ? t('console.sent') : t('console.received')}>{send ? '→' : '←'}</span>
                {multiBox && <span className="con-cb">{ev.chargeBox}</span>}
                <span className="con-action">{ev.action}</span>
                {!open && <span className="con-payload">{JSON.stringify(ev.payload) ?? ''}</span>}
              </button>
              {open && <pre className="con-json"><Json text={pretty(ev.payload)} /></pre>}
            </li>;
          })}
        </ol>
      </div>
      {unseen && <button type="button" className="con-new" onClick={scrollToBottom}>{t('console.newMessages')}</button>}
    </div>
    <div className="con-composer">
      <div className="con-fields">
        <label className="con-field con-cp">
          <span>{t('console.chargePoint')}</span>
          <select value={cpId} onChange={(e) => setCpId(e.target.value)} disabled={!chargePoints.length}>
            {!connected && cpId && <option value={cpId}>{cpId}</option>}
            {chargePoints.map((cp) => <option key={cp.id} value={cp.id}>{cp.id}{cp.connected ? '' : ` (${t('console.offline')})`}</option>)}
          </select>
        </label>
        <label className="con-field con-act">
          <span>{t('console.action')}</span>
          <input list="ocpp-console-actions" value={actionName} autoFocus spellCheck={false} autoCapitalize="off" autoComplete="off"
            placeholder={t('console.actionPlaceholder')}
            onChange={(e) => {
              const name = e.target.value;
              setActionName(name);
              if (name !== loadedAction && actions.some((a) => a.action === name)) loadTemplate(name);
            }} />
          <datalist id="ocpp-console-actions">
            {actions.map((a) => <option key={a.action} value={a.action}>{a.profile}</option>)}
          </datalist>
        </label>
        <label className="con-field con-hist">
          <span>{t('console.history')}</span>
          <select value="" disabled={!history.length} onChange={(e) => { if (e.target.value !== '') recallHistory(Number(e.target.value)); }}>
            <option value="">{history.length ? t('console.historyPick', { count: history.length }) : t('console.historyEmpty')}</option>
            {history.map((h, i) => <option key={i} value={i}>{h.action} {h.payload.replace(/\s+/g, ' ').slice(0, 60)}</option>)}
          </select>
        </label>
      </div>
      <div className="con-editor">
        <pre className="con-editor-hl" ref={hlRef} aria-hidden="true"><Json text={`${text}\n`} /></pre>
        <textarea
          ref={taRef}
          value={text}
          wrap="off"
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          autoComplete="off"
          aria-label={t('console.payload')}
          aria-invalid={!!error}
          aria-describedby="ocpp-console-status"
          aria-autocomplete="list"
          aria-expanded={suggestions.length > 0}
          aria-controls={suggestions.length ? 'ocpp-console-suggest' : undefined}
          aria-activedescendant={suggestions.length ? `ocpp-suggest-${activeIndex}` : undefined}
          role="combobox"
          onChange={(e) => { setText(e.target.value); setDismissed(false); setHistoryIndex(-1); updateCursor(e.target); }}
          onSelect={(e) => updateCursor(e.currentTarget)}
          onClick={(e) => { setDismissed(false); updateCursor(e.currentTarget); }}
          onFocus={(e) => { setFocused(true); updateCursor(e.currentTarget); }}
          onBlur={() => setFocused(false)}
          onKeyDown={onEditorKey}
          onScroll={(e) => {
            if (hlRef.current) {
              hlRef.current.scrollTop = e.currentTarget.scrollTop;
              hlRef.current.scrollLeft = e.currentTarget.scrollLeft;
            }
            updateCursor(e.currentTarget);
          }}
        />
        <span className="con-measure" ref={measureRef} aria-hidden="true">0000000000</span>
        {suggestions.length > 0 && <ul id="ocpp-console-suggest" className="con-suggest" role="listbox" aria-label={t('console.suggestions')}
          style={cursor.above
            ? { top: cursor.top - cursor.lineHeight - 4, left: cursor.left, transform: 'translateY(-100%)' }
            : { top: cursor.top, left: cursor.left }}>
          {suggestions.map((s, i) => <li key={s.label} id={`ocpp-suggest-${i}`} role="option" aria-selected={i === activeIndex}
            className={i === activeIndex ? 'active' : ''}
            onMouseDown={(e) => { e.preventDefault(); accept(s); }}>
            <span className="con-suggest-label">{s.label}</span>
            <span className="con-suggest-detail">{s.detail}</span>
          </li>)}
        </ul>}
      </div>
      <div id="ocpp-console-status" className="con-status" aria-live="polite">
        {jsonStatus && <p className="con-error">{jsonStatus}</p>}
        {unknownAction && <p className="con-warn">{t('console.unknownAction', { action: actionName })}</p>}
        {warnings.map((w, i) => <p key={i} className="con-warn">{warningText(w)}</p>)}
      </div>
      <div className="con-actions">
        <button type="button" className="btn-sm" disabled={!!error} onClick={() => replaceText(pretty(JSON.parse(text)), 0)}>{t('console.format')}</button>
        <button type="button" className="btn-sm" disabled={!actionDef} onClick={() => loadTemplate(actionName)}>{t('console.template')}</button>
        <span className="con-hint">{t('console.shortcuts')}</span>
        <button type="button" className="btn-sm primary con-send" disabled={!canSend} onClick={() => void send()}>
          {sendingSince !== null ? <><span className="con-spinner" aria-hidden="true" />{t('console.sending', { seconds: elapsed })}</> : t('console.send')}
        </button>
      </div>
    </div>
  </div>;
}
