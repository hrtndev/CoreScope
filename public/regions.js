// Regions page (route #/regions) — a focused variant of the Map page:
// the hash-region coverage overlay is always on (no toggle, unlike
// map.js/live.js's opt-in checkbox), and only nodes that have actually
// relayed region-scoped traffic are shown, colored/labeled by which
// region(s) they belong to. Everything else the Map page renders
// (clustering, heatmap, byte-size filters, path inspector, ...) is
// deliberately absent — this page answers one question ("who's carrying
// region traffic, and where") rather than being a general-purpose map.
'use strict';
(function () {
  var map = null;
  var scopeCoverageOverlay = null;
  var countyOverlay = null;
  var nodeLayer = null;
  var destroyed = false;
  var allRegions = []; // last-fetched region list (name/nodeCount), for the legend
  var activeRegionFilter = null; // region name to isolate, or null = show all

  var esc = (typeof escapeHtml === 'function') ? escapeHtml : function (s) { return String(s == null ? '' : s); };

  function pageHtml() {
    return '<div style="position:relative;width:100%;height:100%;">' +
      '<div id="regionsMap" style="width:100%;height:100%;"></div>' +
      // Hidden checkbox+label pair — createScopeCoverageOverlay expects
      // real elements with these ids to wire visibility/localStorage
      // against, but this page has no settings panel of its own and
      // always wants the overlay on, so it's pre-checked and never shown.
      '<div style="display:none">' +
        '<label id="regionsScopeCoverageLabel"><input type="checkbox" id="regionsScopeCoverageToggle" checked></label>' +
      '</div>' +
      '<div class="regions-legend" id="regionsLegend" role="region" aria-label="Regions legend">' +
        '<div class="regions-legend-header">' +
          '<span>Regions</span>' +
          '<button type="button" id="regionsLegendReset" class="regions-legend-reset" style="display:none">Show all</button>' +
        '</div>' +
        '<div class="regions-legend-list" id="regionsLegendList"></div>' +
      '</div>' +
    '</div>';
  }

  // One legend row per region — swatch, name, node count. Clickable (and
  // keyboard-activatable) to isolate that region's coverage shape + node
  // markers; the active row gets a distinct style so it's clear which
  // filter (if any) is applied.
  function legendRowHtml(region, isActive) {
    return '<div class="regions-legend-row' + (isActive ? ' active' : '') + '" data-region="' + esc(region.name) + '" role="button" tabindex="0" aria-pressed="' + (isActive ? 'true' : 'false') + '">' +
      scopeCoverageRegionSwatchHtml(region.name) +
      '<span class="regions-legend-name">' + esc(region.name) + '</span>' +
      '<span class="regions-legend-count">' + region.nodeCount + '</span>' +
    '</div>';
  }

  function renderLegend() {
    var listEl = document.getElementById('regionsLegendList');
    var resetBtn = document.getElementById('regionsLegendReset');
    if (!listEl) return;
    if (!allRegions.length) {
      listEl.innerHTML = '<div class="regions-legend-empty">No regions configured</div>';
      if (resetBtn) resetBtn.style.display = 'none';
      return;
    }
    listEl.innerHTML = allRegions.slice()
      .sort(function (a, b) { return a.name.localeCompare(b.name); })
      .map(function (r) { return legendRowHtml(r, r.name === activeRegionFilter); })
      .join('');
    if (resetBtn) resetBtn.style.display = activeRegionFilter ? '' : 'none';
  }

  // Toggling the same region off (click again) clears the filter — matches
  // how the map-page's own checkboxes behave (click to turn on, click to
  // turn off), rather than requiring a trip to the separate reset button.
  function setActiveRegion(name) {
    activeRegionFilter = (name && name !== activeRegionFilter) ? name : null;
    if (scopeCoverageOverlay) {
      if (activeRegionFilter) scopeCoverageOverlay.setRegionFilter(activeRegionFilter);
      else scopeCoverageOverlay.clearRegionFilter();
    }
    renderLegend();
    loadNodes(); // re-filter markers + refit bounds to just the filtered set
  }

  function wireLegend() {
    var listEl = document.getElementById('regionsLegendList');
    var resetBtn = document.getElementById('regionsLegendReset');
    if (listEl) {
      listEl.addEventListener('click', function (e) {
        var row = e.target.closest('.regions-legend-row');
        if (row) setActiveRegion(row.dataset.region);
      });
      listEl.addEventListener('keydown', function (e) {
        if (e.key !== 'Enter' && e.key !== ' ') return;
        var row = e.target.closest('.regions-legend-row');
        if (!row) return;
        e.preventDefault();
        setActiveRegion(row.dataset.region);
      });
    }
    if (resetBtn) resetBtn.addEventListener('click', function () { setActiveRegion(null); });
  }

  // Marker content lists every region a node belongs to (not just one) —
  // the whole point of this page over the Map page's checkbox is that a
  // boundary node's full membership is visible at a glance, not collapsed
  // to a single color.
  function nodePopupHtml(node, regions) {
    var name = esc(node.name || node.public_key.slice(0, 12));
    var regionRows = regions.map(function (r) {
      return scopeCoverageRegionSwatchHtml(r) + esc(r);
    }).join('<br>');
    return '<strong>' + name + '</strong>' + (node.role ? ' <span style="opacity:0.75">(' + esc(node.role) + ')</span>' : '') +
      '<br>' + regionRows;
  }

  async function loadNodes() {
    if (!nodeLayer) return;
    nodeLayer.clearLayers();
    try {
      var nodesData = await fetchAllNodes('', { ttl: 30000 });
      var membership = await api('/scope-coverage/nodes', { ttl: 30000 });
      if (destroyed) return;

      var regionsByPubkey = {};
      (membership.nodes || []).forEach(function (n) {
        regionsByPubkey[n.pubkey.toLowerCase()] = n.regions;
      });

      var byPubkey = {};
      (nodesData.nodes || []).forEach(function (n) { byPubkey[n.public_key.toLowerCase()] = n; });

      var bounds = [];
      Object.keys(regionsByPubkey).forEach(function (pk) {
        var node = byPubkey[pk];
        if (!node || node.lat == null || node.lon == null) return; // no GPS fix — can't be plotted
        var regions = regionsByPubkey[pk];
        // Boundary nodes belong to more than one region — keep them when
        // ANY of their regions matches the active filter, not just their
        // primary (first-alphabetically) one.
        if (activeRegionFilter && regions.indexOf(activeRegionFilter) === -1) return;
        // Primary color = the active filter (if any) so an isolated
        // region's markers always match its own legend swatch, even for
        // boundary nodes whose alphabetically-first membership is a
        // DIFFERENT region than the one just selected. Unfiltered, falls
        // back to first region alphabetically (server already sorts); the
        // marker can only show one fill color, but hover/click always
        // reveals the complete membership list.
        var color = scopeCoverageRegionColor(activeRegionFilter || regions[0]);
        var marker = L.circleMarker([node.lat, node.lon], {
          pane: 'regionsNodesPane',
          radius: 7, weight: 2, color: '#222', opacity: 0.8,
          fillColor: color, fillOpacity: 0.9
        }).addTo(nodeLayer);
        var content = nodePopupHtml(node, regions);
        marker.bindTooltip(content, { sticky: true, direction: 'top', opacity: 0.95 });
        marker.bindPopup(content, { maxWidth: 260 });
        // The coverage overlay listens for 'mousemove' AND 'click' on the
        // MAP itself (see scope-coverage.js) so it can hit-test polygons
        // regardless of DOM z-order — but that means both fire even when
        // the cursor/click is on a marker sitting inside a region's fill,
        // showing the overlay's own region-list tooltip/popup on top of
        // (click: instead of) the marker's. Stop native event bubbling to
        // the map container while directly over a marker so only the
        // marker's own tooltip/popup shows.
        marker.on('mousemove', L.DomEvent.stopPropagation);
        marker.on('click', L.DomEvent.stopPropagation);
        bounds.push([node.lat, node.lon]);
      });

      if (bounds.length) {
        try { map.fitBounds(bounds, { padding: [40, 40], maxZoom: 10 }); } catch (e) { /* ignore */ }
      }
    } catch (e) {
      console.error('[regions] failed to load node region membership:', e);
    }
  }

  async function init(container) {
    destroyed = false;
    container.innerHTML = pageHtml();

    map = L.map('regionsMap', { zoomControl: false, attributionControl: false }).setView([37.0, -95.0], 4);
    if (typeof window._applyTilesToNodeMap === 'function') {
      window._applyTilesToNodeMap(map);
    } else {
      console.warn('[regions] _applyTilesToNodeMap unavailable — using OSM fallback');
      L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png', { maxZoom: 19 }).addTo(map);
    }
    L.control.zoom({ position: 'topright' }).addTo(map);

    // Node markers must sit in their own pane above the coverage overlay's
    // polygons (default overlayPane, z-index 400) — otherwise a marker
    // sitting inside a region's fill is the exact "buried under an
    // overlapping shape, can't hover/click it" problem scope-coverage.js's
    // own hover/click handlers exist to solve for the polygons themselves.
    // Pane-based (not add-order-based) so it holds regardless of whether
    // the coverage overlay's async load() resolves before or after nodes.
    map.createPane('regionsNodesPane');
    map.getPane('regionsNodesPane').style.zIndex = 450;

    nodeLayer = L.layerGroup().addTo(map);

    scopeCoverageOverlay = createScopeCoverageOverlay(map, {
      checkboxId: 'regionsScopeCoverageToggle', labelId: 'regionsScopeCoverageLabel',
      storageKey: 'meshcore-regions-scope-coverage'
    });
    // Awaited (unlike map.js/live.js's fire-and-forget load()) — the legend
    // needs the resolved region list before it can render anything.
    await scopeCoverageOverlay.load();
    allRegions = scopeCoverageOverlay.getRegions();
    activeRegionFilter = null;
    renderLegend();
    wireLegend();

    // Tennessee county boundary overlay — always on for this page (no
    // toggle, unlike map.js/live.js's opt-in checkbox), since the whole
    // point here is seeing region coverage against familiar geography.
    countyOverlay = createCountyOverlay(map);
    countyOverlay.load();

    await loadNodes();

    setTimeout(function () { if (!destroyed && map) map.invalidateSize(); }, 120);
  }

  function destroy() {
    destroyed = true;
    if (scopeCoverageOverlay) { scopeCoverageOverlay.destroy(); scopeCoverageOverlay = null; }
    if (countyOverlay) { countyOverlay.destroy(); countyOverlay = null; }
    if (map) { try { map.remove(); } catch (e) { /* ignore */ } map = null; }
    nodeLayer = null;
    allRegions = [];
    activeRegionFilter = null;
  }

  var _themeRefreshHandler = null;

  registerPage('regions', {
    init: function (app, routeParam) {
      _themeRefreshHandler = function () {
        if (scopeCoverageOverlay) scopeCoverageOverlay.refreshTheme();
        renderLegend(); // swatches are baked in via HashColor too
        loadNodes(); // marker fill colors are baked in via HashColor, same reason as the overlay
      };
      window.addEventListener('theme-refresh', _themeRefreshHandler);
      return init(app, routeParam);
    },
    destroy: function () {
      if (_themeRefreshHandler) { window.removeEventListener('theme-refresh', _themeRefreshHandler); _themeRefreshHandler = null; }
      return destroy();
    }
  });
})();
