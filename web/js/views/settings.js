import {
  apiGet,
  apiPost,
  apiUpload,
  AppState,
  createEl,
  getGpsHardwareLabel,
  setConnectionBadge,
  showToast,
  ThemeManager
} from '../app.js';

const DUMP1090_GAINS = [
  '0.9', '1.4', '2.7', '3.7', '7.7', '8.7', '12.5', '14.4', '15.7', '16.6',
  '19.7', '20.7', '22.9', '25.4', '28.0', '29.7', '32.8', '33.8', '36.4',
  '37.2', '38.6', '40.2', '42.1', '43.4', '43.9', '44.5', '48.0', '49.6'
];

const WIFI_COUNTRIES = [
  ['', 'Unspecified'], ['US', 'United States'], ['DE', 'Germany'], ['GB', 'United Kingdom'],
  ['FR', 'France'], ['CA', 'Canada'], ['AU', 'Australia'], ['AT', 'Austria'],
  ['CH', 'Switzerland'], ['NL', 'Netherlands'], ['ES', 'Spain'], ['IT', 'Italy'],
  ['SE', 'Sweden'], ['NO', 'Norway'], ['DK', 'Denmark'], ['PL', 'Poland'],
  ['CZ', 'Czech Republic'], ['BE', 'Belgium'], ['NZ', 'New Zealand'], ['JP', 'Japan'],
  ['BR', 'Brazil'], ['ZA', 'South Africa']
];

function isValidSSID(str) { return /^[a-zA-Z0-9()! \._'-]{1,32}$/.test(str); }
function isValidWPA(str) { return /^[\u0020-\u007e]{8,63}$/.test(str); }
function isValidPin(str) { return /^\d{8}$/.test(str); }
function isValidIPv4(str) { return /^((25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(25[0-5]|2[0-4]\d|[01]?\d\d?)$/.test(str); }
function isValidHexList(str) { return /^$|^([0-9A-Fa-f]{6})(,[0-9A-Fa-f]{6})*$/.test(str.trim()); }

export async function mount(container) {
  let destroyed = false;
  let settings = {};
  let status = {};
  let firmwareList = null;

  container.innerHTML = `
    <div class="card-grid-2">
      <section class="card">
        <div class="card-header"><h2 class="card-title">Receivers &amp; Intelligence</h2></div>
        <div id="set-receivers"></div>
      </section>

      <section class="card">
        <div class="card-header"><h2 class="card-title">Flight &amp; Avionics Config</h2></div>
        <div id="set-flight"></div>
      </section>
    </div>

    <div class="card-grid-2">
      <section class="card">
        <div class="card-header"><h2 class="card-title">Wi-Fi &amp; Network</h2></div>
        <div id="set-wifi"></div>
      </section>

      <section class="card">
        <div class="card-header"><h2 class="card-title">Hardware Setup Wizard &amp; Firmware</h2></div>
        <div id="set-wizard"></div>
      </section>
    </div>

    <div class="card-grid-2">
      <section class="card">
        <div class="card-header"><h2 class="card-title">Logging, Outputs &amp; Diagnostics</h2></div>
        <div id="set-outputs"></div>
      </section>

      <section class="card">
        <div class="card-header"><h2 class="card-title">System Update &amp; Power</h2></div>
        <div id="set-system"></div>
      </section>
    </div>

    <section class="card hidden" id="set-tracker-card">
      <div class="card-header"><h2 class="card-title">External OGN / SoftRF Tracker Config</h2></div>
      <div id="set-tracker"></div>
    </section>

    <div id="set-modal-host"></div>
  `;

  const elReceivers = container.querySelector('#set-receivers');
  const elFlight = container.querySelector('#set-flight');
  const elWifi = container.querySelector('#set-wifi');
  const elWizard = container.querySelector('#set-wizard');
  const elOutputs = container.querySelector('#set-outputs');
  const elSystem = container.querySelector('#set-system');
  const elTrackerCard = container.querySelector('#set-tracker-card');
  const elTracker = container.querySelector('#set-tracker');
  const elModalHost = container.querySelector('#set-modal-host');

  async function savePatch(patch, toastMsg = 'Settings saved') {
    try {
      const updated = await apiPost('/setSettings', patch);
      if (destroyed) return;
      if (updated && typeof updated === 'object') {
        settings = updated;
        AppState.updateLocalSettings(updated);
        renderAll();
      }
      showToast(toastMsg, 'ok');
    } catch (err) {
      showToast(`Save failed: ${err.message}`, 'error');
    }
  }

  function switchRow(label, sub, key, disabled = false) {
    const row = createEl('div', { className: 'form-row' });
    const lbl = createEl('div', { className: 'form-label' }, label);
    if (sub) lbl.appendChild(createEl('small', { text: sub }));
    const sw = createEl('label', { className: 'switch' });
    const input = createEl('input', { type: 'checkbox' });
    input.checked = Boolean(settings[key]);
    input.disabled = disabled;
    input.addEventListener('change', () => savePatch({ [key]: input.checked }, `${label}: ${input.checked ? 'ON' : 'OFF'}`));
    sw.appendChild(input);
    sw.appendChild(createEl('span', { className: 'switch-track' }));
    row.appendChild(lbl);
    row.appendChild(sw);
    return row;
  }

  function inputRow(label, sub, initialVal, { type = 'text', placeholder = '', onSave } = {}) {
    const row = createEl('div', { className: 'form-row' });
    const lbl = createEl('div', { className: 'form-label' }, label);
    if (sub) lbl.appendChild(createEl('small', { text: sub }));
    const right = createEl('div', { className: 'btn-row' });
    const inp = createEl('input', {
      type,
      className: 'input',
      value: initialVal ?? '',
      placeholder
    });
    const btn = createEl('button', {
      type: 'button',
      className: 'btn btn-sm',
      text: 'Save',
      onClick: () => onSave(inp.value)
    });
    inp.addEventListener('keydown', (e) => { if (e.key === 'Enter') onSave(inp.value); });
    right.appendChild(inp);
    right.appendChild(btn);
    row.appendChild(lbl);
    row.appendChild(right);
    return row;
  }

  function confirmModal(title, message, onConfirm) {
    elModalHost.textContent = '';
    const bg = createEl('div', { className: 'modal-backdrop' });
    const card = createEl('div', { className: 'modal-card' });
    card.appendChild(createEl('h3', { className: 'modal-title', text: title }));
    card.appendChild(createEl('p', { text: message }));
    const row = createEl('div', { className: 'btn-row', style: { justifyContent: 'flex-end', marginTop: '14px' } });
    row.appendChild(createEl('button', { type: 'button', className: 'btn btn-sm', text: 'Cancel', onClick: () => { elModalHost.textContent = ''; } }));
    row.appendChild(createEl('button', {
      type: 'button',
      className: 'btn btn-danger btn-sm',
      text: 'Confirm',
      onClick: () => { elModalHost.textContent = ''; onConfirm(); }
    }));
    card.appendChild(row);
    bg.appendChild(card);
    elModalHost.appendChild(bg);
  }

  function renderReceivers() {
    elReceivers.textContent = '';
    elReceivers.appendChild(switchRow('1090 MHz (ADS-B / Mode-S ES)', 'Commercial & international ADS-B', 'ES_Enabled'));
    elReceivers.appendChild(switchRow('978 MHz (UAT)', 'US traffic + FIS-B weather', 'UAT_Enabled'));
    elReceivers.appendChild(switchRow('868 MHz (OGN / FLARM / ADS-L)', 'Gliders, GA & European traffic', 'OGN_Enabled'));
    elReceivers.appendChild(switchRow('162 MHz (AIS Marine)', 'Vessels & maritime traffic', 'AIS_Enabled'));
    elReceivers.appendChild(switchRow('Internet OGN APRS', 'Fetch OGN traffic when online', 'APRS_Enabled'));
    elReceivers.appendChild(switchRow('Auto-Gain Optimization', 'Dynamically tune SDR gain to prevent IQ clipping', 'AutoGain_Enabled'));
    elReceivers.appendChild(switchRow('Drone RemoteID Receiver', 'Decode ASTM F3411 Wi-Fi/BLE UAS broadcasts', 'RemoteID_Enabled'));
    elReceivers.appendChild(switchRow('3D CPA Collision Alerts', 'Compute time-to-CPA & traffic advisories', 'CPA_Alerts_Enabled'));
    elReceivers.appendChild(switchRow('GPS Receiver', 'Internal/USB GNSS receiver', 'GPS_Enabled'));
    elReceivers.appendChild(switchRow('AHRS IMU Sensor', 'Attitude & G-load sensor', 'IMU_Sensor_Enabled'));
    elReceivers.appendChild(switchRow('Barometer Sensor', 'Pressure altitude sensor', 'BMP_Sensor_Enabled'));
    elReceivers.appendChild(switchRow('Ping ADS-B', 'uAvionix Ping receiver', 'Ping_Enabled', Boolean(settings.Pong_Enabled)));
    elReceivers.appendChild(switchRow('Pong ADS-B', 'uAvionix Pong receiver', 'Pong_Enabled', Boolean(settings.Ping_Enabled)));
    elReceivers.appendChild(switchRow('OGN I\u00B2C/GPIO TX HAT', 'Transmit OGN beacons via HAT', 'OGNI2CTXEnabled'));
  }

  function renderFlightConfig() {
    elFlight.textContent = '';
    elFlight.appendChild(inputRow(
      'Ownship Mode-S / OGN Hex',
      'Comma-separated 6-char hex codes (suppresses ownship echo)',
      settings.OwnshipModeS || '',
      {
        placeholder: 'A12345',
        onSave: (v) => {
          const clean = v.trim().toUpperCase();
          if (!isValidHexList(clean)) {
            showToast('Must be comma-separated 6-character hex codes', 'error');
            return;
          }
          savePatch({ OwnshipModeS: clean });
        }
      }
    ));

    elFlight.appendChild(inputRow(
      'Weather Watch List',
      'Space-separated 4-letter ICAO airports',
      settings.WatchList || '',
      {
        placeholder: 'KBOS KATL KORD KLAX',
        onSave: (v) => savePatch({ WatchList: v.trim().toUpperCase() })
      }
    ));

    elFlight.appendChild(inputRow(
      'RTL-SDR PPM Correction',
      'Crystal frequency offset (integer)',
      settings.PPM ?? 0,
      {
        type: 'number',
        onSave: (v) => savePatch({ PPM: parseInt(v, 10) || 0 })
      }
    ));

    // Dump1090 gain select
    const gainRow = createEl('div', { className: 'form-row' });
    gainRow.appendChild(createEl('div', { className: 'form-label' }, '1090 ES Manual Gain (dB)', createEl('small', { text: 'Used when Auto-Gain is off' })));
    const gainSel = createEl('select', { className: 'select' });
    const curGain = settings.Dump1090Gain !== undefined ? Number(settings.Dump1090Gain).toFixed(1) : '37.2';
    for (const g of DUMP1090_GAINS) {
      const opt = createEl('option', { value: g, text: g === '37.2' ? '37.2 (Default)' : g });
      if (g === curGain) opt.selected = true;
      gainSel.appendChild(opt);
    }
    gainSel.addEventListener('change', () => savePatch({ Dump1090Gain: parseFloat(gainSel.value) }));
    gainRow.appendChild(gainSel);
    elFlight.appendChild(gainRow);

    elFlight.appendChild(inputRow(
      'Pressure Altitude Offset (ft)',
      'Barometer calibration offset',
      settings.AltitudeOffset ?? 0,
      {
        type: 'number',
        onSave: (v) => savePatch({ AltitudeOffset: parseInt(v, 10) || 0 })
      }
    ));

    elFlight.appendChild(inputRow(
      'G-Meter Limits',
      'Negative and positive G limits (space-separated)',
      settings.GLimits || '-1.76 4.4',
      {
        placeholder: '-1.76 4.4',
        onSave: (v) => savePatch({ GLimits: v.trim() })
      }
    ));

    elFlight.appendChild(inputRow(
      'Static Client IPs',
      'Space-separated EFB IPs to receive GDL90 directly',
      Array.isArray(settings.StaticIps) ? settings.StaticIps.join(' ') : (settings.StaticIps || ''),
      {
        placeholder: '192.168.10.20',
        onSave: (v) => savePatch({ StaticIps: v.trim() })
      }
    ));

    elFlight.appendChild(switchRow('Bearingless Target Circle Emulation', 'Render Mode-S distance rings in GDL90', 'EstimateBearinglessDist'));
    elFlight.appendChild(switchRow('Show Traffic Source in Callsign', 'Prefix target callsigns with source tag', 'DisplayTrafficSource'));

    // Cockpit theme selector row
    const themeRow = createEl('div', { className: 'form-row' });
    themeRow.appendChild(createEl('div', { className: 'form-label' }, 'Cockpit UI Theme', createEl('small', { text: 'Stored locally for instant offline switching' })));
    const tBtns = createEl('div', { className: 'btn-row' });
    for (const [key, lbl] of [['day', 'Day'], ['dark', 'Dark'], ['night', 'Red Night']]) {
      tBtns.appendChild(createEl('button', {
        type: 'button',
        className: `btn btn-sm ${ThemeManager.current === key ? 'btn-active' : ''}`,
        text: lbl,
        onClick: () => {
          ThemeManager.set(key);
          renderFlightConfig();
        }
      }));
    }
    themeRow.appendChild(tBtns);
    elFlight.appendChild(themeRow);
  }

  function renderWifi() {
    elWifi.textContent = '';
    const state = {
      WiFiMode: String(settings.WiFiMode ?? 0),
      WiFiCountry: settings.WiFiCountry || '',
      WiFiSSID: settings.WiFiSSID || 'stratux',
      WiFiSecurityEnabled: Boolean(settings.WiFiSecurityEnabled),
      WiFiPassphrase: settings.WiFiPassphrase || '',
      WiFiChannel: Number(settings.WiFiChannel) || 1,
      WiFiIPAddress: settings.WiFiIPAddress || '192.168.10.1',
      WiFiDirectPin: settings.WiFiDirectPin || '',
      WiFiInternetPassThroughEnabled: Boolean(settings.WiFiInternetPassThroughEnabled),
      WiFiClientNetworks: Array.isArray(settings.WiFiClientNetworks)
        ? settings.WiFiClientNetworks.map((n) => ({ SSID: n.SSID || '', Password: n.Password || '' }))
        : []
    };

    const formWrap = createEl('div', {});
    function drawWifiForm() {
      formWrap.textContent = '';

      // Mode
      const modeSel = createEl('select', { className: 'select' },
        createEl('option', { value: '0', text: 'Access Point (AP)' }),
        createEl('option', { value: '1', text: 'Wi-Fi Direct' }),
        createEl('option', { value: '2', text: 'AP + Client' })
      );
      modeSel.value = state.WiFiMode;
      modeSel.addEventListener('change', () => {
        state.WiFiMode = modeSel.value;
        if (state.WiFiMode === '1') state.WiFiSecurityEnabled = true;
        drawWifiForm();
      });
      formWrap.appendChild(fieldRow('Wi-Fi Mode', modeSel));

      // Country
      const cSel = createEl('select', { className: 'select' });
      for (const [k, v] of WIFI_COUNTRIES) {
        cSel.appendChild(createEl('option', { value: k, text: v }));
      }
      if (state.WiFiCountry && !WIFI_COUNTRIES.some(([k]) => k === state.WiFiCountry)) {
        cSel.appendChild(createEl('option', { value: state.WiFiCountry, text: state.WiFiCountry }));
      }
      cSel.value = state.WiFiCountry;
      cSel.addEventListener('change', () => { state.WiFiCountry = cSel.value; });
      formWrap.appendChild(fieldRow('Regulatory Country', cSel));

      // SSID
      const ssidInp = createEl('input', { type: 'text', className: 'input', value: state.WiFiSSID, maxlength: '32' });
      ssidInp.addEventListener('input', () => { state.WiFiSSID = ssidInp.value; });
      formWrap.appendChild(fieldRow('Wi-Fi SSID', ssidInp));

      // WPA Security switch
      if (state.WiFiMode !== '1') {
        const sw = createEl('label', { className: 'switch' });
        const cb = createEl('input', { type: 'checkbox' });
        cb.checked = state.WiFiSecurityEnabled;
        cb.addEventListener('change', () => { state.WiFiSecurityEnabled = cb.checked; drawWifiForm(); });
        sw.appendChild(cb);
        sw.appendChild(createEl('span', { className: 'switch-track' }));
        formWrap.appendChild(fieldRow('WPA2 Security', sw));
      }

      // Passphrase
      const passInp = createEl('input', {
        type: 'password',
        className: 'input',
        value: state.WiFiPassphrase,
        placeholder: '8-63 printable ASCII chars',
        disabled: !state.WiFiSecurityEnabled
      });
      passInp.addEventListener('input', () => { state.WiFiPassphrase = passInp.value; });
      formWrap.appendChild(fieldRow('Wi-Fi Passphrase', passInp));

      if (state.WiFiMode === '1') {
        const pinInp = createEl('input', { type: 'text', className: 'input', value: state.WiFiDirectPin, placeholder: '8 digits' });
        pinInp.addEventListener('input', () => { state.WiFiDirectPin = pinInp.value; });
        formWrap.appendChild(fieldRow('Wi-Fi Direct PIN', pinInp));
      }

      if (state.WiFiMode === '0') {
        const chSel = createEl('select', { className: 'select' });
        for (let ch = 1; ch <= 11; ch++) {
          chSel.appendChild(createEl('option', { value: String(ch), text: `Channel ${ch}` }));
        }
        chSel.value = String(state.WiFiChannel);
        chSel.addEventListener('change', () => { state.WiFiChannel = parseInt(chSel.value, 10); });
        formWrap.appendChild(fieldRow('Wi-Fi Channel', chSel));
      }

      const ipInp = createEl('input', { type: 'text', className: 'input', value: state.WiFiIPAddress, placeholder: '192.168.10.1' });
      ipInp.addEventListener('input', () => { state.WiFiIPAddress = ipInp.value; });
      formWrap.appendChild(fieldRow('Stratux IP Address', ipInp));

      if (state.WiFiMode === '2') {
        const ptSw = createEl('label', { className: 'switch' });
        const ptCb = createEl('input', { type: 'checkbox' });
        ptCb.checked = state.WiFiInternetPassThroughEnabled;
        ptCb.addEventListener('change', () => { state.WiFiInternetPassThroughEnabled = ptCb.checked; });
        ptSw.appendChild(ptCb);
        ptSw.appendChild(createEl('span', { className: 'switch-track' }));
        formWrap.appendChild(fieldRow('Internet Passthrough', ptSw));

        for (let i = 0; i < state.WiFiClientNetworks.length; i++) {
          const net = state.WiFiClientNetworks[i];
          const box = createEl('div', { className: 'wx-card', style: { marginTop: '8px' } });
          const sIn = createEl('input', { type: 'text', className: 'input input-full', value: net.SSID, placeholder: 'Client SSID' });
          sIn.addEventListener('input', () => { net.SSID = sIn.value; });
          const pIn = createEl('input', { type: 'password', className: 'input input-full', value: net.Password, placeholder: 'Client Password', style: { marginTop: '6px' } });
          pIn.addEventListener('input', () => { net.Password = pIn.value; });
          const rm = createEl('button', {
            type: 'button',
            className: 'btn btn-sm btn-danger',
            style: { marginTop: '6px' },
            text: 'Remove Network',
            onClick: () => { state.WiFiClientNetworks.splice(i, 1); drawWifiForm(); }
          });
          box.appendChild(sIn);
          box.appendChild(pIn);
          box.appendChild(rm);
          formWrap.appendChild(box);
        }

        formWrap.appendChild(createEl('button', {
          type: 'button',
          className: 'btn btn-sm',
          style: { marginTop: '8px' },
          text: '+ Add Client Wi-Fi Network',
          onClick: () => { state.WiFiClientNetworks.push({ SSID: '', Password: '' }); drawWifiForm(); }
        }));
      }

      const submitBtn = createEl('button', {
        type: 'button',
        className: 'btn btn-primary',
        style: { marginTop: '12px', width: '100%' },
        text: 'Apply Wi-Fi Configuration',
        onClick: () => {
          if (!isValidSSID(state.WiFiSSID)) {
            showToast('Invalid SSID (1-32 allowed characters)', 'error');
            return;
          }
          if (state.WiFiSecurityEnabled && !isValidWPA(state.WiFiPassphrase)) {
            showToast('WPA2 passphrase must be 8-63 printable ASCII chars', 'error');
            return;
          }
          if (state.WiFiMode === '1' && !isValidPin(state.WiFiDirectPin)) {
            showToast('Wi-Fi Direct PIN must be 8 digits', 'error');
            return;
          }
          if (state.WiFiIPAddress && !isValidIPv4(state.WiFiIPAddress.trim())) {
            showToast('Invalid Stratux IPv4 address', 'error');
            return;
          }
          savePatch({
            WiFiCountry: state.WiFiCountry,
            WiFiSSID: state.WiFiSSID,
            WiFiSecurityEnabled: state.WiFiSecurityEnabled,
            WiFiPassphrase: state.WiFiPassphrase,
            WiFiChannel: state.WiFiChannel,
            WiFiIPAddress: state.WiFiIPAddress.trim(),
            WiFiMode: parseInt(state.WiFiMode, 10),
            WiFiDirectPin: state.WiFiDirectPin,
            WiFiClientNetworks: state.WiFiClientNetworks,
            WiFiInternetPassThroughEnabled: state.WiFiInternetPassThroughEnabled
          }, 'Wi-Fi settings applied — services restarting (up to 30s)');
        }
      });
      formWrap.appendChild(submitBtn);
    }

    drawWifiForm();
    elWifi.appendChild(formWrap);
  }

  function renderWizard() {
    elWizard.textContent = '';

    // Live SDR & Hardware detection summary
    const hwBox = createEl('div', { className: 'kv-list', style: { marginBottom: '12px' } });
    hwBox.appendChild(kvRow('Detected SDR Dongles', String(status.Devices ?? 0)));
    hwBox.appendChild(kvRow('GPS / Tracker Hardware', getGpsHardwareLabel(status.GPS_detected_type)));
    hwBox.appendChild(kvRow('OGN Receiver', status.OGN_connected ? 'Connected' : 'Not connected'));
    hwBox.appendChild(kvRow('AIS Receiver', status.AIS_connected ? 'Connected' : 'Not connected'));
    elWizard.appendChild(hwBox);

    // Firmware Flasher for OGN Tracker / SoftRF
    const flashSection = createEl('div', { className: 'wx-card' });
    flashSection.appendChild(createEl('strong', { text: 'In-Browser Tracker Firmware Flasher (OGN / SoftRF)' }));
    flashSection.appendChild(createEl('div', {
      className: 'muted',
      style: { fontSize: '0.82rem', margin: '4px 0 10px' },
      text: 'Select target hardware and firmware image detected on Stratux, then flash directly over USB/serial.'
    }));

    const targetSel = createEl('select', { className: 'select' },
      createEl('option', { value: 'ogn', text: 'OGN Tracker (T-Beam / ESP32)' }),
      createEl('option', { value: 'softrf', text: 'SoftRF Dongle / Prime' })
    );
    const fwSel = createEl('select', { className: 'select' });
    const statusLbl = createEl('div', { className: 'muted mono', style: { fontSize: '0.8rem', marginTop: '8px' }, text: '' });

    function populateFwOptions() {
      fwSel.textContent = '';
      const list = targetSel.value === 'ogn'
        ? (firmwareList?.OGN || [])
        : (firmwareList?.SoftRF || []);
      if (!Array.isArray(list) || list.length === 0) {
        fwSel.appendChild(createEl('option', { value: '', text: firmwareList ? 'No firmware images found' : 'Endpoint /getFirmwareList unavailable' }));
      } else {
        for (const fw of list) {
          fwSel.appendChild(createEl('option', { value: String(fw), text: String(fw) }));
        }
      }
    }
    targetSel.addEventListener('change', populateFwOptions);
    populateFwOptions();

    const flashBtn = createEl('button', {
      type: 'button',
      className: 'btn btn-primary btn-sm',
      text: 'Flash Firmware',
      onClick: () => {
        const target = targetSel.value;
        const firmware = fwSel.value;
        if (!firmware) {
          showToast('No firmware image available to flash', 'error');
          return;
        }
        confirmModal(
          'Flash Tracker Firmware',
          `Flash "${firmware}" to ${target.toUpperCase()} hardware? Do not unplug power until complete.`,
          async () => {
            flashBtn.disabled = true;
            statusLbl.textContent = 'Flashing firmware... please wait...';
            try {
              const res = await apiPost('/flashFirmware', { Target: target, Firmware: firmware });
              if (res && typeof res === 'object' && res.Success === false) {
                const errMsg = res.Error || res.Output || 'Firmware flash failed';
                statusLbl.textContent = `Flash error: ${errMsg}`;
                showToast(`Flash failed: ${errMsg}`, 'error');
              } else {
                statusLbl.textContent = typeof res === 'string'
                  ? res
                  : (res?.Output || 'Firmware flashed successfully!');
                showToast('Firmware flash completed', 'ok');
              }
            } catch (err) {
              statusLbl.textContent = `Flash error: ${err.message}`;
              showToast(`Flash failed: ${err.message}`, 'error');
            } finally {
              flashBtn.disabled = false;
            }
          }
        );
      }
    });

    const row = createEl('div', { className: 'btn-row' }, targetSel, fwSel, flashBtn);
    flashSection.appendChild(row);
    flashSection.appendChild(statusLbl);
    elWizard.appendChild(flashSection);
  }

  function renderOutputs() {
    elOutputs.textContent = '';

    // Serial baud if present
    if (settings.SerialOutputs && Object.keys(settings.SerialOutputs).length > 0) {
      const firstKey = Object.keys(settings.SerialOutputs)[0];
      elOutputs.appendChild(inputRow(
        `Serial Output Baud (${firstKey})`,
        'FLARM / NMEA serial output rate',
        settings.SerialOutputs[firstKey]?.Baud ?? 38400,
        {
          type: 'number',
          onSave: (v) => savePatch({ Baud: parseInt(v, 10) || 38400 })
        }
      ));
    }

    const bleCount = Array.isArray(settings.BleOutputs) ? settings.BleOutputs.length : 0;
    const netCount = Array.isArray(settings.NetworkOutputs) ? settings.NetworkOutputs.length : 0;
    elOutputs.appendChild(kvRow('Configured Network / BLE Outputs', `${netCount} Network / ${bleCount} BLE`));

    elOutputs.appendChild(switchRow('Verbose Debug Log', 'Increase stratux.log verbosity', 'DEBUG'));
    elOutputs.appendChild(switchRow('Record Replay Logs', 'Record UAT/ES replay data', 'ReplayLog'));
    elOutputs.appendChild(switchRow('Record Trace Logs', 'Record multi-source trace logs', 'TraceLog'));
    elOutputs.appendChild(switchRow('Record AHRS Logs', 'Log IMU/baro CSV telemetry', 'AHRSLog'));
    elOutputs.appendChild(switchRow('Persistent Logging (SD Card)', 'Write logs to SD instead of RAM (reboot required)', 'PersistentLogging'));
    elOutputs.appendChild(switchRow('Developer Mode', 'Expose Developer tab and diagnostics', 'DeveloperMode'));

    elOutputs.appendChild(inputRow(
      'Minimum Cooling Fan Duty (%)',
      'PWM fan minimum speed (0-100)',
      settings.PWMDutyMin ?? 0,
      {
        type: 'number',
        onSave: (v) => savePatch({ PWMDutyMin: Math.max(0, Math.min(100, parseInt(v, 10) || 0)) })
      }
    ));
  }

  function renderSystem() {
    elSystem.textContent = '';

    // OTA .deb / .sh upload
    const otaBox = createEl('div', { className: 'wx-card' });
    otaBox.appendChild(createEl('strong', { text: 'Upload System Update (.deb / .sh)' }));
    const fileInp = createEl('input', { type: 'file', accept: '.deb,.sh', style: { display: 'block', margin: '8px 0' } });
    const upBtn = createEl('button', {
      type: 'button',
      className: 'btn btn-sm btn-primary',
      text: 'Install Update File',
      onClick: async () => {
        const f = fileInp.files?.[0];
        if (!f) { showToast('Select a .deb or .sh update file first', 'error'); return; }
        const fd = new FormData();
        fd.append('update_file', f);
        upBtn.disabled = true;
        upBtn.textContent = 'Uploading...';
        try {
          await apiUpload('/updateUpload', fd);
          showToast('Update uploaded! Stratux will reboot and apply it.', 'ok');
        } catch (err) {
          showToast(`Upload failed: ${err.message}`, 'error');
        } finally {
          upBtn.disabled = false;
          upBtn.textContent = 'Install Update File';
        }
      }
    });
    otaBox.appendChild(fileInp);
    otaBox.appendChild(upBtn);
    elSystem.appendChild(otaBox);

    // Pong firmware upload
    if (settings.Pong_Enabled) {
      const pongBox = createEl('div', { className: 'wx-card' });
      pongBox.appendChild(createEl('strong', { text: 'Upload Pong Firmware (pongbuild*.zip)' }));
      const pFile = createEl('input', { type: 'file', accept: '.zip', style: { display: 'block', margin: '8px 0' } });
      const pBtn = createEl('button', {
        type: 'button',
        className: 'btn btn-sm',
        text: 'Flash Pong',
        onClick: async () => {
          const f = pFile.files?.[0];
          if (!f || !/^pongbuild.*\.zip$/i.test(f.name)) {
            showToast('Select a valid pongbuild*.zip file', 'error');
            return;
          }
          const fd = new FormData();
          fd.append('pong_update_file', f);
          pBtn.disabled = true;
          try {
            await apiUpload('/updatePong', fd);
            showToast('Pong firmware uploaded — watch Pong LEDs', 'ok');
          } catch (err) {
            showToast(`Pong upload failed: ${err.message}`, 'error');
          } finally {
            pBtn.disabled = false;
          }
        }
      });
      pongBox.appendChild(pFile);
      pongBox.appendChild(pBtn);
      elSystem.appendChild(pongBox);
    }

    // Power buttons
    const pwrRow = createEl('div', { className: 'btn-row', style: { marginTop: '12px' } });
    pwrRow.appendChild(createEl('button', {
      type: 'button',
      className: 'btn',
      text: 'Restart Stratux App',
      onClick: () => confirmModal('Restart Stratux', 'Restart the Stratux daemon without rebooting the Pi?', () => {
        apiPost('/restart').then(() => showToast('Restarting Stratux...', 'ok')).catch((e) => showToast(e.message, 'error'));
      })
    }));
    pwrRow.appendChild(createEl('button', {
      type: 'button',
      className: 'btn btn-danger',
      text: 'Reboot Pi',
      onClick: () => confirmModal('Reboot Stratux', 'Reboot the Raspberry Pi now? Wi-Fi will disconnect briefly.', () => {
        apiPost('/reboot').then(() => showToast('Rebooting Stratux...', 'ok')).catch((e) => showToast(e.message, 'error'));
      })
    }));
    pwrRow.appendChild(createEl('button', {
      type: 'button',
      className: 'btn btn-danger',
      text: 'Shutdown Pi',
      onClick: () => confirmModal('Shutdown Stratux', 'Shut down the Raspberry Pi safely before removing power?', () => {
        apiPost('/shutdown').then(() => showToast('Shutting down Stratux...', 'ok')).catch((e) => showToast(e.message, 'error'));
      })
    }));
    elSystem.appendChild(pwrRow);
  }

  function renderTracker() {
    const gpsCode = (status.GPS_detected_type || 0) & 0x0f;
    const hasTracker = gpsCode === 3 || status.OGN_tx_enabled || gpsCode === 13 || gpsCode === 15;
    elTrackerCard.classList.toggle('hidden', !hasTracker);
    if (!hasTracker) return;

    elTracker.textContent = '';
    const addrType = createEl('select', { className: 'select' },
      createEl('option', { value: '0', text: 'Random' }),
      createEl('option', { value: '1', text: 'ICAO' }),
      createEl('option', { value: '2', text: 'FLARM' }),
      createEl('option', { value: '3', text: 'OGN' })
    );
    addrType.value = String(settings.OGNAddrType ?? 0);

    const addrInp = createEl('input', { type: 'text', className: 'input', maxlength: '6', value: settings.OGNAddr || '' });
    const pilotInp = createEl('input', { type: 'text', className: 'input', maxlength: '15', value: settings.OGNPilot || '' });
    const regInp = createEl('input', { type: 'text', className: 'input', maxlength: '15', value: settings.OGNReg || '' });
    const pwrInp = createEl('input', { type: 'number', className: 'input', min: '-32', max: '31', value: settings.OGNTxPower ?? 14 });

    elTracker.appendChild(fieldRow('Address Type', addrType));
    elTracker.appendChild(fieldRow('Tracker Hex Address', addrInp));
    elTracker.appendChild(fieldRow('Pilot Name', pilotInp));
    elTracker.appendChild(fieldRow('Registration', regInp));
    elTracker.appendChild(fieldRow('TX Power (dBm)', pwrInp));
    elTracker.appendChild(createEl('button', {
      type: 'button',
      className: 'btn btn-primary',
      style: { marginTop: '10px' },
      text: 'Configure Tracker',
      onClick: () => savePatch({
        OGNAddrType: parseInt(addrType.value, 10),
        OGNAddr: addrInp.value.trim().toUpperCase(),
        OGNPilot: pilotInp.value.trim(),
        OGNReg: regInp.value.trim(),
        OGNTxPower: parseInt(pwrInp.value, 10)
      }, 'Tracker configuration sent')
    }));
  }

  function renderAll() {
    renderReceivers();
    renderFlightConfig();
    renderWifi();
    renderWizard();
    renderOutputs();
    renderSystem();
    renderTracker();
  }

  try {
    const [s, st, fw] = await Promise.all([
      apiGet('/getSettings'),
      apiGet('/getStatus', { optional: true }),
      apiGet('/getFirmwareList', { optional: true })
    ]);
    if (destroyed) return () => {};
    settings = s || {};
    status = st || {};
    firmwareList = fw;
    AppState.updateLocalSettings(settings);
    setConnectionBadge('connected', 'Live');
    renderAll();
  } catch (err) {
    if (!destroyed) {
      setConnectionBadge('disconnected', 'Offline');
      showToast(`Failed to load settings: ${err.message}`, 'error');
    }
  }

  return function destroy() {
    destroyed = true;
  };
}

function fieldRow(label, controlEl) {
  const row = createEl('div', { className: 'form-row' });
  row.appendChild(createEl('div', { className: 'form-label', text: label }));
  row.appendChild(controlEl);
  return row;
}

function kvRow(key, val) {
  const row = createEl('div', { className: 'kv-row' });
  row.appendChild(createEl('span', { className: 'kv-key', text: key }));
  row.appendChild(createEl('span', { className: 'kv-val mono', text: val }));
  return row;
}
