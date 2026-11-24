import {
  audioAlerts,
  createEl,
  formatDMS,
  formatUTC,
  getCraftCategory,
  getTrafficSourceMeta,
  isNonZeroTimestamp,
  isSameAircraft,
  isTrafficAged,
  isValidSignalDb,
  normalizeHeadingDeg,
  setConnectionBadge,
  StratuxWS,
  TARGET_TYPE_AIS
} from '../app.js';

const AUDIO_LABELS = {
  off: 'Audio: Off',
  both: 'Audio: Beep + Voice',
  beep: 'Audio: Beep',
  speech: 'Audio: Voice'
};

export async function mount(container) {
  let destroyed = false;
  const validTargets = [];
  const bearinglessTargets = [];
  let sortCol = 'alert';
  let sortAsc = false;
  let searchFilter = '';
  let renderPending = false;

  container.innerHTML = `
    <div class="radar-toolbar">
      <div class="btn-row">
        <span class="badge badge-ok" id="tr-count-valid">0 Positioned</span>
        <span class="badge" id="tr-count-other">0 Mode-S / No Pos</span>
        <span class="badge badge-danger hidden" id="tr-count-cpa">0 CPA Alerts</span>
      </div>
      <div class="btn-row">
        <input type="search" class="input" id="tr-search" placeholder="Filter tail / hex / source..." aria-label="Filter traffic" />
        <button type="button" class="btn btn-sm" id="tr-audio-btn"></button>
      </div>
    </div>

    <div id="tr-cpa-banner" class="alert-banner alert-danger hidden"></div>

    <section class="card">
      <div class="card-header">
        <h2 class="card-title">Positioned Traffic (ADS-B / UAT / OGN / AIS / RemoteID)</h2>
        <span class="muted mono" id="tr-clock">--:--:--Z</span>
      </div>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th class="sortable" data-sort="alert">CPA</th>
              <th class="sortable" data-sort="tail">Callsign / Reg</th>
              <th>Source / Fusion</th>
              <th class="sortable" data-sort="alt">Alt (ft)</th>
              <th class="sortable" data-sort="dist">Dist (NM)</th>
              <th>Brg</th>
              <th class="sortable" data-sort="speed">Spd / Hdg</th>
              <th>Lat / Lon</th>
              <th>Sqk / Cat</th>
              <th class="sortable" data-sort="age">Age</th>
            </tr>
          </thead>
          <tbody id="tr-valid-body"></tbody>
        </table>
      </div>
    </section>

    <section class="card">
      <div class="card-header">
        <h2 class="card-title">Bearingless / Mode-S Traffic</h2>
      </div>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>Hex / Tail</th>
              <th>Source</th>
              <th>Alt (ft)</th>
              <th>Est Dist (NM)</th>
              <th>Signal</th>
              <th>Squawk</th>
              <th>Age</th>
            </tr>
          </thead>
          <tbody id="tr-other-body"></tbody>
        </table>
      </div>
    </section>
  `;

  const elValidCount = container.querySelector('#tr-count-valid');
  const elOtherCount = container.querySelector('#tr-count-other');
  const elCpaCount = container.querySelector('#tr-count-cpa');
  const elCpaBanner = container.querySelector('#tr-cpa-banner');
  const elClock = container.querySelector('#tr-clock');
  const elValidBody = container.querySelector('#tr-valid-body');
  const elOtherBody = container.querySelector('#tr-other-body');
  const elSearch = container.querySelector('#tr-search');
  const elAudioBtn = container.querySelector('#tr-audio-btn');

  function syncAudioBtn() {
    elAudioBtn.textContent = AUDIO_LABELS[audioAlerts.mode] || 'Audio: Off';
    elAudioBtn.classList.toggle('btn-active', audioAlerts.mode !== 'off');
  }
  syncAudioBtn();

  elAudioBtn.addEventListener('click', () => {
    const mode = audioAlerts.cycleMode();
    syncAudioBtn();
    if (mode === 'speech' || mode === 'both') {
      audioAlerts.speak('Traffic alerts enabled');
    } else if (mode === 'beep') {
      audioAlerts.playTone(2);
    }
  });

  elSearch.addEventListener('input', () => {
    searchFilter = elSearch.value.trim().toLowerCase();
    scheduleRender();
  });

  container.querySelectorAll('th.sortable').forEach((th) => {
    th.addEventListener('click', () => {
      const col = th.dataset.sort;
      if (sortCol === col) sortAsc = !sortAsc;
      else {
        sortCol = col;
        sortAsc = col === 'dist' || col === 'tail' || col === 'age';
      }
      scheduleRender();
    });
  });

  function normalizeTarget(msg) {
    const hex = (msg.Icao_addr >>> 0).toString(16).toUpperCase().padStart(6, '0');
    const tail = (msg.Tail && msg.Tail.trim()) ? msg.Tail.trim() : hex;
    const reg = (msg.Reg && msg.Reg.trim()) ? msg.Reg.trim() : '';
    const alertLevel = Number(msg.AlertLevel) || 0;
    const brgDistValid = Boolean(msg.BearingDist_valid);
    const distNM = brgDistValid && typeof msg.Distance === 'number' && Number.isFinite(msg.Distance) && msg.Distance >= 0
      ? msg.Distance / 1852
      : Infinity;
    const distEstNM = typeof msg.DistanceEstimated === 'number' && Number.isFinite(msg.DistanceEstimated) && msg.DistanceEstimated > 0
      ? msg.DistanceEstimated / 1852
      : null;
    const fused = Array.isArray(msg.FusedSources) ? msg.FusedSources : [];
    const altValid = typeof msg.Alt === 'number' && Number.isFinite(msg.Alt) &&
      (isNonZeroTimestamp(msg.Last_alt) || msg.Alt !== 0);
    const altFt = altValid ? Math.round(msg.Alt / 25) * 25 : null;
    const speedValid = Boolean(msg.Speed_valid) && typeof msg.Speed === 'number' && Number.isFinite(msg.Speed);
    const speedKt = speedValid ? Math.max(0, Math.round(msg.Speed)) : null;
    const trackDeg = speedValid ? normalizeHeadingDeg(msg.Track) : null;
    const bearingDeg = brgDistValid ? normalizeHeadingDeg(msg.Bearing) : null;
    const confidence = typeof msg.SourceConfidence === 'number' && Number.isFinite(msg.SourceConfidence) && msg.SourceConfidence > 0
      ? Math.max(0, Math.min(100, Math.round(msg.SourceConfidence)))
      : null;

    return {
      icao_int: msg.Icao_addr,
      addr_type: msg.Addr_type,
      hex,
      tail,
      reg,
      Last_source: msg.Last_source,
      TargetType: msg.TargetType,
      Emitter_category: msg.Emitter_category,
      SurfaceVehicleType: msg.SurfaceVehicleType,
      category: getCraftCategory(msg),
      Position_valid: Boolean(msg.Position_valid),
      Lat: msg.Lat,
      Lng: msg.Lng,
      Alt: altFt,
      Vvel: typeof msg.Vvel === 'number' && Number.isFinite(msg.Vvel) ? Math.round(msg.Vvel / 100) * 100 : 0,
      Speed: speedKt,
      Track: trackDeg,
      Bearing: bearingDeg,
      distNM,
      distEstNM,
      SignalLevel: isValidSignalDb(msg.SignalLevel) ? msg.SignalLevel : null,
      Squawk: (typeof msg.Squawk === 'number' && msg.Squawk > 0) ? String(msg.Squawk).padStart(4, '0') : '----',
      Age: Number.isFinite(Number(msg.Age)) ? Math.max(0, Number(msg.Age)) : 0,
      AgeLastAlt: Number.isFinite(Number(msg.AgeLastAlt)) ? Math.max(0, Number(msg.AgeLastAlt)) : 0,
      Timestamp: msg.Timestamp,
      AlertLevel: alertLevel,
      TimeToCPA: typeof msg.TimeToCPA === 'number' && Number.isFinite(msg.TimeToCPA) && msg.TimeToCPA >= 0 && msg.TimeToCPA < 3600 ? msg.TimeToCPA : null,
      HorizMissDist: typeof msg.HorizMissDist === 'number' && Number.isFinite(msg.HorizMissDist) ? msg.HorizMissDist : null,
      VertMissDist: typeof msg.VertMissDist === 'number' && Number.isFinite(msg.VertMissDist) ? msg.VertMissDist : null,
      Callout: msg.Callout ? String(msg.Callout) : '',
      SourceConfidence: confidence,
      FusedSources: fused
    };
  }

  function upsert(msg) {
    const t = normalizeTarget(msg);
    const key = `${t.icao_int}:${t.addr_type === 1 ? 1 : 0}`;
    if (t.AlertLevel >= 2) {
      const callout = t.Callout || `Traffic ${t.tail}, ${Number.isFinite(t.distNM) ? t.distNM.toFixed(1) + ' miles' : 'nearby'}`;
      audioAlerts.announceTraffic(key, t.AlertLevel, callout);
    }

    const vIdx = validTargets.findIndex((x) => isSameAircraft(x.icao_int, x.addr_type, t.icao_int, t.addr_type));
    const oIdx = bearinglessTargets.findIndex((x) => isSameAircraft(x.icao_int, x.addr_type, t.icao_int, t.addr_type));

    if (t.Position_valid) {
      if (vIdx >= 0) validTargets[vIdx] = t;
      else validTargets.push(t);
      if (oIdx >= 0) bearinglessTargets.splice(oIdx, 1);
    } else {
      if (oIdx >= 0) bearinglessTargets[oIdx] = t;
      else bearinglessTargets.push(t);
      if (vIdx >= 0) validTargets.splice(vIdx, 1);
    }
    scheduleRender();
  }

  function scheduleRender() {
    if (renderPending || destroyed) return;
    renderPending = true;
    requestAnimationFrame(() => {
      renderPending = false;
      if (!destroyed) renderTables();
    });
  }

  function matchesFilter(t) {
    if (!searchFilter) return true;
    const src = getTrafficSourceMeta(t.Last_source).code.toLowerCase();
    return (
      t.tail.toLowerCase().includes(searchFilter) ||
      t.reg.toLowerCase().includes(searchFilter) ||
      t.hex.toLowerCase().includes(searchFilter) ||
      src.includes(searchFilter) ||
      t.category.toLowerCase().includes(searchFilter)
    );
  }

  function compareTargets(a, b) {
    let diff = 0;
    switch (sortCol) {
      case 'alert':
        diff = (a.AlertLevel - b.AlertLevel) || (b.distNM - a.distNM);
        break;
      case 'tail':
        diff = a.tail.localeCompare(b.tail);
        break;
      case 'alt':
        diff = (a.Alt ?? -999999) - (b.Alt ?? -999999);
        break;
      case 'dist':
        diff = a.distNM - b.distNM;
        break;
      case 'speed':
        diff = (a.Speed ?? -1) - (b.Speed ?? -1);
        break;
      case 'age':
        diff = a.Age - b.Age;
        break;
      default:
        diff = a.distNM - b.distNM;
    }
    return sortAsc ? diff : -diff;
  }

  function cpaBadge(t) {
    if (t.AlertLevel <= 0) return createEl('span', { className: 'muted', text: '-' });
    const labels = { 1: 'PROX', 2: 'TA', 3: 'RA!' };
    const ttc = t.TimeToCPA !== null && t.TimeToCPA >= 0 ? ` ${Math.round(t.TimeToCPA)}s` : '';
    return createEl('span', {
      className: `badge cpa-${Math.min(3, t.AlertLevel)}`,
      title: t.Callout || `CPA level ${t.AlertLevel}`,
      text: `${labels[t.AlertLevel] || 'CPA'}${ttc}`
    });
  }

  function sourceBadges(t) {
    const wrap = createEl('div', { className: 'btn-row' });
    const meta = getTrafficSourceMeta(t.Last_source);
    wrap.appendChild(createEl('span', {
      className: 'badge',
      style: { borderColor: meta.color, color: meta.color },
      text: meta.code
    }));
    if (t.FusedSources.length > 1) {
      wrap.appendChild(createEl('span', {
        className: 'badge badge-accent',
        title: `Fused sources: ${t.FusedSources.join(', ')}`,
        text: `FUSED\u00D7${t.FusedSources.length}`
      }));
    }
    if (t.SourceConfidence !== null) {
      wrap.appendChild(createEl('span', {
        className: 'muted mono',
        style: { fontSize: '0.75rem' },
        text: `${t.SourceConfidence}%`
      }));
    }
    return wrap;
  }

  function renderTables() {
    const filteredValid = validTargets.filter(matchesFilter).sort(compareTargets);
    const filteredOther = bearinglessTargets.filter(matchesFilter).sort((a, b) => a.Age - b.Age);

    elValidCount.textContent = `${validTargets.length} Positioned`;
    elOtherCount.textContent = `${bearinglessTargets.length} Mode-S / No Pos`;

    const cpaList = validTargets.filter((t) => t.AlertLevel >= 2).sort((a, b) => b.AlertLevel - a.AlertLevel);
    if (cpaList.length > 0) {
      elCpaCount.classList.remove('hidden');
      elCpaCount.textContent = `${cpaList.length} CPA Alert${cpaList.length > 1 ? 's' : ''}`;
      const top = cpaList[0];
      elCpaBanner.classList.remove('hidden');
      elCpaBanner.textContent = top.Callout ||
        `TRAFFIC ALERT: ${top.tail} — ${Number.isFinite(top.distNM) ? top.distNM.toFixed(1) + ' NM' : ''} ` +
        `${top.TimeToCPA !== null ? `(CPA ${Math.round(top.TimeToCPA)}s)` : ''}`;
    } else {
      elCpaCount.classList.add('hidden');
      elCpaBanner.classList.add('hidden');
    }

    elValidBody.textContent = '';
    if (filteredValid.length === 0) {
      const tr = createEl('tr', {}, createEl('td', { colspan: '10', className: 'muted', text: 'No positioned targets received.' }));
      elValidBody.appendChild(tr);
    } else {
      for (const t of filteredValid) {
        const rowClass = t.AlertLevel >= 3 ? 'row-cpa-3' : t.AlertLevel === 2 ? 'row-cpa-2' : '';
        const tr = createEl('tr', { className: rowClass });
        tr.appendChild(createEl('td', {}, cpaBadge(t)));

        const idCell = createEl('td', {});
        const icon = t.TargetType === TARGET_TYPE_AIS ? '\u26F5 ' : '\u2708 ';
        idCell.appendChild(createEl('strong', { className: 'mono', text: `${icon}${t.tail}` }));
        if (t.reg && t.reg !== t.tail) {
          idCell.appendChild(createEl('div', { className: 'muted mono', style: { fontSize: '0.75rem' }, text: `${t.reg} (${t.hex})` }));
        } else {
          idCell.appendChild(createEl('div', { className: 'muted mono', style: { fontSize: '0.75rem' }, text: t.hex }));
        }
        tr.appendChild(idCell);

        tr.appendChild(createEl('td', {}, sourceBadges(t)));

        const vArrow = t.Vvel > 150 ? ' \u2191' : t.Vvel < -150 ? ' \u2193' : '';
        tr.appendChild(createEl('td', { className: 'mono', text: t.Alt !== null ? `${t.Alt.toLocaleString()}${vArrow}` : '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: Number.isFinite(t.distNM) ? t.distNM.toFixed(1) : '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: t.Bearing !== null ? `${String(t.Bearing).padStart(3, '0')}\u00B0` : '--' }));
        tr.appendChild(createEl('td', {
          className: 'mono',
          text: t.Speed !== null && t.Track !== null ? `${t.Speed}kt / ${String(t.Track).padStart(3, '0')}\u00B0` : '---'
        }));
        tr.appendChild(createEl('td', { className: 'mono', text: `${formatDMS(t.Lat)} ${formatDMS(t.Lng)}` }));
        tr.appendChild(createEl('td', { className: 'mono', text: `${t.Squawk} / ${t.category}` }));
        tr.appendChild(createEl('td', { className: 'mono', text: `${Math.round(t.Age)}s` }));
        elValidBody.appendChild(tr);
      }
    }

    elOtherBody.textContent = '';
    if (filteredOther.length === 0) {
      elOtherBody.appendChild(createEl('tr', {}, createEl('td', { colspan: '7', className: 'muted', text: 'No bearingless targets.' })));
    } else {
      for (const t of filteredOther) {
        const tr = createEl('tr', {});
        tr.appendChild(createEl('td', { className: 'mono', text: `${t.tail} (${t.hex})` }));
        tr.appendChild(createEl('td', {}, sourceBadges(t)));
        tr.appendChild(createEl('td', { className: 'mono', text: t.Alt !== null ? t.Alt.toLocaleString() : '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: t.distEstNM !== null ? `~${t.distEstNM.toFixed(1)}` : '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: t.SignalLevel !== null ? `${t.SignalLevel.toFixed(1)} dB` : '--' }));
        tr.appendChild(createEl('td', { className: 'mono', text: t.Squawk }));
        tr.appendChild(createEl('td', { className: 'mono', text: `${Math.round(t.Age)}s` }));
        elOtherBody.appendChild(tr);
      }
    }
  }

  const trafficWS = new StratuxWS('/traffic', {
    onOpen: () => setConnectionBadge('connected', 'Live'),
    onClose: () => setConnectionBadge('reconnecting', 'Reconnecting'),
    onError: () => setConnectionBadge('error', 'Error'),
    onMessage: (msg) => {
      if (!destroyed && msg && msg.Icao_addr !== undefined) upsert(msg);
    }
  });

  const statusWS = new StratuxWS('/status', {
    onMessage: (status) => {
      if (!destroyed && status?.UptimeClock) {
        elClock.textContent = formatUTC(status.UptimeClock);
      }
    }
  });

  const cleanupTimer = setInterval(() => {
    let changed = false;
    for (let i = validTargets.length - 1; i >= 0; i--) {
      validTargets[i].Age += 5;
      if (isTrafficAged(validTargets[i])) {
        validTargets.splice(i, 1);
        changed = true;
      }
    }
    for (let i = bearinglessTargets.length - 1; i >= 0; i--) {
      bearinglessTargets[i].Age += 5;
      if (isTrafficAged(bearinglessTargets[i]) || isTrafficAged(bearinglessTargets[i], 'AgeLastAlt')) {
        bearinglessTargets.splice(i, 1);
        changed = true;
      }
    }
    if (changed) scheduleRender();
  }, 5000);

  renderTables();

  return function destroy() {
    destroyed = true;
    trafficWS.close();
    statusWS.close();
    clearInterval(cleanupTimer);
  };
}
