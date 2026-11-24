import {
  apiGet,
  apiPost,
  apiPostRaw,
  createEl,
  getGpsHardwareLabel,
  isNonZeroTimestamp,
  isValidAhrsAngle,
  isValidAhrsGLoad,
  isValidGpsAccuracy,
  normalizeHeadingDeg,
  setConnectionBadge,
  showToast,
  StratuxWS,
  WakeLockManager
} from '../app.js';

export async function mount(container) {
  let destroyed = false;
  let gMinLim = -1.76;
  let gMaxLim = 4.4;
  let lastStatus = null;
  let lastSituation = null;
  const wakeLock = new WakeLockManager();

  container.innerHTML = `
    <div class="radar-toolbar">
      <div class="btn-row">
        <span class="badge" id="ahrs-pill-gps">GPS</span>
        <span class="badge" id="ahrs-pill-imu">IMU</span>
        <span class="badge" id="ahrs-pill-bmp">BARO</span>
        <span class="badge" id="ahrs-pill-cal">Ready</span>
      </div>
      <div class="btn-row">
        <button type="button" class="btn btn-sm" id="btn-wakelock">Screen Wake Lock: Off</button>
        <button type="button" class="btn btn-sm" id="btn-cage">Level / Cage</button>
        <button type="button" class="btn btn-sm" id="btn-calibrate">Calibrate</button>
        <button type="button" class="btn btn-sm" id="btn-orient">Set Orientation</button>
        <button type="button" class="btn btn-sm" id="btn-reset-g">Reset G</button>
      </div>
    </div>

    <div id="gps-diag-banner" class="alert-banner alert-info hidden"></div>

    <div class="ahrs-grid">
      <section class="card">
        <div class="card-header">
          <h2 class="card-title">Artificial Horizon</h2>
          <span class="badge mono" id="ahrs-hdg-badge">HDG ---\u00B0</span>
        </div>
        <svg id="horizon-svg" class="horizon-svg" viewBox="-160 -160 320 320" role="img" aria-label="Artificial Horizon">
          <defs>
            <clipPath id="ahrs-clip"><circle cx="0" cy="0" r="146" /></clipPath>
          </defs>
          <g clip-path="url(#ahrs-clip)">
            <g id="ahrs-world">
              <rect x="-400" y="-420" width="800" height="420" fill="#1d6fa5" id="ahrs-sky" />
              <rect x="-400" y="0" width="800" height="420" fill="#6b4423" id="ahrs-ground" />
              <line x1="-400" y1="0" x2="400" y2="0" stroke="#ffffff" stroke-width="2.5" />
              <g id="ahrs-pitch-ladder" stroke="#ffffff" stroke-width="1.5" fill="#ffffff" font-size="11" font-family="ui-monospace, monospace" text-anchor="middle"></g>
            </g>
          </g>
          <!-- Bank scale ring -->
          <circle cx="0" cy="0" r="146" fill="none" stroke="var(--border)" stroke-width="4" />
          <g id="ahrs-bank-marks" stroke="#ffffff" stroke-width="2"></g>
          <!-- Fixed aircraft boresight -->
          <path d="M -52 0 L -16 0 L -16 10 M 16 10 L 16 0 L 52 0" fill="none" stroke="#facc15" stroke-width="4" stroke-linecap="round" />
          <circle cx="0" cy="0" r="4" fill="#facc15" />
          <!-- Slip/Skid inclinometer -->
          <rect x="-48" y="114" width="96" height="18" rx="9" fill="rgba(0,0,0,0.65)" stroke="#ffffff" stroke-width="1.2" />
          <line x1="-10" y1="114" x2="-10" y2="132" stroke="#ffffff" stroke-width="1.2" />
          <line x1="10" y1="114" x2="10" y2="132" stroke="#ffffff" stroke-width="1.2" />
          <circle id="ahrs-slip-ball" cx="0" cy="123" r="7" fill="#ffffff" />
        </svg>

        <div class="metric-grid" style="margin-top:12px;">
          <div class="metric"><div class="metric-label">Pitch</div><div class="metric-value" id="val-pitch">--</div></div>
          <div class="metric"><div class="metric-label">Roll</div><div class="metric-value" id="val-roll">--</div></div>
          <div class="metric"><div class="metric-label">Slip/Skid</div><div class="metric-value" id="val-slip">--</div></div>
          <div class="metric"><div class="metric-label">Baro Alt</div><div class="metric-value" id="val-baro">--</div></div>
        </div>
      </section>

      <div>
        <div class="card-grid">
          <section class="card">
            <div class="card-header">
              <h2 class="card-title">GPS Fix</h2>
              <span class="badge" id="gps-fix-badge">--</span>
            </div>
            <div class="kv-list" id="gps-kv"></div>
          </section>

          <section class="card">
            <div class="card-header">
              <h2 class="card-title">G-Meter</h2>
              <span class="badge mono" id="g-current-badge">1.00 G</span>
            </div>
            <svg id="gmeter-svg" class="gmeter-svg" viewBox="-100 -100 200 200" role="img" aria-label="G-Meter">
              <circle cx="0" cy="0" r="88" fill="var(--bg)" stroke="var(--border)" stroke-width="4" />
              <g id="g-ticks" stroke="var(--text-muted)" stroke-width="1.5"></g>
              <line id="g-min-needle" x1="0" y1="0" x2="0" y2="-70" stroke="var(--accent)" stroke-width="2" stroke-dasharray="4 3" />
              <line id="g-max-needle" x1="0" y1="0" x2="0" y2="-70" stroke="var(--danger)" stroke-width="2" stroke-dasharray="4 3" />
              <line id="g-needle" x1="0" y1="12" x2="0" y2="-74" stroke="var(--text)" stroke-width="3.5" stroke-linecap="round" />
              <circle cx="0" cy="0" r="6" fill="var(--accent)" />
            </svg>
            <div class="kv-list" style="margin-top:8px;">
              <div class="kv-row"><span class="kv-key">Min / Max Load</span><span class="kv-val mono" id="g-minmax">-- / --</span></div>
              <div class="kv-row"><span class="kv-key">Limits</span><span class="kv-val mono" id="g-limits">-1.76 / +4.40 G</span></div>
            </div>
          </section>
        </div>

        <section class="card">
          <div class="card-header">
            <h2 class="card-title">GNSS Constellation C/N0 (dB-Hz)</h2>
            <span class="badge mono" id="sat-count-badge">0 Sats</span>
          </div>
          <div class="sat-chart" id="sat-chart"></div>
          <div class="table-wrap" style="margin-top:10px; max-height:220px; overflow-y:auto;">
            <table class="data-table">
              <thead><tr><th>Sat ID</th><th>Signal (dB)</th><th>Elev</th><th>Azim</th><th>In Fix</th></tr></thead>
              <tbody id="sat-tbody"></tbody>
            </table>
          </div>
        </section>
      </div>
    </div>

    <div id="orient-modal-host"></div>
  `;

  // Build pitch ladder & bank marks once
  const pitchLadder = container.querySelector('#ahrs-pitch-ladder');
  const svgNS = 'http://www.w3.org/2000/svg';
  for (let deg = -30; deg <= 30; deg += 5) {
    if (deg === 0) continue;
    const y = -deg * 4;
    const halfW = deg % 10 === 0 ? 34 : 18;
    const line = document.createElementNS(svgNS, 'line');
    line.setAttribute('x1', String(-halfW));
    line.setAttribute('x2', String(halfW));
    line.setAttribute('y1', String(y));
    line.setAttribute('y2', String(y));
    pitchLadder.appendChild(line);
    if (deg % 10 === 0) {
      const txt = document.createElementNS(svgNS, 'text');
      txt.setAttribute('x', String(halfW + 14));
      txt.setAttribute('y', String(y + 4));
      txt.textContent = String(Math.abs(deg));
      pitchLadder.appendChild(txt);
    }
  }
  const bankMarks = container.querySelector('#ahrs-bank-marks');
  for (const ang of [-60, -45, -30, -20, -10, 0, 10, 20, 30, 45, 60]) {
    const r1 = 136;
    const r2 = ang % 30 === 0 ? 146 : 142;
    const rad = ((ang - 90) * Math.PI) / 180;
    const line = document.createElementNS(svgNS, 'line');
    line.setAttribute('x1', (r1 * Math.cos(rad)).toFixed(1));
    line.setAttribute('y1', (r1 * Math.sin(rad)).toFixed(1));
    line.setAttribute('x2', (r2 * Math.cos(rad)).toFixed(1));
    line.setAttribute('y2', (r2 * Math.sin(rad)).toFixed(1));
    bankMarks.appendChild(line);
  }

  // G-meter scale ticks (-3G to +6G mapped to -135deg..+135deg)
  const gTicks = container.querySelector('#g-ticks');
  const gToDeg = (g) => {
    const clamped = Math.max(-3, Math.min(6, g));
    return -135 + ((clamped + 3) / 9) * 270;
  };
  for (let g = -3; g <= 6; g++) {
    const rad = ((gToDeg(g) - 90) * Math.PI) / 180;
    const line = document.createElementNS(svgNS, 'line');
    line.setAttribute('x1', (72 * Math.cos(rad)).toFixed(1));
    line.setAttribute('y1', (72 * Math.sin(rad)).toFixed(1));
    line.setAttribute('x2', (84 * Math.cos(rad)).toFixed(1));
    line.setAttribute('y2', (84 * Math.sin(rad)).toFixed(1));
    gTicks.appendChild(line);
    const lbl = document.createElementNS(svgNS, 'text');
    lbl.setAttribute('x', (58 * Math.cos(rad)).toFixed(1));
    lbl.setAttribute('y', (58 * Math.sin(rad) + 4).toFixed(1));
    lbl.setAttribute('text-anchor', 'middle');
    lbl.setAttribute('font-size', '11');
    lbl.setAttribute('fill', 'var(--text-muted)');
    lbl.textContent = String(g);
    gTicks.appendChild(lbl);
  }

  const elWorld = container.querySelector('#ahrs-world');
  const elSlipBall = container.querySelector('#ahrs-slip-ball');
  const elHdgBadge = container.querySelector('#ahrs-hdg-badge');
  const elPitch = container.querySelector('#val-pitch');
  const elRoll = container.querySelector('#val-roll');
  const elSlip = container.querySelector('#val-slip');
  const elBaro = container.querySelector('#val-baro');
  const elGpsDiag = container.querySelector('#gps-diag-banner');
  const elGpsFixBadge = container.querySelector('#gps-fix-badge');
  const elGpsKv = container.querySelector('#gps-kv');
  const elSatChart = container.querySelector('#sat-chart');
  const elSatBody = container.querySelector('#sat-tbody');
  const elSatCount = container.querySelector('#sat-count-badge');
  const elPillGps = container.querySelector('#ahrs-pill-gps');
  const elPillImu = container.querySelector('#ahrs-pill-imu');
  const elPillBmp = container.querySelector('#ahrs-pill-bmp');
  const elPillCal = container.querySelector('#ahrs-pill-cal');
  const elGCur = container.querySelector('#g-current-badge');
  const elGMinMax = container.querySelector('#g-minmax');
  const elGLimits = container.querySelector('#g-limits');
  const elGNeedle = container.querySelector('#g-needle');
  const elGMinNeedle = container.querySelector('#g-min-needle');
  const elGMaxNeedle = container.querySelector('#g-max-needle');
  const btnWakeLock = container.querySelector('#btn-wakelock');
  const elModalHost = container.querySelector('#orient-modal-host');

  // Night theme sky/ground adaptation
  function applyHorizonTheme() {
    const isNight = document.documentElement.getAttribute('data-theme') === 'night';
    container.querySelector('#ahrs-sky').setAttribute('fill', isNight ? '#1a0000' : '#1d6fa5');
    container.querySelector('#ahrs-ground').setAttribute('fill', isNight ? '#330404' : '#6b4423');
  }
  applyHorizonTheme();
  const themeObserver = new MutationObserver(applyHorizonTheme);
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

  // Native Screen Wake Lock toggle
  wakeLock.onChange = (active) => {
    btnWakeLock.textContent = active ? 'Screen Wake Lock: ON' : 'Screen Wake Lock: Off';
    btnWakeLock.classList.toggle('btn-active', active);
  };
  if (!wakeLock.isSupported()) {
    btnWakeLock.textContent = 'Wake Lock Unsupported';
    btnWakeLock.disabled = true;
  } else {
    btnWakeLock.addEventListener('click', () => {
      if (wakeLock.isActive()) wakeLock.release();
      else wakeLock.acquire();
    });
  }

  container.querySelector('#btn-cage').addEventListener('click', () => {
    apiPost('/cageAHRS')
      .then(() => showToast('AHRS caged / leveled', 'ok'))
      .catch((err) => showToast(`Cage failed: ${err.message}`, 'error'));
  });
  container.querySelector('#btn-calibrate').addEventListener('click', () => {
    apiPost('/calibrateAHRS')
      .then(() => showToast('AHRS calibration started', 'ok'))
      .catch((err) => showToast(`Calibration failed: ${err.message}`, 'error'));
  });
  container.querySelector('#btn-reset-g').addEventListener('click', () => {
    apiPost('/resetGMeter')
      .then(() => showToast('G-Meter reset', 'ok'))
      .catch((err) => showToast(`G-Meter reset failed: ${err.message}`, 'error'));
  });
  container.querySelector('#btn-orient').addEventListener('click', openOrientWizard);

  function openOrientWizard() {
    elModalHost.textContent = '';
    const backdrop = createEl('div', { className: 'modal-backdrop' });
    const card = createEl('div', { className: 'modal-card' });
    card.appendChild(createEl('h3', { className: 'modal-title', text: 'AHRS Orientation Step 1: Forward' }));
    card.appendChild(createEl('p', {
      text: 'Point the nose-forward side of the Stratux straight up toward the sky, hold still, and press Set Forward.'
    }));
    const actions = createEl('div', { className: 'btn-row', style: { marginTop: '14px', justifyContent: 'flex-end' } });
    actions.appendChild(createEl('button', { type: 'button', className: 'btn btn-sm', text: 'Cancel', onClick: () => { elModalHost.textContent = ''; } }));
    actions.appendChild(createEl('button', {
      type: 'button',
      className: 'btn btn-primary btn-sm',
      text: 'Set Forward',
      onClick: async () => {
        try {
          await apiPostRaw('/orientAHRS', 'forward');
          card.textContent = '';
          card.appendChild(createEl('h3', { className: 'modal-title', text: 'AHRS Orientation Step 2: Level' }));
          card.appendChild(createEl('p', {
            text: 'Place the Stratux in its normal in-flight orientation, keep it stationary, and press Done.'
          }));
          const doneRow = createEl('div', { className: 'btn-row', style: { marginTop: '14px', justifyContent: 'flex-end' } });
          doneRow.appendChild(createEl('button', {
            type: 'button',
            className: 'btn btn-primary btn-sm',
            text: 'Done',
            onClick: async () => {
              try {
                await apiPostRaw('/orientAHRS', 'done');
                showToast('AHRS orientation saved', 'ok');
              } catch (e) {
                showToast(`Orientation failed: ${e.message}`, 'error');
              }
              elModalHost.textContent = '';
            }
          }));
          card.appendChild(doneRow);
        } catch (err) {
          showToast(`Orientation failed: ${err.message}`, 'error');
          elModalHost.textContent = '';
        }
      }
    }));
    card.appendChild(actions);
    backdrop.appendChild(card);
    elModalHost.appendChild(backdrop);
  }

  function renderDiagBanner(sit) {
    if (!elGpsDiag) return;
    const connected = lastStatus ? Boolean(lastStatus.GPS_connected) : isNonZeroTimestamp(sit?.GPSLastValidNMEAMessageTime);
    const fixQ = sit?.GPSFixQuality || 0;
    const seen = sit?.GPSSatellitesSeen ?? lastStatus?.GPS_satellites_seen ?? 0;
    const tracked = sit?.GPSSatellitesTracked ?? lastStatus?.GPS_satellites_tracked ?? 0;

    if (connected && fixQ >= 1) {
      elGpsDiag.classList.add('hidden');
      return;
    }

    elGpsDiag.classList.remove('hidden');
    if (!connected) {
      elGpsDiag.className = 'alert-banner alert-warn';
      elGpsDiag.textContent =
        'GPS receiver disconnected (no valid NMEA sentences received). Check USB/serial connection, ensure GPS is enabled in Settings, or verify baud/port in Developer logs.';
    } else if (seen === 0) {
      const hw = lastStatus ? getGpsHardwareLabel(lastStatus.GPS_detected_type) : 'GPS receiver';
      elGpsDiag.className = 'alert-banner alert-info';
      elGpsDiag.textContent =
        `${hw} connected and streaming NMEA, but 0 satellites have signal yet (${tracked} in almanac). Move near a window or outdoors with clear sky view; keep the GPS antenna separated from RTL-SDR dongles (1–3 min for cold start).`;
    } else {
      elGpsDiag.className = 'alert-banner alert-info';
      elGpsDiag.textContent =
        `Acquiring 3D GPS lock: ${seen} satellite${seen === 1 ? '' : 's'} with signal (${tracked} tracked). Airborne 3D mode requires at least 4 satellites with good sky geometry (C/N0 > 25 dB-Hz).`;
    }
  }

  function renderSituation(sit) {
    if (!sit) return;
    lastSituation = sit;

    const statusBits = sit.AHRSStatus || 0;
    elPillGps.className = `badge ${(statusBits & 0x01) ? 'badge-ok' : ''}`;
    elPillImu.className = `badge ${(statusBits & 0x02) ? 'badge-ok' : ''}`;
    elPillBmp.className = `badge ${(statusBits & 0x04) ? (sit.BaroSourceType === 4 ? 'badge-warn' : 'badge-ok') : ''}`;
    elPillCal.className = `badge ${(statusBits & 0x08) ? 'badge-warn' : 'badge-ok'}`;
    elPillCal.textContent = (statusBits & 0x08) ? 'Caging...' : 'Ready';

    renderDiagBanner(sit);

    // GPS fix card
    const connected = lastStatus ? Boolean(lastStatus.GPS_connected) : isNonZeroTimestamp(sit.GPSLastValidNMEAMessageTime);
    const fixLabels = {
      0: 'No Fix',
      1: '3D GPS',
      2: '3D GPS + SBAS',
      4: 'RTK Fixed',
      5: 'RTK Float',
      6: 'Dead Reckoning'
    };
    const fixText = !connected
      ? 'Disconnected'
      : (lastStatus?.GPS_solution || fixLabels[sit.GPSFixQuality] || 'Unknown');
    const fixOk = connected && (sit.GPSFixQuality || 0) >= 1;
    elGpsFixBadge.textContent = fixText;
    elGpsFixBadge.className = `badge ${fixOk ? 'badge-ok' : 'badge-warn'}`;

    const zeroCoords = sit.GPSLatitude === 0 && sit.GPSLongitude === 0;
    const hasFix = fixOk && !zeroCoords && Number.isFinite(sit.GPSLatitude) && Number.isFinite(sit.GPSLongitude);
    const horizAccOk = hasFix && isValidGpsAccuracy(sit.GPSHorizontalAccuracy);
    const vertAccOk = hasFix && isValidGpsAccuracy(sit.GPSVerticalAccuracy);
    const courseNorm = normalizeHeadingDeg(sit.GPSTrueCourse);
    const vsiFpm = Number.isFinite(sit.GPSVerticalSpeed) ? Math.round(sit.GPSVerticalSpeed * 60) : 0;

    elGpsKv.textContent = '';
    if (lastStatus) {
      elGpsKv.appendChild(kvRow('Hardware', getGpsHardwareLabel(lastStatus.GPS_detected_type)));
    }
    elGpsKv.appendChild(kvRow('Latitude / Longitude', hasFix ? `${sit.GPSLatitude.toFixed(5)}, ${sit.GPSLongitude.toFixed(5)}` : '--'));
    elGpsKv.appendChild(kvRow(
      'Ground Speed / Track',
      hasFix && Number.isFinite(sit.GPSGroundSpeed)
        ? `${sit.GPSGroundSpeed.toFixed(1)} kt / ${courseNorm !== null ? String(courseNorm).padStart(3, '0') + '\u00B0' : '--'}`
        : '--'
    ));
    elGpsKv.appendChild(kvRow(
      'GPS Alt MSL / VSI',
      hasFix && Number.isFinite(sit.GPSAltitudeMSL)
        ? `${Math.round(sit.GPSAltitudeMSL)} ft / ${vsiFpm >= 0 ? '+' : ''}${vsiFpm} fpm`
        : '--'
    ));
    elGpsKv.appendChild(kvRow(
      'Horiz / Vert Acc',
      `${horizAccOk ? sit.GPSHorizontalAccuracy.toFixed(1) + ' m' : '--'} / ${vertAccOk ? (sit.GPSVerticalAccuracy * 3.28084).toFixed(1) + ' ft' : '--'}`
    ));
    elGpsKv.appendChild(kvRow(
      'Satellites (Lock/Trk/Seen)',
      `${sit.GPSSatellites ?? 0} / ${sit.GPSSatellitesTracked ?? 0} / ${sit.GPSSatellitesSeen ?? 0}`
    ));
    elGpsKv.appendChild(kvRow(
      'NACp / Sample Rate',
      `${hasFix && sit.GPSNACp > 0 ? 'NACp ' + sit.GPSNACp : '--'} / ${Number.isFinite(sit.GPSPositionSampleRate) && sit.GPSPositionSampleRate > 0 ? sit.GPSPositionSampleRate.toFixed(1) + ' Hz' : '--'}`
    ));

    // Baro
    const pressValid = isNonZeroTimestamp(sit.BaroLastMeasurementTime) &&
      (sit.BaroSourceType || 0) !== 0 &&
      typeof sit.BaroPressureAltitude === 'number' &&
      Number.isFinite(sit.BaroPressureAltitude) &&
      Math.abs(sit.BaroPressureAltitude) < 100000;
    elBaro.textContent = pressValid ? `${Math.round(sit.BaroPressureAltitude)} ft` : '--';

    // Attitude
    const pitchValid = isValidAhrsAngle(sit.AHRSPitch, 90);
    const rollValid = isValidAhrsAngle(sit.AHRSRoll, 180);
    const ahrsValid = isNonZeroTimestamp(sit.AHRSLastAttitudeTime) && pitchValid && rollValid;
    if (ahrsValid) {
      const pitch = sit.AHRSPitch;
      const roll = sit.AHRSRoll;
      const slipValid = isValidAhrsAngle(sit.AHRSSlipSkid, 90);
      const slip = slipValid ? sit.AHRSSlipSkid : 0;
      const hdgValid = isValidAhrsAngle(sit.AHRSGyroHeading, 360) && sit.AHRSGyroHeading >= 0;
      const hdgNorm = hdgValid ? normalizeHeadingDeg(sit.AHRSGyroHeading) : null;

      elPitch.textContent = `${pitch.toFixed(1)}\u00B0`;
      elRoll.textContent = `${roll.toFixed(1)}\u00B0`;
      elSlip.textContent = slipValid ? `${slip.toFixed(1)}\u00B0` : '--';
      elHdgBadge.textContent = hdgNorm !== null ? `HDG ${String(hdgNorm).padStart(3, '0')}\u00B0` : 'HDG ---\u00B0';

      const pitchPx = Math.max(-120, Math.min(120, pitch * 4));
      elWorld.setAttribute('transform', `rotate(${(-roll).toFixed(2)}) translate(0 ${pitchPx.toFixed(2)})`);
      const ballX = Math.max(-38, Math.min(38, slip * 3));
      elSlipBall.setAttribute('cx', ballX.toFixed(1));

      if (isValidAhrsGLoad(sit.AHRSGLoad)) {
        const g = sit.AHRSGLoad;
        const gMin = isValidAhrsGLoad(sit.AHRSGLoadMin) ? sit.AHRSGLoadMin : g;
        const gMax = isValidAhrsGLoad(sit.AHRSGLoadMax) ? sit.AHRSGLoadMax : g;
        elGCur.textContent = `${g.toFixed(2)} G`;
        elGMinMax.textContent = `${gMin.toFixed(2)} / ${gMax.toFixed(2)} G`;
        elGNeedle.setAttribute('transform', `rotate(${gToDeg(g).toFixed(1)})`);
        elGMinNeedle.setAttribute('transform', `rotate(${gToDeg(gMin).toFixed(1)})`);
        elGMaxNeedle.setAttribute('transform', `rotate(${gToDeg(gMax).toFixed(1)})`);
        elGCur.className = (g > gMaxLim || g < gMinLim) ? 'badge badge-danger mono' : 'badge mono';
      } else {
        elGCur.textContent = '-- G';
        elGCur.className = 'badge mono';
        elGMinMax.textContent = '-- / --';
      }
    } else {
      elPitch.textContent = '--';
      elRoll.textContent = '--';
      elSlip.textContent = '--';
      elHdgBadge.textContent = 'HDG ---\u00B0';
      elGCur.textContent = '-- G';
      elGCur.className = 'badge mono';
      elGMinMax.textContent = '-- / --';
    }
  }

  async function fetchSatellites() {
    try {
      const data = await apiGet('/getSatellites', { optional: true });
      if (destroyed || !data || typeof data !== 'object') return;
      const list = Object.values(data).sort((a, b) => (b.Signal ?? -99) - (a.Signal ?? -99));
      elSatCount.textContent = `${list.length} Sats`;
      elSatChart.textContent = '';
      elSatBody.textContent = '';

      if (list.length === 0) {
        elSatChart.appendChild(createEl('div', { className: 'muted', text: 'No satellites tracked.' }));
        return;
      }

      for (const sat of list) {
        const hasSig = typeof sat.Signal === 'number' && Number.isFinite(sat.Signal) && sat.Signal > 0 && sat.Signal <= 99;
        const sig = hasSig ? Math.min(60, sat.Signal) : 0;
        const elevValid = typeof sat.Elevation === 'number' && Number.isFinite(sat.Elevation) && sat.Elevation >= -90 && sat.Elevation <= 90;
        const azimValid = typeof sat.Azimuth === 'number' && Number.isFinite(sat.Azimuth) && sat.Azimuth >= 0 && sat.Azimuth <= 360;

        const col = createEl('div', {
          className: 'sat-col',
          title: `${sat.SatelliteID || '?'}: ${hasSig ? sat.Signal + ' dB-Hz' : 'no signal'}`
        });
        col.appendChild(createEl('span', { className: 'sat-lbl muted', text: hasSig ? String(sat.Signal) : '-' }));
        col.appendChild(createEl('div', {
          className: `sat-bar ${sat.InSolution ? 'in-sol' : ''}`,
          style: { height: `${Math.max(4, Math.round((sig / 60) * 88))}px` }
        }));
        col.appendChild(createEl('span', { className: 'sat-lbl', text: sat.SatelliteID || '?' }));
        elSatChart.appendChild(col);

        const tr = createEl('tr', {});
        tr.appendChild(createEl('td', { className: 'mono', text: sat.SatelliteID || '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: hasSig ? `${sat.Signal} dB` : '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: elevValid ? `${sat.Elevation}\u00B0` : '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: azimValid ? `${sat.Azimuth}\u00B0` : '--' }));
        tr.appendChild(createEl('td', {}, createEl('span', {
          className: `badge ${sat.InSolution ? 'badge-ok' : ''}`,
          text: sat.InSolution ? 'Yes' : 'No'
        })));
        elSatBody.appendChild(tr);
      }
    } catch {}
  }

  apiGet('/getSettings', { optional: true }).then((s) => {
    if (destroyed || !s || !s.GLimits) return;
    const parts = String(s.GLimits).trim().split(/\s+/).map(Number);
    if (parts.length === 2 && parts.every(Number.isFinite)) {
      gMinLim = parts[0];
      gMaxLim = parts[1];
      elGLimits.textContent = `${gMinLim.toFixed(2)} / +${gMaxLim.toFixed(2)} G`;
    }
  }).catch(() => {});

  apiGet('/getStatus', { optional: true }).then((st) => {
    if (!destroyed && st) {
      lastStatus = st;
      if (lastSituation) renderSituation(lastSituation);
      else renderDiagBanner(null);
    }
  }).catch(() => {});
  apiGet('/getSituation', { optional: true }).then((s) => { if (!destroyed && s) renderSituation(s); }).catch(() => {});
  fetchSatellites();

  const sitWS = new StratuxWS('/situation', {
    onOpen: () => setConnectionBadge('connected', 'Live'),
    onClose: () => setConnectionBadge('reconnecting', 'Reconnecting'),
    onError: () => setConnectionBadge('error', 'Error'),
    onMessage: (data) => { if (!destroyed) renderSituation(data); }
  });

  const statusWS = new StratuxWS('/status', {
    onMessage: (st) => {
      if (destroyed || !st) return;
      lastStatus = st;
      if (lastSituation) renderSituation(lastSituation);
      else renderDiagBanner(null);
    }
  });

  const satTimer = setInterval(fetchSatellites, 1500);

  return function destroy() {
    destroyed = true;
    sitWS.close();
    statusWS.close();
    clearInterval(satTimer);
    themeObserver.disconnect();
    wakeLock.release();
  };
}

function kvRow(key, value) {
  const row = createEl('div', { className: 'kv-row' });
  row.appendChild(createEl('span', { className: 'kv-key', text: key }));
  row.appendChild(createEl('span', { className: 'kv-val mono', text: value }));
  return row;
}
