package com.h3s.notion5g.notion5g_field

import android.net.Network
import java.net.HttpURLConnection
import java.net.URL
import java.security.MessageDigest
import java.security.SecureRandom

/// Reinicio de un Notion por su API web (`xml_action.cgi`, la misma que usa el
/// botón "Reiniciar" de su página), para los equipos que tienen el SSH apagado
/// (el Notion 4G: puerto 22 cerrado). Protocolo en `scripts/modem_http_api.py`:
/// Digest casero con realm/nonce fijos, login en `login.cgi` cuyo resultado va en
/// el header `WWW-Authenticate: status=0,...` y cookie `CGISID`.
///
/// Atado a Ethernet como el SSH (sale por la regla `probe:mgmt-<slot>`). Un solo
/// intento de login por orden: el equipo bloquea el login tras varios fallos.
object RouterWeb {
    class Outcome(val ok: Boolean, val error: String?, val detail: String?, val authenticated: Boolean)

    private const val URI_FOR_DIGEST = "/cgi/xml_action.cgi"
    private const val CSRF = "hfiehifejfklihefiuehflejhfueihfeuihfeui" // constante del propio JS del equipo
    private const val TIMEOUT_MS = 8000
    const val USER = "admin"
    const val PASSWORD = "admin"

    private fun md5(s: String): String =
        MessageDigest.getInstance("MD5").digest(s.toByteArray()).joinToString("") { "%02x".format(it) }

    private fun cnonce(): String {
        val b = ByteArray(8)
        SecureRandom().nextBytes(b)
        return b.joinToString("") { "%02x".format(it) }
    }

    private fun post(network: Network, url: String, body: String, headers: Map<String, String>): HttpURLConnection {
        val c = network.openConnection(URL(url)) as HttpURLConnection
        c.connectTimeout = TIMEOUT_MS
        c.readTimeout = TIMEOUT_MS
        c.instanceFollowRedirects = false
        c.useCaches = false
        c.requestMethod = "POST"
        c.doOutput = true
        for ((k, v) in headers) c.setRequestProperty(k, v)
        c.outputStream.use { it.write(body.toByteArray()) }
        c.responseCode // fuerza la respuesta (401 incluido) sin lanzar
        return c
    }

    private fun drain(c: HttpURLConnection): String = try {
        (if (c.responseCode >= 400) c.errorStream else c.inputStream)?.use { it.readBytes().decodeToString() } ?: ""
    } catch (_: Exception) {
        ""
    } finally {
        c.disconnect()
    }

    private fun get(network: Network, url: String, headers: Map<String, String> = emptyMap()): HttpURLConnection {
        val c = network.openConnection(URL(url)) as HttpURLConnection
        c.connectTimeout = TIMEOUT_MS
        c.readTimeout = TIMEOUT_MS
        c.instanceFollowRedirects = false
        c.useCaches = false
        for ((k, v) in headers) c.setRequestProperty(k, v)
        c.responseCode
        return c
    }

    fun reboot(network: Network, host: String, user: String = USER, password: String = PASSWORD): Outcome {
        // El Notion 4G ("LTE Wireless Router") trae la interfaz Marvell vieja
        // (login por GET firmado con /cgi/protected.cgi y reinicio con
        // json_device_restart); el 5G, la nueva (login por POST y xml_action.cgi).
        val title = try {
            Regex("<title>(.*?)</title>", RegexOption.IGNORE_CASE).find(drain(get(network, "http://$host/")))?.groupValues?.get(1)
        } catch (_: Exception) {
            null
        }
        return if (title?.contains("LTE", ignoreCase = true) == true) rebootLegacy(network, host, user, password)
        else rebootXml(network, host, user, password)
    }

    /// Interfaz vieja (probado contra el Notion 4G PB017, firmware R0238, el 2026-09-29).
    private fun rebootLegacy(network: Network, host: String, user: String, password: String): Outcome {
        val base = "http://$host"
        try {
            val c1 = get(network, "$base/login.cgi")
            val www = c1.getHeaderField("WWW-Authenticate") ?: ""
            drain(c1)
            if (!www.startsWith("Digest")) return Outcome(false, "web-connect", "login.cgi sin challenge Digest", false)
            val parts = www.substringAfter(' ').split(',').mapNotNull {
                val kv = it.trim().split('=', limit = 2)
                if (kv.size == 2) kv[0] to kv[1].trim('"') else null
            }.toMap()
            val realm = parts["realm"] ?: return Outcome(false, "web-connect", "challenge sin realm", false)
            val nonce = parts["nonce"] ?: return Outcome(false, "web-connect", "challenge sin nonce", false)
            val qop = parts["qop"] ?: "auth"
            val ha1 = md5("$user:$realm:$password")
            var nc = 1
            fun authHeader(): String {
                val cn = cnonce()
                val n = "%08x".format(nc++)
                val res = md5("$ha1:$nonce:$n:$cn:$qop:${md5("GET:$URI_FOR_DIGEST")}")
                return "Digest username=\"$user\", realm=\"$realm\", nonce=\"$nonce\", uri=\"$URI_FOR_DIGEST\", " +
                    "response=\"$res\", qop=$qop, nc=$n, cnonce=\"$cn\""
            }
            val cn = cnonce()
            val res = md5("$ha1:$nonce:00000001:$cn:$qop:${md5("GET:/cgi/protected.cgi")}")
            val c2 = get(network, "$base/login.cgi?Action=Digest&username=$user&realm=$realm&nonce=$nonce&response=$res" +
                "&qop=$qop&cnonce=$cn&temp=marvell", mapOf("Authorization" to authHeader()))
            if (!drain(c2).contains("200 OK")) {
                return Outcome(false, "web-auth", "login web rechazado (interfaz vieja); no se reintenta para no bloquear el equipo", false)
            }
            val body = try {
                drain(get(network, "$base/xml_action.cgi?method=get&module=duster&file=json_device_restart${System.currentTimeMillis()}",
                    mapOf("Authorization" to authHeader())))
            } catch (e: java.io.IOException) {
                return Outcome(true, null, "sin respuesta a device_restart (${e.javaClass.simpleName}): probablemente ya se estaba reiniciando", true)
            }
            if (body.contains("UNAUTHORIZED") || body.contains("KICKOFF")) {
                return Outcome(false, "web-auth", "device_restart rechazado (sesión no autorizada)", true)
            }
            return Outcome(true, null, null, true)
        } catch (e: Exception) {
            return Outcome(false, "web-connect", "${e.javaClass.simpleName}${e.message?.let { ": " + it.take(140) } ?: ""}", false)
        }
    }

    private fun rebootXml(network: Network, host: String, user: String, password: String): Outcome {
        val base = "http://$host"
        try {
            // 1. Challenge: realm/nonce/qop.
            val c1 = post(network, "$base/login.cgi", "", emptyMap())
            val www = c1.getHeaderField("WWW-Authenticate") ?: ""
            drain(c1)
            if (!www.startsWith("Digest")) return Outcome(false, "web-connect", "login.cgi sin challenge Digest", false)
            val parts = www.substringAfter(' ').split(',').mapNotNull {
                val kv = it.trim().split('=', limit = 2)
                if (kv.size == 2) kv[0] to kv[1].trim('"') else null
            }.toMap()
            val realm = parts["realm"] ?: return Outcome(false, "web-connect", "challenge sin realm", false)
            val nonce = parts["nonce"] ?: return Outcome(false, "web-connect", "challenge sin nonce", false)
            val qop = parts["qop"] ?: "auth"
            val ha1 = md5("$user:$realm:$password")

            // 2. Login (el HA2 del login usa "GET" aunque sea un POST, como doLogin() del equipo).
            val cn = cnonce()
            val resp = md5("$ha1:$nonce:00000001:$cn:$qop:${md5("GET:$URI_FOR_DIGEST")}")
            val c2 = post(network, "$base/login.cgi",
                "Action=Digest&username=$user&realm=$realm&nonce=$nonce&response=$resp&qop=$qop&cnonce=$cn&nc=00000001&temp=marvell",
                mapOf("Content-Type" to "application/x-www-form-urlencoded"))
            val result = c2.getHeaderField("WWW-Authenticate") ?: ""
            val sid = c2.headerFields["Set-Cookie"].orEmpty().firstOrNull { it.startsWith("CGISID=") }
                ?.substringBefore(';')?.substringAfter('=')
            drain(c2)
            val status = result.substringBefore(',').substringAfterLast('=')
            if (status != "0") {
                return Outcome(false, "web-auth", "login web rechazado (status=$status); no se reintenta para no bloquear el equipo", false)
            }
            if (sid.isNullOrEmpty()) return Outcome(false, "web-auth", "login sin cookie CGISID", false)

            // 3. router/router_call_reboot.
            val cn2 = cnonce()
            val resp2 = md5("$ha1:$nonce:00000001:$cn2:$qop:${md5("POST:$URI_FOR_DIGEST")}")
            val xml = "<?xml version=\"1.0\" encoding=\"US-ASCII\"?><RGW><param><method>call</method><session>000</session>" +
                "<obj_path>router</obj_path><obj_method>router_call_reboot</obj_method></param></RGW>"
            val c3 = try { post(network, "$base/xml_action.cgi?method=set", xml, mapOf(
                "Authorization" to "Digest username=\"$user\", realm=\"$realm\", nonce=\"$nonce\", uri=\"$URI_FOR_DIGEST\", " +
                    "response=\"$resp2\", qop=$qop, nc=00000001, cnonce=\"$cn2\"",
                "Content-Type" to "application/x-www-form-urlencoded; charset=UTF-8",
                "X-Requested-With" to "XMLHttpRequest",
                "csrftoken" to CSRF,
                "Cookie" to "CGISID=$sid; Path=/",
            )) } catch (e: java.io.IOException) {
                // El equipo puede cortar la conexión al empezar a reiniciarse:
                // el pedido salió; la vigilancia confirma si se apagó y volvió.
                return Outcome(true, null, "sin respuesta a router_call_reboot (${e.javaClass.simpleName}): probablemente ya se estaba reiniciando", true)
            }
            val code = c3.responseCode
            val body = drain(c3)
            val err = Regex("<error_cause>(\\d+)</error_cause>").find(body)?.groupValues?.get(1)
            if (code != 200 || (err != null && err != "0")) {
                return Outcome(false, "web-reboot", "router_call_reboot: HTTP $code error_cause=${err ?: "-"}", true)
            }
            return Outcome(true, null, null, true)
        } catch (e: Exception) {
            return Outcome(false, "web-connect", "${e.javaClass.simpleName}${e.message?.let { ": " + it.take(140) } ?: ""}", false)
        }
    }
}
