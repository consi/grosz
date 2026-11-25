import { useEffect, useRef, useState } from 'react';
import { useTranslation } from '../i18n';
import { CameraPreview } from './CameraPreview';

const SOC_MIN = 30;
const SOC_MAX = 100;
const SOC_STEP = 5;
const SOC_DEBOUNCE_MS = 2500;
const SOC_DEFAULT = 80;

interface Props {
  soc: number;
  minSoc: number;
  safeSoc: number;
  socTarget: number;
  batteryAutonomy: number;
  chargingStatus: number;
  plugStatus: number;
  chargingRemainingTime: number;
  batteryTimestamp?: string;
  vehicleModel?: string;
  vehiclePicture?: string;
  mileage?: number;
  // A camera stream is configured: show it live inside the card.
  cameraEnabled?: boolean;
  onSocTargetChange: (target: number) => Promise<boolean>;
}

// clampLimit normalises the car's reported/fallback target into the editable
// range. An unknown/0 value defaults to a sensible 80%; out-of-range values are
// pinned to [SOC_MIN, SOC_MAX].
function clampLimit(v: number): number {
  const n = Math.round(v);
  if (!n || n <= 0) return SOC_DEFAULT;
  return Math.min(SOC_MAX, Math.max(SOC_MIN, n));
}

function socBarColor(soc: number, minSoc: number, safeSoc: number): string {
  if (minSoc > 0 && soc < minSoc) return 'var(--danger)';
  if (safeSoc > 0 && soc >= safeSoc) return 'var(--primary)';
  return 'var(--warning)';
}

function near(a: number, b: number): boolean {
  return Math.abs(a - b) < 0.01;
}

export function CarStatus({ soc, minSoc, safeSoc, socTarget, batteryAutonomy, chargingStatus, plugStatus, chargingRemainingTime, vehicleModel, vehiclePicture, mileage, cameraEnabled, onSocTargetChange }: Props) {
  const { t } = useTranslation();
  const [imgError, setImgError] = useState(false);
  const barColor = socBarColor(soc, minSoc, safeSoc);
  const hasData = soc > 0 || batteryAutonomy > 0;
  const showImage = vehiclePicture && !imgError;

  const charging = (() => {
    if (near(chargingStatus, 1.0)) return { label: t('car.chargingStatusCharging'), color: 'var(--primary)' };
    if (near(chargingStatus, 0.2)) return { label: t('car.chargingStatusComplete'), color: 'var(--primary)' };
    if (near(chargingStatus, 0.1)) return { label: t('car.chargingStatusScheduled'), color: 'var(--muted)' };
    if (near(chargingStatus, 0.3)) return { label: t('car.chargingStatusWaiting'), color: 'var(--warning)' };
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
    <section className={`card car-status-card ${cameraEnabled ? 'has-camera' : ''}`}>
      <div className="card-head">
        <h2>{t('car.heading')}</h2>
        {hasData && <span className="card-aside car-soc num">{soc.toFixed(0)}%</span>}
      </div>

      <div className="soc-bar-container" role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(soc)} aria-valuetext={`${soc.toFixed(0)}%`} aria-label="SoC">
        <div className="soc-bar" style={{ width: `${Math.min(100, Math.max(0, soc))}%`, background: barColor }} />
        {minSoc > 0 && <div className="soc-threshold soc-min" style={{ left: `${minSoc}%` }} title={t('car.minSoc', { value: minSoc })} />}
        {safeSoc > 0 && <div className="soc-threshold soc-skip" style={{ left: `${safeSoc}%` }} title={t('car.safeSoc', { value: safeSoc })} />}
      </div>

      <div className="car-body">
        {cameraEnabled && <CameraPreview />}
        <div className="car-info">
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
              <dl className="stat-list car-details">
                {/* The model is named under the picture; this row stands in
                    wherever the picture is not shown. */}
                {vehicleModel && (
                  <div className={showImage ? 'car-vehicle-row with-image' : 'car-vehicle-row'}>
                    <dt>{t('car.vehicle')}</dt><dd>{vehicleModel}</dd>
                  </div>
                )}
                <div><dt>{t('car.range')}</dt><dd className="num">{batteryAutonomy} km</dd></div>
                {mileage != null && mileage > 0 && (
                  <div><dt>{t('car.mileage')}</dt><dd className="num">{Math.round(mileage).toLocaleString()} km</dd></div>
                )}
                <div><dt>{t('car.charging')}</dt><dd style={{ color: charging.color }}>{charging.label}</dd></div>
                <div><dt>{t('car.plug')}</dt><dd style={{ color: plug.color }}>{plug.label}</dd></div>
                {near(chargingStatus, 1.0) && remaining && (
                  <div><dt>{t('car.remaining')}</dt><dd className="num">{remaining}</dd></div>
                )}
              </dl>
            </div>
          ) : (
            <p className="muted empty">{t('car.noData')}</p>
          )}

          {hasData && <SocLimitControl socTarget={socTarget} onChange={onSocTargetChange} />}
        </div>
      </div>
    </section>
  );
}

// SocLimitControl is the +/- charge-limit stepper pinned to the bottom of the
// Car Status card. It edits locally for instant feedback and debounces the write
// to the car (via onChange) so rapid taps coalesce into a single API call.
function SocLimitControl({ socTarget, onChange }: { socTarget: number; onChange: (target: number) => Promise<boolean> }) {
  const { t } = useTranslation();
  const [text, setText] = useState<string>(() => String(clampLimit(socTarget)));
  const [saveState, setSaveState] = useState<'idle' | 'saving' | 'saved' | 'error'>('idle');
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const hintRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const dirtyRef = useRef(false); // user is mid-edit; don't clobber with prop syncs
  const lastSentRef = useRef<number>(clampLimit(socTarget));

  // Reflect the live car value when it changes, but only while the user isn't
  // actively editing, so a background status refresh can't yank the input.
  useEffect(() => {
    if (!dirtyRef.current) {
      const next = clampLimit(socTarget);
      setText(String(next));
      lastSentRef.current = next;
    }
  }, [socTarget]);

  // Clear timers on unmount.
  useEffect(() => () => {
    if (debounceRef.current) clearTimeout(debounceRef.current);
    if (hintRef.current) clearTimeout(hintRef.current);
  }, []);

  const scheduleSave = (clamped: number) => {
    if (debounceRef.current) clearTimeout(debounceRef.current);
    if (hintRef.current) clearTimeout(hintRef.current);
    dirtyRef.current = true;
    setSaveState('idle');
    debounceRef.current = setTimeout(() => {
      if (clamped === lastSentRef.current) {
        dirtyRef.current = false;
        return;
      }
      lastSentRef.current = clamped;
      setSaveState('saving');
      onChange(clamped).then((ok) => {
        setSaveState(ok ? 'saved' : 'error');
        dirtyRef.current = !ok; // keep the user's value visible if it failed
        hintRef.current = setTimeout(() => setSaveState('idle'), ok ? 2000 : 8000);
      });
    }, SOC_DEBOUNCE_MS);
  };

  const step = (delta: number) => {
    const base = parseInt(text, 10);
    const current = isNaN(base) ? lastSentRef.current : base;
    const next = Math.min(SOC_MAX, Math.max(SOC_MIN, current + delta));
    setText(String(next));
    scheduleSave(next);
  };

  const onInput = (raw: string) => {
    setText(raw);
    const n = parseInt(raw, 10);
    if (!isNaN(n) && n >= SOC_MIN && n <= SOC_MAX) scheduleSave(n);
  };

  const onBlur = () => {
    const n = parseInt(text, 10);
    const clamped = isNaN(n) ? lastSentRef.current : Math.min(SOC_MAX, Math.max(SOC_MIN, n));
    setText(String(clamped));
    scheduleSave(clamped);
  };

  const hint = (() => {
    if (saveState === 'saving') return t('car.chargeLimitSaving');
    if (saveState === 'saved') return t('car.chargeLimitSaved');
    if (saveState === 'error') return t('car.chargeLimitError');
    return '';
  })();

  return (
    <div className="soc-limit-control">
      <div className="soc-limit-label">
        <span className="muted">{t('car.chargeLimit')}</span>
        {hint && <span className={`soc-limit-hint ${saveState === 'error' ? 'error' : ''}`}>{hint}</span>}
      </div>
      <div className="soc-limit-stepper">
        <button
          type="button"
          className="soc-limit-btn"
          aria-label={`-${SOC_STEP}%`}
          onClick={() => step(-SOC_STEP)}
          disabled={parseInt(text, 10) <= SOC_MIN}
        >
          −
        </button>
        <div className="soc-limit-value">
          <input
            type="number"
            inputMode="numeric"
            aria-label={t('car.chargeLimit')}
            value={text}
            min={SOC_MIN}
            max={SOC_MAX}
            step={SOC_STEP}
            onChange={(e) => onInput(e.target.value)}
            onBlur={onBlur}
          />
          <span className="soc-limit-unit">%</span>
        </div>
        <button
          type="button"
          className="soc-limit-btn"
          aria-label={`+${SOC_STEP}%`}
          onClick={() => step(SOC_STEP)}
          disabled={parseInt(text, 10) >= SOC_MAX}
        >
          +
        </button>
      </div>
    </div>
  );
}
