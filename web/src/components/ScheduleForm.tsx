import { useEffect, useState, type ReactNode } from 'react';
import type { PeriodReason, PlanMode, ProfileStatus, Schedule, ScheduleOverride, SchedulePeriod } from '../types';
import { useTranslation, type TranslationKey } from '../i18n';
import { PeriodEditor } from './PeriodEditor';
import { useNow } from '../hooks/useNow';
import {
  fromZonedInput, minuteNow, nextDeparture, toZonedInput,
  type ChargingWindow, type SaveOverride, type DeleteOverride, type MutationResult,
} from '../utils/schedule';

export interface PlanState {
  mode?: PlanMode;
  soc: number;
  safeSoc: number;
  minSoc: number;
  normalTarget: number;
  socTarget: number;
  tripReadyBy?: string;
  departureTime: string;
  tripActive?: boolean;
  fillActive?: boolean;
}

interface Props {
  schedule?: Schedule;
  overrides: ScheduleOverride[];
  plan: PlanState;
  skipReason?: string;
  skipReasonKey?: string;
  skipReasonParams?: Record<string, string>;
  onApply: () => Promise<MutationResult>;
  onCancel: () => Promise<MutationResult>;
  onCreateOverride: SaveOverride;
  onDeleteOverride: DeleteOverride;
  onTripStart: (target: number, readyBy: string) => Promise<MutationResult>;
  onTripCancel: () => Promise<MutationResult>;
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

const REASON_KEYS: Record<Exclude<PeriodReason, 'manual'>, TranslationKey> = {
  needed: 'schedule.reason.needed',
  cheapest: 'schedule.reason.cheapest',
  trip: 'schedule.reason.trip',
  floor: 'schedule.reason.floor',
  negative: 'schedule.reason.negative',
  cheapFill: 'schedule.reason.cheapFill',
};

// Skip reasons that the plan banner already explains for the matching mode.
const SKIP_IMPLIED_BY: Record<string, PlanMode> = {
  target_reached: 'full',
  user_cancelled: 'paused',
  no_rates: 'noRates',
  waiting_cheaper: 'wait',
};

// Go encodes an unset time.Time as year 1; treat that like a missing value.
const validIso = (iso?: string): iso is string => !!iso && Date.parse(iso) > 0;

function ActionIcon({ children, label, onClick, disabled, danger = false }: {
  children: ReactNode; label: string; onClick: () => void; disabled: boolean; danger?: boolean;
}) {
  return <button className={`schedule-action${danger ? ' danger' : ''}`} title={label} aria-label={label}
    disabled={disabled} onClick={onClick}>
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{children}</svg>
  </button>;
}

export function ScheduleForm({ schedule, overrides, plan, skipReason, skipReasonKey, skipReasonParams,
  onApply, onCancel, onCreateOverride, onDeleteOverride, onTripStart, onTripCancel,
  timezone, sessionActive, refreshing }: Props) {
  const { t, locale } = useTranslation();
  const [editing, setEditing] = useState<{ window: ChargingWindow; id?: number; original?: ChargingWindow } | null>(null);
  const [tripOpen, setTripOpen] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const now = useNow();
  const date = (iso: string) => new Date(iso).toLocaleDateString(locale, {
    timeZone: timezone, weekday: 'short', day: 'numeric', month: 'short',
  });
  const time = (iso: string) => new Date(iso).toLocaleTimeString(locale, {
    timeZone: timezone, hour: '2-digit', minute: '2-digit', hour12: false,
  });
  // Short "when": just the time today, otherwise weekday + time.
  const when = (iso: string) => {
    const day = (d: Date) => d.toLocaleDateString('en-CA', { timeZone: timezone });
    const target = new Date(iso);
    if (day(target) === day(new Date())) return time(iso);
    return `${target.toLocaleDateString(locale, { timeZone: timezone, weekday: 'short' })} ${time(iso)}`;
  };
  const sameWindow = (a: ChargingWindow, b: ChargingWindow) =>
    Date.parse(a.start) === Date.parse(b.start) && Date.parse(a.end) === Date.parse(b.end);
  const slots = schedule?.slots || [];
  // Saved manual windows live in the same list as automatic periods, including
  // during the debounce. Their stable IDs remain the source of edit/delete actions.
  const rows: WindowRow[] = overrides.filter((o) => o.kind === 'force').map((override) => {
    const slot = slots.find((s) => (s.periods || []).some((p) => p.source === 'user_force' && sameWindow(p, override)));
    return { key: `override-${override.id}`, window: override, override,
      cancelledDate: slot?.cancelled ? slot.date : undefined };
  });
  for (const slot of slots) {
    for (const period of slot.periods || []) {
      // Zero-power entries (if the backend ever reports gaps) are not charging windows.
      if (typeof period.power === 'number' && period.power <= 0) continue;
      if (period.source === 'user_force' && (!sessionActive || overrides.some((o) => o.kind === 'force' && sameWindow(o, period)))) continue;
      rows.push({ key: `${slot.date}-${period.start}`, window: period, period,
        cancelledDate: slot.cancelled ? slot.date : undefined });
    }
  }
  rows.sort((a, b) => Date.parse(a.window.start) - Date.parse(b.window.start));
  const activeOverrides = overrides
    .filter((o) => Date.parse(o.end) > now)
    .sort((a, b) => Date.parse(a.start) - Date.parse(b.start));
  const act = async (action: () => Promise<MutationResult>) => {
    setBusy(true);
    setError('');
    try {
      const result = await action();
      if (!result.ok) setError(result.error || t('schedule.overrideError'));
      return result.ok;
    } catch {
      setError(t('schedule.overrideError'));
      return false;
    } finally { setBusy(false); }
  };
  const remove = (row: WindowRow) => {
    void act(() => onCreateOverride({ kind: 'block', start: row.window.start, end: row.window.end }, row.override?.id));
  };
  const add = () => {
    const start = Math.ceil(Date.now() / 3600000) * 3600000;
    setEditing({ window: { start: new Date(start).toISOString(), end: new Date(start + 3600000).toISOString() } });
  };
  const chargeNow = (hours: number) => {
    const start = minuteNow();
    void act(() => onCreateOverride({
      kind: 'force', start: new Date(start).toISOString(), end: new Date(start + hours * 3600000).toISOString(),
    }));
  };
  const departure = nextDeparture(plan.departureTime || '07:00', timezone, new Date(now));
  const skipTonight = () => {
    const start = minuteNow();
    void act(() => onCreateOverride({ kind: 'block', start: new Date(start).toISOString(), end: departure }));
  };
  const tripActive = !!plan.tripActive || validIso(plan.tripReadyBy);
  const profile = useProfileStatus(schedule);
  const profileLimited = !!profile && !profile.inSync && (
    (validIso(profile.backoffUntil) && Date.parse(profile.backoffUntil) > now) || profile.writesLastHour >= profile.writeBudget);

  // One-line explanation of what the planner is doing and why.
  const mode = plan.mode ?? schedule?.mode;
  const readyBy = validIso(schedule?.readyBy) ? schedule.readyBy : undefined;
  const banner = (() => {
    switch (mode) {
      case 'charge': return t('plan.charge', {
        target: Math.round(schedule?.target || plan.normalTarget),
        time: readyBy ? when(readyBy) : plan.departureTime,
      });
      case 'wait': return t('plan.wait', { soc: Math.round(plan.soc), safe: Math.round(plan.safeSoc) });
      case 'trip': {
        const by = validIso(plan.tripReadyBy) ? plan.tripReadyBy : readyBy;
        return t('plan.trip', {
          target: Math.round(plan.socTarget || schedule?.target || 0),
          time: by ? when(by) : plan.departureTime,
        });
      }
      case 'floor': return t('plan.floor', { min: Math.round(plan.minSoc) });
      case 'full': return t('plan.full');
      case 'paused': return t('plan.paused');
      case 'noRates': return t('plan.noRates');
      default: return '';
    }
  })();
  const showSkip = !!skipReason && !(skipReasonKey && mode && SKIP_IMPLIED_BY[skipReasonKey] === mode);
  const skipMessage = skipReasonKey
    ? t(`schedule.skip.${skipReasonKey}` as TranslationKey, skipReasonParams)
    : skipReason || '';
  const paused = skipReasonKey === 'user_cancelled' || mode === 'paused';
  const reasonLabel = (row: WindowRow, manual: boolean) => {
    if (manual) return t('schedule.manual');
    const reason = row.period?.reason;
    if (!reason || reason === 'manual') return t('schedule.automatic');
    const label = t(REASON_KEYS[reason], { soc: Math.round(row.period?.fillSoc ?? 0) });
    return label;
  };

  return <section className="card schedule-card">
    <div className="card-head schedule-title">
      <h2>{t('schedule.heading')}</h2>
      {refreshing && <span className="schedule-updating" role="status">{t('schedule.updating')}</span>}
    </div>
    {banner && <p className={`plan-banner plan-${mode}`} role="status">{banner}</p>}
    {sessionActive && <p className="schedule-notice">{t('schedule.deferred')}</p>}
    {error && <div className="custom-period-error" role="alert">{error}</div>}
    {showSkip && <p className="schedule-notice">
      {t('schedule.automaticSkipped', { reason: skipMessage })}
    </p>}
    {schedule && <div className="schedule-totals" aria-busy={refreshing}>
      <strong className="num">{schedule.energy} <span>kWh</span></strong>
      <span className="num">{t('schedule.estCost', { cost: (schedule.cost ?? 0).toFixed(2) })}</span>
    </div>}
    <div className="schedule-windows">
      {rows.map((row) => {
        const manual = !!row.override || row.period?.source === 'user_force';
        const locked = sessionActive && row.period?.source === 'user_force' && !row.override;
        const reason = manual ? 'manual' : row.period?.reason ?? 'auto';
        return <div key={row.key} className={`schedule-window${manual ? ' manual' : ''}${row.cancelledDate ? ' cancelled' : ''}`}>
          <div className="schedule-window-info">
            <div className="schedule-window-date">{date(row.window.start)} <span className={`schedule-window-kind reason-${reason}`}>{reasonLabel(row, manual)}</span></div>
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
    {activeOverrides.length > 0 && <div className="schedule-overrides">
      <div className="schedule-overrides-title">{t('schedule.overrides')} <span>{activeOverrides.length}</span></div>
      {activeOverrides.map((o) => <div className="schedule-override-row" key={o.id}>
        <span className={`override-kind ${o.kind}`}>{t(o.kind === 'force' ? 'schedule.forced' : 'schedule.blocked')}</span>
        <span className="override-range num">{date(o.start)} · {time(o.start)}–{date(o.start) !== date(o.end) && `${date(o.end)} `}{time(o.end)}</span>
        {o.kind === 'block' && <button className="btn-sm" disabled={busy}
          onClick={() => void act(() => onCreateOverride({kind: 'force', start: o.start, end: o.end}, o.id))}>{t('schedule.restore')}</button>}
        <ActionIcon label={t('schedule.removeOverride')} disabled={busy} danger onClick={() => void act(() => onDeleteOverride(o.id))}>
          <path d="M18 6 6 18M6 6l12 12" />
        </ActionIcon>
      </div>)}
    </div>}
    <div className="schedule-quick" role="group" aria-label={t('schedule.quickActions')}>
      <button className="btn-sm" disabled={busy} onClick={() => chargeNow(1)}>{t('schedule.chargeNow', { hours: 1 })}</button>
      <button className="btn-sm" disabled={busy} onClick={() => chargeNow(2)}>{t('schedule.chargeNow', { hours: 2 })}</button>
      <button className="btn-sm" disabled={busy} onClick={skipTonight} title={t('schedule.skipTonightHint', { time: when(departure) })}>{t('schedule.skipTonight')}</button>
      {tripActive
        ? <button className="btn-sm danger" disabled={busy} onClick={() => void act(onTripCancel)}>{t('trip.cancel')}</button>
        : <button className="btn-sm" disabled={busy} aria-expanded={tripOpen} onClick={() => { setEditing(null); setTripOpen((o) => !o); }}>{t('trip.button')}</button>}
    </div>
    {tripActive && <p className="trip-status">{validIso(plan.tripReadyBy)
      ? t('trip.active', { target: Math.round(plan.socTarget), time: when(plan.tripReadyBy) })
      : t('trip.activeNoTime', { target: Math.round(plan.socTarget) })}</p>}
    {plan.fillActive && !tripActive && <p className="fill-status">{t('plan.fillActive', { target: Math.round(plan.socTarget) })}</p>}
    {tripOpen && !tripActive && <TripForm timezone={timezone} departure={departure}
      onCancel={() => setTripOpen(false)}
      onSave={async (target, by) => {
        const result = await onTripStart(target, by);
        if (result.ok) setTripOpen(false);
        return result;
      }} />}
    {editing ? <PeriodEditor key={`${editing.id ?? 'new'}-${editing.window.start}`} initial={editing.window}
      timezone={timezone} sessionActive={sessionActive} onCancel={() => setEditing(null)}
      onSave={async (window) => {
        const result = await onCreateOverride({ kind: 'force', ...window, original: editing.original }, editing.id);
        if (result.ok) setEditing(null);
        return result;
      }} /> : <div className="schedule-footer">
      <button className="btn-sm" disabled={busy} onClick={() => { setTripOpen(false); add(); }}>{t('schedule.addCustom')}</button>
      {(rows.length > 0 || activeOverrides.length > 0) && <button className="schedule-cancel-all" disabled={busy || sessionActive || refreshing} onClick={() => void act(onCancel)}>{t('schedule.cancelAll')}</button>}
      {(paused || (rows.length === 0 && !skipReason)) && <button className="btn-sm primary" disabled={busy || refreshing} onClick={() => void act(onApply)}>{t(paused ? 'schedule.resumeAutomatic' : 'schedule.optimizeApply')}</button>}
    </div>}
    {profile && <div className="profile-status">
      <span className={`profile-dot${profile.inSync ? ' ok' : ''}`} aria-hidden="true" />
      <span title={t('schedule.profile.writes', { writes: profile.writesLastHour, budget: profile.writeBudget })}>
        {t(profile.inSync ? 'schedule.profile.inSync' : profileLimited ? 'schedule.profile.limited' : 'schedule.profile.pending')}
      </span>
      {profile.paused && <span className="profile-paused">{t('schedule.profile.paused')}</span>}
    </div>}
  </section>;
}

// useProfileStatus loads the charger profile sync state from GET
// /api/schedule (it is not part of the SSE status). It refreshes shortly
// after every schedule update and once a minute.
function useProfileStatus(schedule?: Schedule): ProfileStatus | null {
  const [profile, setProfile] = useState<ProfileStatus | null>(null);
  useEffect(() => {
    let cancelled = false;
    const load = () => {
      fetch('/api/schedule')
        .then((r) => (r.ok ? r.json() : null))
        .then((d) => { if (!cancelled && d?.profile) setProfile(d.profile); })
        .catch(() => {});
    };
    const debounce = window.setTimeout(load, 1500);
    const interval = window.setInterval(load, 60000);
    return () => { cancelled = true; window.clearTimeout(debounce); window.clearInterval(interval); };
  }, [schedule]);
  return profile;
}

function TripForm({ timezone, departure, onSave, onCancel }: {
  timezone: string;
  departure: string;
  onSave: (target: number, readyBy: string) => Promise<MutationResult>;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const [target, setTarget] = useState('100');
  const [readyBy, setReadyBy] = useState(() => toZonedInput(departure, timezone));
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const submit = async () => {
    setError('');
    const pct = Number(target);
    if (!Number.isFinite(pct) || pct < 50 || pct > 100) { setError(t('trip.invalidTarget')); return; }
    let iso: string;
    try {
      iso = fromZonedInput(readyBy, timezone);
      if (Date.parse(iso) <= Date.now()) throw new Error();
    } catch { setError(t('trip.invalidTime')); return; }
    setSaving(true);
    try {
      const result = await onSave(Math.round(pct), iso);
      if (!result.ok) setError(result.error || t('trip.error'));
    } catch { setError(t('trip.error')); }
    finally { setSaving(false); }
  };
  return <div className="custom-period-form trip-form">
    <p className="muted">{t('trip.hint')}</p>
    <div className="trip-fields">
      <label><span>{t('trip.target')}</span><input type="number" inputMode="numeric" min={50} max={100} step={5}
        value={target} onChange={(e) => setTarget(e.target.value)} disabled={saving} /></label>
      <label><span>{t('trip.readyBy')} ({timezone})</span><input type="datetime-local" step={60}
        value={readyBy} onChange={(e) => setReadyBy(e.target.value)} disabled={saving} /></label>
    </div>
    {error && <div className="custom-period-error" role="alert">{error}</div>}
    <div className="custom-period-actions">
      <button className="btn-sm" onClick={onCancel} disabled={saving}>{t('common.cancel')}</button>
      <button className="btn-sm primary" onClick={submit} disabled={saving || !readyBy}>{t('trip.start')}</button>
    </div>
  </div>;
}
