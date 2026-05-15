import { useEffect, useRef, useCallback } from 'react';

type SSEHandler = (event: string, data: string) => void;

export function useSSE(url: string | null, handler: SSEHandler, onAuthError?: () => void) {
  const handlerRef = useRef(handler);
  handlerRef.current = handler;

  const onAuthErrorRef = useRef(onAuthError);
  onAuthErrorRef.current = onAuthError;

  const connect = useCallback(() => {
    if (!url) return null;
    const es = new EventSource(url);

    es.addEventListener('ping', () => {});
    es.addEventListener('status', (e) => handlerRef.current('status', e.data));
    es.addEventListener('ocpp', (e) => handlerRef.current('ocpp', e.data));
    es.addEventListener('schedule', (e) => handlerRef.current('schedule', e.data));
    es.addEventListener('rates', (e) => handlerRef.current('rates', e.data));
    es.addEventListener('meter', (e) => handlerRef.current('meter', e.data));
    es.addEventListener('bootid', (e) => handlerRef.current('bootid', e.data));

    es.onerror = () => {
      es.close();
      // Check if auth is still valid before reconnecting. Only treat an
      // explicit 401 as auth failure — 502/503 from the proxy while the
      // backend is still restarting is a transient condition, not a logout.
      fetch('/api/auth/check').then((r) => {
        if (r.status === 401 && onAuthErrorRef.current) {
          onAuthErrorRef.current();
        } else {
          setTimeout(connect, 3000);
        }
      }).catch(() => {
        setTimeout(connect, 3000);
      });
    };

    return es;
  }, [url]);

  useEffect(() => {
    const es = connect();
    return () => es?.close();
  }, [connect]);
}
