package com.h3s.notion5g.notion5g_field

import android.content.ContentValues
import android.content.Context
import android.database.Cursor
import android.database.sqlite.SQLiteDatabase
import android.database.sqlite.SQLiteOpenHelper
import android.util.Log
import org.json.JSONObject

/// Base local de la sonda (contrato §2.2): órdenes, resultados, cambios de
/// estado por mandar, fixes de GPS y el registro que ve el usuario. Una sola
/// instancia por proceso (servicio y canal de Flutter la comparten). Nunca se
/// borra: son datos de campo. Cambios futuros de esquema: subir la versión y
/// ALTER TABLE ... ADD COLUMN en onUpgrade.
class ProbeDb private constructor(context: Context) : SQLiteOpenHelper(context, "probe.db", null, 2) {
    companion object {
        private const val TAG = "ProbeDb"
        @Volatile private var inst: ProbeDb? = null

        fun get(context: Context): ProbeDb =
            inst ?: synchronized(this) { inst ?: ProbeDb(context.applicationContext).also { inst = it } }

        val OPEN = listOf("pending", "delivered")
        const val TYPE_SPEEDTEST = "run_speedtest"
        const val TYPE_REBOOT = "reboot_router"
        val FINAL = setOf("done", "failed", "expired", "interrupted", "cancelled")
    }

    init {
        setWriteAheadLoggingEnabled(true)
    }

    override fun onCreate(db: SQLiteDatabase) {
        db.execSQL(
            """CREATE TABLE orders (
              order_id         TEXT PRIMARY KEY,
              origin           TEXT NOT NULL,
              server_id        INTEGER,
              target           TEXT NOT NULL,
              slot             TEXT,
              device_id        TEXT,
              routing_table    TEXT,
              allow_fallback   INTEGER NOT NULL DEFAULT 0,
              duration_s       INTEGER NOT NULL DEFAULT 10,
              execute_at_ms    INTEGER NOT NULL,
              not_after_ms     INTEGER,
              selection_reason TEXT NOT NULL,
              requested_by     TEXT,
              status           TEXT NOT NULL,
              acked            INTEGER NOT NULL DEFAULT 0,
              result_id        TEXT,
              started_ms       INTEGER,
              error            TEXT,
              created_ms       INTEGER NOT NULL,
              updated_ms       INTEGER NOT NULL
            )"""
        )
        db.execSQL("CREATE INDEX idx_orders_due ON orders(status, execute_at_ms)")
        addV2Columns(db)
        db.execSQL(
            """CREATE TABLE results (
              result_id         TEXT PRIMARY KEY,
              order_id          TEXT,
              attempt           INTEGER NOT NULL DEFAULT 1,
              created_ms        INTEGER NOT NULL,
              payload           TEXT NOT NULL,
              sync_status       TEXT NOT NULL DEFAULT 'pending',
              sync_attempts     INTEGER NOT NULL DEFAULT 0,
              last_sync_error   TEXT,
              synced_ms         INTEGER,
              measurement_id    INTEGER,
              server_net_route  TEXT,
              server_confidence TEXT,
              server_reason     TEXT
            )"""
        )
        db.execSQL("CREATE INDEX idx_results_sync ON results(sync_status, created_ms)")
        db.execSQL(
            """CREATE TABLE outbox (
              id         INTEGER PRIMARY KEY AUTOINCREMENT,
              order_id   TEXT NOT NULL,
              body       TEXT NOT NULL,
              created_ms INTEGER NOT NULL,
              attempts   INTEGER NOT NULL DEFAULT 0,
              last_error TEXT
            )"""
        )
        db.execSQL(
            """CREATE TABLE gps_fixes (
              id                  INTEGER PRIMARY KEY AUTOINCREMENT,
              result_id           TEXT,
              taken_ms            INTEGER NOT NULL,
              fix_time_ms         INTEGER,
              elapsed_realtime_ns INTEGER,
              lat REAL, lon REAL, accuracy_m REAL, speed_mps REAL,
              provider            TEXT,
              satellites_used     INTEGER,
              status              TEXT NOT NULL
            )"""
        )
        db.execSQL(
            """CREATE TABLE events (
              id       INTEGER PRIMARY KEY AUTOINCREMENT,
              ts_ms    INTEGER NOT NULL,
              level    TEXT NOT NULL,
              phase    TEXT,
              order_id TEXT,
              msg      TEXT NOT NULL
            )"""
        )
        db.execSQL("CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT)")
    }

    override fun onUpgrade(db: SQLiteDatabase, oldVersion: Int, newVersion: Int) {
        if (oldVersion < 2) addV2Columns(db)
    }

    /// Versión 2 (contrato §6.2): tipo de orden (run_speedtest | reboot_router)
    /// y el resultado de las órdenes que no generan medición (reinicio).
    private fun addV2Columns(db: SQLiteDatabase) {
        db.execSQL("ALTER TABLE orders ADD COLUMN type TEXT NOT NULL DEFAULT 'run_speedtest'")
        db.execSQL("ALTER TABLE orders ADD COLUMN result_json TEXT")
    }

    val db: SQLiteDatabase get() = writableDatabase

    fun <T> tx(block: (SQLiteDatabase) -> T): T {
        val d = db
        d.beginTransaction()
        try {
            val r = block(d)
            d.setTransactionSuccessful()
            return r
        } finally {
            d.endTransaction()
        }
    }

    // ---------------------------------------------------------------- kv

    fun kvGet(k: String): String? =
        db.rawQuery("SELECT v FROM kv WHERE k=?", arrayOf(k)).use { if (it.moveToFirst()) it.getString(0) else null }

    fun kvSet(k: String, v: String?, d: SQLiteDatabase = db) {
        if (v == null) d.delete("kv", "k=?", arrayOf(k))
        else d.insertWithOnConflict("kv", null, ContentValues().apply { put("k", k); put("v", v) }, SQLiteDatabase.CONFLICT_REPLACE)
    }

    fun kvLong(k: String): Long? = kvGet(k)?.toLongOrNull()

    // ------------------------------------------------------------ events

    private var eventCount = 0

    fun event(level: String, phase: String?, orderId: String?, msg: String) {
        try {
            db.insert("events", null, ContentValues().apply {
                put("ts_ms", System.currentTimeMillis())
                put("level", level)
                put("phase", phase)
                put("order_id", orderId)
                put("msg", msg)
            })
            if (++eventCount % 50 == 0) {
                db.execSQL("DELETE FROM events WHERE id <= (SELECT MAX(id) - 2000 FROM events)")
            }
        } catch (e: Exception) {
            Log.w(TAG, "no se pudo guardar el evento", e)
        }
        when (level) {
            "error" -> Log.e("Probe", msg)
            "warn" -> Log.w("Probe", msg)
            else -> Log.i("Probe", msg)
        }
    }

    fun recentEvents(limit: Int): List<Map<String, Any?>> =
        db.rawQuery("SELECT id, ts_ms, level, phase, order_id, msg FROM events ORDER BY id DESC LIMIT ?", arrayOf(limit.toString())).use { c ->
            val out = ArrayList<Map<String, Any?>>()
            while (c.moveToNext()) {
                out.add(mapOf(
                    "id" to c.getLong(0),
                    "at" to Rfc3339.format(c.getLong(1)),
                    "level" to c.getString(2),
                    "phase" to c.getStringOrNull(3),
                    "order_id" to c.getStringOrNull(4),
                    "msg" to c.getString(5),
                ))
            }
            out
        }

    // ------------------------------------------------------------ orders

    data class Order(
        val orderId: String,
        val origin: String,
        val serverId: Long?,
        val target: String,
        val slot: String?,
        val deviceId: String?,
        val routingTable: String?,
        val allowFallback: Boolean,
        val durationS: Int,
        val executeAtMs: Long,
        val notAfterMs: Long?,
        val selectionReason: String,
        val requestedBy: String?,
        val status: String,
        val acked: Boolean,
        val resultId: String?,
        val startedMs: Long?,
        val error: String?,
        val createdMs: Long,
        val type: String = TYPE_SPEEDTEST,
        val resultJson: String? = null,
        val updatedMs: Long = 0L,
    ) {
        val isReboot: Boolean get() = type == TYPE_REBOOT
    }

    private fun Cursor.getStringOrNull(i: Int): String? = if (isNull(i)) null else getString(i)
    private fun Cursor.getLongOrNull(i: Int): Long? = if (isNull(i)) null else getLong(i)

    private val orderCols = "order_id, origin, server_id, target, slot, device_id, routing_table, allow_fallback, duration_s, " +
        "execute_at_ms, not_after_ms, selection_reason, requested_by, status, acked, result_id, started_ms, error, created_ms, " +
        "type, result_json, updated_ms"

    private fun Cursor.toOrder() = Order(
        orderId = getString(0), origin = getString(1), serverId = getLongOrNull(2), target = getString(3),
        slot = getStringOrNull(4), deviceId = getStringOrNull(5), routingTable = getStringOrNull(6),
        allowFallback = getInt(7) != 0, durationS = getInt(8), executeAtMs = getLong(9), notAfterMs = getLongOrNull(10),
        selectionReason = getString(11), requestedBy = getStringOrNull(12), status = getString(13), acked = getInt(14) != 0,
        resultId = getStringOrNull(15), startedMs = getLongOrNull(16), error = getStringOrNull(17), createdMs = getLong(18),
        type = getStringOrNull(19) ?: TYPE_SPEEDTEST, resultJson = getStringOrNull(20), updatedMs = getLong(21),
    )

    fun orders(where: String, args: Array<String>, orderBy: String = "execute_at_ms, server_id, created_ms", limit: Int = 500): List<Order> =
        db.rawQuery("SELECT $orderCols FROM orders WHERE $where ORDER BY $orderBy LIMIT $limit", args).use { c ->
            val out = ArrayList<Order>()
            while (c.moveToNext()) out.add(c.toOrder())
            out
        }

    fun order(orderId: String): Order? = orders("order_id=?", arrayOf(orderId)).firstOrNull()

    /// Comparación-y-cambio: solo cambia la orden si sigue en uno de `from`.
    /// true = este hilo ganó; false = otro la cambió antes (se respeta).
    fun casOrder(d: SQLiteDatabase, orderId: String, from: Collection<String>, cv: ContentValues): Boolean {
        cv.put("updated_ms", System.currentTimeMillis())
        val ph = from.joinToString(",") { "?" }
        return d.update("orders", cv, "order_id=? AND status IN ($ph)", arrayOf(orderId) + from.toTypedArray()) > 0
    }

    fun addOutbox(d: SQLiteDatabase, orderId: String, body: JSONObject) {
        d.insert("outbox", null, ContentValues().apply {
            put("order_id", orderId)
            put("body", body.toString())
            put("created_ms", System.currentTimeMillis())
        })
    }

    fun insertLocalOrder(d: SQLiteDatabase, id: String, target: String, executeAt: Long, notAfter: Long,
                         reason: String, requestedBy: String, durationS: Int) {
        val now = System.currentTimeMillis()
        d.insertOrThrow("orders", null, ContentValues().apply {
            put("order_id", id); put("origin", "local"); put("target", target)
            put("allow_fallback", 0); put("duration_s", durationS)
            put("execute_at_ms", executeAt); put("not_after_ms", notAfter)
            put("selection_reason", reason); put("requested_by", requestedBy)
            put("status", "pending"); put("acked", 1); put("created_ms", now); put("updated_ms", now)
        })
    }

    /// Últimas órdenes de reinicio (abiertas primero), para la pantalla.
    fun recentReboots(limit: Int): List<Map<String, Any?>> {
        val offset = kvLong("clock_offset_ms") ?: 0L
        return orders("type='$TYPE_REBOOT'", arrayOf(),
            orderBy = "CASE WHEN status IN ('pending','delivered','running') THEN 0 ELSE 1 END, updated_ms DESC", limit = limit).map { o ->
            val res = o.resultJson?.let { try { JSONObject(it) } catch (_: Exception) { null } }
            mapOf(
                "order_id" to o.orderId,
                "origin" to o.origin,
                "slot" to (o.slot ?: o.target),
                "device_id" to o.deviceId,
                "status" to o.status,
                "error" to o.error,
                "requested_by" to o.requestedBy,
                "created_at" to Rfc3339.format(o.createdMs),
                "updated_at" to Rfc3339.format(o.updatedMs),
                "execute_at" to Rfc3339.format(o.executeAtMs - offset),
                "result" to res?.let { JsonConv.toPlatform(it) },
            )
        }
    }

    fun countOpenOrders(): Int =
        db.rawQuery("SELECT COUNT(*) FROM orders WHERE status IN ('pending','delivered')", null).use { if (it.moveToFirst()) it.getInt(0) else 0 }

    // ----------------------------------------------------------- results

    fun insertResult(d: SQLiteDatabase, resultId: String, orderId: String?, attempt: Int, payload: JSONObject) {
        d.insertOrThrow("results", null, ContentValues().apply {
            put("result_id", resultId)
            put("order_id", orderId)
            put("attempt", attempt)
            put("created_ms", System.currentTimeMillis())
            put("payload", payload.toString())
            put("sync_status", "pending")
        })
    }

    fun hasResult(resultId: String?): Boolean {
        if (resultId == null) return false
        return db.rawQuery("SELECT 1 FROM results WHERE result_id=?", arrayOf(resultId)).use { it.moveToFirst() }
    }

    fun countResults(status: String): Int =
        db.rawQuery("SELECT COUNT(*) FROM results WHERE sync_status=?", arrayOf(status)).use { if (it.moveToFirst()) it.getInt(0) else 0 }

    fun countOutbox(): Int = db.rawQuery("SELECT COUNT(*) FROM outbox", null).use { if (it.moveToFirst()) it.getInt(0) else 0 }

    fun pendingResults(limit: Int): List<Pair<String, String>> =
        db.rawQuery("SELECT result_id, payload FROM results WHERE sync_status='pending' ORDER BY created_ms LIMIT ?", arrayOf(limit.toString())).use { c ->
            val out = ArrayList<Pair<String, String>>()
            while (c.moveToNext()) out.add(c.getString(0) to c.getString(1))
            out
        }

    fun markSynced(resultId: String, measurementId: Long?, route: String?, confidence: String?, reason: String?) {
        db.update("results", ContentValues().apply {
            put("sync_status", "synced")
            put("synced_ms", System.currentTimeMillis())
            if (measurementId != null) put("measurement_id", measurementId)
            if (route != null) put("server_net_route", route)
            if (confidence != null) put("server_confidence", confidence)
            if (reason != null) put("server_reason", reason)
            putNull("last_sync_error")
        }, "result_id=?", arrayOf(resultId))
    }

    fun markRejected(resultId: String, error: String) {
        db.update("results", ContentValues().apply {
            put("sync_status", "rejected")
            put("last_sync_error", error)
        }, "result_id=?", arrayOf(resultId))
    }

    fun markSyncFailed(resultId: String, error: String) {
        db.execSQL(
            "UPDATE results SET sync_attempts = sync_attempts + 1, last_sync_error = ? WHERE result_id = ?",
            arrayOf(error, resultId)
        )
    }

    /// Últimos resultados para la pantalla (recentResults, contrato §2.10).
    fun recentResults(limit: Int): List<Map<String, Any?>> =
        db.rawQuery(
            "SELECT result_id, order_id, attempt, created_ms, payload, sync_status, server_net_route, server_confidence, server_reason, last_sync_error " +
                "FROM results ORDER BY created_ms DESC LIMIT ?", arrayOf(limit.toString())
        ).use { c ->
            val out = ArrayList<Map<String, Any?>>()
            while (c.moveToNext()) {
                val p = try { JSONObject(c.getString(4)) } catch (_: Exception) { JSONObject() }
                out.add(mapOf(
                    "result_id" to c.getString(0),
                    "order_id" to c.getStringOrNull(1),
                    "attempt" to c.getInt(2),
                    "created_at" to Rfc3339.format(c.getLong(3)),
                    "test_started_at" to p.optStrN("test_started_at"),
                    "slot" to p.optStrN("slot"),
                    "device_id" to p.optStrN("device_id"),
                    "test_status" to p.optStrN("test_status"),
                    "down_mbps" to p.optDoubleN("down_mbps"),
                    "up_mbps" to p.optDoubleN("up_mbps"),
                    "ping_ms" to p.optDoubleN("ping_ms"),
                    "loss_pct" to p.optDoubleN("loss_pct"),
                    "location_status" to p.optStrN("location_status"),
                    "gps_age_s" to p.optDoubleN("gps_age_s"),
                    "net_path" to p.optStrN("net_path"),
                    "routing_table" to p.optStrN("routing_table"),
                    "selection_reason" to p.optStrN("selection_reason"),
                    "executed_offline" to p.optBoolean("executed_offline", false),
                    "egress_ip" to p.optStrN("egress_ip"),
                    "egress_asn_client" to p.optLongN("egress_asn_client"),
                    "egress_as_org_client" to p.optStrN("egress_as_org_client"),
                    "captive_portal" to (if (p.isNull("captive_portal")) null else p.optBoolean("captive_portal")),
                    "portal_host" to p.optStrN("portal_host"),
                    "target_host" to p.optStrN("target_host"),
                    "sync_status" to c.getString(5),
                    "server_net_route" to c.getStringOrNull(6),
                    "server_confidence" to c.getStringOrNull(7),
                    "server_reason" to c.getStringOrNull(8),
                    "sync_error" to c.getStringOrNull(9),
                    "error" to p.optStrN("error"),
                ))
            }
            out
        }

    // --------------------------------------------------------- gps_fixes

    fun insertFix(cv: ContentValues) {
        try {
            db.insert("gps_fixes", null, cv)
        } catch (e: Exception) {
            Log.w(TAG, "no se pudo guardar el fix", e)
        }
    }

    fun lastFix(): ContentValues? =
        db.rawQuery("SELECT taken_ms, fix_time_ms, lat, lon, accuracy_m, provider, satellites_used, status FROM gps_fixes ORDER BY id DESC LIMIT 1", null).use { c ->
            if (!c.moveToFirst()) null else ContentValues().apply {
                put("taken_ms", c.getLong(0))
                if (!c.isNull(1)) put("fix_time_ms", c.getLong(1))
                if (!c.isNull(2)) put("lat", c.getDouble(2))
                if (!c.isNull(3)) put("lon", c.getDouble(3))
                if (!c.isNull(4)) put("accuracy_m", c.getDouble(4))
                put("provider", c.getStringOrNull(5))
                if (!c.isNull(6)) put("satellites_used", c.getInt(6))
                put("status", c.getString(7))
            }
        }
}
