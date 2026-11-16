import type { ChargePointInfo } from '../types';
import type { TranslationKey } from '../i18n';

// Presentation-level state of a connector. SuspendedEVSE is the charger
// holding power back (e.g. a gap between scheduled windows), which is a normal
// paused state rather than a fault; SuspendedEV is the car pausing itself.
export type ChargerState =
  | 'unplugged' | 'plugged' | 'charging' | 'pausedGap' | 'pausedEV' | 'error' | 'offline';

// Tone drives the shared status colour tokens (--st-*) in App.css.
export type StatusTone = 'charging' | 'ready' | 'paused' | 'idle' | 'error' | 'offline';

export function deriveChargerState(status: string | undefined): ChargerState {
  switch (status) {
    case 'Charging': return 'charging';
    case 'Preparing':
    case 'Finishing': return 'plugged';
    case 'SuspendedEVSE': return 'pausedGap';
    case 'SuspendedEV': return 'pausedEV';
    case 'Faulted': return 'error';
    case 'Unavailable': return 'offline';
    default: return 'unplugged';
  }
}

export const chargerStateTone: Record<ChargerState, StatusTone> = {
  unplugged: 'idle',
  plugged: 'ready',
  charging: 'charging',
  pausedGap: 'paused',
  pausedEV: 'paused',
  error: 'error',
  offline: 'offline',
};

export const chargerStateLabelKey: Record<ChargerState, TranslationKey> = {
  unplugged: 'charger.stateUnplugged',
  plugged: 'charger.statePlugged',
  charging: 'charger.stateCharging',
  pausedGap: 'charger.statePausedGap',
  pausedEV: 'charger.statePaused',
  error: 'charger.stateFault',
  offline: 'charger.stateUnavailable',
};

export function chargePointState(cp: ChargePointInfo): ChargerState {
  const conn = cp.connectors?.[0];
  if (conn) return deriveChargerState(conn.status);
  return cp.connected ? 'unplugged' : 'offline';
}

export function formatPower(watts: number): string {
  if (Math.abs(watts) >= 1000) return `${(watts / 1000).toFixed(2)} kW`;
  return `${watts.toFixed(0)} W`;
}
