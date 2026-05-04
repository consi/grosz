import { useState } from 'react';
import type { Schedule, SchedulePeriod, ScheduleOverride } from '../types';
import { useTranslation, type TranslationKey } from '../i18n';

interface Props {
  schedule?: Schedule;
  overrides: ScheduleOverride[];
  skipReason?: string;
  skipReasonKey?: string;
  skipReasonParams?: Record<string, string>;
  onApply: () => void;
  onCancel: () => void;
  onSlotCancel: (date: string) => void;
  onSlotRestore: (date: string) => void;
  onCreateOverride: (payload: { kind: 'force' | 'block'; start: string; end: string; powerW: number }) => Promise<{ ok: boolean; error?: string }>;
  onDeleteOverride: (id: number) => Promise<void>;
  timezone: string;
  defaultPowerW: number;
}

function formatTime(iso: string, tz: string, locale: string): string {
  return new Date(iso).toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: tz });
}

function formatDate(iso: string, tz: string, locale: string): string {
  return new Date(iso).toLocaleDateString(locale, { weekday: 'short', day: 'numeric', month: 'short', timeZone: tz });
}

// Convert ISO timestamp → "YYYY-MM-DDTHH:MM" for datetime-local input. Interprets
// the timestamp in the browser's local TZ (which usually matches the user's
// display TZ). For users running the UI in a different TZ than the configured
// display TZ this will mismatch — acceptable for the primary use case (Poland).
function toLocalInputValue(iso: string): string {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// Convert "YYYY-MM-DDTHH:MM" (browser-local) back to an RFC3339 UTC string.
function fromLocalInputValue(local: string): string {
  return new Date(local).toISOString();
}

interface PeriodEditorProps {
  initial: { start: string; end: string; powerW: number };
  defaultPowerW: number;
  onSave: (vals: { start: string; end: string; powerW: number }) => void;
  onCancel: () => void;
}

function PeriodEditor({ initial, defaultPowerW, onSave, onCancel }: PeriodEditorProps) {
  const { t } = useTranslation();
  const [start, setStart] = useState(toLocalInputValue(initial.start));
  const [end, setEnd] = useState(toLocalInputValue(initial.end));
  const [power, setPower] = useState(String(initial.powerW || defaultPowerW));
  const [err, setErr] = useState<string | null>(null);

  const submit = () => {
    setErr(null);
    if (!start || !end) return;
    if (end <= start) {
      setErr(t('schedule.invalidRange'));
      return;
    }
    const p = parseFloat(power);
    if (!isFinite(p) || p <= 0) {
      setErr(t('schedule.power'));
      return;
    }
    onSave({
      start: fromLocalInputValue(start),
      end: fromLocalInputValue(end),
      powerW: p,
    });
  };

  // Open the native picker on input click. Chrome opens it only via the icon
  // by default; calling showPicker() makes the whole field act like a button.
  const openPicker = (e: { currentTarget: HTMLInputElement }) => {
    const el = e.currentTarget as HTMLInputElement & { showPicker?: () => void };
    try { el.showPicker?.(); } catch { /* user-gesture required; ignore */ }
  };

  return (
    <div className="custom-period-form">
      <label>
        <span>{t('schedule.start')}</span>
        <input
          type="datetime-local"
          value={start}
          onChange={(e) => setStart(e.target.value)}
          onClick={openPicker}
          onFocus={openPicker}
          step={300}
        />
      </label>
      <label>
        <span>{t('schedule.end')}</span>
        <input
          type="datetime-local"
          value={end}
          onChange={(e) => setEnd(e.target.value)}
          onClick={openPicker}
          onFocus={openPicker}
          step={300}
        />
      </label>
      <label>
        <span>{t('schedule.power')}</span>
        <input type="number" inputMode="numeric" value={power} onChange={(e) => setPower(e.target.value)} step={100} min={100} />
      </label>
      {err && <div className="custom-period-error">{err}</div>}
      <div className="custom-period-actions">
        <button className="btn-sm" onClick={onCancel}>{t('common.cancel')}</button>
        <button className="btn-sm primary" onClick={submit}>{t('schedule.save')}</button>
      </div>
    </div>
  );
}

// Find an override that matches a force-derived period exactly.
function matchingOverrideFor(period: SchedulePeriod, overrides: ScheduleOverride[]): ScheduleOverride | undefined {
  if (period.source !== 'user_force') return undefined;
  return overrides.find((o) => o.kind === 'force' && o.start === period.start && o.end === period.end);
}

export function ScheduleForm({
  schedule, overrides, skipReason, skipReasonKey, skipReasonParams,
  onApply, onCancel, onSlotCancel, onSlotRestore,
  onCreateOverride, onDeleteOverride, timezone, defaultPowerW,
}: Props) {
  const { t, locale } = useTranslation();
  const activeSlots = schedule?.slots?.filter((s) => !s.cancelled) || [];
  const [editing, setEditing] = useState<{ slotDate?: string; period?: SchedulePeriod } | null>(null);
  const [adding, setAdding] = useState<string | null>(null); // slot date

  const blockOverrides = overrides.filter((o) => o.kind === 'block');

  const blocksForSlot = (slotStart: string, slotEnd: string): ScheduleOverride[] => {
    return blockOverrides.filter((o) => o.start < slotEnd && o.end > slotStart);
  };

  const isActive = (period: { start: string; end: string }): boolean => {
    const now = Date.now();
    return new Date(period.start).getTime() <= now && now < new Date(period.end).getTime();
  };

  const handlePinOrUnpin = async (period: SchedulePeriod) => {
    const existing = matchingOverrideFor(period, overrides);
    if (existing) {
      if (isActive(period) && !confirm(t('schedule.confirmDeleteActive'))) return;
      await onDeleteOverride(existing.id);
    } else {
      const r = await onCreateOverride({ kind: 'force', start: period.start, end: period.end, powerW: period.power });
      if (!r.ok) alert(r.error || t('schedule.overrideError'));
    }
  };

  const handleDelete = async (period: SchedulePeriod) => {
    if (isActive(period) && !confirm(t('schedule.confirmDeleteActive'))) return;
    const existing = matchingOverrideFor(period, overrides);
    if (existing) {
      await onDeleteOverride(existing.id);
      return;
    }
    const r = await onCreateOverride({ kind: 'block', start: period.start, end: period.end, powerW: 0 });
    if (!r.ok) alert(r.error || t('schedule.overrideError'));
  };

  const handleEditSave = async (originalPeriod: SchedulePeriod, vals: { start: string; end: string; powerW: number }) => {
    const existing = matchingOverrideFor(originalPeriod, overrides);
    if (existing) await onDeleteOverride(existing.id);
    const r = await onCreateOverride({ kind: 'force', ...vals });
    if (!r.ok) {
      alert(r.error || t('schedule.overrideError'));
      // best-effort restore previous override
      if (existing) {
        await onCreateOverride({ kind: 'force', start: existing.start, end: existing.end, powerW: existing.powerW });
      }
      return;
    }
    setEditing(null);
  };

  const handleAddSave = async (vals: { start: string; end: string; powerW: number }) => {
    const r = await onCreateOverride({ kind: 'force', ...vals });
    if (!r.ok) {
      alert(r.error || t('schedule.overrideError'));
      return;
    }
    setAdding(null);
  };

  const renderSlot = (slot: typeof activeSlots[number]) => {
    const blocks = blocksForSlot(slot.deadline, slot.deadline);
    return (
      <div key={slot.date} className={`slot-card ${slot.cancelled ? 'cancelled' : ''}`}>
        <div className="slot-header">
          <span className="slot-date">{t('schedule.by', { date: formatDate(slot.deadline, timezone, locale), time: formatTime(slot.deadline, timezone, locale) })}</span>
          <span className="slot-meta">{slot.energy} kWh · {slot.cost.toFixed(2)} PLN</span>
          {!slot.cancelled ? (
            <button className="btn-sm danger" onClick={() => onSlotCancel(slot.date)}>{t('common.cancel')}</button>
          ) : (
            <button className="btn-sm" onClick={() => onSlotRestore(slot.date)}>{t('schedule.restore')}</button>
          )}
        </div>
        {!slot.cancelled && (
          <>
            <div className="slot-periods">
              {slot.periods.map((p, i) => {
                const pinned = p.source === 'user_force';
                const isEditing = editing?.slotDate === slot.date && editing.period?.start === p.start;
                if (isEditing) {
                  return (
                    <PeriodEditor
                      key={i}
                      initial={{ start: p.start, end: p.end, powerW: p.power }}
                      defaultPowerW={defaultPowerW}
                      onSave={(vals) => handleEditSave(p, vals)}
                      onCancel={() => setEditing(null)}
                    />
                  );
                }
                return (
                  <span key={i} className={`slot-period ${pinned ? 'pinned' : ''}`}>
                    <span className="slot-period-time">
                      {pinned && <span className="pin-badge" title={t('schedule.pinned')}>📌</span>}
                      {formatTime(p.start, timezone, locale)}-{formatTime(p.end, timezone, locale)}
                      <span className="muted"> {p.price.toFixed(3)}</span>
                    </span>
                    <span className="slot-period-actions">
                      <button className="icon-btn" title={pinned ? t('schedule.unpin') : t('schedule.pin')} onClick={() => handlePinOrUnpin(p)}>{pinned ? '⊘' : '📌'}</button>
                      <button className="icon-btn" title={t('schedule.edit')} onClick={() => setEditing({ slotDate: slot.date, period: p })}>✎</button>
                      <button className="icon-btn danger" title={t('schedule.delete')} onClick={() => handleDelete(p)}>×</button>
                    </span>
                  </span>
                );
              })}
            </div>

            {blocks.length > 0 && (
              <div className="slot-blocks">
                <span className="slot-blocks-label">{t('schedule.blocked')}:</span>
                {blocks.map((b) => (
                  <span key={b.id} className="block-pill">
                    {formatTime(b.start, timezone, locale)}-{formatTime(b.end, timezone, locale)}
                    <button className="icon-btn" title={t('common.delete')} onClick={() => onDeleteOverride(b.id)}>×</button>
                  </span>
                ))}
              </div>
            )}

            {adding === slot.date ? (
              <PeriodEditor
                initial={{ start: slot.deadline, end: slot.deadline, powerW: defaultPowerW }}
                defaultPowerW={defaultPowerW}
                onSave={handleAddSave}
                onCancel={() => setAdding(null)}
              />
            ) : (
              <button className="btn-sm" onClick={() => setAdding(slot.date)}>{t('schedule.addCustom')}</button>
            )}
          </>
        )}
      </div>
    );
  };

  // Block overrides that don't fall in any active slot — surface them so the user can remove them.
  const orphanBlocks = blockOverrides.filter((b) => {
    return !activeSlots.some((slot) => b.start < slot.deadline && b.end > slot.deadline);
  });

  return (
    <div className="card">
      <h2>{t('schedule.heading')}</h2>

      {skipReason && !schedule && (
        <div className="skip-reason">
          {t('schedule.skipped', { reason: skipReasonKey ? t(`schedule.skip.${skipReasonKey}` as TranslationKey, skipReasonParams) : (skipReason || '') })}
        </div>
      )}

      {schedule && schedule.slots?.length > 0 ? (
        <div className="active-schedule">
          <div className="schedule-summary">
            <span><strong>{t('schedule.summary', { energy: schedule.energy, days: activeSlots.length })}</strong></span>
            <span>{t('schedule.estCost', { cost: schedule.cost.toFixed(2) })}</span>
          </div>

          <div className="slot-list">
            {schedule.slots.map(renderSlot)}
          </div>

          {orphanBlocks.length > 0 && (
            <div className="slot-blocks orphan">
              <span className="slot-blocks-label">{t('schedule.blocked')}:</span>
              {orphanBlocks.map((b) => (
                <span key={b.id} className="block-pill">
                  {formatDate(b.start, timezone, locale)} {formatTime(b.start, timezone, locale)}-{formatTime(b.end, timezone, locale)}
                  <button className="icon-btn" title={t('common.delete')} onClick={() => onDeleteOverride(b.id)}>×</button>
                </span>
              ))}
            </div>
          )}

          <button className="btn danger" onClick={onCancel} style={{ marginTop: '0.75rem' }}>
            {t('schedule.cancelAll')}
          </button>
        </div>
      ) : (
        <div className="schedule-form">
          {!skipReason && (
            <button className="btn primary" onClick={onApply}>
              {t('schedule.optimizeApply')}
            </button>
          )}
          {orphanBlocks.length > 0 && (
            <div className="slot-blocks orphan">
              <span className="slot-blocks-label">{t('schedule.blocked')}:</span>
              {orphanBlocks.map((b) => (
                <span key={b.id} className="block-pill">
                  {formatDate(b.start, timezone, locale)} {formatTime(b.start, timezone, locale)}-{formatTime(b.end, timezone, locale)}
                  <button className="icon-btn" title={t('common.delete')} onClick={() => onDeleteOverride(b.id)}>×</button>
                </span>
              ))}
            </div>
          )}
          {/* Always allow adding an override even when schedule is skipped/empty */}
          {adding === '__none__' ? (
            <PeriodEditor
              initial={{
                start: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
                end: new Date(Date.now() + 2 * 60 * 60 * 1000).toISOString(),
                powerW: defaultPowerW,
              }}
              defaultPowerW={defaultPowerW}
              onSave={handleAddSave}
              onCancel={() => setAdding(null)}
            />
          ) : (
            <button className="btn-sm" onClick={() => setAdding('__none__')} style={{ marginTop: '0.75rem' }}>
              {t('schedule.addCustom')}
            </button>
          )}
        </div>
      )}
    </div>
  );
}
