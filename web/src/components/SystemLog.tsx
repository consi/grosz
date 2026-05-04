import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from '../i18n';

interface SystemEvent {
  id: number;
  timestamp: string;
  source: string;
  action: string;
  level: string;
  input: unknown;
  result: unknown;
}

const PAGE_SIZE = 100;
const SOURCES = ['scheduler', 'tariff', 'renault', 'meter', 'zappi', 'ocpp'];

export function SystemLog({ refreshKey, timezone }: { refreshKey?: number; timezone: string }) {
  const { t, locale } = useTranslation();
  const [events, setEvents] = useState<SystemEvent[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(0);
  const [filter, setFilter] = useState('');
  const [expandedId, setExpandedId] = useState<number | null>(null);
  const [loading, setLoading] = useState(false);

  const fetchPage = useCallback(async (p: number, source: string) => {
    setLoading(true);
    try {
      const params = new URLSearchParams({ n: String(PAGE_SIZE), offset: String(p * PAGE_SIZE) });
      if (source) params.set('source', source);
      const resp = await fetch(`/api/system-events?${params}`);
      if (!resp.ok) return;
      const data = await resp.json();
      setEvents(data.events || []);
      setTotal(data.total || 0);
    } catch { /* ignore */ }
    setLoading(false);
  }, []);

  useEffect(() => {
    fetchPage(page, filter);
  }, [page, filter, fetchPage, refreshKey]);

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <div className="card">
      <div className="log-header">
        <h2>{t('sysLog.heading')}</h2>
        <div className="log-controls">
          <span className="log-count muted">{t('common.events', { count: total })}</span>
          <select
            className="log-filter"
            value={filter}
            onChange={(e) => { setFilter(e.target.value); setPage(0); }}
          >
            <option value="">{t('sysLog.allSources')}</option>
            {SOURCES.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
        </div>
      </div>

      {loading && !events.length ? (
        <div className="log-empty"><p className="muted">{t('common.loading')}</p></div>
      ) : !events.length ? (
        <div className="log-empty">
          <p className="muted">{t('sysLog.noEvents')}</p>
          <p className="muted" style={{ fontSize: '0.75rem' }}>{t('sysLog.noEventsHint')}</p>
        </div>
      ) : (
        <>
          <div className="log-list">
            {events.map((ev) => (
              <div
                key={ev.id}
                className={`syslog-row ${expandedId === ev.id ? 'expanded' : ''}`}
                onClick={() => setExpandedId(expandedId === ev.id ? null : ev.id)}
              >
                <span className="log-time">
                  {formatTime(ev.timestamp, timezone, locale)}
                </span>
                <span className={`source-badge ${ev.source}`}>
                  {ev.source}
                </span>
                <span className="log-action">{ev.action}</span>
                <span className={`log-level ${ev.level}`}>{ev.level}</span>

                {expandedId === ev.id && (
                  <div className="log-payload">
                    <div className="log-detail-section">
                      <div className="log-detail-label">{t('sysLog.input')}</div>
                      <pre>{formatJSON(ev.input)}</pre>
                    </div>
                    <div className="log-detail-section">
                      <div className="log-detail-label">{t('sysLog.result')}</div>
                      <pre>{formatJSON(ev.result)}</pre>
                    </div>
                  </div>
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

function formatJSON(v: unknown): string {
  if (v === null || v === undefined) return '\u2014';
  if (typeof v === 'string') {
    try {
      return JSON.stringify(JSON.parse(v), null, 2);
    } catch {
      return v;
    }
  }
  try {
    const s = JSON.stringify(v, null, 2);
    return s === '{}' ? '\u2014' : s;
  } catch {
    return String(v);
  }
}
