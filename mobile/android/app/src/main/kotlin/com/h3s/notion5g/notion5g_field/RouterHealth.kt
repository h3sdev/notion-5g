package com.h3s.notion5g.notion5g_field

import org.json.JSONObject

/// Salud de cada router de la sonda (contrato §6.2), medida por el propio
/// MikroTik con pings chicos: a la IP de la regla `probe:health-<slot>` (que la
/// fuerza por la tabla de ese router: 9.9.9.9 → to-A, 149.112.112.112 → to-B) y
/// a la puerta de enlace de su WAN (leída de las rutas). Nunca sale del
/// celular: no gasta datos del teléfono ni toca la regla phone-probe.
///
/// El último estado por slot vive en kv `routers_health` (JSON) para que
/// `down_since` sobreviva a un reinicio de la app y el estado salga también con
/// el servicio detenido.
object RouterHealth {
    const val RULE_PREFIX = "probe:health-"
    private const val KV = "routers_health"

    class Check(
        val slot: String,
        val deviceId: String?,
        val internetOk: Boolean?,
        val gatewayOk: Boolean?,
        val lossPct: Double?,
        val rttMs: Double?,
        val gateway: String?,
        val healthIp: String?,
        val error: String?,
    )

    /// IP de salud del slot según su regla, o el motivo por el que no se puede usar.
    fun healthTarget(rules: Map<String, Mikrotik.AnyRule>, t: ProbeTarget): Pair<String?, String?> {
        val r = rules[RULE_PREFIX + t.slot]
            ?: return null to "falta la regla $RULE_PREFIX${t.slot} en el MikroTik (§6.1)"
        val ip = r.dstAddress?.substringBefore('/')?.ifEmpty { null }
        return when {
            r.disabled -> null to "la regla $RULE_PREFIX${t.slot} está deshabilitada"
            ip == null -> null to "la regla $RULE_PREFIX${t.slot} no tiene dst-address"
            r.table != t.table -> null to "la regla $RULE_PREFIX${t.slot} apunta a ${r.table}, no a ${t.table}"
            else -> ip to null
        }
    }

    private fun pingError(e: RouterOsException): String =
        if (e.kind == "trap" && (e.message ?: "").contains("permission", ignoreCase = true))
            "el usuario del MikroTik no tiene la política test (§6.1): ${e.message}"
        else "ping falló: ${e.message}"

    /// Verifica los slots dados en una sesión ya abierta. `count` = pings a la IP
    /// de salud (3 en reposo, 1 mientras se vigila un reinicio).
    fun check(
        mk: Mikrotik, api: RouterOsApi, targets: List<ProbeTarget>, routes: Map<String, Mikrotik.Route>,
        rules: Map<String, Mikrotik.AnyRule>, count: Int,
    ): List<Check> {
        val out = ArrayList<Check>()
        for (t in targets) {
            if (t.table.isEmpty()) continue
            val route = routes[t.table]
            val gw = route?.gatewayIp
            var gatewayOk: Boolean? = null
            var err: String? = null
            // Ruta de su tabla inactiva = puerto caído o router apagado: sin
            // camino no hay ping que valga (el MikroTik podría responder con un
            // !trap "no route" que dejaría internet_ok en null y nunca contaría
            // la caída para la recomendación de reinicio, §6.3).
            val linkDown = route != null && !route.active && !route.disabled
            if (linkDown) {
                out.add(Check(t.slot, t.deviceId, false, false, null, null, gw, healthTarget(rules, t).first,
                    "ruta de ${t.table} inactiva (puerto caído o router apagado)"))
                continue
            }
            if (gw != null) {
                gatewayOk = try { mk.ping(api, gw, 1).received > 0 } catch (e: RouterOsException) {
                    if (e.kind != "trap") throw e
                    err = pingError(e); null
                }
            } else err = "sin ruta por defecto en ${t.table}"
            val (ip, why) = healthTarget(rules, t)
            var internetOk: Boolean? = null
            var loss: Double? = null
            var rtt: Double? = null
            if (ip != null) {
                try {
                    val p = mk.ping(api, ip, count)
                    internetOk = p.received > 0
                    loss = ProbeUtil.round(p.lossPct, 1)
                    rtt = ProbeUtil.round(p.avgRttMs, 1)
                } catch (e: RouterOsException) {
                    if (e.kind != "trap") throw e
                    err = pingError(e)
                }
            } else err = err ?: why
            out.add(Check(t.slot, t.deviceId, internetOk, gatewayOk, loss, rtt, gw, ip, err))
        }
        return out
    }

    fun load(db: ProbeDb): JSONObject = try {
        db.kvGet(KV)?.let { JSONObject(it) } ?: JSONObject()
    } catch (_: Exception) {
        JSONObject()
    }

    /// Guarda las verificaciones: `down_since` = primera verificación con
    /// `internet_ok = false` de la racha actual (se borra al volver; si no se
    /// pudo saber, se conserva). Devuelve las transiciones para el registro.
    fun record(db: ProbeDb, checks: List<Check>, nowMs: Long = System.currentTimeMillis()): List<String> {
        val all = load(db)
        val logs = ArrayList<String>()
        for (c in checks) {
            val prev = all.optJSONObject(c.slot)
            val prevDown = prev?.optStrN("down_since")
            val down = when (c.internetOk) {
                false -> prevDown ?: Rfc3339.format(nowMs)
                true -> null
                null -> prevDown
            }
            val prevOk = if (prev == null || prev.isNull("internet_ok")) null else prev.optBoolean("internet_ok")
            if (c.internetOk == false && prevOk != false) logs.add("Router ${c.slot} sin Internet (${if (c.gatewayOk == false && c.lossPct == null) (c.error ?: "sin camino") else "ping a ${c.healthIp} sin respuesta"}${if (c.gatewayOk == false) "; puerta de enlace tampoco responde" else ""})")
            if (c.internetOk == true && prevOk == false) logs.add("Router ${c.slot} con Internet de nuevo${prevDown?.let { " (caído desde $it)" } ?: ""}")
            all.put(c.slot, JSONObject()
                .putN("internet_ok", c.internetOk)
                .putN("gateway_ok", c.gatewayOk)
                .putN("loss_pct", c.lossPct)
                .putN("rtt_ms", c.rttMs)
                .put("checked_at", Rfc3339.format(nowMs))
                .putN("down_since", down)
                .putN("device_id", c.deviceId)
                .putN("gateway", c.gateway)
                .putN("health_ip", c.healthIp)
                .putN("error", c.error))
        }
        db.kvSet(KV, all.toString())
        return logs
    }

    /// No se pudo abrir la sesión: se conservan los últimos valores (y
    /// `checked_at`, que dice su edad) y se anota el motivo.
    fun recordUnchecked(db: ProbeDb, targets: List<ProbeTarget>, msg: String) {
        val all = load(db)
        for (t in targets) {
            if (t.table.isEmpty()) continue
            val o = all.optJSONObject(t.slot) ?: JSONObject()
                .putN("internet_ok", null).putN("gateway_ok", null).putN("loss_pct", null).putN("rtt_ms", null)
                .putN("checked_at", null).putN("down_since", null).putN("device_id", t.deviceId)
                .putN("gateway", null).putN("health_ip", null)
            o.put("error", "no se pudo verificar: $msg")
            all.put(t.slot, o)
        }
        db.kvSet(KV, all.toString())
    }

    /// Bloque `routers` del estado en vivo: solo los slots de la sonda.
    fun json(db: ProbeDb, targets: List<ProbeTarget>): JSONObject {
        val all = load(db)
        val out = JSONObject()
        for (t in targets) {
            if (t.table.isEmpty()) continue
            all.optJSONObject(t.slot)?.let { out.put(t.slot, it) }
        }
        return out
    }
}
