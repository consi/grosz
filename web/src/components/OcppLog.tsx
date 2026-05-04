import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from '../i18n';

interface OcppEvent {
  id: number;
  timestamp: string;
  direction: string;
  chargeBox: string;
  action: string;
  payload: unknown;
}

const PAGE_SIZE = 100;

export function OcppLog({ refreshKey, timezone }: { refreshKey?: number; timezone: string }) {
  const { t, locale } = useTranslation();
  const [events, setEvents] = useState<OcppEvent[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(0);
  const [filter, setFilter] = useState('');
  const [actions, setActions] = useState<string[]>([]);
  const [expandedId, setExpandedId] = useState<number | null>(null);
  const [loading, setLoading] = useState(false);

  const fetchPage = useCallback(async (p: number, action: string) => {
    setLoading(true);
    try {
      const params = new URLSearchParams({ n: String(PAGE_SIZE), offset: String(p * PAGE_SIZE) });
      if (action) params.set('action', action);
      const resp = await fetch(`/api/events?${params}`);
      if (!resp.ok) return;
      const data = await resp.json();
      setEvents(data.events || []);
      setTotal(data.total || 0);
    } catch { /* ignore */ }
    setLoading(false);
  }, []);

  // Fetch actions list once
  useEffect(() => {
    fetch('/api/events?n=500')
      .then((r) => r.json())
      .then((data) => {
        const evts: OcppEvent[] = data.events || [];
        const acts = [...new Set(evts.map((e) => e.action))].sort();
        setActions(acts);
      })
      .catch(() => {});
  }, []);

  useEffect(() => {
    fetchPage(page, filter);
  }, [page, filter, fetchPage, refreshKey]);

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <div className="card">
      <div className="log-header">
        <h2>{t('ocppLog.heading')}</h2>
        <div className="log-controls">
          <span className="log-count muted">{t('common.events', { count: total })}</span>
          <select
            className="log-filter"
            value={filter}
            onChange={(e) => { setFilter(e.target.value); setPage(0); }}
          >
            <option value="">{t('ocppLog.allActions')}</option>
            {actions.map((a) => (
              <option key={a} value={a}>{a}</option>
            ))}
          </select>
        </div>
      </div>

      {loading && !events.length ? (
        <div className="log-empty"><p className="muted">{t('common.loading')}</p></div>
      ) : !events.length ? (
        <div className="log-empty">
          <p className="muted">{t('ocppLog.noEvents')}</p>
          <p className="muted" style={{ fontSize: '0.75rem' }}>{t('ocppLog.noEventsHint')}</p>
        </div>
      ) : (
        <>
          <div className="log-list">
            {events.map((ev) => (
              <div
                key={ev.id}
                className={`log-row ${expandedId === ev.id ? 'expanded' : ''}`}
                onClick={() => setExpandedId(expandedId === ev.id ? null : ev.id)}
              >
                <span className="log-time">
                  {formatTime(ev.timestamp, timezone, locale)}
                </span>
                <span className={`log-dir ${ev.direction}`}>
                  {ev.direction === 'recv' ? '\u2193' : '\u2191'}
                </span>
                <span className="log-action">{ev.action}</span>
                <span className="log-cp muted">{ev.chargeBox}</span>

                {expandedId === ev.id && (
                  <pre className="log-payload">{formatPayload(ev.payload)}</pre>
                )}
              </div>
            ))}
          </div>

          {totalPages > 1 && (
            <div className="log-pagination">
              <button
                className="btn-page"
                disabled={page === 0}
                onClick={() => setPage(page - 1)}
              >
                {t('common.previous')}
              </button>
              <span className="muted">
                {t('common.page', { page: page + 1, total: totalPages })}
              </span>
              <button
                className="btn-page"
                disabled={page >= totalPages - 1}
                onClick={() => setPage(page + 1)}
              >
                {t('common.next')}
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function formatTime(ts: string, tz: string, locale: string): string {
  const d = new Date(ts);
  const date = d.toLocaleDateString(locale, { month: 'short', day: 'numeric', timeZone: tz });
  const time = d.toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false, timeZone: tz });
  return `${date} ${time}`;
}

function formatPayload(payload: unknown): string {
  if (typeof payload === 'string') {
    try {
      return JSON.stringify(JSON.parse(payload), null, 2);
    } catch {
      return payload;
    }
  }
  try {
    return JSON.stringify(payload, null, 2);
  } catch {
    return String(payload);
  }
}
