import { useState } from 'react';
import { useTranslation } from '../i18n';
import { fromZonedInput, toZonedInput, type ChargingWindow, type MutationResult } from '../utils/schedule';

export function PeriodEditor({ initial, timezone, onSave, onCancel, sessionActive = false }: {
  initial: ChargingWindow;
  timezone: string;
  sessionActive?: boolean;
  onSave: (window: ChargingWindow) => Promise<MutationResult>;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const [start, setStart] = useState(() => toZonedInput(initial.start, timezone));
  const [end, setEnd] = useState(() => toZonedInput(initial.end, timezone));
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const submit = async () => {
    setError('');
    let window: ChargingWindow;
    try {
      window = { start: fromZonedInput(start, timezone, initial.start), end: fromZonedInput(end, timezone, initial.end) };
      if (Date.parse(window.end) <= Date.parse(window.start) || Date.parse(window.end) <= Date.now()) throw new Error();
    } catch { setError(t('schedule.invalidRange')); return; }
    setSaving(true);
    try {
      const result = await onSave(window);
      if (!result.ok) setError(result.error || t('schedule.overrideError'));
    } catch { setError(t('schedule.overrideError')); }
    finally { setSaving(false); }
  };
  return <div className="custom-period-form">
    <label><span>{t('schedule.start')} ({timezone})</span><input type="datetime-local" value={start} onChange={(e) => setStart(e.target.value)} step={60} disabled={saving} /></label>
    <label><span>{t('schedule.end')} ({timezone})</span><input type="datetime-local" value={end} onChange={(e) => setEnd(e.target.value)} step={60} disabled={saving} /></label>
    <p className="muted">{t('schedule.maxPower')} {t(sessionActive ? 'schedule.deferred' : 'schedule.debounce')}</p>
    {error && <div className="custom-period-error" role="alert">{error}</div>}
    <div className="custom-period-actions">
      <button className="btn-sm" onClick={onCancel} disabled={saving}>{t('common.cancel')}</button>
      <button className="btn-sm primary" onClick={submit} disabled={saving || !start || !end}>{t('schedule.save')}</button>
    </div>
  </div>;
}
