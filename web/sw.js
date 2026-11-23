/* Stratux Offline-First Service Worker */
const CACHE_VERSION = 'stratux-pwa-__STRATUX_BUILD__';

const APP_SHELL = [
  './',
  './index.html',
  './manifest.json',
  './alert.wav',
  './css/app.css',
  './css/ol.css',
  './css/ol-layerswitcher.css',
  './js/app.js',
  './js/ol.js',
  './js/olms.js',
  './js/ol-layerswitcher.js',
  './js/views/status.js',
  './js/views/traffic.js',
  './js/views/radar.js',
  './js/views/map.js',
  './js/views/gps.js',
  './js/views/weather.js',
  './js/views/towers.js',
  './js/views/flights.js',
  './js/views/settings.js',
  './js/views/developer.js',
  './img/logo-android3.png',
  './img/logo-apple3.png',
  './img/actype/glider.svg',
  './img/actype/heavy.svg',
  './img/actype/helicopter.svg',
  './img/actype/light.svg',
  './img/actype/lighter-than-air.svg',
  './img/actype/skydiver.svg',
  './img/actype/undef.svg',
  './img/actype/vessel.svg'
];

function isNetworkFirstPath(pathname) {
  return (
    pathname.startsWith('/get') ||
    pathname.startsWith('/export') ||
    pathname.startsWith('/checkUpdate') ||
    pathname.startsWith('/tiles/') ||
    pathname.startsWith('/logs/') ||
    pathname.startsWith('/download') ||
    pathname.startsWith('/mapdata/')
  );
}

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_VERSION)
      .then((cache) => Promise.all(
        APP_SHELL.map((url) =>
          cache.add(url).catch(() => {
            // Do not fail install if an optional asset is temporarily absent
          })
        )
      ))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(
        keys
          .filter((key) => key.startsWith('stratux-pwa-') && key !== CACHE_VERSION)
          .map((key) => caches.delete(key))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  if (isNetworkFirstPath(url.pathname)) {
    event.respondWith(
      fetch(req)
        .then((resp) => {
          if (resp && resp.ok && url.pathname.startsWith('/tiles/')) {
            const copy = resp.clone();
            caches.open(CACHE_VERSION).then((cache) => cache.put(req, copy)).catch(() => {});
          }
          return resp;
        })
        .catch(() => caches.match(req).then((hit) => hit || new Response('{"error":"offline"}', {
          status: 503,
          headers: { 'Content-Type': 'application/json' }
        })))
    );
    return;
  }

  // App shell & static assets: cache-first with background revalidation
  event.respondWith(
    caches.match(req).then((cached) => {
      const networkFetch = fetch(req)
        .then((resp) => {
          if (resp && resp.ok && resp.type === 'basic') {
            const copy = resp.clone();
            caches.open(CACHE_VERSION).then((cache) => cache.put(req, copy)).catch(() => {});
          }
          return resp;
        })
        .catch(() => cached);
      return cached || networkFetch;
    })
  );
});
