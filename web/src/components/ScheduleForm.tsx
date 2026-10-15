import { useState, type ReactNode } from 'react';
import type { Schedule, ScheduleOverride, SchedulePeriod } from '../types';
import { useTranslation, type TranslationKey } from '../i18n';
import { PeriodEditor } from './PeriodEditor';
import type { ChargingWindow, SaveOverride, DeleteOverride, MutationResult } from '../utils/schedule';

interface Props {
  schedule?: Schedule;
  overrides: ScheduleOverride[];
  skipReason?: string;
  skipReasonKey?: string;
  skipReasonParams?: Record<string, string>;
  onApply: () => Promise<MutationResult>;
  onCancel: () => Promise<MutationResult>;
  onCreateOverride: SaveOverride;
  onDeleteOverride: DeleteOverride;
  timezone: string;
  sessionActive: boolean;
  refreshing: boolean;
}

type WindowRow = {
  key: string;
  window: ChargingWindow;
  override?: ScheduleOverride;
  period?: SchedulePeriod;
  cancelledDate?: string;
};

function ActionIcon({ children, label, onClick, disabled, danger = false }: {
  children: ReactNode; label: string; onClick: () => void; disabled: boolean; danger?: boolean;
}) {
  return <button className={`schedule-action${danger ? ' danger' : ''}`} title={label} aria-label={label}
    disabled={disabled} onClick={onClick}>
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{children}</svg>
  </button>;
}

export function ScheduleForm({ schedule, overrides, skipReason, skipReasonKey, skipReasonParams,
  onApply, onCancel, onCreateOverride, onDeleteOverride, timezone, sessionActive, refreshing }: Props) {
  const { t, locale } = useTranslation();
  const [editing, setEditing] = useState<{ window: ChargingWindow; id?: number; original?: ChargingWindow } | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const date = (iso: string) => new Date(iso).toLocaleDateString(locale, {
    timeZone: timezone, weekday: 'short', day: 'numeric', month: 'short',
  });
  const time = (iso: string) => new Date(iso).toLocaleTimeString(locale, {
    timeZone: timezone, hour: '2-digit', minute: '2-digit', hour12: false,
  });
  const sameWindow = (a: ChargingWindow, b: ChargingWindow) =>
    Date.parse(a.start) === Date.parse(b.start) && Date.parse(a.end) === Date.parse(b.end);
  const slots = schedule?.slots || [];
  // Saved manual windows live in the same list as automatic periods, including
  // during the debounce. Their stable IDs remain the source of edit/delete actions.
  const rows: WindowRow[] = overrides.filter((o) => o.kind === 'force').map((override) => {
    const slot = slots.find((s) => s.periods.some((p) => p.source === 'user_force' && sameWindow(p, override)));
    return { key: `override-${override.id}`, window: override, override,
      cancelledDate: slot?.cancelled ? slot.date : undefined };
  });
  for (const slot of slots) {
    for (const period of slot.periods) {
      if (period.source === 'user_force' && (!sessionActive || overrides.some((o) => o.kind === 'force' && sameWindow(o, period)))) continue;
      rows.push({ key: `${slot.date}-${period.start}`, window: period, period,
        cancelledDate: slot.cancelled ? slot.date : undefined });
    }
  }
  rows.sort((a, b) => Date.parse(a.window.start) - Date.parse(b.window.start));
  const blocks = overrides.filter((o) => o.kind === 'block');
  const act = async (action: () => Promise<MutationResult>) => {
    setBusy(true);
    setError('');
    try {
      const result = await action();
      if (!result.ok) setError(result.error || t('schedule.overrideError'));
    } catch { setError(t('schedule.overrideError')); }
    finally { setBusy(false); }
  };
  const remove = (row: WindowRow) => {
    void act(() => onCreateOverride({ kind: 'block', start: row.window.start, end: row.window.end }, row.override?.id));
  };
  const add = () => {
    const start = Math.ceil(Date.now() / 3600000) * 3600000;
    setEditing({ window: { start: new Date(start).toISOString(), end: new Date(start + 3600000).toISOString() } });
  };
  const skipMessage = skipReasonKey
    ? t(`schedule.skip.${skipReasonKey}` as TranslationKey, skipReasonParams)
    : skipReason || '';

  return <div className="card schedule-card">
    <div className="schedule-title">
      <h2>{t('schedule.heading')}</h2>
      {refreshing && <span className="schedule-updating" role="status">{t('schedule.updating')}</span>}
    </div>
    {sessionActive && <p className="schedule-notice">{t('schedule.deferred')}</p>}
    {error && <div className="custom-period-error" role="alert">{error}</div>}
    {skipReason && skipReasonKey !== 'covered_by_overrides' && <p className="schedule-notice">
      {t('schedule.automaticSkipped', { reason: skipMessage })}
    </p>}
    {schedule && <div className="schedule-totals" aria-busy={refreshing}>
      <strong>{schedule.energy} <span>kWh</span></strong>
      <span>{t('schedule.estCost', { cost: schedule.cost.toFixed(2) })}</span>
    </div>}
    <div className="schedule-windows">
      {rows.map((row) => {
        const manual = !!row.override || row.period?.source === 'user_force';
        const locked = sessionActive && row.period?.source === 'user_force' && !row.override;
        return <div key={row.key} className={`schedule-window${manual ? ' manual' : ''}${row.cancelledDate ? ' cancelled' : ''}`}>
          <div className="schedule-window-info">
            <div className="schedule-window-date">{date(row.window.start)} <span className="schedule-window-kind">{t(manual ? 'schedule.manual' : 'schedule.automatic')}</span></div>
            <div className="schedule-window-time">{time(row.window.start)}<span>–</span>{date(row.window.start) !== date(row.window.end) && `${date(row.window.end)} `}{time(row.window.end)}</div>
          </div>
          <div className="schedule-window-actions">
            {row.cancelledDate ? <button className="btn-sm" disabled={busy || sessionActive}
              onClick={() => void act(() => onCreateOverride({kind: 'force', start: row.window.start, end: row.window.end}, row.override?.id))}>{t('schedule.restore')}</button> : <>
              <ActionIcon label={t('schedule.edit')} disabled={busy || locked} onClick={() => setEditing({
                window: row.window, id: row.override?.id,
                original: row.override ? undefined : { start: row.window.start, end: row.window.end },
              })}><path d="m16 3 5 5-12 12-6 1 1-6Z" /><path d="m14 5 5 5" /></ActionIcon>
              <ActionIcon label={t('schedule.delete')} disabled={busy || locked} danger onClick={() => remove(row)}>
                <path d="M3 6h18M9 6V3h6v3m-10 0 1 15h12l1-15M10 10v7m4-7v7" />
              </ActionIcon>
              {row.override && <details className="schedule-more">
                <summary aria-label={t('schedule.more')} title={t('schedule.more')}>•••</summary>
                <button disabled={busy} onClick={(event) => {
                  event.currentTarget.closest('details')?.removeAttribute('open');
                  void act(() => onDeleteOverride(row.override!.id));
                }}>{t('schedule.unpin')}</button>
              </details>}
            </>}
          </div>
        </div>;
      })}
    </div>
    {blocks.length > 0 && <details className="schedule-blocks">
      <summary>{t('schedule.blocked')} <span>{blocks.length}</span></summary>
      {blocks.map((block) => <div className="schedule-block-row" key={block.id}>
        <span>{date(block.start)} · {time(block.start)} – {date(block.start) !== date(block.end) && `${date(block.end)} `}{time(block.end)}</span>
        <button className="btn-sm" disabled={busy} onClick={() => void act(() => onCreateOverride({kind: 'force', start: block.start, end: block.end}, block.id))}>{t('schedule.restore')}</button>
      </div>)}
    </details>}
    {editing ? <PeriodEditor key={`${editing.id ?? 'new'}-${editing.window.start}`} initial={editing.window}
      timezone={timezone} sessionActive={sessionActive} onCancel={() => setEditing(null)}
      onSave={async (window) => {
        const result = await onCreateOverride({ kind: 'force', ...window, original: editing.original }, editing.id);
        if (result.ok) setEditing(null);
        return result;
      }} /> : <div className="schedule-footer">
      <button className="btn-sm" disabled={busy} onClick={add}>{t('schedule.addCustom')}</button>
      {(rows.length > 0 || blocks.length > 0) && <button className="schedule-cancel-all" disabled={busy || sessionActive || refreshing} onClick={() => void act(onCancel)}>{t('schedule.cancelAll')}</button>}
      {(skipReasonKey === 'user_cancelled' || (rows.length === 0 && !skipReason)) && <button className="btn-sm primary" disabled={busy || refreshing} onClick={() => void act(onApply)}>{t(skipReasonKey === 'user_cancelled' ? 'schedule.resumeAutomatic' : 'schedule.optimizeApply')}</button>}
    </div>}
  </div>;
}
