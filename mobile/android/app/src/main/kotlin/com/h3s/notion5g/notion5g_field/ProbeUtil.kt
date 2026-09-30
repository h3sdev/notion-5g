package com.h3s.notion5g.notion5g_field

import android.content.Context
import android.os.Build
import org.json.JSONArray
import org.json.JSONObject
import java.net.Inet4Address
import java.net.InetAddress
import java.net.URI
import java.text.SimpleDateFormat
import java.util.Calendar
import java.util.Date
import java.util.Locale
import java.util.TimeZone
import java.util.UUID

/// Utilidades compartidas por la sonda A/B (ProbeService y el canal de Flutter).

/// Horas RFC 3339 en UTC, con "Z" y sin fracciones (contrato §0).
object Rfc3339 {
    private val fmt = object : ThreadLocal<SimpleDateFormat>() {
        override fun initialValue(): SimpleDateFormat =
            SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss'Z'", Locale.US).apply { timeZone = TimeZone.getTimeZone("UTC") }
    }
    private val re = Regex("""^(\d{4})-(\d{2})-(\d{2})[Tt ](\d{2}):(\d{2}):(\d{2})(\.\d+)?([Zz]|[+-]\d{2}:?\d{2})?$""")

    fun format(ms: Long): String = fmt.get()!!.format(Date(ms))
    fun formatOrNull(ms: Long?): String? = if (ms == null || ms <= 0) null else format(ms)

    /// Tolerante: acepta fracciones y desfases (-05:00); sin zona = UTC.
    fun parse(s: String?): Long? {
        if (s.isNullOrBlank()) return null
        val m = re.matchEntire(s.trim()) ?: return null
        val g = m.groupValues
        val cal = Calendar.getInstance(TimeZone.getTimeZone("UTC"))
        cal.clear()
        cal.set(g[1].toInt(), g[2].toInt() - 1, g[3].toInt(), g[4].toInt(), g[5].toInt(), g[6].toInt())
        var ms = cal.timeInMillis
        if (g[7].isNotEmpty()) ms += (("0" + g[7]).toDouble() * 1000).toLong()
        val z = g[8]
        if (z.isNotEmpty() && z != "Z" && z != "z") {
            val sign = if (z[0] == '-') -1 else 1
            val d = z.substring(1).replace(":", "")
            val off = (d.substring(0, 2).toInt() * 60 + d.substring(2, 4).toInt()) * 60_000L
            ms -= sign * off
        }
        return ms
    }
}

object ProbeUtil {
    fun uuid(): String = UUID.randomUUID().toString().lowercase(Locale.US)

    fun appVersion(ctx: Context): String = try {
        val pi = ctx.packageManager.getPackageInfo(ctx.packageName, 0)
        @Suppress("DEPRECATION")
        val code = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) pi.longVersionCode else pi.versionCode.toLong()
        "${pi.versionName}+$code"
    } catch (_: Exception) {
        "?"
    }

    /// true si el host es una IP literal privada (RFC 1918) o de enlace local:
    /// el backend local solo se alcanza por el WiFi de la oficina.
    fun isPrivateHost(host: String?): Boolean {
        if (host.isNullOrEmpty()) return false
        if (!Regex("""^\d{1,3}(\.\d{1,3}){3}$""").matches(host)) return false
        return try {
            val a = InetAddress.getByName(host)
            a is Inet4Address && (a.isSiteLocalAddress || a.isLinkLocalAddress || a.isLoopbackAddress)
        } catch (_: Exception) {
            false
        }
    }

    fun hostOf(url: String?): String? = try {
        URI(url ?: "").host
    } catch (_: Exception) {
        null
    }

    fun portOf(url: String?): Int = try {
        val u = URI(url ?: "")
        if (u.port > 0) u.port else if (u.scheme == "https") 443 else 80
    } catch (_: Exception) {
        80
    }

    fun round(v: Double?, decimals: Int): Double? {
        if (v == null || v.isNaN() || v.isInfinite()) return null
        var f = 1.0
        repeat(decimals) { f *= 10 }
        return Math.round(v * f) / f
    }

    /// "85,3" con coma decimal, para los textos en español.
    fun fmt1(v: Double?): String = if (v == null) "-" else String.format(Locale("es", "CO"), "%.1f", v)
}

/// put que nunca omite la clave: null va como JSON null (el contrato exige
/// que los campos que no aplican vayan null, no ausentes). Los NaN/infinito
/// también van null (JSONObject los rechaza).
fun JSONObject.putN(k: String, v: Any?): JSONObject {
    val x: Any? = when (v) {
        is Double -> if (v.isNaN() || v.isInfinite()) null else v
        is Float -> if (v.isNaN() || v.isInfinite()) null else v.toDouble()
        else -> v
    }
    return put(k, x ?: JSONObject.NULL)
}

fun JSONObject.optStrN(k: String): String? = if (!has(k) || isNull(k)) null else optString(k)
fun JSONObject.optLongN(k: String): Long? = if (!has(k) || isNull(k)) null else optLong(k)
fun JSONObject.optDoubleN(k: String): Double? = if (!has(k) || isNull(k)) null else optDouble(k).takeIf { !it.isNaN() }

/// Conversión JSON ↔ tipos del StandardMessageCodec de Flutter.
object JsonConv {
    fun toPlatform(v: Any?): Any? = when (v) {
        null, JSONObject.NULL -> null
        is JSONObject -> {
            val m = HashMap<String, Any?>()
            val it = v.keys()
            while (it.hasNext()) {
                val k = it.next()
                m[k] = toPlatform(v.opt(k))
            }
            m
        }
        is JSONArray -> (0 until v.length()).map { toPlatform(v.opt(it)) }
        else -> v
    }
}
