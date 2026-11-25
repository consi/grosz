// hls.js ships the light build (no subtitles, DRM or alternate audio) without
// its own type entry; it exposes the same API as the full build.
declare module 'hls.js/light' {
  export * from 'hls.js';
  export { default } from 'hls.js';
}
