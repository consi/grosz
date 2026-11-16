import { useCallback, useSyncExternalStore } from 'react';

// Subscribes to a CSS media query so components can adapt their structure
// (not just styling) to the viewport, e.g. collapsing secondary cards on phones.
export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback((onChange: () => void) => {
    const mql = window.matchMedia(query);
    mql.addEventListener('change', onChange);
    return () => mql.removeEventListener('change', onChange);
  }, [query]);
  return useSyncExternalStore(subscribe, () => window.matchMedia(query).matches, () => false);
}
