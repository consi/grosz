import type { OcppEvent } from '../types';

// Fan-out for live OCPP messages arriving on the app's single SSE stream, so
// views like the debug console can listen without opening a second stream.
type Listener = (event: OcppEvent) => void;
const listeners = new Set<Listener>();

export function publishOcpp(event: OcppEvent): void {
  for (const listener of listeners) listener(event);
}

export function subscribeOcpp(listener: Listener): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
