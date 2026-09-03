import { useState, useRef, useCallback, useMemo } from 'react';
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

const DRAG_THRESHOLD_PX = 6;
const DOUBLE_TAP_MS = 300;
const LOWER_PERCENTILE_KEY = 'grosz.priceChart.lowerPercentile';
const UPPER_PERCENTILE_KEY = 'grosz.priceChart.upperPercentile';
const DEFAULT_LOWER_PERCENTILE = 2.5;
const DEFAULT_UPPER_PERCENTILE = 75;

type RGB = readonly [number, number, number];

const PRICE_GREEN: RGB = [48, 209, 88];
const PRICE_YELLOW: RGB = [255, 214, 10];
const PRICE_RED: RGB = [255, 69, 58];

function mixColor(from: RGB, to: RGB, amount: number): string {
  const channels = from.map((channel, i) =>
    Math.round(channel + (to[i] - channel) * amount),
  );
  return `rgb(${channels.join(', ')})`;
}

function percentile(sortedValues: number[], position: number): number {
  const index = (sortedValues.length - 1) * position;
  const lowerIndex = Math.floor(index);
  const upperIndex = Math.ceil(index);
  const amount = index - lowerIndex;
  return sortedValues[lowerIndex] +
    (sortedValues[upperIndex] - sortedValues[lowerIndex]) * amount;
}

function priceColor(price: number, low: number, middle: number, high: number): string {
  if (price === middle) {
    return `rgb(${PRICE_YELLOW.join(', ')})`;
  }

  if (price < middle) {
    const range = middle - low;
    if (!Number.isFinite(range) || range <= 0) return mixColor(PRICE_GREEN, PRICE_YELLOW, 0);
    const position = Math.max(0, Math.min(1, (price - low) / range));
    return mixColor(PRICE_GREEN, PRICE_YELLOW, position);
  }

  const range = high - middle;
  if (!Number.isFinite(range) || range <= 0) return mixColor(PRICE_YELLOW, PRICE_RED, 1);
  const position = Math.max(0, Math.min(1, (price - middle) / range));
  return mixColor(PRICE_YELLOW, PRICE_RED, position);
}

function storedPercentile(key: string, fallback: number, min: number, max: number): number {
  try {
    const value = Number.parseFloat(localStorage.getItem(key) ?? '');
    return Number.isFinite(value) && value >= min && value <= max ? value : fallback;
  } catch {
    return fallback;
  }
}

function savePercentile(key: string, value: number): void {
  try {
    localStorage.setItem(key, String(value));
  } catch {
    // Keep live controls working when browser storage is unavailable.
  }
}

function storedPercentiles(): { lower: number; upper: number } {
  const lower = storedPercentile(LOWER_PERCENTILE_KEY, DEFAULT_LOWER_PERCENTILE, 0, 99.5);
  const upper = storedPercentile(UPPER_PERCENTILE_KEY, DEFAULT_UPPER_PERCENTILE, 0.5, 100);
  return lower < upper
    ? { lower, upper }
    : { lower: DEFAULT_LOWER_PERCENTILE, upper: DEFAULT_UPPER_PERCENTILE };
}

export function PriceChart({ rates, schedule, consumption, markers, timezone }: Props) {
  const { t, locale } = useTranslation();
  const [hover, setHover] = useState<{ idx: number; x: number; y: number } | null>(null);
  const [zoom, setZoom] = useState<{ startMs: number; endMs: number } | null>(null);
  const [drag, setDrag] = useState<{ startX: number; currentX: number } | null>(null);
  const chartRef = useRef<HTMLDivElement>(null);
  const [chartWidth, setChartWidth] = useState(0);
  const roRef = useRef<ResizeObserver>(null);
  const pointerDownXRef = useRef<number | null>(null);
  const lastTapRef = useRef<number>(0);
  const draggingRef = useRef<boolean>(false);
  const [{ lower: lowerPercentile, upper: upperPercentile }, setPercentiles] =
    useState(storedPercentiles);

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

  // If the zoom window no longer covers ≥2 rates after a refresh, fall back to
  // the full range so the chart stays renderable. The zoom state remains and
  // will recover automatically if fresh rates reintroduce the window.
  const viewRates = useMemo(() => {
    if (!zoom) return rates;
    const filtered = rates.filter((r) => {
      const s = new Date(r.start).getTime();
      const e = new Date(r.end).getTime();
      return e > zoom.startMs && s < zoom.endMs;
    });
    return filtered.length >= 2 ? filtered : rates;
  }, [rates, zoom]);

  if (!rates?.length) {
    return (
      <div className="card">
        <h2>{t('price.heading')}</h2>
        <p className="muted">{t('price.noData')}</p>
      </div>
    );
  }

  const prices = viewRates.map((r) => r.price);
  const maxPrice = Math.max(...prices);
  const minPrice = Math.min(...prices);
  const avgPrice = prices.reduce((a, b) => a + b, 0) / prices.length;
  const sortedPrices = rates.map((r) => r.price).sort((a, b) => a - b);
  const colorLowPrice = percentile(sortedPrices, lowerPercentile / 100);
  const colorMiddlePrice = percentile(
    sortedPrices,
    (lowerPercentile + upperPercentile) / 200,
  );
  const colorHighPrice = percentile(sortedPrices, upperPercentile / 100);
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
      const wh = Math.max(0, c.energyWh);
      consumptionMap.set(toHourKey(new Date(c.hour)), wh);
      if (wh > maxWh) maxWh = wh;
    }
  }
  const hasConsumption = maxWh > 0;

  const useKwh = maxWh >= 1000;
  const maxDisplay = useKwh ? maxWh / 1000 : maxWh;
  const energyUnit = useKwh ? 'kWh' : 'Wh';

  const barCount = viewRates.length;
  const chartHeight = 360;

  // Build consumption line — only connect points that have data
  const consumptionPoints: { x: number; y: number; wh: number }[] = [];
  if (hasConsumption) {
    viewRates.forEach((r, i) => {
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
    for (let i = 0; i < viewRates.length; i++) {
      const rs = new Date(viewRates[i].start).getTime();
      const re = new Date(viewRates[i].end).getTime();
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
      scheduleRanges.push({ startIdx: rangeStart, endIdx: viewRates.length - 1 });
    }
  }

  // X-axis labels: hours + date on day boundaries
  const xLabels: { idx: number; hour: string; date?: string }[] = [];
  let lastDate = '';
  viewRates.forEach((r, i) => {
    const d = new Date(r.start);
    const hour = d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: timezone }).slice(0, 2);
    const dateStr = formatDate(d, timezone, locale);
    const showDate = dateStr !== lastDate;
    if (showDate) lastDate = dateStr;
    xLabels.push({ idx: i, hour, date: showDate ? dateStr : undefined });
  });

  // Hover data
  const hoverRate = hover !== null ? viewRates[hover.idx] : null;
  const hoverStart = hoverRate ? new Date(hoverRate.start) : null;
  const hoverWh = hoverRate ? consumptionMap.get(toHourKey(new Date(hoverRate.start))) : undefined;
  const hoverCost = hoverWh !== undefined && hoverRate ? (hoverRate.price * hoverWh / 1000) : undefined;
  const hoverIsScheduled = hoverRate
    ? isScheduledHour(new Date(hoverRate.start).getTime(), new Date(hoverRate.end).getTime())
    : false;

  const idxAtX = (x: number, rect: DOMRect): number => {
    const idx = Math.floor((x / rect.width) * barCount);
    return Math.max(0, Math.min(barCount - 1, idx));
  };

  const handlePointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    pointerDownXRef.current = x;
    draggingRef.current = false;

    if (e.pointerType !== 'mouse') {
      const now = Date.now();
      if (zoom && now - lastTapRef.current < DOUBLE_TAP_MS) {
        setZoom(null);
        setHover(null);
        pointerDownXRef.current = null;
        lastTapRef.current = 0;
        return;
      }
      lastTapRef.current = now;
    }

    if (rect.width > 0 && barCount > 0) {
      setHover({ idx: idxAtX(x, rect), x, y });
    }

    try { e.currentTarget.setPointerCapture(e.pointerId); } catch { /* ignore */ }
  };

  const handlePointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;

    const startX = pointerDownXRef.current;
    if (startX != null) {
      if (draggingRef.current || Math.abs(x - startX) >= DRAG_THRESHOLD_PX) {
        if (!draggingRef.current) {
          draggingRef.current = true;
          setHover(null);
        }
        setDrag({ startX, currentX: x });
        return;
      }
    }

    // Hover mode (no active drag)
    if (rect.width <= 0 || barCount <= 0) return;
    const idx = idxAtX(x, rect);
    setHover({ idx, x, y });
  };

  const handlePointerUp = (e: React.PointerEvent<HTMLDivElement>) => {
    try { e.currentTarget.releasePointerCapture(e.pointerId); } catch { /* ignore */ }
    const wasDragging = draggingRef.current;
    const startX = pointerDownXRef.current;
    pointerDownXRef.current = null;
    draggingRef.current = false;

    if (wasDragging && startX != null) {
      const rect = e.currentTarget.getBoundingClientRect();
      const currentX = e.clientX - rect.left;
      const a = Math.min(startX, currentX);
      const b = Math.max(startX, currentX);
      if (rect.width > 0 && barCount > 0 && b - a >= DRAG_THRESHOLD_PX) {
        const firstIdx = idxAtX(a, rect);
        const lastIdx = idxAtX(b, rect);
        if (lastIdx > firstIdx) {
          const startMs = new Date(viewRates[firstIdx].start).getTime();
          const endMs = new Date(viewRates[lastIdx].end).getTime();
          setZoom({ startMs, endMs });
        }
      }
    }
    setDrag(null);
  };

  const handlePointerCancel = () => {
    pointerDownXRef.current = null;
    draggingRef.current = false;
    setDrag(null);
  };

  const handleContextMenu = (e: React.MouseEvent<HTMLDivElement>) => {
    if (!zoom) return;
    e.preventDefault();
    setZoom(null);
    setHover(null);
  };

  const handlePointerLeave = (e: React.PointerEvent<HTMLDivElement>) => {
    if (draggingRef.current) return;
    // On touch, leave fires when the finger lifts — keep the tooltip pinned
    // so a tap shows details until the next interaction.
    if (e.pointerType === 'mouse') setHover(null);
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
          style={{ touchAction: 'pan-y' }}
          onPointerDown={handlePointerDown}
          onPointerMove={handlePointerMove}
          onPointerUp={handlePointerUp}
          onPointerCancel={handlePointerCancel}
          onPointerLeave={handlePointerLeave}
          onContextMenu={handleContextMenu}
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
            {viewRates.map((r, i) => {
              const start = new Date(r.start);
              const height = (Math.abs(r.price) / scaleRange) * 100;
              const isNeg = r.price < 0;
              const isCurrent = now >= start && now < new Date(r.end);

              const barStyle: React.CSSProperties = {
                height: `${Math.max(height, 0.5)}%`,
                backgroundColor: priceColor(
                  r.price,
                  colorLowPrice,
                  colorMiddlePrice,
                  colorHighPrice,
                ),
                ...(isNeg ? { top: `${zeroPct}%` } : { bottom: `${100 - zeroPct}%` }),
              };

              return (
                <div key={i} className="bar-slot">
                  <div
                    className={`bar ${isCurrent ? 'current' : ''} ${isNeg ? 'negative' : ''} ${hover?.idx === i ? 'hovered' : ''}`}
                    style={barStyle}
                  />
                </div>
              );
            })}
          </div>

          {/* RemoteStart/Stop markers */}
          {markers?.map((m, mi) => {
            const mt = new Date(m.time).getTime();
            const chartStart = new Date(viewRates[0].start).getTime();
            const chartEnd = new Date(viewRates[viewRates.length - 1].end).getTime();
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
                stroke="rgba(10, 132, 255, 0.9)"
                strokeWidth="2"
                vectorEffect="non-scaling-stroke"
                strokeLinejoin="round"
              />
            </svg>
          )}

          {/* Zoom selection rectangle */}
          {drag && (
            <div
              className="zoom-selection"
              style={{
                left: Math.min(drag.startX, drag.currentX),
                width: Math.abs(drag.currentX - drag.startX),
              }}
            />
          )}

          {/* Hover tooltip */}
          {hover !== null && hoverRate && hoverStart && !drag && (
            <div
              className="chart-tooltip"
              style={{
                left: Math.min(hover.x + 12, Math.max(chartWidth - 180, 0)),
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

        <div className="percentile-control">
          <div className="percentile-rail">
            <div
              className="percentile-selection"
              style={{
                '--selection-start': `${lowerPercentile}%`,
                '--selection-size': `${upperPercentile - lowerPercentile}%`,
              } as React.CSSProperties}
            />
          </div>
          <input
            className="percentile-range percentile-range-lower"
            type="range"
            min="0"
            max="100"
            step="0.5"
            value={lowerPercentile}
            aria-label="Green bound percentile"
            aria-valuemax={upperPercentile - 0.5}
            aria-valuetext={`${lowerPercentile}%`}
            onChange={(e) => {
              const value = Math.min(upperPercentile - 0.5, Number(e.target.value));
              setPercentiles({ lower: value, upper: upperPercentile });
              savePercentile(LOWER_PERCENTILE_KEY, value);
            }}
          />
          <input
            className="percentile-range percentile-range-upper"
            type="range"
            min="0"
            max="100"
            step="0.5"
            value={upperPercentile}
            aria-label="Red bound percentile"
            aria-valuemin={lowerPercentile + 0.5}
            aria-valuetext={`${upperPercentile}%`}
            onChange={(e) => {
              const value = Math.max(lowerPercentile + 0.5, Number(e.target.value));
              setPercentiles({ lower: lowerPercentile, upper: value });
              savePercentile(UPPER_PERCENTILE_KEY, value);
            }}
          />
          <span
            className="percentile-value percentile-value-lower"
            style={{
              '--vertical-position': `${38 + lowerPercentile * 2.84}px`,
              '--horizontal-position': `${lowerPercentile}%`,
              '--horizontal-offset': `${38 - lowerPercentile * 0.76}px`,
            } as React.CSSProperties}
          >
            {lowerPercentile}%
          </span>
          <span
            className="percentile-value percentile-value-upper"
            style={{
              '--vertical-position': `${38 + upperPercentile * 2.84}px`,
              '--horizontal-position': `${upperPercentile}%`,
              '--horizontal-offset': `${38 - upperPercentile * 0.76}px`,
            } as React.CSSProperties}
          >
            {upperPercentile}%
          </span>
        </div>
      </div>

      {/* X-axis labels */}
      {(() => {
        const minLabelWidth = 22;
        const labelStep = chartWidth > 0 ? Math.max(1, Math.ceil((barCount * minLabelWidth) / chartWidth)) : 1;
        return (
          <div className={`x-axis ${hasConsumption ? 'with-consumption' : ''}`}>
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
