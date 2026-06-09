import { useState, useEffect, useCallback, useRef, type ReactNode } from 'react';
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
import { VersionUpdateBanner } from './components/VersionUpdateBanner';
import { RenaultReauthBanner } from './components/RenaultReauthBanner';
import { useSSE } from './hooks/useSSE';
import { usePullToRefresh } from './hooks/usePullToRefresh';
import { I18nProvider, useTranslation, browserLocale } from './i18n';
import type { Locale, TranslationKey } from './i18n';
import type { StatusResponse, Rate, HourlyEnergy, MeterLive } from './types';
import './App.css';

type Tab = 'dashboard' | 'reporting' | 'log' | 'syslog' | 'settings';

const validTabs: Tab[] = ['dashboard', 'reporting', 'log', 'syslog', 'settings'];

// Feather-style outline icons (24x24, stroked with currentColor).
const TABS: { id: Tab; labelKey: TranslationKey; icon: ReactNode }[] = [
  {
    id: 'dashboard', labelKey: 'nav.dashboard',
    icon: <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2" />,
  },
  {
    id: 'reporting', labelKey: 'nav.reporting',
    icon: <><line x1="18" y1="20" x2="18" y2="10" /><line x1="12" y1="20" x2="12" y2="4" /><line x1="6" y1="20" x2="6" y2="14" /></>,
  },
  {
    id: 'log', labelKey: 'nav.ocppLog',
    icon: <><line x1="8" y1="6" x2="21" y2="6" /><line x1="8" y1="12" x2="21" y2="12" /><line x1="8" y1="18" x2="21" y2="18" /><line x1="3" y1="6" x2="3.01" y2="6" /><line x1="3" y1="12" x2="3.01" y2="12" /><line x1="3" y1="18" x2="3.01" y2="18" /></>,
  },
  {
    id: 'syslog', labelKey: 'nav.systemLog',
    icon: <><polyline points="4 17 10 11 4 5" /><line x1="12" y1="19" x2="20" y2="19" /></>,
  },
  {
    id: 'settings', labelKey: 'nav.settings',
    icon: <><circle cx="12" cy="12" r="3" /><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z" /></>,
  },
];

function tabFromHash(): Tab {
  const h = location.hash.replace('#', '') as Tab;
  return validTabs.includes(h) ? h : 'dashboard';
}

function App() {
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
  const initialBootIdRef = useRef<string | null>(null);
  const [newVersionAvailable, setNewVersionAvailable] = useState(false);
  const [versionInfo, setVersionInfo] = useState<{ version: string; commit: string; bootId: string } | null>(null);
  const [renaultTfaAutoStart, setRenaultTfaAutoStart] = useState(false);

  const handleRenaultReauth = useCallback(() => {
    setTab('settings');
    setRenaultTfaAutoStart(true);
  }, []);

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

  // Check auth on mount. Only treat 401 as "not authenticated"; transient
  // proxy errors (502/503 during a deploy) and network errors must not kick
  // the user — keep retrying until we get a definitive answer.
  useEffect(() => {
    let cancelled = false;
    const check = async (): Promise<void> => {
      try {
        const r = await fetch('/api/auth/check');
        if (cancelled) return;
        if (r.status === 401) {
          setAuthed(false);
          try { const data = await r.json(); setPasskeysAvailable(data.passkeys === true); } catch { /* ignore */ }
          return;
        }
        if (r.ok) {
          const data = await r.json();
          setAuthed(data.authenticated === true);
          setPasskeysAvailable(data.passkeys === true);
          return;
        }
        // 5xx / non-2xx-non-401 — backend restarting; retry shortly.
        setTimeout(check, 2000);
      } catch {
        if (cancelled) return;
        setTimeout(check, 2000);
      }
    };
    check();
    return () => { cancelled = true; };
  }, []);

  // Record the boot ID this client saw at load time; later mismatches trigger the
  // "new version landed" banner.
  useEffect(() => {
    fetch('/api/version')
      .then((r) => r.json())
      .then((d) => {
        if (typeof d?.bootId === 'string' && d.bootId) {
          initialBootIdRef.current = d.bootId;
        }
        if (d && typeof d.version === 'string') {
          setVersionInfo({ version: d.version, commit: d.commit ?? '', bootId: d.bootId ?? '' });
        }
      })
      .catch(() => {});
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
      if (event === 'bootid') {
        // Boot ID is a plain string, not JSON. If it differs from the one we saw
        // at first page load → server was redeployed → prompt user to reload.
        if (initialBootIdRef.current && data && data !== initialBootIdRef.current) {
          setNewVersionAvailable(true);
        } else if (!initialBootIdRef.current && data) {
          initialBootIdRef.current = data;
        }
        return;
      }
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

  // Safety net: while a mode change is pending, schedule one refetch at
  // (apply-time + 200ms grace). The SSE broadcast fired by applyPendingMode
  // is the normal path; this guarantees the UI catches up even if SSE
  // drops mid-window.
  useEffect(() => {
    if (!status.pendingMode || !status.pendingModeApplyAt) return;
    const applyAt = Date.parse(status.pendingModeApplyAt);
    if (Number.isNaN(applyAt)) return;
    const delay = Math.max(0, applyAt - Date.now()) + 200;
    const id = window.setTimeout(() => {
      apiFetch('/api/status').then((r) => r.json()).then(setStatus).catch(() => {});
    }, delay);
    return () => window.clearTimeout(id);
  }, [status.pendingMode, status.pendingModeApplyAt, apiFetch]);

  const handleModeChange = async (mode: 'off' | 'schedule' | 'force') => {
    // Server debounces 5s before applying. Optimistically mark the click
    // as pending so the chosen button starts pulsing immediately; the
    // applied mode stays on the previously selected button until the
    // status refetch / SSE confirms the server has applied the change.
    setStatus((s) => ({ ...s, pendingMode: mode }));
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
      <DocumentTitle />
      {newVersionAvailable && <VersionUpdateBanner />}
      <Login onLogin={() => setAuthed(true)} passkeysAvailable={passkeysAvailable} />
      <AppFooter versionInfo={versionInfo} />
    </I18nProvider>
  );

  return (
    <I18nProvider locale={locale}>
      <DocumentTitle />
      {newVersionAvailable && <VersionUpdateBanner />}
      {status.renaultTfaRequired && <RenaultReauthBanner onFix={handleRenaultReauth} />}
      <AppContent
        versionInfo={versionInfo}
        tab={tab} setTab={setTab}
        renaultTfaAutoStart={renaultTfaAutoStart}
        onRenaultTfaAutoStartConsumed={() => setRenaultTfaAutoStart(false)}
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
  tab, setTab,
  status, rates, consumption, meterLive, chartMarkers,
  timezone, locale, setLocale,
  pulling, pullDistance, refreshing,
  modeError, refreshKey,
  defaultPowerW, versionInfo,
  renaultTfaAutoStart, onRenaultTfaAutoStartConsumed,
  onModeChange, onScheduleApply, onScheduleCancel, onSlotCancel, onSlotRestore,
  onCreateOverride, onDeleteOverride, onLogout,
}: {
  tab: Tab; setTab: (t: Tab) => void;
  status: StatusResponse; rates: Rate[]; consumption: HourlyEnergy[];
  meterLive: MeterLive | null; chartMarkers: ChartMarker[];
  timezone: string; locale: Locale; setLocale: (l: Locale) => void;
  pulling: boolean; pullDistance: number; refreshing: boolean;
  modeError: string | null; refreshKey: number;
  defaultPowerW: number;
  versionInfo: { version: string; commit: string; bootId: string } | null;
  renaultTfaAutoStart: boolean; onRenaultTfaAutoStartConsumed: () => void;
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
        <h1 onClick={() => setTab('dashboard')}>grosz</h1>
        <nav>
          {TABS.map((ti) => (
            <button key={ti.id} className={tab === ti.id ? 'active' : ''} onClick={() => setTab(ti.id)}>
              {t(ti.labelKey)}
            </button>
          ))}
        </nav>
        <button className="logout-btn" onClick={onLogout}>{t('nav.logout')}</button>
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
                pendingMode={status.pendingMode}
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
        {tab === 'settings' && <Settings refreshKey={refreshKey} locale={locale} onLocaleChange={setLocale} renaultTfaAutoStart={renaultTfaAutoStart} onRenaultTfaAutoStartConsumed={onRenaultTfaAutoStartConsumed} />}
      </main>
      {/* Fixed-position bar: must stay outside <main>, whose pull-to-refresh
          transform would otherwise become its containing block. */}
      <nav className="tab-bar" aria-label={t('nav.menu')}>
        {TABS.map((ti) => (
          <button
            key={ti.id}
            className={`tab-item ${tab === ti.id ? 'active' : ''}`}
            onClick={() => setTab(ti.id)}
            aria-current={tab === ti.id ? 'page' : undefined}
          >
            <svg className="tab-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              {ti.icon}
            </svg>
            <span className="tab-label">{t(ti.labelKey)}</span>
          </button>
        ))}
      </nav>
      <AppFooter versionInfo={versionInfo} />
    </div>
  );
}

function AppFooter({ versionInfo }: { versionInfo: { version: string; commit: string; bootId: string } | null }) {
  const { t } = useTranslation();
  if (!versionInfo) return null;
  const shortBoot = versionInfo.bootId ? versionInfo.bootId.slice(0, 8) : '';
  return (
    <footer className="app-footer">
      grosz — {t('login.subtitle')} {versionInfo.version} ({versionInfo.commit}/{shortBoot})
    </footer>
  );
}

function DocumentTitle() {
  const { t } = useTranslation();
  useEffect(() => {
    document.title = `grosz — ${t('login.subtitle')}`;
  }, [t]);
  return null;
}

export default App;
