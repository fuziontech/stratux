/* Stratux Modern Offline-First PWA Core (ES2022) */

export const TRAFFIC_MAX_AGE_SECONDS = 59;
export const TRAFFIC_AIS_MAX_AGE_SECONDS = 60 * 15;
export const TARGET_TYPE_AIS = 5;

const THEME_META_COLORS = {
  dark: '#0b0f14',
  day: '#edf1f6',
  night: '#000000'
};

/**
 * Strictly escape untrusted text before any HTML interpolation.
 */
export function escapeHtml(str) {
  if (str === null || str === undefined) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/**
 * Format decimal degrees to DMS string.
 */
export function formatDMS(val, includeSeconds = false) {
  const num = Number(val);
  if (!Number.isFinite(num)) return '--';
  const sign = num < 0 ? '-' : '';
  const absVal = Math.abs(num);
  if (!includeSeconds) {
    const totalMin = Math.floor(absVal * 60 + 1e-9);
    const deg = Math.floor(totalMin / 60);
    const min = totalMin % 60;
    const degStr = deg < 10 ? `0${deg}` : `${deg}`;
    const minStr = min < 10 ? `0${min}` : `${min}`;
    return `${sign}${degStr}\u00B0 ${minStr}'`;
  }
  const totalSec = Math.round(absVal * 3600);
  const deg = Math.floor(totalSec / 3600);
  const min = Math.floor((totalSec % 3600) / 60);
  const sec = totalSec % 60;
  const degStr = deg < 10 ? `0${deg}` : `${deg}`;
  const minStr = min < 10 ? `0${min}` : `${min}`;
  const secStr = sec < 10 ? `0${sec}` : `${sec}`;
  return `${sign}${degStr}\u00B0 ${minStr}' ${secStr}"`;
}

/**
 * Parse FAA flight category (VFR / MVFR / IFR / LIFR) from a METAR/SPECI string.
 */
export function parseFlightCategory(msgType, body) {
  if (msgType !== 'METAR' && msgType !== 'SPECI') return '';
  if (!body || typeof body !== 'string') return '';

  let visibility;
  let match = /([0-9]+)\s+([0-9]+)\/([0-9]+)SM/.exec(body);
  if (match && match.length === 4) {
    const denom = parseInt(match[3], 10);
    if (!denom) return '';
    visibility = parseInt(match[1], 10) + (parseInt(match[2], 10) / denom);
  } else {
    match = /(?:^|\s)[MP]?([0-9]+)(?:\/([0-9]+))?SM(?:\s|$)/.exec(body);
    if (!match) return '';
    if (match[2] !== undefined) {
      const fracDenom = parseInt(match[2], 10);
      if (!fracDenom) return '';
      visibility = parseInt(match[1], 10) / fracDenom;
    } else {
      visibility = parseInt(match[1], 10);
      if (visibility === 0) return '';
    }
  }

  let ceiling = 999;
  const ceilExp = /\b(?:BKN|OVC|VV)([0-9]{3})/g;
  let ceilMatch;
  while ((ceilMatch = ceilExp.exec(body)) !== null) {
    const layer = parseInt(ceilMatch[1], 10);
    if (layer < ceiling) ceiling = layer;
  }

  if (visibility > 5 && ceiling > 30) return 'VFR';
  if (visibility >= 3 && ceiling >= 10) return 'MVFR';
  if (visibility >= 1 && ceiling >= 5) return 'IFR';
  return 'LIFR';
}

export function formatUptime(ms) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '--';
  const totalSec = Math.floor(ms / 1000);
  const d = Math.floor(totalSec / 86400);
  const h = Math.floor((totalSec % 86400) / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  const pad = (v) => (v < 10 ? `0${v}` : `${v}`);
  return `${d}d ${pad(h)}:${pad(m)}:${pad(s)}`;
}

export function formatBytes(size) {
  if (typeof size !== 'number' || !Number.isFinite(size) || size <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(Math.floor(Math.log(size) / Math.log(1024)), units.length - 1);
  return `${Number((size / Math.pow(1024, i)).toFixed(2))} ${units[i]}`;
}

export function formatUTC(ts) {
  const d = typeof ts === 'number' ? new Date(ts) : new Date(Date.parse(ts));
  if (Number.isNaN(d.getTime())) return '--:--:--Z';
  const pad = (v) => (v < 10 ? `0${v}` : `${v}`);
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}Z`;
}

export function isNonZeroTimestamp(ts) {
  return Boolean(ts && ts !== '0001-01-01T00:00:00Z');
}

export function isSameAircraft(addr1, addrType1, addr2, addrType2) {
  if (addr1 !== addr2) return false;
  return (addrType1 === 1 && addrType2 === 1) || (addrType1 !== 1 && addrType2 !== 1);
}

export function isTrafficAged(craft, field = 'Age') {
  const val = craft[field];
  if (typeof val !== 'number') return false;
  if (craft.TargetType === TARGET_TYPE_AIS) return val > TRAFFIC_AIS_MAX_AGE_SECONDS;
  return val > TRAFFIC_MAX_AGE_SECONDS;
}

export function getTrafficSourceMeta(source) {
  switch (source) {
    case 1: return { code: '1090ES', color: 'var(--es-color)', hex: '#60a5fa' };
    case 2: return { code: '978UAT', color: 'var(--uat-color)', hex: '#fb923c' };
    case 4: return { code: 'OGN', color: 'var(--ogn-color)', hex: '#4ade80' };
    case 8: return { code: 'AIS', color: 'var(--ais-color)', hex: '#22d3ee' };
    case 16: return { code: 'RID', color: 'var(--purple)', hex: '#c084fc' };
    case 32: return { code: 'CoT', color: 'var(--text-muted)', hex: '#a3e635' };
    case 64: return { code: 'FLARM', color: 'var(--ogn-color)', hex: '#4ade80' };
    default: return { code: 'UNK', color: 'var(--text-muted)', hex: '#94a3b8' };
  }
}

export function getCraftCategory(craft) {
  if (craft.TargetType === TARGET_TYPE_AIS) {
    const vt = craft.SurfaceVehicleType || 0;
    const d1 = Math.floor(vt / 10);
    const d2 = vt % 10;
    const f = { 2: 'Cargo', 4: 'Cargo', 5: 'Cargo', 6: 'Passenger', 7: 'Cargo', 8: 'Tanker', 9: 'Cargo' };
    const s = { 0: 'Fishing', 1: 'Tug', 2: 'Tug', 3: 'Dredger', 4: 'Diving', 5: 'Military', 6: 'Sailing', 7: 'Pleasure' };
    return f[d1] || (d1 === 3 && s[d2]) || 'Vessel';
  }
  const cats = {
    1: 'Light', 2: 'Small', 3: 'Large', 4: 'VLarge', 5: 'Heavy',
    6: 'HiPerf', 7: 'Heli', 9: 'Glider', 10: 'Balloon', 11: 'Parach',
    12: 'Ultral', 14: 'Drone', 15: 'Space', 16: 'VLarge', 17: 'Vehic',
    18: 'Vehic', 19: 'Obst'
  };
  return cats[craft.Emitter_category] || '---';
}

export function getCraftHexColor(craft) {
  if (craft.AlertLevel >= 3) return '#ef4444';
  if (craft.AlertLevel === 2) return '#f59e0b';
  if (craft.TargetType === TARGET_TYPE_AIS) return '#22d3ee';
  return getTrafficSourceMeta(craft.Last_source).hex;
}

export function getGpsHardwareLabel(detectedType) {
  const code = (detectedType || 0) & 0x0f;
  const map = {
    1: 'Generic GPS',
    2: 'Prolific USB-Serial',
    3: 'OGN Tracker',
    4: 'Generic u-blox',
    5: 'u-blox 10 GNSS',
    7: 'u-blox 6/7 GNSS',
    8: 'u-blox 8 GNSS',
    9: 'u-blox 9 GNSS',
    10: 'USB/Serial IN',
    11: 'SoftRF Dongle',
    12: 'Network GPS',
    13: 'SoftRF',
    15: 'GxAirCom'
  };
  return map[code] || 'Not installed';
}

/**
 * Safe DOM element builder (uses textContent for strings).
 */
export function createEl(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'className') el.className = v;
    else if (k === 'text') el.textContent = String(v);
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'dataset' && typeof v === 'object') Object.assign(el.dataset, v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, String(v));
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    if (typeof child === 'string' || typeof child === 'number') {
      el.appendChild(document.createTextNode(String(child)));
    } else {
      el.appendChild(child);
    }
  }
  return el;
}

/**
 * Build SVG sparkline markup for numeric history arrays (60s RF rates).
 */
export function buildSparklineSVG(values, color = 'var(--accent)') {
  const arr = Array.isArray(values) && values.length > 0 ? values : [0, 0];
  const w = 240;
  const h = 44;
  const maxVal = Math.max(1, ...arr.map((v) => Number(v) || 0));
  const step = arr.length > 1 ? w / (arr.length - 1) : w;
  const pts = arr.map((v, i) => {
    const x = (i * step).toFixed(1);
    const y = (h - 4 - ((Math.max(0, Number(v) || 0) / maxVal) * (h - 8))).toFixed(1);
    return `${x},${y}`;
  });
  const areaPts = `0,${h} ${pts.join(' ')} ${w},${h}`;
  return `<svg class="sparkline-svg" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" aria-hidden="true">
    <polygon points="${areaPts}" fill="${escapeHtml(color)}" fill-opacity="0.16" />
    <polyline points="${pts.join(' ')}" fill="none" stroke="${escapeHtml(color)}" stroke-width="2" vector-effect="non-scaling-stroke" />
  </svg>`;
}

/* ---------------- HTTP Helpers ---------------- */

export async function apiGet(path, { optional = false, signal } = {}) {
  const resp = await fetch(path, { cache: 'no-store', signal });
  if (!resp.ok) {
    if (optional && (resp.status === 404 || resp.status === 501 || resp.status === 503)) {
      return null;
    }
    const text = await resp.text().catch(() => resp.statusText);
    throw new Error(text || `HTTP ${resp.status}`);
  }
  const ct = resp.headers.get('content-type') || '';
  if (ct.includes('application/json')) return resp.json();
  const txt = await resp.text();
  try { return JSON.parse(txt); } catch { return txt; }
}

export async function apiPost(path, body = null, { signal } = {}) {
  const init = {
    method: 'POST',
    cache: 'no-store',
    signal,
    headers: {}
  };
  if (body !== null && body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(path, init);
  if (!resp.ok) {
    const text = await resp.text().catch(() => resp.statusText);
    throw new Error(text || `HTTP ${resp.status}`);
  }
  const txt = await resp.text();
  if (!txt) return null;
  try { return JSON.parse(txt); } catch { return txt; }
}

export async function apiPostRaw(path, textBody) {
  const resp = await fetch(path, {
    method: 'POST',
    cache: 'no-store',
    headers: { 'Content-Type': 'text/plain' },
    body: String(textBody)
  });
  const txt = await resp.text().catch(() => '');
  if (!resp.ok) throw new Error(txt || `HTTP ${resp.status}`);
  return txt;
}

export async function apiUpload(path, formData) {
  const resp = await fetch(path, {
    method: 'POST',
    body: formData
  });
  const txt = await resp.text().catch(() => '');
  if (!resp.ok) throw new Error(txt || `HTTP ${resp.status}`);
  return txt;
}

/* ---------------- Shared WebSocket Manager ---------------- */

export class StratuxWS {
  constructor(path, handlers = {}) {
    this.path = path;
    this.handlers = handlers;
    this.socket = null;
    this.closed = false;
    this.reconnectTimer = null;
    this.backoffMs = 1000;
    this.maxBackoffMs = 10000;
    this.connect();
  }

  buildUrl() {
    const proto = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
    const host = window.location.host;
    const p = this.path.startsWith('/') ? this.path : `/${this.path}`;
    return `${proto}${host}${p}`;
  }

  connect() {
    if (this.closed) return;
    let ws;
    try {
      ws = new WebSocket(this.buildUrl());
    } catch (err) {
      this.handlers.onError?.(err);
      this.scheduleReconnect();
      return;
    }
    this.socket = ws;

    ws.onopen = (ev) => {
      if (this.closed || ws !== this.socket) return;
      this.backoffMs = 1000;
      this.handlers.onOpen?.(ev);
    };

    ws.onmessage = (ev) => {
      if (this.closed || ws !== this.socket) return;
      let data;
      try {
        data = JSON.parse(ev.data);
      } catch (err) {
        console.warn(`StratuxWS JSON parse error on ${this.path}:`, err);
        return;
      }
      this.handlers.onMessage?.(data, ev);
    };

    ws.onerror = (ev) => {
      if (this.closed || ws !== this.socket) return;
      this.handlers.onError?.(ev);
    };

    ws.onclose = (ev) => {
      if (ws === this.socket) this.socket = null;
      ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null;
      if (this.closed) return;
      this.handlers.onClose?.(ev);
      this.scheduleReconnect();
    };
  }

  scheduleReconnect() {
    if (this.closed || this.reconnectTimer !== null) return;
    const delay = this.backoffMs;
    this.backoffMs = Math.min(this.backoffMs * 2, this.maxBackoffMs);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (!this.closed) this.connect();
    }, delay);
  }

  close() {
    this.closed = true;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.socket) {
      const ws = this.socket;
      this.socket = null;
      ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null;
      try { ws.close(); } catch {}
    }
  }
}

/* ---------------- Screen Wake Lock Manager ---------------- */

export class WakeLockManager {
  constructor() {
    this.sentinel = null;
    this.wanted = false;
    this.onChange = null;
    this._visHandler = () => {
      if (document.visibilityState === 'visible' && this.wanted) {
        this.acquire();
      }
    };
  }

  isSupported() {
    return typeof navigator !== 'undefined' && 'wakeLock' in navigator;
  }

  isActive() {
    return Boolean(this.sentinel && !this.sentinel.released);
  }

  async acquire() {
    this.wanted = true;
    if (!this.isSupported()) {
      this.onChange?.(false);
      return false;
    }
    try {
      if (this.sentinel && !this.sentinel.released) return true;
      this.sentinel = await navigator.wakeLock.request('screen');
      this.sentinel.addEventListener('release', () => {
        this.onChange?.(false);
      });
      document.addEventListener('visibilitychange', this._visHandler);
      this.onChange?.(true);
      return true;
    } catch {
      this.onChange?.(false);
      return false;
    }
  }

  async release() {
    this.wanted = false;
    document.removeEventListener('visibilitychange', this._visHandler);
    if (this.sentinel) {
      try { await this.sentinel.release(); } catch {}
      this.sentinel = null;
    }
    this.onChange?.(false);
  }
}

/* ---------------- Audio & Speech CPA Alerts ---------------- */

export class AudioAlertManager {
  constructor() {
    this.mode = 'off'; // 'off' | 'beep' | 'speech' | 'both'
    try {
      const saved = localStorage.getItem('stratux.audioMode');
      if (saved === 'off' || saved === 'beep' || saved === 'speech' || saved === 'both') {
        this.mode = saved;
      }
    } catch {}
    this.cooldowns = new Map();
    this.audioCtx = null;
    this.fallbackAudio = typeof Audio !== 'undefined' ? new Audio('alert.wav') : null;
  }

  setMode(mode) {
    this.mode = mode;
    try { localStorage.setItem('stratux.audioMode', mode); } catch {}
  }

  cycleMode() {
    const order = ['off', 'both', 'beep', 'speech'];
    const next = order[(order.indexOf(this.mode) + 1) % order.length];
    this.setMode(next);
    return next;
  }

  playTone(level = 2) {
    try {
      const Ctx = window.AudioContext || window.webkitAudioContext;
      if (Ctx) {
        if (!this.audioCtx) this.audioCtx = new Ctx();
        if (this.audioCtx.state === 'suspended') this.audioCtx.resume();
        const now = this.audioCtx.currentTime;
        const pulses = level >= 3 ? 3 : 1;
        for (let i = 0; i < pulses; i++) {
          const osc = this.audioCtx.createOscillator();
          const gain = this.audioCtx.createGain();
          osc.type = 'sine';
          osc.frequency.value = level >= 3 ? 1100 : 840;
          gain.gain.setValueAtTime(0.001, now + i * 0.16);
          gain.gain.exponentialRampToValueAtTime(0.25, now + i * 0.16 + 0.02);
          gain.gain.exponentialRampToValueAtTime(0.001, now + i * 0.16 + 0.12);
          osc.connect(gain);
          gain.connect(this.audioCtx.destination);
          osc.start(now + i * 0.16);
          osc.stop(now + i * 0.16 + 0.13);
        }
        return;
      }
    } catch {}
    if (this.fallbackAudio) {
      this.fallbackAudio.currentTime = 0;
      this.fallbackAudio.play().catch(() => {});
    }
  }

  speak(text) {
    if (!text || typeof window === 'undefined' || !('speechSynthesis' in window)) return;
    try {
      const utter = new SpeechSynthesisUtterance(text);
      utter.lang = 'en-US';
      utter.rate = 1.1;
      window.speechSynthesis.speak(utter);
    } catch {}
  }

  announceTraffic(key, level, calloutText) {
    if (this.mode === 'off' || level < 2) return;
    const now = Date.now();
    const cooldownMs = level >= 3 ? 8000 : 18000;
    const last = this.cooldowns.get(key) || 0;
    if (now - last < cooldownMs) return;
    this.cooldowns.set(key, now);

    if (this.mode === 'beep' || this.mode === 'both') {
      this.playTone(level);
    }
    if ((this.mode === 'speech' || this.mode === 'both') && calloutText) {
      this.speak(calloutText);
    }
  }
}

export const audioAlerts = new AudioAlertManager();

/* ---------------- Theme Manager ---------------- */

export const ThemeManager = {
  current: 'dark',
  init() {
    let theme = 'dark';
    try {
      const saved = localStorage.getItem('stratux.theme');
      if (saved === 'day' || saved === 'dark' || saved === 'night') theme = saved;
    } catch {}
    this.set(theme, false);
  },
  set(theme, persist = true) {
    if (theme !== 'day' && theme !== 'dark' && theme !== 'night') theme = 'dark';
    this.current = theme;
    if (typeof document !== 'undefined') {
      document.documentElement.setAttribute('data-theme', theme);
      const meta = document.getElementById('meta-theme-color');
      if (meta) meta.setAttribute('content', THEME_META_COLORS[theme]);
      document.querySelectorAll('[data-theme-choice]').forEach((btn) => {
        btn.classList.toggle('active', btn.dataset.themeChoice === theme);
      });
    }
    if (persist) {
      try { localStorage.setItem('stratux.theme', theme); } catch {}
    }
  }
};

/* ---------------- Toasts & Connection Badge ---------------- */

export function showToast(message, level = 'info') {
  const stack = document.getElementById('toast-stack');
  if (!stack) return;
  const el = createEl('div', {
    className: `toast ${level === 'error' ? 'toast-error' : level === 'ok' ? 'toast-ok' : ''}`,
    text: message
  });
  stack.appendChild(el);
  setTimeout(() => {
    el.remove();
  }, 3800);
}

export function setConnectionBadge(state, label) {
  const badge = document.getElementById('conn-badge');
  const textEl = document.getElementById('conn-label');
  if (!badge || !textEl) return;
  badge.className = `conn-badge conn-${state}`;
  textEl.textContent = label || state;
}

/* ---------------- Shared Settings State ---------------- */

export const AppState = {
  settings: null,
  async refreshSettings() {
    try {
      const s = await apiGet('/getSettings', { optional: true });
      if (s && typeof s === 'object') {
        this.settings = s;
        applyNavVisibility(s);
        // Apply backend DarkMode only if user has never picked a theme locally
        try {
          if (!localStorage.getItem('stratux.theme') && typeof s.DarkMode === 'boolean') {
            ThemeManager.set(s.DarkMode ? 'dark' : 'day', false);
          }
        } catch {}
      }
    } catch {}
    return this.settings;
  },
  updateLocalSettings(s) {
    if (!s || typeof s !== 'object') return;
    this.settings = { ...(this.settings || {}), ...s };
    applyNavVisibility(this.settings);
  }
};

function applyNavVisibility(s) {
  const devLink = document.getElementById('nav-developer');
  if (devLink) devLink.classList.toggle('hidden', !s.DeveloperMode);
  const showUat = Boolean(s.UAT_Enabled || s.Ping_Enabled || s.Pong_Enabled || s.RegionSelected === 1);
  document.querySelectorAll('[data-uat-only="1"]').forEach((el) => {
    el.classList.toggle('hidden', !showUat);
  });
}

/* ---------------- Hash Router ---------------- */

const ROUTES = {
  status:    { title: 'Status',     loader: () => import('./views/status.js') },
  traffic:   { title: 'Traffic',    loader: () => import('./views/traffic.js') },
  radar:     { title: 'Radar',      loader: () => import('./views/radar.js') },
  map:       { title: 'Map',        loader: () => import('./views/map.js') },
  gps:       { title: 'GPS / AHRS', loader: () => import('./views/gps.js') },
  weather:   { title: 'Weather',    loader: () => import('./views/weather.js') },
  towers:    { title: 'Towers',     loader: () => import('./views/towers.js') },
  flights:   { title: 'Flights',    loader: () => import('./views/flights.js') },
  settings:  { title: 'Settings',   loader: () => import('./views/settings.js') },
  developer: { title: 'Developer',  loader: () => import('./views/developer.js') }
};

let activeCleanup = null;
let routeSeq = 0;

function parseRoute() {
  const raw = (window.location.hash || '#/status').replace(/^#\/?/, '').split('?')[0].trim().toLowerCase();
  if (raw === '' || raw === 'home') return 'status';
  if (raw === 'logs') return 'developer';
  return ROUTES[raw] ? raw : 'status';
}

function closeDrawer() {
  document.getElementById('sidebar')?.classList.remove('open');
  document.getElementById('sidebar-backdrop')?.classList.remove('open');
}

async function navigate() {
  const seq = ++routeSeq;
  const routeKey = parseRoute();
  const route = ROUTES[routeKey];

  closeDrawer();

  if (typeof activeCleanup === 'function') {
    try { activeCleanup(); } catch (e) { console.error('View destroy error:', e); }
    activeCleanup = null;
  }

  const titleEl = document.getElementById('topbar-title');
  if (titleEl) titleEl.textContent = route.title;
  document.title = `${route.title} — Stratux`;

  document.querySelectorAll('[data-route]').forEach((el) => {
    el.classList.toggle('active', el.dataset.route === routeKey);
  });

  const container = document.getElementById('view');
  if (!container) return;
  container.textContent = '';

  try {
    const mod = await route.loader();
    if (seq !== routeSeq) return; // Superseded by another navigation
    const mountFn = mod.mount || mod.default;
    if (typeof mountFn === 'function') {
      const cleanup = await mountFn(container);
      if (seq !== routeSeq) {
        if (typeof cleanup === 'function') cleanup();
        return;
      }
      activeCleanup = typeof cleanup === 'function' ? cleanup : null;
    }
  } catch (err) {
    console.error(`Failed to load view ${routeKey}:`, err);
    container.textContent = '';
    container.appendChild(createEl('div', {
      className: 'alert-banner alert-danger',
      text: `Failed to load ${route.title} view: ${err.message}`
    }));
  }
}

function bootstrap() {
  ThemeManager.init();

  document.querySelectorAll('[data-theme-choice]').forEach((btn) => {
    btn.addEventListener('click', () => ThemeManager.set(btn.dataset.themeChoice));
  });

  const menuBtn = document.getElementById('btn-menu-toggle');
  const closeBtn = document.getElementById('btn-sidebar-close');
  const backdrop = document.getElementById('sidebar-backdrop');
  const sidebar = document.getElementById('sidebar');

  menuBtn?.addEventListener('click', () => {
    sidebar?.classList.add('open');
    backdrop?.classList.add('open');
  });
  closeBtn?.addEventListener('click', closeDrawer);
  backdrop?.addEventListener('click', closeDrawer);

  window.addEventListener('hashchange', navigate);

  // Register Offline-First Service Worker
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('./sw.js').catch((err) => {
      console.warn('Service worker registration skipped:', err);
    });
  }

  AppState.refreshSettings();
  navigate();
}

if (typeof document !== 'undefined' && document.getElementById('view')) {
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bootstrap);
  } else {
    bootstrap();
  }
}
