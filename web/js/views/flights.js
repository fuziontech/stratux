import {
  apiGet,
  createEl,
  formatUptime,
  isValidAhrsAngle,
  normalizeHeadingDeg,
  setConnectionBadge,
  showToast
} from '../app.js';

export async function mount(container) {
  let destroyed = false;
  let flights = [];
  let selectedTrack = null;
  let cursorIdx = 0;
  let playing = false;
  let playTimer = null;

  container.innerHTML = `
    <section class="card">
      <div class="card-header">
        <h2 class="card-title">Recorded Flights (SQLite Auto-Segmented)</h2>
        <div class="btn-row">
          <a class="btn btn-sm" href="/downloaddb" download>Download SQLite DB</a>
          <button type="button" class="btn btn-sm btn-primary" id="fl-refresh">Refresh</button>
        </div>
      </div>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>#</th>
              <th>Start (UTC)</th>
              <th>Duration</th>
              <th>Distance</th>
              <th>Max Alt</th>
              <th>Max Spd</th>
              <th>Points</th>
              <th>Export / Replay</th>
            </tr>
          </thead>
          <tbody id="fl-tbody"></tbody>
        </table>
      </div>
    </section>

    <section class="card hidden" id="fl-replay-card">
      <div class="card-header">
        <h2 class="card-title" id="fl-replay-title">Flight Track Replay</h2>
        <div class="btn-row">
          <button type="button" class="btn btn-sm btn-primary" id="fl-play-btn">Play</button>
          <span class="badge mono" id="fl-cursor-lbl">0 / 0</span>
        </div>
      </div>

      <div class="card-grid-2">
        <div>
          <div class="metric-label" style="margin-bottom:6px;">2D Ground Track</div>
          <canvas id="fl-track-canvas" class="flight-canvas"></canvas>
        </div>
        <div>
          <div class="metric-label" style="margin-bottom:6px;">Altitude &amp; Speed Profile</div>
          <canvas id="fl-profile-canvas" class="flight-canvas"></canvas>
        </div>
      </div>

      <div style="margin-top:12px;">
        <input type="range" id="fl-scrubber" class="scrubber-range" min="0" max="0" value="0" aria-label="Flight timeline scrubber" />
      </div>

      <div class="metric-grid" id="fl-point-metrics" style="margin-top:10px;"></div>
    </section>
  `;

  const elTbody = container.querySelector('#fl-tbody');
  const elReplayCard = container.querySelector('#fl-replay-card');
  const elReplayTitle = container.querySelector('#fl-replay-title');
  const elPlayBtn = container.querySelector('#fl-play-btn');
  const elCursorLbl = container.querySelector('#fl-cursor-lbl');
  const elScrubber = container.querySelector('#fl-scrubber');
  const elPointMetrics = container.querySelector('#fl-point-metrics');
  const trackCanvas = container.querySelector('#fl-track-canvas');
  const profileCanvas = container.querySelector('#fl-profile-canvas');

  container.querySelector('#fl-refresh').addEventListener('click', loadFlights);

  function stopPlayback() {
    playing = false;
    if (playTimer !== null) {
      clearInterval(playTimer);
      playTimer = null;
    }
    elPlayBtn.textContent = 'Play';
  }

  elPlayBtn.addEventListener('click', () => {
    if (!selectedTrack || !selectedTrack.Points?.length) return;
    if (playing) {
      stopPlayback();
      return;
    }
    playing = true;
    elPlayBtn.textContent = 'Pause';
    playTimer = setInterval(() => {
      if (destroyed || !selectedTrack) { stopPlayback(); return; }
      const pts = selectedTrack.Points;
      cursorIdx = (cursorIdx + 1) % pts.length;
      elScrubber.value = String(cursorIdx);
      renderReplay();
    }, 120);
  });

  elScrubber.addEventListener('input', () => {
    cursorIdx = parseInt(elScrubber.value, 10) || 0;
    renderReplay();
  });

  function formatDuration(sec) {
    const s = Number(sec);
    if (!Number.isFinite(s) || s < 0) return '--';
    return formatUptime(s * 1000).replace(/^0d /, '');
  }

  function themeColor(varName, fallback) {
    return getComputedStyle(document.documentElement).getPropertyValue(varName).trim() || fallback;
  }

  function prepCanvas(canvas) {
    const rect = canvas.getBoundingClientRect();
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const w = Math.max(260, Math.floor(rect.width || 360));
    const h = Math.max(180, Math.floor(rect.height || 240));
    if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
      canvas.width = w * dpr;
      canvas.height = h * dpr;
    }
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    return { ctx, w, h };
  }

  function renderReplay() {
    if (!selectedTrack || !Array.isArray(selectedTrack.Points) || selectedTrack.Points.length === 0) return;
    const pts = selectedTrack.Points;
    const idx = Math.max(0, Math.min(pts.length - 1, cursorIdx));
    const pt = pts[idx];

    elCursorLbl.textContent = `${idx + 1} / ${pts.length}`;

    // Point telemetry readout
    elPointMetrics.textContent = '';
    const baroValid = typeof pt.BaroAltFt === 'number' && Number.isFinite(pt.BaroAltFt) && Math.abs(pt.BaroAltFt) < 100000;
    const pitchValid = isValidAhrsAngle(pt.PitchDeg, 90);
    const rollValid = isValidAhrsAngle(pt.RollDeg, 180);
    const trackNorm = normalizeHeadingDeg(pt.TrackDeg);
    const metrics = [
      ['Time', pt.Time ? String(pt.Time).slice(11, 19) + 'Z' : '--'],
      ['GPS Alt MSL', Number.isFinite(pt.AltMSLFt) ? `${Math.round(pt.AltMSLFt)} ft` : '--'],
      ['Baro Alt', baroValid ? `${Math.round(pt.BaroAltFt)} ft` : '--'],
      ['Speed', Number.isFinite(pt.SpeedKt) ? `${Math.round(pt.SpeedKt)} kt` : '--'],
      ['Track', trackNorm !== null ? `${String(trackNorm).padStart(3, '0')}\u00B0` : '--'],
      ['Pitch / Roll', `${pitchValid ? pt.PitchDeg.toFixed(1) + '\u00B0' : '--'} / ${rollValid ? pt.RollDeg.toFixed(1) + '\u00B0' : '--'}`]
    ];
    for (const [k, v] of metrics) {
      const m = createEl('div', { className: 'metric' });
      m.appendChild(createEl('div', { className: 'metric-label', text: k }));
      m.appendChild(createEl('div', { className: 'metric-value', text: v }));
      elPointMetrics.appendChild(m);
    }

    const bg = themeColor('--bg', '#0b0f14');
    const border = themeColor('--border', '#263445');
    const accent = themeColor('--accent', '#38bdf8');
    const warn = themeColor('--warn', '#f59e0b');
    const ok = themeColor('--ok', '#22c55e');
    const muted = themeColor('--text-muted', '#8fa3ba');

    // 1) 2D Ground Track Canvas
    const { ctx: tctx, w: tw, h: th } = prepCanvas(trackCanvas);
    tctx.fillStyle = bg;
    tctx.fillRect(0, 0, tw, th);

    let minLat = Infinity, maxLat = -Infinity, minLon = Infinity, maxLon = -Infinity;
    for (const p of pts) {
      if (p.Lat < minLat) minLat = p.Lat;
      if (p.Lat > maxLat) maxLat = p.Lat;
      if (p.Lon < minLon) minLon = p.Lon;
      if (p.Lon > maxLon) maxLon = p.Lon;
    }
    const spanLon = Math.max(0.002, maxLon - minLon);
    const spanLat = Math.max(0.002, maxLat - minLat);
    const pad = 24;
    const scale = Math.min((tw - pad * 2) / spanLon, (th - pad * 2) / spanLat);
    const ox = (tw - spanLon * scale) / 2;
    const oy = (th - spanLat * scale) / 2;
    const toXY = (p) => [
      ox + (p.Lon - minLon) * scale,
      th - oy - (p.Lat - minLat) * scale
    ];

    tctx.strokeStyle = accent;
    tctx.lineWidth = 2;
    tctx.beginPath();
    pts.forEach((p, i) => {
      const [x, y] = toXY(p);
      if (i === 0) tctx.moveTo(x, y);
      else tctx.lineTo(x, y);
    });
    tctx.stroke();

    // Start & End dots
    const [sx, sy] = toXY(pts[0]);
    tctx.fillStyle = ok;
    tctx.beginPath();
    tctx.arc(sx, sy, 5, 0, Math.PI * 2);
    tctx.fill();

    // Active aircraft dot
    const [cx, cy] = toXY(pt);
    tctx.fillStyle = warn;
    tctx.beginPath();
    tctx.arc(cx, cy, 6, 0, Math.PI * 2);
    tctx.fill();

    // 2) Altitude & Speed Profile Canvas
    const { ctx: pctx, w: pw, h: ph } = prepCanvas(profileCanvas);
    pctx.fillStyle = bg;
    pctx.fillRect(0, 0, pw, ph);

    const maxAlt = Math.max(500, ...pts.map((p) => Number(p.AltMSLFt) || 0));
    const maxSpd = Math.max(50, ...pts.map((p) => Number(p.SpeedKt) || 0));
    const ppad = 24;

    // Grid lines
    pctx.strokeStyle = border;
    pctx.lineWidth = 1;
    for (let g = 0; g <= 4; g++) {
      const gy = ppad + ((ph - ppad * 2) * g) / 4;
      pctx.beginPath();
      pctx.moveTo(ppad, gy);
      pctx.lineTo(pw - 10, gy);
      pctx.stroke();
    }

    // Altitude line
    pctx.strokeStyle = accent;
    pctx.lineWidth = 2;
    pctx.beginPath();
    pts.forEach((p, i) => {
      const x = ppad + (i / Math.max(1, pts.length - 1)) * (pw - ppad - 10);
      const y = ph - ppad - ((Math.max(0, Number(p.AltMSLFt) || 0) / maxAlt) * (ph - ppad * 2));
      if (i === 0) pctx.moveTo(x, y);
      else pctx.lineTo(x, y);
    });
    pctx.stroke();

    // Speed line
    pctx.strokeStyle = ok;
    pctx.lineWidth = 1.5;
    pctx.beginPath();
    pts.forEach((p, i) => {
      const x = ppad + (i / Math.max(1, pts.length - 1)) * (pw - ppad - 10);
      const y = ph - ppad - ((Math.max(0, Number(p.SpeedKt) || 0) / maxSpd) * (ph - ppad * 2));
      if (i === 0) pctx.moveTo(x, y);
      else pctx.lineTo(x, y);
    });
    pctx.stroke();

    // Cursor line
    const curX = ppad + (idx / Math.max(1, pts.length - 1)) * (pw - ppad - 10);
    pctx.strokeStyle = warn;
    pctx.lineWidth = 2;
    pctx.beginPath();
    pctx.moveTo(curX, ppad);
    pctx.lineTo(curX, ph - ppad);
    pctx.stroke();

    pctx.fillStyle = muted;
    pctx.font = '11px ui-monospace, monospace';
    pctx.fillText(`Alt max ${Math.round(maxAlt)} ft`, ppad + 4, 15);
    pctx.fillText(`Spd max ${Math.round(maxSpd)} kt`, pw - 115, 15);
  }

  async function openFlight(flightId) {
    stopPlayback();
    try {
      const data = await apiGet(`/getFlightTrack?id=${encodeURIComponent(flightId)}`);
      if (destroyed) return;
      if (!data || !Array.isArray(data.Points) || data.Points.length === 0) {
        showToast('No track points found for this flight', 'info');
        return;
      }
      selectedTrack = data;
      cursorIdx = 0;
      elReplayTitle.textContent = `Flight #${flightId} Replay (${data.Points.length} points)`;
      elScrubber.max = String(Math.max(0, data.Points.length - 1));
      elScrubber.value = '0';
      elReplayCard.classList.remove('hidden');
      renderReplay();
    } catch (err) {
      showToast(`Failed to load flight track: ${err.message}`, 'error');
    }
  }

  function renderTable() {
    elTbody.textContent = '';
    if (!flights || flights.length === 0) {
      elTbody.appendChild(createEl('tr', {}, createEl('td', {
        colspan: '8',
        className: 'muted',
        text: 'No recorded flights available yet (enable Replay/Persistent logging in Settings, or check backend /getFlights support).'
      })));
      return;
    }

    for (const f of flights) {
      const tr = createEl('tr', {});
      tr.appendChild(createEl('td', { className: 'mono', text: `#${f.ID}` }));
      tr.appendChild(createEl('td', { className: 'mono', text: f.StartTime ? String(f.StartTime).replace('T', ' ').slice(0, 19) : '--' }));
      tr.appendChild(createEl('td', { className: 'mono', text: formatDuration(f.DurationSec) }));
      tr.appendChild(createEl('td', { className: 'mono', text: `${Number(f.DistanceNM || 0).toFixed(1)} NM` }));
      tr.appendChild(createEl('td', { className: 'mono', text: `${Math.round(f.MaxAltFt || 0)} ft` }));
      tr.appendChild(createEl('td', { className: 'mono', text: `${Math.round(f.MaxSpeedKt || 0)} kt` }));
      tr.appendChild(createEl('td', { className: 'mono', text: String(f.PointCount ?? 0) }));

      const actions = createEl('div', { className: 'btn-row' });
      actions.appendChild(createEl('button', {
        type: 'button',
        className: 'btn btn-sm btn-primary',
        text: 'Replay',
        onClick: () => openFlight(f.ID)
      }));
      for (const fmt of ['igc', 'gpx', 'kml', 'csv']) {
        actions.appendChild(createEl('a', {
          className: 'btn btn-sm',
          href: `/exportFlight?id=${encodeURIComponent(f.ID)}&format=${fmt}`,
          download: `flight_${f.ID}.${fmt}`,
          text: fmt.toUpperCase()
        }));
      }
      tr.appendChild(createEl('td', {}, actions));
      elTbody.appendChild(tr);
    }
  }

  async function loadFlights() {
    try {
      const res = await apiGet('/getFlights', { optional: true });
      if (destroyed) return;
      if (res === null) {
        flights = [];
        setConnectionBadge('connected', 'Live');
      } else {
        flights = Array.isArray(res) ? res : [];
        setConnectionBadge('connected', 'Live');
      }
      renderTable();
    } catch (err) {
      if (!destroyed) {
        setConnectionBadge('disconnected', 'Offline');
        showToast(`Could not load flights: ${err.message}`, 'error');
      }
    }
  }

  loadFlights();

  return function destroy() {
    destroyed = true;
    stopPlayback();
  };
}
