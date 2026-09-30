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
        refreshProbes();
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
      tr.addEventListener("click", function (e) {
        // los botones de reinicio de la celda "Estado" no abren el detalle
        if (e.target && e.target.closest && e.target.closest(".reboot-ctl button")) return;
        showDetail(d.device_id);
      });
      tr.innerHTML =
        '<td data-label="Equipo">' +
        escapeHtml(d.device_id) +
        '</td><td data-label="Estado"><div class="state-cell">' +
        onlineBadge(d) +
        '<div class="reboot-ctl" data-reboot-cell="' +
        escapeHtml(d.device_id) +
        '">' +
        rebootBlockHtml(d.device_id, true) +
        "</div></div>" +
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
        // Sin re-render mientras se edita una sonda: pisaría lo que se está escribiendo.
        if (state.probeEditing === null) refreshProbes();
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
    stopProbeTestPoll();
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
    stopProbeTestPoll();
    runProbeSpeedtestBtn.disabled = false;
    probeSpeedtestStatusEl.textContent = "";
    updateProbeActions(deviceId);
    showView("detail");
    renderRebootDetail();
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

  // batteryTrend: pendiente real del % de batería (regresión lineal) en la
  // última hora de datos, sin mirar si Android dice "cargando": un cargador
  // inalámbrico flojo reporta "charging" mientras la batería igual baja, y eso
  // es justo lo que hay que ver. rate > 0 = baja (%/h), < 0 = sube. Con menos
  // de 20 min de datos el número es ruido (la batería va en pasos de 1%).
  var TREND_WINDOW_H = 1;
  function batteryTrend(rows) {
    if (rows.length < 3) return null;
    var t0 = new Date(rows[0].received_at).getTime();
    var pts = [];
    for (var i = 0; i < rows.length; i++) {
      var h = (t0 - new Date(rows[i].received_at).getTime()) / 3600000;
      if (isNaN(h) || h > TREND_WINDOW_H) break;
      pts.push([-h, rows[i].battery_pct]);
    }
    if (pts.length < 3) return null;
    var span = -pts[pts.length - 1][0];
    if (span < 20 / 60) return null;
    var mx = 0, my = 0;
    pts.forEach(function (p) { mx += p[0]; my += p[1]; });
    mx /= pts.length;
    my /= pts.length;
    var num = 0, den = 0;
    pts.forEach(function (p) {
      num += (p[0] - mx) * (p[1] - my);
      den += (p[0] - mx) * (p[0] - mx);
    });
    if (!den) return null;
    return { rate: -num / den, hours: span };
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
      var d = batteryTrend(withBattery);
      // El S20+ sigue reportando "carga inalámbrica" después de sacarlo del
      // cargador (visto en campo: lo mantenía en la mano, sin nada cerca).
      // Si la batería baja, ese reporte no se cree: se muestra y se trata como
      // descargando. Un cargador por cable sí se respeta (dato confiable).
      var falseWireless = last.plugged === "wireless" && d && d.rate >= 1;
      var plugged = falseWireless ? "none" : last.plugged;
      var html =
        '<span class="summary-label">Batería:</span> <strong>' +
        last.battery_pct +
        "%</strong> · " +
        escapeHtml(falseWireless ? "descargando" : batteryStatusEs(last));
      if (plugged && plugged !== "none") html += " (conectado a " + escapeHtml(PLUGGED_ES[plugged] || plugged) + ")";
      if (falseWireless) html += ' <span class="muted">(reporta carga inalámbrica pero la batería baja: se ignora)</span>';
      if (last.net_type) html += ' · <span class="summary-label">red:</span> ' + escapeHtml(NET_ES[last.net_type] || last.net_type);
      if (last.battery_temp_c !== null && last.battery_temp_c !== undefined) html += " · " + fmtNum(last.battery_temp_c, " °C", 1);
      html += ' · <span class="muted">' + fmtDate(last.received_at) + "</span>";
      if (d) {
        var trendNote = ' <span class="muted">(tendencia de los últimos ' + Math.round(d.hours * 60) + " min";
        if (Math.abs(d.rate) < 0.5) {
          html += '<br><span class="summary-label">Tendencia:</span> estable' + trendNote + ")</span>";
        } else if (d.rate > 0) {
          html +=
            '<br><span class="summary-label">Consumo:</span> baja ' +
            fmtNum(d.rate, " %/h", 1) +
            trendNote +
            ", unas " +
            fmtNum(last.battery_pct / d.rate, " h", 1) +
            " hasta agotarse)</span>";
        } else {
          html +=
            '<br><span class="summary-label">Tendencia:</span> sube ' +
            fmtNum(-d.rate, " %/h", 1) +
            trendNote +
            (last.battery_pct < 100 ? ", unas " + fmtNum((100 - last.battery_pct) / -d.rate, " h", 1) + " hasta llenarse" : "") +
            ")</span>";
        }
        // Solo cargadores por cable: el reporte de inalámbrica no es confiable (ver falseWireless).
        if (d.rate >= 1 && (plugged === "usb" || plugged === "ac" || plugged === "dock")) {
          html +=
            '<br><span class="muted">⚠ Figura conectado a ' +
            escapeHtml(PLUGGED_ES[plugged]) +
            " pero la batería igual baja: ese cargador no alcanza (mal alineado, de poca potencia, o el celular " +
            "consume más de lo que entrega).</span>";
        }
      }
      if (last.net_type === "ethernet" && (!plugged || plugged === "none")) {
        html +=
          '<br><span class="muted">El adaptador USB-Ethernet se alimenta de la batería del celular (no hay nada ' +
          "cargándolo).</span>";
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
        (c.runner === "probe"
          ? ' <span class="muted">vía sonda ' + escapeHtml(c.probe_id || "") + " (" + escapeHtml(c.routing_table || "") + ")</span>"
          : "") +
        (c.runner === "phone" && isRebootOrder(c)
          ? ' <span class="muted">vía celular ' + escapeHtml(c.probe_id || "") + " (SSH por el MikroTik)</span>"
          : c.runner === "phone"
            ? ' <span class="muted">vía celular ' + escapeHtml(c.probe_id || "") + " (" + escapeHtml(c.routing_table || (c.target === "next" ? "siguiente disponible" : c.slot || "")) + ")</span>"
            : "") +
        '</td><td data-label="Estado">' +
        (c.runner === "phone"
          ? (isRebootOrder(c) ? rebootStatusBadge(c) : orderStatusBadge(c)) + (c.error ? ' <span class="muted">' + escapeHtml(c.error) + "</span>" : "")
          : escapeHtml(c.status)) +
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
    "measured-by-probe": "la corrió la sonda MikroTik por el puerto de este equipo",
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

  // ---------------------------------------------------------------- sondas (MikroTik)
  //
  // Una sonda mide cada equipo por la tabla de ruteo de su puerto. El backend
  // guarda la configuración y la cola; la sonda solo consulta y ejecuta. Acá se
  // configura, se prende/apaga el ciclo automático y se piden pruebas.

  var probesListEl = document.getElementById("probes-list");
  var probeErrorEl = document.getElementById("probe-error");
  var probeNewBtn = document.getElementById("probe-new");
  var probeEditorEl = document.getElementById("probe-editor");
  var probeEditorTitle = document.getElementById("probe-editor-title");
  var peId = document.getElementById("pe-id");
  var peLabel = document.getElementById("pe-label");
  var peDuration = document.getElementById("pe-duration");
  var peTargets = document.getElementById("pe-targets");
  var peDeviceOptions = document.getElementById("pe-device-options");
  var peStatus = document.getElementById("pe-status");
  var peRunner = document.getElementById("pe-runner");
  var probeActionsEl = document.getElementById("probe-actions");
  var probeActionsInfo = document.getElementById("probe-actions-info");
  var runProbeSpeedtestBtn = document.getElementById("run-probe-speedtest");
  var probeSpeedtestStatusEl = document.getElementById("probe-speedtest-status");

  var PROBE_INTERVALS = [
    [0, "Apagado"],
    [300, "cada 5 min"],
    [600, "cada 10 min"],
    [900, "cada 15 min"],
    [1800, "cada 30 min"],
    [3600, "cada 1 h"],
  ];
  // La sonda consulta la cola cada ~30-60 s y puede tener otras pruebas del
  // ciclo delante: se espera bastante más que la prueba del agente.
  var PROBE_TEST_TIMEOUT_MS = 5 * 60 * 1000;
  var PROBE_ID_RE = /^[A-Za-z0-9_-]{1,64}$/;

  state.probes = [];
  state.probeEditing = null; // probe_id que se edita, o "" si es nueva
  state.probeTest = null; // {commandId, timer}
  state.probeMsg = {}; // probe_id -> último mensaje de estado de su tarjeta

  function secondsSince(iso) {
    var t = new Date(iso).getTime();
    return isNaN(t) ? null : (Date.now() - t) / 1000;
  }

  function probeName(p) {
    return p.label ? p.label + " (" + p.probe_id + ")" : p.probe_id;
  }

  function refreshProbes() {
    if (!state.apiKey) return Promise.resolve();
    return apiFetch("/api/v1/probes")
      .then(function (probes) {
        state.probes = probes || [];
        state.probesAt = Date.now();
        clearBanner(probeErrorEl);
        renderProbes();
        if (state.view === "detail") updateProbeActions(state.selectedDeviceId);
      })
      .catch(function (err) {
        // backend sin el endpoint todavía, o red caída: no rompe la lista de equipos
        showBanner(probeErrorEl, "No se pudieron leer las sondas: " + err.message);
      });
  }

  // Lo que el backend acepta en PUT: la config actual con los cambios encima.
  function probePayload(p, changes) {
    // runner, slot y expected_asn van SIEMPRE con el valor actual (contrato
    // §4 punto 5): el backend los conserva si faltan, pero no se depende de eso.
    // runner "" = "dejar el que tenga" (sonda nueva: probe).
    var body = {
      label: p.label || "",
      runner: p.runner || "",
      interval_s: p.interval_s || 0,
      duration_s: p.duration_s || null,
      enabled: p.enabled !== false,
      targets: (p.targets || []).map(function (t) {
        return {
          slot: t.slot || "",
          device_id: t.device_id,
          routing_table: t.routing_table,
          label: t.label || "",
          send_heartbeat: !!t.send_heartbeat,
          enabled: t.enabled !== false,
          expected_asn: t.expected_asn === undefined || t.expected_asn === "" ? null : t.expected_asn,
        };
      }),
    };
    Object.keys(changes || {}).forEach(function (k) {
      body[k] = changes[k];
    });
    return body;
  }

  function putProbe(probeId, body) {
    return apiFetch("/api/v1/probes/" + encodeURIComponent(probeId), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  function deviceById(id) {
    return (state.devices || []).filter(function (d) {
      return d.device_id === id;
    })[0];
  }

  function renderProbes() {
    // El panel del celular se reutiliza entre redibujos: si se estaba
    // escribiendo en uno de sus campos, se le devuelve el foco al final.
    var focused = document.activeElement;
    if (!focused || !focused.closest || !focused.closest(".phone-panel")) focused = null;
    probesListEl.innerHTML = "";
    syncPhoneTimer();
    if (!state.probes.length) {
      probesListEl.innerHTML =
        '<div class="summary-box muted">Todavía no hay ninguna sonda configurada. Tocá "+ Configurar sonda" para ' +
        "registrar el MikroTik y qué equipo va en cada puerto.</div>";
      return;
    }
    state.probes.forEach(function (p) {
      var card = document.createElement("div");
      card.className = "summary-box probe-card";

      var head = document.createElement("div");
      head.className = "probe-card-head";
      head.innerHTML =
        '<span class="probe-name">' +
        escapeHtml(probeName(p)) +
        "</span>" +
        (p.last_seen
          ? onlineBadge({ online: p.online, last_seen_seconds_ago: secondsSince(p.last_seen) })
          : '<span class="online-badge online-no"><span class="dot"></span>nunca se conectó</span>') +
        (p.enabled === false ? '<span class="rat-badge rat-other">desactivada</span>' : "") +
        (isPhoneProbe(p)
          ? '<span class="rat-badge rat-4g">mide un celular por cable</span>'
          : '<span class="rat-badge rat-other">mide el script del MikroTik</span>');
      card.appendChild(head);

      var controls = document.createElement("div");
      controls.className = "probe-controls";
      var sel = document.createElement("select");
      sel.setAttribute("aria-label", "Ciclo automático de " + p.probe_id);
      var known = false;
      PROBE_INTERVALS.forEach(function (opt) {
        var o = document.createElement("option");
        o.value = String(opt[0]);
        o.textContent = opt[1];
        if (opt[0] === (p.interval_s || 0)) known = true;
        sel.appendChild(o);
      });
      if (!known) {
        var o = document.createElement("option");
        o.value = String(p.interval_s);
        o.textContent = "cada " + Math.round(p.interval_s / 60) + " min";
        sel.appendChild(o);
      }
      sel.value = String(p.interval_s || 0);
      var label = document.createElement("span");
      label.className = "summary-label";
      label.textContent = "Ciclo automático:";
      var cycleBtn = document.createElement("button");
      cycleBtn.type = "button";
      cycleBtn.className = "btn-primary";
      cycleBtn.textContent = "Ciclo ahora";
      var editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "btn-link";
      editBtn.textContent = "Editar";
      var status = document.createElement("span");
      status.className = "muted";
      // El mensaje vive en state: cada acción termina refrescando la lista, y
      // el redibujo de la tarjeta lo borraría antes de que se alcance a leer.
      status.textContent = state.probeMsg[p.probe_id] || "";
      function say(text) {
        state.probeMsg[p.probe_id] = text;
        status.textContent = text;
      }
      controls.appendChild(label);
      controls.appendChild(sel);
      controls.appendChild(cycleBtn);
      controls.appendChild(editBtn);
      controls.appendChild(status);
      card.appendChild(controls);

      sel.addEventListener("change", function () {
        var v = parseInt(sel.value, 10) || 0;
        sel.disabled = true;
        say("guardando...");
        putProbe(p.probe_id, probePayload(p, { interval_s: v }))
          .then(function () {
            say(v ? "ciclo automático " + sel.options[sel.selectedIndex].text : "ciclo automático apagado");
            return refreshProbes();
          })
          .catch(function (err) {
            say("");
            sel.value = String(p.interval_s || 0);
            showBanner(probeErrorEl, "No se pudo cambiar el intervalo: " + err.message);
          })
          .then(function () {
            sel.disabled = false;
          });
      });

      cycleBtn.addEventListener("click", function () {
        cycleBtn.disabled = true;
        say("encolando...");
        apiFetch("/api/v1/probes/" + encodeURIComponent(p.probe_id) + "/cycle", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ requested_by: "dashboard" }),
        })
          .then(function (res) {
            var n = res && res.command_ids ? res.command_ids.length : 0;
            say(
              n
                ? n + " prueba(s) en cola" + (p.online ? "" : " — la sonda no está en línea, se ejecutan cuando vuelva")
                : "todos los equipos ya tenían una prueba en curso"
            );
            refreshProbes();
          })
          .catch(function (err) {
            say("");
            showBanner(probeErrorEl, "No se pudo encolar el ciclo: " + err.message);
          })
          .then(function () {
            cycleBtn.disabled = false;
          });
      });

      editBtn.addEventListener("click", function () {
        openProbeEditor(p);
      });

      var info = document.createElement("div");
      info.className = "muted";
      info.innerHTML =
        "Último ciclo: " +
        (p.last_cycle_at ? fmtDate(p.last_cycle_at) : "nunca") +
        (p.duration_s ? " · pruebas de " + p.duration_s + " s" : "");
      card.appendChild(info);

      var ul = document.createElement("ul");
      ul.className = "probe-targets";
      if (!(p.targets || []).length) {
        ul.innerHTML = '<li class="muted">⚠ sin equipos: editala para agregar qué equipo va en cada puerto</li>';
      }
      (p.targets || []).forEach(function (t, i) {
        var d = deviceById(t.device_id);
        var li = document.createElement("li");
        li.innerHTML =
          i +
          1 +
          ". <strong>" +
          (isPhoneProbe(p) ? escapeHtml(targetSlot(t, i)) + " · " : "") +
          escapeHtml(t.label || t.device_id) +
          "</strong>" +
          (t.label ? ' <span class="muted">' + escapeHtml(t.device_id) + "</span>" : "") +
          ' <span class="muted">→ tabla</span> <code>' +
          escapeHtml(t.routing_table) +
          "</code>" +
          (t.send_heartbeat ? ' <span class="band-chip">heartbeat</span>' : "") +
          (t.expected_asn ? ' <span class="muted">ASN esperado ' + escapeHtml(String(t.expected_asn)) + "</span>" : "") +
          (t.enabled === false ? ' <span class="rat-badge rat-other">desactivado</span>' : "") +
          " " +
          (d ? onlineBadge(d) : '<span class="muted">(todavía no reportó)</span>');
        ul.appendChild(li);
      });
      card.appendChild(ul);
      if (isPhoneProbe(p)) {
        var ui = phoneUi(p);
        card.appendChild(ui.root);
        if (!ui.loadedOnce) {
          ui.loadedOnce = true;
          refreshPhoneStatus(ui);
          refreshPhoneLists(ui);
        }
      }
      probesListEl.appendChild(card);
    });
    renderRebootViews();
    if (focused && document.contains(focused)) {
      try {
        focused.focus({ preventScroll: true });
      } catch (e) {
        // navegador sin opciones de focus(): no importa
      }
    }
  }

  // ---- editor

  function addTargetRow(t) {
    var row = document.createElement("div");
    row.className = "probe-target-row";
    row.innerHTML =
      '<input type="text" class="pe-slot" maxlength="1" placeholder="A" title="Letra del equipo en la sonda (A, B…). Vacío = por posición." autocomplete="off" aria-label="Slot" />' +
      '<input type="text" class="pe-device" list="pe-device-options" placeholder="device_id del equipo" autocomplete="off" />' +
      '<input type="text" class="pe-table" placeholder="tabla de ruteo (p. ej. to-A)" autocomplete="off" />' +
      '<input type="text" class="pe-label" placeholder="nombre (p. ej. Notion 5G)" autocomplete="off" />' +
      '<input type="number" class="pe-asn" min="1" step="1" placeholder="ASN esperado" title="ASN esperado de la salida de este router (opcional). Vacío = el que el backend ya conozca del equipo." />' +
      '<label><input type="checkbox" class="pe-hb" /> heartbeat</label>' +
      '<button type="button" class="btn-link btn-danger">quitar</button>';
    row.querySelector(".pe-slot").value = t && t.slot ? t.slot : "";
    row.querySelector(".pe-device").value = t ? t.device_id : "";
    row.querySelector(".pe-table").value = t ? t.routing_table : "";
    row.querySelector(".pe-label").value = t && t.label ? t.label : "";
    row.querySelector(".pe-asn").value = t && t.expected_asn ? String(t.expected_asn) : "";
    row.querySelector(".pe-hb").checked = !!(t && t.send_heartbeat);
    row.dataset.enabled = t && t.enabled === false ? "0" : "1";
    row.querySelector("button").addEventListener("click", function () {
      row.remove();
    });
    peTargets.appendChild(row);
  }

  function openProbeEditor(p) {
    state.probeEditing = p ? p.probe_id : "";
    probeEditorTitle.textContent = p ? "Editar sonda " + p.probe_id : "Configurar sonda nueva";
    peId.value = p ? p.probe_id : "";
    peId.disabled = !!p;
    peLabel.value = p ? p.label || "" : "";
    peRunner.value = p && p.runner === "phone" ? "phone" : "probe";
    peDuration.value = p && p.duration_s ? p.duration_s : 10;
    peStatus.textContent = "";
    peTargets.innerHTML = "";
    peDeviceOptions.innerHTML = "";
    (state.devices || []).forEach(function (d) {
      var o = document.createElement("option");
      o.value = d.device_id;
      peDeviceOptions.appendChild(o);
    });
    var targets = p && p.targets && p.targets.length ? p.targets : [null, null];
    targets.forEach(addTargetRow);
    probeEditorEl.hidden = false;
    probeEditorEl.scrollIntoView({ behavior: "smooth", block: "start" });
    (p ? peLabel : peId).focus();
  }

  function closeProbeEditor() {
    probeEditorEl.hidden = true;
    state.probeEditing = null;
  }

  probeNewBtn.addEventListener("click", function () {
    openProbeEditor(null);
  });
  document.getElementById("pe-add-target").addEventListener("click", function () {
    addTargetRow(null);
  });
  document.getElementById("pe-cancel").addEventListener("click", closeProbeEditor);

  document.getElementById("pe-save").addEventListener("click", function () {
    var id = peId.value.trim();
    if (!PROBE_ID_RE.test(id)) {
      peStatus.textContent = "El ID solo puede tener letras, números, - y _ (máx. 64).";
      return;
    }
    var targets = [];
    var problem = "";
    var runner = peRunner.value === "phone" ? "phone" : "probe";
    var seenSlot = {};
    var seenTable = {};
    Array.prototype.forEach.call(peTargets.querySelectorAll(".probe-target-row"), function (row) {
      var dev = row.querySelector(".pe-device").value.trim();
      var table = row.querySelector(".pe-table").value.trim();
      var slot = row.querySelector(".pe-slot").value.trim().toUpperCase();
      var asnRaw = row.querySelector(".pe-asn").value.trim();
      if (!dev && !table) return; // fila vacía: se ignora
      if (!dev || !table) problem = "Cada equipo necesita device_id y tabla de ruteo.";
      if (slot && !/^[A-Z]$/.test(slot)) problem = "El slot es una sola letra (A, B, C…).";
      if (slot && seenSlot[slot]) problem = "Dos equipos no pueden tener el mismo slot (" + slot + ").";
      seenSlot[slot || "_" + targets.length] = true;
      var asn = null;
      if (asnRaw) {
        asn = parseInt(asnRaw, 10);
        if (isNaN(asn) || asn <= 0 || String(asn) !== asnRaw) problem = "El ASN esperado es un número entero mayor que 0 (o vacío).";
      }
      if (runner === "phone") {
        if (table === "main") problem = "Con un celular, la tabla main es la de respaldo: cada router necesita su propia tabla (to-A, to-B…).";
        else if (seenTable[table]) problem = "Con un celular, cada router necesita una tabla distinta (" + table + " está repetida).";
      }
      seenTable[table] = true;
      targets.push({
        slot: slot,
        device_id: dev,
        routing_table: table,
        label: row.querySelector(".pe-label").value.trim(),
        send_heartbeat: row.querySelector(".pe-hb").checked,
        enabled: row.dataset.enabled !== "0",
        expected_asn: asn,
      });
    });
    if (problem) {
      peStatus.textContent = problem;
      return;
    }
    var existing = state.probes.filter(function (p) {
      return p.probe_id === id;
    })[0];
    if (!state.probeEditing && existing) {
      peStatus.textContent = "Ya existe una sonda con ese ID: usá Editar en su tarjeta.";
      return;
    }
    var dur = parseInt(peDuration.value, 10);
    var body = probePayload(existing || { interval_s: 0 }, {
      label: peLabel.value.trim(),
      runner: runner,
      duration_s: isNaN(dur) ? null : dur,
      targets: targets,
    });
    peStatus.textContent = "guardando...";
    putProbe(id, body)
      .then(function () {
        closeProbeEditor();
        return refreshProbes();
      })
      .catch(function (err) {
        peStatus.textContent = "No se pudo guardar: " + err.message;
      });
  });

  // ---- detalle: prueba de ESTE equipo vía su sonda

  function probeForDevice(deviceId) {
    for (var i = 0; i < state.probes.length; i++) {
      var p = state.probes[i];
      if (p.enabled === false) continue;
      var t = (p.targets || []).filter(function (x) {
        return x.device_id === deviceId && x.enabled !== false;
      })[0];
      if (t) return { probe: p, target: t };
    }
    return null;
  }

  function updateProbeActions(deviceId) {
    var m = deviceId ? probeForDevice(deviceId) : null;
    probeActionsEl.hidden = !m;
    if (!m) return;
    var p = m.probe;
    probeActionsInfo.innerHTML =
      (isPhoneProbe(p) ? "Mide este equipo con el celular por cable de " : "Mide este equipo desde ") +
      escapeHtml(probeName(p)) +
      " por la tabla <code>" +
      escapeHtml(m.target.routing_table) +
      "</code>: la salida por este equipo está garantizada por el cable, no depende de detectarla. " +
      (p.online
        ? "Sonda en línea."
        : "⚠ La sonda no está en línea: la prueba queda en cola hasta que vuelva a consultar.");
  }

  function stopProbeTestPoll() {
    if (state.probeTest && state.probeTest.timer) clearInterval(state.probeTest.timer);
    state.probeTest = null;
  }

  runProbeSpeedtestBtn.addEventListener("click", function () {
    var deviceId = state.selectedDeviceId;
    var m = deviceId ? probeForDevice(deviceId) : null;
    if (!m) return;
    runProbeSpeedtestBtn.disabled = true;
    clearBanner(detailErrorEl);
    // Con un celular por cable, él toma su propio fix GPS: la ubicación del
    // navegador no se pide ni se manda (contrato sonda A/B §1.5).
    var phone = isPhoneProbe(m.probe);
    probeSpeedtestStatusEl.textContent = phone ? "creando la orden para el celular..." : "obteniendo ubicación del navegador...";
    (phone ? Promise.resolve(null) : getBrowserLocation())
      .catch(function () {
        return null; // sin ubicación se mide igual
      })
      .then(function (loc) {
        probeSpeedtestStatusEl.textContent = "creando comando...";
        var body = {
          device_id: deviceId,
          type: "run_speedtest",
          runner: "probe",
          probe_id: m.probe.probe_id,
          duration_s: m.probe.duration_s || 10,
          requested_by: "dashboard",
        };
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
        if (!res || res.id === undefined) throw { message: "El backend no devolvió el id del comando." };
        var commandId = res.id;
        var startedAt = Date.now();
        stopProbeTestPoll();
        state.probeTest = { commandId: commandId };
        probeSpeedtestStatusEl.textContent = "en cola, esperando a la sonda...";
        state.probeTest.timer = setInterval(function () {
          pollProbeTest(deviceId, commandId, startedAt);
        }, 3000);
      })
      .catch(function (err) {
        runProbeSpeedtestBtn.disabled = false;
        probeSpeedtestStatusEl.textContent = "";
        showBanner(detailErrorEl, "No se pudo pedir la prueba a la sonda: " + err.message);
      });
  });

  function pollProbeTest(deviceId, commandId, startedAt) {
    if (!state.probeTest || state.probeTest.commandId !== commandId || state.selectedDeviceId !== deviceId) return;
    var elapsed = Math.round((Date.now() - startedAt) / 1000);
    if (Date.now() - startedAt > PROBE_TEST_TIMEOUT_MS) {
      stopProbeTestPoll();
      runProbeSpeedtestBtn.disabled = false;
      probeSpeedtestStatusEl.textContent =
        "la sonda no la ejecutó en " + PROBE_TEST_TIMEOUT_MS / 60000 + " min (sigue en cola; se va a ejecutar cuando la sonda consulte)";
      return;
    }
    apiFetch("/api/v1/commands?device_id=" + encodeURIComponent(deviceId) + "&limit=30")
      .then(function (cmds) {
        var c = (cmds || []).filter(function (x) {
          return x.id === commandId;
        })[0];
        if (!c) return;
        if (c.status === "pending") {
          probeSpeedtestStatusEl.textContent = "en cola, esperando a la sonda... " + elapsed + "s";
          return;
        }
        if (c.status === "delivered") {
          probeSpeedtestStatusEl.textContent = "el celular ya recibió la orden, esperando su turno... " + elapsed + "s";
          return;
        }
        if (c.status === "claimed" || c.status === "running") {
          probeSpeedtestStatusEl.textContent =
            (c.runner === "phone" ? "el celular está midiendo... " : "la sonda está midiendo... ") + elapsed + "s";
          return;
        }
        stopProbeTestPoll();
        runProbeSpeedtestBtn.disabled = false;
        var FINAL_ES = { failed: "prueba fallida", expired: "la orden venció sin ejecutarse", interrupted: "prueba interrumpida", cancelled: "orden cancelada" };
        if (FINAL_ES[c.status]) {
          probeSpeedtestStatusEl.textContent =
            FINAL_ES[c.status] + (c.closed_by === "server" ? " (cerrada por el servidor)" : "") + (c.error ? ": " + c.error : "");
        } else {
          probeSpeedtestStatusEl.textContent = "prueba completada";
          // Orden de celular: su medición se busca por result_id entre las de
          // la sonda (si no quedó atribuida al router, p. ej. por WiFi, la
          // última del equipo sería OTRA medición, más vieja).
          var isPhoneOrder = c.runner === "phone";
          var q = isPhoneOrder
            ? "/api/v1/measurements?probe_id=" + encodeURIComponent(c.probe_id || "") + "&limit=20"
            : "/api/v1/measurements?device_id=" + encodeURIComponent(deviceId) + "&limit=1";
          apiFetch(q)
            .then(function (rows) {
              var r = isPhoneOrder
                ? (rows || []).filter(function (x) {
                    return x && c.result_id && x.result_id === c.result_id;
                  })[0]
                : (rows || [])[0];
              if (r && (r.source === "mikrotik-probe" || isPhoneOrder)) {
                var reason = r.net && r.net.reason;
                probeSpeedtestStatusEl.textContent =
                  "listo: ↓" + fmtNum(r.down_mbps) + " Mbps ↑" + fmtNum(r.up_mbps) + " Mbps" +
                  (r.ping_ms !== undefined && r.ping_ms !== null ? " · ping " + fmtNum(r.ping_ms, " ms", 0) : "") +
                  (isPhoneOrder && !r.slot ? " · SIN ATRIBUIR al router" + (reason ? ": " + phoneReasonEs(reason) : "") : "");
              }
            })
            .catch(function () {});
          // El celular toma su propio fix: la ubicación del navegador no va a
          // su medición (el backend además la ignora, contrato §1.5).
          if (c.runner !== "phone") sendSpeedtestEndLocation(deviceId, commandId);
        }
        loadDetail(deviceId);
        refreshDevices();
      })
      .catch(function () {
        // error de red pasajero: se reintenta en el próximo tick
      });
  }

  // ---------------------------------------------------------------- sonda con celular por cable (forma A)
  //
  // Contrato: server/docs/CONTRATO-SONDA-AB.md §4. Una sonda con runner
  // "phone" la ejecuta un celular conectado por cable al MikroTik: él mueve la
  // regla phone-probe a la tabla del router (to-A / to-B), mide y sube. Acá se
  // ve qué está haciendo (estado en vivo), se piden pruebas (órdenes) y se ve
  // de dónde salió cada resultado (router, tabla, operador, GPS, sincronización).
  //
  // El panel de cada sonda se arma UNA vez y se reutiliza en cada redibujo de la
  // lista (renderProbes vacía la lista cada 20 s): así los campos de N,
  // separación y fechas no se pierden mientras se escriben.

  var PHONE_TICK_MS = 5000; // estado en vivo
  var PHONE_LISTS_EVERY = 2; // órdenes y resultados: cada 2 ticks (10 s)
  var PHONE_STALE_S = 60;
  var PHONE_LIST_LIMIT = 20;
  var OTHER_TRAFFIC_WARN_BYTES = 5 * 1e6;

  var PHASE_ES = {
    stopped: "Detenida",
    idle: "En espera",
    waiting_ethernet: "Esperando el cable",
    preparing: "Preparando",
    switching_route: "Cambiando la ruta",
    verifying_route: "Verificando la ruta",
    egress_check: "Comprobando la salida",
    gps_fix: "Esperando GPS",
    ping: "Midiendo latencia",
    download: "Descargando",
    upload: "Subiendo",
    storing: "Guardando",
    restoring_route: "Volviendo a respaldo",
    uploading: "Enviando resultado",
    backoff: "Sin backend, reintentando",
  };
  // Barra de pasos (mismos pasos que la pantalla de la app, §2.11).
  var PHASE_STEPS = [
    ["switching_route", "Ruta"],
    ["verifying_route", "Verificación"],
    ["egress_check", "Salida"],
    ["gps_fix", "GPS"],
    ["ping", "Ping"],
    ["download", "Bajada"],
    ["upload", "Subida"],
    ["storing", "Guardar"],
    ["restoring_route", "Respaldo"],
  ];
  var ORDER_STATUS_ES = {
    pending: ["En cola", "st-muted"],
    claimed: ["Tomada", "st-info"],
    delivered: ["Recibida por el celular", "st-info"],
    running: ["Midiendo", "st-run"],
    done: ["Completada", "st-ok"],
    failed: ["Fallida", "st-err"],
    expired: ["Vencida", "st-warn"],
    interrupted: ["Interrumpida", "st-warn"],
    cancelled: ["Cancelada", "st-muted"],
  };
  var SELECTION_ES = {
    requested: "pedida desde el dashboard",
    next: "siguiente disponible",
    sequence: "secuencia alternada",
    alternation: "ciclo automático",
    "offline-schedule": "plan local sin backend",
    "manual-app": "pedida desde la app",
  };
  // Veredictos de red de los resultados del celular (§4 punto 4).
  var PHONE_REASON_ES = {
    "phone-wifi": "por WiFi, no se atribuye a ningún router",
    "probe-route-unconfirmed": "no se confirmó la ruta en el MikroTik",
    "probe-asn-match": "salió por el operador esperado",
    "probe-asn-other-router": "salió por el OTRO router",
    "probe-asn-mismatch": "el operador de salida no coincide",
    "probe-asn-ambiguous": "ruta confirmada; los dos routers son del mismo operador",
    "network-changed-mid-test": "la IP de salida cambió durante la prueba",
    "probe-route-changed": "la regla del MikroTik cambió durante la prueba: no se atribuye",
    "probe-table-confirmed": "ruta confirmada en el MikroTik (sin verificar operador)",
  };
  // El detalle de cada equipo también muestra estas mediciones (tooltip de
  // "Salida"): que las entienda sin pisar los textos que ya existían.
  Object.keys(PHONE_REASON_ES).forEach(function (k) {
    if (!REASON_ES[k]) REASON_ES[k] = PHONE_REASON_ES[k];
  });

  state.phone = {}; // probe_id -> ui (panel reutilizable + últimos datos)
  state.phoneTimer = null;
  state.phoneTick = 0;

  function phoneReasonEs(r) {
    if (!r) return "";
    return PHONE_REASON_ES[r] || reasonEs(r);
  }

  function selectionEs(r) {
    if (!r) return "—";
    var m = /^fallback:([A-Z])-sin-datos$/.exec(r);
    if (m) return "respaldo: " + m[1] + " sin datos";
    return SELECTION_ES[r] || r;
  }

  function isPhoneProbe(p) {
    return p && p.runner === "phone";
  }

  // Slot de un equipo: el que manda el backend o, si todavía no lo manda, la
  // letra por posición (misma regla que usa el backend al asignarlo).
  function targetSlot(t, i) {
    return t && t.slot ? t.slot : String.fromCharCode(65 + i);
  }

  function probeTargetsWithSlots(p) {
    return (p.targets || []).map(function (t, i) {
      return { t: t, slot: targetSlot(t, i) };
    });
  }

  function activeSlots(p) {
    return probeTargetsWithSlots(p).filter(function (x) {
      return x.t.enabled !== false;
    });
  }

  function targetBySlot(p, slot) {
    var m = probeTargetsWithSlots(p).filter(function (x) {
      return x.slot === slot;
    })[0];
    return m ? m.t : null;
  }

  function slotForTable(p, table) {
    var m = probeTargetsWithSlots(p).filter(function (x) {
      return x.t.routing_table === table;
    })[0];
    return m ? m.slot : null;
  }

  function slotLabel(p, slot) {
    var t = targetBySlot(p, slot);
    return t && t.label ? t.label : t ? t.device_id : "";
  }

  // "Respaldo (sale por el que tenga datos)" / "Forzada a A" (§2.11).
  function tableEs(p, table) {
    if (!table) return "desconocida";
    if (table === "main") return "Respaldo (sale por el que tenga datos)";
    var slot = slotForTable(p, table);
    return "Forzada a " + (slot || table);
  }

  // UUID v4 para order_id. crypto.randomUUID solo existe en contextos seguros
  // (HTTPS o localhost): el dashboard local también se abre como
  // http://192.168.40.22:8080, donde no está (contrato §4 punto 2).
  function newUuid() {
    var c = window.crypto || window.msCrypto;
    if (c && typeof c.randomUUID === "function") {
      try {
        return c.randomUUID();
      } catch (e) {
        // sigue abajo
      }
    }
    var b = new Uint8Array(16);
    if (c && c.getRandomValues) c.getRandomValues(b);
    else for (var i = 0; i < 16; i++) b[i] = Math.floor(Math.random() * 256);
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    var h = "";
    for (var j = 0; j < 16; j++) h += (b[j] < 16 ? "0" : "") + b[j].toString(16);
    return h.slice(0, 8) + "-" + h.slice(8, 12) + "-" + h.slice(12, 16) + "-" + h.slice(16, 20) + "-" + h.slice(20);
  }

  // <input type="datetime-local"> da hora local SIN zona: se pasa a UTC RFC
  // 3339 sin fracciones (contrato §0). "" -> null; inválida -> undefined.
  function isoFromLocalInput(v) {
    if (!v) return null;
    var d = new Date(v);
    if (isNaN(d.getTime())) return undefined;
    return d.toISOString().replace(/\.\d{3}Z$/, "Z");
  }

  function localInputValue(d) {
    function p2(n) {
      return (n < 10 ? "0" : "") + n;
    }
    return (
      d.getFullYear() + "-" + p2(d.getMonth() + 1) + "-" + p2(d.getDate()) + "T" + p2(d.getHours()) + ":" + p2(d.getMinutes())
    );
  }

  // Devuelve HTML seguro: las horas del estado en vivo las escribe el celular
  // (raw tal cual en probe_status), así que una que no parsea se escapa.
  function fmtTime(iso) {
    if (!iso) return "—";
    var d = new Date(iso);
    if (isNaN(d.getTime())) return escapeHtml(String(iso));
    var sameDay = new Date().toDateString() === d.toDateString();
    var t = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    return sameDay ? t : d.toLocaleDateString([], { day: "2-digit", month: "2-digit" }) + " " + t;
  }

  function fmtDur(s) {
    if (s === null || s === undefined || isNaN(s)) return "—";
    s = Math.max(0, s);
    if (s < 60) return Math.round(s) + " s";
    if (s < 3600) return Math.round(s / 60) + " min";
    if (s < 86400) return (s / 3600).toFixed(1) + " h";
    return Math.round(s / 86400) + " d";
  }

  function badge(cls, text, title) {
    return (
      '<span class="st-badge ' + cls + '"' + (title ? ' title="' + escapeHtml(title) + '"' : "") + ">" + escapeHtml(text) + "</span>"
    );
  }

  function orderStatusBadge(o) {
    var s = ORDER_STATUS_ES[o.status] || [o.status || "—", "st-muted"];
    return badge(s[1], s[0], o.status);
  }

  function gpsBadge(status, ageS) {
    var age = ageS !== null && ageS !== undefined ? " · " + fmtDur(ageS) : "";
    if (status === "fresh") return badge("st-ok", "GPS FRESCO" + age);
    if (status === "stale") return badge("st-warn", "GPS VIEJO" + age);
    if (status === "unavailable") return badge("st-err", "GPS SIN FIJO");
    return badge("st-muted", "GPS —");
  }

  function yesNo(v) {
    if (v === true) return "sí";
    if (v === false) return "no";
    return "—";
  }

  function has(v) {
    return v !== null && v !== undefined && v !== "";
  }

  function gatewayIp(gw) {
    if (!gw) return "";
    return String(gw).split("%")[0];
  }

  function gatewayIface(gw) {
    if (!gw) return "";
    var i = String(gw).indexOf("%");
    return i === -1 ? "" : String(gw).slice(i + 1);
  }

  // phoneApi: como apiFetch pero NUNCA rechaza y conserva el código HTTP (hace
  // falta para distinguir 409 "order_id en uso" de un error de red, que se
  // reintenta con el mismo order_id).
  //
  // Con tope de tiempo: una petición colgada (red que se traga los paquetes)
  // dejaría el panel congelado en "en línea" para siempre, porque cada
  // refresco espera a que termine el anterior.
  var PHONE_FETCH_TIMEOUT_MS = 15000;

  function phoneApi(method, path, body) {
    var opts = { method: method, headers: { "X-API-Key": state.apiKey } };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    var timer = null;
    if (typeof AbortController === "function") {
      var ac = new AbortController();
      opts.signal = ac.signal;
      timer = setTimeout(function () {
        ac.abort();
      }, PHONE_FETCH_TIMEOUT_MS);
    }
    return fetch(path, opts)
      .then(function (res) {
        return res.text().then(function (text) {
          var data = null;
          try {
            data = text ? JSON.parse(text) : null;
          } catch (e) {
            data = null;
          }
          var error = null;
          if (!res.ok) {
            if (res.status === 401) error = "API key inválida o ausente.";
            else if (data && data.error) error = data.error;
            else if (text && text.charAt(0) === "<") error = "Error " + res.status + " (respuesta HTML)";
            else error = "Error " + res.status + (text ? ": " + text.trim() : "");
          }
          return { ok: res.ok, status: res.status, data: data, error: error };
        });
      })
      .catch(function () {
        return { ok: false, status: 0, data: null, error: "No se pudo conectar con el backend." };
      })
      .then(function (r) {
        if (timer) clearTimeout(timer);
        return r;
      });
  }

  // El mux de Go responde "404 page not found" (texto plano) a una ruta que no
  // existe: el backend todavía no tiene el endpoint. Distinto de un 404 JSON
  // ("sonda no configurada").
  function endpointMissing(r) {
    return (r.status === 404 && !(r.data && r.data.error)) || r.status === 405;
  }

  function probeBase(p) {
    return "/api/v1/probes/" + encodeURIComponent(p.probe_id);
  }

  // ---- panel (se arma una vez por sonda)

  function phoneUi(p) {
    var ui = state.phone[p.probe_id];
    if (!ui) {
      ui = buildPhoneUi(p);
      state.phone[p.probe_id] = ui;
    }
    ui.probe = p;
    syncSlotControls(ui);
    if (!ui.statusResp && p.phone_status) {
      ui.statusResp = p.phone_status;
      ui.statusAt = Date.now();
      renderPhoneStatus(ui);
    }
    return ui;
  }

  function el(tag, cls, html) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (html !== undefined) e.innerHTML = html;
    return e;
  }

  function buildPhoneUi(p) {
    var ui = { probe: p, slotsKey: null, inflight: {}, statusResp: null, orders: null, results: null };
    var root = el("div", "phone-panel");
    ui.root = root;

    root.appendChild(el("div", "phone-panel-title", "Celular por cable"));
    ui.alertsEl = el("div", "phone-alerts");
    root.appendChild(ui.alertsEl);
    ui.statusEl = el("div", "phone-status", '<span class="muted">leyendo el estado del celular...</span>');
    root.appendChild(ui.statusEl);
    // salud de cada router y reinicio remoto (§6.4)
    ui.routersEl = el("div", "phone-routers");
    root.appendChild(ui.routersEl);
    root.addEventListener("click", onRebootClick);

    // -- controles
    var ctl = el("div", "phone-controls");
    root.appendChild(ctl);

    var row1 = el("div", "phone-ctl-row");
    ui.slotBtnsEl = el("span", "phone-slot-btns");
    row1.appendChild(ui.slotBtnsEl);
    var nextBtn = el("button", "btn-primary", "Siguiente disponible");
    nextBtn.type = "button";
    nextBtn.title = "Mide el router que lleva más tiempo sin una prueba completada";
    row1.appendChild(nextBtn);
    var fbLabel = el("label", "phone-check");
    ui.fallbackEl = document.createElement("input");
    ui.fallbackEl.type = "checkbox";
    fbLabel.appendChild(ui.fallbackEl);
    fbLabel.appendChild(document.createTextNode(" si no tiene datos, medir el otro"));
    row1.appendChild(fbLabel);
    ctl.appendChild(row1);

    var row2 = el("div", "phone-ctl-row");
    row2.appendChild(el("span", "summary-label", "Secuencia de"));
    ui.seqCountEl = document.createElement("input");
    ui.seqCountEl.type = "number";
    ui.seqCountEl.min = "2";
    ui.seqCountEl.max = "48";
    ui.seqCountEl.value = "4";
    ui.seqCountEl.className = "phone-num";
    ui.seqCountEl.setAttribute("aria-label", "Cantidad de pruebas de la secuencia");
    row2.appendChild(ui.seqCountEl);
    row2.appendChild(el("span", "summary-label", "alternadas, cada"));
    ui.seqSpacingEl = document.createElement("input");
    ui.seqSpacingEl.type = "number";
    ui.seqSpacingEl.min = "1";
    ui.seqSpacingEl.max = "1440";
    ui.seqSpacingEl.value = "5";
    ui.seqSpacingEl.className = "phone-num";
    ui.seqSpacingEl.setAttribute("aria-label", "Minutos entre pruebas de la secuencia");
    row2.appendChild(ui.seqSpacingEl);
    row2.appendChild(el("span", "summary-label", "min, empezando por"));
    ui.seqFirstEl = document.createElement("select");
    ui.seqFirstEl.setAttribute("aria-label", "Router con el que empieza la secuencia");
    row2.appendChild(ui.seqFirstEl);
    var seqBtn = el("button", "btn-primary", "Crear secuencia");
    seqBtn.type = "button";
    row2.appendChild(seqBtn);
    ctl.appendChild(row2);

    var row3 = el("div", "phone-ctl-row");
    row3.appendChild(el("span", "summary-label", "Programar"));
    ui.schedTargetEl = document.createElement("select");
    ui.schedTargetEl.setAttribute("aria-label", "Router de la prueba programada");
    row3.appendChild(ui.schedTargetEl);
    row3.appendChild(el("span", "summary-label", "a las"));
    ui.schedAtEl = document.createElement("input");
    ui.schedAtEl.type = "datetime-local";
    ui.schedAtEl.setAttribute("aria-label", "Inicio de la prueba programada");
    var start = new Date(Date.now() + 10 * 60000);
    start.setSeconds(0, 0);
    ui.schedAtEl.value = localInputValue(start);
    row3.appendChild(ui.schedAtEl);
    row3.appendChild(el("span", "summary-label", "vence a las"));
    ui.schedUntilEl = document.createElement("input");
    ui.schedUntilEl.type = "datetime-local";
    ui.schedUntilEl.setAttribute("aria-label", "Vencimiento de la prueba programada");
    ui.schedUntilEl.title = "Si el celular no la alcanza a ejecutar antes de esta hora, queda vencida. Vacío = 30 min después del inicio.";
    row3.appendChild(ui.schedUntilEl);
    var schedBtn = el("button", "btn-primary", "Programar");
    schedBtn.type = "button";
    row3.appendChild(schedBtn);
    ctl.appendChild(row3);

    var row4 = el("div", "phone-ctl-row");
    var cancelAllBtn = el("button", "btn-link btn-danger", "Cancelar pendientes");
    cancelAllBtn.type = "button";
    row4.appendChild(cancelAllBtn);
    ui.msgEl = el("span", "phone-msg muted");
    row4.appendChild(ui.msgEl);
    ctl.appendChild(row4);

    // -- órdenes y resultados
    var ordersHead = el("div", "phone-sub-head", "<span>Órdenes</span>");
    root.appendChild(ordersHead);
    ui.ordersEl = el("div", "phone-orders", '<span class="muted">cargando...</span>');
    root.appendChild(ui.ordersEl);
    root.appendChild(el("div", "phone-sub-head", "<span>Resultados</span>"));
    ui.resultsEl = el("div", "phone-results", '<span class="muted">cargando...</span>');
    root.appendChild(ui.resultsEl);

    // -- acciones
    nextBtn.addEventListener("click", function () {
      sendOrder(ui, nextBtn, { target: "next", allow_fallback: ui.fallbackEl.checked }, "Siguiente disponible");
    });

    seqBtn.addEventListener("click", function () {
      var n = parseInt(ui.seqCountEl.value, 10);
      var mins = parseFloat(ui.seqSpacingEl.value);
      if (isNaN(n) || n < 2 || n > 48) return say(ui, "La secuencia debe tener entre 2 y 48 pruebas.", true);
      if (isNaN(mins) || mins < 1 || mins > 1440) return say(ui, "La separación debe ir de 1 a 1440 minutos.", true);
      sendOrder(
        ui,
        seqBtn,
        { target: "sequence", count: n, spacing_s: Math.round(mins * 60), first: ui.seqFirstEl.value || "A" },
        "Secuencia de " + n + " alternadas cada " + mins + " min"
      );
    });

    schedBtn.addEventListener("click", function () {
      var at = isoFromLocalInput(ui.schedAtEl.value);
      var until = isoFromLocalInput(ui.schedUntilEl.value);
      if (!at) return say(ui, "Elige la fecha y hora de inicio.", true);
      if (until === undefined) return say(ui, "La hora de vencimiento no es válida.", true);
      if (until && new Date(until) <= new Date(at)) return say(ui, "La hora de vencimiento debe ser posterior al inicio.", true);
      // El campo de inicio se llena al abrir la página: si quedó en el pasado,
      // la orden vencería apenas creada (sin vencimiento = inicio + 30 min).
      var end = until ? new Date(until).getTime() : new Date(at).getTime() + 30 * 60000;
      if (end <= Date.now()) return say(ui, "Esa prueba ya estaría vencida: elige un inicio o un vencimiento en el futuro.", true);
      var target = ui.schedTargetEl.value || "A";
      sendOrder(
        ui,
        schedBtn,
        { target: target, allow_fallback: ui.fallbackEl.checked, execute_at: at, not_after: until },
        "Prueba programada (" + (target === "next" ? "siguiente disponible" : "router " + target) + ") a las " + fmtTime(at)
      );
    });

    cancelAllBtn.addEventListener("click", function () {
      if (!window.confirm("¿Cancelar todas las órdenes pendientes de " + ui.probe.probe_id + "? Las que ya se están midiendo terminan igual.")) return;
      cancelAllBtn.disabled = true;
      say(ui, "cancelando...");
      phoneApi("POST", probeBase(ui.probe) + "/orders/cancel", { all_open: true }).then(function (r) {
        cancelAllBtn.disabled = false;
        if (!r.ok) return say(ui, "No se pudo cancelar: " + r.error, true);
        var n = r.data && r.data.cancelled ? r.data.cancelled.length : 0;
        var u = r.data && r.data.unchanged ? r.data.unchanged.length : 0;
        say(ui, n ? n + " orden(es) cancelada(s)" + (u ? "; " + u + " ya estaban en curso o cerradas" : "") : "No había órdenes pendientes.");
        refreshPhoneLists(ui);
      });
    });

    return ui;
  }

  // Botones "Prueba en A/B/…" y selectores de slot: se rehacen solo si
  // cambian los slots activos de la sonda (no en cada redibujo).
  function syncSlotControls(ui) {
    var slots = activeSlots(ui.probe);
    var key = slots
      .map(function (x) {
        return x.slot + "=" + (x.t.label || x.t.device_id);
      })
      .join("|");
    if (key === ui.slotsKey) return;
    ui.slotsKey = key;

    ui.slotBtnsEl.innerHTML = "";
    if (!slots.length) ui.slotBtnsEl.innerHTML = '<span class="muted">sin equipos activos</span>';
    slots.forEach(function (x) {
      var b = el("button", "btn-primary", "Prueba en " + escapeHtml(x.slot));
      b.type = "button";
      b.title = (x.t.label || x.t.device_id) + " · tabla " + x.t.routing_table;
      b.addEventListener("click", function () {
        sendOrder(ui, b, { target: x.slot, allow_fallback: ui.fallbackEl.checked }, "Prueba en " + x.slot);
      });
      ui.slotBtnsEl.appendChild(b);
    });

    function fill(sel, withNext) {
      var prev = sel.value;
      sel.innerHTML = "";
      slots.forEach(function (x) {
        var o = document.createElement("option");
        o.value = x.slot;
        o.textContent = x.slot + (x.t.label ? " · " + x.t.label : "");
        sel.appendChild(o);
      });
      if (withNext) {
        var o = document.createElement("option");
        o.value = "next";
        o.textContent = "Siguiente disponible";
        sel.appendChild(o);
      }
      if (prev && Array.prototype.some.call(sel.options, function (op) { return op.value === prev; })) sel.value = prev;
    }
    fill(ui.seqFirstEl, false);
    fill(ui.schedTargetEl, true);
  }

  function say(ui, text, isError) {
    ui.msgEl.textContent = text || "";
    ui.msgEl.className = "phone-msg " + (isError ? "phone-msg-err" : "muted");
  }

  // sendOrder: POST .../orders con un order_id generado UNA vez por clic. Si
  // el envío falla por red o 5xx, el siguiente clic del mismo botón con los
  // mismos datos reutiliza ese order_id (el backend responde existing: true en
  // vez de duplicar). Un 409 o cualquier respuesta definitiva descarta el id.
  function sendOrder(ui, btn, body, what) {
    var p = ui.probe;
    var sig = JSON.stringify(body);
    var pend = btn._pendingOrder;
    if (!pend || pend.sig !== sig) pend = btn._pendingOrder = { sig: sig, id: newUuid() };
    var payload = Object.assign({ order_id: pend.id, duration_s: p.duration_s || 10, requested_by: "dashboard" }, body);
    btn.disabled = true;
    say(ui, what + ": enviando...");
    phoneApi("POST", probeBase(p) + "/orders", payload).then(function (r) {
      btn.disabled = false;
      if (r.ok) {
        btn._pendingOrder = null;
        var orders = (r.data && r.data.orders) || [];
        var first = orders[0];
        var when = first && first.execute_at ? fmtTime(first.execute_at) : "";
        if (r.data && r.data.existing) {
          say(ui, what + ": ya estaba creada (no se duplicó)" + (orders.length > 1 ? " · " + orders.length + " órdenes" : "") + ".");
        } else {
          say(
            ui,
            what +
              ": " +
              (orders.length > 1 ? orders.length + " órdenes en cola desde las " + when : "en cola" + (when ? " para las " + when : "")) +
              (statusOnline(ui) ? "." : " · ⚠ el celular no está reportando: se ejecuta cuando vuelva (si no vence antes).")
          );
        }
        refreshPhoneLists(ui);
        return;
      }
      if (r.status === 0 || r.status >= 500) {
        say(ui, what + ": " + r.error + " Vuelve a tocar el botón: se reenvía la misma orden, no se duplica.", true);
        return;
      }
      btn._pendingOrder = null; // 409 y demás 4xx: el próximo clic lleva un id nuevo
      if (endpointMissing(r)) {
        say(ui, what + ": el backend todavía no acepta órdenes del celular (falta POST /orders).", true);
        return;
      }
      say(ui, what + ": " + r.error, true);
    });
  }

  function cancelOrder(ui, orderId, btn) {
    btn.disabled = true;
    phoneApi("POST", probeBase(ui.probe) + "/orders/cancel", { order_ids: [orderId] }).then(function (r) {
      if (!r.ok) {
        btn.disabled = false;
        return say(ui, "No se pudo cancelar: " + r.error, true);
      }
      var ok = r.data && r.data.cancelled && r.data.cancelled.indexOf(orderId) !== -1;
      say(ui, ok ? "Orden cancelada." : "La orden ya no estaba pendiente (se está midiendo o ya cerró).");
      refreshPhoneLists(ui);
    });
  }

  // ---- refresco

  // Edad del estado HOY: la que dijo el backend más lo que pasó desde esa
  // lectura. Si el backend deja de responder, el panel igual envejece y pasa
  // a "no reporta" en vez de quedarse en "en línea (hace 4 s)".
  function statusAge(ui) {
    var ps = ui.statusResp;
    if (!ps) return null;
    var base = typeof ps.age_s === "number" ? ps.age_s : secondsSince(ps.received_at);
    if (base === null || base === undefined || isNaN(base)) return null;
    return base + (ui.statusAt ? Math.max(0, (Date.now() - ui.statusAt) / 1000) : 0);
  }

  function statusOnline(ui) {
    var ps = ui.statusResp;
    var age = statusAge(ui);
    return !!(ps && ps.status && ps.online !== false && !(age > PHONE_STALE_S));
  }

  function refreshPhoneStatus(ui) {
    if (ui.inflight.status) return;
    ui.inflight.status = true;
    var p = ui.probe;
    phoneApi("GET", probeBase(p) + "/status").then(function (r) {
      ui.inflight.status = false;
      if (r.ok) {
        ui.statusResp = r.data;
        ui.statusAt = Date.now();
        ui.statusErr = null;
      } else {
        ui.statusErr = endpointMissing(r) ? "el backend todavía no expone el estado del celular (GET /status)" : r.error;
      }
      renderPhoneStatus(ui);
    });
  }

  // Si ya hay una lectura en curso (empezó ANTES de crear o cancelar una
  // orden), se pide otra al terminar: si no, la tabla mostraría la respuesta
  // vieja hasta el próximo refresco.
  function refreshPhoneLists(ui) {
    var p = ui.probe;
    if (ui.inflight.orders) ui.againOrders = true;
    else {
      ui.inflight.orders = true;
      ui.againOrders = false;
      phoneApi("GET", probeBase(p) + "/orders?view=history&limit=" + PHONE_LIST_LIMIT).then(function (r) {
        ui.inflight.orders = false;
        var again = ui.againOrders;
        ui.againOrders = false;
        if (r.ok) {
          ui.orders = (r.data && r.data.orders) || [];
          ui.ordersErr = null;
        } else {
          ui.ordersErr = endpointMissing(r) ? "el backend todavía no tiene el historial de órdenes (GET /orders)" : r.error;
        }
        renderPhoneOrders(ui);
        if (again) refreshPhoneLists(ui);
      });
    }
    if (ui.inflight.results) ui.againResults = true;
    else {
      ui.inflight.results = true;
      ui.againResults = false;
      phoneApi("GET", "/api/v1/measurements?probe_id=" + encodeURIComponent(p.probe_id) + "&limit=" + PHONE_LIST_LIMIT).then(function (r) {
        ui.inflight.results = false;
        var again = ui.againResults;
        ui.againResults = false;
        if (r.ok) {
          // Un backend sin el filtro probe_id devolvería TODAS las mediciones:
          // se filtra también acá.
          ui.results = (Array.isArray(r.data) ? r.data : []).filter(function (m) {
            return m && m.probe_id === p.probe_id;
          });
          ui.resultsErr = null;
        } else {
          ui.resultsErr = r.error;
        }
        renderPhoneResults(ui);
        if (again) refreshPhoneLists(ui);
      });
    }
  }

  function phoneTick() {
    if (document.hidden || !state.apiKey) return;
    // En el detalle de un equipo solo se refresca la sonda de celular que lo
    // mide (salud del router y progreso de un reinicio, §6.4).
    var only = null;
    if (state.view !== "list") {
      var dm = state.selectedDeviceId ? phoneProbeForDevice(state.selectedDeviceId) : null;
      if (!dm) return;
      only = dm.probe.probe_id;
    }
    state.phoneTick++;
    var lists = state.phoneTick % PHONE_LISTS_EVERY === 0;
    (state.probes || []).forEach(function (p) {
      if (!isPhoneProbe(p)) return;
      if (only !== null && p.probe_id !== only) return;
      var ui = state.phone[p.probe_id];
      if (!ui) return;
      refreshPhoneStatus(ui);
      if (lists) refreshPhoneLists(ui);
    });
  }

  function syncPhoneTimer() {
    var any = (state.probes || []).some(isPhoneProbe);
    if (any && !state.phoneTimer) state.phoneTimer = setInterval(phoneTick, PHONE_TICK_MS);
    if (!any && state.phoneTimer) {
      clearInterval(state.phoneTimer);
      state.phoneTimer = null;
    }
  }

  // Al volver a la pestaña, no esperar al próximo tick.
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) {
      state.phoneTick = PHONE_LISTS_EVERY - 1;
      phoneTick();
    }
  });

  // ---- dibujo: estado en vivo

  function renderPhoneStatus(ui) {
    var p = ui.probe;
    var ps = ui.statusResp;
    var s = ps && ps.status;
    ui.alertsEl.innerHTML = "";
    if (!s) {
      ui.root.classList.remove("phone-stale");
      ui.statusEl.innerHTML =
        '<span class="muted">' +
        escapeHtml(
          ui.statusErr
            ? "No se pudo leer el estado del celular: " + ui.statusErr
            : 'El celular todavía no reportó su estado. Actívalo en la app ("Sonda A/B") con esta sonda (' + p.probe_id + ")."
        ) +
        "</span>";
      renderRebootViews();
      return;
    }
    var age = statusAge(ui);
    var stale = ps.online === false || (age !== null && age > PHONE_STALE_S);
    ui.root.classList.toggle("phone-stale", stale);

    // alertas del celular, arriba del panel (mismos textos que la app)
    (s.alerts || []).forEach(function (a) {
      var since = a.since ? secondsSince(a.since) : null;
      var div = el(
        "div",
        "phone-alert " + (a.level === "error" ? "phone-alert-error" : "phone-alert-warn"),
        escapeHtml(a.msg || a.code) + (since !== null ? ' <span class="muted">· desde hace ' + fmtDur(since) + "</span>" : "")
      );
      div.title = a.code || "";
      ui.alertsEl.appendChild(div);
    });

    var h = [];
    // cabecera: en línea / sin reportar
    var head = stale
      ? '<span class="online-badge online-no"><span class="dot"></span>el celular no reporta hace ' + fmtDur(age) + "</span>" +
        ' <span class="muted">(último estado conocido, no el actual)</span>'
      : '<span class="online-badge online-yes"><span class="dot"></span>en línea (hace ' + fmtDur(age) + ")</span>";
    head +=
      ' <span class="muted">' +
      escapeHtml(s.measured_by || "celular") +
      (s.app_version ? " · app " + escapeHtml(s.app_version) : "") +
      "</span>";
    if (s.running === false) head += " " + badge("st-muted", "SONDA DETENIDA EN LA APP");
    else if (s.enabled === false) head += " " + badge("st-muted", "SONDA DESACTIVADA");
    h.push('<div class="phone-head">' + head + "</div>");

    // fase
    var phase = s.phase || (s.running === false ? "stopped" : "");
    var inPhase = null;
    if (s.phase_since && s.sent_at) {
      var d = (new Date(s.sent_at) - new Date(s.phase_since)) / 1000;
      if (!isNaN(d)) inPhase = d + (age || 0);
    }
    h.push(
      '<div class="phone-phase">' +
        '<span class="phone-phase-name phase-' + escapeHtml(phase) + '">' +
          escapeHtml(PHASE_ES[phase] || (/^reboot/.test(phase) ? "Reiniciando un router" : phase) || "—") +
          "</span>" +
        (s.phase_detail ? ' <span class="phone-phase-detail">' + escapeHtml(s.phase_detail) + "</span>" : "") +
        (inPhase !== null ? ' <span class="muted">· ' + fmtDur(inPhase) + " en esta fase</span>" : "") +
        "</div>"
    );
    var stepIdx = -1;
    PHASE_STEPS.forEach(function (st, i) {
      if (st[0] === phase) stepIdx = i;
    });
    // (un reinicio muestra sus propios pasos en "Routers: salud y reinicio")
    if (stepIdx !== -1 || (s.current_order && s.current_order.type !== "reboot_router")) {
      h.push(
        '<div class="phone-steps">' +
          PHASE_STEPS.map(function (st, i) {
            var cls = i === stepIdx ? "step-now" : stepIdx !== -1 && i < stepIdx ? "step-done" : "";
            return '<span class="phone-step ' + cls + '">' + escapeHtml(st[1]) + "</span>";
          }).join("") +
          "</div>"
      );
    }

    // orden en curso
    var co = s.current_order;
    if (co) {
      var slot = co.slot || (co.target !== "next" && co.target !== "wifi" ? co.target : null);
      var who = co.type === "reboot_router"
        ? "Reiniciando router " + (slot || "?") + (co.label || slotLabel(p, slot) ? " (" + (co.label || slotLabel(p, slot)) + ")" : "") + " por SSH"
        : co.net_path === "wifi" || co.target === "wifi"
        ? "Midiendo por WiFi (no se atribuye a ningún router)"
        : slot
          ? "Midiendo router " + slot + (co.label || slotLabel(p, slot) ? " (" + (co.label || slotLabel(p, slot)) + ")" : "")
          : "Eligiendo router (siguiente disponible)";
      var bits = [escapeHtml(who)];
      if (co.routing_table) bits.push("tabla <code>" + escapeHtml(co.routing_table) + "</code>");
      bits.push(escapeHtml(selectionEs(co.type === "reboot_router" ? rebootReasonOf(co) : co.selection_reason)) + (co.requested_by ? ' <span class="muted">(' + escapeHtml(co.requested_by) + ")</span>" : ""));
      if (co.attempt > 1) bits.push(badge("st-warn", "INTENTO " + co.attempt));
      if (co.started_at) bits.push('<span class="muted">desde ' + fmtTime(co.started_at) + "</span>");
      h.push('<div class="phone-current">' + bits.join(" · ") + "</div>");
    }

    // tarjetas
    var cards = [];
    cards.push(mikrotikCard(p, s.mikrotik, s.net));
    cards.push(gpsCard(s.gps));
    cards.push(queueCard(s.queue, s.local_plan, s.backend));
    cards.push(netCard(s.net, s.backend));
    cards.push(batteryCard(s.battery, s.permissions));
    cards.push(lastResultCard(p, s.last_result));
    h.push('<div class="phone-grid">' + cards.join("") + "</div>");
    if (ui.statusErr) h.push('<div class="muted">⚠ último intento de lectura falló: ' + escapeHtml(ui.statusErr) + "</div>");

    ui.statusEl.innerHTML = h.join("");
    renderRebootViews();
  }

  function card(title, rows) {
    return (
      '<div class="phone-card"><div class="phone-card-title">' +
      escapeHtml(title) +
      "</div>" +
      rows
        .filter(function (r) {
          return r;
        })
        .map(function (r) {
          return '<div class="phone-row">' + r + "</div>";
        })
        .join("") +
      "</div>"
    );
  }

  function kv(k, v) {
    return '<span class="phone-k">' + escapeHtml(k) + "</span> " + v;
  }

  function mikrotikCard(p, mk, net) {
    if (!mk) return card("MikroTik", ['<span class="muted">sin datos</span>']);
    var rows = [];
    var reach = mk.reachable === true ? badge("st-ok", "ALCANZABLE") : mk.reachable === false ? badge("st-err", "NO ALCANZABLE") : badge("st-muted", "—");
    rows.push(kv("Estado", reach + (mk.host ? ' <span class="muted">' + escapeHtml(mk.host) + "</span>" : "")));
    var tableCls = mk.current_table === "main" ? "st-ok" : mk.current_table ? "st-info" : "st-muted";
    rows.push(
      kv(
        "Regla",
        badge(tableCls, tableEs(p, mk.current_table), mk.current_table || "") +
          (mk.rule_found === false ? " " + badge("st-err", "REGLA phone-probe NO EXISTE") : "")
      )
    );
    // estado de cada router según sus rutas (enlace arriba/caído; si tiene
    // datos lo dice la última prueba, no la ruta)
    var routes = mk.routes || {};
    var mainRoute = routes.main;
    var mainSlot = null;
    activeSlots(p).forEach(function (x) {
      var r = routes[x.t.routing_table];
      var txt;
      if (!r) txt = '<span class="muted">sin ruta leída</span>';
      else if (r.active) txt = badge("st-ok", "enlace arriba") + (r.gateway ? " · " + escapeHtml(gatewayIp(r.gateway)) : "");
      else txt = badge("st-err", "enlace caído");
      if (r && r.gateway) txt += ' <span class="muted">(' + escapeHtml(gatewayIface(r.gateway) || r.gateway) + ")</span>";
      rows.push(kv(x.slot + ":", txt));
      if (r && mainRoute && mainRoute.gateway) {
        if (r.gateway === mainRoute.gateway || (gatewayIface(r.gateway) && gatewayIface(r.gateway) === gatewayIface(mainRoute.gateway)))
          mainSlot = x.slot;
      }
    });
    if (mainRoute) {
      rows.push(
        kv(
          "Respaldo",
          mainRoute.active === false
            ? badge("st-err", "sin ruta activa")
            : mainSlot
              ? "sale hoy por " + mainSlot
              : "sale por " + escapeHtml(mainRoute.gateway || "—")
        )
      );
    }
    if (mk.identity || mk.version)
      rows.push(kv("Equipo", escapeHtml([mk.identity, mk.version ? "RouterOS " + mk.version : ""].filter(Boolean).join(" · "))));
    if (mk.error) rows.push('<span class="phone-err">' + escapeHtml(mk.error) + "</span>");
    rows.push(kv("Última lectura", mk.last_ok ? fmtTime(mk.last_ok) + ' <span class="muted">' + fmtAgo(secondsSince(mk.last_ok)) + "</span>" : "nunca"));
    return card("MikroTik", rows);
  }

  function gpsCard(g) {
    if (!g) return card("GPS", ['<span class="muted">sin datos</span>']);
    var rows = [gpsBadge(g.status, g.age_s)];
    if (has(g.accuracy_m) && isFinite(Number(g.accuracy_m))) rows.push(kv("Precisión", "±" + Math.round(Number(g.accuracy_m)) + " m"));
    if (has(g.satellites_used)) rows.push(kv("Satélites", escapeHtml(String(g.satellites_used))));
    if (g.source) rows.push(kv("Fuente", escapeHtml(g.source)));
    var lat = Number(g.lat);
    var lon = Number(g.lon);
    if (has(g.lat) && has(g.lon) && isFinite(lat) && isFinite(lon)) rows.push(kv("Posición", mapsLink(lat, lon, null)));
    if (g.at) rows.push(kv("Fijo", fmtTime(g.at)));
    return card("GPS", rows);
  }

  function queueCard(q, lp, be) {
    var rows = [];
    q = q || {};
    rows.push(
      kv("Órdenes pendientes", escapeHtml(String(has(q.orders_pending) ? q.orders_pending : "—"))) +
        (q.next_order_at ? ' <span class="muted">· próxima ' + fmtTime(q.next_order_at) + (q.next_order_target ? " (" + escapeHtml(q.next_order_target) + ")" : "") + "</span>" : "")
    );
    rows.push(
      kv(
        "Resultados por subir",
        q.results_pending > 0 ? badge("st-warn", String(q.results_pending)) : escapeHtml(String(has(q.results_pending) ? q.results_pending : "—"))
      )
    );
    if (q.results_rejected > 0) rows.push(kv("Rechazados", badge("st-err", String(q.results_rejected))));
    if (q.outbox_pending > 0) rows.push(kv("Cambios de estado por enviar", escapeHtml(String(q.outbox_pending))));
    if (lp && lp.active)
      rows.push(
        badge("st-warn", "SIN BACKEND: PLAN LOCAL") +
          (lp.interval_s ? " cada " + fmtDur(lp.interval_s) : "") +
          (lp.next_at ? ' <span class="muted">· próxima ' + fmtTime(lp.next_at) + "</span>" : "")
      );
    return card("Cola", rows);
  }

  function netCard(n, be) {
    var rows = [];
    n = n || {};
    var eth = n.ethernet || {};
    var wifi = n.wifi || {};
    rows.push(
      kv(
        "Ethernet",
        (eth.up ? badge("st-ok", "conectado") : badge(n.require_ethernet ? "st-err" : "st-muted", "sin cable")) +
          (eth.ip ? " " + escapeHtml(eth.ip) : "") +
          (eth.up && eth.validated === false ? ' <span class="muted">(sin Internet validado)</span>' : "")
      )
    );
    rows.push(kv("WiFi", (wifi.up ? escapeHtml(wifi.ip || "conectado") : '<span class="muted">apagado / sin red</span>')));
    rows.push(kv("Exigir Ethernet", yesNo(n.require_ethernet)));
    if (n.require_ethernet && !eth.up) rows.push('<span class="phone-err">Se exige Ethernet y no hay cable: las órdenes esperan.</span>');
    if (n.default_network) rows.push(kv("Red por defecto", escapeHtml(n.default_network)));
    rows.push(kv("Camino al backend", escapeHtml(n.control_path || "—")));
    if (be) {
      if (be.reachable === false) rows.push(kv("Backend", badge("st-err", "no alcanzable") + (be.last_error ? ' <span class="muted">' + escapeHtml(be.last_error) + "</span>" : "")));
      if (be.last_ok) rows.push(kv("Último contacto", fmtTime(be.last_ok)));
      if (has(be.clock_skew_s) && Math.abs(be.clock_skew_s) > 5)
        rows.push(kv("Reloj", badge("st-warn", "desfase " + Number(be.clock_skew_s).toFixed(1) + " s")));
    }
    return card("Red", rows);
  }

  var BATTERY_ES = { discharging: "descargando", charging: "cargando", full: "llena", not_charging: "sin cargar" };

  function batteryCard(b, perms) {
    var rows = [];
    if (b) {
      var pct = has(b.pct) ? b.pct + " %" : "—";
      var low = has(b.pct) && b.pct < 20 && b.status !== "charging";
      rows.push(kv("Carga", low ? badge("st-err", pct) : escapeHtml(pct)) + (b.status ? ' <span class="muted">' + escapeHtml(BATTERY_ES[b.status] || b.status) + "</span>" : ""));
      if (b.plugged && b.plugged !== "none") rows.push(kv("Conectado a", escapeHtml(b.plugged)));
      if (has(b.temp_c)) rows.push(kv("Temperatura", Number(b.temp_c).toFixed(1) + " °C"));
      if (b.optimization_ignored === false) rows.push(badge("st-warn", "OPTIMIZACIÓN DE BATERÍA ACTIVA"));
    } else rows.push('<span class="muted">sin datos</span>');
    if (perms) {
      var miss = [];
      if (perms.location === false) miss.push("ubicación");
      if (perms.background_location === false) miss.push("ubicación todo el tiempo");
      if (perms.notifications === false) miss.push("notificaciones");
      if (miss.length) rows.push('<span class="phone-err">Faltan permisos: ' + escapeHtml(miss.join(", ")) + "</span>");
    }
    return card("Batería", rows);
  }

  var SYNC_ES = { pending: ["Pendiente", "st-warn"], synced: ["Subido", "st-ok"], rejected: ["Rechazado", "st-err"] };

  function lastResultCard(p, r) {
    if (!r) return card("Última prueba", ['<span class="muted">ninguna todavía</span>']);
    var rows = [];
    rows.push(
      kv("Hora", fmtTime(r.at)) +
        " · " +
        (r.slot ? "router " + escapeHtml(r.slot) : '<span class="muted">sin atribuir</span>')
    );
    rows.push(r.test_status === "failed" ? badge("st-err", "FALLIDA") : r.test_status === "done" ? badge("st-ok", "COMPLETADA") : "");
    rows.push("↓ " + fmtNum(r.down_mbps, " Mbps") + " · ↑ " + fmtNum(r.up_mbps, " Mbps") + " · ping " + fmtNum(r.ping_ms, " ms", 0));
    var sync = SYNC_ES[r.sync_status];
    if (sync) rows.push(badge(sync[1], sync[0]) + (r.server_reason ? ' <span class="muted">' + escapeHtml(phoneReasonEs(r.server_reason)) + "</span>" : ""));
    if (r.location_status) rows.push(gpsBadge(r.location_status, null));
    if (r.error) rows.push('<span class="phone-err">' + escapeHtml(r.error) + "</span>");
    return card("Última prueba", rows);
  }

  // ---- dibujo: órdenes

  function renderPhoneOrders(ui) {
    var p = ui.probe;
    if (!ui.orders) {
      ui.ordersEl.innerHTML = '<span class="muted">' + escapeHtml(ui.ordersErr || "cargando...") + "</span>";
      return;
    }
    var h = [];
    if (ui.ordersErr) h.push('<div class="muted">⚠ ' + escapeHtml(ui.ordersErr) + "</div>");
    if (!ui.orders.length) {
      h.push('<span class="muted">Todavía no hay órdenes para esta sonda.</span>');
      ui.ordersEl.innerHTML = h.join("");
      return;
    }
    h.push(
      '<div class="table-wrap"><table class="data-table phone-table"><thead><tr>' +
        "<th>Ejecutar</th><th>Objetivo</th><th>Motivo</th><th>Estado</th><th>Detalle</th><th></th>" +
        "</tr></thead><tbody></tbody></table></div>"
    );
    ui.ordersEl.innerHTML = h.join("");
    var tbody = ui.ordersEl.querySelector("tbody");
    ui.orders.forEach(function (o) {
      var tr = document.createElement("tr");
      var reboot = isRebootOrder(o);
      var target =
        o.target === "next"
          ? "Siguiente" + (o.slot ? " → " + o.slot : "")
          : o.target || o.slot || "—";
      if (reboot) target = "Reiniciar " + (o.slot || o.target || "");
      var lbl = o.slot ? slotLabel(p, o.slot) : "";
      var seq = o.batch_id && o.order_id && o.batch_id !== o.order_id ? /-(\d{1,2})$/.exec(o.order_id) : null;
      var detail = [];
      if (o.routing_table) detail.push("tabla <code>" + escapeHtml(o.routing_table) + "</code>");
      if (o.allow_fallback) detail.push("con respaldo");
      if (o.not_after && (o.status === "pending" || o.status === "delivered")) detail.push("vence " + fmtTime(o.not_after));
      if (o.completed_at) detail.push("cerró " + fmtTime(o.completed_at));
      if (o.measurement_id) detail.push("medición #" + escapeHtml(String(o.measurement_id)));
      if (reboot) {
        detail = ['<span class="reboot-progress">' + rebootProgressHtml(o, { probe: p, slot: o.slot, target: targetBySlot(p, o.slot) }) + "</span>"];
        if (o.completed_at) detail.push("cerró " + fmtTime(o.completed_at));
      } else if (o.error) detail.push('<span class="phone-err">' + escapeHtml(o.error) + "</span>");
      tr.innerHTML =
        '<td data-label="Ejecutar">' +
        fmtTime(o.execute_at || o.created_at) +
        (o.started_at ? ' <span class="muted">(empezó ' + fmtTime(o.started_at) + ")</span>" : "") +
        '</td><td data-label="Objetivo"><strong>' +
        escapeHtml(target) +
        "</strong>" +
        (lbl ? ' <span class="muted">' + escapeHtml(lbl) + "</span>" : "") +
        '</td><td data-label="Motivo">' +
        escapeHtml(selectionEs(reboot ? rebootReasonOf(o) : o.selection_reason)) +
        (seq ? ' <span class="muted">#' + escapeHtml(seq[1]) + "</span>" : "") +
        '</td><td data-label="Estado">' +
        (reboot ? rebootStatusBadge(o) : orderStatusBadge(o)) +
        (o.closed_by === "server" ? ' <span class="muted">(cerrada por el servidor)</span>' : "") +
        '</td><td data-label="Detalle">' +
        (detail.join(" · ") || "—") +
        '</td><td data-label=""></td>';
      if (o.status === "pending" || o.status === "delivered") {
        var b = el("button", "btn-link btn-danger", "Cancelar");
        b.type = "button";
        b.addEventListener("click", function () {
          cancelOrder(ui, o.order_id, b);
        });
        tr.lastElementChild.appendChild(b);
      }
      tbody.appendChild(tr);
    });
    renderRebootViews();
  }

  // ---- dibujo: resultados (trazabilidad por medición)

  function fmtBytes(n) {
    if (!has(n)) return "—";
    if (n >= 1e9) return (n / 1e9).toFixed(1) + " GB";
    if (n >= 1e6) return (n / 1e6).toFixed(1) + " MB";
    if (n >= 1e3) return (n / 1e3).toFixed(0) + " kB";
    return n + " B";
  }

  function renderPhoneResults(ui) {
    var p = ui.probe;
    if (!ui.results) {
      ui.resultsEl.innerHTML = '<span class="muted">' + escapeHtml(ui.resultsErr || "cargando...") + "</span>";
      return;
    }
    var h = [];
    if (ui.resultsErr) h.push('<div class="muted">⚠ ' + escapeHtml(ui.resultsErr) + "</div>");
    if (!ui.results.length) {
      h.push('<span class="muted">Todavía no hay resultados subidos por el celular.</span>');
      ui.resultsEl.innerHTML = h.join("");
      return;
    }
    var rows = ui.results.map(function (m) {
      var net = m.net || {};
      // router
      var router;
      if (m.slot) {
        router = "<strong>" + escapeHtml(m.slot) + "</strong>" + (slotLabel(p, m.slot) ? ' <span class="muted">' + escapeHtml(slotLabel(p, m.slot)) + "</span>" : "");
      } else {
        router = badge("st-muted", "SIN ATRIBUIR") + (m.net_path === "wifi" ? ' <span class="muted">por WiFi</span>' : "");
      }
      if (m.routing_table) router += '<br><span class="muted">tabla</span> <code>' + escapeHtml(m.routing_table) + "</code>";
      if (m.device_id_client && m.device_id_client !== m.device_id)
        router += '<br><span class="muted">el celular decía ' + escapeHtml(m.device_id_client) + "</span>";

      // estado
      var st = [];
      st.push(m.test_status === "failed" ? badge("st-err", "PRUEBA FALLIDA") : badge("st-ok", "PRUEBA COMPLETADA"));
      st.push(gpsBadge(m.location_status, m.gps_age_s));
      if (m.captive_portal === true)
        st.push(badge("st-err", "PORTAL CAUTIVO" + (m.portal_host ? " · " + m.portal_host : ""), "la SIM del router pide registro o saldo; no es una caída de señal"));
      if (m.error) st.push('<span class="phone-err">' + escapeHtml(m.error) + "</span>");

      // veredicto
      var reason = net.reason || "";
      var verdict = routeBadge(m) + " " + '<span class="muted">' + escapeHtml(phoneReasonEs(reason) || "sin veredicto") + "</span>";
      if (reason === "probe-asn-mismatch" || reason === "probe-asn-other-router") {
        verdict +=
          "<br>" +
          badge(
            "st-err",
            "⚠ OPERADOR NO COINCIDE",
            "ASN de salida " + (net.egress_asn || "?") + ", esperado " + (net.expected_asn || "?")
          ) +
          ' <span class="muted">' +
          escapeHtml(asnText(net.egress_asn, net.egress_asn_name) || "AS?") +
          (net.expected_asn ? " · esperado AS" + escapeHtml(String(net.expected_asn)) : "") +
          "</span>";
      } else if (net.egress_asn || net.egress_asn_name) {
        verdict += '<br><span class="muted">' + escapeHtml(asnText(net.egress_asn, net.egress_asn_name)) + "</span>";
      }
      if (m.egress_ip) verdict += ' <span class="muted">· ' + escapeHtml(m.egress_ip) + "</span>";
      if (has(m.other_traffic_bytes) && m.other_traffic_bytes > OTHER_TRAFFIC_WARN_BYTES)
        verdict += "<br>" + badge("st-warn", "otras apps usaron la red durante la prueba", fmtBytes(m.other_traffic_bytes) + " de otro tráfico");

      // origen y sincronización
      var src = [];
      if (m.executed_offline)
        src.push(badge("st-warn", "hecha sin Internet, sincronizada a las " + fmtTime(m._received_at)));
      else src.push('<span class="muted">subida ' + fmtTime(m._received_at) + "</span>");
      src.push(escapeHtml(selectionEs(m.selection_reason)));
      if (m.attempt > 1) src.push(badge("st-warn", "intento " + m.attempt));
      if (m.related_order_id) src.push('<span class="muted">intento previo al respaldo</span>');
      if (m.order_id) src.push('<span class="muted" title="' + escapeHtml(m.order_id) + '">orden ' + escapeHtml(String(m.order_id).slice(0, 8)) + "</span>");
      if (m.measured_by) src.push('<span class="muted">' + escapeHtml(m.measured_by) + "</span>");

      return (
        "<tr>" +
        '<td data-label="Prueba">' + fmtTime(m.test_started_at || m.ts) + "</td>" +
        '<td data-label="Router">' + router + "</td>" +
        '<td data-label="↓ / ↑">' + fmtNum(m.down_mbps) + " / " + fmtNum(m.up_mbps) + ' <span class="muted">Mbps' +
        (m.target_kind === "fast" ? " · fast.com" + (m.streams ? " ×" + escapeHtml(String(m.streams)) : "") : "") + "</span>" +
        (m.cf_down_mbps != null || m.cf_up_mbps != null
          ? '<br><span class="muted">Cloudflare ×1: ' + fmtNum(m.cf_down_mbps) + " / " + fmtNum(m.cf_up_mbps) + "</span>"
          : "") +
        "</td>" +
        '<td data-label="Ping">' + fmtPing(m.ping_ms, m.loss_pct) + "</td>" +
        '<td data-label="Estado">' + st.join(" ") + "</td>" +
        '<td data-label="Veredicto">' + verdict + "</td>" +
        '<td data-label="Origen">' + src.join(" · ") + "</td>" +
        "</tr>"
      );
    });
    h.push(
      '<div class="table-wrap"><table class="data-table phone-table"><thead><tr>' +
        "<th>Prueba</th><th>Router</th><th>↓ / ↑</th><th>Ping</th><th>Estado</th><th>Veredicto</th><th>Origen</th>" +
        "</tr></thead><tbody>" +
        rows.join("") +
        "</tbody></table></div>"
    );
    ui.resultsEl.innerHTML = h.join("");
  }

  // ---------------------------------------------------------------- reinicio remoto de un router y salud por router
  //
  // Contrato §6. El backend no reinicia nada: crea una orden reboot_router
  // para el celular de la sonda, y el celular le manda `reboot` por SSH al
  // router a través del MikroTik (camino segregado, no toca las mediciones).
  // Acá se ve:
  //   - la salud de cada router según el celular (`status.routers.A/B`: ping a
  //     Internet por la tabla del router y a su puerta de enlace),
  //   - la recomendación del backend (`reboot` por equipo en GET /devices y en
  //     los targets de GET /probes): > 10 min sin conexión,
  //   - el botón "Reiniciar" (una confirmación si está recomendado, dos si no),
  //   - el progreso de la orden (recibida → SSH → esperando que vuelva → listo)
  //     y el último reinicio.
  // Lo que viene del celular (routers, fase, errores, huella SSH) se escapa
  // siempre: el backend lo guarda tal cual lo mandó.
  //
  // Se muestra en tres lugares con el mismo HTML: la columna "Estado" de la
  // lista de equipos, la caja "Reinicio remoto" del detalle y el bloque
  // "Routers" del panel del celular. Los botones usan delegación de eventos
  // (data-reboot-device), porque esos tres lugares se redibujan seguido.

  var HEALTH_STALE_S = 300; // una verificación más vieja que esto ya no es "la actual"
  var REBOOT_POLL_MS = 5000;
  var REBOOT_TRACK_MAX_MS = 25 * 60 * 1000; // not_after (15 min) + vigilancia del celular (5 min) + margen
  var REBOOT_STEPS = [
    ["delivered", "Recibida"],
    ["ssh", "SSH"],
    ["waiting", "Esperando que vuelva"],
    ["done", "Listo"],
  ];
  var REBOOT_ERROR_ES = {
    "ssh-auth": "el router rechazó el usuario o la clave SSH",
    "ssh-connect": "no se pudo conectar por SSH al router (¿apagado o sin LAN?)",
    "no-ethernet": "el celular no tiene el cable de red conectado",
    "mikrotik-unreachable": "el celular no alcanzó el MikroTik",
    "timeout-back": "el router no volvió en 5 min",
    "vencida-sin-ejecutar": "venció sin que el celular la ejecutara",
    "sin-cierre": "el celular no la cerró",
    "slot-desconocido": "el celular no tiene ese router en su sonda",
    vencida: "venció en el celular sin ejecutarse",
    cancelada: "cancelada desde el dashboard",
  };
  var REBOOT_STATUS_ES = {
    pending: ["En cola", "st-muted"],
    delivered: ["Recibida por el celular", "st-info"],
    claimed: ["Recibida por el celular", "st-info"],
    running: ["Reiniciando", "st-run"],
    done: ["Reinicio completado", "st-ok"],
    failed: ["Reinicio fallido", "st-err"],
    expired: ["Vencida", "st-warn"],
    interrupted: ["Interrumpida", "st-warn"],
    cancelled: ["Cancelada", "st-muted"],
  };
  SELECTION_ES.manual = "reinicio manual";
  SELECTION_ES.recommended = "reinicio recomendado";

  state.reboot = {}; // device_id -> {pending, orderId, order, since, msg, isError, inflight}
  state.rebootTimer = null;

  function isRebootOrder(o) {
    return !!o && o.type === "reboot_router";
  }

  // Busca solo claves propias: `error`/`status` vienen del celular y un
  // "constructor" no puede traer Object.prototype.
  function own(map, k) {
    return k !== null && k !== undefined && Object.prototype.hasOwnProperty.call(map, k) ? map[k] : undefined;
  }

  function rebootErrorEs(e) {
    return e ? own(REBOOT_ERROR_ES, e) || String(e) : "";
  }

  // Motivo de una orden de reinicio: el backend lo expone como reboot_reason.
  function rebootReasonOf(o) {
    return o ? o.reboot_reason || o.reason || o.selection_reason || null : null;
  }

  function isOpenStatus(st) {
    return st === "pending" || st === "delivered" || st === "claimed" || st === "running";
  }

  function num(v) {
    if (v === null || v === undefined || v === "") return null;
    var n = Number(v);
    return isFinite(n) ? n : null;
  }

  // Sonda de celular que tiene este equipo como target (la que puede
  // reiniciarlo: el backend responde 404 si no hay ninguna).
  function phoneProbeForDevice(deviceId) {
    var ps = state.probes || [];
    for (var i = 0; i < ps.length; i++) {
      var p = ps[i];
      if (!isPhoneProbe(p)) continue;
      var tw = probeTargetsWithSlots(p).filter(function (x) {
        return x.t.device_id === deviceId;
      })[0];
      if (tw) return { probe: p, target: tw.t, slot: tw.slot };
    }
    return null;
  }

  // El estado del celular más fresco que se tenga: el del panel (se lee cada
  // 5 s) o, si el panel todavía no existe, el que vino en GET /probes.
  function phoneStatusFor(p) {
    var ui = state.phone[p.probe_id];
    var ps = null;
    var age = null;
    if (ui && ui.statusResp) {
      ps = ui.statusResp;
      age = statusAge(ui);
    } else if (p.phone_status) {
      ps = p.phone_status;
      var base = typeof ps.age_s === "number" ? ps.age_s : secondsSince(ps.received_at);
      if (base !== null && base !== undefined && !isNaN(base))
        age = base + (state.probesAt ? Math.max(0, (Date.now() - state.probesAt) / 1000) : 0);
    }
    var s = ps && ps.status ? ps.status : null;
    var stale = !s || ps.online === false || (age !== null && age > PHONE_STALE_S);
    return { s: s, age: age, stale: stale };
  }

  // Segundos entre una hora del celular y AHORA sin mezclar relojes: la
  // diferencia se mide contra `sent_at` (mismo reloj, el del celular) y se le
  // suma la edad del estado (medida por el servidor). Sin sent_at, contra el
  // reloj de este navegador.
  function phoneSecondsSince(st, iso) {
    if (!iso) return null;
    var t = new Date(iso).getTime();
    if (isNaN(t)) return null;
    if (st.s && st.s.sent_at) {
      var sent = new Date(st.s.sent_at).getTime();
      if (!isNaN(sent)) return Math.max(0, (sent - t) / 1000 + (st.age || 0));
    }
    return Math.max(0, (Date.now() - t) / 1000);
  }

  // Salud de un slot según el celular: {h, st, checkedAgo, old}.
  function routerHealth(p, slot) {
    var st = phoneStatusFor(p);
    var routers = st.s && st.s.routers && typeof st.s.routers === "object" ? st.s.routers : null;
    var h = routers && Object.prototype.hasOwnProperty.call(routers, slot) ? routers[slot] : null;
    if (!h || typeof h !== "object") return { h: null, st: st, checkedAgo: null, old: true };
    var ago = phoneSecondsSince(st, h.checked_at);
    return { h: h, st: st, checkedAgo: ago, old: st.stale || ago === null || ago > HEALTH_STALE_S };
  }

  // HTML seguro con la salud de un router. compact = una línea (lista).
  function healthHtml(rh, compact) {
    var h = rh.h;
    if (!h) return compact ? "" : '<span class="muted">el celular todavía no reportó la salud de este router</span>';
    var parts = [];
    var rtt = num(h.rtt_ms);
    var loss = num(h.loss_pct);
    if (h.internet_ok === true) {
      parts.push(
        badge("st-ok", "INTERNET OK", "ping por la tabla del router desde el MikroTik") +
          (rtt !== null ? " " + escapeHtml(rtt.toFixed(0)) + " ms" : "") +
          (loss ? ' <span class="muted">(' + escapeHtml(loss.toFixed(0)) + " % pérdida)</span>" : "")
      );
    } else if (h.internet_ok === false) {
      var down = phoneSecondsSince(rh.st, h.down_since);
      parts.push(badge("st-err", "SIN INTERNET" + (down !== null ? " hace " + fmtDur(down) : "")));
    } else {
      parts.push(badge("st-muted", "SALUD SIN VERIFICAR"));
    }
    if (h.gateway_ok === false) {
      parts.push(badge("st-err", "ROUTER NO RESPONDE", "tampoco responde el ping a su puerta de enlace: parece apagado o sin cable"));
    } else if (h.gateway_ok === true && h.internet_ok === false && !compact) {
      parts.push('<span class="muted">el router responde en la LAN (el problema es de datos)</span>');
    }
    var when = rh.checkedAgo !== null ? "verificado hace " + fmtDur(rh.checkedAgo) : "sin hora de verificación";
    if (rh.old) parts.push('<span class="muted">· ' + escapeHtml(when) + " (último dato conocido)</span>");
    else if (!compact) parts.push('<span class="muted">· ' + escapeHtml(when) + "</span>");
    return '<span class="' + (rh.old ? "health-old" : "") + '">' + parts.join(" ") + "</span>";
  }

  // Recomendación del backend: la del target de la sonda o la del equipo.
  function rebootInfo(deviceId, target) {
    var rb = target && target.reboot && typeof target.reboot === "object" ? target.reboot : null;
    if (!rb) {
      var d = deviceById(deviceId);
      rb = d && d.reboot && typeof d.reboot === "object" ? d.reboot : null;
    }
    return rb;
  }

  function recommendHtml(rb) {
    if (!rb || !rb.recommended) return "";
    // offline_min lo calcula el backend con su reloj (el del navegador puede
    // estar corrido); offline_since solo si no viene.
    var mins = num(rb.offline_min);
    if (mins === null && rb.offline_since) {
      var since = secondsSince(rb.offline_since);
      if (since !== null) mins = Math.round(since / 60);
    }
    var txt = "Reinicio recomendado" + (mins !== null ? ": sin conexión hace " + Math.max(1, mins) + " min" : "");
    return badge("st-err", txt, rb.reason || "");
  }

  function lastRebootHtml(rb) {
    if (!rb || !rb.last_reboot_at) return "";
    var s = own(REBOOT_STATUS_ES, rb.last_reboot_status);
    return (
      '<span class="muted">último reinicio ' +
      fmtTime(rb.last_reboot_at) +
      "</span>" +
      (rb.last_reboot_status ? " " + badge(s ? s[1].replace("st-run", "st-info") : "st-muted", s ? s[0] : String(rb.last_reboot_status)) : "") +
      (rb.last_reboot_error && !rb.in_progress ? ' <span class="muted">(' + escapeHtml(rebootErrorEs(rb.last_reboot_error)) + ")</span>" : "")
    );
  }

  // Resultado de la orden (§6.2 paso 5). El contrato no fija en qué clave del
  // objeto orden lo expone el backend: se acepta `result` (objeto o texto
  // JSON), `reboot_result`, `detail` o las claves sueltas en la orden.
  function rebootResult(o) {
    if (!o) return null;
    var keys = ["result", "reboot_result", "detail"];
    for (var i = 0; i < keys.length; i++) {
      var v = o[keys[i]];
      if (typeof v === "string" && v.charAt(0) === "{") {
        try {
          v = JSON.parse(v);
        } catch (e) {
          v = null;
        }
      }
      if (v && typeof v === "object") return v;
    }
    if (o.ssh_ok !== undefined || o.gateway_back_s !== undefined || o.internet_back_s !== undefined) return o;
    return null;
  }

  // La orden de reinicio más nueva de este equipo: de la lista de órdenes del
  // panel del celular o de la que se sigue desde acá (GET /commands).
  function latestRebootOrder(deviceId, m) {
    var best = null;
    var ui = m ? state.phone[m.probe.probe_id] : null;
    (ui && ui.orders ? ui.orders : []).forEach(function (o) {
      if (!isRebootOrder(o)) return;
      if (o.device_id !== deviceId && !(m && o.slot === m.slot && o.device_id === m.probe.probe_id)) return;
      if (newerOrder(o, best)) best = o;
    });
    var tr = state.reboot[deviceId];
    if (tr && tr.order && newerOrder(tr.order, best)) best = tr.order;
    return best;
  }

  // a es más nueva que b: otra orden (id mayor) o la misma con un estado más
  // reciente (la lista del panel y GET /commands se leen en momentos distintos).
  function newerOrder(a, b) {
    if (!b) return true;
    var ia = a.id || 0;
    var ib = b.id || 0;
    if (ia !== ib) return ia > ib;
    var ca = !isOpenStatus(a.status);
    var cb = !isOpenStatus(b.status);
    if (ca !== cb) return ca;
    return String(a.updated_at || "") > String(b.updated_at || "");
  }

  // Paso actual de una orden de reinicio: {idx, text, final, cls}.
  function rebootProgress(o, m) {
    var st = o.status;
    var res = rebootResult(o) || {};
    var err = rebootErrorEs(o.error);
    if (st === "pending") return { idx: -1, text: "en cola, esperando a que el celular la reciba", final: false };
    if (st === "delivered" || st === "claimed") {
      var ph = m ? phoneStatusFor(m.probe) : null;
      var busy = ph && ph.s && ph.s.current_order && ph.s.current_order.order_id !== o.order_id;
      return { idx: 0, text: busy ? "recibida; espera a que termine la prueba en curso" : "recibida por el celular", final: false };
    }
    if (st === "running") {
      // Paso: el que el celular guardó en la orden (`step`, §6.3) o, más
      // fresco, el de su estado en vivo si es esta misma orden (`reboot` o
      // `current_order`). Pasos de la app: reading_gateway → ssh →
      // waiting_down → waiting_back → waiting_internet.
      var step = o.step ? String(o.step) : "";
      var detail = "";
      var ps = m ? phoneStatusFor(m.probe) : null;
      if (ps && ps.s && !ps.stale) {
        var live = ps.s.reboot && ps.s.reboot.order_id === o.order_id ? ps.s.reboot : null;
        var co = ps.s.current_order && ps.s.current_order.order_id === o.order_id ? ps.s.current_order : null;
        var ls = (live && live.step) || (co && (co.step || co.reboot_step)) || "";
        if (ls) step = String(ls);
        if ((live || co) && ps.s.phase_detail) detail = String(ps.s.phase_detail);
      }
      step = step.toLowerCase();
      var STEP_TEXT = {
        received: [1, "recibida; empezando"],
        reading_gateway: [1, "leyendo en el MikroTik la IP del router"],
        ssh: [1, "conectando por SSH al router"],
        waiting_down: [2, "reboot enviado; esperando que se apague"],
        waiting_back: [2, "reboot enviado; esperando que el router vuelva (hasta 5 min)"],
        waiting_internet: [2, "el router ya responde; esperando Internet"],
      };
      var known = own(STEP_TEXT, step);
      var idx = known ? known[0] : /wait|back|vuelv|watch|health/.test(step) ? 2 : 1;
      var text = known ? known[1] : idx === 2 ? "reboot enviado; esperando que el router vuelva (hasta 5 min)" : "conectando por SSH al router";
      if (res.ssh_ok === true && idx < 2) {
        idx = 2;
        text = "reboot enviado; esperando que el router vuelva (hasta 5 min)";
      }
      if (idx === 1 && m) {
        // Sin señal explícita: si el celular ya ve la LAN del router caída
        // después de empezar, el reboot ya salió.
        var rh = routerHealth(m.probe, m.slot);
        var started = o.started_at ? new Date(o.started_at).getTime() : NaN;
        var checked = rh.h && rh.h.checked_at ? new Date(rh.h.checked_at).getTime() : NaN;
        if (!step && rh.h && rh.h.gateway_ok === false && !isNaN(started) && !isNaN(checked) && checked > started) {
          idx = 2;
          text = "reboot enviado; esperando que el router vuelva (hasta 5 min)";
        }
      }
      return { idx: idx, text: text + (detail ? " · " + detail : ""), final: false };
    }
    if (st === "done") {
      // en segundos hasta 10 min: "95 s" dice más que "2 min"
      var backDur = function (v) {
        return v < 600 ? Math.round(Math.max(0, v)) + " s" : fmtDur(v);
      };
      var back = [];
      if (num(res.gateway_back_s) !== null) back.push("LAN en " + backDur(num(res.gateway_back_s)));
      if (num(res.internet_back_s) !== null) back.push("Internet en " + backDur(num(res.internet_back_s)));
      return { idx: 4, text: "listo" + (back.length ? ": volvió " + back.join(", ") : ""), final: true, cls: "st-ok" };
    }
    var FINAL = { failed: "falló", expired: "venció sin ejecutarse", interrupted: "interrumpida", cancelled: "cancelada" };
    return {
      idx: -1,
      text:
        (own(FINAL, st) || st || "—") +
        (o.closed_by === "server" ? " (cerrada por el servidor)" : "") +
        (err ? ": " + err : "") +
        (st === "failed" && res.ssh_ok === true ? " (el reboot sí se envió)" : ""),
      final: true,
      cls: st === "failed" ? "st-err" : "st-warn",
    };
  }

  function rebootStatusBadge(o) {
    var s = own(REBOOT_STATUS_ES, o.status) || [o.status || "—", "st-muted"];
    return badge(s[1], s[0], o.status);
  }

  // Barra de pasos + texto. Solo HTML seguro.
  function rebootProgressHtml(o, m) {
    var pr = rebootProgress(o, m);
    var h = "";
    if (!pr.final || o.status === "done") {
      h +=
        '<span class="phone-steps reboot-steps">' +
        REBOOT_STEPS.map(function (s, i) {
          var cls = i === pr.idx ? "step-now" : i < pr.idx ? "step-done" : "";
          if (o.status === "done") cls = "step-done";
          return '<span class="phone-step ' + cls + '">' + escapeHtml(s[1]) + "</span>";
        }).join("") +
        "</span>";
    }
    h += " " + rebootStatusBadge(o) + ' <span class="muted">' + escapeHtml(pr.text) + "</span>";
    var res = rebootResult(o);
    if (res && res.host_key_fp)
      h += ' <span class="muted" title="huella de la llave SSH del router">· huella ' + escapeHtml(String(res.host_key_fp)) + "</span>";
    if (o.created_at) h += ' <span class="muted">· pedida ' + fmtTime(o.created_at) + "</span>";
    return h;
  }

  // Bloque completo de un equipo (lista / detalle / panel). HTML seguro.
  //   compact: la lista (una línea, sin textos largos).
  function rebootBlockHtml(deviceId, compact) {
    var m = phoneProbeForDevice(deviceId);
    var rb = rebootInfo(deviceId, m && m.target);
    var bits = [];
    if (m) {
      var hh = healthHtml(routerHealth(m.probe, m.slot), compact);
      if (hh) bits.push(hh);
    }
    var rec = recommendHtml(rb);
    if (rec) bits.push(rec);
    if (rb && rb.recommended && rb.reason && !compact) bits.push('<span class="muted">' + escapeHtml(rb.reason) + "</span>");

    var o = latestRebootOrder(deviceId, m);
    var tr = state.reboot[deviceId];
    var open = o && isOpenStatus(o.status);
    // Una orden recién cerrada se sigue mostrando un rato; después queda
    // solo "último reinicio".
    var recentClosed = o && !open && tr && tr.orderId === o.order_id && tr.since && Date.now() - tr.since < REBOOT_TRACK_MAX_MS;
    // El backend sabe de un reinicio en curso aunque la orden no esté en las
    // listas de este navegador (pedido desde otro lado). Si acá ya se la ve
    // cerrada, manda lo de acá (in_progress se relee cada 20 s).
    var serverOpen = !open && !!(rb && rb.in_progress) && !(o && o.order_id === rb.last_reboot_order_id && !isOpenStatus(o.status));
    if (o && (open || recentClosed)) {
      if (compact) {
        // lista: solo la insignia y el paso, sin la barra ni los textos largos
        var pr = rebootProgress(o, m);
        var stepName = !pr.final && pr.idx >= 0 ? REBOOT_STEPS[pr.idx][1] : "";
        bits.push(rebootStatusBadge(o) + (stepName ? ' <span class="muted">' + escapeHtml(stepName) + "</span>" : ""));
      } else bits.push('<span class="reboot-progress">' + rebootProgressHtml(o, m) + "</span>");
    }
    else if (tr && tr.orderId && !o) bits.push(badge("st-info", "orden creada") + ' <span class="muted">leyendo su estado...</span>');
    else if (serverOpen) bits.push(badge("st-run", "Reinicio en curso") + (compact ? "" : ' <span class="muted">pedido ' + fmtTime(rb.last_reboot_at) + "</span>"));
    else {
      var last = lastRebootHtml(rb);
      if (last && !compact) bits.push(last);
      else if (last && compact && rb.last_reboot_at && secondsSince(rb.last_reboot_at) < 3600) bits.push(last);
    }
    if (!compact && !open && !serverOpen && rb && rb.cooldown_until && secondsSince(rb.cooldown_until) < 0)
      bits.push('<span class="muted">enfriamiento hasta ' + fmtTime(rb.cooldown_until) + " (reiniciar antes pide forzar)</span>");

    if (m) {
      var busy = !!open || serverOpen || !!(tr && tr.inflight);
      var cls = rb && rb.recommended ? "btn-primary btn-reboot btn-reboot-rec" : "btn-link btn-reboot";
      bits.push(
        '<button type="button" class="' +
          cls +
          '" data-reboot-device="' +
          escapeHtml(deviceId) +
          '"' +
          (busy ? " disabled" : "") +
          ' title="El celular de ' +
          escapeHtml(m.probe.probe_id) +
          " le manda reboot por SSH a través del MikroTik" +
          '">' +
          (open || serverOpen ? "Reinicio en curso" : tr && tr.inflight ? "Enviando..." : "Reiniciar") +
          "</button>"
      );
    } else if (rec && !compact) {
      bits.push('<span class="muted">Ninguna sonda con celular tiene este equipo: no se puede reiniciar desde acá.</span>');
    }
    if (tr && tr.msg) bits.push('<span class="' + (tr.isError ? "phone-err" : "muted") + '">' + escapeHtml(tr.msg) + "</span>");
    return bits.join(" ");
  }

  // ---- dónde se dibuja

  var rebootBoxEl = document.getElementById("reboot-box");
  var rebootBoxBody = document.getElementById("reboot-box-body");

  // Reemplaza el HTML solo si cambió: estos bloques se redibujan cada 5 s y
  // un reemplazo en medio de un clic lo perdería.
  function setHtml(node, html) {
    if (node._html === html) return;
    node._html = html;
    node.innerHTML = html;
  }

  function renderRebootDetail() {
    if (!rebootBoxEl) return;
    var id = state.view === "detail" ? state.selectedDeviceId : null;
    if (!id) {
      rebootBoxEl.hidden = true;
      return;
    }
    var m = phoneProbeForDevice(id);
    var rb = rebootInfo(id, m && m.target);
    if (!m && !(rb && (rb.recommended || rb.last_reboot_at))) {
      rebootBoxEl.hidden = true;
      return;
    }
    rebootBoxEl.hidden = false;
    setHtml(
      rebootBoxBody,
      (m
        ? '<div class="muted">Router ' +
          escapeHtml(m.slot) +
          " de " +
          escapeHtml(probeName(m.probe)) +
          ". El celular le manda <code>reboot</code> por SSH a través del MikroTik (por la LAN del router, sin tocar las mediciones).</div>"
        : "") +
        '<div class="reboot-line">' +
        rebootBlockHtml(id, false) +
        "</div>"
    );
  }

  function renderRebootListCells() {
    var cells = devicesTbody.querySelectorAll("[data-reboot-cell]");
    Array.prototype.forEach.call(cells, function (c) {
      setHtml(c, rebootBlockHtml(c.getAttribute("data-reboot-cell"), true));
    });
  }

  function renderPhoneRouters(ui) {
    if (!ui || !ui.routersEl) return;
    var p = ui.probe;
    var slots = probeTargetsWithSlots(p);
    if (!slots.length) {
      setHtml(ui.routersEl, "");
      return;
    }
    setHtml(
      ui.routersEl,
      '<div class="phone-sub-head"><span>Routers: salud y reinicio</span></div>' +
      slots
        .map(function (x) {
          return (
            '<div class="phone-router-row' +
            (x.t.enabled === false ? " phone-router-off" : "") +
            '"><strong>' +
            escapeHtml(x.slot) +
            "</strong> " +
            '<span class="muted">' +
            escapeHtml(x.t.label || x.t.device_id) +
            "</span> " +
            rebootBlockHtml(x.t.device_id, false) +
            "</div>"
          );
        })
        .join("")
    );
  }

  function renderRebootViews() {
    Object.keys(state.phone).forEach(function (k) {
      renderPhoneRouters(state.phone[k]);
    });
    if (state.view === "list") renderRebootListCells();
    renderRebootDetail();
  }

  // ---- acción

  function rebootSay(deviceId, msg, isError) {
    var tr = state.reboot[deviceId] || (state.reboot[deviceId] = {});
    tr.msg = msg || "";
    tr.isError = !!isError;
    renderRebootViews();
  }

  function confirmReboot(deviceId, m, rb) {
    var name = (m.target.label ? m.target.label + " (" + deviceId + ")" : deviceId) + ", router " + m.slot + " de " + m.probe.probe_id;
    var rh = routerHealth(m.probe, m.slot);
    var off = rh.h && rh.h.gateway_ok === false && !rh.old;
    var base =
      "¿Reiniciar " + name + "?\n\n" +
      "El celular le manda `reboot` por SSH a través del MikroTik cuando termine la prueba que esté midiendo " +
      "(nunca en medio de una). El router queda sin servicio 1 a 3 minutos y, mientras el celular espera " +
      "que vuelva (hasta 5 min), no mide ninguno de los routers." +
      (off ? "\n\n⚠ El router no responde ni en la LAN: parece apagado, así que el SSH probablemente falle." : "");
    if (rb && rb.recommended) {
      return window.confirm(base + "\n\nRecomendado: " + (rb.reason || "lleva más de 10 min sin conexión") + ".");
    }
    if (!window.confirm(base + "\n\n⚠ NO está recomendado: el equipo no lleva más de 10 min sin conexión.")) return false;
    return window.confirm("Segunda confirmación: el reinicio de " + name + " no está recomendado. ¿Reiniciarlo de todas formas?");
  }

  // POST /api/v1/devices/{id}/reboot con un order_id generado UNA vez por
  // intento: un error de red o 5xx deja el id guardado y el próximo clic lo
  // reenvía (el backend es idempotente por order_id, no se duplica). El id
  // guardado se reusa aunque cambie la recomendación (el backend devuelve la
  // orden que ya existe sin mirar el cuerpo), pero vence a los
  // REBOOT_RETRY_MS: un clic mucho después es un pedido nuevo, no el
  // reintento de uno viejo que quizá ya se ejecutó (el backend igual frena un
  // duplicado con el 409 de "en curso" o del enfriamiento).
  var REBOOT_RETRY_MS = 3 * 60 * 1000;

  function adoptRebootOrder(tr, o) {
    tr.orderId = o.order_id;
    tr.order = o.status ? o : null;
    tr.since = Date.now();
  }

  function sendReboot(deviceId, force) {
    var m = phoneProbeForDevice(deviceId);
    if (!m) return;
    var rb = rebootInfo(deviceId, m.target);
    var tr = state.reboot[deviceId] || (state.reboot[deviceId] = {});
    if (tr.inflight) return;
    var reason = rb && rb.recommended ? "recommended" : "manual";
    if (!tr.pending || Date.now() - tr.pending.at > REBOOT_RETRY_MS) tr.pending = { id: newUuid() };
    tr.pending.at = Date.now();
    tr.inflight = true;
    tr.msg = "enviando la orden de reinicio...";
    tr.isError = false;
    renderRebootViews();
    phoneApi("POST", "/api/v1/devices/" + encodeURIComponent(deviceId) + "/reboot", {
      order_id: tr.pending.id,
      requested_by: "dashboard",
      reason: reason,
      force: !!force,
    }).then(function (r) {
      tr.inflight = false;
      if (r.ok) {
        var d = r.data || {};
        var o = d.order || (d.orders && d.orders[0]) || (d.order_id ? d : null);
        adoptRebootOrder(tr, o && o.order_id ? o : { order_id: tr.pending.id });
        tr.pending = null;
        tr.msg = d.existing
          ? o && o.status && !isOpenStatus(o.status)
            ? "esa orden ya existía y está cerrada; toca Reiniciar otra vez para pedir un reinicio nuevo"
            : "la orden ya estaba creada (no se duplicó)"
          : "";
        var ph = phoneStatusFor(m.probe);
        if (!d.existing && ph.stale) tr.msg = "⚠ el celular no está reportando: se ejecuta cuando vuelva (vence en 15 min)";
        renderRebootViews();
        var ui = state.phone[m.probe.probe_id];
        if (ui) refreshPhoneLists(ui);
        refreshDevices(); // in_progress / último reinicio nuevos
        startRebootTimer();
        return;
      }
      if (r.status === 0 || r.status >= 500) {
        rebootSay(deviceId, "No se pudo enviar: " + r.error + " Vuelve a tocar Reiniciar: se reenvía la misma orden, no se duplica.", true);
        return;
      }
      tr.pending = null;
      if (endpointMissing(r)) {
        rebootSay(deviceId, "El backend todavía no acepta reinicios (falta POST /devices/{id}/reboot).", true);
        return;
      }
      var cd = r.status === 409 && r.data && typeof r.data === "object" ? r.data : null;
      if (cd && cd.in_progress) {
        // Ya hay un reinicio en curso (pedido desde otro lado o antes): se lo
        // sigue; forzar no sirve (el backend también lo rechaza con force).
        if (cd.order && cd.order.order_id) {
          if (!cd.order.type) cd.order.type = "reboot_router";
          adoptRebootOrder(tr, cd.order);
          startRebootTimer();
        }
        refreshDevices();
        rebootSay(deviceId, "Ya hay un reinicio en curso para este equipo: no se pidió otro.", false);
        return;
      }
      if (r.status === 409 && !force && cd && (cd.cooldown_until || cd.last_reboot_at) && !/order_id/i.test(r.error || "")) {
        // Enfriamiento de 15 min (§6.3): se ofrece forzar.
        renderRebootViews();
        if (window.confirm("El backend no lo reinicia: " + r.error + "\n\nHubo un reinicio de este equipo hace menos de 15 min. ¿Forzar otro reinicio igual?")) {
          sendReboot(deviceId, true);
        } else rebootSay(deviceId, "No se reinició: " + r.error, true);
        return;
      }
      rebootSay(deviceId, "No se pudo reiniciar: " + r.error, true);
    });
  }

  function onRebootClick(e) {
    var btn = e.target && e.target.closest ? e.target.closest("button[data-reboot-device]") : null;
    if (!btn) return;
    e.preventDefault();
    e.stopPropagation();
    if (btn.disabled) return;
    var deviceId = btn.getAttribute("data-reboot-device");
    var m = phoneProbeForDevice(deviceId);
    if (!m) return;
    var o = latestRebootOrder(deviceId, m);
    if (o && isOpenStatus(o.status)) return;
    if (!confirmReboot(deviceId, m, rebootInfo(deviceId, m.target))) return;
    sendReboot(deviceId, false);
  }

  devicesTbody.addEventListener("click", onRebootClick);
  if (rebootBoxEl) rebootBoxEl.addEventListener("click", onRebootClick);

  // ---- seguimiento de la orden (también en el detalle, donde el panel del
  // celular no refresca su lista de órdenes)

  function trackedOpen(deviceId) {
    var tr = state.reboot[deviceId];
    if (!tr || !tr.orderId || !tr.since) return false;
    if (Date.now() - tr.since > REBOOT_TRACK_MAX_MS) return false;
    var o = latestRebootOrder(deviceId, phoneProbeForDevice(deviceId));
    return !o || o.order_id !== tr.orderId || isOpenStatus(o.status);
  }

  function rebootTick() {
    if (document.hidden || !state.apiKey) return;
    var any = false;
    Object.keys(state.reboot).forEach(function (deviceId) {
      if (!trackedOpen(deviceId)) return;
      any = true;
      var tr = state.reboot[deviceId];
      if (tr.polling) return;
      tr.polling = true;
      phoneApi("GET", "/api/v1/commands?device_id=" + encodeURIComponent(deviceId) + "&limit=30").then(function (r) {
        tr.polling = false;
        if (!r.ok || !Array.isArray(r.data)) return;
        var o = r.data.filter(function (c) {
          return c && c.order_id === tr.orderId;
        })[0];
        if (o) {
          if (!o.type) o.type = "reboot_router";
          tr.order = o;
        }
        if (o && !isOpenStatus(o.status)) refreshDevices(); // último reinicio / recomendación nuevos
        renderRebootViews();
      });
    });
    if (!any) stopRebootTimer();
  }

  function startRebootTimer() {
    if (!state.rebootTimer) state.rebootTimer = setInterval(rebootTick, REBOOT_POLL_MS);
  }

  function stopRebootTimer() {
    if (state.rebootTimer) clearInterval(state.rebootTimer);
    state.rebootTimer = null;
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
