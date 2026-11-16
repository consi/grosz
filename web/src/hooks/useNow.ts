import { useEffect, useState } from 'react';

// Current time that re-renders the caller every `intervalMs`, so render code
// can compare against "now" without calling Date.now() during render.
export function useNow(intervalMs = 60000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}
