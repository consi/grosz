import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from '../i18n';

interface CostLogItem {
  type: 'charging' | 'idle' | 'external';
  date: string;
  description: string;
  energyKwh?: number;
  cost: number;
  sourceId?: number;
  startTime?: string;
  stopTime?: string;
  distance?: number;
  kwhPer100km?: number;
}

interface SessionReport {
  totalSessions: number;
  totalEnergy: number;
  totalCost: number;
  avgCostPerKwh: number;
  totalDuration: number;
  idleEnergy: number;
  idleCost: number;
  externalCosts: number;
  grandTotalCost: number;
  distance: number;
  costPer100km: number;
  kwhPer100km: number;
}

const PAGE_SIZE = 50;

function formatTime(iso: string, tz: string, locale: string): string {
  return new Date(iso).toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: tz });
}

function formatDate(iso: string, locale: string): string {
  const d = new Date(iso + 'T00:00:00');
  return d.toLocaleDateString(locale, { day: 'numeric', month: 'short', year: 'numeric' });
}

function todayStr(): string {
  return new Date().toISOString().slice(0, 10);
}

function monthAgoStr(): string {
  const d = new Date();
  d.setMonth(d.getMonth() - 1);
  return d.toISOString().slice(0, 10);
}

export function Sessions({ refreshKey, timezone }: { refreshKey?: number; timezone: string }) {
  const { t, locale } = useTranslation();
  const [items, setItems] = useState<CostLogItem[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(0);
  const [loading, setLoading] = useState(false);
  const [fromDate, setFromDate] = useState(monthAgoStr());
  const [toDate, setToDate] = useState(todayStr());
  const [report, setReport] = useState<SessionReport | null>(null);
  const [reportLoading, setReportLoading] = useState(false);

  // External cost form
  const [newCostDate, setNewCostDate] = useState(todayStr());
  const [newCostDesc, setNewCostDesc] = useState('');
  const [newCostAmount, setNewCostAmount] = useState('');
  const [costSaving, setCostSaving] = useState(false);

  const fetchCostLog = useCallback(async (p: number) => {
    setLoading(true);
    try {
      const params = new URLSearchParams({
        limit: String(PAGE_SIZE),
        offset: String(p * PAGE_SIZE),
        from: fromDate,
        to: toDate,
      });
      const resp = await fetch(`/api/costlog?${params}`);
      if (!resp.ok) return;
      const data = await resp.json();
      setItems(data.items || []);
      setTotal(data.total || 0);
    } catch { /* ignore */ }
    setLoading(false);
  }, [fromDate, toDate]);

  const fetchReport = useCallback(async () => {
    setReportLoading(true);
    try {
      const params = new URLSearchParams({ from: fromDate, to: toDate });
      const resp = await fetch(`/api/sessions/report?${params}`);
      if (resp.ok) {
        setReport(await resp.json());
      }
    } catch { /* ignore */ }
    setReportLoading(false);
  }, [fromDate, toDate]);

  useEffect(() => {
    fetchCostLog(page);
  }, [page, fetchCostLog, refreshKey]);

  useEffect(() => {
    setPage(0);
    fetchReport();
  }, [fromDate, toDate, fetchReport]);

  const addCost = async () => {
    const amount = parseFloat(newCostAmount);
    if (!newCostDesc || !amount) return;
    setCostSaving(true);
    try {
      await fetch('/api/costs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ date: newCostDate, description: newCostDesc, amount }),
      });
      setNewCostDesc('');
      setNewCostAmount('');
      await Promise.all([fetchCostLog(page), fetchReport()]);
    } catch { /* ignore */ }
    setCostSaving(false);
  };

  const deleteCost = async (id: number) => {
    try {
      await fetch(`/api/costs/${id}`, { method: 'DELETE' });
      await Promise.all([fetchCostLog(page), fetchReport()]);
    } catch { /* ignore */ }
  };

  const typeLabels: Record<string, string> = {
    charging: t('costLog.typeCharging'),
    idle: t('costLog.typeIdle'),
    external: t('costLog.typeExternal'),
  };

  const itemDescription = (item: CostLogItem): string => {
    if (item.type === 'charging' && item.startTime) {
      const start = formatTime(item.startTime, timezone, locale);
      const end = item.stopTime ? formatTime(item.stopTime, timezone, locale) : '\u2026';
      return t('costLog.charging', { start, end });
    }
    if (item.type === 'idle' && item.description === 'Idle consumption') {
      return t('costLog.idleConsumption');
    }
    return item.description;
  };

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <div>
      {/* Report Summary */}
      <div className="card">
        <h2>{t('report.heading')}</h2>
        <div className="report-range">
          <label>
            {t('report.from')}
            <input type="date" value={fromDate} onChange={(e) => setFromDate(e.target.value)} />
          </label>
          <label>
            {t('report.to')}
            <input type="date" value={toDate} onChange={(e) => setToDate(e.target.value)} />
          </label>
          <button
            className="btn-page"
            style={{ alignSelf: 'flex-end' }}
            onClick={() => window.open(`/api/sessions/report/html?from=${fromDate}&to=${toDate}`, '_blank')}
            title={t('report.printReportTitle')}
          >
            {t('report.printReport')}
          </button>
        </div>

        {reportLoading ? (
          <p className="muted">{t('report.loadingReport')}</p>
        ) : report ? (
          <div className="report-grid">
            <div className="report-stat">
              <div className="report-value">{report.totalSessions}</div>
              <div className="report-label">{t('report.sessions')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.totalEnergy.toFixed(1)}</div>
              <div className="report-label">{t('report.kwhCharged')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.totalCost.toFixed(2)}</div>
              <div className="report-label">{t('report.plnCharging')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.avgCostPerKwh.toFixed(3)}</div>
              <div className="report-label">{t('report.plnKwhAvg')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.totalDuration.toFixed(1)}</div>
              <div className="report-label">{t('report.hoursCharging')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.idleEnergy.toFixed(2)}</div>
              <div className="report-label">{t('report.kwhIdle')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.idleCost.toFixed(2)}</div>
              <div className="report-label">{t('report.plnIdle')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.externalCosts.toFixed(2)}</div>
              <div className="report-label">{t('report.plnExternal')}</div>
            </div>
            <div className="report-stat highlight">
              <div className="report-value">{report.grandTotalCost.toFixed(2)}</div>
              <div className="report-label">{t('report.plnTotal')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.distance > 0 ? report.distance.toFixed(0) : '-'}</div>
              <div className="report-label">{t('report.kmDriven')}</div>
            </div>
            <div className="report-stat highlight">
              <div className="report-value">{report.distance > 0 ? report.costPer100km.toFixed(2) : '-'}</div>
              <div className="report-label">{t('report.plnPer100km')}</div>
            </div>
            <div className="report-stat">
              <div className="report-value">{report.distance > 0 ? report.kwhPer100km.toFixed(1) : '-'}</div>
              <div className="report-label">{t('report.kwhPer100km')}</div>
            </div>
          </div>
        ) : null}
      </div>

      {/* Add External Cost */}
      <div className="card">
        <h2>{t('cost.heading')}</h2>
        <div className="cost-form">
          <label>
            {t('cost.date')}
            <input type="date" value={newCostDate} onChange={(e) => setNewCostDate(e.target.value)} />
          </label>
          <label style={{ flex: 1 }}>
            {t('cost.description')}
            <input
              type="text"
              value={newCostDesc}
              onChange={(e) => setNewCostDesc(e.target.value)}
              placeholder={t('cost.placeholder')}
            />
          </label>
          <label>
            {t('cost.amount')}
            <input
              type="number"
              step="0.01"
              value={newCostAmount}
              onChange={(e) => setNewCostAmount(e.target.value)}
              placeholder="0.00"
              style={{ width: '100px' }}
            />
          </label>
          <button className="btn-page" onClick={addCost} disabled={costSaving || !newCostDesc || !newCostAmount}>
            {t('common.add')}
          </button>
        </div>
      </div>

      {/* Unified Cost Log */}
      <div className="card">
        <div className="log-header">
          <h2>{t('costLog.heading')}</h2>
          <span className="log-count muted">{t('common.items', { count: total })}</span>
        </div>

        {loading && !items.length ? (
          <p className="muted" style={{ textAlign: 'center', padding: '2rem 0' }}>{t('common.loading')}</p>
        ) : !items.length ? (
          <p className="muted" style={{ textAlign: 'center', padding: '2rem 0' }}>{t('costLog.noEntries')}</p>
        ) : (
          <>
            <div className="costlog-table">
              <div className="costlog-header-row">
                <span>{t('costLog.date')}</span>
                <span>{t('costLog.type')}</span>
                <span>{t('costLog.description')}</span>
                <span>{t('costLog.energy')}</span>
                <span>{t('costLog.cost')}</span>
                <span></span>
              </div>
              {items.map((item, i) => (
                <div key={`${item.type}-${item.sourceId}-${i}`} className={`costlog-row ${item.type}`}>
                  <span>{formatDate(item.date, locale)}</span>
                  <span><span className={`type-badge ${item.type}`}>{typeLabels[item.type] ?? item.type}</span></span>
                  <span>{itemDescription(item)}</span>
                  <span>{item.energyKwh && item.energyKwh > 0 ? `${item.energyKwh.toFixed(2)} kWh` : '-'}</span>
                  <span>{item.cost.toFixed(2)} PLN</span>
                  <span className="costlog-details">
                    {item.type === 'charging' && item.distance ? `${item.distance.toFixed(0)} km · ${item.kwhPer100km?.toFixed(1)} kWh/100km` : ''}
                    {item.type === 'external' && item.sourceId ? (
                      <button className="btn-delete" onClick={() => deleteCost(item.sourceId!)} title={t('common.delete')}>&times;</button>
                    ) : null}
                  </span>
                </div>
              ))}
            </div>

            {totalPages > 1 && (
              <div className="log-pagination">
                <button className="btn-page" disabled={page === 0} onClick={() => setPage(page - 1)}>
                  {t('common.previous')}
                </button>
                <span className="muted">{t('common.page', { page: page + 1, total: totalPages })}</span>
                <button className="btn-page" disabled={page >= totalPages - 1} onClick={() => setPage(page + 1)}>
                  {t('common.next')}
                </button>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}
