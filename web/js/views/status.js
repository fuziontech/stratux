import {
  apiGet,
  apiPost,
  AppState,
  buildSparklineSVG,
  createEl,
  escapeHtml,
  formatUptime,
  getGpsHardwareLabel,
  setConnectionBadge,
  showToast,
  StratuxWS
} from '../app.js';

export async function mount(container) {
  let destroyed = false;
  let lastStatus = null;
  let lastHealth = null;
  let uatTowerCount = null;
  let regionInfo = null;
  let otaInfo = null;

  // 8-tap Developer Mode state
  let versionTaps = 0;
  let versionTapReset = null;

  container.innerHTML = `
    <div id="st-region-banner" class="alert-banner alert-info hidden">
      <div>
        <div>Region is not configured yet. Select your operating region:</div>
      </div>
      <div class="btn-row">
        <button type="button" class="btn btn-primary btn-sm" id="btn-region-us">US (978 UAT + 1090ES)</button>
        <button type="button" class="btn btn-primary btn-sm" id="btn-region-eu">EU (868 OGN + 1090ES)</button>
      </div>
    </div>

    <div id="st-health-banners"></div>
    <div id="st-errors-banner" class="alert-banner alert-danger hidden"></div>

    <div class="card-grid">
      <section class="card">
        <div class="card-header">
          <h2 class="card-title">System</h2>
          <button type="button" class="badge badge-accent" id="btn-version-tap" title="Tap 8 times quickly to enable Developer Mode">v--</button>
        </div>
        <div class="metric-grid" id="st-sys-metrics"></div>
        <div class="kv-list" id="st-sys-kv" style="margin-top:10px;"></div>
      </section>

      <section class="card">
        <div class="card-header">
          <h2 class="card-title">Hardware &amp; Power Health</h2>
          <span class="badge" id="st-power-badge">Checking...</span>
        </div>
        <div class="metric-grid" id="st-power-metrics"></div>
        <div class="kv-list" id="st-power-kv" style="margin-top:10px;"></div>
      </section>

      <section class="card">
        <div class="card-header">
          <h2 class="card-title">GPS &amp; Sensors</h2>
          <span class="badge" id="st-gps-badge">--</span>
        </div>
        <div class="kv-list" id="st-gps-kv"></div>
      </section>
    </div>

    <section class="card">
      <div class="card-header">
        <h2 class="card-title">RF Receivers (60s Message Rate)</h2>
        <span class="badge" id="st-sdr-count">0 SDRs</span>
      </div>
      <div class="card-grid" id="st-rf-grid"></div>
    </section>

    <div class="card-grid-2">
      <section class="card" id="st-fisb-card">
        <div class="card-header">
          <h2 class="card-title">FIS-B Weather &amp; Towers</h2>
          <span class="badge" id="st-tower-badge">Towers: --</span>
        </div>
        <div class="metric-grid" id="st-fisb-metrics"></div>
      </section>

      <section class="card">
        <div class="card-header">
          <h2 class="card-title">Region &amp; OTA Updates</h2>
          <button type="button" class="btn btn-sm" id="btn-check-ota">Check Updates</button>
        </div>
        <div class="kv-list" id="st-ota-kv"></div>
        <div class="btn-row" style="margin-top:10px;">
          <button type="button" class="btn btn-sm" id="btn-switch-us">Region: US</button>
          <button type="button" class="btn btn-sm" id="btn-switch-eu">Region: EU</button>
        </div>
      </section>
    </div>
  `;

  const elRegionBanner = container.querySelector('#st-region-banner');
  const elHealthBanners = container.querySelector('#st-health-banners');
  const elErrorsBanner = container.querySelector('#st-errors-banner');
  const elVersionBtn = container.querySelector('#btn-version-tap');
  const elSysMetrics = container.querySelector('#st-sys-metrics');
  const elSysKv = container.querySelector('#st-sys-kv');
  const elPowerBadge = container.querySelector('#st-power-badge');
  const elPowerMetrics = container.querySelector('#st-power-metrics');
  const elPowerKv = container.querySelector('#st-power-kv');
  const elGpsBadge = container.querySelector('#st-gps-badge');
  const elGpsKv = container.querySelector('#st-gps-kv');
  const elSdrCount = container.querySelector('#st-sdr-count');
  const elRfGrid = container.querySelector('#st-rf-grid');
  const elTowerBadge = container.querySelector('#st-tower-badge');
  const elFisbMetrics = container.querySelector('#st-fisb-metrics');
  const elOtaKv = container.querySelector('#st-ota-kv');

  function setRegion(region) {
    apiPost('/setRegion', { Region: region })
      .then(() => {
        showToast(`Region switched to ${region}`, 'ok');
        AppState.refreshSettings();
        fetchRegion();
      })
      .catch((err) => showToast(`Failed to set region: ${err.message}`, 'error'));
  }

  container.querySelector('#btn-region-us').addEventListener('click', () => setRegion('US'));
  container.querySelector('#btn-region-eu').addEventListener('click', () => setRegion('EU'));
  container.querySelector('#btn-switch-us').addEventListener('click', () => setRegion('US'));
  container.querySelector('#btn-switch-eu').addEventListener('click', () => setRegion('EU'));
  container.querySelector('#btn-check-ota').addEventListener('click', () => checkUpdate(true));

  elVersionBtn.addEventListener('click', () => {
    versionTaps += 1;
    if (versionTapReset) clearTimeout(versionTapReset);
    versionTapReset = setTimeout(() => { versionTaps = 0; }, 3000);
    if (versionTaps >= 8) {
      versionTaps = 0;
      apiPost('/develmodetoggle')
        .then(() => {
          showToast('Developer Mode enabled!', 'ok');
          return AppState.refreshSettings();
        })
        .catch((err) => showToast(`Developer mode toggle failed: ${err.message}`, 'error'));
    }
  });

  function renderHealthBanners() {
    elHealthBanners.textContent = '';
    if (!lastHealth) return;
    const banners = [];
    const th = lastHealth.Throttle;
    if (th && th.Available) {
      if (th.UnderVoltageNow) {
        banners.push({ level: 'danger', text: 'POWER WARNING: Raspberry Pi under-voltage detected RIGHT NOW! Check 5V power supply / cable.' });
      } else if (th.UnderVoltageOccurred) {
        banners.push({ level: 'warn', text: 'Power notice: Raspberry Pi under-voltage occurred earlier during this boot.' });
      }
      if (th.ThrottledNow || th.FreqCappedNow) {
        banners.push({ level: 'danger', text: 'THERMAL/POWER THROTTLE ACTIVE: CPU frequency is capped or throttled.' });
      }
    }
    if (lastHealth.UAT?.OverloadAlert) {
      banners.push({ level: 'warn', text: `978 UAT RF overload / IQ clipping (${Number(lastHealth.UAT.ClippedPercent || 0).toFixed(1)}%). Recommended gain: ${lastHealth.UAT.RecommendedGain ?? 'lower'}` });
    }
    if (lastHealth.ES?.OverloadAlert) {
      banners.push({ level: 'warn', text: `1090 ES RF overload detected. Recommended gain: ${lastHealth.ES.RecommendedGain ?? 'lower'}` });
    }
    if (Array.isArray(lastHealth.Warnings)) {
      for (const w of lastHealth.Warnings) {
        banners.push({ level: 'warn', text: String(w) });
      }
    }
    for (const b of banners) {
      elHealthBanners.appendChild(createEl('div', {
        className: `alert-banner ${b.level === 'danger' ? 'alert-danger' : 'alert-warn'}`,
        text: b.text
      }));
    }
  }

  function renderPowerCard() {
    elPowerMetrics.textContent = '';
    elPowerKv.textContent = '';

    const tempC = lastHealth?.CPUTempC ?? lastStatus?.CPUTemp;
    const tempStr = typeof tempC === 'number' && Number.isFinite(tempC)
      ? `${tempC.toFixed(1)}\u00B0C`
      : '--';
    const tempF = typeof tempC === 'number' && Number.isFinite(tempC)
      ? `${((tempC * 9) / 5 + 32).toFixed(1)}\u00B0F`
      : '';

    elPowerMetrics.appendChild(metricBox('CPU Temp', tempStr, tempF));

    const pwr = lastHealth?.Power;
    if (pwr && pwr.Available) {
      const pct = typeof pwr.BatteryPct === 'number' ? `${Math.round(pwr.BatteryPct)}%` : '--';
      const volts = typeof pwr.VoltageV === 'number' ? `${pwr.VoltageV.toFixed(2)} V` : '';
      elPowerMetrics.appendChild(metricBox('Power / Battery', pct, `${volts}${pwr.Source ? ` (${pwr.Source})` : ''}`));
      elPowerBadge.textContent = pwr.Source || 'Monitored';
      elPowerBadge.className = 'badge badge-ok';
    } else {
      elPowerMetrics.appendChild(metricBox('Power Monitor', 'N/A', 'UPS/INA219 not detected'));
      elPowerBadge.textContent = lastHealth ? 'Pi Telemetry' : 'Basic';
      elPowerBadge.className = 'badge';
    }

    const th = lastHealth?.Throttle;
    if (th && th.Available) {
      const uvStatus = th.UnderVoltageNow ? 'UNDER-VOLT NOW' : (th.UnderVoltageOccurred ? 'Occurred earlier' : 'OK');
      const thStatus = th.ThrottledNow ? 'THROTTLED' : (th.FreqCappedNow ? 'Freq Capped' : 'OK');
      elPowerKv.appendChild(kvRow('Under-Voltage', uvStatus, th.UnderVoltageNow ? 'text-danger' : th.UnderVoltageOccurred ? 'text-warn' : 'text-ok'));
      elPowerKv.appendChild(kvRow('CPU Throttling', thStatus, (th.ThrottledNow || th.FreqCappedNow) ? 'text-danger' : 'text-ok'));
    } else {
      elPowerKv.appendChild(kvRow('Pi Throttle Flags', lastHealth ? 'Unavailable on host' : 'Waiting for /getHealth'));
    }

    if (lastStatus && typeof lastStatus.DiskBytesFree === 'number') {
      elPowerKv.appendChild(kvRow('Disk Free', `${(lastStatus.DiskBytesFree / 1048576).toFixed(1)} MiB`));
    }
  }

  function renderStatus() {
    if (!lastStatus) return;
    const s = lastStatus;
    const buildShort = s.Build ? String(s.Build).slice(0, 10) : '';
    elVersionBtn.textContent = `${s.Version || 'dev'}${buildShort ? ` (${buildShort})` : ''}`;

    // Errors banner
    if (Array.isArray(s.Errors) && s.Errors.length > 0) {
      elErrorsBanner.classList.remove('hidden');
      elErrorsBanner.textContent = '';
      const ul = createEl('div', {});
      ul.appendChild(createEl('div', { text: `System Errors (${s.Errors.length}):` }));
      for (const e of s.Errors) {
        ul.appendChild(createEl('div', { text: `\u2022 ${String(e)}` }));
      }
      elErrorsBanner.appendChild(ul);
    } else {
      elErrorsBanner.classList.add('hidden');
    }

    // System card
    elSysMetrics.textContent = '';
    elSysMetrics.appendChild(metricBox('Uptime', formatUptime(s.Uptime)));
    elSysMetrics.appendChild(metricBox('EFB Clients', String(s.Connected_Users ?? 0)));
    elSysKv.textContent = '';
    if (typeof s.CPUTempMin === 'number' && typeof s.CPUTempMax === 'number') {
      elSysKv.appendChild(kvRow('CPU Min / Max', `${s.CPUTempMin.toFixed(1)}\u00B0C / ${s.CPUTempMax.toFixed(1)}\u00B0C`));
    }
    elSysKv.appendChild(kvRow('SDR Devices', String(s.Devices ?? 0)));
    if (s.Ping_connected) elSysKv.appendChild(kvRow('Ping ADS-B', 'Connected', 'text-ok'));
    if (s.Pong_connected) elSysKv.appendChild(kvRow('Pong Receiver', `Connected (${s.Pong_Heartbeats ?? 0} hb)`, 'text-ok'));

    // GPS card
    const sol = s.GPS_solution || 'Unknown';
    elGpsBadge.textContent = sol;
    elGpsBadge.className = (sol === '3D GPS' || sol === '3D GPS + SBAS') ? 'badge badge-ok' : 'badge badge-warn';
    elGpsKv.textContent = '';
    elGpsKv.appendChild(kvRow('Hardware', getGpsHardwareLabel(s.GPS_detected_type)));
    const acc = (sol !== 'Disconnected' && sol !== 'No Fix' && sol !== 'Unknown' && typeof s.GPS_position_accuracy === 'number')
      ? `${s.GPS_position_accuracy.toFixed(1)} m`
      : '--';
    elGpsKv.appendChild(kvRow('Position Accuracy', acc));
    elGpsKv.appendChild(kvRow('Satellites (Lock / Track / Seen)', `${s.GPS_satellites_locked ?? 0} / ${s.GPS_satellites_tracked ?? 0} / ${s.GPS_satellites_seen ?? 0}`));
    if (s.GPS_NetworkRemoteIp) elGpsKv.appendChild(kvRow('Network GPS IP', s.GPS_NetworkRemoteIp));

    // RF Receiver cards + 60s sparklines
    elSdrCount.textContent = `${s.Devices ?? 0} SDR${s.Devices === 1 ? '' : 's'}`;
    renderRfGrid(s);

    // FIS-B Weather Totals
    elTowerBadge.textContent = `Towers: ${uatTowerCount === null ? '--' : uatTowerCount}`;
    elFisbMetrics.textContent = '';
    const fisb = [
      ['METAR', s.UAT_METAR_total ?? 0],
      ['TAF', s.UAT_TAF_total ?? 0],
      ['NEXRAD', s.UAT_NEXRAD_total ?? 0],
      ['SIGMET/AIR', s.UAT_SIGMET_total ?? 0],
      ['PIREP', s.UAT_PIREP_total ?? 0],
      ['NOTAM', s.UAT_NOTAM_total ?? 0]
    ];
    for (const [label, val] of fisb) {
      elFisbMetrics.appendChild(metricBox(label, String(val)));
    }

    renderPowerCard();
  }

  function renderRfGrid(s) {
    elRfGrid.textContent = '';
    const bands = [
      {
        name: '1090 MHz ES',
        color: 'var(--es-color)',
        lastMin: s.ES_messages_last_minute ?? 0,
        maxMin: s.ES_messages_max ?? 0,
        total: s.ES_messages_total ?? 0,
        health: lastHealth?.ES
      },
      {
        name: '978 MHz UAT',
        color: 'var(--uat-color)',
        lastMin: s.UAT_messages_last_minute ?? 0,
        maxMin: s.UAT_messages_max ?? 0,
        total: s.UAT_messages_total ?? 0,
        health: lastHealth?.UAT
      },
      {
        name: '868 MHz OGN',
        color: 'var(--ogn-color)',
        lastMin: s.OGN_messages_last_minute ?? 0,
        maxMin: s.OGN_messages_max ?? 0,
        total: s.OGN_messages_total ?? 0,
        extra: s.OGN_connected ? `Noise ${Number(s.OGN_noise_db || 0).toFixed(1)} dB | Gain ${Number(s.OGN_gain_db || 0).toFixed(1)} dB` : 'Disconnected',
        health: lastHealth?.OGN
      },
      {
        name: '162 MHz AIS',
        color: 'var(--ais-color)',
        lastMin: s.AIS_messages_last_minute ?? 0,
        maxMin: s.AIS_messages_max ?? 0,
        total: s.AIS_messages_total ?? 0,
        extra: s.AIS_connected ? 'Connected' : 'Inactive',
        health: lastHealth?.AIS
      }
    ];

    for (const b of bands) {
      const box = createEl('div', { className: 'sparkline-box' });
      const head = createEl('div', { className: 'sparkline-head' });
      head.appendChild(createEl('strong', { text: b.name }));
      const badges = createEl('div', { className: 'btn-row' });
      if (b.health?.OverloadAlert) {
        badges.appendChild(createEl('span', { className: 'badge badge-danger', text: 'OVERLOAD' }));
      }
      if (typeof b.health?.ClippedPercent === 'number' && b.health.ClippedPercent > 0) {
        badges.appendChild(createEl('span', {
          className: `badge ${b.health.ClippedPercent > 5 ? 'badge-warn' : ''}`,
          text: `IQ Clip ${b.health.ClippedPercent.toFixed(1)}%`
        }));
      }
      const rateStr = typeof b.health?.MsgsPerSec === 'number'
        ? `${b.health.MsgsPerSec.toFixed(1)}/s`
        : `${b.lastMin}/min`;
      badges.appendChild(createEl('span', { className: 'badge mono', text: rateStr }));
      head.appendChild(badges);
      box.appendChild(head);

      const hist = Array.isArray(b.health?.History60s) && b.health.History60s.length > 0
        ? b.health.History60s
        : [0, b.lastMin / 60];
      const svgWrap = createEl('div', {});
      svgWrap.innerHTML = buildSparklineSVG(hist, b.color);
      box.appendChild(svgWrap);

      const footText = `${b.lastMin}/min (peak ${b.maxMin}) \u2022 Total ${b.total}` +
        (b.health?.DroppedFrames ? ` \u2022 Dropped ${b.health.DroppedFrames}` : '') +
        (b.extra ? ` \u2022 ${b.extra}` : '');
      box.appendChild(createEl('div', { className: 'metric-sub mono', text: footText }));
      elRfGrid.appendChild(box);
    }
  }

  function renderOtaCard() {
    elOtaKv.textContent = '';
    const regStr = regionInfo ? (regionInfo.IsSet ? regionInfo.Region : 'Not Set') : '--';
    elOtaKv.appendChild(kvRow('Selected Region', regStr));
    if (otaInfo) {
      elOtaKv.appendChild(kvRow('Current Version', otaInfo.CurrentVersion || lastStatus?.Version || '--'));
      elOtaKv.appendChild(kvRow('Latest Version', otaInfo.LatestVersion || '--'));
      elOtaKv.appendChild(kvRow(
        'Update Available',
        otaInfo.UpdateAvailable ? 'Yes — update ready' : 'Up to date',
        otaInfo.UpdateAvailable ? 'text-warn' : 'text-ok'
      ));
      if (otaInfo.OTAStatus) {
        elOtaKv.appendChild(kvRow('OTA Rollback Status', String(otaInfo.OTAStatus)));
      }
      if (otaInfo.ReleaseURL && /^https?:\/\//i.test(otaInfo.ReleaseURL)) {
        const row = createEl('div', { className: 'kv-row' });
        row.appendChild(createEl('span', { className: 'kv-key', text: 'Release Notes' }));
        row.appendChild(createEl('a', {
          className: 'kv-val',
          href: otaInfo.ReleaseURL,
          target: '_blank',
          rel: 'noopener noreferrer',
          text: 'Open release page'
        }));
        elOtaKv.appendChild(row);
      }
    } else {
      elOtaKv.appendChild(kvRow('OTA Checker', 'Click "Check Updates" (requires internet)'));
    }
  }

  async function fetchHealth() {
    try {
      const h = await apiGet('/getHealth', { optional: true });
      if (destroyed) return;
      if (h && typeof h === 'object') {
        lastHealth = h;
        renderHealthBanners();
        renderPowerCard();
        if (lastStatus) renderRfGrid(lastStatus);
      }
    } catch {}
  }

  async function fetchTowers() {
    try {
      const data = await apiGet('/getTowers', { optional: true });
      if (destroyed || !data || typeof data !== 'object') return;
      let cnt = 0;
      for (const k of Object.keys(data)) {
        if ((data[k]?.Messages_last_minute || 0) > 0) cnt++;
      }
      uatTowerCount = cnt;
      elTowerBadge.textContent = `Towers: ${cnt}`;
    } catch {}
  }

  async function fetchRegion() {
    try {
      const r = await apiGet('/getRegion', { optional: true });
      if (destroyed || !r) return;
      regionInfo = r;
      elRegionBanner.classList.toggle('hidden', Boolean(r.IsSet));
      renderOtaCard();
    } catch {}
  }

  async function checkUpdate(userInitiated = false) {
    try {
      const info = await apiGet('/checkUpdate', { optional: true });
      if (destroyed) return;
      if (!info) {
        if (userInitiated) showToast('Update check endpoint unavailable on this build', 'info');
        return;
      }
      otaInfo = info;
      renderOtaCard();
      if (userInitiated) {
        showToast(info.UpdateAvailable ? `Update ${info.LatestVersion} available` : 'Stratux is up to date', 'ok');
      }
    } catch (err) {
      if (userInitiated) showToast(`Update check failed: ${err.message}`, 'error');
    }
  }

  // Initial HTTP fetch for instant paint before WS tick
  apiGet('/getStatus', { optional: true }).then((s) => {
    if (!destroyed && s) {
      lastStatus = s;
      renderStatus();
    }
  }).catch(() => {});

  fetchHealth();
  fetchTowers();
  fetchRegion();
  renderOtaCard();

  const statusWS = new StratuxWS('/status', {
    onOpen: () => setConnectionBadge('connected', 'Live'),
    onClose: () => setConnectionBadge('reconnecting', 'Reconnecting'),
    onError: () => setConnectionBadge('error', 'Error'),
    onMessage: (msg) => {
      if (destroyed || !msg) return;
      lastStatus = msg;
      renderStatus();
    }
  });

  const healthTimer = setInterval(fetchHealth, 2000);
  const towerTimer = setInterval(fetchTowers, 5000);

  return function destroy() {
    destroyed = true;
    statusWS.close();
    clearInterval(healthTimer);
    clearInterval(towerTimer);
    if (versionTapReset) clearTimeout(versionTapReset);
  };
}

function metricBox(label, value, sub = '') {
  const m = createEl('div', { className: 'metric' });
  m.appendChild(createEl('div', { className: 'metric-label', text: label }));
  m.appendChild(createEl('div', { className: 'metric-value', text: value }));
  if (sub) m.appendChild(createEl('div', { className: 'metric-sub', text: sub }));
  return m;
}

function kvRow(key, value, valClass = '') {
  const row = createEl('div', { className: 'kv-row' });
  row.appendChild(createEl('span', { className: 'kv-key', text: key }));
  row.appendChild(createEl('span', { className: `kv-val mono ${valClass}`.trim(), text: value }));
  return row;
}
