import { useEffect, useEffectEvent } from 'react';

type SSEHandler = (event: string, data: string) => void;

export function useSSE(url: string | null, handler: SSEHandler, onAuthError?: () => void) {
  const handleEvent = useEffectEvent(handler);
  const handleAuthError = useEffectEvent(() => onAuthError?.());

  useEffect(() => {
    if (!url) return;

    let stopped = false;
    let es: EventSource | null = null;
    let reconnectTimer: number | null = null;

    const reconnect = () => {
      if (!stopped) reconnectTimer = window.setTimeout(connect, 3000);
    };

    const connect = () => {
      if (stopped) return;
      es = new EventSource(url);

      es.addEventListener('ping', () => {});
      es.addEventListener('status', (e) => handleEvent('status', e.data));
      es.addEventListener('ocpp', (e) => handleEvent('ocpp', e.data));
      es.addEventListener('schedule', (e) => handleEvent('schedule', e.data));
      es.addEventListener('rates', (e) => handleEvent('rates', e.data));
      es.addEventListener('meter', (e) => handleEvent('meter', e.data));
      es.addEventListener('bootid', (e) => handleEvent('bootid', e.data));

      es.onerror = () => {
        es?.close();
        // Only an explicit 401 is an auth failure. Proxy and network errors
        // while the backend restarts remain retryable.
        fetch('/api/auth/check').then((r) => {
          if (r.status === 401) handleAuthError();
          else reconnect();
        }).catch(reconnect);
      };
    };

    connect();
    return () => {
      stopped = true;
      if (reconnectTimer !== null) window.clearTimeout(reconnectTimer);
      es?.close();
    };
  }, [url]);
}
