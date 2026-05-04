import { useState, useRef, useCallback } from 'react';
import type { Rate, Schedule, HourlyEnergy } from '../types';
import { useTranslation } from '../i18n';

export interface ChartMarker {
  time: string; // ISO timestamp
  type: 'start' | 'stop' | 'plug' | 'unplug';
}

interface Props {
  rates: Rate[];
  schedule?: Schedule;
  consumption?: HourlyEnergy[];
  markers?: ChartMarker[];
  timezone: string;
}

function formatEnergy(wh: number): string {
  if (wh >= 1000) return `${(wh / 1000).toFixed(2)} kWh`;
  return `${wh.toFixed(0)} Wh`;
}

function formatDate(d: Date, tz: string, locale: string): string {
  return d.toLocaleDateString(locale, { day: 'numeric', month: 'short', timeZone: tz });
}

export function PriceChart({ rates, schedule, consumption, markers, timezone }: Props) {
  const { t, locale } = useTranslation();
  const [hover, setHover] = useState<{ idx: number; x: number; y: number } | null>(null);
  const chartRef = useRef<HTMLDivElement>(null);
  const [chartWidth, setChartWidth] = useState(0);
  const roRef = useRef<ResizeObserver>(null);

  const setChartRef = useCallback((node: HTMLDivElement | null) => {
    chartRef.current = node;
    if (roRef.current) roRef.current.disconnect();
    if (node) {
      setChartWidth(node.offsetWidth);
      roRef.current = new ResizeObserver((entries) => {
        for (const e of entries) setChartWidth(e.contentRect.width);
      });
      roRef.current.observe(node);
    }
  }, []);

  if (!rates?.length) {
    return (
      <div className="card">
        <h2>{t('price.heading')}</h2>
        <p className="muted">{t('price.noData')}</p>
      </div>
    );
  }

  const prices = rates.map((r) => r.price);
  const maxPrice = Math.max(...prices);
  const minPrice = Math.min(...prices);
  const avgPrice = prices.reduce((a, b) => a + b, 0) / prices.length;
  const now = new Date();

  const hasNegative = minPrice < 0;
  const scaleMax = Math.max(maxPrice, 0);
  const scaleMin = Math.min(minPrice, 0);
  const scaleRange = scaleMax - scaleMin || 1;
  const zeroPct = (scaleMax / scaleRange) * 100;

  // Collect scheduled periods from all active (non-cancelled) slots as
  // [startMs, endMs) intervals, so a tariff hour is "scheduled" if any
  // period overlaps it — including sub-hour custom force overrides.
  const scheduledRanges: { start: number; end: number }[] =
    schedule?.slots
      ?.filter((s) => !s.cancelled)
      .flatMap((s) => s.periods)
      .filter((p) => p.power > 0)
      .map((p) => ({ start: new Date(p.start).getTime(), end: new Date(p.end).getTime() })) || [];

  const isScheduledHour = (rateStartMs: number, rateEndMs: number): boolean => {
    for (const r of scheduledRanges) {
      if (r.start < rateEndMs && r.end > rateStartMs) return true;
    }
    return false;
  };

  // Build consumption lookup keyed by local hour start
  const consumptionMap = new Map<number, number>();
  let maxWh = 0;
  const toHourKey = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate(), d.getHours()).getTime();
  if (consumption?.length) {
    for (const c of consumption) {
      consumptionMap.set(toHourKey(new Date(c.hour)), c.energyWh);
      if (c.energyWh > maxWh) maxWh = c.energyWh;
    }
  }
  const hasConsumption = maxWh > 0;

  const useKwh = maxWh >= 1000;
  const maxDisplay = useKwh ? maxWh / 1000 : maxWh;
  const energyUnit = useKwh ? 'kWh' : 'Wh';

  const barCount = rates.length;
  const chartHeight = 360;

  // Build consumption line — only connect points that have data
  const consumptionPoints: { x: number; y: number; wh: number }[] = [];
  if (hasConsumption) {
    rates.forEach((r, i) => {
      const wh = consumptionMap.get(toHourKey(new Date(r.start))) ?? 0;
      if (wh > 0) {
        const xLeft = (i / barCount) * 100;
        const xRight = ((i + 1) / barCount) * 100;
        const y = chartHeight - (wh / maxWh) * (chartHeight * 0.85);
        consumptionPoints.push({ x: xLeft, y, wh });
        consumptionPoints.push({ x: xRight, y, wh });
      }
    });
  }

  // Schedule background ranges — group consecutive scheduled tariff hours.
  const scheduleRanges: { startIdx: number; endIdx: number }[] = [];
  if (scheduledRanges.length > 0) {
    let rangeStart = -1;
    for (let i = 0; i < rates.length; i++) {
      const rs = new Date(rates[i].start).getTime();
      const re = new Date(rates[i].end).getTime();
      if (isScheduledHour(rs, re)) {
        if (rangeStart === -1) rangeStart = i;
      } else {
        if (rangeStart !== -1) {
          scheduleRanges.push({ startIdx: rangeStart, endIdx: i - 1 });
          rangeStart = -1;
        }
      }
    }
    if (rangeStart !== -1) {
      scheduleRanges.push({ startIdx: rangeStart, endIdx: rates.length - 1 });
    }
  }

  // X-axis labels: hours + date on day boundaries
  const xLabels: { idx: number; hour: string; date?: string }[] = [];
  let lastDate = '';
  rates.forEach((r, i) => {
    const d = new Date(r.start);
    const hour = d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: timezone }).slice(0, 2);
    const dateStr = formatDate(d, timezone, locale);
    const showDate = dateStr !== lastDate;
    if (showDate) lastDate = dateStr;
    xLabels.push({ idx: i, hour, date: showDate ? dateStr : undefined });
  });

  // Hover data
  const hoverRate = hover !== null ? rates[hover.idx] : null;
  const hoverStart = hoverRate ? new Date(hoverRate.start) : null;
  const hoverWh = hoverRate ? consumptionMap.get(toHourKey(new Date(hoverRate.start))) : undefined;
  const hoverCost = hoverWh !== undefined && hoverRate ? (hoverRate.price * hoverWh / 1000) : undefined;
  const hoverIsScheduled = hoverRate
    ? isScheduledHour(new Date(hoverRate.start).getTime(), new Date(hoverRate.end).getTime())
    : false;

  const handleMouseMove = (e: React.MouseEvent) => {
    const area = chartRef.current;
    if (!area) return;
    const rect = area.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    const idx = Math.floor((x / rect.width) * barCount);
    if (idx >= 0 && idx < barCount) {
      setHover({ idx, x, y });
    }
  };

  return (
    <div className="card">
      <h2>{t('price.heading')}</h2>
      <div className="price-stats">
        <span>{t('price.min', { value: minPrice.toFixed(3) })}</span>
        <span>{t('price.avg', { value: avgPrice.toFixed(3) })}</span>
        <span>{t('price.max', { value: maxPrice.toFixed(3) })}</span>
        <span className="muted">PLN/kWh</span>
      </div>

      <div className="chart-wrapper">
        <div className={`y-axis y-axis-left${hasNegative ? ' y-axis-neg' : ''}`}>
          {hasNegative ? (
            <>
              <span style={{ position: 'absolute', top: 0 }}>{scaleMax.toFixed(2)}</span>
              <span style={{ position: 'absolute', top: `${zeroPct}%`, transform: 'translateY(-50%)' }}>0</span>
              <span style={{ position: 'absolute', bottom: 0 }}>{scaleMin.toFixed(2)}</span>
            </>
          ) : (
            <>
              <span>{maxPrice.toFixed(2)}</span>
              <span>{(maxPrice / 2).toFixed(2)}</span>
              <span className="muted">PLN</span>
            </>
          )}
        </div>

        <div
          className="chart-area"
          ref={setChartRef}
          onMouseMove={handleMouseMove}
          onMouseLeave={() => setHover(null)}
        >
          {/* Schedule background areas */}
          {scheduleRanges.map((range, ri) => {
            const left = (range.startIdx / barCount) * 100;
            const width = ((range.endIdx - range.startIdx + 1) / barCount) * 100;
            return (
              <div
                key={ri}
                className="schedule-bg"
                style={{ left: `${left}%`, width: `${width}%` }}
              />
            );
          })}

          {hasNegative && (
            <div className="zero-line" style={{ top: `${zeroPct}%` }} />
          )}

          <div className="chart">
            {rates.map((r, i) => {
              const start = new Date(r.start);
              const height = (Math.abs(r.price) / scaleRange) * 100;
              const isNeg = r.price < 0;
              const isCurrent = now >= start && now < new Date(r.end);
              const priceLevel =
                r.price <= avgPrice * 0.7 ? 'cheap' :
                r.price >= avgPrice * 1.3 ? 'expensive' : 'normal';

              const barStyle: React.CSSProperties = isNeg
                ? { top: `${zeroPct}%`, height: `${Math.max(height, 0.5)}%` }
                : { bottom: `${100 - zeroPct}%`, height: `${Math.max(height, 0.5)}%` };

              return (
                <div key={i} className="bar-slot">
                  <div
                    className={`bar ${priceLevel} ${isCurrent ? 'current' : ''} ${isNeg ? 'negative' : ''} ${hover?.idx === i ? 'hovered' : ''}`}
                    style={barStyle}
                  />
                </div>
              );
            })}
          </div>

          {/* RemoteStart/Stop markers */}
          {markers?.map((m, mi) => {
            const mt = new Date(m.time).getTime();
            const chartStart = new Date(rates[0].start).getTime();
            const chartEnd = new Date(rates[rates.length - 1].end).getTime();
            if (mt < chartStart || mt > chartEnd) return null;
            const left = ((mt - chartStart) / (chartEnd - chartStart)) * 100;
            return (
              <div
                key={mi}
                className={`chart-marker ${m.type}`}
                style={{ left: `${left}%` }}
              />
            );
          })}

          {consumptionPoints.length > 1 && (
            <svg className="consumption-line" viewBox={`0 0 100 ${chartHeight}`} preserveAspectRatio="none">
              <polyline
                points={consumptionPoints.map((p) => `${p.x},${p.y}`).join(' ')}
                fill="none"
                stroke="rgba(33, 150, 243, 0.9)"
                strokeWidth="2"
                vectorEffect="non-scaling-stroke"
                strokeLinejoin="round"
              />
            </svg>
          )}

          {/* Hover tooltip */}
          {hover !== null && hoverRate && hoverStart && (
            <div
              className="chart-tooltip"
              style={{
                left: Math.min(hover.x + 12, chartRef.current ? chartRef.current.offsetWidth - 180 : 250),
                top: Math.max(hover.y - 80, 0),
              }}
            >
              <div className="tooltip-time">
                {hoverStart.toLocaleDateString(locale, { weekday: 'short', day: 'numeric', month: 'short', timeZone: timezone })}{' '}
                {hoverStart.toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: timezone })}
                {' - '}
                {new Date(hoverRate.end).toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: timezone })}
              </div>
              <div className="tooltip-price">{hoverRate.price.toFixed(3)} PLN/kWh</div>
              {hoverWh !== undefined && (
                <>
                  <div className="tooltip-usage">{formatEnergy(hoverWh)}</div>
                  {hoverCost !== undefined && (
                    <div className="tooltip-cost">{hoverCost.toFixed(2)} PLN</div>
                  )}
                </>
              )}
              {hoverIsScheduled && schedule && (
                <div className="tooltip-schedule-info">
                  <div className="tooltip-scheduled">{t('price.scheduled')}</div>
                  <div className="tooltip-schedule-detail">
                    {t('price.plan', { energy: schedule.energy, cost: schedule.cost.toFixed(2) })}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>

        {hasConsumption && (
          <div className="y-axis y-axis-right">
            <span>{maxDisplay.toFixed(useKwh ? 1 : 0)}</span>
            <span>{(maxDisplay / 2).toFixed(useKwh ? 1 : 0)}</span>
            <span className="muted">{energyUnit}</span>
          </div>
        )}
      </div>

      {/* X-axis labels */}
      {(() => {
        const minLabelWidth = 22;
        const labelStep = chartWidth > 0 ? Math.max(1, Math.ceil((barCount * minLabelWidth) / chartWidth)) : 1;
        return (
          <div className="x-axis" style={{ marginLeft: '38px', marginRight: hasConsumption ? '38px' : '0' }}>
            {xLabels.map((l) => {
              const show = l.date || l.idx % labelStep === 0;
              return (
                <div key={l.idx} className="x-label">
                  <span style={{ visibility: show ? 'visible' : 'hidden' }}>{l.hour}</span>
                  {l.date && <span className="x-date">{l.date}</span>}
                </div>
              );
            })}
          </div>
        );
      })()}

      {/* Legend */}
      <div className="chart-legend">
        <span className="legend-item"><span className="legend-swatch schedule-swatch" /> {t('price.scheduled')}</span>
        <span className="legend-item"><span className="legend-swatch usage-swatch" /> {t('price.usage')}</span>
        <span className="legend-item"><span className="legend-swatch start-swatch" /> {t('price.chargeStart')}</span>
        <span className="legend-item"><span className="legend-swatch stop-swatch" /> {t('price.chargeStop')}</span>
        <span className="legend-item"><span className="legend-swatch plug-swatch" /> {t('price.pluggedIn')}</span>
        <span className="legend-item"><span className="legend-swatch unplug-swatch" /> {t('price.unplugged')}</span>
      </div>
    </div>
  );
}
