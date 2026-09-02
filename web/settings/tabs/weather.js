// Settings → Weather tab: forecast provider + location + PV arrays.
// Owns its own Leaflet loader + PV-array editor + 3D preview loader
// so the Settings shell stays weather-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  var leafletLoading = null;
  function loadLeaflet() {
    if (window.L) return Promise.resolve();
    if (leafletLoading) return leafletLoading;
    leafletLoading = new Promise(function (resolve, reject) {
      var css = document.createElement("link");
      css.rel = "stylesheet";
      css.href = "/vendor/leaflet/leaflet.css";
      document.head.appendChild(css);

      var script = document.createElement("script");
      script.src = "/vendor/leaflet/leaflet.js";
      script.async = true;
      script.onload = function () { resolve(); };
      script.onerror = function () {
        leafletLoading = null;
        reject(new Error("Leaflet failed to load"));
      };
      document.head.appendChild(script);
    });
    return leafletLoading;
  }

  var pvArraysModulePromise = null;
  var pvArraysModuleFailed = false;
  function ensurePvArraysComponent() {
    if (window.customElements.get("ftw-pv-arrays-3d")) return Promise.resolve();
    if (pvArraysModuleFailed) return Promise.reject(new Error("pv-arrays-3d unavailable"));
    if (pvArraysModulePromise) return pvArraysModulePromise;
    pvArraysModulePromise = import("/components/ftw-pv-arrays-3d.js")
      .catch(function (e) {
        pvArraysModulePromise = null;
        pvArraysModuleFailed = true;
        throw e;
      });
    return pvArraysModulePromise;
  }

  // Legacy yaml/json key `kwp` is kilowatt-peak, except values ≥ 1000
  // which were nameplate watts pasted into that field.
  function ratedWattsFromLegacyKwp(kwp) {
    var v = Number(kwp);
    if (!(v > 0)) return 0;
    return v >= 1000 ? v : v * 1000;
  }

  function migrateArrayRatedW(a) {
    if (!a) return;
    if (!(Number(a.rated_w) > 0) && Number(a.kwp) > 0) {
      a.rated_w = ratedWattsFromLegacyKwp(a.kwp);
    }
    delete a.kwp;
  }

  function renderPVArrays(ctx) {
    var host = document.getElementById("pv-arrays-list");
    if (!host) return;
    var escHtml = ctx.escHtml;
    var config = ctx.config;
    var arrays = (config.weather && config.weather.pv_arrays) || [];
    arrays.forEach(migrateArrayRatedW);
    if (arrays.length === 0) {
      host.innerHTML = '<p style="color:var(--text-dim);font-size:0.75rem;margin:4px 0 8px">No arrays defined. The model learns the production pattern from measured solar.</p>';
      return;
    }
    var previewHtml = '<div class="pv-arrays-3d-slot" ' +
      'style="margin:4px 0 10px"><ftw-pv-arrays-3d></ftw-pv-arrays-3d></div>';
    var rows = arrays.map(function (a, i) {
      return '<fieldset style="margin:6px 0;padding:8px 10px">' +
        '<div class="field-row" style="gap:8px;align-items:flex-end">' +
          '<div style="flex:1.4"><label>Name</label>' +
            '<input type="text" data-pv-arr="' + i + '" data-field="name" value="' + escHtml(a.name || "") + '" placeholder="e.g. south roof">' +
          '</div>' +
          '<div style="flex:1"><label>Rated (W)</label>' +
            '<input type="number" step="1" min="0" data-pv-arr="' + i + '" data-field="rated_w" value="' + (a.rated_w || 0) + '" placeholder="12960">' +
          '</div>' +
          '<div style="flex:1"><label>Tilt °</label>' +
            '<input type="number" step="1" min="0" max="90" data-pv-arr="' + i + '" data-field="tilt_deg" value="' + (a.tilt_deg || 0) + '">' +
          '</div>' +
          '<div style="flex:1"><label>Azimuth °</label>' +
            '<input type="number" step="1" min="0" max="360" data-pv-arr="' + i + '" data-field="azimuth_deg" value="' + (a.azimuth_deg || 0) + '">' +
          '</div>' +
          '<button class="btn-remove" data-pv-arr-remove="' + i + '" type="button" title="Remove">✕</button>' +
        '</div></fieldset>';
    });
    host.innerHTML = previewHtml + rows.join("");
    var pushArraysToPreview = function () {
      var el = host.querySelector("ftw-pv-arrays-3d");
      if (el && typeof el.setArrays === "function") {
        el.setArrays(config.weather.pv_arrays || []);
      }
    };
    ensurePvArraysComponent().then(pushArraysToPreview).catch(function () {
      var slot = host.querySelector(".pv-arrays-3d-slot");
      if (slot) slot.style.display = "none";
    });
    host.oninput = function (e) {
      var idx = e.target && e.target.dataset && e.target.dataset.pvArr;
      if (idx == null || idx === "") return;
      var fieldName = e.target.dataset.field;
      var arr = config.weather.pv_arrays;
      if (!arr[idx]) return;
      if (fieldName === "name") {
        arr[idx][fieldName] = e.target.value;
      } else {
        var v = parseFloat(e.target.value);
        if (!isNaN(v)) arr[idx][fieldName] = v;
      }
      pushArraysToPreview();
    };
    host.onclick = function (e) {
      var idx = e.target && e.target.dataset && e.target.dataset.pvArrRemove;
      if (idx == null || idx === "") return;
      config.weather.pv_arrays.splice(parseInt(idx, 10), 1);
      renderPVArrays(ctx);
      refreshArraysSummary(config);
    };
  }

  function mountMap(ctx, container) {
    if (!window.L) return;
    var bodyEl = ctx.bodyEl;
    var setByPath = ctx.setByPath;
    var latInput = bodyEl.querySelector('[data-path="weather.latitude"]');
    var lonInput = bodyEl.querySelector('[data-path="weather.longitude"]');
    if (!latInput || !lonInput) return;
    var lat = parseFloat(latInput.value);
    var lon = parseFloat(lonInput.value);
    if (isNaN(lat)) lat = 59.3293;
    if (isNaN(lon)) lon = 18.0686;
    if (window._weatherMap) { try { window._weatherMap.remove(); } catch (e) {} window._weatherMap = null; }
    var map = L.map(container, { zoomControl: true }).setView([lat, lon], 11);
    window._weatherMap = map;
    // OSM volunteer tiles 403 when the page sends no Referer. The box
    // sends Referrer-Policy: no-referrer on every response, so Leaflet
    // must opt these images back in. Origin only; the path stays private.
    L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
      maxZoom: 18,
      attribution: "© OpenStreetMap",
      referrerPolicy: "strict-origin-when-cross-origin",
    }).addTo(map);
    var marker = L.marker([lat, lon], { draggable: true }).addTo(map);
    function setCoord(la, lo) {
      latInput.value = la.toFixed(4);
      lonInput.value = lo.toFixed(4);
      setByPath(ctx.config, "weather.latitude", la);
      setByPath(ctx.config, "weather.longitude", lo);
      renderCoverage(ctx, la, lo);
    }
    marker.on("dragend", function () {
      var ll = marker.getLatLng();
      setCoord(ll.lat, ll.lng);
    });
    map.on("click", function (e) {
      marker.setLatLng(e.latlng);
      setCoord(e.latlng.lat, e.latlng.lng);
    });
    function syncFromInputs() {
      var la = parseFloat(latInput.value), lo = parseFloat(lonInput.value);
      if (!isNaN(la) && !isNaN(lo)) {
        marker.setLatLng([la, lo]);
        map.panTo([la, lo]);
        renderCoverage(ctx, la, lo);
      }
    }
    latInput.addEventListener("change", syncFromInputs);
    lonInput.addEventListener("change", syncFromInputs);
    setTimeout(function () { map.invalidateSize(); }, 150);
  }

  // Data-source coverage for the current pin. Every price provider is
  // European and until now nothing said so — a site outside them just got
  // empty results. Rendered from GET /api/data-sources, which answers for
  // a specific lat/lon.
  var coverageSeq = 0;
  function renderCoverage(ctx, lat, lon) {
    var host = document.getElementById("data-coverage");
    if (!host) return;
    // Coordinates change on marker drag; keep only the newest response so
    // a slow early request cannot overwrite a fast later one.
    var seq = ++coverageSeq;
    var q = lat != null && lon != null
      ? "?lat=" + encodeURIComponent(lat) + "&lon=" + encodeURIComponent(lon)
      : "";
    var fetchFn = ctx.apiFetch || fetch;
    fetchFn("/api/data-sources" + q)
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (seq !== coverageSeq || !d || !Array.isArray(d.sources)) return;
        host.innerHTML = coverageHTML(ctx.escHtml, d.sources);
      })
      .catch(function () { /* coverage is advisory; never break the tab */ });
  }

  var kindLabels = { forecast: "Forecast", irradiance: "Irradiance", price: "Price" };
  var kindOrder = ["forecast", "irradiance", "price"];

  function coverageHTML(esc, sources) {
    var unavailable = sources.filter(function (s) { return s.covers === false; });
    var rows = kindOrder.map(function (kind) {
      var list = sources.filter(function (s) { return s.kind === kind; });
      if (!list.length) return "";
      var names = list.map(function (s) {
        var ok = s.covers !== false;
        var mark = ok ? "✓" : "✕";
        var title = (s.area || "") + (s.note ? " — " + s.note : "");
        return '<span class="' + (ok ? "data-coverage-ok" : "data-coverage-miss") +
          '" title="' + esc(title) + '">' + mark + " " + esc(s.label) + "</span>";
      }).join('<span class="data-coverage-sep"> · </span>');
      return '<div class="data-coverage-row"><span class="data-coverage-kind">' +
        esc(kindLabels[kind] || kind) + "</span>" + names + "</div>";
    }).join("");
    var warn = "";
    if (unavailable.length) {
      var msg = unavailable.length === 1
        ? unavailable[0].label + " does not cover this location."
        : unavailable.length + " sources do not cover this location.";
      warn = '<p class="data-coverage-warn">' + esc(msg) + " Hover for details.</p>";
    }
    return '<div class="data-coverage-panel">' +
      '<div class="data-coverage-title">Data sources available here</div>' +
      rows + warn + "</div>";
  }

  function arraysSummary(n) {
    var count = Number(n) || 0;
    if (count > 0) {
      return count === 1 ? "1 array set in config" : count + " arrays set in config";
    }
    return "The solar production pattern is learned from measured solar.";
  }

  function refreshArraysSummary(config) {
    var el = document.getElementById("pv-arrays-summary");
    if (!el) return;
    var n = ((config.weather && config.weather.pv_arrays) || []).length;
    el.textContent = arraysSummary(n);
  }

  function initWeatherMap(ctx) {
    var container = document.getElementById("weather-map");
    if (!container) return;
    loadLeaflet().then(function () { mountMap(ctx, container); })
      .catch(function (e) { container.textContent = "map unavailable: " + e.message; });
  }

  S.tabs.weather = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, help = ctx.help, config = ctx.config;
      if (!config.weather) config.weather = { latitude: 59.3293, longitude: 18.0686 };
      if (!Array.isArray(config.weather.pv_arrays)) config.weather.pv_arrays = [];
      var n = config.weather.pv_arrays.length;
      return '<fieldset><legend>Weather forecast &amp; PV</legend>' +
        selectField("Provider", "weather.provider", ["met_no", "openweather", "open_meteo", "forecast_solar", "none"], "met_no",
          "met_no + openweather: cloud-cover only. open_meteo: direct shortwave radiation (better day-one forecast). forecast_solar: site-calibrated watts. The production pattern is learned from measured solar.") +
        '<div class="field-row"><div>' +
        field("Latitude", "weather.latitude", "number", 59.3293) +
        '</div><div>' +
        field("Longitude", "weather.longitude", "number", 18.0686) +
        '</div></div>' +
        '<div id="weather-map" style="height:260px;border-radius:6px;margin:6px 0;background:var(--ink-sunken)"></div>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:-2px 0 8px">Click or drag the marker to set your location.</p>' +
        '<div id="data-coverage"></div>' +
        field("PV rated (W)", "weather.pv_rated_w", "number", 10000) +
        field("API key (OpenWeather only)", "weather.api_key", "text", "") +
        '</fieldset>' +
        '<p id="pv-arrays-summary" style="color:var(--text-dim);font-size:0.8rem;margin:8px 0">' +
        arraysSummary(n) + '</p>' +
        '<details class="engine-details" id="pv-arrays-advanced">' +
        '<summary>Advanced array geometry — leave this unless you are debugging.</summary>' +
        '<fieldset><legend>PV arrays ' + help(
          'Optional. Open-Meteo uses these per-plane values to project shortwave radiation onto each array. ' +
          'Forecast.Solar uses them for its site-calibrated forecast. Leave empty unless you are debugging a multi-plane site.') + '</legend>' +
        '<div id="pv-arrays-list"></div>' +
        '<button class="btn-add" id="pv-array-add" type="button">+ Add array</button>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">' +
        'Tilt: 0° = flat roof, 35° = typical pitched roof, 90° = wall. Azimuth: 0 = N, 90 = E, 180 = S, 270 = W. ' +
        'Rated (W) is watts, same unit as PV rated.' +
        '</p>' +
        '</fieldset></details>';
    },
    after: function (ctx) {
      initWeatherMap(ctx);
      renderPVArrays(ctx);
      refreshArraysSummary(ctx.config);
      // The map may fail (no WebGL); coverage must still render, so drive
      // it from the numeric fields rather than from the map.
      var w = (ctx.config && ctx.config.weather) || {};
      renderCoverage(ctx, w.latitude, w.longitude);
      var addBtn = document.getElementById("pv-array-add");
      if (addBtn) addBtn.addEventListener("click", function () {
        ctx.config.weather.pv_arrays.push({ name: "", rated_w: 0, tilt_deg: 35, azimuth_deg: 180 });
        renderPVArrays(ctx);
        refreshArraysSummary(ctx.config);
      });
    },
  };

  S.tabs.weather._pure = { arraysSummary: arraysSummary, coverageHTML: coverageHTML };
})();
