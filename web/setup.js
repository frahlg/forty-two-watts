// setup.js — multi-step setup wizard for forty-two-watts
// Single-page state machine: show/hide step divs, collect config, POST to /api/config.

(function () {
  'use strict';

  var TOTAL_STEPS = 8;
  var currentStep = 1;

  // Collected state
  var configuredDrivers = [];    // array of driver objects ready for config.drivers
  var selectedDevice = null;     // { ip, port, protocol } from scan or manual entry
  var selectedCatalog = null;    // CatalogEntry from /api/drivers/catalog
  var driverCatalog = [];        // full catalog cache

  // --- Step navigation ---

  function renderDots() {
    var container = document.getElementById('step-dots');
    container.innerHTML = '';
    for (var i = 1; i <= TOTAL_STEPS; i++) {
      var dot = document.createElement('div');
      dot.className = 'step-dot';
      if (i === currentStep) dot.className += ' active';
      else if (i < currentStep) dot.className += ' done';
      container.appendChild(dot);
    }
  }

  window.goStep = function (n) {
    if (n < 1 || n > TOTAL_STEPS) return;

    // Pre-step hooks
    if (n === 4) loadCatalog();
    if (n === 6) renderDriversSummary();
    if (n === 7) prepareIntegrations();
    if (n === 8) renderReview();

    currentStep = n;
    var steps = document.querySelectorAll('.step');
    for (var i = 0; i < steps.length; i++) {
      steps[i].classList.remove('visible');
    }
    document.getElementById('step-' + n).classList.add('visible');
    renderDots();
    window.scrollTo(0, 0);
  };

  // Back from step 7 goes to step 6 if we have drivers, step 2 if we skipped
  window.goStepBack7 = function () {
    goStep(configuredDrivers.length > 0 ? 6 : 2);
  };

  // --- Step 3: Scan ---

  window.startScan = function () {
    var statusEl = document.getElementById('scan-status');
    var resultsEl = document.getElementById('scan-results');
    statusEl.style.display = 'block';
    statusEl.innerHTML = '<span class="spinner"></span> Scanning network...';
    resultsEl.style.display = 'none';

    fetch('/api/scan')
      .then(function (r) { return r.json(); })
      .then(function (data) {
        // API returns a raw array, not {devices: [...]}
        var devices = Array.isArray(data) ? data : (data.devices || []);
        if (devices.length === 0) {
          statusEl.innerHTML = 'No devices found. Try entering the IP manually.';
          return;
        }
        statusEl.style.display = 'none';
        var tbody = document.getElementById('scan-tbody');
        tbody.innerHTML = '';
        devices.forEach(function (d) {
          var tr = document.createElement('tr');
          var proto = d.protocol || 'modbus';
          tr.innerHTML =
            '<td>' + esc(d.ip) + '</td>' +
            '<td>' + esc(String(d.port)) + '</td>' +
            '<td>' + esc(proto) + '</td>' +
            '<td><button class="btn-use">Use this device</button></td>';
          tr.querySelector('.btn-use').addEventListener('click', function () {
            useScanDevice(d.ip, d.port, proto);
          });
          tbody.appendChild(tr);
        });
        resultsEl.style.display = 'block';
      })
      .catch(function (err) {
        statusEl.innerHTML = 'Scan failed: ' + esc(err.message) +
          '. Try entering the IP manually.';
      });
  };

  function useScanDevice(ip, port, protocol) {
    selectedDevice = { ip: ip, port: port, protocol: protocol };
    goStep(4);
  }

  // "Add device manually" — skip the scan, go straight to driver
  // selection. No device pre-selected, so step 4 shows the full driver
  // catalog (no protocol filter) and step 5 renders connection fields
  // from the chosen driver's `connection_defaults`.
  window.addDeviceManually = function () {
    selectedDevice = null;
    goStep(4);
  };

  // --- Step 4: Driver catalog ---

  function loadCatalog() {
    if (driverCatalog.length > 0) {
      populateDriverDropdown();
      return;
    }
    fetch('/api/drivers/catalog')
      .then(function (r) { return r.json(); })
      .then(function (data) {
        driverCatalog = data.entries || [];
        populateDriverDropdown();
      })
      .catch(function () {
        driverCatalog = [];
        populateDriverDropdown();
      });
  }

  function driverLabel(entry) {
    // Prepend the manufacturer only when the driver's own name doesn't
    // already start with it — avoids "CTEK CTEK Chargestorm (…)" when
    // the driver metadata already sets the manufacturer into the name.
    var base = entry.name || entry.filename || '';
    var mfr  = entry.manufacturer || '';
    if (mfr && base.toLowerCase().indexOf(mfr.toLowerCase()) !== 0) {
      base = mfr + ' ' + base;
    }
    if (entry.protocols && entry.protocols.length > 0) {
      base += ' (' + entry.protocols.join(', ');
      if (entry.capabilities && entry.capabilities.length > 0) {
        base += ', ' + entry.capabilities.join('+');
      }
      base += ')';
    }
    return base;
  }

  function populateDriverDropdown() {
    var sel = document.getElementById('driver-select');
    sel.innerHTML = '<option value="">-- Select a driver --</option>';

    // Protocol filter only applies when the user picked a device from a
    // scan (we know its protocol). Manual entry shows the full catalog.
    var proto = selectedDevice ? selectedDevice.protocol : null;

    // Decorate each entry with its rendered label + original index so
    // the sort-by-label stays stable and we can still recover the
    // catalog index when the user picks one.
    var rows = [];
    driverCatalog.forEach(function (entry, idx) {
      if (proto && entry.protocols && entry.protocols.length > 0) {
        var match = entry.protocols.some(function (p) {
          return p.toLowerCase() === proto.toLowerCase();
        });
        if (!match) return;
      }
      rows.push({ idx: idx, label: driverLabel(entry) });
    });
    rows.sort(function (a, b) {
      return a.label.localeCompare(b.label, undefined, { sensitivity: 'base' });
    });

    rows.forEach(function (r) {
      var opt = document.createElement('option');
      opt.value = String(r.idx);
      opt.textContent = r.label;
      sel.appendChild(opt);
    });
  }

  window.onDriverSelected = function () {
    var sel = document.getElementById('driver-select');
    var btn = document.getElementById('driver-next-btn');
    var descEl = document.getElementById('driver-description');

    if (!sel.value) {
      selectedCatalog = null;
      btn.disabled = true;
      descEl.style.display = 'none';
      return;
    }

    selectedCatalog = driverCatalog[parseInt(sel.value, 10)];
    btn.disabled = false;

    if (selectedCatalog.description) {
      descEl.textContent = selectedCatalog.description;
      descEl.style.display = 'block';
    } else {
      descEl.style.display = 'none';
    }

    // Pre-fill step 5
    prefillDriverConfig();
  };

  // --- Step 5: Configure driver ---

  // Describe which connection + config fields to render per driver. The
  // primary lookup is by the driver's declared `protocols`; a small
  // registry of driver-specific extras (CTEK MQTT's base_topic / cbid
  // etc.) rides on top, keyed by DRIVER.id. Everything here is a plain
  // description list so the renderer stays agnostic of field IDs.
  //
  // Field shape:
  //   { key: "host", label: "IP address", type: "text",
  //     placeholder: "...", default: <from connection_defaults> }
  //
  // `target` is where the value lands:
  //   "capabilities.<proto>.<key>"  → config.drivers[].capabilities.<proto>.<key>
  //   "config.<key>"                → config.drivers[].config.<key>
  // Default port per protocol — used as the form-field default when the
  // driver's connection_defaults block doesn't specify one. Only the
  // primary protocol's port comes from connection_defaults.port (which
  // is a single scalar, by design — drivers with multiple protocols
  // can't disambiguate which one the default belongs to).
  var PROTO_DEFAULT_PORT = { modbus: 502, mqtt: 1883, http: 80 };

  function fieldsForProtocol(proto, entry, isPrimary, multi) {
    var cd     = entry.connection_defaults || {};
    var prefix = multi ? (proto.toUpperCase() + ' ') : '';
    var port   = isPrimary && cd.port != null ? cd.port : PROTO_DEFAULT_PORT[proto];
    // When a driver speaks more than one protocol the secondary
    // endpoints are optional — operator might wire only Modbus and let
    // MQTT stay dark. Required validation is only enforced on the
    // primary protocol's host.
    var hostRequired = isPrimary;
    var fields = [];

    if (proto === 'modbus') {
      fields.push({ target: 'capabilities.modbus.host',    key: 'modbus_host',
                    label: prefix + 'IP address', type: 'text',
                    placeholder: '192.168.1.10', required: hostRequired });
      fields.push({ target: 'capabilities.modbus.port',    key: 'modbus_port',
                    label: prefix + 'Port', type: 'number', default: port });
      fields.push({ target: 'capabilities.modbus.unit_id', key: 'modbus_unit_id',
                    label: prefix + 'Unit ID', type: 'number',
                    default: cd.unit_id != null ? cd.unit_id : 1 });
    } else if (proto === 'mqtt') {
      fields.push({ target: 'capabilities.mqtt.host',     key: 'mqtt_host',
                    label: prefix + (multi ? 'broker IP' : 'Broker IP'), type: 'text',
                    placeholder: '192.168.1.10', required: hostRequired });
      fields.push({ target: 'capabilities.mqtt.port',     key: 'mqtt_port',
                    label: prefix + 'Port', type: 'number', default: port });
      fields.push({ target: 'capabilities.mqtt.username', key: 'mqtt_username',
                    label: prefix + 'Username', type: 'text', default: cd.username || '' });
      fields.push({ target: 'capabilities.mqtt.password', key: 'mqtt_password',
                    label: prefix + 'Password', type: 'password', default: cd.password || '' });
    } else if (proto === 'http') {
      // Local HTTP driver (e.g. a REST-speaking inverter on the LAN) —
      // CD advertises a host. Cloud drivers (Easee) declare http_hosts
      // but no CD.host, and their creds live in config.* instead; we
      // punt those to the integrations step (step 7) via cloudHint.
      if (cd.host !== undefined || (!entry.http_hosts || entry.http_hosts.length === 0)) {
        fields.push({ target: 'capabilities.http.host', key: 'http_host',
                      label: prefix + 'IP address', type: 'text',
                      placeholder: '192.168.1.10', required: hostRequired });
        fields.push({ target: 'capabilities.http.port', key: 'http_port',
                      label: prefix + 'Port', type: 'number', default: port });
      } else if (isPrimary) {
        fields.push({ cloudHint: true,
                      message: 'This driver is a cloud API. Configure its credentials in the "Optional integrations" step (step 7).' });
      }
    }
    return fields;
  }

  // Iterate every protocol the driver declares, not just the first —
  // hybrid drivers (e.g. CTEK Chargestorm with both Modbus control and
  // MQTT telemetry on the same box) need both endpoints rendered so the
  // operator can point each at its actual port. The driver's metadata
  // (`protocols = { ... }`) is the single source of truth for what's
  // shown.
  function connectionFieldsFor(entry) {
    if (!entry || !entry.protocols || entry.protocols.length === 0) return [];
    var protos = entry.protocols;
    var multi  = protos.length > 1;
    var fields = [];
    protos.forEach(function (p, idx) {
      fields = fields.concat(fieldsForProtocol(p, entry, idx === 0, multi));
    });
    return fields;
  }

  // Driver-specific config knobs rendered alongside the connection
  // fields. Only the handful we actually need for a first-run bootstrap
  // — everything else can be tweaked from /settings later.
  function extraFieldsFor(entry) {
    if (!entry) return [];
    var out = [];
    if (entry.id === 'ctek-chargestorm' || entry.id === 'ctek-chargestorm-v2') {
      out.push({ target: 'config.outlet', key: 'outlet', label: 'Outlet (passed via unit_id)', type: 'number',
                 default: 1, disabled: true, hint: 'unit_id above controls the outlet.' });
    }
    return out;
  }

  function renderFields(containerId, fields) {
    var container = document.getElementById(containerId);
    container.innerHTML = '';
    if (fields.length === 0) return;

    // Group numeric host/port fields onto a single row for a tidier
    // layout. Everything else goes on its own row.
    var i = 0;
    while (i < fields.length) {
      var f = fields[i];

      if (f.cloudHint) {
        var hint = document.createElement('div');
        hint.className = 'cloud-hint';
        hint.textContent = f.message;
        container.appendChild(hint);
        i++;
        continue;
      }

      var next = fields[i + 1];
      // Pair host+port (or port+unit_id) onto a single row when both are
      // for the same protocol. Match by target suffix so the namespaced
      // keys (`modbus_host`, `mqtt_host`, …) used by hybrid drivers
      // still pair correctly.
      function suffix(t) {
        if (!t) return '';
        var dot = t.lastIndexOf('.');
        return dot < 0 ? t : t.substr(dot + 1);
      }
      var fSuf = suffix(f.target), nSuf = next && suffix(next.target);
      var pairable = next && !next.cloudHint &&
                     ((fSuf === 'host' && (nSuf === 'port' || nSuf === 'unit_id')) ||
                      (fSuf === 'port' && nSuf === 'unit_id'));
      if (pairable) {
        var row = document.createElement('div');
        row.className = 'form-row';
        row.appendChild(makeFieldGroup(f));
        row.appendChild(makeFieldGroup(next));
        container.appendChild(row);
        i += 2;
      } else {
        container.appendChild(makeFieldGroup(f));
        i++;
      }
    }
  }

  function makeFieldGroup(f) {
    var group = document.createElement('div');
    group.className = 'form-group';
    var label = document.createElement('label');
    // Build the input id from the target path so keys like `modbus_host`
    // and `mqtt_host` (both rendered for a hybrid driver) get distinct
    // DOM ids and the <label for=…> still binds to the right input.
    var inputId = 'drv-f-' + (f.target || f.key).replace(/\./g, '-');
    label.setAttribute('for', inputId);
    label.textContent = f.label;
    group.appendChild(label);

    var input = document.createElement('input');
    input.id = inputId;
    input.type = f.type || 'text';
    if (f.default !== undefined && f.default !== null) input.value = String(f.default);
    if (f.placeholder) input.placeholder = f.placeholder;
    if (f.disabled)    input.disabled = true;
    input.dataset.target = f.target;
    input.dataset.key    = f.key;
    if (f.required) input.dataset.required = '1';
    group.appendChild(input);

    if (f.hint) {
      var small = document.createElement('small');
      small.textContent = f.hint;
      group.appendChild(small);
    }
    return group;
  }

  function readFieldInto(driver, input) {
    var target = input.dataset.target;
    if (!target) return;
    var raw = input.value;
    if (input.type === 'number') {
      raw = raw.trim();
      if (raw === '') return;
      raw = parseFloat(raw);
      if (isNaN(raw)) return;
      if (Number.isInteger(raw)) raw = parseInt(raw, 10);
    } else {
      raw = raw.trim();
      if (raw === '') return;
    }
    // target looks like "capabilities.modbus.host" or "config.base_topic"
    var parts = target.split('.');
    var cur = driver;
    for (var i = 0; i < parts.length - 1; i++) {
      var p = parts[i];
      if (cur[p] == null) cur[p] = {};
      cur = cur[p];
    }
    cur[parts[parts.length - 1]] = raw;
  }

  function prefillDriverConfig() {
    if (!selectedCatalog) return;

    var nameBase = (selectedCatalog.manufacturer || selectedCatalog.id || 'device').toLowerCase()
      .replace(/[^a-z0-9]/g, '_').replace(/_+/g, '_').replace(/^_|_$/g, '');
    var name = nameBase;
    var n = 2;
    while (configuredDrivers.some(function (d) { return d.name === name; })) {
      name = nameBase + '_' + n;
      n++;
    }
    document.getElementById('drv-name').value = name;

    // Render connection fields from the driver's catalog entry.
    var connFields = connectionFieldsFor(selectedCatalog);
    // Pre-fill host/port from a scanned device if the user got here via
    // the scan path, so they don't have to retype the IP.
    if (selectedDevice) {
      connFields.forEach(function (f) {
        if (f.key === 'host' && selectedDevice.ip)   f.default = selectedDevice.ip;
        if (f.key === 'port' && selectedDevice.port) f.default = selectedDevice.port;
      });
    }
    renderFields('drv-connection-fields', connFields);
    renderFields('drv-extra-fields',      extraFieldsFor(selectedCatalog));

    var hasBattery = selectedCatalog.capabilities &&
      selectedCatalog.capabilities.some(function (c) { return c === 'battery'; });
    document.getElementById('drv-battery-group').style.display = hasBattery ? 'block' : 'none';

    // First driver defaults to site meter. EV-only drivers never are.
    var isEVOnly = selectedCatalog.capabilities &&
      selectedCatalog.capabilities.length === 1 &&
      selectedCatalog.capabilities[0] === 'ev';
    document.getElementById('drv-site-meter').checked =
      !isEVOnly && configuredDrivers.length === 0;
  }

  window.saveDriver = function () {
    var name = document.getElementById('drv-name').value.trim();
    if (!name) { alert('Driver name is required.'); return; }

    // Validate required fields on the dynamic form.
    var inputs = document.querySelectorAll('#drv-connection-fields [data-target], #drv-extra-fields [data-target]');
    for (var i = 0; i < inputs.length; i++) {
      var inp = inputs[i];
      if (inp.dataset.required === '1' && !inp.value.trim()) {
        alert('Missing: ' + inp.previousSibling.textContent);
        inp.focus();
        return;
      }
    }

    var isSiteMeter = document.getElementById('drv-site-meter').checked;
    var batteryKwh  = parseFloat(document.getElementById('drv-battery-kwh').value) || 0;

    var driver = {
      name: name,
      lua: selectedCatalog ? selectedCatalog.path : '',
      is_site_meter: isSiteMeter,
      capabilities: {}
    };
    if (batteryKwh > 0) driver.battery_capacity_wh = batteryKwh * 1000;

    // Walk every rendered input and set its target path on the driver
    // object. This replaces the older protocol-switch hardcode so a new
    // driver type only needs a connectionFieldsFor() entry.
    for (var j = 0; j < inputs.length; j++) {
      readFieldInto(driver, inputs[j]);
    }

    if (isSiteMeter) {
      configuredDrivers.forEach(function (d) { d.is_site_meter = false; });
    }
    driver._catalog = selectedCatalog;

    configuredDrivers.push(driver);
    selectedDevice = null;
    selectedCatalog = null;
    goStep(6);
  };

  // --- Driver description helper ---

  function driverDetail(d) {
    var detail = '';
    if (d.capabilities.modbus) {
      detail = d.capabilities.modbus.host + ':' + d.capabilities.modbus.port;
    } else if (d.capabilities.mqtt) {
      detail = d.capabilities.mqtt.host + ':' + d.capabilities.mqtt.port;
    } else if (d.capabilities.http) {
      detail = (d.config && d.config.host) || (d.capabilities.http.allowed_hosts || [])[0] || '';
    }
    var tags = [];
    if (d.is_site_meter) tags.push('site meter');
    if (d.battery_capacity_wh) tags.push((d.battery_capacity_wh / 1000).toFixed(1) + ' kWh battery');
    if (tags.length > 0) detail += ' (' + tags.join(', ') + ')';
    return detail;
  }

  // --- Step 6: Drivers summary ---

  function renderDriversSummary() {
    var container = document.getElementById('drivers-summary');
    container.innerHTML = '';

    if (configuredDrivers.length === 0) {
      container.innerHTML = '<p style="color:var(--text-dim);font-size:0.85rem;">No devices configured yet.</p>';
      return;
    }

    configuredDrivers.forEach(function (d, idx) {
      var item = document.createElement('div');
      item.className = 'driver-summary-item';
      item.innerHTML =
        '<div class="driver-info">' +
          '<span class="driver-label">' + esc(d.name) + '</span>' +
          '<span class="driver-detail">' + esc(driverDetail(d)) + '</span>' +
        '</div>';
      var btn = document.createElement('button');
      btn.className = 'btn-remove-sm';
      btn.textContent = 'Remove';
      btn.addEventListener('click', function () { removeDriver(idx); });
      item.appendChild(btn);
      container.appendChild(item);
    });
  }

  function removeDriver(idx) {
    configuredDrivers.splice(idx, 1);
    renderDriversSummary();
  }

  window.addAnotherDevice = function () {
    selectedDevice = null;
    selectedCatalog = null;
    goStep(3);
  };

  // --- Step 7: Integrations ---

  var integrationListenersBound = false;

  // Extra config fields to render for EV drivers that don't fit the
  // generic connectionFieldsFor() shape. Keyed by DRIVER.id so a new EV
  // driver can declare its own credentials/knobs without touching the
  // step 7 renderer. Targets starting with "ev_charger." mean "save as
  // the top-level cfg.ev_charger block" (preserves the backend's
  // password-persistence + masking behaviour for cloud providers);
  // everything else lands as a regular drivers[] entry.
  var EV_DRIVER_FIELDS = {
    'easee-cloud': [
      { target: 'ev_charger.email',    key: 'email',    label: 'Email',
        type: 'text',     placeholder: 'user@example.com', required: true },
      { target: 'ev_charger.password', key: 'password', label: 'Password',
        type: 'password', required: true },
      { target: 'ev_charger.serial',   key: 'serial',   label: 'Serial (optional)',
        type: 'text',     placeholder: 'EHHZBKPF',
        hint: 'Leave blank to auto-detect the first charger on the account.' }
    ]
  };

  // Everything needed to render fields for a given EV driver catalog
  // entry. Cloud drivers (declared via a custom entry in EV_DRIVER_FIELDS
  // with ev_charger.* targets) get just those fields — no connection
  // block, because cfg.ev_charger carries the creds and the backend
  // materialises the driver. Local drivers reuse the same helpers as the
  // main driver wizard (step 5), so a new Modbus/MQTT EV driver works
  // with zero changes here.
  function evFieldsFor(entry) {
    if (!entry) return [];
    var custom = EV_DRIVER_FIELDS[entry.id];
    if (custom && custom.length > 0) {
      var usesEVCharger = custom.some(function (f) {
        return f.target && f.target.indexOf('ev_charger.') === 0;
      });
      if (usesEVCharger) return custom;
      return connectionFieldsFor(entry).concat(custom);
    }
    return connectionFieldsFor(entry).concat(extraFieldsFor(entry));
  }

  function populateEVProviders() {
    var sel = document.getElementById('ev-provider');
    var prev = sel.value;
    sel.innerHTML = '<option value="">None</option>';

    var rows = [];
    driverCatalog.forEach(function (entry) {
      var caps = entry.capabilities || [];
      if (caps.indexOf('ev') === -1) return;
      rows.push({ entry: entry, label: driverLabel(entry) });
    });
    rows.sort(function (a, b) {
      return a.label.localeCompare(b.label, undefined, { sensitivity: 'base' });
    });

    rows.forEach(function (r) {
      var opt = document.createElement('option');
      opt.value = r.entry.id;
      opt.textContent = r.label;
      sel.appendChild(opt);
    });

    if (prev) sel.value = prev;
    renderEVFields();
  }

  function findEVEntry(id) {
    if (!id) return null;
    for (var i = 0; i < driverCatalog.length; i++) {
      if (driverCatalog[i].id === id) return driverCatalog[i];
    }
    return null;
  }

  function renderEVFields() {
    var entry = findEVEntry(document.getElementById('ev-provider').value);
    renderFields('ev-provider-fields', evFieldsFor(entry));
  }

  function prepareIntegrations() {
    var zone = document.getElementById('price-zone').value;
    document.getElementById('price-zone-readonly').value = zone;

    var providerSel = document.getElementById('price-provider');
    if (zone.startsWith('SE') || zone.startsWith('NO') || zone.startsWith('DK') || zone === 'FI') {
      providerSel.value = 'elprisetjustnu';
    }

    // Catalog is loaded on step 4 in the standard flow, but the "Skip
    // device setup" shortcut jumps straight here — fetch it lazily.
    if (driverCatalog.length === 0) {
      fetch('/api/drivers/catalog')
        .then(function (r) { return r.json(); })
        .then(function (data) {
          driverCatalog = data.entries || [];
          populateEVProviders();
        })
        .catch(function () { populateEVProviders(); });
    } else {
      populateEVProviders();
    }

    if (!integrationListenersBound) {
      integrationListenersBound = true;
      document.getElementById('ev-provider').addEventListener('change', renderEVFields);
      document.getElementById('ha-enabled').addEventListener('change', function () {
        document.getElementById('ha-fields').style.display = this.checked ? 'block' : 'none';
      });
    }
  }

  // --- Step 8: Review ---

  function renderReview() {
    var zone = document.getElementById('price-zone').value;
    var html = '';

    // Site
    html += '<div class="review-section"><h3>Site</h3><div class="review-item">';
    html += esc(document.getElementById('site-name').value) + ', ';
    html += document.getElementById('fuse-phases').value + '&times;' +
            document.getElementById('fuse-amps').value + 'A @ ' +
            document.getElementById('fuse-voltage').value + 'V, ' +
            esc(zone);
    html += '</div></div>';

    // Devices
    if (configuredDrivers.length > 0) {
      html += '<div class="review-section"><h3>Devices</h3>';
      configuredDrivers.forEach(function (d) {
        var desc = esc(d.name);
        var detail = driverDetail(d);
        if (detail) desc += ' — ' + esc(detail);
        html += '<div class="review-item">' + desc + '</div>';
      });
      html += '</div>';
    }

    // EV
    var evProviderId = document.getElementById('ev-provider').value;
    var evEntry = findEVEntry(evProviderId);
    if (evEntry) {
      html += '<div class="review-section"><h3>EV Charger</h3><div class="review-item">';
      html += esc(evEntry.name || evEntry.id);
      // Surface the distinguishing field (host for local drivers, serial
      // for cloud ones) if the operator entered one — handy for sanity
      // checking the review screen without enumerating every possible
      // knob.
      var summary = '';
      var hostInp   = document.querySelector('#ev-provider-fields [data-key="host"]');
      var serialInp = document.querySelector('#ev-provider-fields [data-key="serial"]');
      if (hostInp && hostInp.value.trim())        summary = hostInp.value.trim();
      else if (serialInp && serialInp.value.trim()) summary = serialInp.value.trim();
      if (summary) html += ' (' + esc(summary) + ')';
      html += '</div></div>';
    }

    // HA
    var haEnabled = document.getElementById('ha-enabled').checked;
    if (haEnabled) {
      var haBroker = document.getElementById('ha-broker').value;
      var haPort = document.getElementById('ha-port').value;
      html += '<div class="review-section"><h3>Home Assistant</h3><div class="review-item">';
      html += esc(haBroker) + ':' + esc(haPort);
      html += '</div></div>';
    }

    // Price
    var priceProv = document.getElementById('price-provider').value;
    if (priceProv) {
      html += '<div class="review-section"><h3>Price</h3><div class="review-item">';
      html += esc(priceProv) + ' / ' + esc(zone);
      html += '</div></div>';
    }

    document.getElementById('review-content').innerHTML = html;
    document.getElementById('save-error').style.display = 'none';
    document.getElementById('save-actions').style.display = 'flex';
    document.getElementById('save-success').style.display = 'none';
  }

  // --- Save config ---

  window.saveConfig = function () {
    // Empty device list is OK now — the operator can come back and add
    // drivers from /settings. The backend accepts a config with zero
    // drivers and runs the API + web UI in a parked state so this first
    // save isn't blocking.
    var btn = document.getElementById('save-btn');
    btn.disabled = true;
    btn.textContent = 'Saving...';

    var cfg = buildConfig();

    fetch('/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg)
    })
      .then(function (r) {
        if (!r.ok) return r.json().then(function (d) { throw new Error(d.error || 'Save failed'); });
        return r.json();
      })
      .then(function () {
        document.getElementById('save-actions').style.display = 'none';
        document.getElementById('save-success').style.display = 'block';
        setTimeout(function () { window.location.href = '/'; }, 3000);
      })
      .catch(function (err) {
        btn.disabled = false;
        btn.textContent = 'Start 42W';
        var errEl = document.getElementById('save-error');
        errEl.className = 'error-msg';
        errEl.textContent = err.message;
        errEl.style.display = 'block';
      });
  };

  function buildConfig() {
    var zone = document.getElementById('price-zone').value;

    var cfg = {
      site: {
        name: document.getElementById('site-name').value.trim() || 'My Home',
        control_interval_s: 5,
        grid_target_w: 0,
        grid_tolerance_w: 42,
        watchdog_timeout_s: 60,
        smoothing_alpha: 0.3,
        gain: 0.5,
        slew_rate_w: 500,
        min_dispatch_interval_s: 5
      },
      fuse: {
        max_amps: parseFloat(document.getElementById('fuse-amps').value) || 16,
        phases: parseInt(document.getElementById('fuse-phases').value, 10) || 3,
        voltage: parseFloat(document.getElementById('fuse-voltage').value) || 230
      },
      drivers: configuredDrivers.map(function (d) {
        var clean = {};
        for (var k in d) {
          if (d.hasOwnProperty(k) && k !== '_catalog') clean[k] = d[k];
        }
        return clean;
      }),
      api: { port: 8080 }
    };

    // Price
    var priceProv = document.getElementById('price-provider').value;
    if (priceProv) {
      cfg.price = {
        provider: priceProv,
        zone: zone
      };
    }

    // EV Charger
    //
    // The selected EV driver is a catalog entry (capability contains
    // "ev"). We walk the rendered fields and let their `target` paths
    // decide where the values land:
    //
    //   target starts with "ev_charger.*" → cfg.ev_charger (cloud
    //     providers like Easee — backend persists/masks the password
    //     from state.db and auto-materialises a Lua driver entry).
    //
    //   any other target (capabilities.*, config.*) → cfg.drivers[]
    //     entry using the catalog entry's path + whatever connection
    //     fields the driver declared.
    //
    // Adding a new EV driver therefore needs nothing but a .lua file in
    // drivers/ — the dropdown picks it up via /api/drivers/catalog and
    // the renderer turns its metadata into fields automatically.
    var evProviderId = document.getElementById('ev-provider').value;
    var evEntry = findEVEntry(evProviderId);
    if (evEntry) {
      var evInputs = document.querySelectorAll('#ev-provider-fields [data-target]');
      var useEVCharger = false;
      var bag = {};
      for (var e = 0; e < evInputs.length; e++) {
        if ((evInputs[e].dataset.target || '').indexOf('ev_charger.') === 0) {
          useEVCharger = true;
          break;
        }
      }

      if (useEVCharger) {
        cfg.ev_charger = {
          provider: (evEntry.manufacturer || evEntry.id).toLowerCase()
        };
        for (var f = 0; f < evInputs.length; f++) {
          readFieldInto(bag, evInputs[f]);
        }
        if (bag.ev_charger) {
          for (var k in bag.ev_charger) {
            if (bag.ev_charger.hasOwnProperty(k)) cfg.ev_charger[k] = bag.ev_charger[k];
          }
        }
      } else {
        var baseName = (evEntry.manufacturer || evEntry.id || 'ev')
          .toLowerCase().replace(/[^a-z0-9]/g, '_').replace(/_+/g, '_').replace(/^_|_$/g, '');
        var name = baseName;
        var idx = 2;
        while (cfg.drivers.some(function (d) { return d.name === name; })) {
          name = baseName + '_' + idx;
          idx++;
        }
        var drv = {
          name: name,
          is_site_meter: false,
          lua: evEntry.path,
          capabilities: {}
        };
        for (var g = 0; g < evInputs.length; g++) {
          readFieldInto(drv, evInputs[g]);
        }
        // Strip empty capability blocks: a hybrid driver with only its
        // primary endpoint filled in shouldn't carry an empty
        // {capabilities:{mqtt:{}}} into config — that wires a broker-
        // less MQTT cap and the driver fails on the first subscribe.
        for (var capProto in drv.capabilities) {
          if (drv.capabilities.hasOwnProperty(capProto)) {
            var cap = drv.capabilities[capProto];
            if (!cap || typeof cap !== 'object' || !cap.host) {
              delete drv.capabilities[capProto];
            }
          }
        }
        cfg.drivers.push(drv);
      }
    }

    // Home Assistant
    var haEnabled = document.getElementById('ha-enabled').checked;
    if (haEnabled) {
      cfg.homeassistant = {
        enabled: true,
        broker: document.getElementById('ha-broker').value.trim(),
        port: parseInt(document.getElementById('ha-port').value, 10) || 1883,
        username: document.getElementById('ha-user').value.trim(),
        password: document.getElementById('ha-pass').value
      };
    }

    return cfg;
  }

  // --- Helpers ---

  function esc(s) {
    if (!s) return '';
    var d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
  }

  // Skip the whole wizard — write a minimal default config so the
  // backend restarts out of bootstrap mode, then land the browser in
  // the main app. Operators finish the work in /settings.
  window.skipSetup = function () {
    var cfg = {
      site: {
        name: 'My Home',
        control_interval_s: 5,
        grid_target_w: 0,
        grid_tolerance_w: 42,
        watchdog_timeout_s: 60,
        smoothing_alpha: 0.3,
        gain: 0.5,
        slew_rate_w: 500,
        min_dispatch_interval_s: 5
      },
      fuse:    { max_amps: 16, phases: 3, voltage: 230 },
      drivers: [],
      api:     { port: 8080 }
    };
    fetch('/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg)
    }).then(function (r) {
      if (!r.ok) return r.json().then(function (d) {
        throw new Error(d.error || 'Skip failed');
      });
      // Give the bootstrap a moment to write the config and restart
      // the process. If it's back up quickly the redirect will hit
      // the real UI; if not the browser will retry.
      setTimeout(function () { window.location.href = '/'; }, 2000);
    }).catch(function (err) {
      alert('Could not skip setup: ' + err.message);
    });
  };

  // --- Init ---
  renderDots();
  goStep(1);
})();
