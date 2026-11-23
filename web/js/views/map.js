import {
  apiGet,
  createEl,
  escapeHtml,
  getCraftHexColor,
  isTrafficAged,
  setConnectionBadge,
  StratuxWS,
  TARGET_TYPE_AIS,
  TRAFFIC_MAX_AGE_SECONDS
} from '../app.js';

function getAircraftIcon(ac) {
  if (ac.TargetType === TARGET_TYPE_AIS) return ['img/actype/vessel.svg', true];
  switch (ac.Emitter_category) {
    case 1:
    case 6: return ['img/actype/light.svg', true];
    case 2:
    case 3:
    case 4:
    case 5: return ['img/actype/heavy.svg', true];
    case 7: return ['img/actype/helicopter.svg', true];
    case 9: return ['img/actype/glider.svg', true];
    case 10: return ['img/actype/lighter-than-air.svg', false];
    case 11:
    case 12: return ['img/actype/skydiver.svg', false];
    default: return ['img/actype/undef.svg', true];
  }
}

function productColor(name = '') {
  const n = String(name).toUpperCase();
  if (n.includes('TFR') || n.includes('SIGMET')) return { stroke: '#ef4444', fill: 'rgba(239, 68, 68, 0.22)' };
  if (n.includes('AIRMET') || n.includes('G-AIRMET')) return { stroke: '#f59e0b', fill: 'rgba(245, 158, 11, 0.20)' };
  if (n.includes('SUA')) return { stroke: '#a855f7', fill: 'rgba(168, 85, 247, 0.20)' };
  return { stroke: '#38bdf8', fill: 'rgba(56, 189, 248, 0.18)' };
}

export async function mount(container) {
  let destroyed = false;

  container.innerHTML = `
    <div class="map-toolbar">
      <div class="btn-row">
        <span class="badge badge-ok" id="map-ac-count">0 Targets</span>
        <span class="badge" id="map-wx-count">0 FIS-B Polygons</span>
      </div>
      <div class="btn-row">
        <button type="button" class="btn btn-sm" id="map-center-own">Center Ownship</button>
        <button type="button" class="btn btn-sm" id="map-refresh-wx">Refresh Weather</button>
      </div>
    </div>
    <div class="map-stage" id="map-stage">
      <div id="map-canvas" style="width:100%;height:100%;"></div>
      <div id="map-popup" class="map-popup hidden"></div>
    </div>
  `;

  if (typeof window.ol === 'undefined') {
    container.querySelector('#map-stage').appendChild(
      createEl('div', { className: 'alert-banner alert-danger', text: 'OpenLayers library (js/ol.js) failed to load.' })
    );
    return () => {};
  }

  const ol = window.ol;
  const elAcCount = container.querySelector('#map-ac-count');
  const elWxCount = container.querySelector('#map-wx-count');
  const elPopup = container.querySelector('#map-popup');

  const aircraftSymbolsSource = new ol.source.Vector();
  const aircraftTrailsSource = new ol.source.Vector();
  const weatherOverlaySource = new ol.source.Vector();
  const ownshipSource = new ol.source.Vector();

  function initLayerVisibility(layer) {
    const title = layer.get('title');
    if (!title) return;
    const key = `stratux.map.layers.${title}.visible`;
    try {
      const saved = localStorage.getItem(key);
      if (saved !== null) layer.setVisible(saved === 'true');
      layer.on('change:visible', (ev) => {
        try {
          localStorage.setItem(key, String(ev.target.get('visible')));
        } catch {}
      });
    } catch {}
  }

  const osmLayer = new ol.layer.Tile({
    title: 'OSM (Online)',
    type: 'base',
    source: new ol.source.OSM()
  });

  const weatherLayer = new ol.layer.Vector({
    title: 'FIS-B Weather (AIRMET/SIGMET/TFR/SUA)',
    source: weatherOverlaySource,
    zIndex: 8
  });

  const trailsLayer = new ol.layer.Vector({
    title: 'Aircraft Trails',
    source: aircraftTrailsSource,
    zIndex: 9
  });

  const symbolsLayer = new ol.layer.Vector({
    title: 'Aircraft Symbols',
    source: aircraftSymbolsSource,
    zIndex: 10
  });

  const ownshipLayer = new ol.layer.Vector({
    title: 'Ownship GPS',
    source: ownshipSource,
    zIndex: 11
  });

  const map = new ol.Map({
    target: container.querySelector('#map-canvas'),
    layers: [osmLayer, weatherLayer, trailsLayer, symbolsLayer, ownshipLayer],
    view: new ol.View({
      center: ol.proj.fromLonLat([-98.5, 39.8]),
      zoom: 5,
      enableRotation: false
    })
  });

  if (ol.control && ol.control.LayerSwitcher) {
    map.addControl(new ol.control.LayerSwitcher());
  }
  map.getLayers().forEach(initLayerVisibility);

  // Dynamic offline MBTiles layers (strictly HTML-escaped titles!)
  apiGet('/tiles/tilesets', { optional: true }).then((tilesets) => {
    if (destroyed || !tilesets || typeof tilesets !== 'object') return;
    for (const file of Object.keys(tilesets)) {
      const meta = tilesets[file] || {};
      const safeTitle = escapeHtml(meta.name || file);
      const isBase = meta.type === 'baselayer';
      const format = meta.format || 'png';
      const minZoom = meta.minzoom ? parseInt(meta.minzoom, 10) : 1;
      const maxZoom = meta.maxzoom ? parseInt(meta.maxzoom, 10) : 18;
      let ext = [-180, -85, 180, 85];
      if (meta.bounds) {
        const parsed = String(meta.bounds).split(',').map(Number);
        if (parsed.length === 4 && parsed.every(Number.isFinite)) ext = parsed;
      }
      ext = ol.proj.transformExtent(ext, 'EPSG:4326', 'EPSG:3857');

      let layer;
      if (format.toLowerCase() === 'pbf') {
        const vt = new ol.layer.VectorTile({
          title: safeTitle,
          type: isBase ? 'base' : 'overlay',
          extent: ext,
          source: new ol.source.VectorTile({
            url: `/tiles/${encodeURIComponent(file)}/{z}/{x}/{-y}.${encodeURIComponent(format)}`,
            format: new ol.format.MVT(),
            minZoom,
            maxZoom
          })
        });
        if (meta.stratux_style_url && window.olms) {
          fetch(meta.stratux_style_url)
            .then((r) => r.json())
            .then((style) => { if (!destroyed) window.olms.stylefunction(vt, style, meta.id); })
            .catch(() => {});
        }
        layer = vt;
      } else {
        layer = new ol.layer.Tile({
          title: safeTitle,
          type: isBase ? 'base' : 'overlay',
          extent: ext,
          source: new ol.source.XYZ({
            url: `/tiles/${encodeURIComponent(file)}/{z}/{x}/{-y}.${encodeURIComponent(format)}`,
            minZoom,
            maxZoom
          })
        });
      }
      if (isBase) map.getLayers().insertAt(0, layer);
      else map.addLayer(layer);
      initLayerVisibility(layer);
    }
  }).catch(() => {});

  // FIS-B Graphical Weather Overlay
  async function loadGraphicalWeather() {
    try {
      const items = await apiGet('/getGraphicalWeather', { optional: true });
      if (destroyed) return;
      weatherOverlaySource.clear();
      if (!Array.isArray(items)) {
        elWxCount.textContent = '0 FIS-B Polygons';
        return;
      }

      let count = 0;
      for (const item of items) {
        const styleColors = productColor(item.ProductName || item.ProductID);
        let geom = null;

        if (item.GeometryType === 'circle' && Array.isArray(item.Coordinates) && item.Coordinates.length >= 1) {
          const c = item.Coordinates[0];
          const lon = Array.isArray(c) ? c[0] : (c.Lon ?? c.Lng);
          const lat = Array.isArray(c) ? c[1] : c.Lat;
          const radiusMeters = (Number(item.RadiusNM) || 5) * 1852;
          if (Number.isFinite(lon) && Number.isFinite(lat)) {
            geom = new ol.geom.Circle(ol.proj.fromLonLat([lon, lat]), radiusMeters);
          }
        } else if (Array.isArray(item.Coordinates) && item.Coordinates.length >= 2) {
          const pts = item.Coordinates
            .map((c) => {
              const lon = Array.isArray(c) ? c[0] : (c.Lon ?? c.Lng);
              const lat = Array.isArray(c) ? c[1] : c.Lat;
              return (Number.isFinite(lon) && Number.isFinite(lat)) ? ol.proj.fromLonLat([lon, lat]) : null;
            })
            .filter(Boolean);
          if (pts.length >= 3) {
            if (pts[0][0] !== pts[pts.length - 1][0] || pts[0][1] !== pts[pts.length - 1][1]) {
              pts.push(pts[0]);
            }
            geom = new ol.geom.Polygon([pts]);
          } else if (pts.length === 2) {
            geom = new ol.geom.LineString(pts);
          }
        }

        if (!geom) continue;
        const feat = new ol.Feature({ geometry: geom });
        feat.set('wxItem', item);
        feat.setStyle(new ol.style.Style({
          stroke: new ol.style.Stroke({ color: styleColors.stroke, width: 2 }),
          fill: new ol.style.Fill({ color: styleColors.fill })
        }));
        weatherOverlaySource.addFeature(feat);
        count++;
      }
      elWxCount.textContent = `${count} FIS-B Polygon${count === 1 ? '' : 's'}`;
    } catch {}
  }

  // Click popup for weather polygons & aircraft
  map.on('singleclick', (ev) => {
    let shown = false;
    map.forEachFeatureAtPixel(ev.pixel, (feature) => {
      if (shown) return;
      const wx = feature.get('wxItem');
      const ac = feature.get('acItem');
      if (wx) {
        showPopup(
          `${wx.ProductName || 'FIS-B'} ${wx.Location ? `(${wx.Location})` : ''}`,
          [
            wx.AltLowFt || wx.AltHighFt ? `Altitudes: ${wx.AltLowFt ?? 0} - ${wx.AltHighFt ?? 'UNL'} ft` : '',
            wx.ValidStart || wx.ValidEnd ? `Valid: ${wx.ValidStart || '?'} to ${wx.ValidEnd || '?'}` : '',
            wx.Text || ''
          ].filter(Boolean)
        );
        shown = true;
      } else if (ac) {
        const hex = (ac.Icao_addr >>> 0).toString(16).toUpperCase().padStart(6, '0');
        showPopup(
          `${ac.Tail || hex}`,
          [
            `Alt: ${Math.round(ac.Alt || 0)} ft | Spd: ${ac.Speed_valid ? Math.round(ac.Speed) + ' kt' : '--'} | Hdg: ${ac.Speed_valid ? Math.round(ac.Track) + '\u00B0' : '--'}`,
            ac.Callout ? `CPA: ${ac.Callout}` : ''
          ].filter(Boolean)
        );
        shown = true;
      }
    });
    if (!shown) elPopup.classList.add('hidden');
  });

  function showPopup(title, lines) {
    elPopup.textContent = '';
    const head = createEl('div', { className: 'card-header', style: { marginBottom: '6px', paddingBottom: '6px' } });
    head.appendChild(createEl('strong', { text: title }));
    const close = createEl('button', { type: 'button', className: 'btn btn-sm', text: '\u00D7', onClick: () => elPopup.classList.add('hidden') });
    head.appendChild(close);
    elPopup.appendChild(head);
    for (const line of lines) {
      elPopup.appendChild(createEl('div', { className: 'mono', style: { fontSize: '0.82rem', marginTop: '4px', whiteSpace: 'pre-wrap' }, text: line }));
    }
    elPopup.classList.remove('hidden');
  }

  // Aircraft tracking
  const aircraftMap = new Map();
  let ownPosLonLat = null;
  let ownFeature = null;
  let centeredOnce = false;

  function updateAircraft(ac) {
    if (!ac || !ac.Position_valid || isTrafficAged(ac)) return;
    const key = `${ac.Icao_addr}:${ac.Addr_type === 1 ? 1 : 0}`;
    const pos = ol.proj.fromLonLat([ac.Lng, ac.Lat]);
    const color = getCraftHexColor(ac);
    const [iconSrc, rotatable] = getAircraftIcon(ac);
    let entry = aircraftMap.get(key);

    if (!entry) {
      const marker = new ol.Feature({ geometry: new ol.geom.Point(pos) });
      marker.setStyle(new ol.style.Style({
        image: new ol.style.Icon({
          src: iconSrc,
          color,
          rotation: rotatable ? ((ac.Track || 0) * Math.PI) / 180 : 0,
          anchor: [0.5, 0.5],
          opacity: 1
        }),
        text: new ol.style.Text({
          text: '',
          offsetY: ac.TargetType === TARGET_TYPE_AIS ? 20 : 34,
          font: 'bold 12px ui-monospace, monospace',
          fill: new ol.style.Fill({ color: '#ffffff' }),
          stroke: new ol.style.Stroke({ color: '#000000', width: 3 })
        })
      }));
      aircraftSymbolsSource.addFeature(marker);
      entry = {
        ac,
        marker,
        trailFeature: null,
        history: [[ac.Lng, ac.Lat]],
        receivedAt: Date.now(),
        color,
        emitterCat: ac.Emitter_category
      };
      aircraftMap.set(key, entry);
    } else {
      entry.ac = ac;
      entry.receivedAt = Date.now();
      entry.marker.getGeometry().setCoordinates(pos);
      const last = entry.history[entry.history.length - 1];
      if (Math.hypot(ac.Lng - last[0], ac.Lat - last[1]) > 0.001) {
        entry.history.push([ac.Lng, ac.Lat]);
        if (entry.history.length > 50) entry.history.shift();
      }
      if (entry.history.length >= 2) {
        const coords = entry.history.map((c) => ol.proj.fromLonLat(c));
        if (!entry.trailFeature) {
          entry.trailFeature = new ol.Feature({ geometry: new ol.geom.LineString(coords) });
          entry.trailFeature.setStyle(new ol.style.Style({
            stroke: new ol.style.Stroke({ color, width: 2 })
          }));
          aircraftTrailsSource.addFeature(entry.trailFeature);
        } else {
          entry.trailFeature.getGeometry().setCoordinates(coords);
        }
      }
      if (entry.color !== color || entry.emitterCat !== ac.Emitter_category) {
        entry.color = color;
        entry.emitterCat = ac.Emitter_category;
        entry.marker.getStyle().setImage(new ol.style.Icon({
          src: iconSrc,
          color,
          rotation: rotatable ? ((ac.Track || 0) * Math.PI) / 180 : 0,
          anchor: [0.5, 0.5]
        }));
      }
    }

    entry.marker.set('acItem', ac);
    const labelParts = [];
    if (ac.Tail && ac.Tail.trim()) labelParts.push(ac.Tail.trim());
    if (ac.TargetType !== TARGET_TYPE_AIS) labelParts.push(`${Math.round(ac.Alt || 0)}ft`);
    entry.marker.getStyle().getText().setText(labelParts.join('\n'));
    const img = entry.marker.getStyle().getImage();
    if (img) {
      if (rotatable) img.setRotation(((ac.Track || 0) * Math.PI) / 180);
      const op = ac.TargetType === TARGET_TYPE_AIS ? 1 : Math.max(0.2, 1 - (ac.Age || 0) / TRAFFIC_MAX_AGE_SECONDS);
      img.setOpacity(op);
    }
    elAcCount.textContent = `${aircraftMap.size} Target${aircraftMap.size === 1 ? '' : 's'}`;
  }

  function updateOwnship(sit) {
    if (!sit || sit.GPSFixQuality <= 0 || (sit.GPSLatitude === 0 && sit.GPSLongitude === 0)) return;
    ownPosLonLat = [sit.GPSLongitude, sit.GPSLatitude];
    const proj = ol.proj.fromLonLat(ownPosLonLat);
    if (!ownFeature) {
      ownFeature = new ol.Feature({ geometry: new ol.geom.Point(proj) });
      ownFeature.setStyle(new ol.style.Style({
        image: new ol.style.Circle({
          radius: 7,
          fill: new ol.style.Fill({ color: '#38bdf8' }),
          stroke: new ol.style.Stroke({ color: '#ffffff', width: 2 })
        })
      }));
      ownshipSource.addFeature(ownFeature);
    } else {
      ownFeature.getGeometry().setCoordinates(proj);
    }
    if (!centeredOnce) {
      centeredOnce = true;
      map.getView().setCenter(proj);
      map.getView().setZoom(10);
    }
  }

  container.querySelector('#map-center-own').addEventListener('click', () => {
    if (ownPosLonLat) {
      map.getView().animate({ center: ol.proj.fromLonLat(ownPosLonLat), zoom: 10, duration: 300 });
    }
  });
  container.querySelector('#map-refresh-wx').addEventListener('click', loadGraphicalWeather);

  const trafficWS = new StratuxWS('/traffic', {
    onOpen: () => setConnectionBadge('connected', 'Live'),
    onClose: () => setConnectionBadge('reconnecting', 'Reconnecting'),
    onError: () => setConnectionBadge('error', 'Error'),
    onMessage: (ac) => { if (!destroyed) updateAircraft(ac); }
  });

  const sitWS = new StratuxWS('/situation', {
    onMessage: (sit) => { if (!destroyed) updateOwnship(sit); }
  });

  loadGraphicalWeather();
  const wxTimer = setInterval(loadGraphicalWeather, 60000);
  const ageTimer = setInterval(() => {
    const now = Date.now();
    for (const [k, entry] of aircraftMap.entries()) {
      const ageSec = (entry.ac.Age || 0) + (now - entry.receivedAt) / 1000;
      if (isTrafficAged({ ...entry.ac, Age: ageSec })) {
        aircraftSymbolsSource.removeFeature(entry.marker);
        if (entry.trailFeature) aircraftTrailsSource.removeFeature(entry.trailFeature);
        aircraftMap.delete(k);
      }
    }
    elAcCount.textContent = `${aircraftMap.size} Target${aircraftMap.size === 1 ? '' : 's'}`;
  }, 2000);

  return function destroy() {
    destroyed = true;
    trafficWS.close();
    sitWS.close();
    clearInterval(wxTimer);
    clearInterval(ageTimer);
    map.setTarget(null);
  };
}
