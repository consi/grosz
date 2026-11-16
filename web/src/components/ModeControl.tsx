import type { ChargePointInfo } from '../types';
import { useTranslation } from '../i18n';
import { chargePointState, chargerStateLabelKey, chargerStateTone, formatPower } from '../utils/chargerState';

type Mode = 'off' | 'schedule' | 'force';

// Segmented Off / Schedule / Force control. Rendered in the sticky top bar on
// wide screens and at the top of the dashboard on narrower ones.
export function ModeControl({ mode, pendingMode, onModeChange, className = '' }: {
  mode: Mode;
  pendingMode?: Mode;
  onModeChange: (mode: Mode) => void;
  className?: string;
}) {
  const { t } = useTranslation();
  const modes: { value: Mode; label: string }[] = [
    { value: 'off', label: t('charger.modeOff') },
    { value: 'schedule', label: t('charger.modeSchedule') },
    { value: 'force', label: t('charger.modeForce') },
  ];
  return (
    <div className={`mode-toggle ${className}`} role="group" aria-label={t('charger.mode')}>
      {modes.map((m) => {
        const isActive = mode === m.value;
        const isPending = pendingMode != null && pendingMode === m.value && pendingMode !== mode;
        return (
          <button
            key={m.value}
            type="button"
            className={`mode-btn ${m.value} ${isActive ? 'active' : ''} ${isPending ? 'pending' : ''}`}
            aria-pressed={isActive}
            onClick={() => onModeChange(m.value)}
          >
            {m.label}
          </button>
        );
      })}
    </div>
  );
}

// Compact live connector status for the top bar: coloured dot, state label and
// the active charging power.
export function StatusPill({ chargePoints }: { chargePoints: ChargePointInfo[] }) {
  const { t } = useTranslation();
  const cp = chargePoints?.[0];
  if (!cp) return null;
  const state = chargePointState(cp);
  const tone = chargerStateTone[state];
  const label = t(chargerStateLabelKey[state]);
  const power = cp.connectors?.[0]?.measurements?.['Power.Active.Import']?.value ?? 0;
  return (
    <span className={`status-pill tone-${tone}`} title={label} role="status">
      <span className="status-dot" aria-hidden="true" />
      <span className="status-pill-label">{label}</span>
      {state === 'charging' && power > 0 && <span className="status-pill-value">{formatPower(power)}</span>}
    </span>
  );
}
