package com.h3s.notion5g.notion5g_field

import android.net.Network
import org.json.JSONObject
import java.io.BufferedInputStream
import java.io.BufferedOutputStream
import java.io.ByteArrayOutputStream
import java.io.Closeable
import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.net.InetSocketAddress
import java.net.Socket

/// Error de la API del MikroTik. `kind` se usa para elegir la alerta:
/// unreachable → mikrotik-unreachable, login → mikrotik-login,
/// rule-missing → mikrotik-rule-missing, trap/fatal/io → error de la operación.
class RouterOsException(val kind: String, message: String) : IOException(message)

/// Cliente mínimo de la API clásica de RouterOS (TCP 8728, contrato §3.6): la
/// REST de la 7.6 exige www-ssl. Palabras con prefijo de largo, frase
/// terminada en palabra vacía, respuestas !re/!done/!trap/!fatal. Login plano
/// (RouterOS >= 6.43). Una sesión por operación: se abre, se usa y se cierra.
class RouterOsApi private constructor(private val sock: Socket) : Closeable {
    companion object {
        /// Socket atado a la red dada (Ethernet): creado sin conectar desde su
        /// socketFactory y luego connect con tope.
        fun open(network: Network, host: String, port: Int, timeoutMs: Int = 5000): RouterOsApi {
            val s = network.socketFactory.createSocket()
            try {
                val addr = network.getAllByName(host).first()
                s.connect(InetSocketAddress(addr, port), timeoutMs)
                s.soTimeout = timeoutMs
                s.tcpNoDelay = true
            } catch (e: Exception) {
                try { s.close() } catch (_: Exception) {}
                throw RouterOsException("unreachable", "MikroTik $host:$port no responde (${e.javaClass.simpleName})")
            }
            return RouterOsApi(s)
        }
    }

    private val ins: InputStream = BufferedInputStream(sock.getInputStream())
    private val out = BufferedOutputStream(sock.getOutputStream())

    fun login(user: String, password: String) {
        try {
            run("/login", "=name=$user", "=password=$password")
        } catch (e: RouterOsException) {
            if (e.kind == "trap") throw RouterOsException("login", "MikroTik rechazó el usuario $user: ${e.message}")
            throw e
        }
    }

    /// Manda una frase y lee hasta !done. Devuelve los atributos de cada !re.
    /// Un !trap (seguido de !done) lanza RouterOsException("trap").
    fun run(vararg words: String): List<Map<String, String>> {
        for (w in words) writeWord(w)
        writeWord("")
        out.flush()
        val res = ArrayList<Map<String, String>>()
        var trap: String? = null
        while (true) {
            val s = readSentence()
            if (s.isEmpty()) continue
            val type = s[0]
            val attrs = HashMap<String, String>()
            for (w in s.drop(1)) {
                if (w.startsWith("=")) {
                    val i = w.indexOf('=', 1)
                    if (i > 0) attrs[w.substring(1, i)] = w.substring(i + 1) else attrs[w.substring(1)] = ""
                }
            }
            when (type) {
                "!re" -> res.add(attrs)
                "!trap" -> if (trap == null) trap = attrs["message"] ?: "error"
                "!fatal" -> throw RouterOsException("fatal", "MikroTik cerró la sesión: ${s.drop(1).joinToString(" ")}")
                "!done" -> {
                    if (trap != null) throw RouterOsException("trap", trap)
                    return res
                }
                // !empty (RouterOS 7.18+) u otras: se ignoran.
            }
        }
    }

    private fun writeWord(w: String) {
        val b = w.toByteArray(Charsets.UTF_8)
        writeLen(b.size)
        out.write(b)
    }

    private fun writeLen(n: Int) {
        when {
            n < 0x80 -> out.write(n)
            n < 0x4000 -> { out.write((n shr 8) or 0x80); out.write(n and 0xFF) }
            n < 0x200000 -> { out.write((n shr 16) or 0xC0); out.write((n shr 8) and 0xFF); out.write(n and 0xFF) }
            n < 0x10000000 -> {
                out.write((n shr 24) or 0xE0); out.write((n shr 16) and 0xFF); out.write((n shr 8) and 0xFF); out.write(n and 0xFF)
            }
            else -> {
                out.write(0xF0); out.write((n shr 24) and 0xFF); out.write((n shr 16) and 0xFF); out.write((n shr 8) and 0xFF); out.write(n and 0xFF)
            }
        }
    }

    private fun readByte(): Int {
        val b = ins.read()
        if (b < 0) throw EOFException("el MikroTik cerró la conexión")
        return b
    }

    private fun readLen(): Int {
        val c = readByte()
        return when {
            c and 0x80 == 0 -> c
            c and 0xC0 == 0x80 -> ((c and 0x3F) shl 8) or readByte()
            c and 0xE0 == 0xC0 -> ((c and 0x1F) shl 16) or (readByte() shl 8) or readByte()
            c and 0xF0 == 0xE0 -> ((c and 0x0F) shl 24) or (readByte() shl 16) or (readByte() shl 8) or readByte()
            c == 0xF0 -> (readByte() shl 24) or (readByte() shl 16) or (readByte() shl 8) or readByte()
            else -> throw IOException("largo de palabra inválido: $c")
        }
    }

    private fun readSentence(): List<String> {
        val words = ArrayList<String>()
        while (true) {
            val n = readLen()
            if (n == 0) return words
            val buf = ByteArrayOutputStream(n)
            val tmp = ByteArray(minOf(n, 8192))
            var left = n
            while (left > 0) {
                val r = ins.read(tmp, 0, minOf(left, tmp.size))
                if (r < 0) throw EOFException("el MikroTik cerró la conexión")
                buf.write(tmp, 0, r)
                left -= r
            }
            words.add(buf.toString("UTF-8"))
        }
    }

    override fun close() {
        try { sock.close() } catch (_: Exception) {}
    }
}

/// Operaciones de la sonda sobre el MikroTik (contrato §3.5-§3.6). El celular
/// solo cambia `table` de la regla phone-probe y borra conexiones rastreadas de
/// su propia IP; nunca crea, borra ni reordena nada.
class Mikrotik(
    private val network: Network,
    private val host: String,
    private val port: Int,
    private val user: String,
    private val password: String,
    private val ruleComment: String,
) {
    data class Rule(val id: String, val table: String, val action: String, val disabled: Boolean, val inactive: Boolean) {
        fun json(): JSONObject = JSONObject().put("table", table).put("action", action).put("disabled", disabled).put("inactive", inactive)
    }

    data class Route(val table: String, val gateway: String?, val active: Boolean, val distance: Int, val disabled: Boolean) {
        val gatewayIp: String? get() = gateway?.substringBefore('%')?.ifEmpty { null }
        val gatewayIface: String? get() = gateway?.let { if (it.contains('%')) it.substringAfter('%') else null }
        fun json(): JSONObject = JSONObject().put("active", active).putN("gateway", gateway)
    }

    private fun flag(v: String?): Boolean = v == "true" || v == "yes"

    fun <T> session(block: (RouterOsApi) -> T): T {
        if (password.isEmpty()) throw RouterOsException("login", "Falta la contraseña del MikroTik en los ajustes")
        return RouterOsApi.open(network, host, port).use { api ->
            api.login(user, password)
            block(api)
        }
    }

    fun readRule(api: RouterOsApi): Rule {
        val r = api.run("/routing/rule/print", "?comment=$ruleComment", "=.proplist=.id,table,action,disabled,inactive")
        if (r.size != 1) {
            throw RouterOsException("rule-missing",
                if (r.isEmpty()) "No hay regla con comment=$ruleComment en el MikroTik" else "Hay ${r.size} reglas con comment=$ruleComment (debe haber una)")
        }
        val a = r[0]
        return Rule(a[".id"] ?: "", a["table"] ?: "", a["action"] ?: "", flag(a["disabled"]), flag(a["inactive"]))
    }

    fun setRuleTable(api: RouterOsApi, id: String, table: String) {
        api.run("/routing/rule/set", "=.id=$id", "=table=$table", "=action=lookup-only-in-table", "=disabled=no")
    }

    /// Ruta por defecto de una tabla (la activa si hay varias).
    fun defaultRoute(api: RouterOsApi, table: String): Route? {
        val r = api.run("/ip/route/print", "?routing-table=$table", "?dst-address=0.0.0.0/0", "=.proplist=gateway,active,disabled,distance")
        val routes = r.map { Route(table, it["gateway"], flag(it["active"]), it["distance"]?.toIntOrNull() ?: 0, flag(it["disabled"])) }
        return routes.firstOrNull { it.active } ?: routes.filter { !it.disabled }.minByOrNull { it.distance } ?: routes.firstOrNull()
    }

    /// Todas las rutas por defecto, agrupadas por tabla: por tabla de equipo la
    /// activa o la primera; para `main` la activa (por dónde sale el respaldo).
    fun allDefaultRoutes(api: RouterOsApi): Map<String, Route> {
        val r = api.run("/ip/route/print", "?dst-address=0.0.0.0/0", "=.proplist=routing-table,gateway,active,distance,disabled")
        val all = r.map { Route(it["routing-table"] ?: "main", it["gateway"], flag(it["active"]), it["distance"]?.toIntOrNull() ?: 0, flag(it["disabled"])) }
        val out = LinkedHashMap<String, Route>()
        for ((t, list) in all.groupBy { it.table }) {
            out[t] = list.firstOrNull { it.active } ?: list.filter { !it.disabled }.minByOrNull { it.distance } ?: list.first()
        }
        return out
    }

    /// Borra las conexiones rastreadas cuyo origen es la IP del celular (salvo
    /// la propia sesión de la API). Devuelve cuántas borró, o -1 si la tabla es
    /// demasiado grande y se omitió.
    fun flushConntrack(api: RouterOsApi, phoneIp: String): Int {
        val r = api.run("/ip/firewall/connection/print", "=.proplist=.id,src-address,dst-address")
        if (r.size > 2000) return -1
        val ids = r.filter {
            (it["src-address"] ?: "").startsWith("$phoneIp:") && !(it["dst-address"] ?: "").startsWith("$host:")
        }.mapNotNull { it[".id"] }
        for (chunk in ids.chunked(50)) {
            try {
                api.run("/ip/firewall/connection/remove", "=.id=" + chunk.joinToString(","))
            } catch (e: RouterOsException) {
                // "no such item": la conexión ya expiró.
                if (e.kind != "trap") throw e
            }
        }
        return ids.size
    }

    fun identity(api: RouterOsApi): String? = api.run("/system/identity/print").firstOrNull()?.get("name")

    fun version(api: RouterOsApi): String? =
        api.run("/system/resource/print", "=.proplist=version").firstOrNull()?.get("version")?.substringBefore(' ')
}
