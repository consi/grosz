import { useState, useEffect, useCallback } from 'react';
import type { MeterLive } from '../types';
import { useTranslation } from '../i18n';

interface PhaseReading {
  timestamp: string;
  phase1W: number;
  phase2W: number;
  phase3W: number;
}

interface Props {
  data: MeterLive | null;
}

export function GridStatus({ data }: Props) {
  const { t } = useTranslation();
  const [history, setHistory] = useState<PhaseReading[]>([]);

  const fetchHistory = useCallback(() => {
    fetch('/api/meter/phases?minutes=60')
      .then((r) => r.json())
      .then((d) => { if (Array.isArray(d)) setHistory(d); })
      .catch(() => {});
  }, []);

  useEffect(() => {
    fetchHistory();
    const interval = setInterval(fetchHistory, 30000);
    return () => clearInterval(interval);
  }, [fetchHistory]);

  if (!data || !data.phases?.length) {
    return (
      <div className="card">
        <h2>{t('grid.heading')}</h2>
        <p className="muted">{t('grid.noData')}</p>
      </div>
    );
  }

  const importing = data.totalPower >= 0;

  const phaseData = [
    history.map((r) => r.phase1W),
    history.map((r) => r.phase2W),
    history.map((r) => r.phase3W),
  ];

  return (
    <div className="card">
      <h2>{t('grid.heading')}</h2>
      <div className="grid-total">
        <div className={`grid-total-power ${importing ? 'importing' : 'exporting'}`}>
          {Math.abs(data.totalPower).toFixed(0)} W
        </div>
        <div className="grid-freq">{data.frequency.toFixed(2)} Hz</div>
      </div>
      <div className="grid-status">
        {data.phases.map((phase, i) => (
          <div key={i} className="grid-phase">
            <Sparkline data={phaseData[i]} />
            <div className="grid-phase-content">
              <div className="grid-phase-title">{t('grid.phase', { n: i + 1 })}</div>
              <div className="grid-value">{phase.power.toFixed(0)} W</div>
              <div className="grid-detail">{phase.voltage.toFixed(1)} V</div>
              <div className="grid-detail">{phase.current.toFixed(2)} A</div>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

function Sparkline({ data }: { data: number[] }) {
  if (data.length < 2) return null;

  const w = 200;
  const h = 60;
  const max = Math.max(...data, 1);
  const step = w / (data.length - 1);

  const points = data.map((v, i) => {
    const x = i * step;
    const y = h - (v / max) * h;
    return `${x},${y}`;
  });

  const areaPath = `M0,${h} L${points.join(' L')} L${w},${h} Z`;
  const linePath = `M${points.join(' L')}`;

  return (
    <svg
      className="sparkline"
      viewBox={`0 0 ${w} ${h}`}
      preserveAspectRatio="none"
    >
      <path d={areaPath} className="sparkline-area" />
      <path d={linePath} className="sparkline-line" />
    </svg>
  );
}
