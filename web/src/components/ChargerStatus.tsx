import type { ChargePointInfo, Schedule } from '../types';
import { useTranslation } from '../i18n';

interface Props {
  chargePoints: ChargePointInfo[];
  schedule?: Schedule;
  charging: boolean;
  mode: 'off' | 'schedule' | 'force';
  onModeChange: (mode: 'off' | 'schedule' | 'force') => void;
  error?: string | null;
}

type CarState = 'unplugged' | 'plugged' | 'charging' | 'suspended' | 'error' | 'offline';

function deriveCarState(status: string): CarState {
  switch (status) {
    case 'Charging': return 'charging';
    case 'Preparing':
    case 'Finishing': return 'plugged';
    case 'SuspendedEV':
    case 'SuspendedEVSE': return 'suspended';
    case 'Faulted': return 'error';
    case 'Unavailable': return 'offline';
    default: return 'unplugged';
  }
}

const carStateColors: Record<CarState, string> = {
  unplugged: '#9e9e9e',
  plugged: '#ff9800',
  charging: '#4caf50',
  suspended: '#9c27b0',
  error: '#f44336',
  offline: '#616161',
};

const carStateIcons: Record<CarState, string> = {
  unplugged: '🔌',
  plugged: '🔋',
  charging: '⚡',
  suspended: '⏸',
  error: '⚠',
  offline: '◯',
};

function formatPower(watts: number): string {
  if (watts >= 1000) return `${(watts / 1000).toFixed(2)} kW`;
  return `${watts.toFixed(0)} W`;
}

export function ChargerStatus({ chargePoints, schedule, charging, mode, onModeChange, error }: Props) {
  const { t } = useTranslation();

  const carStateLabelKeys: Record<CarState, Parameters<typeof t>[0]> = {
    unplugged: 'charger.stateUnplugged',
    plugged: 'charger.statePlugged',
    charging: 'charger.stateCharging',
    suspended: 'charger.statePaused',
    error: 'charger.stateFault',
    offline: 'charger.stateUnavailable',
  };

  const modes = [
    { value: 'off' as const, label: t('charger.modeOff') },
    { value: 'schedule' as const, label: t('charger.modeSchedule') },
    { value: 'force' as const, label: t('charger.modeForce') },
  ];

  return (
    <div className="card">
      <div className="charger-top">
        <h2>{t('charger.heading')}</h2>
        <div className="mode-toggle">
          {modes.map((m) => (
            <button
              key={m.value}
              className={`mode-btn ${m.value} ${mode === m.value ? 'active' : ''}`}
              onClick={() => onModeChange(m.value)}
            >
              {m.label}
            </button>
          ))}
        </div>
      </div>
      {error && <div className="mode-error">{error}</div>}

      {!chargePoints?.length ? (
        <p className="muted">{t('charger.noChargers')}</p>
      ) : (
        chargePoints.map((cp) => {
          const conn = cp.connectors?.[0];
          const state = conn ? deriveCarState(conn.status) : (cp.connected ? 'unplugged' : 'offline');
          const color = carStateColors[state];
          const power = conn?.measurements?.['Power.Active.Import']?.value ?? 0;
          const energy = conn?.measurements?.['Energy.Active.Import.Register']?.value;
          const voltage = conn?.measurements?.['Voltage']?.value;

          return (
            <div key={cp.id} className="charger">
              <div className="charger-header">
                <span className={`dot ${cp.connected ? 'online' : 'offline'}`} />
                <strong>{cp.id}</strong>
                {cp.vendor && <span className="muted"> {cp.vendor} {cp.model}</span>}
              </div>

              <div className="car-state" style={{ borderLeftColor: color }}>
                <div className="car-state-main">
                  <span className="car-state-icon">{carStateIcons[state]}</span>
                  <span className="car-state-label" style={{ color }}>{t(carStateLabelKeys[state])}</span>
                  {state === 'charging' && (
                    <span className="car-state-power">{formatPower(power)}</span>
                  )}
                </div>

                {(state === 'charging' || state === 'plugged' || state === 'suspended') && conn?.measurements && (
                  <div className="car-state-details">
                    {state === 'charging' && power > 0 && (
                      <div className="car-detail">
                        <span className="muted">{t('charger.power')}</span>
                        <span>{formatPower(power)}</span>
                      </div>
                    )}
                    {energy != null && (
                      <div className="car-detail">
                        <span className="muted">{t('charger.meter')}</span>
                        <span>{(energy / 1000).toFixed(2)} kWh</span>
                      </div>
                    )}
                    {voltage != null && (
                      <div className="car-detail">
                        <span className="muted">{t('charger.voltage')}</span>
                        <span>{voltage.toFixed(0)} V</span>
                      </div>
                    )}
                    {conn.measurements['Current.Import'] && (
                      <div className="car-detail">
                        <span className="muted">{t('charger.current')}</span>
                        <span>{conn.measurements['Current.Import'].value.toFixed(1)} A</span>
                      </div>
                    )}
                    {conn.measurements['Current.Offered'] && (
                      <div className="car-detail">
                        <span className="muted">{t('charger.offered')}</span>
                        <span>{conn.measurements['Current.Offered'].value.toFixed(1)} A</span>
                      </div>
                    )}
                  </div>
                )}
              </div>

              {charging && schedule && (
                <div className="schedule-summary">
                  <span>{t('charger.scheduled', { energy: schedule.energy })}</span>
                  <span>{t('charger.estCost', { cost: schedule.cost.toFixed(2) })}</span>
                </div>
              )}
            </div>
          );
        })
      )}
    </div>
  );
}
