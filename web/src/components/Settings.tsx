import { useState, useEffect, useRef } from 'react';
import { PasskeyManager } from './PasskeyManager';
import { useTranslation, locales } from '../i18n';
import type { Locale, TranslationKey } from '../i18n';

interface SettingGroup {
  titleKey: TranslationKey;
  keys: { key: string; labelKey: TranslationKey; type: 'text' | 'number' | 'toggle' | 'select' | 'password'; options?: string[] }[];
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
      { key: 'scheduler.battery_capacity', labelKey: 'settings.batteryCapacity', type: 'number' },
      { key: 'scheduler.charge_headroom', labelKey: 'settings.chargeHeadroom', type: 'number' },
    ],
  },
  {
    titleKey: 'settings.scheduler',
    keys: [
      { key: 'scheduler.enabled', labelKey: 'settings.enabled', type: 'toggle' },
      { key: 'scheduler.target_soc', labelKey: 'settings.targetSoc', type: 'number' },
      { key: 'scheduler.skip_above_soc', labelKey: 'settings.skipAboveSoc', type: 'number' },
      { key: 'scheduler.min_soc', labelKey: 'settings.minSoc', type: 'number' },
      { key: 'scheduler.max_price', labelKey: 'settings.maxPrice', type: 'number' },
      { key: 'scheduler.target_energy', labelKey: 'settings.targetEnergy', type: 'number' },
      { key: 'scheduler.deadline_time', labelKey: 'settings.deadline', type: 'text' },
    ],
  },
  {
    titleKey: 'settings.logging',
    keys: [
      { key: 'log.level', labelKey: 'settings.logLevel', type: 'select', options: ['debug', 'info', 'warn', 'error'] },
    ],
  },
];

export function Settings({ refreshKey, locale, onLocaleChange }: { refreshKey?: number; locale: Locale; onLocaleChange: (l: Locale) => void }) {
  const { t } = useTranslation();
  const [settings, setSettings] = useState<Record<string, string>>({});
  const [dirty, setDirty] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState('');

  useEffect(() => {
    fetch('/api/settings')
      .then((r) => r.json())
      .then(setSettings);
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
      } else {
        setMsg(t('settings.saveFailed'));
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
          {g.keys.map(({ key, labelKey, type, options }) => (
            <label key={key} className="setting-row">
              <span>{t(labelKey)}</span>
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
                  type={type === 'password' ? 'password' : type}
                  value={getValue(key)}
                  onChange={(e) => handleChange(key, e.target.value)}
                  autoComplete={type === 'password' ? 'off' : undefined}
                  placeholder={type === 'password' ? t('settings.unchanged') : undefined}
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
        </fieldset>
        {g.titleKey === 'settings.charger' && (
          <MaintenanceSection cpID={getValue('zappi.charge_box_id')} />
        )}
        </div>
      ))}
      <div className="settings-actions">
        <button className="btn primary" onClick={handleSave} disabled={saving || !Object.keys(dirty).length}>
          {saving ? t('settings.saving') : t('settings.saveChanges')}
        </button>
        {msg && <span className="msg">{msg}</span>}
      </div>
    </div>
  );
}

function MaintenanceSection({ cpID }: { cpID: string }) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState<string | null>(null);
  const [msg, setMsg] = useState<{ kind: 'ok' | 'err'; text: string } | null>(null);
  const [pendingReset, setPendingReset] = useState<'Hard' | 'Soft' | null>(null);
  const resetTimerRef = useRef<number | null>(null);

  const disabled = !cpID;

  const post = async (path: string, body?: unknown) => {
    if (!cpID || busy) return;
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
      } else {
        const text = await resp.text();
        setMsg({ kind: 'err', text: text || t('settings.maintenanceFailed') });
      }
    } catch (e) {
      setMsg({ kind: 'err', text: (e as Error).message });
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

  return (
    <fieldset>
      <legend>{t('settings.maintenance')}</legend>
      {disabled && <div className="msg">{t('settings.maintenanceNoCharger')}</div>}
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
      {msg && (
        <div className="msg" style={{ color: msg.kind === 'err' ? 'var(--danger)' : undefined }}>
          {msg.text}
        </div>
      )}
    </fieldset>
  );
}
