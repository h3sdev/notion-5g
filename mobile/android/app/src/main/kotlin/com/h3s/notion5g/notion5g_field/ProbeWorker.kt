package com.h3s.notion5g.notion5g_field

import android.content.ContentValues
import android.content.Context
import android.location.LocationManager
import android.net.Network
import android.net.TrafficStats
import android.os.Handler
import android.os.Process
import android.os.SystemClock
import org.json.JSONObject
import java.net.URLEncoder

/// Un equipo de la sonda (de la config del backend en caché, o A/B por defecto).
data class ProbeTarget(val slot: String, val deviceId: String?, val table: String, val label: String?, val enabled: Boolean)

/// Operaciones del MikroTik de alto nivel, compartidas por el worker y por el
/// canal de Flutter con el servicio detenido (hilo de fondo de un solo uso).
object MikrotikOps {
    fun client(s: ProbeSettings, network: Network) = Mikrotik(
        network, s.str("mikrotik_host"), s.int("mikrotik_port"), s.str("mikrotik_user"), s.str("mikrotik_password"), s.ruleComment,
    )

    fun targets(db: ProbeDb, s: ProbeSettings): List<ProbeTarget> {
        val cfg = try { db.kvGet("probe_config")?.let { JSONObject(it) } } catch (_: Exception) { null }
        val arr = cfg?.optJSONArray("targets")
        if (arr == null || arr.length() == 0) {
            return listOf(
                ProbeTarget("A", null, s.str("table_A"), "Router A", true),
                ProbeTarget("B", null, s.str("table_B"), "Router B", true),
            )
        }
        val out = ArrayList<ProbeTarget>()
        for (i in 0 until arr.length()) {
            val t = arr.optJSONObject(i) ?: continue
            val slot = t.optStrN("slot")?.ifEmpty { null } ?: ('A' + i).toString()
            out.add(ProbeTarget(slot, t.optStrN("device_id"), t.optStrN("routing_table") ?: "", t.optStrN("label"), t.optBoolean("enabled", true)))
        }
        return out.sortedBy { it.slot }
    }

    fun probeEnabled(db: ProbeDb): Boolean = try {
        db.kvGet("probe_config")?.let { JSONObject(it).optBoolean("enabled", true) } ?: true
    } catch (_: Exception) {
        true
    }

    /// Una clave por tabla de equipo y `main` (respaldo) con su ruta por defecto.
    fun routesJson(routes: Map<String, Mikrotik.Route>, targets: List<ProbeTarget>, fallback: String): JSONObject {
        val o = JSONObject()
        for (t in targets) if (t.table.isNotEmpty()) o.put(t.table, routes[t.table]?.json() ?: JSONObject().put("active", false).putN("gateway", null))
        o.put(fallback, routes[fallback]?.json() ?: JSONObject().put("active", false).putN("gateway", null))
        return o
    }

    fun noteOk(rule: Mikrotik.Rule?) {
        val st = ProbeState
        st.mkReachable = true
        st.mkError = null
        st.mkLastOkMs = System.currentTimeMillis()
        if (rule != null) {
            st.mkRuleFound = true
            st.mkCurrentTable = rule.table
            st.mkLastRule = rule.json()
        }
        st.clearAlert("mikrotik-unreachable")
        st.clearAlert("mikrotik-login")
        st.clearAlert("mikrotik-rule-missing")
    }

    /// Registra el error en el estado y en las alertas; devuelve el mensaje.
    fun noteError(e: Exception): String {
        val st = ProbeState
        val kind = (e as? RouterOsException)?.kind ?: "io"
        val msg = when (kind) {
            "io" -> "MikroTik: error de comunicación (${e.javaClass.simpleName})"
            else -> e.message ?: kind
        }
        st.mkError = msg
        when (kind) {
            "unreachable" -> { st.mkReachable = false; st.setAlert("mikrotik-unreachable", "No se alcanza el MikroTik: $msg") }
            "login" -> { st.mkReachable = true; st.setAlert("mikrotik-login", msg) }
            "rule-missing" -> { st.mkReachable = true; st.mkRuleFound = false; st.setAlert("mikrotik-rule-missing", msg) }
            else -> st.mkReachable = true
        }
        return msg
    }

    class RestoreResult(val ok: Boolean, val table: String?, val error: String?)

    /// Deja la regla en fallback_table (lee, cambia solo si hace falta, relee) y
    /// actualiza las rutas en el estado.
    fun restore(db: ProbeDb, s: ProbeSettings, network: Network?): RestoreResult {
        if (network == null) {
            ProbeState.mkReachable = false
            ProbeState.mkError = "Sin Ethernet"
            return RestoreResult(false, null, "Sin Ethernet: no se alcanza el MikroTik")
        }
        val fb = s.fallbackTable
        return try {
            val mk = client(s, network)
            val (rule, routes) = mk.session { api ->
                var r = mk.readRule(api)
                if (r.table != fb) {
                    mk.setRuleTable(api, r.id, fb)
                    r = mk.readRule(api)
                }
                r to mk.allDefaultRoutes(api)
            }
            noteOk(rule)
            ProbeState.mkRoutes = routesJson(routes, targets(db, s), fb)
            if (rule.table == fb) RestoreResult(true, rule.table, null)
            else RestoreResult(false, rule.table, "La regla quedó en ${rule.table}")
        } catch (e: Exception) {
            RestoreResult(false, null, noteError(e))
        }
    }

    /// "Probar MikroTik": identidad, versión, regla y rutas; no cambia nada.
    fun test(db: ProbeDb, s: ProbeSettings, network: Network?): Map<String, Any?> {
        if (network == null) {
            ProbeState.mkReachable = false
            ProbeState.mkError = "Sin Ethernet"
            return mapOf("ok" to false, "identity" to null, "version" to null, "rule" to null, "routes" to null,
                "error" to "No hay cable Ethernet: el MikroTik solo se alcanza por Ethernet")
        }
        return try {
            val mk = client(s, network)
            var identity: String? = null
            var version: String? = null
            val (rule, routes) = mk.session { api ->
                identity = try { mk.identity(api) } catch (_: RouterOsException) { null }
                version = try { mk.version(api) } catch (_: RouterOsException) { null }
                mk.readRule(api) to mk.allDefaultRoutes(api)
            }
            noteOk(rule)
            ProbeState.mkIdentity = identity
            ProbeState.mkVersion = version
            val rj = routesJson(routes, targets(db, s), s.fallbackTable)
            ProbeState.mkRoutes = rj
            db.event("info", ProbeState.phase, null, "MikroTik: ${identity ?: "?"} ${version ?: ""}, regla en ${rule.table}")
            mapOf("ok" to true, "identity" to identity, "version" to version,
                "rule" to JsonConv.toPlatform(rule.json()), "routes" to JsonConv.toPlatform(rj), "error" to null)
        } catch (e: Exception) {
            val msg = noteError(e)
            db.event("error", ProbeState.phase, null, msg)
            mapOf("ok" to false, "identity" to null, "version" to null, "rule" to null, "routes" to null, "error" to msg)
        }
    }

    /// Lo último leído, sin abrir sesión (con una prueba en curso).
    fun lastRead(): Map<String, Any?> {
        val st = ProbeState
        return mapOf("ok" to (st.mkReachable == true && st.mkError == null), "identity" to st.mkIdentity, "version" to st.mkVersion,
            "rule" to st.mkLastRule?.let { JsonConv.toPlatform(it) }, "routes" to JsonConv.toPlatform(st.mkRoutes),
            "error" to (st.mkError ?: "Prueba en curso: se muestra lo último leído"))
    }
}

/// Hilo probe-worker (contrato §2.3): la máquina de estados, las pruebas, el
/// plan local, los vencimientos y todas las sesiones con el MikroTik. Es el
/// único que mide: nunca hay dos pruebas a la vez.
class ProbeWorker(
    private val ctx: Context,
    private val db: ProbeDb,
    private val prefs: ProbePrefs,
    private val net: NetPaths,
    private val cp: ControlPlane,
    val handler: Handler,
    /// Pide al control vaciar outbox y subir resultados ya.
    private val kickControl: () -> Unit,
    /// Publica el estado (phaseChanged = true también lo manda al backend).
    private val publish: (phaseChanged: Boolean) -> Unit,
) {
    @Volatile var abortReason: String? = null
    @Volatile var stopping = false
    private var forcedSinceElapsed = 0L
    private var lastIdleCheck = 0L
    private var routeNotRestored = false
    /// forceRoute llegó a mandar el `set` (aunque no se haya confirmado).
    private var ruleSetAttempted = false
    private val cooldown = HashMap<String, Long>()
    private val portalSlots = HashMap<String, String?>()
    private val gps = GpsFix(ctx)
    private val appVersion = ProbeUtil.appVersion(ctx)
    private val ua = "notion5g-probe/$appVersion"
    private var lastProgressPublish = 0L
    private val tickR = Runnable { tick() }

    companion object {
        private const val MAX_FORCED_MS = 8 * 60 * 1000L
        private val OPEN = ProbeDb.OPEN
    }

    // ------------------------------------------------------------ ciclo

    fun start() {
        handler.post { recover() }
        handler.post(tickR)
    }

    fun wake() {
        handler.removeCallbacks(tickR)
        handler.post(tickR)
    }

    private fun offset(): Long = db.kvLong("clock_offset_ms") ?: 0L
    private fun corrNow(): Long = System.currentTimeMillis() + offset()

    private fun phase(p: String, detail: String? = null) {
        val changed = p != ProbeState.phase
        ProbeState.setPhase(p, detail)
        publish(changed)
    }

    private fun progress(detail: String) {
        ProbeState.phaseDetail = detail
        val now = SystemClock.elapsedRealtime()
        if (now - lastProgressPublish >= 1000) {
            lastProgressPublish = now
            publish(false)
        }
    }

    private fun checkpoint() {
        abortReason?.let { throw ProbeAbort(it) }
        if (forcedSinceElapsed > 0 && SystemClock.elapsedRealtime() - forcedSinceElapsed > MAX_FORCED_MS) throw ProbeAbort("tope-de-tiempo")
    }

    private fun sleepChecked(ms: Long) {
        val end = SystemClock.elapsedRealtime() + ms
        while (true) {
            checkpoint()
            val left = end - SystemClock.elapsedRealtime()
            if (left <= 0) return
            Thread.sleep(minOf(left, 250))
        }
    }

    /// Paso 1 (§2.3): órdenes que quedaron `running` al morir el proceso pasan a
    /// interrupted y no se repiten; se intenta dejar la regla en respaldo.
    private fun recover() {
        try {
            val s = prefs.snapshot()
            for (o in db.orders("status='running'", arrayOf())) {
                if (db.hasResult(o.resultId)) continue
                val ok = db.tx { d ->
                    val won = db.casOrder(d, o.orderId, listOf("running"), ContentValues().apply {
                        put("status", "interrupted"); put("error", "app-reiniciada")
                    })
                    if (won && o.origin == "server") db.addOutbox(d, o.orderId, stateBody("interrupted", o.resultId, "app-reiniciada"))
                    won
                }
                if (ok) db.event("warn", "preparing", o.orderId, "Orden ${o.target} interrumpida (la app se reinició): no se repite")
            }
            // Al arrancar, NetPaths todavía no suele haber visto Ethernet: si no se
            // pudo restaurar ahora, la lectura en reposo lo reintenta en el primer
            // ciclo con cable (en vez de esperar mikrotik_idle_check_s).
            if (restoreRoute(s, strict = false).ok) lastIdleCheck = SystemClock.elapsedRealtime()
        } catch (e: Exception) {
            db.event("error", null, null, "Error al recuperar el estado: ${e.javaClass.simpleName}: ${e.message}")
        }
        kickControl()
    }

    private fun tick() {
        if (stopping) return
        var didWork = false
        try {
            val s = prefs.snapshot()
            if (routeNotRestored) restoreRoute(s, strict = false)
            expireDue()
            idleCheckIfDue(s)
            var order = pick()
            if (order == null && maybeLocalPlan(s)) order = pick()
            if (order != null) {
                didWork = execute(order, s)
            } else {
                idlePhase(s)
            }
        } catch (e: Exception) {
            db.event("error", ProbeState.phase, null, "Error interno del worker: ${e.javaClass.simpleName}: ${e.message}")
        } finally {
            if (!stopping) {
                handler.removeCallbacks(tickR)
                handler.postDelayed(tickR, if (didWork) 300L else 5000L)
            }
        }
    }

    private fun idlePhase(s: ProbeSettings) {
        ProbeState.currentOrder = null
        val waitingEth = s.requireEthernet && net.eth == null &&
            db.orders("status IN ('pending','delivered') AND execute_at_ms <= ?", arrayOf(corrNow().toString()), limit = 1).isNotEmpty()
        val p = when {
            waitingEth -> "waiting_ethernet"
            ProbeState.backendFailing && db.countOpenOrders() == 0 -> "backoff"
            else -> "idle"
        }
        if (p != ProbeState.phase) phase(p, null)
    }

    private fun stateBody(status: String, resultId: String?, error: String?): JSONObject =
        JSONObject().put("status", status).put("at", Rfc3339.format(System.currentTimeMillis())).putN("result_id", resultId).putN("error", error)

    /// Paso 3: vencidas sin ejecutar.
    private fun expireDue() {
        val now = corrNow()
        val list = db.orders("status IN ('pending','delivered') AND not_after_ms IS NOT NULL AND not_after_ms < ?", arrayOf(now.toString()))
        for (o in list) {
            val err = if (o.error == "sin-ethernet" || o.error == "mikrotik-no-confirma") o.error else "vencida"
            val won = db.tx { d ->
                val w = db.casOrder(d, o.orderId, OPEN, ContentValues().apply { put("status", "expired"); put("error", err) })
                if (w && o.origin == "server") db.addOutbox(d, o.orderId, stateBody("expired", null, err))
                w
            }
            if (won) db.event("warn", ProbeState.phase, o.orderId, "Orden ${o.target} vencida sin ejecutar ($err)")
            cooldown.remove(o.orderId)
        }
        if (list.isNotEmpty()) kickControl()
    }

    /// Paso 4: la orden debida más antigua (fuera de su espera de reintento).
    private fun pick(): ProbeDb.Order? {
        val nowEl = SystemClock.elapsedRealtime()
        return db.orders("status IN ('pending','delivered') AND execute_at_ms <= ?", arrayOf(corrNow().toString()), limit = 50)
            .firstOrNull { (cooldown[it.orderId] ?: 0L) <= nowEl }
    }

    /// Plan local sin backend (§2.9). true si creó una orden.
    private fun maybeLocalPlan(s: ProbeSettings): Boolean {
        val st = ProbeState
        if (!s.bool("local_plan_enabled") || !MikrotikOps.probeEnabled(db)) { st.localPlanActive = false; return false }
        // "Sin backend" se cuenta desde el último contacto o desde que arrancó el
        // servicio: al arrancar se le da tiempo al control de llegar al backend.
        val lastOk = maxOf(db.kvLong("last_backend_ok_ms") ?: 0L, st.serviceStartedMs)
        if (System.currentTimeMillis() - lastOk <= s.int("offline_after_s") * 1000L) { st.localPlanActive = false; return false }
        st.localPlanActive = true
        val corr = corrNow()
        // Las órdenes del backend ya descargadas mandan mientras no venzan.
        if (db.orders("status IN ('pending','delivered') AND (not_after_ms IS NULL OR not_after_ms >= ?)", arrayOf(corr.toString()), limit = 1).isNotEmpty()) return false
        val intervalMs = ProbeStatus.localPlanInterval(ctx, s) * 1000L
        val last = db.kvLong("local_plan_last_ms") ?: 0L
        val next = last + intervalMs
        st.localPlanNextMs = maxOf(next, corr)
        if (corr < next) return false
        val active = MikrotikOps.targets(db, s).filter { it.enabled }
        if (active.isEmpty()) return false
        val lastSlot = db.kvGet("last_slot_local")
        val idx = active.indexOfFirst { it.slot == lastSlot }
        val slot = active[(idx + 1) % active.size].slot
        val id = "local-" + ProbeUtil.uuid()
        db.tx { d ->
            db.insertLocalOrder(d, id, slot, corr, corr + intervalMs, "offline-schedule", "plan-local", s.int("duration_s"))
            db.kvSet("last_slot_local", slot, d)
            db.kvSet("local_plan_last_ms", corr.toString(), d)
        }
        st.localPlanNextMs = corr + intervalMs
        db.event("info", ProbeState.phase, id, "Sin backend: plan local, orden para $slot")
        return true
    }

    // ------------------------------------------------------ MikroTik

    private fun idleCheckIfDue(s: ProbeSettings) {
        val now = SystemClock.elapsedRealtime()
        if (now - lastIdleCheck < s.int("mikrotik_idle_check_s") * 1000L) return
        val eth = net.eth
        if (eth == null) {
            // Sin cable no hay sesión que abrir: no se adelanta lastIdleCheck, así
            // la lectura corre en el primer ciclo en que aparezca Ethernet.
            val was = ProbeState.mkReachable
            ProbeState.mkReachable = false
            ProbeState.mkError = "Sin Ethernet"
            cp.ruleOnFallback = false
            if (s.requireEthernet) ProbeState.setAlert("mikrotik-unreachable", "Sin cable Ethernet: no se alcanza el MikroTik")
            else ProbeState.clearAlert("mikrotik-unreachable")
            if (was != false) publish(false)
            return
        }
        lastIdleCheck = now
        val fb = s.fallbackTable
        try {
            val mk = MikrotikOps.client(s, eth.network)
            val (before, after, routes) = mk.session { api ->
                val r = mk.readRule(api)
                var r2 = r
                if (r.table != fb) {
                    mk.setRuleTable(api, r.id, fb)
                    r2 = mk.readRule(api)
                }
                Triple(r, r2, mk.allDefaultRoutes(api))
            }
            MikrotikOps.noteOk(after)
            ProbeState.mkRoutes = MikrotikOps.routesJson(routes, MikrotikOps.targets(db, s), fb)
            if (before.table != fb) {
                db.event("warn", ProbeState.phase, null, "Regla estaba en ${before.table} sin prueba: vuelta a respaldo")
            }
            cp.ruleOnFallback = after.table == fb
            if (after.table == fb) { routeNotRestored = false; ProbeState.clearAlert("route-not-restored"); forcedSinceElapsed = 0 }
        } catch (e: Exception) {
            cp.ruleOnFallback = false
            val msg = MikrotikOps.noteError(e)
            // Una línea por error distinto (o cada 10 min si sigue igual).
            if (msg != lastMkErrMsg || now - lastMkErrLog > 10 * 60_000L) {
                lastMkErrLog = now
                lastMkErrMsg = msg
                db.event("warn", ProbeState.phase, null, msg)
            }
        }
        publish(false)
    }

    private var lastMkErrLog = 0L
    private var lastMkErrMsg: String? = null

    /// Deja la regla en respaldo. strict = se llama tras una prueba (si falla,
    /// alerta route-not-restored y se reintenta en cada ciclo).
    fun restoreRoute(s: ProbeSettings, strict: Boolean): MikrotikOps.RestoreResult {
        val eth = net.eth
        if (eth == null && !strict) return MikrotikOps.RestoreResult(false, null, "Sin Ethernet")
        val r = MikrotikOps.restore(db, s, eth?.network)
        if (r.ok) {
            forcedSinceElapsed = 0
            cp.ruleOnFallback = true
            if (routeNotRestored) db.event("info", ProbeState.phase, null, "MikroTik: regla de vuelta en ${r.table}")
            routeNotRestored = false
            ProbeState.clearAlert("route-not-restored")
        } else if (strict || routeNotRestored) {
            cp.ruleOnFallback = false
            if (!routeNotRestored) db.event("error", ProbeState.phase, null, "Regla NO restaurada: ${r.error}")
            routeNotRestored = true
            ProbeState.setAlert("route-not-restored", "La regla del MikroTik no volvió a respaldo (${r.error})")
        }
        publish(false)
        return r
    }

    class RouteCheck(val confirmed: Boolean, val rule: Mikrotik.Rule, val route: Mikrotik.Route?)

    /// Pasos b-c: forzar la tabla, releer, leer su ruta, limpiar conntrack.
    /// 3 intentos con 2 s de espera. null = no se pudo confirmar.
    private fun forceRoute(s: ProbeSettings, table: String, orderId: String): RouteCheck? {
        cp.testActive = true
        ruleSetAttempted = false
        phase("switching_route", table)
        for (i in 1..3) {
            checkpoint()
            val eth = net.eth ?: return null.also { db.event("error", "switching_route", orderId, "Sin Ethernet: no se puede cambiar la ruta") }
            try {
                val mk = MikrotikOps.client(s, eth.network)
                var flushed: Int? = null
                val rc = mk.session { api ->
                    val r0 = mk.readRule(api)
                    ruleSetAttempted = true
                    mk.setRuleTable(api, r0.id, table)
                    if (table != s.fallbackTable && forcedSinceElapsed == 0L) forcedSinceElapsed = SystemClock.elapsedRealtime()
                    phase("verifying_route", table)
                    val r1 = mk.readRule(api)
                    val ok = r1.table == table && r1.action == "lookup-only-in-table" && !r1.disabled
                    val route = mk.defaultRoute(api, table)
                    val ip = eth.ipv4
                    if (ok && s.bool("flush_conntrack") && ip != null) {
                        flushed = try { mk.flushConntrack(api, ip) } catch (e: RouterOsException) {
                            db.event("warn", "verifying_route", orderId, "No se pudo limpiar el conntrack: ${e.message}"); null
                        }
                    }
                    RouteCheck(ok, r1, route)
                }
                MikrotikOps.noteOk(rc.rule)
                cp.ruleOnFallback = rc.rule.table == s.fallbackTable
                // Copia: el estado se lee desde otros hilos.
                ProbeState.mkRoutes = JSONObject(ProbeState.mkRoutes.toString())
                    .put(table, rc.route?.json() ?: JSONObject().put("active", false).putN("gateway", null))
                if (rc.confirmed) {
                    val gw = rc.route?.gateway ?: "sin ruta"
                    db.event("info", "verifying_route", orderId,
                        "MikroTik: regla ${s.ruleComment} → $table (confirmada) · ruta $gw${if (rc.route?.active == false) " (inactiva)" else ""}" +
                            (if (flushed == -1) " · conntrack con más de 2000 entradas: no se limpió" else flushed?.let { " · $it conexiones limpiadas" } ?: ""))
                    sleepChecked(1000)
                    return rc
                }
                db.event("warn", "verifying_route", orderId, "MikroTik: la regla quedó en ${rc.rule.table} (se pidió $table)")
            } catch (e: ProbeAbort) {
                throw e
            } catch (e: Exception) {
                val msg = MikrotikOps.noteError(e)
                db.event("error", "switching_route", orderId, "Intento $i: $msg")
                val kind = (e as? RouterOsException)?.kind
                if (kind == "login" || kind == "rule-missing") return null
            }
            if (i < 3) sleepChecked(2000)
        }
        return null
    }

    // ---------------------------------------------------------- prueba

    private inner class Run(val order: ProbeDb.Order, val s: ProbeSettings, val netPath: String, val network: Network) {
        var slot: String? = null
        var target: ProbeTarget? = null
        var attempt = 1
        var reason = order.selectionReason
        var resultId = ""
        var route: RouteCheck? = null
        var ruleEnd: Mikrotik.Rule? = null
        var ruleEndRead = false
        var captive: Boolean? = null
        var portalHost: String? = null
        var meta: SpeedTester.Meta? = null
        var metaEnd: SpeedTester.Meta? = null
        var gwReachable: Boolean? = null
        var dnsOk: Boolean? = null
        var internetOk: Boolean? = null
        var ping: SpeedTester.PingResult? = null
        var down: SpeedTester.PhaseResult? = null
        var up: SpeedTester.PhaseResult? = null
        var testStartedMs: Long? = null
        var testStartedNs = 0L
        var testFinishedMs: Long? = null
        var attemptStartMs = System.currentTimeMillis()
        var gpsPending: GpsFix.Pending? = null
        var gpsResult: GpsFix.Result? = null
        var traffic0: Pair<Long, Long>? = null
        var status = "done"
        var error: String? = null
        var kind = "cloudflare"
        var requestedKind = "cloudflare"
        var targetHost = "speed.cloudflare.com"
        var executedOffline = false
        var clockSkew: Double? = null
        val durationS get() = if (order.durationS in 3..60) order.durationS else s.int("duration_s").coerceIn(3, 60)

        fun resetAttempt() {
            route = null; ruleEnd = null; ruleEndRead = false; captive = null; portalHost = null; meta = null; metaEnd = null
            gwReachable = null; dnsOk = null; internetOk = null; ping = null; down = null; up = null
            testStartedMs = null; testStartedNs = 0L; testFinishedMs = null; status = "done"; error = null
            attemptStartMs = System.currentTimeMillis()
        }
    }

    private fun setOrderError(o: ProbeDb.Order, err: String, log: String?) {
        if (o.error == err) return
        if (log != null) db.event("warn", ProbeState.phase, o.orderId, log)
        db.tx { d -> db.casOrder(d, o.orderId, OPEN, ContentValues().apply { put("error", err) }) }
    }

    private fun currentOrderJson(r: Run?, o: ProbeDb.Order, netPath: String?): JSONObject = JSONObject()
        .put("order_id", o.orderId)
        .put("origin", o.origin)
        .putN("result_id", r?.resultId?.ifEmpty { null })
        .put("target", o.target)
        .putN("slot", r?.slot)
        .putN("device_id", r?.target?.deviceId)
        .putN("label", r?.target?.label)
        .putN("routing_table", if (netPath == "ethernet") r?.target?.table else null)
        .put("selection_reason", r?.reason ?: o.selectionReason)
        .putN("requested_by", o.requestedBy)
        .put("attempt", r?.attempt ?: 1)
        .putN("net_path", netPath)
        .putN("started_at", Rfc3339.formatOrNull(r?.testStartedMs))

    private fun traffic(): Pair<Long, Long>? {
        val uid = Process.myUid()
        val v = listOf(TrafficStats.getTotalRxBytes(), TrafficStats.getTotalTxBytes(), TrafficStats.getUidRxBytes(uid), TrafficStats.getUidTxBytes(uid))
        if (v.any { it == TrafficStats.UNSUPPORTED.toLong() }) return null
        return (v[0] + v[1]) to (v[2] + v[3])
    }

    /// true = hubo progreso (la orden se consumió o cambió); false = espera.
    private fun execute(o: ProbeDb.Order, s: ProbeSettings): Boolean {
        ProbeState.busy = true
        abortReason = if (stopping) "detenida" else null
        try {
            return executeInner(o, s)
        } finally {
            ProbeState.busy = false
            cp.testActive = false
            forcedSinceElapsed = 0L
            ProbeState.currentOrder = null
        }
    }

    private fun executeInner(o: ProbeDb.Order, s: ProbeSettings): Boolean {
        val targets = MikrotikOps.targets(db, s)
        val active = targets.filter { it.enabled }

        // a) camino (antes de cambiar de fase: una orden que espera el cable no
        // debe hacer parpadear la fase en cada ciclo)
        val forceWifi = o.target == "wifi"
        val eth = net.eth
        val wifi = net.wifi
        val netPath: String
        val network: Network
        if (!forceWifi && eth != null) {
            netPath = "ethernet"; network = eth.network
        } else if (!forceWifi && s.requireEthernet) {
            setOrderError(o, "sin-ethernet", "Orden ${o.target} esperando el cable Ethernet")
            cooldown[o.orderId] = SystemClock.elapsedRealtime() + 5000
            if (ProbeState.phase != "waiting_ethernet") phase("waiting_ethernet", null)
            return false
        } else if (wifi != null) {
            netPath = "wifi"; network = wifi.network
        } else {
            setOrderError(o, "sin-wifi", "Orden ${o.target}: no hay WiFi ni Ethernet para medir")
            cooldown[o.orderId] = SystemClock.elapsedRealtime() + 15000
            if (ProbeState.phase != "idle") phase("idle", null)
            return false
        }
        ProbeState.currentOrder = currentOrderJson(null, o, netPath)
        phase("preparing", null)

        val r = Run(o, s, netPath, network)
        if (netPath == "ethernet") {
            val slot = when (o.target) {
                // El activo con el `done` más viejo (sin dato = más viejo); empate → orden de slot.
                "next" -> active.minWithOrNull(compareBy<ProbeTarget>({ db.kvLong("last_done_ms_${it.slot}") ?: 0L }, { it.slot }))?.slot
                else -> o.target
            }
            val t = targets.firstOrNull { it.slot == slot && it.table.isNotEmpty() }
            if (slot == null || t == null) {
                val won = db.tx { d ->
                    val w = db.casOrder(d, o.orderId, OPEN, ContentValues().apply { put("status", "interrupted"); put("error", "slot-desconocido") })
                    if (w && o.origin == "server") db.addOutbox(d, o.orderId, stateBody("interrupted", null, "slot-desconocido"))
                    w
                }
                if (won) db.event("error", "preparing", o.orderId, "Orden ${o.target}: el equipo no está en la sonda (slot-desconocido)")
                kickControl()
                return true
            }
            r.slot = slot
            r.target = t
        } else if (o.target != "wifi" && o.target != "next") {
            // Por WiFi no hay router: la orden se ejecuta pero no se atribuye.
            r.target = targets.firstOrNull { it.slot == o.target }
        }
        ProbeState.currentOrder = currentOrderJson(r, o, netPath)

        r.gpsPending = gps.start(s.int("location_fix_timeout_s").coerceIn(5, 120))
        r.traffic0 = traffic()
        val lastOk = db.kvLong("last_backend_ok_ms") ?: 0L
        r.executedOffline = o.selectionReason == "offline-schedule" || System.currentTimeMillis() - lastOk > 5 * 60_000L
        r.clockSkew = db.kvLong("clock_offset_ms")?.let { ProbeUtil.round(-it / 1000.0, 3) }

        try {
            // b-c) ruta
            if (netPath == "ethernet") {
                val rc = forceRoute(s, r.target!!.table, o.orderId)
                if (rc == null) {
                    setOrderError(o, "mikrotik-no-confirma", null)
                    db.event("error", "verifying_route", o.orderId, "No se confirmó la ruta ${r.target!!.table}: la orden espera y se reintenta")
                    cooldown[o.orderId] = SystemClock.elapsedRealtime() + 30_000
                    r.gpsPending?.let { it.await(0) }
                    // Solo se restaura (y solo es "no restaurada") si la regla llegó
                    // a cambiarse: con un login rechazado, otra sesión en el mismo
                    // ciclo solo agrega un intento fallido más en el MikroTik (§3.6).
                    if (forcedSinceElapsed > 0 || ruleSetAttempted) {
                        phase("restoring_route", null)
                        restoreRoute(s, strict = true)
                    }
                    return false
                }
                r.route = rc
            }
            r.resultId = ProbeUtil.uuid()
            if (!markRunning(o, r)) {
                db.event("info", ProbeState.phase, o.orderId, "La orden ya no estaba abierta (cancelada o vencida): no se mide")
                if (netPath == "ethernet") { phase("restoring_route", null); restoreRoute(s, strict = true) }
                return true
            }
            ProbeState.currentOrder = currentOrderJson(r, o, netPath)
            kickControl()

            while (true) {
                // d) salida
                egressCheck(r)
                val other = active.firstOrNull { it.slot != r.slot && it.table.isNotEmpty() }
                if (r.internetOk != true && netPath == "ethernet" && (o.target == "next" || o.allowFallback) && r.attempt == 1 && other != null) {
                    r.status = "failed"
                    r.error = if (r.captive == true) "portal-cautivo" else "sin-internet"
                    r.gpsResult = r.gpsPending?.peek()
                    val p = buildPayload(r, attempt1OfFallback = true)
                    // El intento 1 y el result_id nuevo de la orden van en UNA
                    // transacción: si el proceso muriera entre las dos, la orden
                    // quedaría `running` con un result_id que ya tiene resultado y
                    // recover() no la interrumpiría nunca.
                    val nextResultId = ProbeUtil.uuid()
                    db.tx { d ->
                        db.insertResult(d, r.resultId, o.orderId, 1, p)
                        d.update("orders", ContentValues().apply {
                            put("result_id", nextResultId); put("updated_ms", System.currentTimeMillis())
                        }, "order_id=? AND status='running'", arrayOf(o.orderId))
                    }
                    db.event("warn", "egress_check", o.orderId, "Router ${r.slot} sin datos (${r.error}): se mide ${other.slot} como respaldo")
                    kickControl()
                    val from = r.slot
                    r.resetAttempt()
                    r.attempt = 2
                    r.slot = other.slot
                    r.target = other
                    r.reason = "fallback:$from-sin-datos"
                    r.resultId = nextResultId
                    ProbeState.currentOrder = currentOrderJson(r, o, netPath)
                    val rc = forceRoute(s, other.table, o.orderId)
                    if (rc == null) {
                        r.status = "failed"
                        r.error = "mikrotik-no-confirma"
                        r.gpsResult = r.gpsPending?.peek()
                        storeFinal(r)
                        phase("restoring_route", null)
                        restoreRoute(s, strict = true)
                        kickControl()
                        return true
                    }
                    r.route = rc
                    continue
                }
                break
            }

            // e) GPS
            phase("gps_fix", null)
            val waitMs = (s.int("location_fix_timeout_s").coerceIn(5, 120) + 6) * 1000L - (System.currentTimeMillis() - r.attemptStartMs)
            r.gpsResult = r.gpsPending?.await(maxOf(0L, waitMs))
            logGps(r)
            checkpoint()

            // f) ping
            phase("ping", null)
            r.testStartedMs = System.currentTimeMillis()
            r.testStartedNs = SystemClock.elapsedRealtimeNanos()
            ProbeState.currentOrder = currentOrderJson(r, o, netPath)
            val st = tester(r)
            val (pingHost, pingPort) = pingTarget(r)
            r.ping = st.ping(pingHost, pingPort)
            val pr = r.ping!!
            db.event("info", "ping", o.orderId, "Ping TCP a $pingHost:$pingPort: ${ProbeUtil.fmt1(pr.medianMs)} ms, pérdida ${ProbeUtil.fmt1(pr.lossPct)} %")
            if (r.internetOk != true || pr.lossPct >= 100.0) {
                r.status = "failed"
                r.error = if (r.captive == true) "portal-cautivo" else "sin-internet"
            } else {
                // g) descarga
                phase("download", null)
                val maxBytes = s.int("max_mb_per_phase") * 1_000_000L
                r.down = st.download({ downloadUrl(r) }, authHeaders(r, true), r.durationS, maxBytes)
                val d = r.down!!
                noteRedirect(r, d)
                if (d.error != null) {
                    r.status = "failed"
                    r.error = "descarga: ${d.error}"
                    db.event("error", "download", o.orderId, "Descarga falló: ${d.error}")
                } else {
                    db.event("info", "download", o.orderId, "Descarga: ${ProbeUtil.fmt1(d.mbps)} Mbps (${ProbeUtil.fmt1(d.bytes / 1e6)} MB en ${ProbeUtil.fmt1(d.seconds)} s)")
                }
                // h) subida
                phase("upload", null)
                r.up = st.upload({ uploadUrl(r) }, authHeaders(r, false), r.durationS, maxBytes)
                val u = r.up!!
                noteRedirect(r, u)
                if (u.error != null) {
                    if (r.status == "failed") r.error = "${r.error}; subida: ${u.error}" else r.error = "subida: ${u.error}"
                    db.event("error", "upload", o.orderId, "Subida falló: ${u.error}")
                } else {
                    db.event("info", "upload", o.orderId, "Subida: ${ProbeUtil.fmt1(u.mbps)} Mbps (${ProbeUtil.fmt1(u.bytes / 1e6)} MB en ${ProbeUtil.fmt1(u.seconds)} s)")
                }
            }
            r.testFinishedMs = System.currentTimeMillis()
            if (r.internetOk == true) r.metaEnd = st.meta()
            if (netPath == "ethernet") readRuleEnd(r)

            // i) guardar
            storeFinal(r)
        } catch (e: ProbeAbort) {
            if (e.reason == "tope-de-tiempo" && r.resultId.isNotEmpty()) {
                r.status = "failed"
                r.error = "tope-de-tiempo"
                r.gpsResult = r.gpsResult ?: r.gpsPending?.peek()
                if (r.testFinishedMs == null) r.testFinishedMs = System.currentTimeMillis()
                storeFinal(r)
            } else if (r.resultId.isNotEmpty()) {
                val won = db.tx { d ->
                    val w = db.casOrder(d, o.orderId, listOf("running"), ContentValues().apply { put("status", "interrupted"); put("error", "detenida") })
                    if (w && o.origin == "server") db.addOutbox(d, o.orderId, stateBody("interrupted", r.resultId, "detenida"))
                    w
                }
                if (won) db.event("warn", ProbeState.phase, o.orderId, "Prueba detenida: la orden queda interrumpida")
            }
        } catch (e: Exception) {
            // Error inesperado (base de datos, JSON, un bug): nunca dejar la orden
            // `running` para siempre ni la regla forzada. Se cierra como failed si
            // se puede guardar el resultado; si no, interrupted. Luego se restaura.
            val what = "${e.javaClass.simpleName}${e.message?.let { ": " + it.take(120) } ?: ""}"
            db.event("error", ProbeState.phase, o.orderId, "Error interno durante la prueba: $what")
            // Si la orden no llegó a running, espera como tras un fallo de ruta
            // (un error que se repite no debe loguearse en el MikroTik cada 300 ms).
            if (r.resultId.isEmpty()) cooldown[o.orderId] = SystemClock.elapsedRealtime() + 30_000
            if (r.resultId.isNotEmpty() && db.order(o.orderId)?.status == "running" && !db.hasResult(r.resultId)) {
                val stored = try {
                    r.status = "failed"
                    r.error = "error-interno: ${e.javaClass.simpleName}"
                    r.gpsResult = r.gpsResult ?: r.gpsPending?.peek()
                    if (r.testFinishedMs == null) r.testFinishedMs = System.currentTimeMillis()
                    storeFinal(r)
                    true
                } catch (_: Exception) {
                    false
                }
                if (!stored) {
                    try {
                        db.tx { d ->
                            val w = db.casOrder(d, o.orderId, listOf("running"), ContentValues().apply {
                                put("status", "interrupted"); put("error", "error-interno")
                            })
                            if (w && o.origin == "server") db.addOutbox(d, o.orderId, stateBody("interrupted", r.resultId, "error-interno"))
                        }
                    } catch (_: Exception) {
                    }
                }
            }
        } finally {
            r.gpsPending?.let { if (!it.done) it.await(0) }
        }

        // j) restaurar
        if (netPath == "ethernet") {
            abortReason = null
            phase("restoring_route", null)
            val rr = restoreRoute(s, strict = true)
            if (rr.ok) db.event("info", "restoring_route", o.orderId, "MikroTik: regla de vuelta en ${rr.table}")
        }
        // k) enviar
        cp.testActive = false
        phase("uploading", null)
        kickControl()
        return true
    }

    private fun markRunning(o: ProbeDb.Order, r: Run): Boolean {
        val now = System.currentTimeMillis()
        return db.tx { d ->
            val w = db.casOrder(d, o.orderId, OPEN, ContentValues().apply {
                put("status", "running"); put("result_id", r.resultId); put("started_ms", now); putNull("error")
            })
            if (w && o.origin == "server") db.addOutbox(d, o.orderId, stateBody("running", r.resultId, null))
            w
        }
    }

    private fun tester(r: Run) = SpeedTester(r.network, ua, { checkpoint() }, { progress(it) })

    /// Paso d: portal cautivo, IP de salida, puerta de enlace, DNS.
    private fun egressCheck(r: Run) {
        phase("egress_check", null)
        val st = tester(r)
        chooseTarget(r)
        val p = st.portalCheck()
        r.captive = p.captive
        r.portalHost = p.host
        checkpoint()
        r.meta = if (p.captive == true) null else st.meta()
        checkpoint()
        if (r.netPath == "ethernet") {
            val gc = r.s.str("gateway_check")
            if (gc.isNotEmpty()) {
                val i = gc.lastIndexOf(':')
                r.gwReachable = st.tcpReachable(gc.substring(0, i), gc.substring(i + 1).toInt())
            } else {
                r.gwReachable = r.route?.route?.gatewayIp?.let { st.tcpReachable(it, 80) }
            }
        }
        r.dnsOk = st.resolve(r.targetHost) != null
        r.internetOk = r.meta?.ip != null && r.captive != true
        val slotTxt = r.slot ?: "WiFi"
        if (r.captive == true) {
            portalSlots[slotTxt] = r.portalHost
            db.event("warn", "egress_check", r.order.orderId, "Portal cautivo en $slotTxt${r.portalHost?.let { " ($it)" } ?: ""}")
        } else if (r.captive == false) portalSlots.remove(slotTxt)
        updatePortalAlert()
        db.event("info", "egress_check", r.order.orderId,
            "Salida $slotTxt: ${r.meta?.ip ?: "sin IP"}${r.meta?.asn?.let { " AS$it" } ?: ""}${r.meta?.org?.let { " $it" } ?: ""}" +
                " · gateway ${r.gwReachable?.let { if (it) "responde" else "no responde" } ?: "-"} · DNS ${if (r.dnsOk == true) "ok" else "falla"}")
    }

    private fun updatePortalAlert() {
        if (portalSlots.isEmpty()) ProbeState.clearAlert("captive-portal")
        else ProbeState.setAlert("captive-portal", "Portal cautivo en " + portalSlots.entries.joinToString(", ") { "${it.key}${it.value?.let { h -> " ($h)" } ?: ""}" } +
            ": la SIM pide registro o saldo")
    }

    private fun noteRedirect(r: Run, p: SpeedTester.PhaseResult) {
        if (p.error == "http-3xx" && r.captive != true) {
            r.captive = true
            r.portalHost = p.redirectHost
        }
    }

    /// Destino efectivo (§2.6).
    private fun chooseTarget(r: Run) {
        val s = r.s
        r.requestedKind = s.str("speed_target")
        var k = r.requestedKind
        // La API key de producción nunca viaja en texto plano: sin https (o sin
        // key), este destino no se usa y se mide con Cloudflare.
        if (k == "prod-download" && (s.str("prod_api_key").isEmpty() || !s.str("prod_url").startsWith("https://", ignoreCase = true))) k = "cloudflare"
        if (k == "local" && r.netPath == "ethernet" && ProbeUtil.isPrivateHost(ProbeUtil.hostOf(s.backendUrl))) k = "cloudflare"
        r.kind = k
        r.targetHost = when (k) {
            "prod-download" -> ProbeUtil.hostOf(s.str("prod_url")) ?: "speed.cloudflare.com"
            "local" -> ProbeUtil.hostOf(s.backendUrl) ?: "speed.cloudflare.com"
            else -> "speed.cloudflare.com"
        }
    }

    private fun pingTarget(r: Run): Pair<String, Int> = when (r.kind) {
        "local" -> r.targetHost to ProbeUtil.portOf(r.s.backendUrl)
        "prod-download" -> r.targetHost to ProbeUtil.portOf(r.s.str("prod_url"))
        else -> r.targetHost to 443
    }

    private fun enc(v: String) = URLEncoder.encode(v, "UTF-8")

    private fun downloadUrl(r: Run): String = when (r.kind) {
        "prod-download" -> "${r.s.str("prod_url").trimEnd('/')}/api/v1/speedtest/download?bytes=50000000"
        "local" -> "${r.s.backendUrl}/api/v1/speedtest/download?bytes=50000000&test_id=${enc(r.resultId)}"
        else -> "https://speed.cloudflare.com/__down?bytes=25000000&measId=${enc(r.resultId)}"
    }

    private fun uploadUrl(r: Run): String = when (r.kind) {
        "local" -> "${r.s.backendUrl}/api/v1/speedtest/upload?test_id=${enc(r.resultId)}"
        // A producción nunca se le hace POST: la subida de prod-download va a Cloudflare.
        else -> "https://speed.cloudflare.com/__up?measId=${enc(r.resultId)}"
    }

    private fun authHeaders(r: Run, download: Boolean): Map<String, String> = when {
        r.kind == "local" && r.s.apiKey.isNotEmpty() -> mapOf("X-API-Key" to r.s.apiKey)
        r.kind == "prod-download" && download -> mapOf("X-API-Key" to r.s.str("prod_api_key"))
        else -> emptyMap()
    }

    private fun readRuleEnd(r: Run) {
        val eth = net.eth ?: return
        try {
            val mk = MikrotikOps.client(r.s, eth.network)
            r.ruleEnd = mk.session { api -> mk.readRule(api) }
            r.ruleEndRead = true
            val want = r.target?.table
            if (r.ruleEnd!!.table != want) {
                db.event("error", "upload", r.order.orderId, "La regla cambió durante la prueba: quedó en ${r.ruleEnd!!.table} (se pidió $want)")
            }
        } catch (e: Exception) {
            r.ruleEnd = null
            db.event("warn", "upload", r.order.orderId, "No se pudo releer la regla al final: ${e.message}")
        }
    }

    private fun logGps(r: Run) {
        val g = r.gpsResult
        val loc = g?.location
        when {
            g?.problem == "gps-off" -> db.event("warn", "gps_fix", r.order.orderId, "GPS apagado: prueba sin posición")
            g?.problem == "no-permission" -> db.event("warn", "gps_fix", r.order.orderId, "Sin permiso de ubicación: prueba sin posición")
            loc == null -> db.event("warn", "gps_fix", r.order.orderId, "GPS: sin fijo en ${r.s.int("location_fix_timeout_s").coerceIn(5, 120)} s")
            else -> db.event("info", "gps_fix", r.order.orderId,
                "GPS: fijo ${if (loc.provider == LocationManager.GPS_PROVIDER) "GNSS" else loc.provider}, " +
                    "${ProbeUtil.fmt1(GpsFix.ageSeconds(loc))} s, ±${ProbeUtil.fmt1(loc.accuracy.toDouble())} m" +
                    (g?.satellitesUsed?.let { ", $it satélites" } ?: ""))
        }
    }

    /// Paso i: resultado + cierre de la orden local en una transacción.
    private fun storeFinal(r: Run) {
        phase("storing", null)
        val o = r.order
        if (r.testFinishedMs == null) r.testFinishedMs = r.testStartedMs ?: System.currentTimeMillis()
        val p = buildPayload(r, attempt1OfFallback = false)
        val attributedSlot = if (r.netPath == "ethernet" && r.route?.confirmed == true) r.slot else null
        db.tx { d ->
            db.insertResult(d, r.resultId, o.orderId, r.attempt, p)
            d.update("orders", ContentValues().apply {
                put("status", r.status)
                put("error", r.error)
                put("result_id", r.resultId)
                put("updated_ms", System.currentTimeMillis())
                if (attributedSlot != null) {
                    put("slot", attributedSlot)
                    put("device_id", r.target?.deviceId)
                    put("routing_table", r.target?.table)
                }
            }, "order_id=? AND status='running'", arrayOf(o.orderId))
            if (r.status == "done" && attributedSlot != null) db.kvSet("last_done_ms_$attributedSlot", System.currentTimeMillis().toString(), d)
        }
        db.event(if (r.status == "done") "info" else "warn", "storing", o.orderId,
            "Resultado ${if (r.status == "done") "completo" else "fallido"} ${r.slot ?: "WiFi"}: " +
                "↓${ProbeUtil.fmt1(r.down?.mbps)} ↑${ProbeUtil.fmt1(r.up?.mbps)} Mbps, ping ${ProbeUtil.fmt1(r.ping?.medianMs)} ms" +
                (r.error?.let { " · $it" } ?: ""))
    }

    private fun buildPayload(r: Run, attempt1OfFallback: Boolean): JSONObject {
        val s = r.s
        val o = r.order
        val eth = r.netPath == "ethernet"
        val local = o.origin == "local"
        val startMs = r.testStartedMs ?: r.attemptStartMs
        val startNs = if (r.testStartedNs > 0) r.testStartedNs else SystemClock.elapsedRealtimeNanos()
        val p = JSONObject()
        p.put("result_id", r.resultId)
        p.putN("order_id", if (attempt1OfFallback || local) null else o.orderId)
        p.putN("local_order_id", if (local) o.orderId else null)
        p.putN("related_order_id", if (attempt1OfFallback) o.orderId else null)
        p.put("attempt", r.attempt)
        p.put("probe_id", s.probeId)
        p.put("device_id", if (eth) (r.target?.deviceId ?: s.phoneId) else s.phoneId)
        p.putN("slot", if (eth) r.slot else null)
        p.put("measured_by", s.phoneId)
        p.put("source", "phone-probe")
        p.put("tag", "probe-ab")
        p.put("ts", Rfc3339.format(startMs))
        p.put("test_started_at", Rfc3339.format(startMs))
        p.put("test_finished_at", Rfc3339.format(r.testFinishedMs ?: startMs))
        p.put("test_status", r.status)
        p.putN("error", r.error)
        p.putN("routing_table", if (eth) r.target?.table else null)
        p.put("mikrotik_confirmed", eth && r.route?.confirmed == true)
        p.putN("mikrotik_rule", if (eth) r.route?.rule?.json() else null)
        p.putN("mikrotik_route_active", if (eth) r.route?.route?.active else null)
        p.putN("mikrotik_rule_end", if (eth) r.ruleEnd?.json() else null)
        p.putN("gateway_ip", if (eth) r.route?.route?.gatewayIp else null)
        p.putN("gateway_iface", if (eth) r.route?.route?.gatewayIface else null)
        p.putN("captive_portal", r.captive)
        p.putN("portal_host", r.portalHost)
        p.put("net_path", r.netPath)
        p.put("selection_reason", r.reason)
        p.put("executed_offline", r.executedOffline)
        p.putN("clock_skew_s", r.clockSkew)
        p.put("target_kind", r.kind)
        if (r.kind != r.requestedKind) p.put("target_requested", r.requestedKind)
        p.put("target_host", r.targetHost)
        if (r.kind == "prod-download") p.put("upload_host", "speed.cloudflare.com")
        p.putN("egress_ip", r.meta?.ip)
        p.putN("egress_ip_end", r.metaEnd?.ip)
        p.putN("egress_colo", r.meta?.colo)
        p.putN("egress_asn_client", r.meta?.asn)
        p.putN("egress_as_org_client", r.meta?.org)
        p.putN("gw_reachable", r.gwReachable)
        p.putN("dns_ok", r.dnsOk)
        p.putN("internet_ok", r.internetOk)
        p.putN("ping_ms", ProbeUtil.round(r.ping?.medianMs, 1))
        p.putN("jitter_ms", ProbeUtil.round(r.ping?.jitterMs, 1))
        p.putN("loss_pct", ProbeUtil.round(r.ping?.lossPct, 1))
        p.putN("ping_method", if (r.ping != null) "tcp-connect" else null)
        p.putN("ping_samples", r.ping?.samples)
        p.putN("down_mbps", ProbeUtil.round(r.down?.mbps, 2))
        p.putN("down_bytes", r.down?.bytes)
        p.putN("down_s", ProbeUtil.round(r.down?.seconds, 2))
        p.putN("up_mbps", ProbeUtil.round(r.up?.mbps, 2))
        p.putN("up_bytes", r.up?.bytes)
        p.putN("up_s", ProbeUtil.round(r.up?.seconds, 2))
        p.putN("concurrent_tests", r.down?.concurrent ?: r.up?.concurrent)
        val t1 = traffic()
        val t0 = r.traffic0
        p.putN("other_traffic_bytes", if (t0 != null && t1 != null) maxOf(0L, (t1.first - t0.first) - (t1.second - t0.second)) else null)
        p.put("duration_s", r.durationS)

        // GPS (§2.4)
        val g = r.gpsResult
        val loc = g?.location
        var status = "unavailable"
        if (loc != null) {
            val age = GpsFix.ageSeconds(loc, startNs)
            val maxAge = if (loc.hasSpeed() && loc.speed < 1f) s.int("location_max_age_stationary_s") else s.int("location_max_age_s")
            status = if (age <= maxAge && loc.accuracy <= s.int("location_max_accuracy_m")) "fresh" else "stale"
            val isGps = loc.provider == LocationManager.GPS_PROVIDER
            if (status == "fresh") {
                p.put("lat", loc.latitude); p.put("lon", loc.longitude)
                p.putN("gps_accuracy_m", ProbeUtil.round(loc.accuracy.toDouble(), 1))
                p.put("gps_source", if (isGps) "android-gps" else "android-fused")
            } else {
                p.putN("lat", null); p.putN("lon", null); p.putN("gps_accuracy_m", null); p.putN("gps_source", null)
                p.put("stale_lat", loc.latitude); p.put("stale_lon", loc.longitude)
                p.putN("stale_accuracy_m", ProbeUtil.round(loc.accuracy.toDouble(), 1))
            }
            p.put("gps_ts", Rfc3339.format(startMs - (age * 1000).toLong()))
            p.putN("gps_age_s", ProbeUtil.round(age, 1))
            p.put("gps_fix_time", Rfc3339.format(loc.time))
            p.put("location_source", if (isGps) "gps" else "fused")
            p.putN("gps_satellites_used", g?.satellitesUsed)
            p.putN("gps_speed_mps", if (loc.hasSpeed()) ProbeUtil.round(loc.speed.toDouble(), 2) else null)
            ProbeState.lastGps = JSONObject().put("status", status).putN("age_s", ProbeUtil.round(age, 1))
                .putN("accuracy_m", ProbeUtil.round(loc.accuracy.toDouble(), 1)).put("source", if (isGps) "gps" else "fused")
                .putN("satellites_used", g?.satellitesUsed).put("lat", loc.latitude).put("lon", loc.longitude)
                .put("at", Rfc3339.format(startMs - (age * 1000).toLong()))
        } else {
            for (k in listOf("lat", "lon", "gps_accuracy_m", "gps_source", "gps_ts", "gps_age_s", "gps_satellites_used", "gps_speed_mps")) p.putN(k, null)
            p.put("location_source", "none")
            ProbeState.lastGps = JSONObject().put("status", "unavailable").putN("age_s", null).putN("accuracy_m", null).putN("source", null)
                .putN("satellites_used", null).putN("lat", null).putN("lon", null).putN("at", null)
        }
        p.put("location_status", status)
        db.insertFix(ContentValues().apply {
            put("result_id", r.resultId)
            put("taken_ms", System.currentTimeMillis())
            if (loc != null) {
                put("fix_time_ms", loc.time)
                put("elapsed_realtime_ns", loc.elapsedRealtimeNanos)
                put("lat", loc.latitude); put("lon", loc.longitude)
                put("accuracy_m", loc.accuracy.toDouble())
                if (loc.hasSpeed()) put("speed_mps", loc.speed.toDouble())
                put("provider", if (loc.provider == LocationManager.GPS_PROVIDER) "gps" else "fused")
                g?.satellitesUsed?.let { put("satellites_used", it) }
            }
            put("status", status)
        })

        p.putN("battery_pct", DeviceInfo.status(ctx)["battery_pct"])
        p.put("app_version", appVersion)
        return p
    }

    // --------------------------------------------------- canal (worker)

    fun runTestMikrotik(): Map<String, Any?> {
        val s = prefs.snapshot()
        val r = MikrotikOps.test(db, s, net.eth?.network)
        cp.ruleOnFallback = (r["rule"] as? Map<*, *>)?.get("table") == s.fallbackTable
        publish(false)
        return r
    }

    fun runRestore(): Map<String, Any?> {
        val r = restoreRoute(prefs.snapshot(), strict = false)
        if (!r.ok && net.eth == null) return mapOf("ok" to false, "table" to null, "error" to "No hay cable Ethernet: el MikroTik solo se alcanza por Ethernet")
        return mapOf("ok" to r.ok, "table" to r.table, "error" to r.error)
    }
}
