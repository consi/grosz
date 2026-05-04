import { useState } from 'react';
import { useTranslation } from '../i18n';

interface Props {
  soc: number;
  minSoc: number;
  skipAboveSoc: number;
  batteryAutonomy: number;
  chargingStatus: number;
  plugStatus: number;
  chargingRemainingTime: number;
  batteryTimestamp?: string;
  vehicleModel?: string;
  vehiclePicture?: string;
  mileage?: number;
}

function socBarColor(soc: number, minSoc: number, skipAboveSoc: number): string {
  if (minSoc > 0 && soc < minSoc) return 'var(--danger)';
  if (skipAboveSoc > 0 && soc >= skipAboveSoc) return 'var(--primary)';
  return '#ff9800';
}

function near(a: number, b: number): boolean {
  return Math.abs(a - b) < 0.01;
}

export function CarStatus({ soc, minSoc, skipAboveSoc, batteryAutonomy, chargingStatus, plugStatus, chargingRemainingTime, vehicleModel, vehiclePicture, mileage }: Props) {
  const { t } = useTranslation();
  const [imgError, setImgError] = useState(false);
  const barColor = socBarColor(soc, minSoc, skipAboveSoc);
  const hasData = soc > 0 || batteryAutonomy > 0;
  const showImage = vehiclePicture && !imgError;

  const charging = (() => {
    if (near(chargingStatus, 1.0)) return { label: t('car.chargingStatusCharging'), color: 'var(--primary)' };
    if (near(chargingStatus, 0.2)) return { label: t('car.chargingStatusComplete'), color: 'var(--primary)' };
    if (near(chargingStatus, 0.1)) return { label: t('car.chargingStatusScheduled'), color: 'var(--muted)' };
    if (near(chargingStatus, 0.3)) return { label: t('car.chargingStatusWaiting'), color: '#ff9800' };
    if (near(chargingStatus, -1.0)) return { label: t('car.chargingStatusError'), color: 'var(--danger)' };
    if (near(chargingStatus, -1.1)) return { label: t('car.chargingStatusUnavailable'), color: 'var(--muted)' };
    return { label: t('car.chargingStatusNotCharging'), color: 'var(--muted)' };
  })();

  const plug = (() => {
    if (plugStatus === 1) return { label: t('car.plugStatusPluggedIn'), color: 'var(--primary)' };
    if (plugStatus === -1) return { label: t('car.plugStatusError'), color: 'var(--danger)' };
    return { label: t('car.plugStatusUnplugged'), color: 'var(--muted)' };
  })();

  const remaining = (() => {
    if (chargingRemainingTime <= 0) return '';
    const h = Math.floor(chargingRemainingTime / 60);
    const m = chargingRemainingTime % 60;
    if (h === 0) return t('car.remainingMin', { m });
    return m > 0 ? t('car.remainingHourMin', { h, m }) : t('car.remainingHour', { h });
  })();

  return (
    <div className="card">
      <h2>{t('car.heading')}</h2>

      <div className="soc-display">
        <div className="soc-bar-container">
          <div className="soc-bar" style={{ width: `${Math.min(100, Math.max(0, soc))}%`, background: barColor }}>
            <span className="soc-bar-text">{soc.toFixed(0)}%</span>
          </div>
          {minSoc > 0 && <div className="soc-threshold soc-min" style={{ left: `${minSoc}%` }} title={t('car.minSoc', { value: minSoc })} />}
          {skipAboveSoc > 0 && <div className="soc-threshold soc-skip" style={{ left: `${skipAboveSoc}%` }} title={t('car.skipAbove', { value: skipAboveSoc })} />}
        </div>
      </div>

      {hasData ? (
        <div className={`car-status-layout ${showImage ? '' : 'no-image'}`}>
          {showImage && (
            <div className="car-image-section">
              <img
                className="car-image"
                src={vehiclePicture}
                alt={vehicleModel || 'Vehicle'}
                onError={() => setImgError(true)}
              />
              {vehicleModel && <span className="car-model-name">{vehicleModel}</span>}
            </div>
          )}
          <div className="car-details">
            {!showImage && vehicleModel && (
              <div className="car-detail">
                <span className="muted">{t('car.vehicle')}</span>
                <span>{vehicleModel}</span>
              </div>
            )}
            <div className="car-detail">
              <span className="muted">{t('car.range')}</span>
              <span>{batteryAutonomy} km</span>
            </div>
            {mileage != null && mileage > 0 && (
              <div className="car-detail">
                <span className="muted">{t('car.mileage')}</span>
                <span>{Math.round(mileage).toLocaleString()} km</span>
              </div>
            )}
            <div className="car-detail">
              <span className="muted">{t('car.charging')}</span>
              <span style={{ color: charging.color }}>{charging.label}</span>
            </div>
            <div className="car-detail">
              <span className="muted">{t('car.plug')}</span>
              <span style={{ color: plug.color }}>{plug.label}</span>
            </div>
            {near(chargingStatus, 1.0) && remaining && (
              <div className="car-detail">
                <span className="muted">{t('car.remaining')}</span>
                <span>{remaining}</span>
              </div>
            )}
          </div>
        </div>
      ) : (
        <p className="muted">{t('car.noData')}</p>
      )}
    </div>
  );
}
