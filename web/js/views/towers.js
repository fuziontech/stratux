import {
  apiGet,
  createEl,
  formatDMS,
  isValidSignalDb,
  setConnectionBadge
} from '../app.js';

export async function mount(container) {
  let destroyed = false;

  container.innerHTML = `
    <section class="card">
      <div class="card-header">
        <h2 class="card-title">978 MHz UAT Ground Towers</h2>
        <span class="badge badge-ok" id="tw-count">0 Active Towers</span>
      </div>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>Location (Lat / Lon)</th>
              <th>Signal Now (dB)</th>
              <th>Strength</th>
              <th>Last Min (dB)</th>
              <th>Peak (dB)</th>
              <th>Msgs / Min</th>
              <th>Total Msgs</th>
            </tr>
          </thead>
          <tbody id="tw-tbody"></tbody>
        </table>
      </div>
    </section>
  `;

  const elCount = container.querySelector('#tw-count');
  const elBody = container.querySelector('#tw-tbody');

  function signalPct(db) {
    // Typical RTL-SDR UAT dB readings range roughly -45 to 0 dB (or positive RSSI)
    if (!isValidSignalDb(db)) return 0;
    if (db <= 0) return Math.max(4, Math.min(100, Math.round(((db + 45) / 45) * 100)));
    return Math.max(4, Math.min(100, Math.round(db)));
  }

  function formatDb(val) {
    return isValidSignalDb(val) ? val.toFixed(1) : '--';
  }

  async function poll() {
    try {
      const data = await apiGet('/getTowers');
      if (destroyed) return;
      setConnectionBadge('connected', 'Live');

      const rows = Object.values(data || {})
        .filter((t) => (t.Messages_last_minute || 0) > 0)
        .sort((a, b) => (b.Messages_last_minute || 0) - (a.Messages_last_minute || 0));

      elCount.textContent = `${rows.length} Active Tower${rows.length === 1 ? '' : 's'}`;
      elBody.textContent = '';

      if (rows.length === 0) {
        elBody.appendChild(createEl('tr', {}, createEl('td', {
          colspan: '7',
          className: 'muted',
          text: 'No UAT ground stations heard in the last minute.'
        })));
        return;
      }

      for (const t of rows) {
        const tr = createEl('tr', {});
        tr.appendChild(createEl('td', { className: 'mono', text: `${formatDMS(t.Lat, true)}, ${formatDMS(t.Lng, true)}` }));
        const nowSig = t.Signal_strength_now;
        tr.appendChild(createEl('td', { className: 'mono', text: formatDb(nowSig) }));

        const barTd = createEl('td', { style: { minWidth: '120px' } });
        const meter = createEl('div', { className: 'bar-meter' });
        meter.appendChild(createEl('div', {
          className: 'bar-meter-fill',
          style: { width: `${signalPct(nowSig)}%` }
        }));
        barTd.appendChild(meter);
        tr.appendChild(barTd);

        tr.appendChild(createEl('td', { className: 'mono', text: formatDb(t.Signal_strength_last_minute) }));
        tr.appendChild(createEl('td', { className: 'mono', text: formatDb(t.Signal_strength_max) }));
        tr.appendChild(createEl('td', { className: 'mono', text: String(t.Messages_last_minute ?? 0) }));
        tr.appendChild(createEl('td', { className: 'mono', text: String(t.Messages_total ?? 0) }));
        elBody.appendChild(tr);
      }
    } catch {
      if (!destroyed) setConnectionBadge('disconnected', 'Offline');
    }
  }

  poll();
  const timer = setInterval(poll, 2500);

  return function destroy() {
    destroyed = true;
    clearInterval(timer);
  };
}
