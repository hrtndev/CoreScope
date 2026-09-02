// Hash Region Coverage overlay — shared between map.js and live.js.
//
// Renders /api/scope-coverage's per-region convex hulls as colored map
// shapes — the positions of repeaters/rooms that have actually RELAYED
// traffic for that region (not just self-declared it), so an inferred
// coverage area, not an authoritative boundary; hash regions carry no
// shape of their own. See cmd/server/scope_coverage.go.
//
// Usage (per Leaflet map instance):
//   const overlay = createScopeCoverageOverlay(map, {
//     checkboxId: 'mcScopeCoverage', labelId: 'mcScopeCoverageLabel',
//     storageKey: 'meshcore-map-scope-coverage'
//   });
//   overlay.load();          // fetch + render, wires checkbox + map events (async, fire-and-forget)
//   overlay.refreshTheme();  // call on 'theme-refresh' — colors are baked into shape styles
//   overlay.destroy();       // call when leaving the page
//
// Each call to createScopeCoverageOverlay owns its own state (layer, fetched
// data, shape-by-name index, shared hover tooltip) — safe to have one
// instance per map even if more than one were ever alive at once.

// Deterministic string -> 8-hex-char digest (FNV-1a 32-bit) so region
// names (arbitrary strings like "#eu", not hex hashes) can feed
// HashColor.hashToHsl the same way packet-hash coloring does elsewhere
// (live.js, packets.js) — same visual language, no new color system.
// Module-level (not per-overlay-instance) and unprefixed because other
// pages that show region-tagged nodes without their own coverage overlay
// (e.g. the standalone Regions page) need the exact same name -> color
// mapping to stay visually consistent with the coverage shapes.
function scopeCoverageRegionNameToHex(name) {
  var h = 0x811c9dc5;
  for (var i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = (h * 0x01000193) >>> 0;
  }
  return ('00000000' + h.toString(16)).slice(-8);
}

function scopeCoverageIsDarkTheme() {
  return document.documentElement.getAttribute('data-theme') === 'dark' ||
    (document.documentElement.getAttribute('data-theme') !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches);
}

// The name-to-color mapping itself — the only place a region name becomes
// an actual color. Every polygon fill, marker fill, and swatch anywhere in
// the app must go through this so the same region always renders the same
// color no matter which page/overlay is drawing it.
function scopeCoverageRegionColor(name) {
  return window.HashColor ? HashColor.hashToHsl(scopeCoverageRegionNameToHex(name), scopeCoverageIsDarkTheme() ? 'dark' : 'light') : '#888';
}
function scopeCoverageRegionSwatchHtml(name) {
  return '<span style="display:inline-block;width:9px;height:9px;border-radius:50%;background:' +
    scopeCoverageRegionColor(name) + ';margin-right:5px;vertical-align:middle;"></span>';
}

function createScopeCoverageOverlay(map, opts) {
  var checkboxId = opts.checkboxId;
  var labelId = opts.labelId;
  var storageKey = opts.storageKey;

  var esc = (typeof escapeHtml === 'function') ? escapeHtml : function (s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  };

  var layer = null;
  var data = null; // last-fetched /api/scope-coverage response, kept for theme re-render
  var shapesByName = null; // region name -> its Leaflet shape, for highlight wiring
  var hoverTooltip = null; // shared tooltip instance for the map-level mousemove handler

  // Rough planar polygon area (shoelace formula) — not a real geographic
  // measurement, just a relative size used to (a) paint larger regions
  // first so they sit visually "under" smaller ones by default, and (b)
  // order a multi-region match list smallest/most-specific first. Actual
  // hover/click selection does NOT depend on paint order — see
  // _pointInHull below — because regions often overlap in ways that
  // aren't cleanly nested (a "smaller-sounding" region's hull can end up
  // numerically larger than a region that contains it, if even one member
  // node's position pulls it wide), so no fixed z-order can guarantee
  // every region keeps an exposed, clickable sliver.
  function _hullArea(hull) {
    if (!hull || hull.length < 3) return 0;
    var area = 0;
    for (var i = 0; i < hull.length; i++) {
      var p1 = hull[i], p2 = hull[(i + 1) % hull.length];
      area += p1[0] * p2[1] - p2[0] * p1[1];
    }
    return Math.abs(area / 2);
  }

  // Ray-casting point-in-polygon test. hull is [[lat,lon], ...]; treats
  // lat/lon as generic planar x/y, consistent with _hullArea/convexHull.
  function _pointInHull(latlng, hull) {
    if (!hull || hull.length < 3) return false;
    var x = latlng.lat, y = latlng.lng;
    var inside = false;
    for (var i = 0, j = hull.length - 1; i < hull.length; j = i++) {
      var xi = hull[i][0], yi = hull[i][1];
      var xj = hull[j][0], yj = hull[j][1];
      var intersect = ((yi > y) !== (yj > y)) && (x < (xj - xi) * (y - yi) / (yj - yi) + xi);
      if (intersect) inside = !inside;
    }
    return inside;
  }

  // Every polygon region (3+ point hull) whose area actually contains
  // latlng — computed directly by geometry, not by which shape happens to
  // be drawn on top, so a region buried under several others at a given
  // pixel is still found here. Sorted smallest-area first (most specific).
  function _polygonMatchesAt(latlng) {
    if (!data || !data.regions) return [];
    return data.regions
      .filter(function (r) { return r.hull && r.hull.length >= 3 && _pointInHull(latlng, r.hull); })
      .sort(function (a, b) { return _hullArea(a.hull) - _hullArea(b.hull); });
  }

  // Single source of truth for "which regions are currently highlighted."
  // Resets every shape not in names to its base style, applies the hover
  // style + bringToFront to every shape in names.
  function _setHighlighted(names) {
    var wanted = {};
    (names || []).forEach(function (n) { wanted[n] = true; });
    Object.keys(shapesByName || {}).forEach(function (name) {
      var shape = shapesByName[name];
      if (!shape) return;
      if (wanted[name]) {
        if (shape._scopeHoverStyle) shape.setStyle(shape._scopeHoverStyle);
        if (shape.bringToFront) shape.bringToFront();
      } else if (shape._scopeBaseStyle) {
        shape.setStyle(shape._scopeBaseStyle);
      }
    });
  }

  // Rebuilds the layer from `data`. Called on initial load and again on
  // theme-refresh (colors are baked into shape styles via HashColor, not
  // CSS vars, so they need an explicit re-render).
  function render() {
    if (layer) { map.removeLayer(layer); layer = null; }
    shapesByName = {};
    if (!data || !data.regions || !data.regions.length) return;
    var theme = scopeCoverageIsDarkTheme() ? 'dark' : 'light';

    // Largest-area first so smaller/nested regions draw on top — see _hullArea.
    var regionsByZOrder = data.regions.slice().sort(function (a, b) {
      return _hullArea(b.hull) - _hullArea(a.hull);
    });

    var shapes = [];
    regionsByZOrder.forEach(function (region) {
      var hull = region.hull || [];
      var colorHex = scopeCoverageRegionNameToHex(region.name);
      var fill = window.HashColor ? HashColor.hashToHsl(colorHex, theme) : '#888';
      var outline = window.HashColor ? HashColor.hashToOutline(colorHex, theme) : '#444';
      var shape, baseStyle, hoverStyle;
      if (hull.length >= 3) {
        baseStyle = { color: outline, weight: 2, opacity: 0.8, fillColor: fill, fillOpacity: 0.15 };
        hoverStyle = { weight: 4, opacity: 1, fillOpacity: 0.4 };
        shape = L.polygon(hull, baseStyle);
      } else if (hull.length === 2) {
        baseStyle = { color: outline, weight: 3, opacity: 0.8, dashArray: '4 4' };
        hoverStyle = { weight: 5, opacity: 1 };
        shape = L.polyline(hull, baseStyle);
      } else if (hull.length === 1) {
        baseStyle = { radius: 10, color: outline, weight: 2, fillColor: fill, fillOpacity: 0.6 };
        hoverStyle = { weight: 4, fillOpacity: 0.9 };
        shape = L.circleMarker(hull[0], baseStyle);
      } else {
        return;
      }

      shape._scopeBaseStyle = baseStyle;
      shape._scopeHoverStyle = hoverStyle;

      if (hull.length >= 3) {
        // Polygons: no native hover/click bindings here at all — with
        // overlapping hulls, Leaflet would only ever reach whichever shape
        // happens to be topmost at that pixel, exactly the "can't hover
        // it, too many layers on top" problem. Real selection instead
        // happens via the map-level mousemove/click handlers below, which
        // do their own point-in-polygon test against every region, so a
        // fully-buried region is still reachable.
      } else {
        // Lines (2-point hulls) and single-point markers don't have the
        // same overlap problem in practice, so they keep simple native
        // Leaflet hover/click — routed through the same shared highlight
        // function so all hover paths agree on state.
        var label = esc(region.name) + ' <span style="opacity:0.75">(' + region.nodeCount + ' node' + (region.nodeCount === 1 ? '' : 's') + ')</span>';
        shape.bindTooltip(label, { sticky: true, direction: 'top', opacity: 0.95 });
        shape.bindPopup('<strong>' + esc(region.name) + '</strong><br>' + region.nodeCount + ' node' + (region.nodeCount === 1 ? '' : 's'));
        shape.on('mouseover', function () { _setHighlighted([region.name]); });
        shape.on('mouseout', function () { _setHighlighted([]); });
      }

      shapes.push(shape);
      shapesByName[region.name] = shape;
    });
    layer = L.layerGroup(shapes);
    var el = document.getElementById(checkboxId);
    if (el && el.checked) layer.addTo(map);
  }

  // Map-level mousemove: finds every polygon region containing the cursor
  // (not just whichever shape is visually on top) and highlights all of
  // them, with a tooltip listing every match. This is what makes a region
  // buried under several overlapping siblings still discoverable by hover.
  function onMouseMove(e) {
    if (!layer || !map.hasLayer(layer)) return;
    var matches = _polygonMatchesAt(e.latlng);
    if (!matches.length) {
      _setHighlighted([]);
      if (hoverTooltip) { map.closeTooltip(hoverTooltip); hoverTooltip = null; }
      return;
    }
    _setHighlighted(matches.map(function (r) { return r.name; }));
    var content = matches.length === 1
      ? scopeCoverageRegionSwatchHtml(matches[0].name) + esc(matches[0].name) + ' <span style="opacity:0.75">(' + matches[0].nodeCount + ')</span>'
      : '<strong>' + matches.length + ' overlapping here:</strong><br>' + matches.map(function (r) {
          return scopeCoverageRegionSwatchHtml(r.name) + esc(r.name) + ' <span style="opacity:0.75">(' + r.nodeCount + ')</span>';
        }).join('<br>');
    if (!hoverTooltip) {
      hoverTooltip = L.tooltip({ sticky: true, direction: 'top', opacity: 0.95 }).setLatLng(e.latlng).setContent(content).openOn(map);
    } else {
      hoverTooltip.setLatLng(e.latlng).setContent(content);
    }
  }

  // Map-level click: same point-in-polygon match set as hover, but opens a
  // persistent popup. A single match gets its info directly; multiple
  // matches get a clickable list so any region — no matter how deeply
  // buried — can be definitively selected and highlighted.
  function onMapClick(e) {
    if (!layer || !map.hasLayer(layer)) return;
    var matches = _polygonMatchesAt(e.latlng);
    if (!matches.length) return;
    if (matches.length === 1) {
      var r = matches[0];
      L.popup({ maxWidth: 260 }).setLatLng(e.latlng)
        .setContent(scopeCoverageRegionSwatchHtml(r.name) + '<strong>' + esc(r.name) + '</strong><br>' + r.nodeCount + ' node' + (r.nodeCount === 1 ? '' : 's'))
        .openOn(map);
      return;
    }
    var container = document.createElement('div');
    var header = document.createElement('strong');
    header.textContent = matches.length + ' overlapping regions here:';
    container.appendChild(header);
    matches.forEach(function (region) {
      var row = document.createElement('div');
      var swatch = document.createElement('span');
      swatch.style.cssText = 'display:inline-block;width:9px;height:9px;border-radius:50%;background:' +
        scopeCoverageRegionColor(region.name) + ';margin-right:5px;vertical-align:middle;';
      row.appendChild(swatch);
      row.appendChild(document.createTextNode(region.name + ' (' + region.nodeCount + ')'));
      row.style.cssText = 'cursor:pointer;padding:2px 0;';
      row.addEventListener('mouseenter', function () { _setHighlighted([region.name]); });
      row.addEventListener('click', function () {
        _setHighlighted([region.name]);
        container.querySelectorAll('div').forEach(function (r2) { r2.style.fontWeight = 'normal'; });
        row.style.fontWeight = '700';
      });
      container.appendChild(row);
    });
    L.popup({ maxWidth: 260 }).setLatLng(e.latlng).setContent(container).openOn(map);
  }

  async function load() {
    try {
      var resp = await api('/scope-coverage', { ttl: 30000 });
      if (!resp || !resp.regions || !resp.regions.length) return;
      data = resp;
      var label = document.getElementById(labelId);
      var el = document.getElementById(checkboxId);
      if (label) label.style.display = '';
      if (el) {
        var saved = localStorage.getItem(storageKey);
        if (saved === 'true') el.checked = true;
        el.addEventListener('change', function (e) {
          localStorage.setItem(storageKey, e.target.checked);
          if (!layer) return;
          if (e.target.checked) { layer.addTo(map); } else { map.removeLayer(layer); }
        });
      }
      render();
      map.on('mousemove', onMouseMove);
      map.on('click', onMapClick);
    } catch (e) { /* no hash regions configured / endpoint unavailable */ }
  }

  function refreshTheme() {
    render();
  }

  function destroy() {
    if (layer) { map.removeLayer(layer); layer = null; }
    map.off('mousemove', onMouseMove);
    map.off('click', onMapClick);
    data = null;
    shapesByName = null;
    hoverTooltip = null;
  }

  return { load: load, refreshTheme: refreshTheme, destroy: destroy };
}
