package com.h3s.notion5g.notion5g_field

import android.net.Network
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/// Destino "fast": lo mismo que hace la página fast.com (Netflix). Se piden a
/// api.fast.com los servidores OCA que le tocan a la IP de salida (muchas veces
/// dentro de la red del operador) y se mide con varias conexiones en paralelo.
/// Atado a la red de la prueba: la lista depende de por qué router se sale.
object FastCom {
    class Targets(
        val urls: List<String>,
        val clientIp: String?,
        val clientAsn: Long?,
        val clientIsp: String?,
        val servers: List<String>,
    )

    private const val TIMEOUT_MS = 8000
    @Volatile private var token: String? = null

    private fun get(network: Network, url: String, ua: String): String {
        val c = network.openConnection(URL(url)) as HttpURLConnection
        c.connectTimeout = TIMEOUT_MS
        c.readTimeout = TIMEOUT_MS
        c.instanceFollowRedirects = false
        c.setRequestProperty("User-Agent", ua)
        try {
            if (c.responseCode !in 200..299) throw java.io.IOException("HTTP ${c.responseCode} de ${URL(url).host}")
            val bytes = c.inputStream.use { it.readBytes() }
            // api.fast.com a veces responde en Latin-1 ("Bogotá" llega como un byte 0xE1).
            return try {
                Charsets.UTF_8.newDecoder().decode(java.nio.ByteBuffer.wrap(bytes)).toString()
            } catch (_: java.nio.charset.CharacterCodingException) {
                String(bytes, Charsets.ISO_8859_1)
            }
        } finally {
            c.disconnect()
        }
    }

    /// El token está en el JS de la página; se guarda mientras la app viva.
    private fun token(network: Network, ua: String, refresh: Boolean): String {
        if (!refresh) token?.let { return it }
        val html = get(network, "https://fast.com/", ua)
        val js = Regex("src=\"(/app-[a-z0-9]+\\.js)\"").find(html)?.groupValues?.get(1)
            ?: throw java.io.IOException("fast.com: no se encontró el script")
        val t = Regex("token:\"([A-Za-z0-9]+)\"").find(get(network, "https://fast.com$js", ua))?.groupValues?.get(1)
            ?: throw java.io.IOException("fast.com: no se encontró el token")
        token = t
        return t
    }

    fun targets(network: Network, ua: String, count: Int = 5): Targets {
        var lastErr: Exception? = null
        for (refresh in listOf(false, true)) {
            try {
                val j = JSONObject(get(network, "https://api.fast.com/netflix/speedtest/v2?https=true&token=${token(network, ua, refresh)}&urlCount=$count", ua))
                val arr = j.getJSONArray("targets")
                val urls = ArrayList<String>()
                val servers = ArrayList<String>()
                for (i in 0 until arr.length()) {
                    val t = arr.getJSONObject(i)
                    val u = t.optString("url")
                    if (!u.startsWith("https://")) continue
                    urls.add(u)
                    servers.add("${URL(u).host.substringBefore('.')}@${t.optJSONObject("location")?.optString("city") ?: "?"}")
                }
                if (urls.isEmpty()) throw java.io.IOException("fast.com: sin servidores")
                val cl = j.optJSONObject("client")
                return Targets(urls, cl?.optString("ip")?.ifEmpty { null }, cl?.optString("asn")?.toLongOrNull(),
                    cl?.optString("isp")?.ifEmpty { null }, servers)
            } catch (e: Exception) {
                lastErr = e
            }
        }
        throw lastErr ?: java.io.IOException("fast.com: error")
    }

    fun downloadUrl(u: String): String = u.replaceFirst("/speedtest", "/speedtest/range/0-26214400")
    fun uploadUrl(u: String): String = u.replaceFirst("/speedtest", "/speedtest/range/0-0")
}
