import {
  apiPost,
  audioAlerts,
  getCraftHexColor,
  isNonZeroTimestamp,
  setConnectionBadge,
  StratuxWS
} from '../app.js';

const RANGES_NM = [2, 5, 10, 20, 40];
const ALT_LIMITS_FT = [500, 1000, 2000, 5000, 10000, 50000];
const MAX_TRAIL_POINTS = 50;
const RADAR_CUTOFF_MS = 35000;

const AUDIO_LABELS = {
  off: 'Audio: Off',
  both: 'Audio: Beep+Voice',
  beep: 'Audio: Beep',
  speech: 'Audio: Voice'
};

export async function mount(container) {
  let destroyed = false;
  let rangeNM = 10;
  let altLimitFt = 2000;
  let headingUp = true;
  let rafId = null;

  const own = {
    lat: 0,
    lon: 0,
    course: 0,
    altFt: null,
    gpsValid: false
  };

  const targets = new Map(); // key -> target record with trails

  container.innerHTML = `
    <div class="radar-toolbar">
      <div class="btn-row">
        <button type="button" class="btn btn-sm" id="rad-range-dec">Range -</button>
        <span class="badge badge-accent mono" id="rad-range-lbl">10 NM</span>
        <button type="button" class="btn btn-sm" id="rad-range-inc">Range +</button>
        <button type="button" class="btn btn-sm" id="rad-alt-dec">Alt -</button>
        <span class="badge mono" id="rad-alt-lbl">\u00B12,000 ft</span>
        <button type="button" class="btn btn-sm" id="rad-alt-inc">Alt +</button>
      </div>
      <div class="btn-row">
        <button type="button" class="btn btn-sm btn-active" id="rad-orient-btn">Track Up</button>
        <button type="button" class="btn btn-sm" id="rad-audio-btn"></button>
        <button type="button" class="btn btn-sm" id="rad-fs-btn">Fullscreen</button>
      </div>
    </div>

    <div class="radar-stage" id="radar-stage">
      <canvas id="radar-canvas" class="radar-canvas"></canvas>
    </div>
  `;

  const stage = container.querySelector('#radar-stage');
  const canvas = container.querySelector('#radar-canvas');
  const ctx = canvas.getContext('2d');

  const lblRange = container.querySelector('#rad-range-lbl');
  const lblAlt = container.querySelector('#rad-alt-lbl');
  const btnOrient = container.querySelector('#rad-orient-btn');
  const btnAudio = container.querySelector('#rad-audio-btn');
  const btnFs = container.querySelector('#rad-fs-btn');

  function syncToolbar() {
    lblRange.textContent = `${rangeNM} NM`;
    lblAlt.textContent = `\u00B1${altLimitFt.toLocaleString()} ft`;
    btnOrient.textContent = headingUp ? 'Track Up' : 'North Up';
    btnAudio.textContent = AUDIO_LABELS[audioAlerts.mode] || 'Audio: Off';
    btnAudio.classList.toggle('btn-active', audioAlerts.mode !== 'off');
  }
  syncToolbar();

  function pushRadarSettings() {
    apiPost('/setSettings', { RadarRange: rangeNM, RadarLimits: altLimitFt }).catch(() => {});
  }

  container.querySelector('#rad-range-dec').addEventListener('click', () => {
    const idx = RANGES_NM.indexOf(rangeNM);
    if (idx > 0) {
      rangeNM = RANGES_NM[idx - 1];
      syncToolbar();
      pushRadarSettings();
    }
  });
  container.querySelector('#rad-range-inc').addEventListener('click', () => {
    const idx = RANGES_NM.indexOf(rangeNM);
    if (idx >= 0 && idx < RANGES_NM.length - 1) {
      rangeNM = RANGES_NM[idx + 1];
      syncToolbar();
      pushRadarSettings();
    }
  });
  container.querySelector('#rad-alt-dec').addEventListener('click', () => {
    const idx = ALT_LIMITS_FT.indexOf(altLimitFt);
    if (idx > 0) {
      altLimitFt = ALT_LIMITS_FT[idx - 1];
      syncToolbar();
      pushRadarSettings();
    }
  });
  container.querySelector('#rad-alt-inc').addEventListener('click', () => {
    const idx = ALT_LIMITS_FT.indexOf(altLimitFt);
    if (idx >= 0 && idx < ALT_LIMITS_FT.length - 1) {
      altLimitFt = ALT_LIMITS_FT[idx + 1];
      syncToolbar();
      pushRadarSettings();
    }
  });

  btnOrient.addEventListener('click', () => {
    headingUp = !headingUp;
    syncToolbar();
  });

  btnAudio.addEventListener('click', () => {
    const mode = audioAlerts.cycleMode();
    syncToolbar();
    if (mode === 'speech' || mode === 'both') audioAlerts.speak('Radar audio active');
    else if (mode === 'beep') audioAlerts.playTone(2);
  });

  btnFs.addEventListener('click', () => {
    if (document.fullscreenElement) {
      document.exitFullscreen?.().catch(() => {});
    } else {
      stage.requestFullscreen?.().catch(() => {});
    }
  });

  function onFsChange() {
    btnFs.textContent = document.fullscreenElement ? 'Exit F/S' : 'Fullscreen';
  }
  document.addEventListener('fullscreenchange', onFsChange);

  function toRelNM(lat1, lon1, lat2, lon2) {
    const R = 3440.065; // Earth radius in NM
    const rad = Math.PI / 180;
    const avgLat = ((lat1 + lat2) / 2) * rad;
    const dLat = (lat2 - lat1) * rad;
    const dLon = (lon2 - lon1) * rad;
    return {
      eastNM: dLon * R * Math.cos(avgLat),
      northNM: dLat * R
    };
  }

  function themeColor(varName, fallback) {
    return getComputedStyle(document.documentElement).getPropertyValue(varName).trim() || fallback;
  }

  function resizeCanvas() {
    const rect = stage.getBoundingClientRect();
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const size = Math.max(240, Math.floor(Math.min(rect.width, rect.height)));
    if (canvas.width !== size * dpr || canvas.height !== size * dpr) {
      canvas.width = size * dpr;
      canvas.height = size * dpr;
    }
    return { size, dpr };
  }

  function draw() {
    if (destroyed) return;
    const { size, dpr } = resizeCanvas();
    ctx.save();
    ctx.scale(dpr, dpr);

    const cx = size / 2;
    const cy = size / 2;
    const radius = size * 0.44;
    const isNight = document.documentElement.getAttribute('data-theme') === 'night';

    const bg = themeColor('--bg-elevated', '#0b0f14');
    const border = themeColor('--border', '#263445');
    const text = themeColor('--text', '#e8eff7');
    const muted = themeColor('--text-muted', '#8fa3ba');
    const accent = themeColor('--accent', '#38bdf8');
    const warn = themeColor('--warn', '#f59e0b');
    const danger = themeColor('--danger', '#ef4444');

    ctx.fillStyle = bg;
    ctx.fillRect(0, 0, size, size);

    // Range rings (outer, 1/2, 1/4)
    ctx.strokeStyle = border;
    ctx.lineWidth = 1;
    for (const frac of [1, 0.5, 0.25]) {
      ctx.beginPath();
      ctx.arc(cx, cy, radius * frac, 0, Math.PI * 2);
      ctx.stroke();
    }

    // Crosshairs
    ctx.beginPath();
    ctx.moveTo(cx - radius, cy);
    ctx.lineTo(cx + radius, cy);
    ctx.moveTo(cx, cy - radius);
    ctx.lineTo(cx, cy + radius);
    ctx.stroke();

    // Cardinal labels (rotated if Track Up)
    const rotRad = headingUp && own.gpsValid ? -(own.course * Math.PI) / 180 : 0;
    ctx.fillStyle = muted;
    ctx.font = 'bold 12px system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    const cardinals = [['N', 0], ['E', 90], ['S', 180], ['W', 270]];
    for (const [lbl, deg] of cardinals) {
      const a = (deg * Math.PI) / 180 + rotRad - Math.PI / 2;
      const rx = cx + (radius + 14) * Math.cos(a);
      const ry = cy + (radius + 14) * Math.sin(a);
      ctx.fillText(lbl, rx, ry);
    }

    // Ring distance labels
    ctx.fillStyle = muted;
    ctx.font = '11px ui-monospace, monospace';
    ctx.textAlign = 'left';
    ctx.fillText(`${(rangeNM / 2).toFixed(rangeNM < 5 ? 1 : 0)} NM`, cx + 4, cy - radius * 0.5 + 12);
    ctx.fillText(`${rangeNM} NM`, cx + 4, cy - radius + 12);

    const now = Date.now();
    // Draw targets
    for (const [key, t] of targets.entries()) {
      if (now - t.updatedAt > RADAR_CUTOFF_MS) {
        targets.delete(key);
        continue;
      }

      const altDiffFt = own.altFt !== null && t.Alt ? t.Alt - own.altFt : null;
      if (altDiffFt !== null && Math.abs(altDiffFt) > altLimitFt) continue;

      if (!t.Position_valid) {
        // Bearingless distance ring
        if (t.DistanceEstimated > 0) {
          const estNM = t.DistanceEstimated / 1852;
          if (estNM <= rangeNM) {
            const rr = Math.max(18, (estNM / rangeNM) * radius);
            ctx.save();
            ctx.setLineDash([5, 5]);
            ctx.strokeStyle = isNight ? '#ff2b2b' : warn;
            ctx.lineWidth = 1.5;
            ctx.beginPath();
            ctx.arc(cx, cy, rr, 0, Math.PI * 2);
            ctx.stroke();
            ctx.fillStyle = isNight ? '#ff2b2b' : warn;
            ctx.font = 'bold 11px ui-monospace, monospace';
            ctx.fillText(`${t.tail} ~${estNM.toFixed(1)}NM`, cx - rr * 0.7, cy - rr * 0.7);
            ctx.restore();
          }
        }
        continue;
      }

      if (!own.gpsValid) continue;

      const rel = toRelNM(own.lat, own.lon, t.Lat, t.Lng);
      const dist = Math.hypot(rel.eastNM, rel.northNM);
      if (dist > rangeNM * 1.05) continue;

      const pxRaw = (rel.eastNM / rangeNM) * radius;
      const pyRaw = -(rel.northNM / rangeNM) * radius;
      const cosR = Math.cos(rotRad);
      const sinR = Math.sin(rotRad);
      const x = cx + pxRaw * cosR - pyRaw * sinR;
      const y = cy + pxRaw * sinR + pyRaw * cosR;

      const color = isNight ? '#ff2b2b' : getCraftHexColor(t);

      // Trail
      if (t.trail.length > 1) {
        ctx.save();
        ctx.strokeStyle = color;
        ctx.globalAlpha = 0.45;
        ctx.lineWidth = 1.5;
        ctx.beginPath();
        t.trail.forEach((pt, idx) => {
          const rpt = toRelNM(own.lat, own.lon, pt[0], pt[1]);
          const tx = (rpt.eastNM / rangeNM) * radius;
          const ty = -(rpt.northNM / rangeNM) * radius;
          const sx = cx + tx * cosR - ty * sinR;
          const sy = cy + tx * sinR + ty * cosR;
          if (idx === 0) ctx.moveTo(sx, sy);
          else ctx.lineTo(sx, sy);
        });
        ctx.stroke();
        ctx.restore();
      }

      // CPA Threat Vector (1-min velocity projection)
      if (t.Speed_valid && t.Speed > 5) {
        const nmPerMin = t.Speed / 60;
        const vecLenPx = Math.min(radius * 0.5, (nmPerMin / rangeNM) * radius);
        const trkRad = ((t.Track || 0) * Math.PI) / 180 + rotRad - Math.PI / 2;
        const vx = x + vecLenPx * Math.cos(trkRad);
        const vy = y + vecLenPx * Math.sin(trkRad);
        ctx.save();
        ctx.strokeStyle = t.AlertLevel >= 3 ? danger : t.AlertLevel === 2 ? warn : color;
        ctx.lineWidth = t.AlertLevel >= 2 ? 2.5 : 1.2;
        ctx.beginPath();
        ctx.moveTo(x, y);
        ctx.lineTo(vx, vy);
        ctx.stroke();
        ctx.restore();
      }

      // CPA Halo
      if (t.AlertLevel >= 2) {
        ctx.save();
        ctx.strokeStyle = t.AlertLevel >= 3 ? danger : warn;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(x, y, 13, 0, Math.PI * 2);
        ctx.stroke();
        ctx.restore();
      }

      // Aircraft Chevron
      ctx.save();
      ctx.translate(x, y);
      const hdgRad = ((t.Track || 0) * Math.PI) / 180 + rotRad;
      ctx.rotate(hdgRad);
      ctx.fillStyle = color;
      ctx.beginPath();
      ctx.moveTo(0, -8);
      ctx.lineTo(6, 7);
      ctx.lineTo(0, 3.5);
      ctx.lineTo(-6, 7);
      ctx.closePath();
      ctx.fill();
      ctx.restore();

      // Label: altitude diff + tail
      const arrow = t.Vvel > 150 ? '\u2191' : t.Vvel < -150 ? '\u2193' : '';
      let altTxt = '';
      if (altDiffFt !== null) {
        const h = Math.round(altDiffFt / 100);
        altTxt = `${h >= 0 ? '+' : ''}${h}${arrow}`;
      } else {
        altTxt = `FL${Math.round((t.Alt || 0) / 100)}${arrow}`;
      }

      ctx.fillStyle = text;
      ctx.font = 'bold 11px ui-monospace, monospace';
      ctx.textAlign = 'left';
      ctx.fillText(altTxt, x + 10, y - 4);
      ctx.fillStyle = muted;
      ctx.font = '10px ui-monospace, monospace';
      ctx.fillText(t.tail, x + 10, y + 8);
    }

    // Ownship symbol at center
    ctx.save();
    ctx.translate(cx, cy);
    if (!headingUp && own.gpsValid) {
      ctx.rotate((own.course * Math.PI) / 180);
    }
    ctx.strokeStyle = accent;
    ctx.lineWidth = 2.4;
    ctx.beginPath();
    ctx.moveTo(0, -10);
    ctx.lineTo(0, 10);
    ctx.moveTo(-9, -1);
    ctx.lineTo(9, -1);
    ctx.moveTo(-4, 8);
    ctx.lineTo(4, 8);
    ctx.stroke();
    ctx.restore();

    // HUD Corner Readouts
    ctx.fillStyle = text;
    ctx.font = 'bold 12px ui-monospace, monospace';
    ctx.textAlign = 'left';
    ctx.fillText(
      own.gpsValid ? `HDG ${String(Math.round(own.course)).padStart(3, '0')}\u00B0` : 'NO GPS FIX',
      12, 20
    );
    ctx.textAlign = 'right';
    ctx.fillText(
      own.altFt !== null ? `ALT ${Math.round(own.altFt)} ft` : 'ALT ---',
      size - 12, 20
    );
    ctx.textAlign = 'left';
    ctx.fillStyle = muted;
    ctx.fillText(`Targets: ${targets.size}`, 12, size - 12);

    ctx.restore();
    rafId = requestAnimationFrame(draw);
  }

  const radarWS = new StratuxWS('/radar', {
    onOpen: () => setConnectionBadge('connected', 'Live'),
    onClose: () => setConnectionBadge('reconnecting', 'Reconnecting'),
    onError: () => setConnectionBadge('error', 'Error'),
    onMessage: (msg) => {
      if (destroyed || !msg) return;
      if ('RadarRange' in msg || 'RadarLimits' in msg) {
        if (RANGES_NM.includes(msg.RadarRange)) rangeNM = msg.RadarRange;
        if (ALT_LIMITS_FT.includes(msg.RadarLimits)) altLimitFt = msg.RadarLimits;
        syncToolbar();
        return;
      }
      if (msg.Icao_addr === undefined) return;
      const key = `${msg.Icao_addr}:${msg.Addr_type === 1 ? 1 : 0}`;
      const existing = targets.get(key);
      const trail = existing ? existing.trail : [];
      if (msg.Position_valid && typeof msg.Lat === 'number' && typeof msg.Lng === 'number') {
        const last = trail[trail.length - 1];
        if (!last || Math.abs(last[0] - msg.Lat) > 0.0004 || Math.abs(last[1] - msg.Lng) > 0.0004) {
          trail.push([msg.Lat, msg.Lng]);
          if (trail.length > MAX_TRAIL_POINTS) trail.shift();
        }
      }
      const hex = (msg.Icao_addr >>> 0).toString(16).toUpperCase().padStart(6, '0');
      const tail = (msg.Tail && msg.Tail.trim()) ? msg.Tail.trim() : hex;
      const rec = {
        ...msg,
        tail,
        trail,
        updatedAt: Date.now()
      };
      targets.set(key, rec);

      if ((msg.AlertLevel || 0) >= 2) {
        audioAlerts.announceTraffic(key, msg.AlertLevel, msg.Callout || `Traffic ${tail}`);
      }
    }
  });

  const sitWS = new StratuxWS('/situation', {
    onMessage: (sit) => {
      if (destroyed || !sit) return;
      own.gpsValid = Boolean(sit.GPSHorizontalAccuracy < 19999 && (sit.GPSLatitude !== 0 || sit.GPSLongitude !== 0));
      if (own.gpsValid) {
        own.lat = sit.GPSLatitude;
        own.lon = sit.GPSLongitude;
        own.course = sit.GPSTrueCourse || 0;
      }
      const pressValid = isNonZeroTimestamp(sit.BaroLastMeasurementTime);
      const gpsTimeValid = isNonZeroTimestamp(sit.GPSLastGPSTimeStratuxTime);
      if (pressValid && gpsTimeValid && (Date.parse(sit.GPSLastGPSTimeStratuxTime) - Date.parse(sit.BaroLastMeasurementTime) < 1000)) {
        own.altFt = sit.BaroPressureAltitude;
      } else if (own.gpsValid) {
        own.altFt = sit.GPSAltitudeMSL;
      }
    }
  });

  rafId = requestAnimationFrame(draw);

  return function destroy() {
    destroyed = true;
    if (rafId !== null) cancelAnimationFrame(rafId);
    document.removeEventListener('fullscreenchange', onFsChange);
    radarWS.close();
    sitWS.close();
  };
}
