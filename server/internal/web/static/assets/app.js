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
  // Prueba contra un CDN público (Cloudflare) en vez de contra este backend.
  // Sirve para dos cosas: (a) el VPS de la oficina tiene un enlace de subida
  // mucho más chico que lo que un 5G puede bajar, así que la prueba propia
  // topea por el servidor y no por la red del equipo; (b) cuando dos equipos
  // prueban a la vez contra el backend se reparten ESE enlace y las dos
  // mediciones salen bajas -- contra el CDN cada uno mide su propia red.
  // Se usa Cloudflare y no fast.com porque los endpoints de Netflix no mandan
  // cabeceras CORS (verificado 2026-09-19: api.fast.com responde sin
  // Access-Control-Allow-Origin), así que el navegador bloquea la lectura;
  // speed.cloudflare.com sí las manda (Access-Control-Allow-Origin: *) tanto
  // en __down como en __up.
  var CF_DOWN_URL = "https://speed.cloudflare.com/__down?bytes=";
  var CF_UP_URL = "https://speed.cloudflare.com/__up";
  var CF_DOWN_BYTES = 300 * 1e6; // tope del cuerpo; se corta por tiempo igual
  var CF_PING_SAMPLES = 5;
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
    // Última respuesta de GET /api/v1/netinfo: qué ve el backend de la
    // conexión de ESTE navegador (IP pública, operador, y si coincide con la
    // del equipo). Es una previsualización -- la clasificación que queda
    // guardada la calcula el servidor con la IP de la ventana de medición.
    netinfo: null,
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
              // La zona de Cloudflare está en security_level = medium: una IP
              // de CGNAT móvil (o de un relay) con mala reputación puede
              // recibir un managed challenge, y entonces acá llega el HTML de
              // la página de verificación donde esperábamos JSON -- justo en
              // la ruta "otra red" que estamos habilitando. Sin esto, el
              // mensaje de error sería un volcado de HTML ilegible.
              if (body && body.charAt(0) === "<") {
                return Promise.reject({
                  kind: "http",
                  message:
                    "Cloudflare bloqueó la petición (posible verificación por la red desde la que estás midiendo). Probá de nuevo o cambiá de red.",
                });
              }
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

  // concurrentFromResponse: el backend cuenta cuántas pruebas de velocidad
  // están corriendo contra él en ese instante (X-Speedtest-Concurrent). Si es
  // >1, dos equipos se están repartiendo el mismo enlace del servidor y el
  // número medido no es el techo de ninguno de los dos.
  function concurrentFromResponse(res) {
    var v = parseInt(res.headers.get("X-Speedtest-Concurrent") || "", 10);
    return isNaN(v) ? 0 : v;
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
        applyHash(); // #/device/<id> -> abre ese equipo directo (dos equipos = dos pestañas)
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
  var runCfSpeedtestBtn = document.getElementById("run-cf-speedtest");
  var cfSpeedtestStatusEl = document.getElementById("cf-speedtest-status");
  var movementToggle = document.getElementById("movement-toggle");
  var movementStatusEl = document.getElementById("movement-status");
  var operatorFilterEl = document.getElementById("operator-filter");
  var routeFilterEl = document.getElementById("route-filter");
  var bandSummaryEl = document.getElementById("band-summary");

  backBtn.addEventListener("click", function () {
    goToList(false);
  });

  // Ruteo por hash (#/device/<id>): permite abrir directo la página de un
  // equipo en otra pestaña/otro celular, y que sobreviva a un refresh. Es lo
  // que hace posible seguir dos equipos EN PARALELO: cada navegador (o cada
  // pestaña) queda fijado a su propio device_id, porque el estado de esta
  // página es uno solo por pestaña.
  function deviceIdFromHash() {
    var m = /^#\/device\/(.+)$/.exec(location.hash || "");
    if (!m) return null;
    try {
      return decodeURIComponent(m[1]);
    } catch (e) {
      return m[1];
    }
  }

  function applyHash() {
    var id = deviceIdFromHash();
    if (id) {
      if (state.view !== "detail" || state.selectedDeviceId !== id) showDetail(id, true);
      return;
    }
    if (state.view === "detail") goToList(true);
  }

  window.addEventListener("hashchange", applyHash);

  function goToList(fromHash) {
    stopSpeedtestPoll();
    stopMovementBeacon();
    showView("list");
    renderDeviceList(); // lo que ya había, al instante
    refreshDevices(); // y de inmediato pide lo fresco (por si acabas de correr una prueba)
    if (!fromHash && deviceIdFromHash()) location.hash = "";
  }

  function showDetail(deviceId, fromHash) {
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
    routeFilterEl.value = "";
    bandSummaryEl.innerHTML = "";
    phoneSummaryEl.innerHTML = "";
    phoneLogTbody.innerHTML = "";
    clearBanner(detailErrorEl);
    browserSpeedtestStatusEl.textContent = "";
    cfSpeedtestStatusEl.textContent = "";
    runBrowserSpeedtestBtn.disabled = false;
    runCfSpeedtestBtn.disabled = false;
    showView("detail");
    if (!fromHash) location.hash = "#/device/" + encodeURIComponent(deviceId);
    loadDetail(deviceId);
    // Qué red está usando ESTE navegador ahora mismo, comparada con la del
    // equipo. Va aparte de loadDetail() a propósito: si falla, no puede
    // romper la carga del detalle (refreshNetInfo nunca rechaza).
    refreshNetInfo(deviceId);
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
    loadPhoneLog(deviceId);
  }

  // ---------------------------------------------------------------- celular acompañante (batería y red)

  var phoneSummaryEl = document.getElementById("phone-summary");
  var phoneLogTbody = document.getElementById("phone-log-tbody");
  var PHONE_LOG_LIMIT = 300;

  // Aparte de loadDetail a propósito: es información de apoyo, y si falla (un
  // backend viejo sin el endpoint) no puede tumbar el resto del detalle.
  function loadPhoneLog(deviceId) {
    apiFetch("/api/v1/devices/" + encodeURIComponent(deviceId) + "/phone_log?limit=" + PHONE_LOG_LIMIT)
      .then(function (rows) {
        if (state.selectedDeviceId !== deviceId) return;
        renderPhoneLog(rows || []);
      })
      .catch(function () {
        if (state.selectedDeviceId !== deviceId) return;
        phoneSummaryEl.innerHTML = '<span class="muted">No se pudo leer el historial del celular.</span>';
        phoneLogTbody.innerHTML = "";
      });
  }

  var BATTERY_STATUS_ES = {
    charging: "cargando",
    discharging: "descargando",
    full: "llena",
    not_charging: "conectado, sin cargar",
  };
  var PLUGGED_ES = { ac: "cargador", usb: "USB", wireless: "inalámbrico", dock: "base", none: "nada" };
  var NET_ES = { ethernet: "cable (Ethernet)", wifi: "WiFi", cellular: "datos móviles", vpn: "VPN", none: "sin red" };

  function batteryStatusEs(r) {
    if (r.battery_status) return BATTERY_STATUS_ES[r.battery_status] || r.battery_status;
    if (r.charging === true) return "cargando";
    if (r.charging === false) return "descargando";
    return "—";
  }

  // drainPerHour: %/h en el tramo más reciente en que la batería estuvo
  // descargándose sin interrupción. Con menos de 20 min de tramo el número es
  // ruido (la batería se reporta en pasos de 1%), así que no se muestra.
  function drainPerHour(rows) {
    var newest = null;
    var oldest = null;
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      if (r.battery_pct === null || r.battery_pct === undefined || r.battery_status !== "discharging") break;
      if (!newest) newest = r;
      oldest = r;
    }
    if (!newest || oldest === newest) return null;
    var hours = (new Date(newest.received_at) - new Date(oldest.received_at)) / 3600000;
    if (hours < 20 / 60) return null;
    return { rate: (oldest.battery_pct - newest.battery_pct) / hours, hours: hours };
  }

  function renderPhoneLog(rows) {
    var withBattery = rows.filter(function (r) {
      return r.battery_pct !== null && r.battery_pct !== undefined;
    });
    if (!withBattery.length) {
      phoneSummaryEl.innerHTML = rows.length
        ? '<span class="muted">El celular manda ubicación pero no batería: actualizá la app en el celular.</span>'
        : '<span class="muted">Todavía no hay datos del celular (activá el modo en movimiento en la app).</span>';
    } else {
      var last = withBattery[0];
      var html =
        '<span class="summary-label">Batería:</span> <strong>' +
        last.battery_pct +
        "%</strong> · " +
        escapeHtml(batteryStatusEs(last));
      if (last.plugged && last.plugged !== "none") html += " (conectado a " + escapeHtml(PLUGGED_ES[last.plugged] || last.plugged) + ")";
      if (last.net_type) html += ' · <span class="summary-label">red:</span> ' + escapeHtml(NET_ES[last.net_type] || last.net_type);
      if (last.battery_temp_c !== null && last.battery_temp_c !== undefined) html += " · " + fmtNum(last.battery_temp_c, " °C", 1);
      html += ' · <span class="muted">' + fmtDate(last.received_at) + "</span>";
      var d = drainPerHour(withBattery);
      if (d) {
        html +=
          '<br><span class="summary-label">Consumo:</span> ' +
          fmtNum(d.rate, " %/h", 1) +
          ' <span class="muted">(últimas ' +
          fmtNum(d.hours, " h", 1) +
          " descargándose" +
          (d.rate > 0 ? ", unas " + fmtNum(last.battery_pct / d.rate, " h", 1) + " hasta agotarse" : "") +
          ")</span>";
      }
      if (last.battery_status === "discharging" && last.plugged === "usb") {
        html +=
          '<br><span class="muted">⚠ Está conectado por USB pero descargándose: el celular alimenta el ' +
          "adaptador (p. ej. USB-Ethernet) en vez de cargarse.</span>";
      }
      phoneSummaryEl.innerHTML = html;
    }

    phoneLogTbody.innerHTML = "";
    if (!rows.length) {
      phoneLogTbody.innerHTML = '<tr><td colspan="7" class="muted">Sin datos del celular todavía.</td></tr>';
      return;
    }
    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      tr.innerHTML =
        '<td data-label="Fecha">' +
        fmtDate(r.received_at) +
        '</td><td data-label="Batería">' +
        (r.battery_pct !== null && r.battery_pct !== undefined ? r.battery_pct + "%" : "—") +
        '</td><td data-label="Estado">' +
        escapeHtml(batteryStatusEs(r)) +
        '</td><td data-label="Conectado a">' +
        escapeHtml(r.plugged ? PLUGGED_ES[r.plugged] || r.plugged : "—") +
        '</td><td data-label="Red del celular">' +
        escapeHtml(r.net_type ? NET_ES[r.net_type] || r.net_type : "—") +
        '</td><td data-label="Temp.">' +
        fmtNum(r.battery_temp_c, " °C", 1) +
        '</td><td data-label="Ubicación">' +
        mapsLink(r.lat, r.lon, r.gps_accuracy_m) +
        "</td>";
      phoneLogTbody.appendChild(tr);
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

  // Filtro por ruta de salida (la calcula el servidor, ver routeBadge()).
  // "none" = las mediciones anteriores a esta función, que quedaron sin
  // clasificar: se pueden aislar para auditar el histórico, pero NO se
  // presentan como confirmadas por el router en ningún lado.
  function filterByRoute(rows) {
    var v = routeFilterEl.value;
    if (!v) return rows;
    if (v === "none") {
      return rows.filter(function (r) {
        return !r.net_route;
      });
    }
    return rows.filter(function (r) {
      return r.net_route === v;
    });
  }

  function applyDetailFilter() {
    // El filtro de salida se aplica SOLO a las mediciones (los heartbeats los
    // manda el propio equipo, siempre por su propia red) y ANTES del resumen
    // de bandas y del mapa: si no, el mapa y el resumen seguirían mostrando
    // como datos del equipo puntos medidos por otra red.
    var measurements = filterByRoute(filterByOperator(state.detail.measurements || []));
    var heartbeats = filterByOperator(state.detail.heartbeats || []);
    renderMeasurements(measurements);
    renderHeartbeats(heartbeats);
    renderBandSummary(measurements);
    renderMap(measurements, heartbeats);
  }

  operatorFilterEl.addEventListener("change", applyDetailFilter);
  routeFilterEl.addEventListener("change", applyDetailFilter);

  // ---------------------------------------------------------------- resumen de bandas probadas

  // NR_DESCONOCIDA: marcador explícito para cuando se sabe que hubo agregación
  // NR pero no qué banda. Nunca se rellena con un número inventado.
  var NR_DESCONOCIDA = "NR sin identificar";

  // isNSA: el agente del router manda rat="5G-NSA" (sys_mode 5 = ENDC), pero
  // otros clientes (notion5g.py, navegador) pueden mandar otras grafías, así
  // que se acepta cualquier variante que mencione NSA o ENDC.
  function isNSA(rat) {
    if (!rat) return false;
    var r = String(rat).toUpperCase().replace(/[-_\s]/g, "");
    return r.indexOf("NSA") !== -1 || r.indexOf("ENDC") !== -1;
  }

  // byBandNumber: "B2" antes que "B28" (el orden alfabético los da al revés).
  function byBandNumber(a, b) {
    var na = parseInt(String(a).replace(/^[A-Za-z]+/, ""), 10);
    var nb = parseInt(String(b).replace(/^[A-Za-z]+/, ""), 10);
    if (isNaN(na) || isNaN(nb)) return a < b ? -1 : a > b ? 1 : 0;
    return na - nb;
  }

  function renderBandSummary(measurements) {
    if (!measurements.length) {
      bandSummaryEl.innerHTML =
        '<span class="muted">Todavía no hay mediciones (con este filtro) para resumir bandas probadas.</span>';
      return;
    }
    var lte = {};
    var nr = {};
    var sawNR = false;
    // combos: en Colombia el 5G es siempre NSA/ENDC (ver cmd/routeragent/main.go,
    // comentario de sys_mode), o sea que el equipo va agregado a DOS portadoras a
    // la vez: un ancla LTE más una NR. Listar las bandas por separado pierde
    // justamente el dato que importa en campo -- cuál ancla se usó con cuál NR --
    // así que además de los dos conjuntos se cuenta cada PAR observado.
    var combos = {};
    measurements.forEach(function (m) {
      var hasLte = m.band_lte !== null && m.band_lte !== undefined;
      var hasNR = m.nr_band !== null && m.nr_band !== undefined;
      if (hasLte) lte["B" + m.band_lte] = true;
      if (hasNR) {
        nr["n" + m.nr_band] = true;
        sawNR = true;
      }
      if (!isNSA(m.rat)) return;
      sawNR = true;
      // El ancla puede faltar en una medición suelta; se deja explícito en vez
      // de inventar un par.
      var anchor = hasLte ? "B" + m.band_lte : "ancla LTE sin identificar";
      var key = anchor + " + " + (hasNR ? "n" + m.nr_band : NR_DESCONOCIDA);
      combos[key] = (combos[key] || 0) + 1;
    });
    var lteList = Object.keys(lte).sort(byBandNumber);
    var nrList = Object.keys(nr).sort(byBandNumber);
    var comboList = Object.keys(combos).sort();
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
    html += '<br><span class="summary-label">Combinaciones NSA medidas:</span>';
    if (comboList.length) {
      html += comboList
        .map(function (c) {
          var incompleta = c.indexOf(NR_DESCONOCIDA) !== -1 || c.indexOf("ancla LTE sin identificar") !== -1;
          return (
            '<span class="band-chip' +
            (incompleta ? "" : " band-nr") +
            '" title="' +
            combos[c] +
            ' medición(es)">' +
            escapeHtml(c) +
            ' <span class="muted">×' +
            combos[c] +
            "</span></span>"
          );
        })
        .join("");
      if (comboList.some(function (c) { return c.indexOf(NR_DESCONOCIDA) !== -1; })) {
        html +=
          '<br><span class="muted">El equipo reportó 5G NSA pero el agente del router todavía no ' +
          "manda la banda NR: hoy solo lee el bloque <code>lte</code> de <code>get_zcainfo</code>. " +
          "Hasta que se actualice el agente, la mitad NR de la combinación no se puede saber.</span>";
      }
    } else if (sawNR) {
      html += '<span class="muted">se vio 5G, pero ninguna medición quedó marcada como NSA</span>';
    } else {
      html += '<span class="muted">ninguna todavía</span>';
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
      measurementsTbody.innerHTML = '<tr><td colspan="9" class="muted">Sin mediciones todavía.</td></tr>';
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
        '</td><td data-label="Salida">' +
        routeBadge(m) +
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

  // ---------------------------------------------------------------- red de salida (¿por dónde estoy midiendo?)
  //
  // El problema: esta página se puede abrir desde un celular conectado al WiFi
  // del router (lo que queremos medir) o desde el MISMO celular saliendo por su
  // propia SIM. Las dos pruebas se guardaban igual, atribuidas al router, y
  // después no había forma de distinguirlas.
  //
  // Quien decide es el servidor: compara la IP pública que ve en esta petición
  // contra la última IP pública desde la que vio salir al equipo (la aprende de
  // los heartbeats que el agente ya manda). Acá abajo solo se aportan PISTAS
  // locales y se muestra el resultado.

  // netHint: PISTA local sobre la interfaz de red activa. NO es autoritativa y
  // no puede serlo:
  //  - iOS Safari (y la PWA instalada, mismo WebKit) no tiene navigator.connection.
  //  - Firefox Android >= 99 tampoco (la API se removió).
  //  - Chrome de escritorio expone el objeto pero .type solo funciona en ChromeOS:
  //    por eso NO alcanza con "if (navigator.connection)", hay que mirar la
  //    plataforma y el VALOR.
  //  - "wifi" NO dice CUÁL wifi: el del router, el de la oficina y el hotspot de
  //    otro celular devuelven los tres lo mismo.
  // Quien decide es la IP pública vista por el backend; esto solo suma o resta.
  function netHint() {
    var c = navigator.connection || navigator.mozConnection || navigator.webkitConnection;
    if (!c) return { api: "unavailable", type: null };
    var extra = {
      effective_type: c.effectiveType || null,
      // OJO: downlink viene topeado a 10 Mbps y rtt a 3000 ms por
      // anti-fingerprinting. Van acá adentro como pista y NUNCA se muestran al
      // lado de los Mbps reales: un 5G de 300 Mbps aparecería como "10 Mbps".
      rtt_ms: typeof c.rtt === "number" ? c.rtt : null,
      downlink_capped_mbps: typeof c.downlink === "number" ? c.downlink : null,
      save_data: !!c.saveData,
    };
    var mobile = /Android|iPhone|iPad|iPod|Mobile/i.test(navigator.userAgent || "");
    var t = c.type || null;
    if (!mobile || t === "unknown" || t === "other" || t === "none" || !t) {
      extra.api = "present-untrusted";
      extra.type = null;
      return extra;
    }
    extra.api = "network-information";
    extra.type = t; // "wifi" | "cellular" | "ethernet" | ...
    return extra;
  }

  // watchNetChange: en Android el evento 'change' avisa si se pasó de WiFi a
  // datos EN MEDIO de la prueba (justo lo que hacen "Asistencia Wi-Fi" de iOS y
  // su equivalente de Android cuando la señal está débil, que es la condición
  // que estamos midiendo). Puede llegar tarde o no llegar con la pestaña en
  // segundo plano, así que es una señal más, no la única: el backend además
  // compara la IP del principio y la del final.
  function watchNetChange() {
    var c = navigator.connection || navigator.mozConnection || navigator.webkitConnection;
    var w = { changed: false, stop: function () {} };
    if (!c || typeof c.addEventListener !== "function") return w;
    function onChange() {
      w.changed = true;
    }
    c.addEventListener("change", onChange);
    w.stop = function () {
      try {
        c.removeEventListener("change", onChange);
      } catch (e) {}
    };
    return w;
  }

  // newTestId: ata las 3 peticiones de una misma prueba (download, upload y el
  // POST). Hoy no tienen nada en común y el `ts` lo pone el reloj del celular,
  // que puede estar mal, así que no sirve para correlacionar del lado servidor.
  function newTestId() {
    var hex = "0123456789abcdef",
      s = "",
      i;
    if (window.crypto && window.crypto.getRandomValues) {
      var buf = new Uint8Array(16);
      window.crypto.getRandomValues(buf);
      for (i = 0; i < buf.length; i++) s += hex[(buf[i] >> 4) & 15] + hex[buf[i] & 15];
      return s;
    }
    return "t" + Date.now().toString(16) + Math.random().toString(16).slice(2, 10);
  }

  // cfMetaIp: única cabecera de speed.cloudflare.com/__down que el navegador
  // puede leer de verdad. Verificado 2026-09-19: access-control-expose-headers
  // lista solo cf-meta-*; `asn`, `colo` y `country` llegan en la respuesta pero
  // NO están expuestos, así que desde JS son inalcanzables. Nunca rechaza.
  function cfMetaIp() {
    return fetch(CF_DOWN_URL + "0", { cache: "no-store" })
      .then(function (res) {
        return res.headers.get("cf-meta-ip") || null;
      })
      .catch(function () {
        return null;
      });
  }

  // ---------------------------------------------------------------- caja "Red de salida" + etiquetas

  var netBoxEl = document.getElementById("net-box");
  var netBadgeEl = document.getElementById("net-badge");
  var netDetailEl = document.getElementById("net-detail");
  var netRefreshBtn = document.getElementById("net-refresh");
  var netDeclaredEl = document.getElementById("net-declared");

  // Traducción de los códigos que devuelve el servidor. Se usan los MISMOS
  // textos en la caja de arriba y en el tooltip de cada fila de la tabla, para
  // que un "sin determinar" signifique siempre lo mismo en toda la página.
  var REASON_ES = {
    "ip-matches-router": "misma IP pública que el equipo",
    // La IP coincide, pero el equipo sale por el CGNAT del operador: ahí
    // también cae el celular midiendo por su propia SIM del mismo operador.
    // Es evidencia, no es prueba -- por eso sola queda en confianza baja.
    "ip-matches-router-cgnat-ambiguous":
      "misma IP pública que el equipo, pero es una IP compartida de CGNAT: no alcanza para confirmar",
    "same-v6-prefix": "mismo prefijo IPv6 que el equipo",
    "same-v6-prefix-64": "mismo enlace IPv6 (/64) que el equipo",
    "same-v6-prefix-carrier-pool":
      "prefijo IPv6 parecido al del equipo, pero es el pool del operador: no alcanza para confirmar",
    "ip-differs-from-router": "IP pública distinta a la del equipo",
    "same-v4-prefix-cgnat":
      "IP parecida a la del equipo, pero puede ser el CGNAT del operador: no alcanza para confirmar",
    "ip-family-mismatch":
      "el equipo reporta IPv4 y este navegador sale por IPv6 (o al revés): no son comparables",
    "no-router-reference": "todavía no se vio salir al equipo por ninguna IP (¿está reportando?)",
    "stale-router-reference": "el equipo no reporta hace más de 10 minutos: su IP pudo haber cambiado",
    "client-ip-absent": "no se pudo leer la IP pública de este navegador",
    "client-ip-invalid": "no se pudo leer la IP pública de este navegador",
    "client-ip-proxy-unverified": "no se pudo leer la IP pública de este navegador",
    "client-ip-forged-header": "la IP que llegó no vino por el túnel: no se puede creer",
    "client-ip-private": "la IP que llegó no es pública: algo se interpuso en el camino",
    "client-ip-cgnat": "la IP que llegó no es pública: algo se interpuso en el camino",
    "conflicting-signals": "las señales se contradicen (el navegador dice una cosa y la IP otra)",
    "network-changed-mid-test": "⚠ la red cambió durante la prueba: el resultado no es atribuible a ninguna",
    "relay-or-vpn": "estás saliendo por un relay o VPN (iCloud Private Relay, WARP…): no se puede saber la red real",
    "measured-by-device": "la corrió el propio equipo",
    "asn-differs": "operador de salida distinto al del equipo",
    // Caso en que la única señal fue la pista del navegador (Android diciendo
    // que la interfaz activa es la radio). El equipo bajo prueba es un AP
    // WiFi: si el celular está en datos, no salió por él.
    "hint-cellular": "el navegador dice que está usando los datos móviles, no un WiFi",
  };

  // Un código que no esté en la tabla se muestra tal cual: es feo, pero es
  // visible. Traducirlo a "sin determinar" a secas escondería que el backend
  // está mandando algo que esta página todavía no conoce.
  function reasonEs(reason) {
    if (!reason) return "";
    return REASON_ES[reason] || reason;
  }

  function confianzaEs(c) {
    if (c === "high") return "alta";
    if (c === "medium") return "media";
    if (c === "low") return "baja";
    return c || "";
  }

  function routeEs(route) {
    if (route === "router") return "vía router";
    if (route === "other-network") return "otra red";
    return "sin determinar";
  }

  // "AS14080 Telmex Colombia S.A." -- el número solo si no hay razón social.
  function asnText(asn, name) {
    if (!asn && !name) return "";
    if (asn && name) return "AS" + asn + " " + name;
    return name ? name : "AS" + asn;
  }

  // declaredWarning: lo declarado a mano NO manda (el servidor guarda lo
  // detectado), pero si no coinciden hay que decirlo: o la detección se
  // equivocó, o Diego cree estar midiendo por una red y está saliendo por otra
  // -- las dos cosas hay que verlas ANTES de medir, no descubrirlas después.
  function declaredWarning(info) {
    var declared = netDeclaredEl.value;
    if (!declared) return "";
    var detected = info && info.net_route ? info.net_route : null;
    if (!detected) return "";
    if (declared === "router" && detected === "router") return "";
    if ((declared === "phone-cellular" || declared === "other-wifi") && detected === "other-network") return "";
    var label = netDeclaredEl.options[netDeclaredEl.selectedIndex]
      ? netDeclaredEl.options[netDeclaredEl.selectedIndex].text
      : declared;
    return (
      ' ⚠ dijiste "' +
      label +
      '" pero la detección ve ' +
      routeEs(detected) +
      ". Se guarda la detectada; queda anotado que no coincidieron."
    );
  }

  function setNetBadge(cls, text) {
    netBadgeEl.className = "route-badge " + cls;
    netBadgeEl.textContent = text;
    // El borde de la caja entera acompaña al badge: en campo, con el sol en la
    // pantalla, hay que poder ver de reojo que se está por medir por otra red
    // sin ponerse a leer el detalle.
    var boxCls = "summary-box";
    if (cls === "route-router") boxCls += " net-router";
    else if (cls === "route-other") boxCls += " net-other";
    netBoxEl.className = boxCls;
  }

  function paintNetInfo(info) {
    if (!info || !info.net_route) {
      setNetBadge("route-unknown", "sin determinar");
      netDetailEl.textContent =
        "no se pudo detectar (la medición se va a guardar como sin determinar)" + declaredWarning(info);
      return;
    }
    var salida = asnText(info.asn, info.asn_name);
    var conf = info.confidence ? " · confianza " + confianzaEs(info.confidence) : "";
    if (info.net_route === "router") {
      if (info.confidence === "low") setNetBadge("route-maybe", "¿vía router? sin confirmar");
      else setNetBadge("route-router", "vía router");
      // El prefijo /24 (o /48) y no la IP completa: es lo mismo que el backend
      // persiste, y alcanza para reconocerla de un vistazo.
      var donde = [info.client_ip_prefix, salida].filter(function (x) {
        return !!x;
      });
      netDetailEl.textContent = (
        reasonEs(info.reason) +
        (donde.length ? " (" + donde.join(" · ") + ")" : "") +
        conf +
        declaredWarning(info)
      ).replace(/^ · /, "");
      return;
    }
    if (info.net_route === "other-network") {
      setNetBadge("route-other", "⚠ otra red");
      netDetailEl.textContent =
        "saliendo por " +
        (salida || "otra red") +
        (info.reason ? " — " + reasonEs(info.reason) : "") +
        conf +
        declaredWarning(info);
      return;
    }
    setNetBadge("route-unknown", "sin determinar");
    netDetailEl.textContent = (reasonEs(info.reason) + declaredWarning(info)).replace(/^ ⚠/, "⚠");
  }

  // refreshNetInfo: NUNCA rechaza y nunca muestra un banner de error rojo. Es
  // información de apoyo: si el endpoint falla (backend viejo, red caída a
  // mitad de camino), la prueba se puede correr igual y el servidor la va a
  // guardar como "sin determinar", que es la respuesta honesta.
  function refreshNetInfo(deviceId) {
    if (!deviceId) return Promise.resolve(null);
    var h = netHint();
    // El endpoint documenta cuatro valores para ?hint=: cellular | wifi |
    // unavailable | untrusted. netHint() usa "present-untrusted" adentro (es
    // el valor que viaja en net_hint.api al guardar la medición), así que acá
    // se traduce al nombre del contrato en vez de mandar uno inventado.
    var hint = h.type || (h.api === "unavailable" ? "unavailable" : "untrusted");
    setNetBadge("route-unknown", "detectando...");
    netDetailEl.textContent = "";
    return apiFetch(
      "/api/v1/netinfo?device_id=" + encodeURIComponent(deviceId) + "&hint=" + encodeURIComponent(hint)
    )
      .then(function (info) {
        if (state.selectedDeviceId !== deviceId) return null; // cambiaste de equipo mientras tanto
        state.netinfo = info || null;
        paintNetInfo(state.netinfo);
        return info;
      })
      .catch(function () {
        if (state.selectedDeviceId !== deviceId) return null;
        state.netinfo = null;
        paintNetInfo(null);
        return null;
      });
  }

  netRefreshBtn.addEventListener("click", function () {
    refreshNetInfo(state.selectedDeviceId);
  });

  // Cambiar lo declarado no vuelve a pedir nada al backend: solo re-evalúa si
  // coincide con lo ya detectado.
  netDeclaredEl.addEventListener("change", function () {
    if (state.view === "detail") paintNetInfo(state.netinfo);
  });

  // routeBadge: etiqueta la ruta de salida de cada medición. "Sin clasificar"
  // (—) son las anteriores a esta función: NO se pintan como "vía router",
  // porque eso legitimaría retroactivamente datos que nadie verificó.
  function routeBadge(m) {
    var net = m.net || {};
    var title = net.reason ? reasonEs(net.reason) : "";
    if (net.confidence) title += " · confianza " + confianzaEs(net.confidence);
    if (net.changed) title += " · ⚠ la red cambió durante la prueba";
    if (net.client_ip_status === "proxy-unverified")
      title += " · ⚠ no se pudo verificar el peer del túnel: toda clasificación queda en confianza baja";
    var asn = net.asn_name ? " " + net.asn_name : m.net_asn ? " AS" + m.net_asn : "";
    if (m.net_route === "router") {
      // Confianza baja = la única evidencia fue que la IP pública coincide, y
      // bajo CGNAT eso no distingue al router de la SIM del propio celular.
      // Se muestra, pero no de verde y con el signo de pregunta: el número
      // grande del equipo tampoco sale de estas (ver ListDeviceSummaries).
      if (net.confidence === "low")
        return '<span class="route-badge route-maybe" title="' + escapeHtml(title) + '">vía router?</span>';
      return '<span class="route-badge route-router" title="' + escapeHtml(title) + '">vía router</span>';
    }
    if (m.net_route === "other-network")
      return (
        '<span class="route-badge route-other" title="' +
        escapeHtml(title) +
        '">⚠ otra red' +
        escapeHtml(asn) +
        "</span>"
      );
    if (m.net_route === "unknown")
      return '<span class="route-badge route-unknown" title="' + escapeHtml(title) + '">sin determinar</span>';
    return '<span class="route-badge route-unknown" title="medición anterior a la detección de red">—</span>';
  }

  // routeSuffix: cómo terminó clasificada la prueba que se acaba de guardar.
  // El backend lo devuelve en el body del POST (net[], una entrada por ítem),
  // así que no hace falta recargar el detalle para saberlo.
  function routeSuffix(resp) {
    var n = resp && resp.net && resp.net[0];
    if (!n || !n.net_route) return " · salida sin determinar";
    if (n.net_route === "router") return " · vía router";
    if (n.net_route === "other-network") return " · ⚠ otra red";
    return " · salida sin determinar";
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
  //
  // testId viaja como query param para que el backend pueda anotar desde qué
  // IP pública salió ESTA descarga: es la IP del momento en que se estaba
  // midiendo de verdad, no la del POST de después (que puede ser otra si el
  // celular saltó de red al terminar).
  function timedBrowserDownload(durationMs, testId) {
    var url = "/api/v1/speedtest/download?bytes=500000000";
    if (testId) url += "&test_id=" + encodeURIComponent(testId);
    return apiRawFetch(url).then(function (res) {
      if (!res.ok) throw new Error("HTTP " + res.status + " en la descarga de prueba");
      var concurrent = concurrentFromResponse(res);
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
        return {
          bytes: finalBytes,
          elapsedS: elapsedS,
          mbps: round2((finalBytes * 8) / elapsedS / 1e6),
          concurrent: concurrent,
        };
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
  function timedBrowserUpload(bytes, testId) {
    var body = new Uint8Array(bytes); // ya viene en ceros, no hace falta llenarlo
    var startedAt = Date.now();
    // el test_id va en la query y no en el body: el body es el relleno de la
    // prueba (ceros), no un JSON donde se pueda meter nada.
    var url = "/api/v1/speedtest/upload";
    if (testId) url += "?test_id=" + encodeURIComponent(testId);
    return apiFetch(url, { method: "POST", body: body }).then(function (res) {
      var elapsedS = (Date.now() - startedAt) / 1000;
      // El contador de la descarga se lee al EMPEZAR: el primero de los dos
      // equipos arranca solo y vería 1 aunque el otro entre un segundo
      // después. La respuesta de la subida trae el contador del momento
      // final, así que con el máximo de los dos se detecta el solape venga de
      // donde venga.
      return {
        bytes: bytes,
        elapsedS: elapsedS,
        mbps: round2((bytes * 8) / elapsedS / 1e6),
        concurrent: res && res.concurrent ? res.concurrent : 0,
      };
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
    var concurrentSeen = 0;
    // Detección de la red de salida: el test_id ata las tres peticiones de esta
    // prueba del lado del servidor, el watch avisa si la red cambió a mitad, y
    // el hint es lo poco que el navegador sabe de su propia interfaz. Ninguno
    // de los tres puede hacer fallar la prueba.
    var testId = newTestId();
    var watch = watchNetChange();
    var hint = netHint();
    refreshNetInfo(deviceId); // sin bloquear: la prueba arranca igual

    getBrowserLocation()
      .then(function (l) {
        loc = l;
      })
      .catch(function () {
        // sin ubicación no se cancela la prueba, solo queda sin coordenadas
      })
      .then(function () {
        browserSpeedtestStatusEl.textContent = "bajando datos de prueba...";
        return timedBrowserDownload(BROWSER_DOWNLOAD_MS, testId);
      })
      .then(function (down) {
        downResult = down;
        browserSpeedtestStatusEl.textContent = "↓ " + down.mbps + " Mbps, subiendo datos de prueba...";
        return timedBrowserUpload(sizeBrowserUploadBytes(down.mbps), testId);
      })
      .then(function (up) {
        upResult = up;
        concurrentSeen = Math.max(downResult.concurrent || 0, up.concurrent || 0);
        browserSpeedtestStatusEl.textContent =
          "↓ " + downResult.mbps + " Mbps / ↑ " + up.mbps + " Mbps, guardando...";
        var body = {
          device_id: deviceId,
          source: "browser",
          tag: "browser-speedtest",
          ts: new Date().toISOString(),
          down_mbps: downResult.mbps,
          up_mbps: up.mbps,
          note:
            "prueba real desde el navegador (tráfico vía NAT por hardware del router, no limitada por su CPU)" +
            (concurrentSeen > 1
              ? " — ⚠ había " +
                concurrentSeen +
                " pruebas simultáneas contra este backend: el resultado está repartido entre ellas, no es el techo del equipo"
              : ""),
        };
        if (concurrentSeen > 1) body.concurrent_tests = concurrentSeen;
        if (loc) {
          body.lat = loc.lat;
          body.lon = loc.lon;
          body.gps_accuracy_m = loc.accuracy;
          body.gps_source = "browser-geolocation";
        }
        // Señales de red: el servidor las combina con la IP pública que él
        // mismo observó y decide. net_declared es solo lo que el usuario dijo
        // que iba a hacer; no puede ganarle a lo detectado.
        body.test_id = testId;
        body.net_hint = hint;
        body.net_changed_hint = watch.changed;
        if (netDeclaredEl.value) body.net_declared = netDeclaredEl.value;
        return apiFetch("/api/v1/measurements", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
      })
      .then(function (resp) {
        watch.stop();
        browserSpeedtestStatusEl.textContent =
          "listo: ↓" +
          downResult.mbps +
          " Mbps ↑" +
          upResult.mbps +
          " Mbps" +
          routeSuffix(resp) +
          (concurrentSeen > 1
            ? " — ⚠ " +
              concurrentSeen +
              " pruebas a la vez contra el backend: se repartieron el enlace del servidor. Usá la prueba contra Cloudflare para medir los dos equipos en paralelo."
            : "");
        runBrowserSpeedtestBtn.disabled = false;
        if (state.selectedDeviceId === deviceId) loadDetail(deviceId);
        refreshDevices();
      })
      .catch(function (err) {
        watch.stop();
        runBrowserSpeedtestBtn.disabled = false;
        browserSpeedtestStatusEl.textContent = "";
        showBanner(detailErrorEl, "Prueba desde el navegador falló: " + (err && err.message ? err.message : err));
      });
  });


  // ---------------------------------------------------------------- prueba contra Cloudflare (CDN público)
  //
  // Misma idea que la prueba del navegador de arriba, pero contra
  // speed.cloudflare.com en vez de contra este backend. Dos razones:
  //
  //   1. El VPS de la oficina no tiene enlace para saturar un 5G, así que la
  //      prueba propia mide el techo DEL SERVIDOR, no el del equipo.
  //   2. Cuando dos equipos prueban al mismo tiempo contra el backend se
  //      reparten ese mismo enlace y ambas mediciones salen bajas. Contra un
  //      CDN global cada navegador llega a un edge distinto y no compiten.
  //
  // fast.com no sirve para esto: sus endpoints no mandan cabeceras CORS, así
  // que el navegador no deja leer la respuesta desde esta página (verificado
  // 2026-09-19). Cloudflare es el equivalente que sí las manda.

  function cfPing() {
    var samples = [];
    function one(i) {
      if (i >= CF_PING_SAMPLES) return samples;
      var t0 = performance.now();
      return fetch(CF_DOWN_URL + "0", { cache: "no-store" })
        .then(function (res) {
          return res.arrayBuffer();
        })
        .then(function () {
          samples.push(performance.now() - t0);
          return one(i + 1);
        });
    }
    return Promise.resolve(one(0)).then(function (all) {
      if (!all.length) return { ping_ms: null, jitter_ms: null };
      var sorted = all.slice().sort(function (a, b) {
        return a - b;
      });
      var min = sorted[0];
      var max = sorted[sorted.length - 1];
      return { ping_ms: round2(min), jitter_ms: round2(max - min) };
    });
  }

  function cfTimedDownload(durationMs) {
    return fetch(CF_DOWN_URL + CF_DOWN_BYTES, { cache: "no-store" }).then(function (res) {
      if (!res.ok) throw new Error("HTTP " + res.status + " bajando de Cloudflare");
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

  // Sin cabeceras propias a propósito: así el POST queda como "simple request"
  // y el navegador no dispara un preflight OPTIONS (que __up no responde).
  function cfTimedUpload(bytes) {
    var body = new Uint8Array(bytes);
    var startedAt = Date.now();
    return fetch(CF_UP_URL, { method: "POST", body: body }).then(function (res) {
      if (!res.ok) throw new Error("HTTP " + res.status + " subiendo a Cloudflare");
      var elapsedS = (Date.now() - startedAt) / 1000;
      return { bytes: bytes, elapsedS: elapsedS, mbps: round2((bytes * 8) / elapsedS / 1e6) };
    });
  }

  runCfSpeedtestBtn.addEventListener("click", function () {
    var deviceId = state.selectedDeviceId;
    if (!deviceId) return;
    runCfSpeedtestBtn.disabled = true;
    clearBanner(detailErrorEl);
    cfSpeedtestStatusEl.textContent = "obteniendo ubicación del navegador...";

    var loc = null;
    var lat = null;
    var downResult = null;
    var upResult = null;
    var testId = newTestId();
    var watch = watchNetChange();
    var hint = netHint();
    // Estas dos IPs se comparan SOLO entre sí (las dos las ve el mismo host,
    // speed.cloudflare.com). Compararlas contra la IP que el backend ve en el
    // POST sería un falso positivo garantizado: son hosts distintos y con Happy
    // Eyeballs el mismo celular puede salir por IPv6 hacia uno y por IPv4 hacia
    // el otro en el mismo segundo. El servidor tiene la misma prohibición.
    var cfStart = null;
    var cfEnd = null;
    refreshNetInfo(deviceId); // sin bloquear

    getBrowserLocation()
      .then(function (l) {
        loc = l;
      })
      .catch(function () {
        // sin ubicación no se cancela la prueba, solo queda sin coordenadas
      })
      .then(function () {
        // Esta prueba no toca el backend, así que él no puede observar nada
        // durante la ventana de medición: la única evidencia del período la
        // aporta el navegador con estas dos lecturas.
        return cfMetaIp();
      })
      .then(function (ip) {
        cfStart = ip;
        cfSpeedtestStatusEl.textContent = "midiendo latencia...";
        return cfPing();
      })
      .then(function (p) {
        lat = p;
        cfSpeedtestStatusEl.textContent = "bajando desde Cloudflare...";
        return cfTimedDownload(BROWSER_DOWNLOAD_MS);
      })
      .then(function (down) {
        downResult = down;
        cfSpeedtestStatusEl.textContent = "↓ " + down.mbps + " Mbps, subiendo a Cloudflare...";
        return cfTimedUpload(sizeBrowserUploadBytes(down.mbps));
      })
      .then(function (up) {
        upResult = up;
        cfSpeedtestStatusEl.textContent = "↓ " + downResult.mbps + " Mbps / ↑ " + up.mbps + " Mbps, guardando...";
        // segunda lectura, ya terminada la medición: si la IP cambió respecto
        // del arranque, la red cambió a mitad de prueba.
        return cfMetaIp();
      })
      .then(function (ip) {
        cfEnd = ip;
        var body = {
          device_id: deviceId,
          source: "browser",
          tag: "browser-speedtest-cloudflare",
          ts: new Date().toISOString(),
          down_mbps: downResult.mbps,
          up_mbps: upResult.mbps,
          note:
            "prueba desde el navegador contra speed.cloudflare.com (CDN público): no la topea el enlace del VPS " +
            "y no compite con otro equipo probando al mismo tiempo",
        };
        if (lat && lat.ping_ms !== null) {
          body.ping_ms = lat.ping_ms;
          body.jitter_ms = lat.jitter_ms;
        }
        if (loc) {
          body.lat = loc.lat;
          body.lon = loc.lon;
          body.gps_accuracy_m = loc.accuracy;
          body.gps_source = "browser-geolocation";
        }
        body.test_id = testId;
        body.net_hint = hint;
        body.net_changed_hint = watch.changed;
        if (netDeclaredEl.value) body.net_declared = netDeclaredEl.value;
        if (cfStart) body.net_cf_ip_start = cfStart;
        if (cfEnd) body.net_cf_ip_end = cfEnd;
        return apiFetch("/api/v1/measurements", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
      })
      .then(function (resp) {
        watch.stop();
        cfSpeedtestStatusEl.textContent =
          "listo: ↓" +
          downResult.mbps +
          " Mbps ↑" +
          upResult.mbps +
          " Mbps" +
          (lat && lat.ping_ms !== null ? " · ping " + lat.ping_ms + " ms" : "") +
          routeSuffix(resp);
        runCfSpeedtestBtn.disabled = false;
        if (state.selectedDeviceId === deviceId) loadDetail(deviceId);
        refreshDevices();
      })
      .catch(function (err) {
        watch.stop();
        runCfSpeedtestBtn.disabled = false;
        cfSpeedtestStatusEl.textContent = "";
        showBanner(
          detailErrorEl,
          "Prueba contra Cloudflare falló: " + (err && err.message ? err.message : err)
        );
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
