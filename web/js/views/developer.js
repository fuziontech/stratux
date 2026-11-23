import {
  apiGet,
  apiPost,
  createEl,
  formatBytes,
  formatUptime,
  setConnectionBadge,
  showToast,
  StratuxWS
} from '../app.js';

function capabilityLabel(cap) {
  const c = Number(cap) || 0;
  const parts = [];
  if (c & 1) parts.push('GDL90');
  if (c & 2) parts.push('AHRS');
  if (c & 4) parts.push('FLARM/NMEA');
  if (c & 8) parts.push('X-Plane');
  return parts.length ? parts.join(', ') : `0x${c.toString(16)}`;
}

export async function mount(container) {
  let destroyed = false;
  let activeInspector = 'status';
  let latestStatus = null;

  container.innerHTML = `
    <div class="card-grid-2">
      <section class="card">
        <div class="card-header">
          <h2 class="card-title">Connected EFB Clients</h2>
          <button type="button" class="btn btn-sm" id="dev-refresh-clients">Refresh</button>
        </div>
        <div class="table-wrap">
          <table class="data-table">
            <thead>
              <tr>
                <th>Client / IP</th>
                <th>Interface</th>
                <th>Protocol</th>
                <th>State</th>
                <th>Msgs / Bytes</th>
              </tr>
            </thead>
            <tbody id="dev-clients-body"></tbody>
          </table>
        </div>
      </section>

      <section class="card">
        <div class="card-header">
          <h2 class="card-title">Logs, Diagnostics &amp; Actions</h2>
          <span class="badge mono" id="dev-uptime">--</span>
        </div>
        <div class="kv-list" id="dev-log-kv"></div>
        <div class="btn-row" style="margin-top:12px;">
          <a class="btn btn-sm btn-primary" href="/downloadlog" download>Download stratux.log</a>
          <a class="btn btn-sm btn-primary" href="/downloadahrslogs" download>Download AHRS Logs</a>
          <a class="btn btn-sm btn-primary" href="/downloaddb" download>Download SQLite DB</a>
          <a class="btn btn-sm" href="/logs/" target="_blank" rel="noopener noreferrer">Browse /logs/</a>
        </div>
        <div class="btn-row" style="margin-top:10px;">
          <button type="button" class="btn btn-sm btn-danger" id="dev-clear-log">Clear Debug Log</button>
          <button type="button" class="btn btn-sm btn-danger" id="dev-clear-ahrs">Clear AHRS Logs</button>
          <button type="button" class="btn btn-sm" id="dev-restart">Restart Stratux</button>
        </div>
      </section>
    </div>

    <section class="card">
      <div class="card-header">
        <h2 class="card-title">Live JSON Inspector</h2>
        <div class="btn-row" id="dev-json-tabs">
          <button type="button" class="tab-btn active" data-ep="status">/getStatus</button>
          <button type="button" class="tab-btn" data-ep="settings">/getSettings</button>
          <button type="button" class="tab-btn" data-ep="situation">/getSituation</button>
          <button type="button" class="tab-btn" data-ep="health">/getHealth</button>
          <button type="button" class="tab-btn" data-ep="clients">/getClients</button>
        </div>
      </div>
      <pre class="pre-code" id="dev-json-pre">Loading...</pre>
    </section>
  `;

  const elClientsBody = container.querySelector('#dev-clients-body');
  const elLogKv = container.querySelector('#dev-log-kv');
  const elUptime = container.querySelector('#dev-uptime');
  const elJsonPre = container.querySelector('#dev-json-pre');

  container.querySelector('#dev-refresh-clients').addEventListener('click', fetchClients);

  container.querySelector('#dev-clear-log').addEventListener('click', () => {
    apiPost('/deletelogfile')
      .then(() => showToast('Debug log cleared', 'ok'))
      .catch((e) => showToast(`Failed: ${e.message}`, 'error'));
  });
  container.querySelector('#dev-clear-ahrs').addEventListener('click', () => {
    apiPost('/deleteahrslogfiles')
      .then(() => showToast('AHRS logs cleared', 'ok'))
      .catch((e) => showToast(`Failed: ${e.message}`, 'error'));
  });
  container.querySelector('#dev-restart').addEventListener('click', () => {
    apiPost('/restart')
      .then(() => showToast('Stratux restarting...', 'ok'))
      .catch((e) => showToast(`Restart failed: ${e.message}`, 'error'));
  });

  container.querySelectorAll('#dev-json-tabs .tab-btn').forEach((btn) => {
    btn.addEventListener('click', () => {
      activeInspector = btn.dataset.ep;
      container.querySelectorAll('#dev-json-tabs .tab-btn').forEach((b) => b.classList.toggle('active', b === btn));
      refreshInspector();
    });
  });

  function renderLogSummary(s) {
    if (!s) return;
    elUptime.textContent = formatUptime(s.Uptime);
    elLogKv.textContent = '';
    const rows = [
      ['Version / Build', `${s.Version || '--'} (${(s.Build || '').slice(0, 10)})`],
      ['Debug Log Size', formatBytes(s.Logfile_Size)],
      ['AHRS Logs Size', formatBytes(s.AHRS_LogFiles_Size)],
      ['Connected Users', String(s.Connected_Users ?? 0)],
      ['CPU Temp', typeof s.CPUTemp === 'number' ? `${s.CPUTemp.toFixed(1)}\u00B0C` : '--'],
      ['Trace Logging', 'Toggle Record Trace Logs in Settings; replay via stratuxrun -trace']
    ];
    for (const [k, v] of rows) {
      const row = createEl('div', { className: 'kv-row' });
      row.appendChild(createEl('span', { className: 'kv-key', text: k }));
      row.appendChild(createEl('span', { className: 'kv-val mono', text: v }));
      elLogKv.appendChild(row);
    }
  }

  async function fetchClients() {
    try {
      const data = await apiGet('/getClients', { optional: true });
      if (destroyed) return;
      elClientsBody.textContent = '';

      let entries = [];
      if (Array.isArray(data)) {
        entries = data;
      } else if (data && typeof data === 'object') {
        entries = Object.entries(data).map(([k, v]) => ({ _key: k, ...(v || {}) }));
      }

      if (entries.length === 0) {
        elClientsBody.appendChild(createEl('tr', {}, createEl('td', {
          colspan: '5',
          className: 'muted',
          text: 'No EFB clients currently connected.'
        })));
        return;
      }

      for (const c of entries) {
        const tr = createEl('tr', {});
        const addr = c.Ip ? `${c.Ip}${c.Port ? ':' + c.Port : ''}` : (c.Address || c._key || '--');
        const iface = c.Interface || c.Iface || (addr.startsWith('192.168.10.') ? 'ap0/wlan0' : 'net');
        const proto = c.Protocol || capabilityLabel(c.Capability);
        const sleeping = Boolean(c.SleepFlag || c.Sleeping);
        const stats = (c.MessagesSent !== undefined || c.BytesSent !== undefined)
          ? `${c.MessagesSent ?? 0} msgs / ${formatBytes(c.BytesSent ?? 0)}`
          : (c.LastPingResponse ? 'Active' : '--');

        tr.appendChild(createEl('td', { className: 'mono', text: addr }));
        tr.appendChild(createEl('td', { className: 'mono', text: String(iface) }));
        tr.appendChild(createEl('td', { className: 'mono', text: String(proto) }));
        tr.appendChild(createEl('td', {}, createEl('span', {
          className: `badge ${sleeping ? 'badge-warn' : 'badge-ok'}`,
          text: sleeping ? 'Sleeping' : 'Active'
        })));
        tr.appendChild(createEl('td', { className: 'mono', text: stats }));
        elClientsBody.appendChild(tr);
      }
    } catch {}
  }

  async function refreshInspector() {
    const map = {
      status: '/getStatus',
      settings: '/getSettings',
      situation: '/getSituation',
      health: '/getHealth',
      clients: '/getClients'
    };
    const path = map[activeInspector] || '/getStatus';
    try {
      const data = await apiGet(path, { optional: true });
      if (destroyed) return;
      elJsonPre.textContent = data === null
        ? `${path} is not enabled on this backend build.`
        : JSON.stringify(data, null, 2);
    } catch (err) {
      if (!destroyed) elJsonPre.textContent = `Error fetching ${path}: ${err.message}`;
    }
  }

  const statusWS = new StratuxWS('/status', {
    onOpen: () => setConnectionBadge('connected', 'Live'),
    onClose: () => setConnectionBadge('reconnecting', 'Reconnecting'),
    onError: () => setConnectionBadge('error', 'Error'),
    onMessage: (s) => {
      if (destroyed || !s) return;
      latestStatus = s;
      renderLogSummary(s);
      if (activeInspector === 'status') {
        elJsonPre.textContent = JSON.stringify(latestStatus, null, 2);
      }
    }
  });

  fetchClients();
  refreshInspector();
  const pollTimer = setInterval(() => {
    fetchClients();
    if (activeInspector !== 'status') refreshInspector();
  }, 4000);

  return function destroy() {
    destroyed = true;
    statusWS.close();
    clearInterval(pollTimer);
  };
}
