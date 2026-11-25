import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react';
import type Hls from 'hls.js';
import { useTranslation } from '../i18n';

const STREAM_URL = '/api/camera/hls/index.m3u8';
const HLS_MIME = 'application/vnd.apple.mpegurl';
// The server needs one keyframe interval (often 5-10 s) before it can serve
// the first playlist, so give a cold start plenty of room.
const START_TIMEOUT_MS = 30000;
// Wait before reconnecting after the Nth consecutive failure. The last value
// repeats, so a dashboard left open recovers on its own once the camera is back.
const RETRY_DELAYS_MS = [2000, 5000, 10000, 20000, 30000];

type Phase = 'connecting' | 'live' | 'error' | 'unsupported';

function subscribeVisibility(onChange: () => void) {
  document.addEventListener('visibilitychange', onChange);
  return () => document.removeEventListener('visibilitychange', onChange);
}

// Safari (and every iOS browser) plays HLS natively and more efficiently than
// a script player; other engines may claim support but handle live
// low-latency streams better through hls.js.
function prefersNativeHls(video: HTMLVideoElement): boolean {
  return video.canPlayType(HLS_MIME) !== '' && /Apple/.test(navigator.vendor);
}

// CameraPreview shows the live camera stream restreamed by grosz. The stream
// is pulled only while the page is visible: the server closes its camera
// connection a minute after the last viewer went away.
export function CameraPreview() {
  const visible = useSyncExternalStore(subscribeVisibility, () => document.visibilityState === 'visible');
  // Each (re)connect mounts a fresh player; `failures` picks its retry delay.
  const [run, setRun] = useState({ attempt: 0, failures: 0 });
  const [aspect, setAspect] = useState('16 / 9');

  const handleLive = useCallback(() => setRun((r) => (r.failures === 0 ? r : { ...r, failures: 0 })), []);
  const handleRetry = useCallback((failed: boolean) => {
    setRun((r) => ({ attempt: r.attempt + 1, failures: failed ? r.failures + 1 : 0 }));
  }, []);

  return (
    <div className="camera-preview" style={{ aspectRatio: aspect }}>
      {visible && (
        <Player key={run.attempt} failures={run.failures} onLive={handleLive} onRetry={handleRetry} onAspect={setAspect} />
      )}
    </div>
  );
}

function Player({ failures, onLive, onRetry, onAspect }: {
  failures: number;
  onLive: () => void;
  onRetry: (failed: boolean) => void;
  onAspect: (aspect: string) => void;
}) {
  const { t } = useTranslation();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [phase, setPhase] = useState<Phase>('connecting');

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    let disposed = false;
    let hls: Hls | null = null;
    let recoveredOnce = false;
    let retryTimer: number | undefined;

    const release = () => {
      hls?.destroy();
      hls = null;
      video.removeAttribute('src');
      video.load();
    };
    const fail = (next: Phase = 'error') => {
      if (disposed) return;
      window.clearTimeout(startTimer);
      release();
      setPhase(next);
      if (next === 'error' && retryTimer === undefined) {
        const delay = RETRY_DELAYS_MS[Math.min(failures, RETRY_DELAYS_MS.length - 1)];
        retryTimer = window.setTimeout(() => onRetry(true), delay);
      }
    };
    const startTimer = window.setTimeout(() => fail(), START_TIMEOUT_MS);

    const onPlaying = () => {
      window.clearTimeout(startTimer);
      setPhase('live');
      onLive();
    };
    const onSize = () => {
      if (video.videoWidth > 0 && video.videoHeight > 0) onAspect(`${video.videoWidth} / ${video.videoHeight}`);
    };
    const onNativeError = () => fail();
    video.addEventListener('playing', onPlaying);
    video.addEventListener('loadedmetadata', onSize);
    video.addEventListener('resize', onSize);

    const playNative = () => {
      video.addEventListener('error', onNativeError);
      video.src = STREAM_URL;
      void video.play().catch(() => { /* autoplay is retried by the autoPlay attribute */ });
    };

    if (prefersNativeHls(video)) {
      playNative();
    } else {
      import('hls.js/light').then(({ default: HlsPlayer }) => {
        if (disposed) return;
        if (!HlsPlayer.isSupported()) {
          if (video.canPlayType(HLS_MIME) !== '') playNative();
          else fail('unsupported');
          return;
        }
        const player = new HlsPlayer({
          lowLatencyMode: true,
          // A preview has no use for history: keep memory flat on a 4K stream.
          backBufferLength: 10,
          maxBufferLength: 10,
          // Catch up to the live edge after a stall instead of drifting behind.
          maxLiveSyncPlaybackRate: 1.5,
        });
        hls = player;
        player.on(HlsPlayer.Events.MANIFEST_PARSED, () => {
          void video.play().catch(() => { /* see above */ });
        });
        player.on(HlsPlayer.Events.ERROR, (_event, data) => {
          if (!data.fatal) return;
          if (data.details === HlsPlayer.ErrorDetails.MANIFEST_INCOMPATIBLE_CODECS_ERROR) {
            fail('unsupported');
          } else if (data.type === HlsPlayer.ErrorTypes.MEDIA_ERROR && !recoveredOnce) {
            recoveredOnce = true;
            player.recoverMediaError();
          } else {
            fail();
          }
        });
        player.loadSource(STREAM_URL);
        player.attachMedia(video);
      }).catch(() => fail());
    }

    return () => {
      disposed = true;
      window.clearTimeout(startTimer);
      window.clearTimeout(retryTimer);
      video.removeEventListener('playing', onPlaying);
      video.removeEventListener('loadedmetadata', onSize);
      video.removeEventListener('resize', onSize);
      video.removeEventListener('error', onNativeError);
      release();
    };
    // One player per mount: the parent remounts (new key) to reconnect.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const enterFullscreen = () => {
    const video = videoRef.current as (HTMLVideoElement & { webkitEnterFullscreen?: () => void }) | null;
    if (!video) return;
    if (video.requestFullscreen) void video.requestFullscreen().catch(() => {});
    else video.webkitEnterFullscreen?.(); // iPhone Safari
  };

  return (
    <>
      <video ref={videoRef} muted playsInline autoPlay aria-label={t('camera.label')} />
      {phase === 'connecting' && (
        <div className="camera-overlay" role="status">
          <span className="camera-spinner" aria-hidden="true" />
          <span>{t('camera.connecting')}</span>
        </div>
      )}
      {phase === 'error' && (
        <div className="camera-overlay" role="status">
          <span>{t('camera.unavailable')}</span>
          <button type="button" className="btn btn-sm" onClick={() => onRetry(false)}>{t('camera.retry')}</button>
        </div>
      )}
      {phase === 'unsupported' && (
        <div className="camera-overlay" role="status">
          <span>{t('camera.unsupported')}</span>
        </div>
      )}
      {phase === 'live' && (
        <>
          <span className="camera-live">{t('camera.live')}</span>
          <button type="button" className="camera-fullscreen" onClick={enterFullscreen} aria-label={t('camera.fullscreen')} title={t('camera.fullscreen')}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M8 3H5a2 2 0 0 0-2 2v3M21 8V5a2 2 0 0 0-2-2h-3M3 16v3a2 2 0 0 0 2 2h3M16 21h3a2 2 0 0 0 2-2v-3" />
            </svg>
          </button>
        </>
      )}
    </>
  );
}
