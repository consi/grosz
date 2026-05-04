import { useRef, useState, useEffect, useCallback } from 'react';

interface Options {
  onRefresh: () => void;
  enabled: boolean;
  threshold?: number;
}

export function usePullToRefresh({ onRefresh, enabled, threshold = 80 }: Options) {
  const [pulling, setPulling] = useState(false);
  const [pullDistance, setPullDistance] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const startY = useRef(0);
  const isPulling = useRef(false);

  const handleRefresh = useCallback(() => {
    setRefreshing(true);
    onRefresh();
    setTimeout(() => setRefreshing(false), 1000);
  }, [onRefresh]);

  useEffect(() => {
    if (!enabled) return;

    const onTouchStart = (e: TouchEvent) => {
      if (window.scrollY > 0 || refreshing) return;
      startY.current = e.touches[0].clientY;
    };

    const onTouchMove = (e: TouchEvent) => {
      if (window.scrollY > 0 || refreshing) {
        if (isPulling.current) {
          isPulling.current = false;
          setPulling(false);
          setPullDistance(0);
        }
        return;
      }
      const dy = e.touches[0].clientY - startY.current;
      if (dy > 10) {
        isPulling.current = true;
        setPulling(true);
        // Diminishing pull: sqrt curve for natural feel
        setPullDistance(Math.min(Math.sqrt(dy) * 8, 150));
        if (dy > 30) {
          e.preventDefault();
        }
      }
    };

    const onTouchEnd = () => {
      if (!isPulling.current) return;
      const dist = pullDistance;
      isPulling.current = false;
      setPulling(false);
      setPullDistance(0);
      if (dist >= threshold) {
        handleRefresh();
      }
    };

    document.addEventListener('touchstart', onTouchStart, { passive: true });
    document.addEventListener('touchmove', onTouchMove, { passive: false });
    document.addEventListener('touchend', onTouchEnd, { passive: true });
    return () => {
      document.removeEventListener('touchstart', onTouchStart);
      document.removeEventListener('touchmove', onTouchMove);
      document.removeEventListener('touchend', onTouchEnd);
    };
  }, [enabled, refreshing, threshold, pullDistance, handleRefresh]);

  return { pulling, pullDistance, refreshing };
}
