import type { ChargePointInfo, Schedule } from '../types';
import { useTranslation } from '../i18n';
import { chargePointState, chargerStateLabelKey, chargerStateTone, formatPower, type ChargerState } from '../utils/chargerState';

interface Props {
  chargePoints: ChargePointInfo[];
  schedule?: Schedule;
  charging: boolean;
}

const stateIcons: Record<ChargerState, string> = {
  unplugged: 'M9 2v6m6-6v6M6 8h12v3a6 6 0 0 1-12 0zm6 9v5',
  plugged: 'M9 2v6m6-6v6M6 8h12v3a6 6 0 0 1-12 0zm6 9v5',
  charging: 'M13 2 3 14h9l-1 8 10-12h-9z',
  pausedGap: 'M9 5v14m6-14v14',
  pausedEV: 'M9 5v14m6-14v14',
  error: 'M12 3 2 21h20zm0 6v5m0 3v.01',
  offline: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z',
};

export function ChargerStatus({ chargePoints, schedule, charging }: Props) {
  const { t } = useTranslation();

  return (
    <section className="card charger-card">
      <div className="card-head">
        <h2>{t('charger.heading')}</h2>
      </div>

      {!chargePoints?.length ? (
        <p className="muted empty">{t('charger.noChargers')}</p>
      ) : (
        chargePoints.map((cp) => {
          const conn = cp.connectors?.[0];
          const state = chargePointState(cp);
          const tone = chargerStateTone[state];
          const m = conn?.measurements;
          const power = m?.['Power.Active.Import']?.value ?? 0;
          const energy = m?.['Energy.Active.Import.Register']?.value;
          const voltage = m?.['Voltage']?.value;
          const current = m?.['Current.Import']?.value;
          const offered = m?.['Current.Offered']?.value;
          const showDetails = (state === 'charging' || state === 'plugged' || state === 'pausedGap' || state === 'pausedEV') && !!m;

          return (
            <div key={cp.id} className="charger">
              <div className="charger-id">
                <span className={`dot ${cp.connected ? 'online' : 'offline'}`} aria-hidden="true" />
                <strong>{cp.id}</strong>
                {cp.vendor && <span className="muted">{cp.vendor} {cp.model}</span>}
              </div>

              <div className={`charger-state tone-${tone}`}>
                <svg className="charger-state-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d={stateIcons[state]} />
                </svg>
                <span className="charger-state-label">{t(chargerStateLabelKey[state])}</span>
                {state === 'charging' && <span className="charger-state-power num">{formatPower(power)}</span>}
              </div>

              {showDetails && (
                <dl className="stat-list">
                  {state === 'charging' && power > 0 && (
                    <div><dt>{t('charger.power')}</dt><dd className="num">{formatPower(power)}</dd></div>
                  )}
                  {energy != null && (
                    <div><dt>{t('charger.meter')}</dt><dd className="num">{(energy / 1000).toFixed(2)} kWh</dd></div>
                  )}
                  {voltage != null && (
                    <div><dt>{t('charger.voltage')}</dt><dd className="num">{voltage.toFixed(0)} V</dd></div>
                  )}
                  {current != null && (
                    <div><dt>{t('charger.current')}</dt><dd className="num">{current.toFixed(1)} A</dd></div>
                  )}
                  {offered != null && (
                    <div><dt>{t('charger.offered')}</dt><dd className="num">{offered.toFixed(1)} A</dd></div>
                  )}
                </dl>
              )}

              {charging && schedule && (
                <div className="charger-plan num">
                  <span>{t('charger.scheduled', { energy: schedule.energy })}</span>
                  <span>{t('charger.estCost', { cost: schedule.cost.toFixed(2) })}</span>
                </div>
              )}
            </div>
          );
        })
      )}
    </section>
  );
}
