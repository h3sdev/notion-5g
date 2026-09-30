package com.h3s.notion5g.notion5g_field

import android.content.ContentValues
import android.content.Context
import android.os.Handler
import android.os.SystemClock
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.URLEncoder

/// Hilo probe-control (contrato §2.3 paso 2): todo el HTTP al backend -- traer
/// órdenes, ack, outbox, resultados y POST status. Nunca bloquea al worker.
/// Sus métodos de subida también sirven sin servicio (syncNow con la sonda
/// apagada), con handler = null.
class ProbeControl(
    private val ctx: Context,
    private val db: ProbeDb,
    private val prefs: ProbePrefs,
    private val cp: ControlPlane,
    private val handler: Handler?,
    /// Avisa al worker que hay órdenes nuevas.
    private val onOrders: () -> Unit = {},
    /// Arma y publica el estado; devuelve el JSON a mandar.
    private val buildStatus: () -> JSONObject? = { null },
) {
    companion object {
        private const val HORIZON_MS = 6 * 3600_000L

        fun enc(v: String): String = URLEncoder.encode(v, "UTF-8")

        /// "Probar backend" (§2.10): GET /healthz y GET /api/v1/probes/{id}.
        fun testBackend(s: ProbeSettings, cp: ControlPlane): Map<String, Any?> {
            val out = HashMap<String, Any?>()
            out["ok"] = false; out["path"] = null; out["status"] = null; out["probe_found"] = false
            out["runner"] = null; out["server_time"] = null; out["error"] = null
            try {
                val h = cp.request("GET", "${s.backendUrl}/healthz", s.apiKey, readMs = 8000)
                out["path"] = h.path
                out["status"] = h.code
                if (h.code !in 200..299) {
                    out["error"] = "El backend respondió ${h.code} en /healthz"
                    return out
                }
                if (s.apiKey.isEmpty()) {
                    out["error"] = "Falta la API key del backend local"
                    return out
                }
                val r = cp.request("GET", "${s.backendUrl}/api/v1/probes/${enc(s.probeId)}", s.apiKey, readMs = 8000)
                out["status"] = r.code
                out["path"] = r.path
                if (r.date > 0) out["server_time"] = Rfc3339.format(r.date)
                when (r.code) {
                    401 -> out["error"] = "API key rechazada (401): usa la API_KEY del backend local"
                    404 -> out["error"] = "La sonda ${s.probeId} no está configurada en el backend"
                    in 200..299 -> {
                        val o = try { JSONObject(r.body) } catch (_: Exception) { JSONObject() }
                        val p = o.optJSONObject("probe") ?: o
                        out["probe_found"] = true
                        out["runner"] = p.optStrN("runner") ?: "probe"
                        out["ok"] = out["runner"] == "phone"
                        if (out["runner"] != "phone") out["error"] = "La sonda existe pero no la ejecuta un celular (runner=${out["runner"]})"
                    }
                    else -> out["error"] = "El backend respondió ${r.code}"
                }
            } catch (e: IOException) {
                out["error"] = e.message ?: "backend no alcanzable"
            }
            return out
        }
    }

    @Volatile private var stopped = false
    private var backoffMs = 0L
    private var lastStatusSentEl = 0L
    private var consecutiveFails = 0

    private val pollR = Runnable { pollLoop() }
    private val statusR = Runnable { statusLoop() }
    private val statusNowR = Runnable { sendStatus(rebuild = false) }
    private val flushR = Runnable { flush() }

    fun start() {
        handler?.post(pollR)
        handler?.postDelayed(statusR, 1000)
    }

    fun stop() {
        stopped = true
        handler?.removeCallbacksAndMessages(null)
    }

    /// Vaciar outbox y subir resultados ya (también durante una prueba, por WiFi).
    fun kick() {
        val h = handler ?: return
        h.removeCallbacks(flushR)
        h.post(flushR)
    }

    /// POST status en cuanto se pueda, como mucho uno cada 2 s; solo el último.
    fun statusSoon() {
        val h = handler ?: return
        h.removeCallbacks(statusNowR)
        val since = SystemClock.elapsedRealtime() - lastStatusSentEl
        if (since >= 2000) h.post(statusNowR) else h.postDelayed(statusNowR, 2000 - since)
    }

    fun pollNow() {
        val h = handler ?: return
        h.removeCallbacks(pollR)
        h.post(pollR)
    }

    private fun flush() {
        if (stopped) return
        val s = prefs.snapshot()
        if (s.apiKey.isEmpty()) return
        try {
            if (flushOutbox(s)) uploadResults(s)
        } catch (_: Exception) {
        }
    }

    // --------------------------------------------------------- órdenes

    private fun pollLoop() {
        if (stopped) return
        val s = prefs.snapshot()
        val ok = try {
            pollOnce(s)
        } catch (e: Exception) {
            fail(e.message ?: e.javaClass.simpleName)
            false
        }
        val st = ProbeState
        if (ok) {
            consecutiveFails = 0
            backoffMs = 0
            st.backendFailing = false
        } else {
            consecutiveFails++
            backoffMs = if (backoffMs == 0L) 5000L else minOf(backoffMs * 2, 300_000L)
            st.backendFailing = true
        }
        if (!stopped) handler?.postDelayed(pollR, if (ok) s.int("poll_interval_s") * 1000L else backoffMs)
    }

    private fun fail(msg: String) {
        ProbeState.backendLastError = msg
        ProbeState.backendReachable = false
        ProbeState.setAlert("backend-unreachable", "Sin contacto con el backend: $msg")
    }

    private fun pollOnce(s: ProbeSettings): Boolean {
        val st = ProbeState
        if (s.apiKey.isEmpty()) {
            fail("falta la API key del backend local en los ajustes de la sonda")
            return false
        }
        val url = "${s.backendUrl}/api/v1/probes/${enc(s.probeId)}/orders?horizon=6h"
        val sendWall = System.currentTimeMillis()
        val t0 = SystemClock.elapsedRealtime()
        val r = cp.request("GET", url, s.apiKey)
        val rtt = SystemClock.elapsedRealtime() - t0
        st.backendReachable = true
        when (r.code) {
            401 -> {
                st.backendLastError = "API key rechazada (401)"
                st.setAlert("backend-auth", "El backend rechazó la API key (401): revisa los ajustes de la sonda")
                st.clearAlert("backend-unreachable")
                return false
            }
            404 -> {
                st.backendLastError = "sonda no configurada (404)"
                st.setAlert("probe-not-configured", "La sonda ${s.probeId} no está configurada en el backend")
                st.clearAlert("backend-unreachable")
                return false
            }
        }
        if (r.code !in 200..299) {
            fail("el backend respondió ${r.code}")
            return false
        }
        val o = JSONObject(r.body)
        st.clearAlert("backend-auth")
        st.clearAlert("probe-not-configured")
        st.clearAlert("backend-unreachable")
        st.backendLastError = null

        // Desfase = hora del servidor − hora local, corregido por la mitad de la
        // ida y vuelta (server_time viene sin fracciones: +0,5 s en promedio).
        Rfc3339.parse(o.optStrN("server_time"))?.let { srv ->
            db.kvSet("clock_offset_ms", (srv + 500 - (sendWall + rtt / 2)).toString())
        }
        db.kvSet("last_backend_ok_ms", System.currentTimeMillis().toString())
        o.optJSONObject("probe")?.let { db.kvSet("probe_config", it.toString()) }

        if (ingest(o)) onOrders()
        ack(s)
        if (flushOutbox(s)) uploadResults(s)
        onOrders()
        return true
    }

    /// Guarda las órdenes, aplica `closed` y la reconciliación por ausencia.
    /// true si algo cambió.
    private fun ingest(o: JSONObject): Boolean {
        val offset = db.kvLong("clock_offset_ms") ?: 0L
        val corrNow = System.currentTimeMillis() + offset
        val arr = o.optJSONArray("orders") ?: JSONArray()
        val truncated = o.optBoolean("truncated", false)
        val seen = HashSet<String>()
        val logs = ArrayList<Triple<String, String?, String>>()
        var changed = false
        db.tx { d ->
            for (i in 0 until arr.length()) {
                val so = arr.optJSONObject(i) ?: continue
                val id = so.optStrN("order_id") ?: continue
                seen.add(id)
                val exec = Rfc3339.parse(so.optStrN("execute_at")) ?: corrNow
                val notAfter = Rfc3339.parse(so.optStrN("not_after"))
                val existing = db.order(id)
                if (existing == null) {
                    val lost = so.optString("status") == "running"
                    val now = System.currentTimeMillis()
                    d.insertOrThrow("orders", null, ContentValues().apply {
                        put("order_id", id); put("origin", "server")
                        so.optLongN("id")?.let { put("server_id", it) }
                        put("target", so.optStrN("target") ?: "next")
                        put("slot", so.optStrN("slot")); put("device_id", so.optStrN("device_id"))
                        put("routing_table", so.optStrN("routing_table"))
                        put("allow_fallback", if (so.optBoolean("allow_fallback", false)) 1 else 0)
                        put("duration_s", so.optInt("duration_s", 10))
                        put("execute_at_ms", exec)
                        if (notAfter != null) put("not_after_ms", notAfter)
                        put("selection_reason", so.optStrN("selection_reason") ?: "requested")
                        put("requested_by", so.optStrN("requested_by"))
                        put("status", if (lost) "interrupted" else "pending")
                        if (lost) { put("error", "estado-perdido"); put("result_id", so.optStrN("result_id")) }
                        put("acked", 0); put("created_ms", now); put("updated_ms", now)
                    })
                    if (lost) {
                        db.addOutbox(d, id, JSONObject().put("status", "interrupted").put("at", Rfc3339.format(now))
                            .putN("result_id", so.optStrN("result_id")).put("error", "estado-perdido"))
                        logs.add(Triple("warn", id, "Orden ${so.optStrN("target")} estaba en curso en el servidor pero no aquí: queda interrumpida"))
                    } else {
                        logs.add(Triple("info", id, "Orden nueva: ${so.optStrN("target")} (${so.optStrN("selection_reason")}, ${so.optStrN("requested_by") ?: "?"})"))
                    }
                    changed = true
                } else if (existing.status in ProbeDb.OPEN &&
                    (existing.executeAtMs != exec || existing.notAfterMs != notAfter)) {
                    d.update("orders", ContentValues().apply {
                        put("execute_at_ms", exec)
                        if (notAfter != null) put("not_after_ms", notAfter) else putNull("not_after_ms")
                        put("updated_ms", System.currentTimeMillis())
                    }, "order_id=? AND status IN ('pending','delivered')", arrayOf(id))
                    changed = true
                }
            }
            // Cerradas por el servidor o el dashboard.
            val closed = o.optJSONArray("closed") ?: JSONArray()
            val closedIds = HashSet<String>()
            for (i in 0 until closed.length()) {
                val c = closed.optJSONObject(i) ?: continue
                val id = c.optStrN("order_id") ?: continue
                closedIds.add(id)
                val status = c.optStrN("status") ?: continue
                if (status !in setOf("cancelled", "expired", "interrupted")) continue
                val err = when (status) { "cancelled" -> "cancelada"; "expired" -> "vencida-sin-ejecutar"; else -> "sin-cierre" }
                if (db.casOrder(d, id, ProbeDb.OPEN, ContentValues().apply { put("status", status); put("error", err) })) {
                    logs.add(Triple("info", id, "Orden ${if (status == "cancelled") "cancelada" else "cerrada"} en el servidor: no se ejecuta"))
                    changed = true
                }
            }
            // Reconciliación por ausencia: ya no está abierta en el servidor.
            if (!truncated) {
                val limit = corrNow + HORIZON_MS - 60_000L
                for (lo in db.orders("origin='server' AND status IN ('pending','delivered') AND execute_at_ms <= ?", arrayOf(limit.toString()))) {
                    if (lo.orderId in seen || lo.orderId in closedIds) continue
                    if (db.casOrder(d, lo.orderId, ProbeDb.OPEN, ContentValues().apply { put("status", "cancelled"); put("error", "cerrada-en-servidor") })) {
                        logs.add(Triple("warn", lo.orderId, "Orden ${lo.target} ya no está abierta en el servidor: cancelada"))
                        changed = true
                    }
                }
            }
            // El backend volvió: el plan local ya no aplica.
            for (lo in db.orders("origin='local' AND selection_reason='offline-schedule' AND status IN ('pending','delivered')", arrayOf())) {
                if (db.casOrder(d, lo.orderId, ProbeDb.OPEN, ContentValues().apply { put("status", "cancelled"); put("error", "backend-volvio") })) {
                    logs.add(Triple("info", lo.orderId, "Backend de vuelta: se cancela la orden del plan local"))
                    changed = true
                }
            }
        }
        for ((lvl, id, msg) in logs) db.event(lvl, ProbeState.phase, id, msg)
        return changed
    }

    private fun ack(s: ProbeSettings) {
        val list = db.orders("origin='server' AND acked=0", arrayOf(), orderBy = "created_ms", limit = 200)
        if (list.isEmpty()) return
        val body = JSONObject().put("order_ids", JSONArray(list.map { it.orderId }))
        val r = cp.request("POST", "${s.backendUrl}/api/v1/probes/${enc(s.probeId)}/orders/ack", s.apiKey, body.toString())
        if (r.code !in 200..299) return
        val o = JSONObject(r.body)
        val ids = ArrayList<String>()
        for (k in listOf("acked", "unchanged", "unknown")) {
            val a = o.optJSONArray(k) ?: continue
            for (i in 0 until a.length()) ids.add(a.optString(i))
        }
        db.tx { d ->
            for (id in ids) {
                d.update("orders", ContentValues().apply { put("acked", 1) }, "order_id=?", arrayOf(id))
                db.casOrder(d, id, listOf("pending"), ContentValues().apply { put("status", "delivered") })
            }
        }
    }

    // ----------------------------------------------------- outbox/results

    /// Vacía outbox en orden. false si el backend no respondió (se reintenta).
    fun flushOutbox(s: ProbeSettings, deadlineEl: Long = Long.MAX_VALUE): Boolean {
        val rows = db.db.rawQuery("SELECT id, order_id, body FROM outbox ORDER BY id LIMIT 200", null).use { c ->
            val out = ArrayList<Triple<Long, String, String>>()
            while (c.moveToNext()) out.add(Triple(c.getLong(0), c.getString(1), c.getString(2)))
            out
        }
        for ((id, orderId, body) in rows) {
            if (SystemClock.elapsedRealtime() > deadlineEl) return false
            val code = try {
                cp.request("POST", "${s.backendUrl}/api/v1/probes/${enc(s.probeId)}/orders/${enc(orderId)}/state", s.apiKey, body).code
            } catch (e: IOException) {
                db.db.execSQL("UPDATE outbox SET attempts = attempts + 1, last_error = ? WHERE id = ?", arrayOf<Any>(e.message ?: "io", id))
                return false
            }
            when {
                code in 200..299 || code == 404 -> db.db.delete("outbox", "id=?", arrayOf(id.toString()))
                code == 401 -> {
                    ProbeState.setAlert("backend-auth", "El backend rechazó la API key (401): revisa los ajustes de la sonda")
                    return false
                }
                code >= 500 -> {
                    db.db.execSQL("UPDATE outbox SET attempts = attempts + 1, last_error = ? WHERE id = ?", arrayOf<Any>("http-$code", id))
                    return false
                }
                else -> {
                    db.db.delete("outbox", "id=?", arrayOf(id.toString()))
                    db.event("error", ProbeState.phase, orderId, "El backend rechazó el cambio de estado (HTTP $code): se descarta")
                }
            }
        }
        return true
    }

    class SyncCounts(var uploaded: Int = 0, var rejected: Int = 0, var error: String? = null)

    /// Sube resultados pendientes en lotes de 20, más viejos primero.
    fun uploadResults(s: ProbeSettings, counts: SyncCounts = SyncCounts()): SyncCounts {
        for (round in 0 until 20) {
            val batch = db.pendingResults(20)
            if (batch.isEmpty()) break
            val before = counts.uploaded + counts.rejected
            if (!uploadBatch(s, batch, counts)) break
            if (counts.uploaded + counts.rejected == before) break // nada avanzó (errores reintentables)
        }
        return counts
    }

    private fun uploadBatch(s: ProbeSettings, batch: List<Pair<String, String>>, counts: SyncCounts): Boolean {
        val arr = JSONArray()
        for ((_, payload) in batch) arr.put(JSONObject(payload))
        val r = try {
            cp.request("POST", "${s.backendUrl}/api/v1/probes/${enc(s.probeId)}/results", s.apiKey,
                JSONObject().put("results", arr).toString(), readMs = 30000)
        } catch (e: IOException) {
            counts.error = e.message
            for ((id, _) in batch) db.markSyncFailed(id, e.message ?: "io")
            return false
        }
        when {
            r.code == 400 -> {
                val err = try { JSONObject(r.body).optString("error", "HTTP 400") } catch (_: Exception) { "HTTP 400" }
                if (batch.size == 1) {
                    db.markRejected(batch[0].first, err)
                    counts.rejected++
                    db.event("error", ProbeState.phase, null, "Resultado rechazado por el backend: $err")
                    return true
                }
                val half = batch.size / 2
                return uploadBatch(s, batch.subList(0, half), counts) && uploadBatch(s, batch.subList(half, batch.size), counts)
            }
            r.code == 401 -> {
                ProbeState.setAlert("backend-auth", "El backend rechazó la API key (401): revisa los ajustes de la sonda")
                counts.error = "API key rechazada (401)"
                return false
            }
            r.code !in 200..299 -> {
                counts.error = "HTTP ${r.code}"
                for ((id, _) in batch) db.markSyncFailed(id, "http-${r.code}")
                return false
            }
        }
        val o = try { JSONObject(r.body) } catch (_: Exception) { JSONObject() }
        val items = o.optJSONArray("results") ?: JSONArray()
        for (i in 0 until items.length()) {
            val it = items.optJSONObject(i) ?: continue
            val id = it.optStrN("result_id") ?: continue
            when (it.optString("status")) {
                "inserted", "duplicate" -> {
                    db.markSynced(id, it.optLongN("measurement_id"), it.optStrN("net_route"), it.optStrN("confidence"), it.optStrN("reason"))
                    counts.uploaded++
                }
                "error" -> if (!it.optBoolean("retryable", false)) {
                    val err = it.optString("error", "error")
                    db.markRejected(id, err)
                    counts.rejected++
                    db.event("error", ProbeState.phase, null, "Resultado $id rechazado: $err")
                } else db.markSyncFailed(id, it.optString("error", "error"))
            }
        }
        if (counts.uploaded > 0) db.event("info", ProbeState.phase, null, "Resultados subidos: ${counts.uploaded}")
        return true
    }

    /// "Sincronizar ahora" (§2.10).
    fun syncNow(): Map<String, Any?> {
        val s = prefs.snapshot()
        if (s.apiKey.isEmpty()) return mapOf("uploaded" to 0, "rejected" to 0, "error" to "Falta la API key del backend local")
        val c = SyncCounts()
        try {
            if (flushOutbox(s)) uploadResults(s, c) else c.error = c.error ?: "No se pudo vaciar la cola de estados"
        } catch (e: Exception) {
            c.error = e.message ?: e.javaClass.simpleName
        }
        return mapOf("uploaded" to c.uploaded, "rejected" to c.rejected, "error" to c.error)
    }

    // ----------------------------------------------------------- status

    private fun statusLoop() {
        if (stopped) return
        sendStatus(rebuild = true)
        handler?.postDelayed(statusR, prefs.int("status_interval_s") * 1000L)
    }

    private fun sendStatus(rebuild: Boolean) {
        if (stopped) return
        val status = (if (rebuild) buildStatus() else null) ?: ProbeBus.lastJson ?: buildStatus() ?: return
        lastStatusSentEl = SystemClock.elapsedRealtime()
        val s = prefs.snapshot()
        if (s.apiKey.isEmpty()) return
        try {
            cp.request("POST", "${s.backendUrl}/api/v1/probes/${enc(s.probeId)}/status", s.apiKey, status.toString())
        } catch (_: IOException) {
        }
    }

    /// Al detener: vaciar outbox una vez, 5 s como mucho.
    fun flushOnStop() {
        val s = prefs.snapshot()
        if (s.apiKey.isEmpty()) return
        try { flushOutbox(s, SystemClock.elapsedRealtime() + 5000) } catch (_: Exception) {}
    }
}
