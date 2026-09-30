package com.h3s.notion5g.notion5g_field

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.location.LocationManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.os.SystemClock
import io.flutter.plugin.common.EventChannel
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

/// Estado en vivo de la sonda, compartido por todo el proceso (contrato §2.5).
/// Lo escriben el worker y el control; ProbeStatus.build lo arma en el objeto
/// que ven la app (EventChannel / status) y el backend (POST .../status).
object ProbeState {
    class Alert(val level: String, val msg: String, val sinceMs: Long)

    @Volatile var running = false
    @Volatile var serviceStartedMs = 0L
    @Volatile var phase = "stopped"
    @Volatile var phaseSinceMs = 0L
    @Volatile var phaseDetail: String? = null
    @Volatile var currentOrder: JSONObject? = null
    /// true mientras el worker ejecuta una prueba (no se abren otras sesiones con el MikroTik).
    @Volatile var busy = false
    @Volatile var net: NetPaths? = null
    @Volatile var control: ControlPlane? = null

    // MikroTik (último leído)
    @Volatile var mkReachable: Boolean? = null
    @Volatile var mkRuleFound: Boolean? = null
    @Volatile var mkCurrentTable: String? = null
    @Volatile var mkIdentity: String? = null
    @Volatile var mkVersion: String? = null
    @Volatile var mkRoutes: JSONObject = JSONObject()
    /// Links a la web de cada equipo por el MikroTik (Mikrotik.webLinks).
    @Volatile var mkWeb: JSONObject? = null
    @Volatile var mkLastOkMs = 0L
    @Volatile var mkError: String? = null
    @Volatile var mkLastRule: JSONObject? = null

    // Backend
    @Volatile var backendReachable: Boolean? = null
    @Volatile var backendLastError: String? = null
    @Volatile var backendFailing = false

    // GPS (último fix de una prueba)
    @Volatile var lastGps: JSONObject? = null

    /// Reinicio en curso (§6.2), o null: lo arma el worker paso a paso.
    @Volatile var reboot: JSONObject? = null

    // Plan local
    @Volatile var localPlanActive = false
    @Volatile var localPlanNextMs = 0L

    val seq = AtomicLong(0)
    val alerts = ConcurrentHashMap<String, Alert>()

    private val ERROR_CODES = setOf(
        "mikrotik-login", "mikrotik-rule-missing", "route-not-restored", "backend-auth",
        "probe-not-configured", "no-location-permission", "restart-failed",
    )

    /// Nivel fijo por código (§2.5); mikrotik-unreachable es error solo si se
    /// exige Ethernet y hay cable.
    fun levelOf(code: String, requireEth: Boolean, ethUp: Boolean): String = when {
        code == "mikrotik-unreachable" -> if (requireEth && ethUp) "error" else "warn"
        code in ERROR_CODES -> "error"
        else -> "warn"
    }

    fun setAlert(code: String, msg: String) {
        val old = alerts[code]
        alerts[code] = Alert("warn", msg, old?.sinceMs ?: System.currentTimeMillis())
    }

    fun clearAlert(code: String) {
        alerts.remove(code)
    }

    fun setPhase(p: String, detail: String? = null) {
        if (p != phase) phaseSinceMs = System.currentTimeMillis()
        phase = p
        phaseDetail = detail
    }

    fun mikrotikJson(host: String): JSONObject = JSONObject()
        .put("host", host)
        .putN("reachable", mkReachable)
        .putN("rule_found", mkRuleFound)
        .putN("current_table", mkCurrentTable)
        .putN("identity", mkIdentity)
        .putN("version", mkVersion)
        .put("routes", mkRoutes)
        .putN("web", mkWeb)
        .putN("last_ok", Rfc3339.formatOrNull(mkLastOkMs))
        .putN("error", mkError)
}

object ProbeStatus {
    private val PHASE_NAMES = mapOf(
        "idle" to "En espera", "waiting_ethernet" to "Esperando el cable", "preparing" to "Preparando",
        "switching_route" to "Cambiando la ruta", "verifying_route" to "Verificando la ruta",
        "egress_check" to "Comprobando la salida", "gps_fix" to "Esperando GPS", "ping" to "Midiendo latencia",
        "download" to "Descargando", "upload" to "Subiendo", "storing" to "Guardando",
        "restoring_route" to "Volviendo a respaldo", "uploading" to "Enviando resultado",
        "backoff" to "Sin backend, reintentando", "stopped" to "Detenida",
        "rebooting" to "Reiniciando router",
    )

    fun phaseName(p: String): String = PHASE_NAMES[p] ?: p

    /// Arma el objeto de §2.5. Funciona con el servicio detenido (lee prefs y la base).
    fun build(ctx: Context): JSONObject {
        val prefs = ProbePrefs(ctx)
        val s = prefs.snapshot()
        val db = ProbeDb.get(ctx)
        val st = ProbeState
        val running = st.running
        val net = st.net
        val (eth, wifi) = if (net != null && running) net.eth to net.wifi else NetPaths.findNow(ctx)
        val now = System.currentTimeMillis()
        val offset = db.kvLong("clock_offset_ms")
        val skew = offset?.let { -it / 1000.0 }
        val lastBackendOk = db.kvLong("last_backend_ok_ms") ?: 0L

        val o = JSONObject()
        o.put("measured_by", s.phoneId)
        o.put("probe_id", s.probeId)
        o.put("sent_at", Rfc3339.format(now))
        o.put("seq", st.seq.incrementAndGet())
        o.putN("service_started_at", if (running) Rfc3339.formatOrNull(st.serviceStartedMs) else null)
        o.put("enabled", prefs.enabled)
        o.put("running", running)
        val phase = if (running) st.phase else "stopped"
        o.put("phase", phase)
        o.putN("phase_since", if (running) Rfc3339.formatOrNull(st.phaseSinceMs) else null)
        o.putN("phase_detail", if (running) st.phaseDetail else null)
        o.putN("current_order", if (running && phase !in setOf("idle", "backoff", "stopped")) st.currentOrder else null)
        o.put("mikrotik", st.mikrotikJson(s.str("mikrotik_host")))
        // Salud por router (§6.2) y reinicio en curso.
        o.put("routers", RouterHealth.json(db, MikrotikOps.targets(db, s)))
        o.putN("reboot", if (running) st.reboot else null)

        o.put("net", JSONObject()
            .put("require_ethernet", s.requireEthernet)
            .put("ethernet", NetPaths.linkJson(eth, true))
            .put("wifi", NetPaths.linkJson(wifi, false))
            .put("default_network", NetPaths.defaultNetworkType(ctx))
            .put("control_path", st.control?.lastPath ?: "none"))

        o.put("backend", JSONObject()
            .put("url", s.backendUrl)
            .putN("reachable", st.backendReachable)
            .putN("last_ok", Rfc3339.formatOrNull(lastBackendOk))
            .putN("last_error", st.backendLastError)
            .putN("clock_skew_s", ProbeUtil.round(skew, 1)))

        o.put("gps", st.lastGps ?: gpsFromDb(db))

        // Cola
        val next = db.orders("status IN ('pending','delivered')", arrayOf(), limit = 1).firstOrNull()
        o.put("queue", JSONObject()
            .put("orders_pending", db.countOpenOrders())
            .putN("next_order_at", next?.let { Rfc3339.format(it.executeAtMs - (offset ?: 0L)) })
            .putN("next_order_target", next?.target)
            .putN("next_order_type", next?.type)
            .put("results_pending", db.countResults("pending"))
            .put("results_rejected", db.countResults("rejected"))
            .put("outbox_pending", db.countOutbox()))

        o.put("local_plan", JSONObject()
            .put("active", running && st.localPlanActive)
            .put("interval_s", localPlanInterval(ctx, s))
            .putN("next_at", if (running && st.localPlanActive && st.localPlanNextMs > 0) Rfc3339.format(st.localPlanNextMs - (offset ?: 0L)) else null))

        o.putN("last_result", lastResult(db))

        // Batería
        val dev = DeviceInfo.status(ctx)
        val pm = ctx.getSystemService(PowerManager::class.java)
        val optIgnored = pm.isIgnoringBatteryOptimizations(ctx.packageName)
        o.put("battery", JSONObject()
            .putN("pct", dev["battery_pct"])
            .putN("status", dev["battery_status"])
            .putN("plugged", dev["plugged"])
            .putN("temp_c", dev["battery_temp_c"])
            .put("optimization_ignored", optIgnored))

        val fine = ctx.checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) == PackageManager.PERMISSION_GRANTED
        val bg = Build.VERSION.SDK_INT < Build.VERSION_CODES.Q ||
            ctx.checkSelfPermission(Manifest.permission.ACCESS_BACKGROUND_LOCATION) == PackageManager.PERMISSION_GRANTED
        val notif = Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU ||
            ctx.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED
        o.put("permissions", JSONObject().put("location", fine).put("background_location", bg).put("notifications", notif))

        // Alertas calculadas en el momento
        fun cond(code: String, on: Boolean, msg: String) = if (on) st.setAlert(code, msg) else st.clearAlert(code)
        val enabled = prefs.enabled
        cond("no-ethernet", enabled && s.requireEthernet && eth == null, "Se exige Ethernet y no hay cable: las órdenes esperan")
        cond("no-wifi", enabled && wifi == null, "Sin WiFi: el backend local no es alcanzable (el plano de control no tiene camino)")
        cond("no-location-permission", !fine, "Falta el permiso de ubicación: las pruebas salen sin posición")
        cond("no-background-location", fine && !bg, "Falta \"Permitir todo el tiempo\": si Android relanza la sonda, no tendrá GPS")
        val gpsOn = try {
            (ctx.getSystemService(Context.LOCATION_SERVICE) as LocationManager).isProviderEnabled(LocationManager.GPS_PROVIDER)
        } catch (_: Exception) { true }
        cond("gps-off", !gpsOn, "El GPS del teléfono está apagado")
        cond("battery-optimized", enabled && !optIgnored, "La app está bajo optimización de batería: Android puede no relanzar la sonda")
        val pct = (dev["battery_pct"] as? Number)?.toInt()
        cond("battery-low", pct != null && pct < 20 && dev["battery_status"] == "discharging", "Batería baja ($pct %) y descargando")
        cond("clock-skew", skew != null && Math.abs(skew) > 5, "El reloj del teléfono difiere ${ProbeUtil.fmt1(skew)} s del servidor")
        val rejected = db.countResults("rejected")
        cond("results-rejected", rejected > 0, "$rejected resultado(s) rechazados por el backend")
        val rf = prefs.restartFailed
        if (rf != null) {
            if (st.alerts["restart-failed"] == null) st.alerts["restart-failed"] = ProbeState.Alert("error", rf.first, rf.second)
        } else st.clearAlert("restart-failed")
        if (!running) {
            // Con el servicio detenido no hay lecturas vivas del MikroTik ni del backend.
            for (c in listOf("mikrotik-unreachable", "mikrotik-login", "mikrotik-rule-missing", "backend-unreachable",
                "backend-auth", "probe-not-configured")) st.clearAlert(c)
        }
        val alerts = JSONArray()
        for ((code, a) in st.alerts.entries.sortedBy { it.value.sinceMs }) {
            alerts.put(JSONObject()
                .put("code", code)
                .put("level", st.levelOf(code, s.requireEthernet, eth != null))
                .put("msg", a.msg)
                .put("since", Rfc3339.format(a.sinceMs)))
        }
        o.put("alerts", alerts)
        o.put("app_version", ProbeUtil.appVersion(ctx))
        return o
    }

    fun localPlanInterval(ctx: Context, s: ProbeSettings): Int {
        val cfg = try { ProbeDb.get(ctx).kvGet("probe_config")?.let { JSONObject(it) } } catch (_: Exception) { null }
        val iv = cfg?.optInt("interval_s", 0) ?: 0
        return if (iv > 0) iv else s.int("local_plan_interval_s")
    }

    private fun gpsFromDb(db: ProbeDb): Any {
        val f = db.lastFix() ?: return JSONObject().put("status", "unavailable").putN("age_s", null).putN("accuracy_m", null)
            .putN("source", null).putN("satellites_used", null).putN("lat", null).putN("lon", null).putN("at", null)
        val fixMs = f.getAsLong("fix_time_ms")
        return JSONObject()
            .put("status", f.getAsString("status"))
            .putN("age_s", fixMs?.let { ProbeUtil.round((System.currentTimeMillis() - it) / 1000.0, 1) })
            .putN("accuracy_m", f.getAsDouble("accuracy_m"))
            .putN("source", when (f.getAsString("provider")) { null -> null; "gps" -> "gps"; else -> "fused" })
            .putN("satellites_used", f.getAsInteger("satellites_used"))
            .putN("lat", f.getAsDouble("lat"))
            .putN("lon", f.getAsDouble("lon"))
            .putN("at", Rfc3339.formatOrNull(fixMs))
    }

    private fun lastResult(db: ProbeDb): JSONObject? {
        val r = db.recentResults(1).firstOrNull() ?: return null
        return JSONObject()
            .put("result_id", r["result_id"])
            .putN("at", r["test_started_at"] ?: r["created_at"])
            .putN("slot", r["slot"])
            .putN("device_id", r["device_id"])
            .putN("test_status", r["test_status"])
            .putN("down_mbps", r["down_mbps"])
            .putN("up_mbps", r["up_mbps"])
            .putN("ping_ms", r["ping_ms"])
            .putN("location_status", r["location_status"])
            .putN("sync_status", r["sync_status"])
            .putN("server_reason", r["server_reason"])
            .putN("error", r["error"])
    }
}

/// Publica el estado al EventChannel "notion5g/probe/events" en el hilo
/// principal, como mucho 4 por segundo (el último siempre sale).
object ProbeBus {
    private val main = Handler(Looper.getMainLooper())
    @Volatile var last: Map<String, Any?>? = null
        private set
    @Volatile var lastJson: JSONObject? = null
        private set
    private var sink: EventChannel.EventSink? = null
    private var lastEmitMs = 0L
    private var scheduled = false

    fun setSink(s: EventChannel.EventSink?) {
        main.post {
            sink = s
            last?.let { s?.success(it) }
        }
    }

    @Suppress("UNCHECKED_CAST")
    fun publish(status: JSONObject) {
        lastJson = status
        last = JsonConv.toPlatform(status) as Map<String, Any?>
        main.post { schedule() }
    }

    private fun schedule() {
        if (scheduled) return
        val wait = 250 - (SystemClock.elapsedRealtime() - lastEmitMs)
        if (wait <= 0) emit() else {
            scheduled = true
            main.postDelayed({ scheduled = false; emit() }, wait)
        }
    }

    private fun emit() {
        lastEmitMs = SystemClock.elapsedRealtime()
        val s = sink ?: return
        last?.let { s.success(it) }
    }
}
