// Dashboard del backend Notion 5G. Vanilla JS, sin dependencias externas.
// La API key vive en localStorage del navegador y se manda como header
// X-API-Key en cada fetch() a /api/v1/*. Esta página (/, /assets/*) no
// requiere la key para cargar.
(function () {
  "use strict";

  var LS_KEY = "notion5g_api_key";
  var DEVICE_REFRESH_MS = 20000;
  var SPEEDTEST_POLL_MS = 1500;
  var SPEEDTEST_TIMEOUT_MS = 60000;
  // Prueba de velocidad "real" corrida por este navegador (no por el router):
  // ver runBrowserSpeedtestBtn más abajo. BROWSER_UPLOAD_TARGET_S es cuánto
  // debería durar la subida si acertamos el tamaño del cuerpo a mandar; como
  // no sabemos la velocidad de subida de antemano, se estima como una
  // fracción de la bajada recién medida (5G suele ser bastante asimétrico) y
  // se acota entre MIN/MAX_BYTES para no quedarse corto (ruido de RTT) ni
  // trabarse mandando demasiado en una conexión lenta.
  var BROWSER_DOWNLOAD_MS = 6000;
  var BROWSER_UPLOAD_TARGET_S = 5;
  var BROWSER_UPLOAD_MIN_BYTES = 3 * 1e6;
  var BROWSER_UPLOAD_MAX_BYTES = 80 * 1e6;
  // Cuántas filas traer en la vista de detalle. Antes eran 20/10/30 -- se
  // cortaba el historial del mismo día (con el equipo mandando un heartbeat
  // cada minuto desde temprano, 30 apenas cubre media hora) y la primera
  // prueba de la mañana quedaba afuera. El backend soporta hasta 5000; estos
  // valores alcanzan cómodo para un día entero de prueba de campo continua.
  var DETAIL_MEASUREMENTS_LIMIT = 500;
  var DETAIL_COMMANDS_LIMIT = 100;
  var DETAIL_HEARTBEATS_LIMIT = 2000;

  var state = {
    apiKey: "",
    devices: [],
    view: "list", // "list" | "detail"
    selectedDeviceId: null,
    deviceRefreshTimer: null,
    speedtest: null, // {commandId, pollTimer, deadline}
    movement: null, // {deviceId, timer, inFlight}
    detail: { measurements: [], heartbeats: [], commands: [] }, // último detalle cargado, sin filtrar por operador
  };

  // ---------------------------------------------------------------- storage

  // Clave por defecto para que la página funcione sola (sin escribir nada) en
  // cualquier navegador, incluido modo incógnito -- pedido explícito de Diego
  // el 2026-09-18 antes de salir a una prueba de campo, aceptando a propósito
  // que esto deja la clave visible en el código fuente de la página para
  // cualquiera con el link. Es un tradeoff consciente por la urgencia, no un
  // descuido: revisar si sigue siendo aceptable más adelante (rotar la clave
  // y quitar este default si el link llega a compartirse más ampliamente).
  var DEFAULT_API_KEY = "7f4066d8090492ff77bc89bbd4e972f10c0143c3d17b274d";

  function loadKey() {
    try {
      return localStorage.getItem(LS_KEY) || DEFAULT_API_KEY;
    } catch (e) {
      return DEFAULT_API_KEY;
    }
  }

  function saveKey(key) {
    try {
      localStorage.setItem(LS_KEY, key);
    } catch (e) {
      // localStorage bloqueado (ventana privada, etc.): seguimos igual, solo
      // que la key no sobrevivirá a un refresh de página.
    }
  }

  // ---------------------------------------------------------------- fetch helper

  // apiFetch: agrega X-API-Key y normaliza errores en {kind, message}.
  // kind: "network" (backend inalcanzable) | "auth" (401) | "http" (otro
  // status no-2xx) | null (ok).
  function apiFetch(path, opts) {
    opts = opts || {};
    var headers = Object.assign({}, opts.headers || {}, { "X-API-Key": state.apiKey });
    return fetch(path, Object.assign({}, opts, { headers: headers }))
      .catch(function () {
        return Promise.reject({ kind: "network", message: "No se pudo conectar con el backend." });
      })
      .then(function (res) {
        if (res.status === 401) {
          return Promise.reject({ kind: "auth", message: "API key inválida o ausente." });
        }
        if (!res.ok) {
          return res
            .text()
            .catch(function () {
              return "";
            })
            .then(function (body) {
              return Promise.reject({
                kind: "http",
                message: "Error " + res.status + (body ? ": " + body : ""),
              });
            });
        }
        if (res.status === 204) return null;
        return res.json().catch(function () {
          return null;
        });
      });
  }

  // apiRawFetch: como apiFetch pero devuelve el Response tal cual, sin tocar
  // el body -- lo necesita la descarga de la prueba de velocidad para leer el
  // stream a mano por chunks (res.json() se lo comería entero de golpe).
  function apiRawFetch(path, opts) {
    opts = opts || {};
    var headers = Object.assign({}, opts.headers || {}, { "X-API-Key": state.apiKey });
    return fetch(path, Object.assign({}, opts, { headers: headers })).catch(function () {
      return Promise.reject({ kind: "network", message: "No se pudo conectar con el backend." });
    });
  }

  // ---------------------------------------------------------------- key status

  var keyInput = document.getElementById("api-key");
  var keySaveBtn = document.getElementById("key-save");
  var keyStatusEl = document.getElementById("key-status");

  function setKeyStatus(kind, text) {
    keyStatusEl.className = "status-pill status-" + kind;
    keyStatusEl.textContent = text;
  }

  function testKeyAndLoad() {
    if (!state.apiKey) {
      setKeyStatus("unknown", "sin probar");
      return;
    }
    setKeyStatus("unknown", "probando...");
    apiFetch("/api/v1/devices")
      .then(function (devices) {
        setKeyStatus("ok", "OK");
        state.devices = devices || [];
        renderDeviceList();
        clearBanner(listErrorEl);
        startDeviceAutoRefresh();
      })
      .catch(function (err) {
        if (err.kind === "auth") {
          setKeyStatus("error", "key inválida");
        } else if (err.kind === "network") {
          setKeyStatus("error", "backend inalcanzable");
        } else {
          setKeyStatus("error", "error");
        }
        showListError(err.message);
      });
  }

  keySaveBtn.addEventListener("click", function () {
    state.apiKey = keyInput.value.trim();
    saveKey(state.apiKey);
    testKeyAndLoad();
  });
  keyInput.addEventListener("keydown", function (e) {
    if (e.key === "Enter") keySaveBtn.click();
  });

  // ---------------------------------------------------------------- vistas

  var viewList = document.getElementById("view-list");
  var viewDetail = document.getElementById("view-detail");
  var listErrorEl = document.getElementById("list-error");
  var listEmptyEl = document.getElementById("list-empty");
  var devicesTable = document.getElementById("devices-table");
  var devicesTbody = document.getElementById("devices-tbody");
  var listUpdatedEl = document.getElementById("list-updated");

  function showBanner(el, message) {
    el.textContent = message;
    el.hidden = false;
  }
  function clearBanner(el) {
    el.hidden = true;
    el.textContent = "";
  }
  function showListError(message) {
    showBanner(listErrorEl, message);
  }

  function showView(name) {
    state.view = name;
    viewList.hidden = name !== "list";
    viewDetail.hidden = name !== "detail";
  }

  // ---------------------------------------------------------------- formato

  function fmtDate(iso) {
    if (!iso) return "—";
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toLocaleString();
  }

  function fmtNum(n, unit, digits) {
    if (n === null || n === undefined) return "—";
    return Number(n).toFixed(digits === undefined ? 1 : digits) + (unit || "");
  }

  function ratBadge(rat) {
    if (!rat) return '<span class="rat-badge rat-other">—</span>';
    var s = String(rat).toLowerCase();
    var cls = "rat-other";
    if (s.indexOf("5g") !== -1 || s.indexOf("nr") !== -1) cls = "rat-5g";
    else if (s.indexOf("4g") !== -1 || s.indexOf("lte") !== -1) cls = "rat-4g";
    return '<span class="rat-badge ' + cls + '">' + escapeHtml(rat) + "</span>";
  }

  function fmtAgo(seconds) {
    if (seconds === null || seconds === undefined) return "";
    if (seconds < 60) return " (hace " + Math.round(seconds) + "s)";
    if (seconds < 3600) return " (hace " + Math.round(seconds / 60) + "m)";
    return " (hace " + Math.round(seconds / 3600) + "h)";
  }

  function onlineBadge(d) {
    var ago = fmtAgo(d.last_seen_seconds_ago);
    if (d.online) {
      return '<span class="online-badge online-yes"><span class="dot"></span>en línea' + ago + "</span>";
    }
    return '<span class="online-badge online-no"><span class="dot"></span>sin señal' + ago + "</span>";
  }

  function fmtUptime(seconds) {
    if (seconds === null || seconds === undefined) return "—";
    var s = Math.round(seconds);
    var d = Math.floor(s / 86400);
    var h = Math.floor((s % 86400) / 3600);
    var m = Math.floor((s % 3600) / 60);
    if (d > 0) return d + "d " + h + "h";
    if (h > 0) return h + "h " + m + "m";
    return m + "m";
  }

  function ratAndBand(rat, bandLte) {
    var badge = ratBadge(rat);
    if (bandLte === null || bandLte === undefined) return badge;
    return badge + ' <span class="muted">B' + escapeHtml(String(bandLte)) + "</span>";
  }

  function fmtPing(pingMs, lossPct) {
    if (pingMs === null || pingMs === undefined) return "—";
    var s = fmtNum(pingMs, " ms", 0);
    if (lossPct !== null && lossPct !== undefined && lossPct > 0) {
      s += " (" + fmtNum(lossPct, "% pérdida", 0) + ")";
    }
    return s;
  }

  function mapsLink(lat, lon, accuracy) {
    if (lat === null || lat === undefined || lon === null || lon === undefined) return "—";
    var text = lat.toFixed(5) + ", " + lon.toFixed(5);
    var url = "https://maps.google.com/?q=" + lat + "," + lon;
    var acc = accuracy !== null && accuracy !== undefined ? " (±" + Math.round(accuracy) + "m)" : "";
    return '<a href="' + url + '" target="_blank" rel="noopener">' + text + "</a>" + acc;
  }

  function escapeHtml(s) {
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  // ---------------------------------------------------------------- lista de dispositivos

  function renderDeviceList() {
    listUpdatedEl.textContent = "actualizado " + new Date().toLocaleTimeString();
    if (!state.devices || state.devices.length === 0) {
      devicesTable.hidden = true;
      listEmptyEl.hidden = false;
      return;
    }
    listEmptyEl.hidden = true;
    devicesTable.hidden = false;
    devicesTbody.innerHTML = "";
    state.devices.forEach(function (d) {
      var tr = document.createElement("tr");
      tr.className = "device-row";
      tr.addEventListener("click", function () {
        showDetail(d.device_id);
      });
      tr.innerHTML =
        '<td data-label="Equipo">' +
        escapeHtml(d.device_id) +
        '</td><td data-label="Estado">' +
        onlineBadge(d) +
        '</td><td data-label="Última vez visto">' +
        fmtDate(d.last_seen) +
        '</td><td data-label="Operador">' +
        escapeHtml(d.operator || "—") +
        '</td><td data-label="Red / Banda">' +
        ratAndBand(d.rat, d.band_lte) +
        '</td><td data-label="RSRP">' +
        fmtNum(d.rsrp_dbm, " dBm", 0) +
        '</td><td data-label="Ping">' +
        fmtPing(d.ping_ms, d.loss_pct) +
        '</td><td data-label="Ubicación">' +
        mapsLink(d.lat, d.lon, d.gps_accuracy_m) +
        '</td><td data-label="Velocidad (↓/↑)">' +
        fmtNum(d.down_mbps, " Mbps") +
        " / " +
        fmtNum(d.up_mbps, " Mbps") +
        '</td><td data-label="Encendido">' +
        fmtUptime(d.uptime_s) +
        "</td>";
      devicesTbody.appendChild(tr);
    });
  }

  function refreshDevices() {
    if (!state.apiKey) return;
    apiFetch("/api/v1/devices")
      .then(function (devices) {
        state.devices = devices || [];
        clearBanner(listErrorEl);
        if (state.view === "list") renderDeviceList();
        else listUpdatedEl.textContent = "actualizado " + new Date().toLocaleTimeString();
      })
      .catch(function (err) {
        showListError(err.message);
      });
  }

  function startDeviceAutoRefresh() {
    if (state.deviceRefreshTimer) clearInterval(state.deviceRefreshTimer);
    state.deviceRefreshTimer = setInterval(refreshDevices, DEVICE_REFRESH_MS);
  }

  // ---------------------------------------------------------------- detalle

  var backBtn = document.getElementById("back-btn");
  var detailTitle = document.getElementById("detail-title");
  var detailErrorEl = document.getElementById("detail-error");
  var measurementsTbody = document.getElementById("measurements-tbody");
  var heartbeatsTbody = document.getElementById("heartbeats-tbody");
  var commandsTbody = document.getElementById("commands-tbody");
  var runSpeedtestBtn = document.getElementById("run-speedtest");
  var speedtestStatusEl = document.getElementById("speedtest-status");
  var runBrowserSpeedtestBtn = document.getElementById("run-browser-speedtest");
  var browserSpeedtestStatusEl = document.getElementById("browser-speedtest-status");
  var movementToggle = document.getElementById("movement-toggle");
  var movementStatusEl = document.getElementById("movement-status");
  var operatorFilterEl = document.getElementById("operator-filter");
  var bandSummaryEl = document.getElementById("band-summary");

  backBtn.addEventListener("click", function () {
    stopSpeedtestPoll();
    stopMovementBeacon();
    showView("list");
    renderDeviceList(); // lo que ya había, al instante
    refreshDevices(); // y de inmediato pide lo fresco (por si acabas de correr una prueba)
  });

  function showDetail(deviceId) {
    stopMovementBeacon(); // por si venías de otro equipo con el modo activo
    state.selectedDeviceId = deviceId;
    state.detail = { measurements: [], heartbeats: [], commands: [] };
    detailTitle.textContent = deviceId;
    speedtestStatusEl.textContent = "";
    movementStatusEl.textContent = "";
    movementToggle.checked = false;
    runSpeedtestBtn.disabled = false;
    operatorFilterEl.innerHTML = '<option value="">Todos los operadores</option>';
    operatorFilterEl.value = "";
    bandSummaryEl.innerHTML = "";
    clearBanner(detailErrorEl);
    showView("detail");
    loadDetail(deviceId);
  }

  function loadDetail(deviceId) {
    Promise.all([
      apiFetch("/api/v1/measurements?device_id=" + encodeURIComponent(deviceId) + "&limit=" + DETAIL_MEASUREMENTS_LIMIT),
      apiFetch("/api/v1/commands?device_id=" + encodeURIComponent(deviceId) + "&limit=" + DETAIL_COMMANDS_LIMIT),
      apiFetch("/api/v1/heartbeats?device_id=" + encodeURIComponent(deviceId) + "&limit=" + DETAIL_HEARTBEATS_LIMIT),
    ])
      .then(function (results) {
        state.detail = {
          measurements: results[0] || [],
          commands: results[1] || [],
          heartbeats: results[2] || [],
        };
        renderCommands(state.detail.commands);
        populateOperatorFilter(state.detail.measurements, state.detail.heartbeats);
        applyDetailFilter();
      })
      .catch(function (err) {
        showBanner(detailErrorEl, err.message);
      });
  }

  // El operador se guarda por medición/heartbeat (puede cambiar a mitad de
  // trayecto si el módem hace roaming o si se prueban varias SIM). El filtro
  // es 100% del lado del cliente sobre lo que ya se trajo (measurements +
  // heartbeats + mapa comparten el mismo filtro) -- no hace falta pegarle de
  // nuevo al backend, que ya soporta ?operator= si algún día se necesita
  // paginar un historial más largo por operador.
  function populateOperatorFilter(measurements, heartbeats) {
    var seen = {};
    measurements.concat(heartbeats).forEach(function (r) {
      if (r.operator) seen[r.operator] = true;
    });
    var current = operatorFilterEl.value;
    var ops = Object.keys(seen).sort();
    operatorFilterEl.innerHTML = '<option value="">Todos los operadores</option>';
    ops.forEach(function (op) {
      var opt = document.createElement("option");
      opt.value = op;
      opt.textContent = op;
      operatorFilterEl.appendChild(opt);
    });
    operatorFilterEl.value = ops.indexOf(current) !== -1 ? current : "";
  }

  function filterByOperator(rows) {
    var op = operatorFilterEl.value;
    if (!op) return rows;
    return rows.filter(function (r) {
      return r.operator === op;
    });
  }

  function applyDetailFilter() {
    var measurements = filterByOperator(state.detail.measurements || []);
    var heartbeats = filterByOperator(state.detail.heartbeats || []);
    renderMeasurements(measurements);
    renderHeartbeats(heartbeats);
    renderBandSummary(measurements);
    renderMap(measurements, heartbeats);
  }

  operatorFilterEl.addEventListener("change", applyDetailFilter);

  // ---------------------------------------------------------------- resumen de bandas probadas

  function renderBandSummary(measurements) {
    if (!measurements.length) {
      bandSummaryEl.innerHTML =
        '<span class="muted">Todavía no hay mediciones (con este filtro) para resumir bandas probadas.</span>';
      return;
    }
    var lte = {};
    var nr = {};
    var sawNR = false;
    measurements.forEach(function (m) {
      if (m.band_lte !== null && m.band_lte !== undefined) lte["B" + m.band_lte] = true;
      if (m.nr_band !== null && m.nr_band !== undefined) {
        nr["n" + m.nr_band] = true;
        sawNR = true;
      } else if (m.rat && String(m.rat).toLowerCase().indexOf("nr") !== -1) {
        sawNR = true; // reportó 5G pero sin nr_band específico en esa medición
      }
    });
    var lteList = Object.keys(lte).sort();
    var nrList = Object.keys(nr).sort();
    var html = '<span class="summary-label">Bandas LTE probadas:</span>';
    html += lteList.length
      ? lteList.map(function (b) { return '<span class="band-chip">' + escapeHtml(b) + "</span>"; }).join("")
      : '<span class="muted">ninguna todavía</span>';
    html += '<br><span class="summary-label">Bandas 5G/NR probadas:</span>';
    if (nrList.length) {
      html += nrList.map(function (b) { return '<span class="band-chip band-nr">' + escapeHtml(b) + "</span>"; }).join("");
    } else if (sawNR) {
      html += '<span class="muted">el equipo reportó 5G pero sin banda NR específica en la medición</span>';
    } else {
      html += '<span class="muted">todavía no se ha probado en 5G</span>';
    }
    bandSummaryEl.innerHTML = html;
  }

  // ---------------------------------------------------------------- mapa de ubicaciones (Google Maps)
  //
  // La API key (proyecto ocpp-energy, ios/Flutter/Secrets.xcconfig) es de una
  // app de iOS -- si Google Cloud la tiene restringida por bundle ID, en un
  // sitio web va a fallar. window.gm_authFailure (definido en index.html, se
  // llama desde el JS de Google, no algo que se pueda atrapar acá) avisa en
  // pantalla si pasa eso, en vez de dejar el mapa roto en silencio.

  var mapInstance = null;
  var mapMarkers = [];
  var mapTrailLine = null;
  var mapInfoWindow = null;

  // Color de cada punto por calidad de señal (RSRP), no por operador: en una
  // prueba de campo casi siempre hay una sola SIM/operador a la vez, así que
  // colorear por operador dejaba casi todos los puntos del mismo color. Lo
  // que sí varía real y visualmente a lo largo del trayecto es la señal --
  // umbrales estándar de RSRP LTE/NR (dBm).
  var SIGNAL_SCALE = [
    { min: -80, color: "#37c974", label: "≥ -80 dBm · excelente" },
    { min: -90, color: "#8bd450", label: "-80 a -90 dBm · buena" },
    { min: -100, color: "#ffb84f", label: "-90 a -100 dBm · regular" },
    { min: -110, color: "#ff8a5d", label: "-100 a -110 dBm · mala" },
    { min: -Infinity, color: "#ff5d6c", label: "< -110 dBm · muy mala" },
  ];
  var SIGNAL_UNKNOWN = { color: "#8b93b8", label: "sin dato de señal" };

  function signalBucket(rsrp) {
    if (rsrp === null || rsrp === undefined) return SIGNAL_UNKNOWN;
    for (var i = 0; i < SIGNAL_SCALE.length; i++) {
      if (rsrp >= SIGNAL_SCALE[i].min) return SIGNAL_SCALE[i];
    }
    return SIGNAL_UNKNOWN;
  }

  function renderMapLegend() {
    var el = document.getElementById("map-legend");
    if (!el || el.childNodes.length) return; // estática, se arma una sola vez
    var items = SIGNAL_SCALE.concat([SIGNAL_UNKNOWN]);
    el.innerHTML = items
      .map(function (b) {
        return '<span class="map-legend-item"><i style="background:' + b.color + '"></i>' + escapeHtml(b.label) + "</span>";
      })
      .join("");
  }

  // Tema oscuro de Google Maps para que combine con el resto del dashboard
  // (por defecto Google Maps es claro).
  var GMAPS_DARK_STYLE = [
    { elementType: "geometry", stylers: [{ color: "#131a2e" }] },
    { elementType: "labels.text.stroke", stylers: [{ color: "#0b1020" }] },
    { elementType: "labels.text.fill", stylers: [{ color: "#8b93b8" }] },
    { featureType: "administrative", elementType: "geometry", stylers: [{ color: "#26315a" }] },
    { featureType: "poi", stylers: [{ visibility: "off" }] },
    { featureType: "road", elementType: "geometry", stylers: [{ color: "#1a2340" }] },
    { featureType: "road", elementType: "geometry.stroke", stylers: [{ color: "#26315a" }] },
    { featureType: "transit", stylers: [{ visibility: "off" }] },
    { featureType: "water", elementType: "geometry", stylers: [{ color: "#0b1020" }] },
  ];

  function ensureMap() {
    if (mapInstance) return mapInstance;
    if (window.__gmapsAuthFailed) return null;
    if (typeof google === "undefined" || !google.maps) return null; // el script de Google Maps no cargó todavía (o falló)
    mapInstance = new google.maps.Map(document.getElementById("map"), {
      center: { lat: 4.711, lng: -74.0721 }, // Bogotá, fallback hasta tener puntos reales
      zoom: 11,
      styles: GMAPS_DARK_STYLE,
      streetViewControl: false,
      mapTypeControl: false,
      fullscreenControl: false,
    });
    mapInfoWindow = new google.maps.InfoWindow();
    return mapInstance;
  }

  function mapPopupHtml(row, kind) {
    var isNR = row.rat && String(row.rat).toLowerCase().indexOf("nr") !== -1;
    var band = isNR && row.nr_band !== null && row.nr_band !== undefined
      ? "n" + row.nr_band
      : row.band_lte !== null && row.band_lte !== undefined
      ? "B" + row.band_lte
      : "";
    var bits = ["<b>" + escapeHtml(kind) + " · " + escapeHtml(fmtDate(row._received_at || row.ts)) + "</b>"];
    bits.push(escapeHtml(row.operator || "—") + (band ? " · " + escapeHtml(band) : ""));
    bits.push(fmtNum(row.rsrp_dbm, " dBm RSRP", 0));
    if (row.down_mbps !== undefined || row.up_mbps !== undefined) {
      bits.push("↓" + fmtNum(row.down_mbps) + " Mbps / ↑" + fmtNum(row.up_mbps) + " Mbps");
    }
    if (row.ping_ms !== undefined) {
      bits.push("ping " + fmtPing(row.ping_ms, row.loss_pct));
    }
    if (row.uptime_s !== undefined) {
      bits.push("uptime del equipo: " + fmtUptime(row.uptime_s));
    }
    if (row.lat_end !== undefined && row.lat_end !== null && row.lon_end !== undefined && row.lon_end !== null) {
      var movedM = haversineMeters(row.lat, row.lon, row.lat_end, row.lon_end);
      bits.push("se movió ~" + Math.round(movedM) + "m durante la prueba (marcador ◇ = fin)");
    }
    return '<div class="map-popup">' + bits.join("<br>") + "</div>";
  }

  // Distancia entre dos puntos (fórmula de Haversine, suficiente en
  // distancias cortas como las que se mueve un equipo durante una prueba de
  // pocos segundos) -- usada para mostrar cuánto se desplazó entre el inicio
  // y el fin de la prueba de velocidad.
  function haversineMeters(lat1, lon1, lat2, lon2) {
    var R = 6371000;
    var toRad = function (d) { return (d * Math.PI) / 180; };
    var dLat = toRad(lat2 - lat1);
    var dLon = toRad(lon2 - lon1);
    var a =
      Math.sin(dLat / 2) * Math.sin(dLat / 2) +
      Math.cos(toRad(lat1)) * Math.cos(toRad(lat2)) * Math.sin(dLon / 2) * Math.sin(dLon / 2);
    return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
  }

  // Etiqueta de velocidad ↓/↑ SIEMPRE visible junto al marcador (no hace
  // falta tocar/hacer hover) -- pedido explícito: como la gran mayoría de
  // los puntos son heartbeats (sin prueba de velocidad, uno por minuto) y
  // las mediciones con velocidad real son pocas, escondida detrás de un
  // hover casi nunca se veía. undefined = null también cuenta como "sin
  // dato" (la API puede mandar el campo explícito en null).
  function speedLabelText(row) {
    if (row.down_mbps === undefined && row.up_mbps === undefined) return null;
    if (row.down_mbps === null && row.up_mbps === null) return null;
    return "↓" + fmtNum(row.down_mbps) + " / ↑" + fmtNum(row.up_mbps) + " Mbps";
  }

  // Orden cronológico (más viejo primero) de los puntos con ubicación, para
  // poder unirlos con una línea que muestre el trayecto recorrido.
  function chronologicalLatLngs(rows) {
    return rows
      .filter(function (r) { return r.lat !== null && r.lat !== undefined && r.lon !== null && r.lon !== undefined; })
      .slice()
      .sort(function (a, b) {
        return new Date(a._received_at || a.ts) - new Date(b._received_at || b.ts);
      })
      .map(function (r) { return { lat: r.lat, lng: r.lon }; });
  }

  function addMapMarker(row, kind, radius) {
    var color = signalBucket(row.rsrp_dbm).color;
    var speed = speedLabelText(row);
    var marker = new google.maps.Marker({
      position: { lat: row.lat, lng: row.lon },
      map: mapInstance,
      title: kind + " · " + fmtDate(row._received_at || row.ts), // tooltip nativo del navegador al pasar el mouse (desktop)
      icon: {
        path: google.maps.SymbolPath.CIRCLE,
        scale: radius,
        fillColor: color,
        fillOpacity: kind === "medición" ? 0.9 : 0.7,
        strokeColor: "#ffffff",
        strokeWeight: kind === "medición" ? 1.5 : 0.75,
        // la etiqueta de velocidad se dibuja arriba del punto, no encima --
        // si no, tapa el propio marcador que la generó.
        labelOrigin: new google.maps.Point(0, -(radius + 9)),
      },
      zIndex: kind === "medición" ? 20 : 10,
      label: speed ? { text: speed, color: "#e7ecff", fontSize: "11px", fontWeight: "700", className: "gmap-speed-label" } : null,
    });
    marker.addListener("click", function () {
      mapInfoWindow.setContent(mapPopupHtml(row, kind));
      mapInfoWindow.open({ anchor: marker, map: mapInstance });
    });
    mapMarkers.push(marker);

    // Si esta medición tiene ubicación de FIN (se guarda después, cuando
    // termina la prueba -- ver sendSpeedtestEndLocation), se dibuja un
    // segundo marcador en forma de rombo hueco + un segmento punteado
    // uniéndolo con el de arranque. Se ignora si el movimiento es tan chico
    // que es solo ruido del GPS (<3m), para no ensuciar el mapa sin motivo.
    if (row.lat_end !== undefined && row.lat_end !== null && row.lon_end !== undefined && row.lon_end !== null) {
      if (haversineMeters(row.lat, row.lon, row.lat_end, row.lon_end) >= 3) {
        var endMarker = new google.maps.Marker({
          position: { lat: row.lat_end, lng: row.lon_end },
          map: mapInstance,
          title: "fin de la prueba · " + fmtDate(row._received_at || row.ts),
          icon: {
            path: "M 0,-7 L 7,0 L 0,7 L -7,0 Z", // rombo
            fillColor: "#0b1020",
            fillOpacity: 0.9,
            strokeColor: color,
            strokeWeight: 2,
          },
          zIndex: 21,
        });
        endMarker.addListener("click", function () {
          mapInfoWindow.setContent(mapPopupHtml(row, kind));
          mapInfoWindow.open({ anchor: endMarker, map: mapInstance });
        });
        var segment = new google.maps.Polyline({
          path: [
            { lat: row.lat, lng: row.lon },
            { lat: row.lat_end, lng: row.lon_end },
          ],
          strokeOpacity: 0,
          icons: [
            {
              icon: { path: "M 0,-1 0,1", strokeOpacity: 0.9, scale: 2, strokeColor: color },
              offset: "0",
              repeat: "8px",
            },
          ],
          map: mapInstance,
        });
        mapMarkers.push(endMarker, segment);
      }
    }
  }

  function clearMap() {
    mapMarkers.forEach(function (m) { m.setMap(null); });
    mapMarkers = [];
    if (mapTrailLine) {
      mapTrailLine.setMap(null);
      mapTrailLine = null;
    }
  }

  function renderMap(measurements, heartbeats) {
    var mapEl = document.getElementById("map");
    var map = ensureMap();
    if (!map) {
      if (!window.__gmapsAuthFailed) {
        mapEl.innerHTML = '<p class="muted" style="padding:14px">No se pudo cargar Google Maps (sin conexión al script de Google, o todavía está cargando).</p>';
      }
      return;
    }
    renderMapLegend();
    clearMap();

    // Trayecto: los heartbeats son el pulso cada 1 min, así que son la mejor
    // aproximación al camino recorrido; si no hay (todavía) al menos 2, se
    // arma la línea con las mediciones en su lugar. La flecha se repite cada
    // tanto a lo largo de la línea, marcando el sentido de cada tramo.
    var heartbeatTrail = chronologicalLatLngs(heartbeats);
    var usingHeartbeatTrail = heartbeatTrail.length >= 2;
    var trail = usingHeartbeatTrail ? heartbeatTrail : chronologicalLatLngs(measurements);
    if (trail.length >= 2) {
      // Línea sólida (heartbeats, un punto por minuto: el trayecto real) o
      // punteada (fallback con solo mediciones sueltas: es una aproximación
      // gruesa entre pruebas de velocidad, no el camino real). En los dos
      // casos se repite una flecha a lo largo de la línea marcando el
      // sentido de cada tramo -- soporte nativo de Google Maps (`icons` +
      // `repeat`), sin plugins extra.
      mapTrailLine = new google.maps.Polyline({
        path: trail,
        strokeColor: "#4f8cff",
        strokeOpacity: usingHeartbeatTrail ? 0.65 : 0,
        strokeWeight: 3,
        icons: [
          {
            icon: { path: "M 0,-1 0,1", strokeOpacity: 1, scale: 3, strokeColor: "#4f8cff" },
            offset: "0",
            repeat: usingHeartbeatTrail ? "0" : "10px", // línea punteada solo en el fallback aproximado
          },
          {
            icon: {
              path: google.maps.SymbolPath.FORWARD_CLOSED_ARROW,
              scale: 3,
              strokeColor: "#4f8cff",
              fillColor: "#4f8cff",
              fillOpacity: 1,
            },
            offset: "0",
            repeat: "70px", // una flecha de dirección cada ~70px de línea, en cada tramo
          },
        ],
        map: mapInstance,
      });
    }

    var bounds = new google.maps.LatLngBounds();
    var hasPoints = false;
    measurements.forEach(function (m) {
      if (m.lat === null || m.lat === undefined || m.lon === null || m.lon === undefined) return;
      addMapMarker(m, "medición", 7);
      bounds.extend({ lat: m.lat, lng: m.lon });
      if (m.lat_end !== undefined && m.lat_end !== null && m.lon_end !== undefined && m.lon_end !== null) {
        bounds.extend({ lat: m.lat_end, lng: m.lon_end }); // que el marcador de "fin" también entre en el encuadre
      }
      hasPoints = true;
    });
    heartbeats.forEach(function (h) {
      if (h.lat === null || h.lat === undefined || h.lon === null || h.lon === undefined) return;
      addMapMarker(h, "heartbeat", 3);
      bounds.extend({ lat: h.lat, lng: h.lon });
      hasPoints = true;
    });
    if (hasPoints) {
      map.fitBounds(bounds, 24);
    } else {
      map.setCenter({ lat: 4.711, lng: -74.0721 }); // sin ubicaciones todavía: centro de Bogotá como fallback razonable
      map.setZoom(11);
    }
  }

  // deleteMeasurement: borra una prueba puntual del historial (p. ej. una
  // corrida por error con datos móviles en vez de por el módem, o cualquier
  // otra que Diego considere inválida) y recarga el detalle para que la tabla,
  // el mapa y el resumen por banda queden consistentes de inmediato.
  function deleteMeasurement(id, btn) {
    if (!window.confirm("¿Borrar esta prueba del historial? No se puede deshacer.")) return;
    btn.disabled = true;
    apiFetch("/api/v1/measurements/" + encodeURIComponent(id), { method: "DELETE" })
      .then(function () {
        var deviceId = state.selectedDeviceId;
        if (deviceId) loadDetail(deviceId);
        refreshDevices();
      })
      .catch(function (err) {
        btn.disabled = false;
        showBanner(detailErrorEl, "No se pudo borrar la prueba: " + err.message);
      });
  }

  function renderMeasurements(rows) {
    measurementsTbody.innerHTML = "";
    if (!rows.length) {
      measurementsTbody.innerHTML = '<tr><td colspan="8" class="muted">Sin mediciones todavía.</td></tr>';
      return;
    }
    rows.forEach(function (m) {
      var tr = document.createElement("tr");
      var band = m.band_lte || m.nr_band || "—";
      var sig = [m.rsrp_dbm, m.rsrq_db, m.sinr_db]
        .map(function (v) {
          return v === null || v === undefined ? "—" : Number(v).toFixed(0);
        })
        .join(" / ");
      tr.innerHTML =
        '<td data-label="Fecha">' +
        fmtDate(m._received_at || m.ts) +
        '</td><td data-label="Operador">' +
        escapeHtml(m.operator || "—") +
        '</td><td data-label="Banda">' +
        escapeHtml(String(band)) +
        '</td><td data-label="RSRP / RSRQ / SINR">' +
        sig +
        '</td><td data-label="↓ Mbps">' +
        fmtNum(m.down_mbps) +
        '</td><td data-label="↑ Mbps">' +
        fmtNum(m.up_mbps) +
        '</td><td data-label="Uptime">' +
        fmtUptime(m.uptime_s) +
        '</td><td data-label=""></td>';
      if (m._id !== undefined && m._id !== null) {
        var delBtn = document.createElement("button");
        delBtn.type = "button";
        delBtn.className = "btn-link btn-danger";
        delBtn.textContent = "Borrar";
        delBtn.title = "Borrar esta prueba del historial (p. ej. si se corrió por error con datos móviles en vez del módem)";
        delBtn.addEventListener("click", function () {
          deleteMeasurement(m._id, delBtn);
        });
        tr.lastElementChild.appendChild(delBtn);
      }
      measurementsTbody.appendChild(tr);
    });
  }

  function renderHeartbeats(rows) {
    heartbeatsTbody.innerHTML = "";
    if (!rows.length) {
      heartbeatsTbody.innerHTML = '<tr><td colspan="7" class="muted">Sin heartbeats todavía.</td></tr>';
      return;
    }
    rows.forEach(function (h) {
      var tr = document.createElement("tr");
      tr.innerHTML =
        '<td data-label="Fecha">' +
        fmtDate(h._received_at || h.ts) +
        '</td><td data-label="Operador">' +
        escapeHtml(h.operator || "—") +
        '</td><td data-label="Red">' +
        ratBadge(h.rat) +
        '</td><td data-label="RSRP">' +
        fmtNum(h.rsrp_dbm, " dBm", 0) +
        '</td><td data-label="Ping">' +
        fmtPing(h.ping_ms, h.loss_pct) +
        '</td><td data-label="Ubicación">' +
        mapsLink(h.lat, h.lon, h.gps_accuracy_m) +
        '</td><td data-label="Uptime">' +
        fmtUptime(h.uptime_s) +
        "</td>";
      heartbeatsTbody.appendChild(tr);
    });
  }

  function renderCommands(rows) {
    commandsTbody.innerHTML = "";
    if (!rows.length) {
      commandsTbody.innerHTML = '<tr><td colspan="5" class="muted">Sin comandos todavía.</td></tr>';
      return;
    }
    rows.forEach(function (c) {
      var tr = document.createElement("tr");
      tr.innerHTML =
        '<td data-label="ID">' +
        c.id +
        '</td><td data-label="Creado">' +
        fmtDate(c.created_at) +
        '</td><td data-label="Tipo">' +
        escapeHtml(c.type) +
        '</td><td data-label="Estado">' +
        escapeHtml(c.status) +
        '</td><td data-label="Pedido por">' +
        escapeHtml(c.requested_by || "—") +
        "</td>";
      commandsTbody.appendChild(tr);
    });
  }

  // ---------------------------------------------------------------- ubicación del navegador
  //
  // El módem no tiene GPS propio; si el navegador (celular o PC) da permiso,
  // usamos su ubicación para etiquetar la prueba de velocidad y, en modo en
  // movimiento, para el perfil de ping+ubicación -- reemplaza a la app nativa
  // para este caso de uso, sin instalar nada.

  function getBrowserLocation(timeoutMs) {
    return new Promise(function (resolve, reject) {
      if (!("geolocation" in navigator)) {
        reject(new Error("este navegador no expone geolocalización"));
        return;
      }
      navigator.geolocation.getCurrentPosition(
        function (pos) {
          resolve({ lat: pos.coords.latitude, lon: pos.coords.longitude, accuracy: pos.coords.accuracy });
        },
        function (err) {
          reject(new Error(err.message || "ubicación denegada o no disponible"));
        },
        { enableHighAccuracy: true, timeout: timeoutMs || 8000, maximumAge: 0 }
      );
    });
  }

  // ---------------------------------------------------------------- modo en movimiento (navegador)
  //
  // Equivalente en el dashboard al "modo en movimiento" de la app nativa
  // (lib/location_beacon.dart): manda la ubicación del navegador cada 15s a
  // POST /api/v1/devices/{id}/location mientras esté prendido. Sirve para no
  // tener que instalar la app solo para el perfil de ping+ubicación.
  var MOVEMENT_INTERVAL_MS = 15000;

  // Wake Lock: pide que la pantalla no se apague sola mientras el modo en
  // movimiento está prendido -- mitiga (no elimina) la limitación real de
  // §5.6 del handover: el navegador igual puede pausar el timer si la
  // PESTAÑA pasa a segundo plano (otra app, cambiar de pestaña), Wake Lock
  // solo evita que la PANTALLA se apague sola por inactividad. Sin soporte
  // (navegador viejo, o el permiso lo niega el SO) se degrada en silencio:
  // sigue funcionando como antes, solo sin esta ayuda extra.
  var wakeLock = null;

  function requestWakeLock() {
    if (!("wakeLock" in navigator)) return;
    navigator.wakeLock
      .request("screen")
      .then(function (lock) {
        wakeLock = lock;
        wakeLock.addEventListener("release", function () {
          wakeLock = null;
        });
      })
      .catch(function () {
        // negado, no soportado en este contexto (p. ej. pestaña ya oculta), etc.
      });
  }

  function releaseWakeLock() {
    if (wakeLock) {
      wakeLock.release().catch(function () {});
      wakeLock = null;
    }
  }

  function stopMovementBeacon() {
    if (state.movement && state.movement.timer) {
      clearInterval(state.movement.timer);
    }
    state.movement = null;
    movementToggle.checked = false;
    releaseWakeLock();
  }

  function movementTick(deviceId) {
    // si ya hay un tick esperando el GPS/la red, no se acumulan
    if (state.movement && state.movement.inFlight) return;
    if (state.movement) state.movement.inFlight = true;
    getBrowserLocation(10000)
      .then(function (loc) {
        return apiFetch("/api/v1/devices/" + encodeURIComponent(deviceId) + "/location", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            lat: loc.lat,
            lon: loc.lon,
            gps_accuracy_m: loc.accuracy,
            gps_source: "browser-geolocation",
          }),
        });
      })
      .then(function () {
        if (state.selectedDeviceId !== deviceId) return; // se cambió de vista mientras tanto
        movementStatusEl.textContent = "última ubicación mandada: " + new Date().toLocaleTimeString();
      })
      .catch(function (err) {
        if (state.selectedDeviceId !== deviceId) return;
        movementStatusEl.textContent = "último intento falló: " + err.message;
      })
      .finally(function () {
        if (state.movement) state.movement.inFlight = false;
      });
  }

  movementToggle.addEventListener("change", function () {
    var deviceId = state.selectedDeviceId;
    if (!deviceId) return;
    if (movementToggle.checked) {
      movementStatusEl.textContent = "mandando la primera ubicación...";
      state.movement = { deviceId: deviceId, inFlight: false };
      requestWakeLock();
      movementTick(deviceId);
      state.movement.timer = setInterval(function () {
        movementTick(deviceId);
      }, MOVEMENT_INTERVAL_MS);
    } else {
      stopMovementBeacon();
      movementStatusEl.textContent = "";
    }
  });

  // ---------------------------------------------------------------- prueba de velocidad

  function stopSpeedtestPoll() {
    if (state.speedtest && state.speedtest.pollTimer) {
      clearInterval(state.speedtest.pollTimer);
    }
    state.speedtest = null;
  }

  runSpeedtestBtn.addEventListener("click", function () {
    var deviceId = state.selectedDeviceId;
    if (!deviceId) return;
    runSpeedtestBtn.disabled = true;
    speedtestStatusEl.textContent = "obteniendo ubicación del navegador...";
    clearBanner(detailErrorEl);

    getBrowserLocation()
      .catch(function (err) {
        // sin ubicación no se cancela la prueba, solo queda sin coordenadas
        // (igual que cuando la dispara el celular sin señal de GPS)
        speedtestStatusEl.textContent = "sin ubicación (" + err.message + "), sigo sin ella...";
        return null;
      })
      .then(function (loc) {
        speedtestStatusEl.textContent = "creando comando...";
        var body = { device_id: deviceId, type: "run_speedtest", duration_s: 8, requested_by: "dashboard" };
        if (loc) {
          body.lat = loc.lat;
          body.lon = loc.lon;
          body.gps_accuracy_m = loc.accuracy;
          body.gps_source = "browser-geolocation";
        }
        return apiFetch("/api/v1/commands", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
      })
      .then(function (res) {
        var commandId = res && res.id;
        if (commandId === undefined || commandId === null) {
          throw { kind: "http", message: "El backend no devolvió el id del comando." };
        }
        var startedAt = Date.now();
        var deadline = startedAt + SPEEDTEST_TIMEOUT_MS;
        state.speedtest = { commandId: commandId, deadline: deadline, startedAt: startedAt };
        speedtestStatusEl.textContent = "corriendo... 0s";
        state.speedtest.pollTimer = setInterval(function () {
          pollSpeedtest(deviceId, commandId, deadline, startedAt);
        }, SPEEDTEST_POLL_MS);
      })
      .catch(function (err) {
        runSpeedtestBtn.disabled = false;
        speedtestStatusEl.textContent = "";
        showBanner(detailErrorEl, "No se pudo crear el comando: " + err.message);
      });
  });

  // Ubicación al TERMINAR la prueba (la de arranque ya se manda al crear el
  // comando, ver el listener de runSpeedtestBtn más abajo). Recarga el
  // detalle si el POST tiene éxito y seguís viendo el mismo equipo, para que
  // el mapa/las tablas reflejen la ubicación de fin sin esperar al próximo
  // refresco automático.
  function sendSpeedtestEndLocation(deviceId, commandId) {
    getBrowserLocation(10000)
      .then(function (loc) {
        return apiFetch("/api/v1/commands/" + encodeURIComponent(commandId) + "/end_location", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            lat: loc.lat,
            lon: loc.lon,
            gps_accuracy_m: loc.accuracy,
            gps_source: "browser-geolocation",
          }),
        });
      })
      .then(function () {
        if (state.selectedDeviceId === deviceId) loadDetail(deviceId);
      })
      .catch(function () {
        // sin ubicación disponible o el POST falló: la medición queda solo
        // con la ubicación de arranque, no es un error que valga un banner.
      });
  }

  function pollSpeedtest(deviceId, commandId, deadline, startedAt) {
    // Si el usuario navegó a otro dispositivo o volvió a la lista, este poll
    // ya no aplica.
    if (!state.speedtest || state.speedtest.commandId !== commandId || state.selectedDeviceId !== deviceId) {
      return;
    }
    if (Date.now() > deadline) {
      stopSpeedtestPoll();
      runSpeedtestBtn.disabled = false;
      speedtestStatusEl.textContent = "";
      showBanner(detailErrorEl, "La prueba no terminó dentro de " + SPEEDTEST_TIMEOUT_MS / 1000 + "s.");
      return;
    }
    var elapsedS = Math.round((Date.now() - startedAt) / 1000);
    speedtestStatusEl.textContent = "corriendo... " + elapsedS + "s";
    apiFetch("/api/v1/commands?device_id=" + encodeURIComponent(deviceId) + "&limit=20")
      .then(function (cmds) {
        var found = (cmds || []).filter(function (c) {
          return c.id === commandId;
        })[0];
        if (!found) return; // todavía no aparece o quedó fuera del limit; seguimos esperando
        if (found.status === "done" || found.status === "failed") {
          stopSpeedtestPoll();
          runSpeedtestBtn.disabled = false;
          if (found.status === "done") {
            // el resultado real (↓/↑ Mbps) vive en la medición que generó el
            // comando, no en el comando mismo -- lo traemos aparte para
            // mostrarlo de inmediato junto al botón, sin que el usuario tenga
            // que ir a buscarlo en la tabla de abajo.
            apiFetch("/api/v1/measurements?device_id=" + encodeURIComponent(deviceId) + "&limit=1")
              .then(function (rows) {
                var m = (rows || [])[0];
                if (m && (m.down_mbps !== undefined || m.up_mbps !== undefined)) {
                  speedtestStatusEl.textContent =
                    "listo: ↓" + fmtNum(m.down_mbps) + " Mbps ↑" + fmtNum(m.up_mbps) + " Mbps";
                } else {
                  speedtestStatusEl.textContent = "prueba completada";
                }
              })
              .catch(function () {
                speedtestStatusEl.textContent = "prueba completada";
              });
            // Ubicación de FIN: la de arranque ya viajó al crear el comando;
            // esta segunda toma (recién ahora, con el resultado ya
            // confirmado) sirve para saber si el equipo se movió durante los
            // segundos que duró la prueba. No bloquea nada de lo de arriba
            // ni pisa la ubicación de arranque -- va aparte, a
            // lat_end/lon_end. Best-effort: sin ubicación o con el POST
            // fallando, la medición se queda solo con la de arranque.
            sendSpeedtestEndLocation(deviceId, commandId);
          } else {
            speedtestStatusEl.textContent = "prueba fallida" + (found.error ? ": " + found.error : "");
          }
          loadDetail(deviceId);
          refreshDevices(); // que el panel principal también quede al día de una vez
        }
      })
      .catch(function () {
        // error de red pasajero durante el polling: se reintenta en el
        // próximo tick, no se corta el timeout por un solo fallo.
      });
  }

  // ---------------------------------------------------------------- prueba de velocidad (navegador)
  //
  // El botón de arriba corre la prueba EN el router: su CPU (ARM Cortex-A7,
  // sin aceleración de cifrado) queda muy por debajo de lo que el módem puede
  // dar en 5G, porque ese tráfico nunca pasa por el camino de NAT acelerado
  // por hardware del equipo (nf_flow_table_hw/xt_FLOWOFFLOAD, confirmado con
  // Diego 2026-09-18) -- ese camino solo aplica al tráfico que el router
  // REENVÍA entre LAN y WAN, no al que genera su propia CPU. Esta prueba en
  // cambio corre en el navegador de quien la dispare (celular o PC conectado
  // al router): mide contra los mismos endpoints de descarga/subida, pero el
  // tráfico sí atraviesa el router como tráfico reenviado, así que refleja la
  // velocidad real.

  function round2(n) {
    return Math.round(n * 100) / 100;
  }

  // timedBrowserDownload: lee /api/v1/speedtest/download por durationMs,
  // contando bytes recibidos (sin guardarlos), y corta la conexión al
  // cumplirse el tiempo -- mismo idiom que timedDownload() en el agente Go
  // del router, pero corriendo en el navegador.
  function timedBrowserDownload(durationMs) {
    return apiRawFetch("/api/v1/speedtest/download?bytes=500000000").then(function (res) {
      if (!res.ok) throw new Error("HTTP " + res.status + " en la descarga de prueba");
      var reader = res.body.getReader();
      var bytes = 0;
      var startedAt = Date.now();
      var deadline = startedAt + durationMs;
      function pump() {
        if (Date.now() >= deadline) {
          reader.cancel().catch(function () {});
          return bytes;
        }
        return reader.read().then(function (result) {
          if (result.done) return bytes;
          bytes += result.value.length;
          return pump();
        });
      }
      return pump().then(function (finalBytes) {
        var elapsedS = (Date.now() - startedAt) / 1000;
        return { bytes: finalBytes, elapsedS: elapsedS, mbps: round2((finalBytes * 8) / elapsedS / 1e6) };
      });
    });
  }

  // sizeBrowserUploadBytes: no hay forma de saber la velocidad de subida de
  // antemano, así que se estima como una fracción de la bajada recién medida
  // (5G suele ser bastante asimétrico) y se acota para no quedarse corto
  // (mucho ruido de RTT en cuerpos chicos) ni trabarse mandando de más en una
  // conexión lenta.
  function sizeBrowserUploadBytes(downMbps) {
    var assumedUpMbps = downMbps > 0 ? downMbps / 4 : 20;
    var bytes = Math.round(((assumedUpMbps * 1e6) / 8) * BROWSER_UPLOAD_TARGET_S);
    return Math.max(BROWSER_UPLOAD_MIN_BYTES, Math.min(BROWSER_UPLOAD_MAX_BYTES, bytes));
  }

  // timedBrowserUpload: manda `bytes` en ceros a /api/v1/speedtest/upload y
  // mide el tiempo real que tardó el POST completo (a diferencia de la
  // descarga, acá no hay forma simple de cortar a mitad de subida desde el
  // navegador, así que se dimensiona el cuerpo en sizeBrowserUploadBytes en
  // vez de cortar por tiempo).
  function timedBrowserUpload(bytes) {
    var body = new Uint8Array(bytes); // ya viene en ceros, no hace falta llenarlo
    var startedAt = Date.now();
    return apiFetch("/api/v1/speedtest/upload", { method: "POST", body: body }).then(function () {
      var elapsedS = (Date.now() - startedAt) / 1000;
      return { bytes: bytes, elapsedS: elapsedS, mbps: round2((bytes * 8) / elapsedS / 1e6) };
    });
  }

  runBrowserSpeedtestBtn.addEventListener("click", function () {
    var deviceId = state.selectedDeviceId;
    if (!deviceId) return;
    runBrowserSpeedtestBtn.disabled = true;
    clearBanner(detailErrorEl);
    browserSpeedtestStatusEl.textContent = "obteniendo ubicación del navegador...";

    var loc = null;
    var downResult = null;
    var upResult = null;

    getBrowserLocation()
      .then(function (l) {
        loc = l;
      })
      .catch(function () {
        // sin ubicación no se cancela la prueba, solo queda sin coordenadas
      })
      .then(function () {
        browserSpeedtestStatusEl.textContent = "bajando datos de prueba...";
        return timedBrowserDownload(BROWSER_DOWNLOAD_MS);
      })
      .then(function (down) {
        downResult = down;
        browserSpeedtestStatusEl.textContent = "↓ " + down.mbps + " Mbps, subiendo datos de prueba...";
        return timedBrowserUpload(sizeBrowserUploadBytes(down.mbps));
      })
      .then(function (up) {
        upResult = up;
        browserSpeedtestStatusEl.textContent =
          "↓ " + downResult.mbps + " Mbps / ↑ " + up.mbps + " Mbps, guardando...";
        var body = {
          device_id: deviceId,
          source: "browser",
          tag: "browser-speedtest",
          ts: new Date().toISOString(),
          down_mbps: downResult.mbps,
          up_mbps: up.mbps,
          note: "prueba real desde el navegador (tráfico vía NAT por hardware del router, no limitada por su CPU)",
        };
        if (loc) {
          body.lat = loc.lat;
          body.lon = loc.lon;
          body.gps_accuracy_m = loc.accuracy;
          body.gps_source = "browser-geolocation";
        }
        return apiFetch("/api/v1/measurements", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
      })
      .then(function () {
        browserSpeedtestStatusEl.textContent =
          "listo: ↓" + downResult.mbps + " Mbps ↑" + upResult.mbps + " Mbps";
        runBrowserSpeedtestBtn.disabled = false;
        if (state.selectedDeviceId === deviceId) loadDetail(deviceId);
        refreshDevices();
      })
      .catch(function (err) {
        runBrowserSpeedtestBtn.disabled = false;
        browserSpeedtestStatusEl.textContent = "";
        showBanner(detailErrorEl, "Prueba desde el navegador falló: " + (err && err.message ? err.message : err));
      });
  });

  // ---------------------------------------------------------------- pestaña en segundo plano
  //
  // Los navegadores frenan (o directo pausan) setInterval/setTimeout cuando la
  // pestaña no está visible, para ahorrar batería -- esto SÍ afecta tanto el
  // sondeo de la prueba de velocidad como el modo en movimiento; no hay forma
  // de evitarlo desde esta página (haría falta un Service Worker con
  // notificaciones push, otro alcance). En vez de fallar en silencio, se
  // avisa apenas la pestaña deja de estar visible.
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) {
      if (state.movement) {
        movementStatusEl.textContent =
          "⚠ la pestaña pasó a segundo plano: el navegador puede pausar el envío hasta que vuelvas a verla.";
      }
      if (state.speedtest) {
        speedtestStatusEl.textContent = "⚠ pestaña en segundo plano: puede que el resultado tarde en aparecer.";
      }
      return;
    }
    // el sistema libera el Wake Lock solo al ocultar la pestaña -- si el modo
    // en movimiento sigue prendido, hay que volver a pedirlo al volver.
    if (state.movement) requestWakeLock();
  });

  // ---------------------------------------------------------------- arranque

  state.apiKey = loadKey();
  keyInput.value = state.apiKey;
  if (state.apiKey) {
    testKeyAndLoad();
  } else {
    setKeyStatus("unknown", "sin probar");
  }
})();
