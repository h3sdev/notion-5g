package com.h3s.notion5g.notion5g_field

import android.net.Network
import android.os.SystemClock
import org.json.JSONObject
import java.io.IOException
import java.net.ConnectException
import java.net.HttpURLConnection
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.NoRouteToHostException
import java.net.SocketTimeoutException
import java.net.URI
import java.net.URL
import javax.net.ssl.SSLException

/// La prueba se aborta (detenida o tope de tiempo con la regla forzada).
class ProbeAbort(val reason: String) : Exception(reason)

/// Pruebas atadas a una red (Ethernet o WiFi, contrato §2.3 d-h y §2.6):
/// portal cautivo, IP de salida, puerta de enlace, DNS, ping TCP, descarga y
/// subida. Nunca sigue redirecciones: un 3xx es un dato (portal), no algo que
/// seguir. Todas las conexiones llevan Connection: close y son nuevas.
class SpeedTester(
    private val network: Network,
    private val userAgent: String,
    /// Lanza ProbeAbort si hay que cortar; se llama entre bloques.
    private val checkpoint: () -> Unit,
    /// Texto de avance para la UI ("42,1 MB · 4 s").
    private val progress: (String) -> Unit,
) {
    companion object {
        const val CONNECT_MS = 5000
        const val READ_MS = 10000
        const val CHUNK = 64 * 1024
    }

    class PortalResult(val captive: Boolean?, val host: String?)
    class Meta(val ip: String?, val asn: Long?, val org: String?, val colo: String?)
    class PingResult(val medianMs: Double?, val jitterMs: Double?, val lossPct: Double, val samples: Int)
    class PhaseResult(val mbps: Double?, val bytes: Long, val seconds: Double, val error: String?, val concurrent: Int?, val redirectHost: String?)

    private fun open(url: String, method: String = "GET", headers: Map<String, String> = emptyMap(), readMs: Int = READ_MS): HttpURLConnection {
        val c = network.openConnection(URL(url)) as HttpURLConnection
        c.instanceFollowRedirects = false
        c.connectTimeout = CONNECT_MS
        c.readTimeout = readMs
        c.requestMethod = method
        c.useCaches = false
        c.setRequestProperty("Connection", "close")
        c.setRequestProperty("User-Agent", userAgent)
        for ((k, v) in headers) c.setRequestProperty(k, v)
        return c
    }

    private fun hostOfLocation(c: HttpURLConnection): String? = try {
        c.getHeaderField("Location")?.let { URI(it).host }
    } catch (_: Exception) {
        null
    }

    /// GET http://connectivitycheck.gstatic.com/generate_204 sin seguir redirecciones.
    fun portalCheck(): PortalResult {
        var c: HttpURLConnection? = null
        return try {
            c = open("http://connectivitycheck.gstatic.com/generate_204", readMs = 5000)
            c.connectTimeout = 5000
            val code = c.responseCode
            when {
                code == 204 -> PortalResult(false, null)
                code in 300..399 -> PortalResult(true, hostOfLocation(c))
                else -> PortalResult(true, null)
            }
        } catch (_: Exception) {
            PortalResult(null, null)
        } finally {
            c?.disconnect()
        }
    }

    /// IP de salida vista por Cloudflare: /meta y, si falla, /cdn-cgi/trace.
    fun meta(): Meta? {
        var c: HttpURLConnection? = null
        try {
            c = open("https://speed.cloudflare.com/meta", readMs = 5000)
            if (c.responseCode == 200) {
                val o = JSONObject(c.inputStream.bufferedReader().use { it.readText() })
                return Meta(o.optStrN("clientIp"), o.optLongN("asn"), o.optStrN("asOrganization"), o.optStrN("colo"))
            }
        } catch (_: Exception) {
        } finally {
            c?.disconnect()
        }
        c = null
        try {
            c = open("https://www.cloudflare.com/cdn-cgi/trace", readMs = 5000)
            if (c.responseCode == 200) {
                val kv = c.inputStream.bufferedReader().use { it.readText() }.lines()
                    .mapNotNull { l -> l.indexOf('=').takeIf { it > 0 }?.let { l.substring(0, it) to l.substring(it + 1) } }.toMap()
                if (kv["ip"] != null) return Meta(kv["ip"], null, null, kv["colo"])
            }
        } catch (_: Exception) {
        } finally {
            c?.disconnect()
        }
        return null
    }

    /// true si conecta o si responde RST (ECONNREFUSED: el equipo contestó);
    /// false si vence el tiempo o no hay ruta (router desconectado).
    fun tcpReachable(host: String, port: Int, timeoutMs: Int = 2000): Boolean {
        val s = network.socketFactory.createSocket()
        return try {
            val addr = InetAddress.getByName(host) // IP literal: no consulta DNS
            s.connect(InetSocketAddress(addr, port), timeoutMs)
            true
        } catch (e: SocketTimeoutException) {
            false
        } catch (e: NoRouteToHostException) {
            false
        } catch (e: ConnectException) {
            val m = e.message ?: ""
            m.contains("ECONNREFUSED") || m.contains("refused", ignoreCase = true)
        } catch (e: IOException) {
            false
        } finally {
            try { s.close() } catch (_: Exception) {}
        }
    }

    fun resolve(host: String): InetAddress? = try {
        network.getAllByName(host).firstOrNull()
    } catch (_: Exception) {
        null
    }

    /// 10 conexiones TCP, 200 ms entre cada una, 2 s de tope.
    fun ping(host: String, port: Int, count: Int = 10): PingResult {
        val addr = resolve(host) ?: return PingResult(null, null, 100.0, count)
        val times = ArrayList<Double>()
        for (i in 0 until count) {
            checkpoint()
            val s = network.socketFactory.createSocket()
            val t0 = SystemClock.elapsedRealtimeNanos()
            try {
                s.connect(InetSocketAddress(addr, port), 2000)
                times.add((SystemClock.elapsedRealtimeNanos() - t0) / 1e6)
            } catch (_: Exception) {
            } finally {
                try { s.close() } catch (_: Exception) {}
            }
            progress("${i + 1}/$count")
            if (i < count - 1) Thread.sleep(200)
        }
        val loss = (count - times.size) * 100.0 / count
        if (times.isEmpty()) return PingResult(null, null, loss, count)
        val sorted = times.sorted()
        val median = if (sorted.size % 2 == 1) sorted[sorted.size / 2] else (sorted[sorted.size / 2 - 1] + sorted[sorted.size / 2]) / 2
        val jitter = if (times.size < 2) 0.0 else (1 until times.size).map { Math.abs(times[it] - times[it - 1]) }.average()
        return PingResult(median, jitter, loss, count)
    }

    private fun failReason(e: Exception): String = when (e) {
        is SSLException -> "tls: ${e.javaClass.simpleName}"
        is SocketTimeoutException -> "tiempo agotado"
        is ProbeAbort -> throw e
        else -> e.javaClass.simpleName + (e.message?.let { ": " + it.take(80) } ?: "")
    }

    /// Descarga: repite pedidos hasta duration_s o maxBytes, en bloques de 64 KB.
    /// Mbps = bytes·8 / segundos desde el primer byte hasta el fin.
    fun download(url: () -> String, headers: Map<String, String>, durationS: Int, maxBytes: Long): PhaseResult {
        val buf = ByteArray(CHUNK)
        var bytes = 0L
        var firstNs = 0L
        var lastNs = 0L
        val limitNs = durationS * 1_000_000_000L
        var error: String? = null
        var concurrent: Int? = null
        var redirect: String? = null
        var lastProgress = 0L
        outer@ while (true) {
            checkpoint()
            var c: HttpURLConnection? = null
            try {
                c = open(url(), headers = headers)
                val code = c.responseCode
                if (code in 300..399) { redirect = hostOfLocation(c); error = "http-3xx"; break }
                if (code !in 200..299) { error = "http-$code"; break }
                c.getHeaderField("X-Speedtest-Concurrent")?.toIntOrNull()?.let { concurrent = it }
                val ins = c.inputStream
                var got = 0L
                while (true) {
                    val n = ins.read(buf)
                    if (n < 0) break
                    val now = SystemClock.elapsedRealtimeNanos()
                    if (firstNs == 0L) firstNs = now
                    bytes += n
                    got += n
                    lastNs = now
                    if (now - lastProgress > 1_000_000_000L) {
                        lastProgress = now
                        progress("${ProbeUtil.fmt1(bytes / 1e6)} MB · ${((now - firstNs) / 1e9).toInt()} s")
                    }
                    checkpoint()
                    if (now - firstNs >= limitNs || bytes >= maxBytes) break@outer
                }
                if (got == 0L) { error = "respuesta vacía"; break }
            } catch (e: ProbeAbort) {
                throw e
            } catch (e: Exception) {
                error = failReason(e)
                break
            } finally {
                c?.disconnect()
            }
        }
        val secs = if (firstNs > 0 && lastNs > firstNs) (lastNs - firstNs) / 1e9 else 0.0
        // Un corte (o un 429 en un pedido posterior) cuando ya hubo al menos 1 s
        // de datos no invalida la fase: se mide con lo que llegó.
        val enough = secs >= 1.0 && bytes > 0
        val mbps = if (enough) bytes * 8 / secs / 1e6 else null
        return PhaseResult(mbps, bytes, secs, if (enough) null else error ?: "sin datos", concurrent, redirect)
    }

    /// Varias conexiones a la vez (como fast.com): cada hilo corre `run` con su
    /// índice; Mbps = suma de bytes · 8 / la fase más larga. Falla solo si
    /// fallan todas. Un ProbeAbort en cualquier hilo corta la fase entera.
    fun parallel(n: Int, run: (SpeedTester, Int) -> PhaseResult): PhaseResult {
        val results = arrayOfNulls<PhaseResult>(n)
        var abort: ProbeAbort? = null
        var stop = false
        val sub = SpeedTester(network, userAgent, { if (stop) throw ProbeAbort("parallel-stop"); checkpoint() }, {})
        val threads = (0 until n).map { i ->
            Thread {
                try {
                    results[i] = run(sub, i)
                } catch (e: ProbeAbort) {
                    synchronized(this) { if (abort == null) abort = e }
                    stop = true
                } catch (e: Exception) {
                    results[i] = PhaseResult(null, 0, 0.0, failReason(e), null, null)
                }
            }.apply { name = "speed-$i"; start() }
        }
        val t0 = SystemClock.elapsedRealtime()
        while (threads.any { it.isAlive }) {
            threads.forEach { it.join(1000) }
            progress("$n conexiones · ${(SystemClock.elapsedRealtime() - t0) / 1000} s")
        }
        abort?.let { if (it.reason != "parallel-stop") throw it }
        val ok = results.filterNotNull().filter { it.bytes > 0 && it.seconds > 0 }
        val bytes = ok.sumOf { it.bytes }
        val secs = ok.maxOfOrNull { it.seconds } ?: 0.0
        val enough = secs >= 1.0 && bytes > 0
        return PhaseResult(if (enough) bytes * 8 / secs / 1e6 else null, bytes, secs,
            if (enough) null else results.filterNotNull().firstNotNullOfOrNull { it.error } ?: "sin datos",
            null, results.filterNotNull().firstNotNullOfOrNull { it.redirectHost })
    }

    /// Subida: POST de 10 MB repetidos (cuerpo desde un búfer reutilizado) hasta
    /// duration_s o maxBytes, contando los bytes escritos al socket.
    fun upload(url: () -> String, headers: Map<String, String>, durationS: Int, maxBytes: Long, bodyBytes: Int = 10_000_000): PhaseResult {
        val buf = ByteArray(CHUNK) { (it * 31 + 7).toByte() }
        var bytes = 0L
        val startNs = SystemClock.elapsedRealtimeNanos()
        var endNs = startNs
        val limitNs = durationS * 1_000_000_000L
        var error: String? = null
        var concurrent: Int? = null
        var redirect: String? = null
        var lastProgress = 0L
        var timeUp = false
        while (!timeUp) {
            checkpoint()
            var c: HttpURLConnection? = null
            try {
                c = open(url(), method = "POST", headers = headers + mapOf("Content-Type" to "application/octet-stream"))
                c.doOutput = true
                c.setFixedLengthStreamingMode(bodyBytes)
                val os = c.outputStream
                var left = bodyBytes
                while (left > 0) {
                    val n = minOf(left, buf.size)
                    os.write(buf, 0, n)
                    left -= n
                    bytes += n
                    val now = SystemClock.elapsedRealtimeNanos()
                    endNs = now
                    if (now - lastProgress > 1_000_000_000L) {
                        lastProgress = now
                        progress("${ProbeUtil.fmt1(bytes / 1e6)} MB · ${((now - startNs) / 1e9).toInt()} s")
                    }
                    checkpoint()
                    if (now - startNs >= limitNs || bytes >= maxBytes) { timeUp = true; break }
                }
                if (timeUp) break // cuerpo incompleto: se corta la conexión
                os.flush()
                val code = c.responseCode
                endNs = SystemClock.elapsedRealtimeNanos()
                if (code in 300..399) { redirect = hostOfLocation(c); error = "http-3xx"; break }
                if (code !in 200..299) { error = "http-$code"; break }
                c.getHeaderField("X-Speedtest-Concurrent")?.toIntOrNull()?.let { concurrent = it }
                try { c.inputStream.use { it.readBytes() } } catch (_: Exception) {}
            } catch (e: ProbeAbort) {
                throw e
            } catch (e: Exception) {
                error = failReason(e)
                break
            } finally {
                c?.disconnect()
            }
        }
        val secs = (endNs - startNs) / 1e9
        val enough = secs >= 1.0 && bytes > 0
        val mbps = if (enough) bytes * 8 / secs / 1e6 else null
        return PhaseResult(mbps, bytes, secs, if (enough) null else error ?: "sin datos", concurrent, redirect)
    }
}
