import { useState, useEffect, useCallback, useRef } from 'react';
import { Login } from './components/Login';
import { ChargerStatus } from './components/ChargerStatus';
import { PriceChart, type ChartMarker } from './components/PriceChart';
import { GridStatus } from './components/GridStatus';
import { CarStatus } from './components/CarStatus';
import { ScheduleForm } from './components/ScheduleForm';
import { Sessions } from './components/Sessions';
import { OcppLog } from './components/OcppLog';
import { SystemLog } from './components/SystemLog';
import { Settings } from './components/Settings';
import { useSSE } from './hooks/useSSE';
import { usePullToRefresh } from './hooks/usePullToRefresh';
import { I18nProvider, useTranslation, browserLocale } from './i18n';
import type { Locale } from './i18n';
import type { StatusResponse, Rate, HourlyEnergy, MeterLive } from './types';
import './App.css';

type Tab = 'dashboard' | 'reporting' | 'log' | 'syslog' | 'settings';

const validTabs: Tab[] = ['dashboard', 'reporting', 'log', 'syslog', 'settings'];

function tabFromHash(): Tab {
  const h = location.hash.replace('#', '') as Tab;
  return validTabs.includes(h) ? h : 'dashboard';
}

function App() {
  const [menuOpen, setMenuOpen] = useState(false);
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [passkeysAvailable, setPasskeysAvailable] = useState(false);
  const [tab, setTab] = useState<Tab>(tabFromHash);
  const [status, setStatus] = useState<StatusResponse>({ chargePoints: [], charging: false, mode: 'schedule', soc: 0, minSoc: 0, skipAboveSoc: 0, deadlineTime: '07:00', batteryAutonomy: 0, chargingStatus: 0, plugStatus: 0, chargingRemainingTime: 0 });
  const [rates, setRates] = useState<Rate[]>([]);
  const [consumption, setConsumption] = useState<HourlyEnergy[]>([]);
  const [meterLive, setMeterLive] = useState<MeterLive | null>(null);
  const [chartMarkers, setChartMarkers] = useState<ChartMarker[]>([]);
  const [timezone, setTimezone] = useState<string>(Intl.DateTimeFormat().resolvedOptions().timeZone);
  const [locale, setLocale] = useState<Locale>(browserLocale);
  const [defaultPowerW, setDefaultPowerW] = useState<number>(11000);

  // Auth-aware fetch: redirects to login on 401
  const authedRef = useRef(authed);
  authedRef.current = authed;

  const apiFetch = useCallback((url: string, options?: RequestInit) => {
    return fetch(url, options).then((r) => {
      if (r.status === 401) {
        setAuthed(false);
        return Promise.reject(new Error('unauthorized'));
      }
      return r;
    });
  }, []);

  // Check auth on mount
  useEffect(() => {
    fetch('/api/auth/check')
      .then(async (r) => {
        const data = await r.json();
        setAuthed(r.ok && data.authenticated);
        setPasskeysAvailable(data.passkeys === true);
      })
      .catch(() => setAuthed(false));
  }, []);

  // Initial data fetch
  useEffect(() => {
    if (!authed) return;
    apiFetch('/api/status').then((r) => r.json()).then(setStatus).catch(() => {});
    apiFetch('/api/tariff/rates').then((r) => r.json()).then((d) => setRates(d.rates || [])).catch(() => {});
    apiFetch('/api/meter/hourly').then((r) => r.json()).then(setConsumption).catch(() => {});
    apiFetch('/api/meter/live').then((r) => r.json()).then((d) => { if (d.timestamp) setMeterLive(d); }).catch(() => {});
    apiFetch('/api/chart-markers?hours=72').then((r) => r.json()).then((d) => {
      if (Array.isArray(d)) setChartMarkers(d);
    }).catch(() => {});
    apiFetch('/api/settings').then((r) => r.json()).then((s) => {
      if (s['display.timezone']) setTimezone(s['display.timezone']);
      if (s['display.language']) setLocale(s['display.language'] as Locale);
      const mp = parseFloat(s['charger.max_power']);
      if (isFinite(mp) && mp > 0) setDefaultPowerW(mp);
    }).catch(() => {});
  }, [authed, apiFetch]);

  // Refresh consumption data periodically (DB aggregation, not real-time)
  useEffect(() => {
    if (!authed) return;
    const interval = setInterval(() => {
      apiFetch('/api/meter/hourly').then((r) => r.json()).then(setConsumption).catch(() => {});
    }, 60000);
    return () => clearInterval(interval);
  }, [authed, apiFetch]);

  // Refresh rates every 5 minutes (changes at most once a day)
  useEffect(() => {
    if (!authed) return;
    const interval = setInterval(() => {
      apiFetch('/api/tariff/rates').then((r) => r.json()).then((d) => setRates(d.rates || [])).catch(() => {});
    }, 300000);
    return () => clearInterval(interval);
  }, [authed, apiFetch]);

  // SSE for real-time updates
  useSSE(
    authed ? '/api/events/stream' : null,
    useCallback((event: string, data: string) => {
      try {
        const parsed = JSON.parse(data);
        if (event === 'status') setStatus(parsed);
        if (event === 'rates') setRates(parsed.rates || []);
        if (event === 'meter') setMeterLive(parsed);
      } catch { /* ignore */ }
    }, []),
    useCallback(() => setAuthed(false), []),
  );

  // Persist tab in URL hash
  useEffect(() => {
    location.hash = tab;
  }, [tab]);
  useEffect(() => {
    const onHash = () => setTab(tabFromHash());
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  // Pull-to-refresh
  const [refreshKey, setRefreshKey] = useState(0);
  const refreshDashboard = useCallback(() => {
    apiFetch('/api/status').then((r) => r.json()).then(setStatus).catch(() => {});
    apiFetch('/api/tariff/rates').then((r) => r.json()).then((d) => setRates(d.rates || [])).catch(() => {});
    apiFetch('/api/meter/hourly').then((r) => r.json()).then(setConsumption).catch(() => {});
    apiFetch('/api/meter/live').then((r) => r.json()).then((d) => { if (d.timestamp) setMeterLive(d); }).catch(() => {});
    apiFetch('/api/chart-markers?hours=72').then((r) => r.json()).then((d) => { if (Array.isArray(d)) setChartMarkers(d); }).catch(() => {});
  }, [apiFetch]);

  const handlePullRefresh = useCallback(() => {
    if (tab === 'dashboard') {
      refreshDashboard();
    } else {
      setRefreshKey((k) => k + 1);
    }
  }, [tab, refreshDashboard]);

  const { pulling, pullDistance, refreshing } = usePullToRefresh({
    onRefresh: handlePullRefresh,
    enabled: authed === true,
  });

  const [modeError, setModeError] = useState<string | null>(null);

  const handleModeChange = async (mode: 'off' | 'schedule' | 'force') => {
    setStatus((s) => ({ ...s, mode }));
    setModeError(null);
    try {
      const resp = await apiFetch('/api/charger/mode', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mode }),
      });
      const data = await resp.json();
      if (data.errors?.length) {
        setModeError(data.errors.join(', '));
        setTimeout(() => setModeError(null), 8000);
      }
    } catch { /* handled by apiFetch */ }
    apiFetch('/api/status').then((r) => r.json()).then(setStatus).catch(() => {});
  };

  const handleScheduleApply = async () => {
    await apiFetch('/api/schedule', { method: 'POST' }).catch(() => null);
    apiFetch('/api/status').then((r) => r.json()).then(setStatus).catch(() => {});
    apiFetch('/api/tariff/rates').then((r) => r.json()).then((d) => setRates(d.rates || [])).catch(() => {});
    apiFetch('/api/meter/hourly').then((r) => r.json()).then(setConsumption).catch(() => {});
    apiFetch('/api/chart-markers?hours=72').then((r) => r.json()).then((d) => { if (Array.isArray(d)) setChartMarkers(d); }).catch(() => {});
  };

  const handleScheduleCancel = async () => {
    await apiFetch('/api/schedule', { method: 'DELETE' }).catch(() => {});
    apiFetch('/api/status').then((r) => r.json()).then(setStatus).catch(() => {});
    apiFetch('/api/tariff/rates').then((r) => r.json()).then((d) => setRates(d.rates || [])).catch(() => {});
    apiFetch('/api/meter/hourly').then((r) => r.json()).then(setConsumption).catch(() => {});
    apiFetch('/api/chart-markers?hours=72').then((r) => r.json()).then((d) => { if (Array.isArray(d)) setChartMarkers(d); }).catch(() => {});
  };

  const handleSlotCancel = async (date: string) => {
    const resp = await apiFetch(`/api/schedule/${date}`, { method: 'DELETE' }).catch(() => null);
    if (resp?.ok) {
      const data = await resp.json();
      if (data.schedule) {
        setStatus((s) => ({ ...s, schedule: data.schedule }));
      }
    }
  };

  const handleSlotRestore = async (date: string) => {
    const resp = await apiFetch(`/api/schedule/${date}/restore`, { method: 'POST' }).catch(() => null);
    if (resp?.ok) {
      const data = await resp.json();
      if (data.schedule) {
        setStatus((s) => ({ ...s, schedule: data.schedule }));
      }
    }
  };

  const handleCreateOverride = async (payload: { kind: 'force' | 'block'; start: string; end: string; powerW: number }): Promise<{ ok: boolean; error?: string }> => {
    try {
      const r = await apiFetch('/api/schedule/overrides', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      const data = await r.json();
      if (!r.ok) return { ok: false, error: data?.error || `HTTP ${r.status}` };
      // Refresh status (also updates overrides + schedule)
      apiFetch('/api/status').then((res) => res.json()).then(setStatus).catch(() => {});
      return { ok: true };
    } catch (e) {
      return { ok: false, error: String(e) };
    }
  };

  const handleDeleteOverride = async (id: number): Promise<void> => {
    try {
      await apiFetch(`/api/schedule/overrides/${id}`, { method: 'DELETE' });
      apiFetch('/api/status').then((r) => r.json()).then(setStatus).catch(() => {});
    } catch {
      /* handled by apiFetch */
    }
  };

  const handleLogout = async () => {
    await fetch('/api/logout', { method: 'POST' });
    setAuthed(false);
  };

  // Loading state
  if (authed === null) return null;

  // Login screen
  if (!authed) return (
    <I18nProvider locale={locale}>
      <Login onLogin={() => setAuthed(true)} passkeysAvailable={passkeysAvailable} />
    </I18nProvider>
  );

  return (
    <I18nProvider locale={locale}>
      <AppContent
        tab={tab} setTab={setTab}
        menuOpen={menuOpen} setMenuOpen={setMenuOpen}
        status={status} rates={rates} consumption={consumption}
        meterLive={meterLive} chartMarkers={chartMarkers}
        timezone={timezone} locale={locale} setLocale={setLocale}
        pulling={pulling} pullDistance={pullDistance} refreshing={refreshing}
        modeError={modeError} refreshKey={refreshKey}
        defaultPowerW={defaultPowerW}
        onModeChange={handleModeChange}
        onScheduleApply={handleScheduleApply}
        onScheduleCancel={handleScheduleCancel}
        onSlotCancel={handleSlotCancel}
        onSlotRestore={handleSlotRestore}
        onCreateOverride={handleCreateOverride}
        onDeleteOverride={handleDeleteOverride}
        onLogout={handleLogout}
      />
    </I18nProvider>
  );
}

function AppContent({
  tab, setTab, menuOpen, setMenuOpen,
  status, rates, consumption, meterLive, chartMarkers,
  timezone, locale, setLocale,
  pulling, pullDistance, refreshing,
  modeError, refreshKey,
  defaultPowerW,
  onModeChange, onScheduleApply, onScheduleCancel, onSlotCancel, onSlotRestore,
  onCreateOverride, onDeleteOverride, onLogout,
}: {
  tab: Tab; setTab: (t: Tab) => void;
  menuOpen: boolean; setMenuOpen: (o: boolean | ((p: boolean) => boolean)) => void;
  status: StatusResponse; rates: Rate[]; consumption: HourlyEnergy[];
  meterLive: MeterLive | null; chartMarkers: ChartMarker[];
  timezone: string; locale: Locale; setLocale: (l: Locale) => void;
  pulling: boolean; pullDistance: number; refreshing: boolean;
  modeError: string | null; refreshKey: number;
  defaultPowerW: number;
  onModeChange: (mode: 'off' | 'schedule' | 'force') => void;
  onScheduleApply: () => void; onScheduleCancel: () => void;
  onSlotCancel: (date: string) => void; onSlotRestore: (date: string) => void;
  onCreateOverride: (payload: { kind: 'force' | 'block'; start: string; end: string; powerW: number }) => Promise<{ ok: boolean; error?: string }>;
  onDeleteOverride: (id: number) => Promise<void>;
  onLogout: () => void;
}) {
  const { t } = useTranslation();

  return (
    <div className="app">
      <header>
        <h1 onClick={() => { setTab('dashboard'); setMenuOpen(false); }}>grosz</h1>
        <button className="hamburger" onClick={() => setMenuOpen((o: boolean) => !o)} aria-label={t('nav.menu')}>
          <span /><span /><span />
        </button>
        <nav className={menuOpen ? 'open' : ''}>
          <button className={tab === 'dashboard' ? 'active' : ''} onClick={() => { setTab('dashboard'); setMenuOpen(false); }}>
            {t('nav.dashboard')}
          </button>
          <button className={tab === 'reporting' ? 'active' : ''} onClick={() => { setTab('reporting'); setMenuOpen(false); }}>
            {t('nav.reporting')}
          </button>
          <button className={tab === 'log' ? 'active' : ''} onClick={() => { setTab('log'); setMenuOpen(false); }}>
            {t('nav.ocppLog')}
          </button>
          <button className={tab === 'syslog' ? 'active' : ''} onClick={() => { setTab('syslog'); setMenuOpen(false); }}>
            {t('nav.systemLog')}
          </button>
          <button className={tab === 'settings' ? 'active' : ''} onClick={() => { setTab('settings'); setMenuOpen(false); }}>
            {t('nav.settings')}
          </button>
          <button className="logout-btn" onClick={onLogout}>{t('nav.logout')}</button>
        </nav>
      </header>

      {(pulling || refreshing) && (
        <div className="pull-indicator" style={{ opacity: refreshing ? 1 : Math.min(1, pullDistance / 80) }}>
          <div className={`pull-spinner ${refreshing ? 'spinning' : ''}`} style={{ transform: `rotate(${pullDistance * 3}deg)` }}>↻</div>
        </div>
      )}
      <main style={pulling ? { transform: `translateY(${pullDistance}px)`, transition: 'none' } : undefined}>
        {tab === 'dashboard' && (
          <div className="dashboard-grid">
            <div className="dashboard-col">
              <ChargerStatus
                chargePoints={status.chargePoints}
                schedule={status.schedule}
                charging={status.charging}
                mode={status.mode || 'schedule'}
                onModeChange={onModeChange}
                error={modeError}
              />
              <PriceChart rates={rates} schedule={status.schedule} consumption={consumption} markers={chartMarkers} timezone={timezone} />
            </div>
            <div className="dashboard-col">
              <GridStatus data={meterLive} />
              <CarStatus
                soc={status.soc}
                minSoc={status.minSoc}
                skipAboveSoc={status.skipAboveSoc}
                batteryAutonomy={status.batteryAutonomy}
                chargingStatus={status.chargingStatus}
                plugStatus={status.plugStatus}
                chargingRemainingTime={status.chargingRemainingTime}
                batteryTimestamp={status.batteryTimestamp}
                vehicleModel={status.vehicleModel}
                vehiclePicture={status.vehiclePicture}
                mileage={status.mileage}
              />
              <ScheduleForm
                schedule={status.schedule}
                overrides={status.overrides || []}
                skipReason={status.skipReason}
                skipReasonKey={status.skipReasonKey}
                skipReasonParams={status.skipReasonParams}
                onApply={onScheduleApply}
                onCancel={onScheduleCancel}
                onSlotCancel={onSlotCancel}
                onSlotRestore={onSlotRestore}
                onCreateOverride={onCreateOverride}
                onDeleteOverride={onDeleteOverride}
                timezone={timezone}
                defaultPowerW={defaultPowerW}
              />
            </div>
          </div>
        )}
        {tab === 'reporting' && <Sessions refreshKey={refreshKey} timezone={timezone} />}
        {tab === 'log' && <OcppLog refreshKey={refreshKey} timezone={timezone} />}
        {tab === 'syslog' && <SystemLog refreshKey={refreshKey} timezone={timezone} />}
        {tab === 'settings' && <Settings refreshKey={refreshKey} locale={locale} onLocaleChange={setLocale} />}
      </main>
    </div>
  );
}

export default App;
