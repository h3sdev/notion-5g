package com.h3s.notion5g.notion5g_field

import android.net.Network
import android.os.SystemClock
import org.json.JSONObject

/// Estado del módem del router de un slot, leído justo antes de cada prueba
/// (docs/PENDIENTE-SENAL-POR-SSH.md del servidor). Por SSH corre los mismos
/// `ubus` que `readLocalStatus()` del agente (cmd/routeragent) y devuelve los
/// campos con los nombres que el backend ya guarda en columnas (`operator`,
/// `rat`, `band_lte`, `rsrp_dbm`…). Nunca `serial_atcmd`: el puerto AT es un
/// canal único compartido con el firmware y puede dejarlo bloqueado.
///
/// Si el equipo no tiene SSH (el Notion 4G: puerto 22 cerrado), se usa la
/// lectura de su web vieja (`RouterWeb.readSignal`) con `signal_source =
/// router-web`. Si todo falla, solo `signal_error`: la prueba se corre igual.
object RouterSignal {
    const val BUDGET_MS = 8000L

    const val COMMAND = "ubus -t 6 call cm get_zcainfo; echo '@@'; ubus -t 6 call cm get_link_context; echo '@@'; " +
        "ubus -t 6 call util_wan get_network_mode; echo '@@'; cat /proc/uptime; echo '@@'; ubus -t 6 call cm get_eng_info"

    /// Campos planos que se copian al resultado (van a columnas o a `raw`).
    val FIELDS = listOf(
        "operator", "rat", "band_lte", "pci", "earfcn", "bw_mhz", "rsrp_dbm", "rsrq_db", "sinr_db", "rssi_dbm", "ca_secondary",
        "band_lte_ca", "pci_ca", "earfcn_ca",
        "nr_band", "nr_pci", "nr_arfcn", "nr_rsrp_dbm", "nr_rsrq_db", "nr_sinr_db", "nr_bw_mhz", "nr_bw_prb", "nr_scs_khz", "zcainfo_nr_raw", "eng_info_nr_raw", "eng_info_raw",
        "eps_reg", "nw_mode", "prefer_mode", "nr_mode", "uptime_s", "ecgi",
    )

    /// Slots cuyo puerto 22 rechazó la conexión: se va directo a la web
    /// (sin perder un intento SSH por prueba). Se vuelve a probar SSH cada hora.
    private val sshRefusedAt = HashMap<String, Long>()
    private const val SSH_RETRY_MS = 60 * 60_000L

    /// Lee la señal. Siempre devuelve un objeto con `signal_source` o
    /// `signal_error`, y `signal_read_at`; `signal_ms` es lo que tardó.
    fun read(network: Network, host: String, slot: String, ssh: ProbeSettings.Ssh, budgetMs: Long = BUDGET_MS): JSONObject {
        val t0 = SystemClock.elapsedRealtime()
        val skipSsh = sshRefusedAt[slot]?.let { t0 - it < SSH_RETRY_MS } == true
        var sshError: String? = null
        var sshDetail: String? = null
        if (!skipSsh) {
            val c = RouterSsh.capture(network, host, ssh, COMMAND, budgetMs)
            if (c.stdout != null) {
                sshRefusedAt.remove(slot)
                val parsed = try { parse(c.stdout) } catch (_: Exception) { null }
                return if (parsed != null && parsed.length() > 0) finish(parsed.put("signal_source", "ssh-ubus"), t0)
                else finish(JSONObject().put("signal_error", "parse").put("signal_detail", c.stdout.take(300)), t0)
            }
            if (c.refused) sshRefusedAt[slot] = SystemClock.elapsedRealtime()
            sshError = c.error ?: "ssh-connect"
            sshDetail = c.detail
            // Clave rechazada o sin tiempo: no se insiste por la web.
            if (!c.refused) return finish(JSONObject().put("signal_error", sshError).putN("signal_detail", sshDetail?.take(200)), t0)
        }
        // Respaldo: web vieja del equipo (solo el 4G "LTE Wireless Router" la trae).
        val web = RouterWeb.readSignal(network, host)
        if (web != null) {
            val o = JSONObject()
            for (k in FIELDS) if (web.has(k) && !web.isNull(k)) o.put(k, web.get(k))
            return finish(o.put("signal_source", "router-web"), t0)
        }
        return finish(JSONObject().put("signal_error", sshError ?: "ssh-connect")
            .put("signal_detail", (if (skipSsh) "SSH cerrado; " else "") + "la web del router no dio la señal"), t0)
    }

    private fun finish(o: JSONObject, t0: Long): JSONObject =
        o.put("signal_read_at", Rfc3339.format(System.currentTimeMillis()))
            .put("signal_ms", SystemClock.elapsedRealtime() - t0)

    /// Salida de `COMMAND` → campos. Misma lógica que readLocalStatus() del agente.
    fun parse(out: String): JSONObject {
        val parts = out.split(Regex("(?m)^@@\\s*$"))
        fun obj(i: Int): JSONObject? = parts.getOrNull(i)?.let { p ->
            val a = p.indexOf('{')
            val b = p.lastIndexOf('}')
            if (a >= 0 && b > a) try { JSONObject(p.substring(a, b + 1)) } catch (_: Exception) { null } else null
        }
        val st = JSONObject()
        obj(0)?.let { zca ->
            val lte = zca.optJSONObject("lte") ?: JSONObject()
            valid(lte, "p_band")?.let { st.put("band_lte", it) }
            valid(lte, "p_pci")?.let { st.put("pci", it) }
            valid(lte, "p_dlEuArfcn")?.let { st.put("earfcn", it) }
            num(lte, "p_dlBandwitdh")?.let { st.put("bw_mhz", it) }
            num(lte, "p_rsrp")?.let { if (it != 255.0) st.put("rsrp_dbm", clean(-141 + it)) }
            num(lte, "p_rsrq")?.let { if (it != 255.0) st.put("rsrq_db", clean(Math.round((-19.5 + it * 0.5) * 100) / 100.0)) }
            num(lte, "p_sinr")?.let { st.put("sinr_db", clean(it)) }
            num(lte, "p_rssi")?.let { if (it != 99.0) st.put("rssi_dbm", clean(-111 + it)) }
            num(lte, "s_status")?.let { st.put("ca_secondary", it != 0.0) }
            // Portadora secundaria de la CA LTE (la web del equipo la muestra como "B7/B2").
            if (num(lte, "s_status")?.let { it != 0.0 } != false) {
                valid(lte, "s_band")?.let { st.put("band_lte_ca", it) }
                valid(lte, "s_pci")?.let { st.put("pci_ca", it) }
                valid(lte, "s_dlEuArfcn")?.let { st.put("earfcn_ca", it) }
            }

            // Bloque NR: nombres sin confirmar en el firmware (igual que el agente).
            val nr = listOf("nr", "nr5g", "NR", "endc", "sa", "nsa").firstNotNullOfOrNull { k -> zca.optJSONObject(k)?.takeIf { it.length() > 0 } }
            if (nr != null) {
                firstValid(nr, "n_band", "band", "nr_band", "p_band")?.let { st.put("nr_band", it) }
                firstValid(nr, "n_pci", "pci", "p_pci")?.let { st.put("nr_pci", it) }
                firstValid(nr, "n_arfcn", "nrarfcn", "arfcn", "p_dlEuArfcn")?.let { st.put("nr_arfcn", it) }
                listOf("n_rsrp", "rsrp", "p_rsrp").firstNotNullOfOrNull { num(nr, it) }?.let { st.put("nr_rsrp_dbm", clean(it)) }
                listOf("n_sinr", "sinr", "p_sinr").firstNotNullOfOrNull { num(nr, it) }?.let { st.put("nr_sinr_db", clean(it)) }
                if (!st.has("nr_band")) st.put("zcainfo_nr_raw", nr)
            }
        }
        obj(1)?.optJSONObject("celluar_basic_info")?.let { info ->
            info.optString("network_name").takeIf { it.isNotEmpty() }?.let { st.put("operator", it) }
            num(info, "RegStatus")?.let { st.put("eps_reg", it.toInt()) }
            // 0 sin servicio · 1 2G/3G · 2 LTE · 3 LTE-CA · 4 5G SA · 5 5G NSA (ENDC)
            when (num(info, "sys_mode")?.toInt()) {
                1 -> "2G/3G"
                2 -> "LTE"
                3 -> "LTE-CA"
                4 -> "5G-SA"
                5 -> "5G-NSA"
                else -> null
            }?.let { st.put("rat", it) }
        }
        if (!st.has("rat") && st.has("band_lte")) st.put("rat", "LTE")
        obj(2)?.optJSONObject("net_mode")?.let { nm ->
            for (k in listOf("nw_mode", "prefer_mode", "nr_mode")) if (nm.has(k) && !nm.isNull(k)) st.put(k, nm.get(k))
        }
        parts.getOrNull(3)?.trim()?.split(Regex("\\s+"))?.firstOrNull()?.toDoubleOrNull()?.let { st.put("uptime_s", it.toLong()) }
        // Portadora NR de la ENDC: `cm get_eng_info` → `nr` (los mismos campos que
        // lee la sección ENDC de /js/panel/internet/engineeringInfo.js del equipo).
        // get_zcainfo solo trae las portadoras LTE (p_* y s_*). En NSA la red
        // agrega la NR con tráfico y la suelta al quedar quieto: por eso también
        // se lee al terminar la prueba (signal_end).
        // La web lo busca con jQuery .find("nr"): a cualquier profundidad.
        val eng = obj(4)
        val engNr = eng?.let { findObj(it, "nr", 0) }
        if (engNr == null) {
            // Sin bloque NR donde se esperaba: se adjunta la salida cruda (acotada)
            // para ver la forma real del objeto en este firmware.
            parts.getOrNull(4)?.trim()?.takeIf { it.isNotEmpty() }?.let { st.put("eng_info_raw", it.take(1500)) }
        }
        engNr?.takeIf { it.length() > 0 }?.let { nr ->
            val band = valid(nr, "band")
            if (band != null) {
                st.put("nr_band", band)
                valid(nr, "phy_cell_id")?.let { st.put("nr_pci", it) }
                valid(nr, "dl_nrafcn")?.let { st.put("nr_arfcn", it) }
                num(nr, "rsrp")?.let { st.put("nr_rsrp_dbm", clean(it)) }
                num(nr, "rsrq")?.let { st.put("nr_rsrq_db", clean(it)) }
                num(nr, "sinr")?.let { st.put("nr_sinr_db", clean(it)) }
                // dl_bandwidth viene en PRB y dl_scs como código (0=15, 1=30, 2=60 kHz):
                // visto el 2026-09-30 en TIGO n78 → 162 PRB a 30 kHz = 60 MHz.
                val scsKhz = when (num(nr, "dl_scs")?.toInt()) { 0 -> 15; 1 -> 30; 2 -> 60; else -> null }
                scsKhz?.let { st.put("nr_scs_khz", it) }
                num(nr, "dl_bandwidth")?.toInt()?.takeIf { it > 0 }?.let { prb ->
                    st.put("nr_bw_prb", prb)
                    nrBandwidthMhz(prb, scsKhz)?.let { st.put("nr_bw_mhz", it) }
                }
                st.remove("zcainfo_nr_raw")
            } else {
                // Bloque NR sin banda: se adjunta crudo para diagnosticar.
                st.put("eng_info_nr_raw", nr)
            }
        }
        return st
    }

    /// Ancho de banda NR (TS 38.101-1, tabla 5.3.2-1) a partir de los PRB y el SCS.
    private fun nrBandwidthMhz(prb: Int, scsKhz: Int?): Int? = when (scsKhz) {
        15 -> mapOf(25 to 5, 52 to 10, 79 to 15, 106 to 20, 133 to 25, 160 to 30, 216 to 40, 270 to 50)[prb]
        30 -> mapOf(11 to 5, 24 to 10, 38 to 15, 51 to 20, 65 to 25, 78 to 30, 106 to 40, 133 to 50, 162 to 60, 189 to 70, 217 to 80, 245 to 90, 273 to 100)[prb]
        60 -> mapOf(11 to 10, 18 to 15, 24 to 20, 31 to 25, 38 to 30, 51 to 40, 65 to 50, 79 to 60, 93 to 70, 107 to 80, 121 to 90, 135 to 100)[prb]
        else -> null
    }

    /// Primer sub-objeto llamado `key` a cualquier profundidad (también dentro de arreglos).
    private fun findObj(o: JSONObject, key: String, depth: Int): JSONObject? {
        if (depth > 6) return null
        o.optJSONObject(key)?.let { return it }
        for (k in o.keys()) {
            when (val v = o.opt(k)) {
                is JSONObject -> findObj(v, key, depth + 1)?.let { return it }
                is org.json.JSONArray -> for (i in 0 until v.length()) v.optJSONObject(i)?.let { x -> findObj(x, key, depth + 1)?.let { return it } }
            }
        }
        return null
    }

    /// Número del campo (el firmware a veces lo manda como texto).
    private fun num(o: JSONObject, k: String): Double? = when (val v = o.opt(k)) {
        is Number -> v.toDouble()
        is String -> v.trim().toDoubleOrNull()
        else -> null
    }?.takeIf { !it.isNaN() && !it.isInfinite() }

    /// Entero de banda/PCI/ARFCN; 0 y -1 son "sin dato" (el dashboard mostraría "banda 0").
    private fun valid(o: JSONObject, k: String): Long? = num(o, k)?.takeIf { it != 0.0 && it != -1.0 }?.toLong()

    private fun firstValid(o: JSONObject, vararg keys: String): Long? = keys.firstNotNullOfOrNull { valid(o, it) }

    /// Entero si no tiene decimales (−67.0 → −67).
    private fun clean(v: Double): Any = if (v == Math.floor(v) && Math.abs(v) < 1e9) v.toLong() else v
}
