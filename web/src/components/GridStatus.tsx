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
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);

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

  const hoverReading = hoverIdx != null ? history[hoverIdx] : null;

  const phaseValues: [number, number, number] = [
    hoverReading ? hoverReading.phase1W : data.phases[0]?.power ?? 0,
    hoverReading ? hoverReading.phase2W : data.phases[1]?.power ?? 0,
    hoverReading ? hoverReading.phase3W : data.phases[2]?.power ?? 0,
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
            <Sparkline
              data={phaseData[i]}
              hoverIdx={hoverIdx}
              onHoverChange={setHoverIdx}
            />
            <div className="grid-phase-content">
              <div className="grid-phase-title">{t('grid.phase', { n: i + 1 })}</div>
              <div className="grid-value">{phaseValues[i].toFixed(0)} W</div>
              <div className="grid-detail">{phase.voltage.toFixed(1)} V</div>
              <div className="grid-detail">{phase.current.toFixed(2)} A</div>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

interface SparklineProps {
  data: number[];
  hoverIdx: number | null;
  onHoverChange: (idx: number | null) => void;
}

function Sparkline({ data, hoverIdx, onHoverChange }: SparklineProps) {
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

  const updateFromPointer = (e: React.PointerEvent<SVGSVGElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    if (rect.width <= 0) return;
    const x = e.clientX - rect.left;
    const ratio = Math.max(0, Math.min(1, x / rect.width));
    const idx = Math.round(ratio * (data.length - 1));
    onHoverChange(idx);
  };

  const handlePointerDown = (e: React.PointerEvent<SVGSVGElement>) => {
    try { e.currentTarget.setPointerCapture(e.pointerId); } catch { /* ignore */ }
    updateFromPointer(e);
  };

  const handlePointerUp = (e: React.PointerEvent<SVGSVGElement>) => {
    try { e.currentTarget.releasePointerCapture(e.pointerId); } catch { /* ignore */ }
    if (e.pointerType !== 'mouse') onHoverChange(null);
  };

  const validIdx = hoverIdx != null && hoverIdx >= 0 && hoverIdx < data.length ? hoverIdx : null;
  const cursorX = validIdx != null ? validIdx * step : 0;
  const cursorY = validIdx != null ? h - (data[validIdx] / max) * h : 0;

  return (
    <svg
      className="sparkline"
      viewBox={`0 0 ${w} ${h}`}
      preserveAspectRatio="none"
      style={{ touchAction: 'pan-y' }}
      onPointerMove={updateFromPointer}
      onPointerDown={handlePointerDown}
      onPointerUp={handlePointerUp}
      onPointerLeave={() => onHoverChange(null)}
      onPointerCancel={() => onHoverChange(null)}
    >
      <path d={areaPath} className="sparkline-area" />
      <path d={linePath} className="sparkline-line" />
      {validIdx != null && (
        <>
          <line
            x1={cursorX}
            x2={cursorX}
            y1={0}
            y2={h}
            className="sparkline-cursor"
          />
          <circle
            cx={cursorX}
            cy={cursorY}
            r={2}
            className="sparkline-cursor-dot"
            vectorEffect="non-scaling-stroke"
          />
        </>
      )}
    </svg>
  );
}
