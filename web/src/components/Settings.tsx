import { useState, useEffect, useRef } from 'react';
import { PasskeyManager } from './PasskeyManager';
import { OcppConsole } from './OcppConsole';
import { useTranslation, locales } from '../i18n';
import type { Locale, TranslationKey } from '../i18n';
import type { ChargePointInfo, RenaultTfaStatus } from '../types';

interface SettingGroup {
  titleKey: TranslationKey;
  keys: SettingField[];
}

interface SettingField {
  key: string;
  labelKey: TranslationKey;
  type: 'text' | 'number' | 'toggle' | 'select' | 'password' | 'time';
  options?: string[];
  helpKey?: TranslationKey;
  placeholder?: string;
  min?: number;
  max?: number;
  step?: number;
}

const timezones: string[] = (() => {
  try {
    return Intl.supportedValuesOf('timeZone');
  } catch {
    return [
      'UTC', 'Europe/Warsaw', 'Europe/London', 'Europe/Berlin', 'Europe/Paris',
      'Europe/Rome', 'Europe/Madrid', 'Europe/Amsterdam', 'Europe/Prague',
      'Europe/Stockholm', 'Europe/Helsinki', 'Europe/Bucharest', 'Europe/Athens',
      'America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles',
      'Asia/Tokyo', 'Asia/Shanghai', 'Australia/Sydney',
    ];
  }
})();

const groups: SettingGroup[] = [
  {
    titleKey: 'settings.display',
    keys: [
      { key: 'display.timezone', labelKey: 'settings.timezone', type: 'select', options: timezones },
    ],
  },
  {
    titleKey: 'settings.userManagement',
    keys: [
      { key: 'auth.username', labelKey: 'settings.username', type: 'text' },
      { key: 'auth.password', labelKey: 'settings.password', type: 'password' },
      { key: 'auth.session_lifetime_days', labelKey: 'settings.sessionLifetimeDays', type: 'number' },
    ],
  },
  {
    titleKey: 'settings.ocppServer',
    keys: [
      { key: 'ocpp.auth_key', labelKey: 'settings.authKey', type: 'password' },
    ],
  },
  {
    titleKey: 'settings.charger',
    keys: [
      { key: 'zappi.charge_box_id', labelKey: 'settings.chargeBoxId', type: 'text' },
      { key: 'zappi.charger_name', labelKey: 'settings.displayName', type: 'text' },
      { key: 'zappi.qr_url', labelKey: 'settings.qrCodeUrl', type: 'text' },
      { key: 'charger.max_power', labelKey: 'settings.maxPower', type: 'number' },
      { key: 'charger.min_power', labelKey: 'settings.minPower', type: 'number' },
      { key: 'charger.phases', labelKey: 'settings.phases', type: 'select', options: ['1', '3'] },
      { key: 'zappi.id_tag', labelKey: 'settings.idTag', type: 'text' },
      { key: 'zappi.meter_interval', labelKey: 'settings.meterInterval', type: 'number' },
      { key: 'charger.status_check_minutes', labelKey: 'settings.statusCheckMinutes', type: 'number' },
    ],
  },
  {
    titleKey: 'settings.tariff',
    keys: [
      { key: 'tariff.pstryk_token', labelKey: 'settings.pstrykToken', type: 'password' },
    ],
  },
  {
    titleKey: 'settings.abrp',
    keys: [
      { key: 'abrp.token', labelKey: 'settings.abrpToken', type: 'password' },
    ],
  },
  {
    titleKey: 'settings.energyMeter',
    keys: [
      { key: 'meter.url', labelKey: 'settings.meterUrl', type: 'text' },
      { key: 'meter.interval', labelKey: 'settings.pollInterval', type: 'number' },
    ],
  },
  {
    titleKey: 'settings.vehicle',
    keys: [
      { key: 'vehicle.renault_user', labelKey: 'settings.renaultEmail', type: 'text' },
      { key: 'vehicle.renault_password', labelKey: 'settings.renaultPassword', type: 'password' },
      { key: 'vehicle.vin', labelKey: 'settings.vin', type: 'text' },
      { key: 'vehicle.poll_interval', labelKey: 'settings.socPollInterval', type: 'number' },
      { key: 'vehicle.require_plug_check', labelKey: 'settings.requirePlugCheck', type: 'toggle' },
    ],
  },
  {
    titleKey: 'settings.camera',
    keys: [
      { key: 'camera.rtsp_url', labelKey: 'settings.cameraUrl', helpKey: 'settings.cameraUrlHelp', type: 'text', placeholder: 'rtsp://192.168.1.50:554/stream1' },
      { key: 'camera.rtsp_username', labelKey: 'settings.cameraUsername', type: 'text' },
      { key: 'camera.rtsp_password', labelKey: 'settings.cameraPassword', helpKey: 'settings.cameraPasswordHelp', type: 'password' },
    ],
  },
  {
    titleKey: 'settings.planning',
    keys: [
      { key: 'scheduler.enabled', labelKey: 'settings.enabled', type: 'toggle' },
      { key: 'scheduler.target_soc', labelKey: 'settings.targetSoc', helpKey: 'settings.targetSocHelp', type: 'number', min: 50, max: 100, step: 5 },
      { key: 'scheduler.departure_time', labelKey: 'settings.departureTime', helpKey: 'settings.departureTimeHelp', type: 'time' },
      { key: 'scheduler.safe_soc', labelKey: 'settings.safeSoc', helpKey: 'settings.safeSocHelp', type: 'number', min: 0, max: 100, step: 5 },
      { key: 'scheduler.min_soc', labelKey: 'settings.minSoc', helpKey: 'settings.minSocHelp', type: 'number', min: 0, max: 100, step: 5 },
      { key: 'scheduler.max_price', labelKey: 'settings.maxPrice', helpKey: 'settings.maxPriceHelp', type: 'number', min: 0, step: 0.01 },
      { key: 'scheduler.cheap_fill_price', labelKey: 'settings.cheapFillPrice', helpKey: 'settings.cheapFillPriceHelp', type: 'number', min: 0, step: 0.01 },
      { key: 'scheduler.cheap_fill_soc', labelKey: 'settings.cheapFillSoc', helpKey: 'settings.cheapFillSocHelp', type: 'number', min: 50, max: 100, step: 5 },
      { key: 'scheduler.battery_capacity', labelKey: 'settings.batteryCapacity', helpKey: 'settings.batteryCapacityHelp', type: 'number', min: 1, step: 0.1 },
    ],
  },
  {
    titleKey: 'settings.logging',
    keys: [
      { key: 'log.level', labelKey: 'settings.logLevel', type: 'select', options: ['debug', 'info', 'warn', 'error'] },
    ],
  },
];

export function Settings({ refreshKey, locale, onLocaleChange, renaultTfaAutoStart, onRenaultTfaAutoStartConsumed, chargePoints = [], timezone }: {
  refreshKey?: number; locale: Locale; onLocaleChange: (l: Locale) => void; renaultTfaAutoStart?: boolean; onRenaultTfaAutoStartConsumed?: () => void;
  chargePoints?: ChargePointInfo[]; timezone: string;
}) {
  const { t } = useTranslation();
  const [settings, setSettings] = useState<Record<string, string>>({});
  const [dirty, setDirty] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState('');

  const loadSettings = () => fetch('/api/settings')
    .then((r) => r.json())
    .then(setSettings);

  useEffect(() => {
    void loadSettings();
  }, [refreshKey]);

  const handleChange = (key: string, value: string) => {
    setDirty((d) => ({ ...d, [key]: value }));
  };

  const getValue = (key: string) => dirty[key] ?? settings[key] ?? '';

  const handleSave = async () => {
    if (!Object.keys(dirty).length) return;
    setSaving(true);
    setMsg('');
    try {
      const resp = await fetch('/api/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(dirty),
      });
      if (resp.ok) {
        setSettings((s) => ({ ...s, ...dirty }));
        setDirty({});
        setMsg(t('settings.saved'));
        // The server may normalise what it stores (for example it moves
        // credentials out of the camera URL), so show its version.
        void loadSettings().catch(() => {});
      } else {
        // Validation errors name a translation key for their message.
        const body = await resp.json().catch(() => null) as { errorKey?: string } | null;
        const errorKey = body?.errorKey as TranslationKey | undefined;
        setMsg(errorKey && t(errorKey) !== errorKey ? t(errorKey) : t('settings.saveFailed'));
      }
    } finally {
      setSaving(false);
    }
  };

  const handleLocaleChange = (newLocale: string) => {
    onLocaleChange(newLocale as Locale);
    handleChange('display.language', newLocale);
  };

  return (
    <div className="card settings">
      <h2>{t('settings.heading')}</h2>
      <div className="settings-grid">
      <fieldset>
        <legend>{t('passkey.heading')}</legend>
        <PasskeyManager />
      </fieldset>
      {groups.map((g, gi) => (
        <div key={gi}>
        <fieldset>
          <legend>{t(g.titleKey)}</legend>
          {gi === 0 && (
            <label className="setting-row">
              <span>{t('settings.language')}</span>
              <select value={locale} onChange={(e) => handleLocaleChange(e.target.value)}>
                {locales.map((l) => <option key={l.code} value={l.code}>{l.name}</option>)}
              </select>
            </label>
          )}
          {g.keys.map(({ key, labelKey, type, options, helpKey, placeholder, min, max, step }) => (
            <label key={key} className="setting-row">
              <span>{t(labelKey)}{helpKey && <small className="setting-help">{t(helpKey)}</small>}</span>
              {type === 'toggle' ? (
                <input
                  type="checkbox"
                  checked={getValue(key) === 'true'}
                  onChange={(e) => handleChange(key, e.target.checked ? 'true' : 'false')}
                />
              ) : type === 'select' ? (
                <select value={getValue(key)} onChange={(e) => handleChange(key, e.target.value)}>
                  {options?.map((o) => <option key={o} value={o}>{o}</option>)}
                </select>
              ) : (
                <input
                  type={type}
                  min={min}
                  max={max}
                  step={step}
                  inputMode={type === 'number' ? 'decimal' : undefined}
                  value={getValue(key)}
                  onChange={(e) => handleChange(key, e.target.value)}
                  autoComplete={type === 'password' ? 'off' : undefined}
                  placeholder={type === 'password' ? t('settings.unchanged') : placeholder}
                  autoCapitalize={placeholder ? 'none' : undefined}
                  spellCheck={placeholder ? false : undefined}
                />
              )}
              {key === 'display.timezone' && (
                <button
                  className="btn-page"
                  type="button"
                  onClick={() => handleChange('display.timezone', Intl.DateTimeFormat().resolvedOptions().timeZone)}
                >
                  {t('settings.autoDetect')}
                </button>
              )}
            </label>
          ))}
          {g.titleKey === 'settings.vehicle' && (
            <RenaultTfaSection
              refreshKey={refreshKey}
              autoStart={renaultTfaAutoStart}
              onAutoStartConsumed={onRenaultTfaAutoStartConsumed}
            />
          )}
        </fieldset>
        {g.titleKey === 'settings.charger' && (
          <MaintenanceSection
            cpID={getValue('zappi.charge_box_id')}
            maintenanceMode={getValue('zappi.maintenance_mode') === 'true'}
            onMaintenanceModeChange={(enabled) => {
              setSettings((s) => ({ ...s, 'zappi.maintenance_mode': enabled ? 'true' : 'false' }));
            }}
            chargePoints={chargePoints}
            timezone={timezone}
          />
        )}
        </div>
      ))}
      </div>
      <div className="settings-actions">
        <button className="btn primary" onClick={handleSave} disabled={saving || !Object.keys(dirty).length}>
          {saving ? t('settings.saving') : t('settings.saveChanges')}
        </button>
        {msg && <span className="msg">{msg}</span>}
      </div>
    </div>
  );
}

function MaintenanceSection({ cpID, maintenanceMode, onMaintenanceModeChange, chargePoints, timezone }: {
  cpID: string;
  maintenanceMode: boolean;
  onMaintenanceModeChange: (enabled: boolean) => void;
  chargePoints: ChargePointInfo[];
  timezone: string;
}) {
  const { t } = useTranslation();
  const [consoleOpen, setConsoleOpen] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [msg, setMsg] = useState<{ kind: 'ok' | 'err'; text: string } | null>(null);
  const [pendingReset, setPendingReset] = useState<'Hard' | 'Soft' | null>(null);
  const resetTimerRef = useRef<number | null>(null);

  const disabled = !cpID;

  const post = async (path: string, body?: unknown): Promise<boolean> => {
    if (!cpID || busy) return false;
    setBusy(path);
    setMsg(null);
    try {
      const resp = await fetch(`/api/charger/${encodeURIComponent(cpID)}${path}`, {
        method: 'POST',
        headers: body ? { 'Content-Type': 'application/json' } : undefined,
        body: body ? JSON.stringify(body) : undefined,
      });
      if (resp.ok) {
        setMsg({ kind: 'ok', text: t('settings.maintenanceSent') });
        return true;
      } else {
        const text = await resp.text();
        setMsg({ kind: 'err', text: text || t('settings.maintenanceFailed') });
        return false;
      }
    } catch (e) {
      setMsg({ kind: 'err', text: (e as Error).message });
      return false;
    } finally {
      setBusy(null);
    }
  };

  const armReset = (type: 'Hard' | 'Soft') => {
    if (resetTimerRef.current != null) {
      window.clearTimeout(resetTimerRef.current);
    }
    setPendingReset(type);
    resetTimerRef.current = window.setTimeout(() => {
      setPendingReset((cur) => (cur === type ? null : cur));
      resetTimerRef.current = null;
    }, 5000);
  };

  const confirmReset = async (type: 'Hard' | 'Soft') => {
    if (resetTimerRef.current != null) {
      window.clearTimeout(resetTimerRef.current);
      resetTimerRef.current = null;
    }
    setPendingReset(null);
    await post('/reset', { type });
  };

  const handleReset = (type: 'Hard' | 'Soft') => {
    if (pendingReset === type) {
      confirmReset(type);
    } else {
      armReset(type);
    }
  };

  const toggleMaintenanceMode = async () => {
    const enabled = !maintenanceMode;
    if (await post('/maintenance-mode', { enabled })) {
      onMaintenanceModeChange(enabled);
    }
  };

  return (
    <fieldset>
      <legend>{t('settings.maintenance')}</legend>
      {disabled && <div className="msg">{t('settings.maintenanceNoCharger')}</div>}
      <div className="maint-grid">
      <div className="setting-row">
        <span>{t('settings.maintenanceMode')}</span>
        <button
          className={`btn btn-sm ${maintenanceMode ? 'danger' : 'primary'}`}
          disabled={disabled || busy !== null}
          onClick={toggleMaintenanceMode}
        >
          {busy === '/maintenance-mode'
            ? t('settings.maintenanceSending')
            : maintenanceMode
              ? t('settings.disableMaintenanceMode')
              : t('settings.enableMaintenanceMode')}
        </button>
      </div>
      <div className="setting-row">
        <span>{t('console.open')}</span>
        <button className="btn primary btn-sm" aria-haspopup="dialog" onClick={() => setConsoleOpen(true)}>
          {t('console.openButton')}
        </button>
      </div>
      <div className="setting-row">
        <span>{t('settings.updateFirmware')}</span>
        <button
          className="btn primary btn-sm"
          disabled={disabled || busy !== null}
          onClick={() => post('/update-firmware')}
        >
          {busy === '/update-firmware' ? t('settings.maintenanceSending') : t('settings.updateFirmware')}
        </button>
      </div>
      <div className="setting-row">
        <span>{t('settings.clearCache')}</span>
        <button
          className="btn primary btn-sm"
          disabled={disabled || busy !== null}
          onClick={() => post('/clear-cache')}
        >
          {busy === '/clear-cache' ? t('settings.maintenanceSending') : t('settings.clearCache')}
        </button>
      </div>
      <div className="setting-row">
        <span>{t('settings.softReset')}</span>
        <button
          className={`btn btn-sm ${pendingReset === 'Soft' ? 'danger' : 'primary'}`}
          disabled={disabled || busy !== null}
          onClick={() => handleReset('Soft')}
        >
          {pendingReset === 'Soft' ? t('settings.confirmAction') : t('settings.softReset')}
        </button>
      </div>
      <div className="setting-row">
        <span>{t('settings.hardReset')}</span>
        <button
          className={`btn btn-sm ${pendingReset === 'Hard' ? 'danger' : 'primary'}`}
          disabled={disabled || busy !== null}
          onClick={() => handleReset('Hard')}
        >
          {pendingReset === 'Hard' ? t('settings.confirmAction') : t('settings.hardReset')}
        </button>
      </div>
      </div>
      {msg && (
        <div className="msg" style={{ color: msg.kind === 'err' ? 'var(--danger)' : undefined }}>
          {msg.text}
        </div>
      )}
      <OcppConsole open={consoleOpen} onClose={() => setConsoleOpen(false)} timezone={timezone}
        chargePoints={chargePoints} defaultCpId={cpID || undefined} />
    </fieldset>
  );
}

function RenaultTfaSection({ refreshKey, autoStart, onAutoStartConsumed }: { refreshKey?: number; autoStart?: boolean; onAutoStartConsumed?: () => void }) {
  const { t } = useTranslation();
  const [status, setStatus] = useState<RenaultTfaStatus | null>(null);
  const [phase, setPhase] = useState<'idle' | 'sending' | 'codeSent' | 'verifying'>('idle');
  const [code, setCode] = useState('');
  const [msg, setMsg] = useState<{ kind: 'ok' | 'err'; text: string } | null>(null);
  const autoStartedRef = useRef(false);

  const loadStatus = () => {
    fetch('/api/renault/tfa/status')
      .then((r) => (r.ok ? r.json() : null))
      .then((s: RenaultTfaStatus | null) => { if (s) setStatus(s); })
      .catch(() => {});
  };

  useEffect(loadStatus, [refreshKey]);

  const sendCode = async () => {
    setPhase('sending');
    setMsg(null);
    try {
      const resp = await fetch('/api/renault/tfa/start', { method: 'POST' });
      if (!resp.ok) {
        const text = await resp.text();
        setMsg({ kind: 'err', text: text.trim() || t('renault.tfaSendCode') });
        setPhase('idle');
        return;
      }
      const data = await resp.json();
      if (data.alreadyAuthenticated) {
        setMsg({ kind: 'ok', text: t('renault.tfaAlreadyAuthed') });
        setPhase('idle');
        loadStatus();
        return;
      }
      setPhase('codeSent');
      setMsg({ kind: 'ok', text: t('renault.tfaCodeSent', { email: data.obfuscatedEmail || '' }) });
    } catch (e) {
      setMsg({ kind: 'err', text: (e as Error).message });
      setPhase('idle');
    }
  };

  const verify = async () => {
    const trimmed = code.trim();
    if (!trimmed) return;
    setPhase('verifying');
    setMsg(null);
    try {
      const resp = await fetch('/api/renault/tfa/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: trimmed }),
      });
      if (!resp.ok) {
        const text = await resp.text();
        setMsg({ kind: 'err', text: text.trim() || t('renault.tfaVerify') });
        setPhase('codeSent');
        return;
      }
      setMsg({ kind: 'ok', text: t('renault.tfaSuccess') });
      setCode('');
      setPhase('idle');
      loadStatus();
    } catch (e) {
      setMsg({ kind: 'err', text: (e as Error).message });
      setPhase('codeSent');
    }
  };

  // When the user arrives via the re-auth banner, scroll to the verification
  // controls and kick off the code request once.
  useEffect(() => {
    if (!autoStart) return;
    document.getElementById('renault-verification')?.scrollIntoView({ behavior: 'smooth', block: 'center' });
    if (status == null) return; // wait until we know whether Renault is configured
    if (!autoStartedRef.current && status.configured) {
      autoStartedRef.current = true;
      sendCode();
    }
    onAutoStartConsumed?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoStart, status]);

  const configured = status?.configured ?? false;
  const busy = phase === 'sending' || phase === 'verifying';

  return (
    <div id="renault-verification" className="renault-tfa">
      <div className="setting-subheading">{t('renault.tfa')}</div>
      {!configured ? (
        <div className="msg">{t('renault.tfaNotConfigured')}</div>
      ) : (
        <>
          <div className="setting-row">
            <span>
              {status?.required
                ? t('renault.tfaRequired')
                : status?.completedAt
                  ? t('renault.tfaLastVerified', { date: new Date(status.completedAt).toLocaleString() })
                  : t('renault.tfaVerified')}
            </span>
            <button
              className="btn primary btn-sm"
              disabled={busy}
              onClick={sendCode}
            >
              {phase === 'sending'
                ? t('renault.tfaSending')
                : phase === 'codeSent' || phase === 'verifying'
                  ? t('renault.tfaResend')
                  : t('renault.tfaSendCode')}
            </button>
          </div>
          {(phase === 'codeSent' || phase === 'verifying') && (
            <div className="setting-row">
              <input
                type="text"
                inputMode="numeric"
                autoComplete="one-time-code"
                maxLength={8}
                placeholder={t('renault.tfaCodePlaceholder')}
                value={code}
                onChange={(e) => setCode(e.target.value)}
              />
              <button
                className="btn primary btn-sm"
                disabled={phase === 'verifying' || !code.trim()}
                onClick={verify}
              >
                {phase === 'verifying' ? t('renault.tfaVerifying') : t('renault.tfaVerify')}
              </button>
            </div>
          )}
        </>
      )}
      {msg && (
        <div className="msg" style={{ color: msg.kind === 'err' ? 'var(--danger)' : undefined }}>
          {msg.text}
        </div>
      )}
    </div>
  );
}
