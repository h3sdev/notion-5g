package com.h3s.notion5g.notion5g_field

import android.net.Network
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

/// HTTP al backend de la sonda eligiendo camino (contrato §2.8):
///  1) atado al WiFi si está arriba;
///  2) atado a Ethernet solo con la regla confirmada en el respaldo, sin prueba
///     en curso y si el backend no es una IP privada (el local no se alcanza
///     por los routers);
///  3) la red por defecto, solo si no es Ethernet (o si lo es y valen las
///     condiciones de 2).
/// Durante una prueba solo WiFi.
class ControlPlane(
    private val ethernet: () -> Network?,
    private val wifi: () -> Network?,
    private val defaultType: () -> String,
    private val userAgent: String,
) {
    class Resp(val code: Int, val body: String, val path: String, val date: Long)

    /// Entre switching_route y restoring_route (lo pone el worker).
    @Volatile var testActive = false
    /// La última lectura de la regla dijo fallback_table (lo pone el worker).
    @Volatile var ruleOnFallback = false
    /// Último camino que funcionó: wifi | ethernet | default | none.
    @Volatile var lastPath = "none"

    private fun candidates(host: String?): List<Pair<String, Network?>> {
        val out = ArrayList<Pair<String, Network?>>()
        wifi()?.let { out.add("wifi" to it) }
        if (testActive) return out
        val ethOk = ruleOnFallback && !ProbeUtil.isPrivateHost(host)
        val eth = ethernet()
        if (eth != null && ethOk) out.add("ethernet" to eth)
        val def = defaultType()
        if (def != "none" && (def != "ethernet" || ethOk) && !(def == "wifi" && out.any { it.first == "wifi" })) out.add("default" to null)
        return out
    }

    fun request(method: String, url: String, apiKey: String, body: String? = null, readMs: Int = 15000): Resp {
        val host = ProbeUtil.hostOf(url)
        val cands = candidates(host)
        if (cands.isEmpty()) throw IOException(if (testActive) "sin WiFi durante la prueba" else "sin red hacia el backend")
        var last: Exception? = null
        for ((name, net) in cands) {
            var c: HttpURLConnection? = null
            try {
                val u = URL(url)
                c = (if (net != null) net.openConnection(u) else u.openConnection()) as HttpURLConnection
                c.instanceFollowRedirects = false
                c.connectTimeout = 5000
                c.readTimeout = readMs
                c.requestMethod = method
                c.useCaches = false
                c.setRequestProperty("User-Agent", userAgent)
                c.setRequestProperty("Accept", "application/json")
                if (apiKey.isNotEmpty()) c.setRequestProperty("X-API-Key", apiKey)
                if (body != null) {
                    c.doOutput = true
                    c.setRequestProperty("Content-Type", "application/json")
                    val b = body.toByteArray(Charsets.UTF_8)
                    c.setFixedLengthStreamingMode(b.size)
                    c.outputStream.use { it.write(b) }
                }
                val code = c.responseCode
                val text = try {
                    (if (code >= 400) c.errorStream else c.inputStream)?.bufferedReader()?.use { it.readText() } ?: ""
                } catch (_: IOException) {
                    ""
                }
                lastPath = name
                return Resp(code, text, name, c.getHeaderFieldDate("Date", 0))
            } catch (e: Exception) {
                last = e
            } finally {
                c?.disconnect()
            }
        }
        throw IOException("backend no alcanzable: ${last?.javaClass?.simpleName}${last?.message?.let { " ($it)" } ?: ""}".take(200))
    }
}
