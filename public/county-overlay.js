// Tennessee county boundary reference overlay — shared between map.js,
// live.js, and regions.js. Outline-only styling (no visible fill) so it
// reads as reference geography, not another data layer competing with the
// hash-region coverage overlay's colors — but each county IS hoverable
// (a near-invisible fill gives the whole polygon area a hit region, not
// just its 1px stroke) so a name tooltip can surface on demand instead of
// permanently labeling all 95 counties, which is unreadable clutter at
// most zoom levels. This coexists fine with scope-coverage.js's own
// hover/click: that overlay listens on the MAP itself (not per-shape), so
// it keeps firing regardless of what a county polygon's own hover does.
//
// Drawn in a dedicated pane (z-index 450) above Leaflet's default
// overlayPane (400), where scope-coverage's region hulls live — so county
// lines always stay visible on top of them. This can't be done by simply
// adding the county layer after the coverage layer: scope-coverage.js
// rebuilds and re-adds its layer (map.removeLayer + addTo) on activation,
// theme refresh, and every region-filter change, which would otherwise
// bump it back above the counties each time.
//
// Usage (per Leaflet map instance):
//   const counties = createCountyOverlay(map);
//   await counties.load();   // fetches once, builds the layer, adds it to the map
//   counties.hide();         // removes from map but keeps the built layer (cheap re-show)
//   counties.load();         // re-add if previously hidden — no re-fetch
//   counties.destroy();      // call when leaving the page
function createCountyOverlay(map) {
  var layer = null;
  var loading = null; // in-flight fetch promise, shared across overlapping load() calls

  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  }

  async function load() {
    if (layer) {
      if (!map.hasLayer(layer)) layer.addTo(map);
      return;
    }
    if (!loading) {
      loading = fetch('/geo/tn-counties.geojson').then(function (r) {
        if (!r.ok) throw new Error('tn-counties.geojson: HTTP ' + r.status);
        return r.json();
      });
    }
    var geojson;
    try {
      geojson = await loading;
    } catch (e) {
      loading = null; // allow a retry on next load() rather than caching a failed fetch forever
      console.error('[county-overlay] failed to load tn-counties.geojson:', e);
      return;
    }
    if (layer) { // another load() call already won the race and built it
      if (!map.hasLayer(layer)) layer.addTo(map);
      return;
    }
    if (!map.getPane('countyOverlayPane')) {
      map.createPane('countyOverlayPane');
      map.getPane('countyOverlayPane').style.zIndex = 450;
    }
    var color = cssVar('--text-muted', '#888');
    layer = L.geoJSON(geojson, {
      pane: 'countyOverlayPane',
      interactive: true,
      style: { color: color, weight: 1, opacity: 0.45, fill: true, fillOpacity: 0.02 },
      onEachFeature: function (feature, shape) {
        var name = feature && feature.properties && (feature.properties.NAMELSAD || feature.properties.NAME);
        if (name) shape.bindTooltip(name, { direction: 'center', className: 'county-tooltip', opacity: 1, sticky: false });
      }
    }).addTo(map);
  }

  function hide() {
    if (layer && map.hasLayer(layer)) map.removeLayer(layer);
  }

  function destroy() {
    if (layer) { map.removeLayer(layer); layer = null; }
    loading = null;
  }

  return { load: load, hide: hide, destroy: destroy };
}
