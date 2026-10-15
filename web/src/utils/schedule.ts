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
