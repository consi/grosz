export interface ChargingWindow { start: string; end: string }
export interface OverridePayload extends ChargingWindow {
  kind: 'force' | 'block';
  original?: ChargingWindow;
}
export interface MutationResult { ok: boolean; error?: string }
export type SaveOverride = (payload: OverridePayload, id?: number) => Promise<MutationResult>;
export type DeleteOverride = (id: number) => Promise<MutationResult>;

// datetime-local has no timezone: explicitly interpret it in the display zone.
export function toZonedInput(iso: string, timezone: string): string {
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  }).formatToParts(new Date(iso));
  const part = (type: string) => parts.find((p) => p.type === type)!.value;
  return `${part('year')}-${part('month')}-${part('day')}T${part('hour')}:${part('minute')}`;
}

export function fromZonedInput(value: string, timezone: string, original?: string): string {
  // Keep the original offset when editing an unchanged ambiguous autumn hour.
  if (original && toZonedInput(original, timezone) === value) return original;
  const wall = Date.parse(`${value}:00Z`);
  let instant = wall;
  for (let i = 0; i < 4; i++) {
    const rendered = toZonedInput(new Date(instant).toISOString(), timezone);
    const delta = wall - Date.parse(`${rendered}:00Z`);
    if (delta === 0) return new Date(instant).toISOString();
    instant += delta;
  }
  throw new Error('Invalid local time'); // Includes times skipped by DST.
}

// nextDeparture returns the next occurrence (strictly after `now`) of the
// "HH:MM" wall time in the display timezone, as an ISO instant.
export function nextDeparture(hhmm: string, timezone: string, now: Date = new Date()): string {
  const match = /^(\d{1,2}):(\d{2})$/.exec(hhmm.trim());
  const time = match ? `${match[1].padStart(2, '0')}:${match[2]}` : '07:00';
  const today = toZonedInput(now.toISOString(), timezone).slice(0, 10);
  for (let day = 0; day < 3; day++) {
    const [y, m, d] = today.split('-').map(Number);
    const date = new Date(Date.UTC(y, m - 1, d + day)).toISOString().slice(0, 10);
    try {
      const candidate = fromZonedInput(`${date}T${time}`, timezone);
      if (Date.parse(candidate) > now.getTime()) return candidate;
    } catch { /* Skipped by DST: try the next day. */ }
  }
  return new Date(now.getTime() + 86400000).toISOString();
}

// overlaps reports whether two [start, end) windows share any time.
export function overlaps(a: ChargingWindow, b: ChargingWindow): boolean {
  return Date.parse(a.start) < Date.parse(b.end) && Date.parse(a.end) > Date.parse(b.start);
}

// covers reports whether `outer` fully contains `inner`.
export function covers(outer: ChargingWindow, inner: ChargingWindow): boolean {
  return Date.parse(outer.start) <= Date.parse(inner.start) && Date.parse(outer.end) >= Date.parse(inner.end);
}

// Current time truncated to the minute, as override windows are minute-aligned.
export function minuteNow(): number {
  return Math.floor(Date.now() / 60000) * 60000;
}
