import {
  apiGet,
  createEl,
  parseFlightCategory,
  setConnectionBadge,
  StratuxWS
} from '../app.js';

const MAX_TEXT_ITEMS = 80;

export async function mount(container) {
  let destroyed = false;
  let watchListStr = 'KBOS KATL KORD KLAX';
  let typeFilter = 'ALL';
  let queryFilter = '';
  const textMessages = [];
  let graphicalProducts = [];

  container.innerHTML = `
    <div class="radar-toolbar">
      <div class="tabs" id="wx-tabs">
        <button type="button" class="tab-btn active" data-type="ALL">All</button>
        <button type="button" class="tab-btn" data-type="METAR">METAR</button>
        <button type="button" class="tab-btn" data-type="TAF">TAF</button>
        <button type="button" class="tab-btn" data-type="PIREP">PIREP</button>
        <button type="button" class="tab-btn" data-type="NOTAM">NOTAM</button>
        <button type="button" class="tab-btn" data-type="WINDS">Winds</button>
        <button type="button" class="tab-btn" data-type="GRAPHICAL">Graphical FIS-B</button>
      </div>
      <div class="btn-row">
        <input type="search" class="input" id="wx-search" placeholder="Search station or text..." aria-label="Search weather" />
        <button type="button" class="btn btn-sm" id="wx-refresh-gfx">Refresh Graphical</button>
      </div>
    </div>

    <section class="card" id="wx-watch-section">
      <div class="card-header">
        <h2 class="card-title">Watch List</h2>
        <span class="badge mono" id="wx-watch-lbl">--</span>
      </div>
      <div id="wx-watch-list"></div>
    </section>

    <section class="card" id="wx-gfx-section">
      <div class="card-header">
        <h2 class="card-title">FIS-B Graphical Products (AIRMET / SIGMET / TFR / SUA)</h2>
        <span class="badge" id="wx-gfx-count">0</span>
      </div>
      <div id="wx-gfx-list"></div>
    </section>

    <section class="card" id="wx-text-section">
      <div class="card-header">
        <h2 class="card-title">Live Text Weather Stream</h2>
        <span class="badge" id="wx-text-count">0</span>
      </div>
      <div id="wx-text-list"></div>
    </section>
  `;

  const elWatchLbl = container.querySelector('#wx-watch-lbl');
  const elWatchList = container.querySelector('#wx-watch-list');
  const elGfxSection = container.querySelector('#wx-gfx-section');
  const elGfxCount = container.querySelector('#wx-gfx-count');
  const elGfxList = container.querySelector('#wx-gfx-list');
  const elTextSection = container.querySelector('#wx-text-section');
  const elTextCount = container.querySelector('#wx-text-count');
  const elTextList = container.querySelector('#wx-text-list');
  const elSearch = container.querySelector('#wx-search');

  container.querySelectorAll('#wx-tabs .tab-btn').forEach((btn) => {
    btn.addEventListener('click', () => {
      typeFilter = btn.dataset.type;
      container.querySelectorAll('#wx-tabs .tab-btn').forEach((b) => b.classList.toggle('active', b === btn));
      renderAll();
    });
  });

  elSearch.addEventListener('input', () => {
    queryFilter = elSearch.value.trim().toLowerCase();
    renderAll();
  });

  container.querySelector('#wx-refresh-gfx').addEventListener('click', loadGraphical);

  function normalizeType(raw) {
    const t = String(raw || '').toUpperCase();
    if (t === 'TAF.AMD') return 'TAF';
    if (t === 'SPECI') return 'METAR';
    if (t.includes('WIND')) return 'WINDS';
    return t || 'OTHER';
  }

  function inWatchList(loc) {
    if (!loc || !watchListStr) return false;
    return watchListStr.split(/\s+/).includes(String(loc).toUpperCase());
  }

  function renderWxCard(item) {
    const card = createEl('div', { className: 'wx-card' });
    const head = createEl('div', { className: 'sparkline-head' });
    const left = createEl('div', { className: 'btn-row' });
    left.appendChild(createEl('strong', { className: 'mono', text: item.Location || '----' }));
    left.appendChild(createEl('span', { className: 'badge', text: item.Type || 'WX' }));
    if (item.category) {
      left.appendChild(createEl('span', { className: `badge pill-${item.category}`, text: item.category }));
    }
    head.appendChild(left);
    head.appendChild(createEl('span', { className: 'muted mono', style: { fontSize: '0.78rem' }, text: item.Time || '' }));
    card.appendChild(head);
    card.appendChild(createEl('pre', { className: 'wx-body', text: item.Data || '' }));
    return card;
  }

  function matches(item) {
    if (queryFilter) {
      const hay = `${item.Location || ''} ${item.Type || ''} ${item.Data || ''} ${item.ProductName || ''} ${item.Text || ''}`.toLowerCase();
      if (!hay.includes(queryFilter)) return false;
    }
    if (typeFilter === 'ALL' || typeFilter === 'GRAPHICAL') return true;
    return normalizeType(item.Type) === typeFilter;
  }

  function renderAll() {
    elWatchLbl.textContent = watchListStr || 'None';

    // Watchlist items
    const watched = textMessages.filter((m) => inWatchList(m.Location) && matches(m));
    elWatchList.textContent = '';
    if (watched.length === 0) {
      elWatchList.appendChild(createEl('div', { className: 'muted', text: 'No matching watch-list reports received yet.' }));
    } else {
      for (const item of watched) elWatchList.appendChild(renderWxCard(item));
    }

    // Graphical products
    elGfxSection.classList.toggle('hidden', typeFilter !== 'ALL' && typeFilter !== 'GRAPHICAL');
    const filteredGfx = graphicalProducts.filter((g) => {
      if (!queryFilter) return true;
      return `${g.ProductName || ''} ${g.Location || ''} ${g.Text || ''}`.toLowerCase().includes(queryFilter);
    });
    elGfxCount.textContent = String(filteredGfx.length);
    elGfxList.textContent = '';
    if (filteredGfx.length === 0) {
      elGfxList.appendChild(createEl('div', { className: 'muted', text: 'No graphical FIS-B products available.' }));
    } else {
      for (const g of filteredGfx) {
        const card = createEl('div', { className: 'wx-card' });
        const head = createEl('div', { className: 'sparkline-head' });
        const left = createEl('div', { className: 'btn-row' });
        left.appendChild(createEl('strong', { text: g.ProductName || `Product ${g.ProductID ?? ''}` }));
        if (g.Location) left.appendChild(createEl('span', { className: 'badge mono', text: g.Location }));
        if (g.AltLowFt || g.AltHighFt) {
          left.appendChild(createEl('span', { className: 'badge mono', text: `${g.AltLowFt ?? 0}-${g.AltHighFt ?? 'UNL'} ft` }));
        }
        head.appendChild(left);
        if (g.ValidStart || g.ValidEnd) {
          head.appendChild(createEl('span', { className: 'muted mono', style: { fontSize: '0.78rem' }, text: `${g.ValidStart || '?'} \u2192 ${g.ValidEnd || '?'}` }));
        }
        card.appendChild(head);
        if (g.Text) card.appendChild(createEl('pre', { className: 'wx-body', text: g.Text }));
        elGfxList.appendChild(card);
      }
    }

    // Text stream
    elTextSection.classList.toggle('hidden', typeFilter === 'GRAPHICAL');
    const filteredText = textMessages.filter(matches);
    elTextCount.textContent = String(filteredText.length);
    elTextList.textContent = '';
    if (filteredText.length === 0) {
      elTextList.appendChild(createEl('div', { className: 'muted', text: 'Waiting for FIS-B weather broadcasts on 978 MHz UAT...' }));
    } else {
      for (const item of filteredText) elTextList.appendChild(renderWxCard(item));
    }
  }

  async function loadGraphical() {
    try {
      const res = await apiGet('/getGraphicalWeather', { optional: true });
      if (destroyed) return;
      graphicalProducts = Array.isArray(res) ? res : [];
      renderAll();
    } catch {}
  }

  apiGet('/getSettings', { optional: true }).then((s) => {
    if (!destroyed && s && typeof s.WatchList === 'string') {
      watchListStr = s.WatchList.toUpperCase().trim();
      renderAll();
    }
  }).catch(() => {});

  const wxWS = new StratuxWS('/weather', {
    onOpen: () => setConnectionBadge('connected', 'Live'),
    onClose: () => setConnectionBadge('reconnecting', 'Reconnecting'),
    onError: () => setConnectionBadge('error', 'Error'),
    onMessage: (msg) => {
      if (destroyed || !msg) return;
      const item = {
        Type: msg.Type || 'WX',
        Location: msg.Location || '',
        Time: msg.Time || '',
        Data: msg.Data || '',
        category: parseFlightCategory(msg.Type, msg.Data)
      };
      // Deduplicate by Type+Location so watch list stays crisp
      const existingIdx = textMessages.findIndex((x) => x.Type === item.Type && x.Location === item.Location && x.Data === item.Data);
      if (existingIdx >= 0) textMessages.splice(existingIdx, 1);
      textMessages.unshift(item);
      if (textMessages.length > MAX_TEXT_ITEMS) textMessages.pop();
      renderAll();
    }
  });

  loadGraphical();
  const gfxTimer = setInterval(loadGraphical, 60000);
  renderAll();

  return function destroy() {
    destroyed = true;
    wxWS.close();
    clearInterval(gfxTimer);
  };
}
