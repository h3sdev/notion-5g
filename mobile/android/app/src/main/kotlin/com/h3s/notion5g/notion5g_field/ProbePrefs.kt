package com.h3s.notion5g.notion5g_field

import android.annotation.SuppressLint
import android.content.Context
import android.content.SharedPreferences
import android.provider.Settings
import java.net.URI
import java.util.Locale

/// Ajustes de la sonda A/B (contrato §2.7): SharedPreferences "probe", cuyo
/// dueño es el lado nativo. Dart los lee con getConfig y los cambia con
/// saveConfig. Los secretos (api_key, prod_api_key, mikrotik_password y
/// ssh_password_<slot>) solo salen por getConfig: nunca van al estado en vivo,
/// a eventos ni a resultados.
class ProbePrefs(context: Context) {
    companion object {
        const val NAME = "probe"
        val DEFAULTS: LinkedHashMap<String, Any> = linkedMapOf(
            "enabled" to false,
            "backend_url" to "http://192.168.40.22:8080",
            "api_key" to "",
            "probe_id" to "hap-oficina",
            "phone_id" to "",
            "mikrotik_host" to "192.168.89.1",
            "mikrotik_port" to 8728,
            "mikrotik_user" to "phone-probe",
            "mikrotik_password" to "",
            "mikrotik_rule_comment" to "phone-probe",
            "fallback_table" to "main",
            "table_A" to "to-A",
            "table_B" to "to-B",
            "require_ethernet" to true,
            "also_cloudflare" to true,
            "speed_target" to "fast",
            "prod_url" to "https://notion.h3s-iot.com",
            "prod_api_key" to "",
            "duration_s" to 10,
            "max_mb_per_phase" to 200,
            "gateway_check" to "",
            "flush_conntrack" to true,
            "mikrotik_idle_check_s" to 60,
            "poll_interval_s" to 30,
            "status_interval_s" to 15,
            "local_plan_enabled" to true,
            "local_plan_interval_s" to 900,
            "offline_after_s" to 600,
            "location_fix_timeout_s" to 20,
            "location_max_age_s" to 30,
            "location_max_age_stationary_s" to 300,
            "location_max_accuracy_m" to 100,
            // SSH a cada router para reiniciarlo (contrato §6.2). La clave es un
            // secreto: solo sale por getConfig.
            "ssh_user_A" to "root",
            "ssh_password_A" to "",
            "ssh_port_A" to 22,
            "ssh_command_A" to "reboot",
            "ssh_user_B" to "root",
            "ssh_password_B" to "",
            "ssh_port_B" to 22,
            "ssh_command_B" to "reboot",
        )
        val SSH_SLOTS = listOf("A", "B")
        val SPEED_TARGETS = setOf("cloudflare", "fast", "prod-download", "local")

        /// Rangos que evitan que la regla quede forzada más del tope de 8 min
        /// (duración y espera del GPS van con la ruta forzada) o que se inunde
        /// el MikroTik o el backend con sesiones.
        val RANGES: Map<String, Triple<Long, Long, String>> = mapOf(
            "duration_s" to Triple(3L, 60L, "Duración de cada fase (s)"),
            "location_fix_timeout_s" to Triple(5L, 120L, "Espera del fijo de GPS (s)"),
            "mikrotik_idle_check_s" to Triple(10L, 3600L, "Lectura de la regla en reposo (s)"),
            "poll_interval_s" to Triple(5L, 3600L, "Consulta de órdenes (s)"),
            "status_interval_s" to Triple(5L, 3600L, "Envío del estado (s)"),
            "local_plan_interval_s" to Triple(60L, 86400L, "Intervalo del plan local (s)"),
            "offline_after_s" to Triple(30L, 86400L, "Sin backend tras (s)"),
            "max_mb_per_phase" to Triple(1L, 2000L, "Tope por fase (MB)"),
        )

        // Estado de fallos al relanzar (no es un ajuste: no sale en getConfig).
        private const val K_RESTART_ERR = "_restart_failed_msg"
        private const val K_RESTART_ERR_MS = "_restart_failed_ms"
    }

    private val appContext = context.applicationContext
    private val p: SharedPreferences = appContext.getSharedPreferences(NAME, Context.MODE_PRIVATE)

    fun str(k: String): String = p.getString(k, DEFAULTS[k] as? String ?: "") ?: ""
    fun int(k: String): Int = try { p.getInt(k, DEFAULTS[k] as? Int ?: 0) } catch (_: ClassCastException) { DEFAULTS[k] as? Int ?: 0 }
    fun bool(k: String): Boolean = p.getBoolean(k, DEFAULTS[k] as? Boolean ?: false)

    var enabled: Boolean
        get() = bool("enabled")
        set(v) { p.edit().putBoolean("enabled", v).apply() }

    /// "phone-" + primeros 8 caracteres de ANDROID_ID, salvo que se fije a mano.
    val phoneId: String
        get() = str("phone_id").trim().ifEmpty { "phone-" + androidId().take(8).lowercase(Locale.US) }

    @SuppressLint("HardwareIds")
    private fun androidId(): String =
        (Settings.Secure.getString(appContext.contentResolver, Settings.Secure.ANDROID_ID) ?: "unknown").ifEmpty { "unknown" }

    /// Todas las claves con sus valores por defecto resueltos (getConfig).
    fun all(): Map<String, Any?> {
        val m = LinkedHashMap<String, Any?>()
        for ((k, d) in DEFAULTS) {
            m[k] = when (d) {
                is Boolean -> bool(k)
                is Int -> int(k)
                else -> str(k)
            }
        }
        m["phone_id"] = phoneId
        return m
    }

    fun snapshot(): ProbeSettings = ProbeSettings(all())

    /// Guarda un mapa parcial. Devuelve null si está bien o el mensaje de error
    /// (en ese caso no guarda nada). "enabled" se ignora: lo cambian start/stop.
    fun save(m: Map<String, Any?>): String? {
        val e = p.edit()
        for ((k, v) in m) {
            if (k == "enabled") continue
            val d = DEFAULTS[k] ?: return "Clave desconocida: $k"
            when (d) {
                is Boolean -> {
                    val b = v as? Boolean ?: return "$k debe ser sí/no"
                    e.putBoolean(k, b)
                }
                is Int -> {
                    val n = when (v) {
                        is Number -> v.toLong()
                        is String -> v.trim().toLongOrNull()
                        else -> null
                    } ?: return "$k debe ser un número entero"
                    if (n <= 0) return "$k debe ser mayor que 0"
                    if (k == "mikrotik_port" && n > 65535) return "Puerto del MikroTik fuera de rango (1-65535)"
                    if (k.startsWith("ssh_port_") && n > 65535) return "Puerto SSH de ${k.removePrefix("ssh_port_")} fuera de rango (1-65535)"
                    if (n > Int.MAX_VALUE) return "$k es demasiado grande"
                    RANGES[k]?.let { (lo, hi, what) -> if (n < lo || n > hi) return "$what: entre $lo y $hi" }
                    e.putInt(k, n.toInt())
                }
                else -> {
                    val raw = v as? String ?: v?.toString() ?: ""
                    val s = if (k.startsWith("ssh_password_")) raw else raw.trim()
                    when (k) {
                        "backend_url" -> if (!validUrl(s)) return "URL mal formada en $k: debe empezar por http:// o https://"
                        // Lleva la API key de producción: solo https.
                        "prod_url" -> if (!validUrl(s) || !s.startsWith("https://", ignoreCase = true)) return "La URL de producción debe empezar por https://"
                        "speed_target" -> if (s !in SPEED_TARGETS) return "Destino de velocidad inválido: $s"
                        "gateway_check" -> if (s.isNotEmpty() && !validHostPort(s)) return "Chequeo de puerta de enlace: debe ser host:puerto"
                        "probe_id" -> if (s.isEmpty()) return "El id de la sonda no puede quedar vacío"
                        "mikrotik_host" -> if (s.isEmpty()) return "Falta la dirección del MikroTik"
                        "fallback_table", "table_A", "table_B", "mikrotik_rule_comment", "mikrotik_user" ->
                            if (s.isEmpty()) return "$k no puede quedar vacío"
                        "ssh_user_A", "ssh_user_B" -> if (s.isEmpty()) return "Falta el usuario SSH de ${k.last()}"
                        "ssh_command_A", "ssh_command_B" -> if (s.isEmpty()) return "Falta el comando de reinicio de ${k.last()}"
                    }
                    e.putString(k, if (k == "backend_url" || k == "prod_url") s.trimEnd('/') else s)
                }
            }
        }
        e.apply()
        return null
    }

    private fun validUrl(s: String): Boolean = try {
        val u = URI(s)
        (u.scheme == "http" || u.scheme == "https") && !u.host.isNullOrEmpty()
    } catch (_: Exception) {
        false
    }

    private fun validHostPort(s: String): Boolean {
        val i = s.lastIndexOf(':')
        if (i <= 0) return false
        val port = s.substring(i + 1).toIntOrNull() ?: return false
        return port in 1..65535
    }

    fun recordRestartFailed(msg: String) {
        p.edit().putString(K_RESTART_ERR, msg).putLong(K_RESTART_ERR_MS, System.currentTimeMillis()).apply()
    }

    fun clearRestartFailed() {
        if (p.contains(K_RESTART_ERR)) p.edit().remove(K_RESTART_ERR).remove(K_RESTART_ERR_MS).apply()
    }

    val restartFailed: Pair<String, Long>?
        get() {
            val m = p.getString(K_RESTART_ERR, null) ?: return null
            return m to p.getLong(K_RESTART_ERR_MS, System.currentTimeMillis())
        }
}

/// Copia inmutable de los ajustes: el worker la toma al inicio de cada ciclo
/// para que un cambio no caiga a mitad de una prueba.
class ProbeSettings(private val m: Map<String, Any?>) {
    fun str(k: String): String = m[k] as? String ?: ""
    fun int(k: String): Int = (m[k] as? Number)?.toInt() ?: (ProbePrefs.DEFAULTS[k] as? Int ?: 0)
    fun bool(k: String): Boolean = m[k] as? Boolean ?: false

    val backendUrl get() = str("backend_url").trimEnd('/')
    val apiKey get() = str("api_key")
    val probeId get() = str("probe_id")
    val phoneId get() = str("phone_id")
    val requireEthernet get() = bool("require_ethernet")
    val fallbackTable get() = str("fallback_table").ifEmpty { "main" }
    val ruleComment get() = str("mikrotik_rule_comment").ifEmpty { "phone-probe" }

    /// Credenciales SSH del router de un slot (§6.2); slots sin ajustes propios
    /// usan los valores por defecto (root, clave de fábrica, 22, reboot).
    class Ssh(val user: String, val password: String, val port: Int, val command: String)

    fun ssh(slot: String): Ssh {
        val has = slot in ProbePrefs.SSH_SLOTS
        fun s(k: String, d: String) = if (has) str("ssh_${k}_$slot").ifEmpty { d } else d
        val port = if (has) int("ssh_port_$slot") else 22
        return Ssh(s("user", "root"), if (has) str("ssh_password_$slot") else "", if (port in 1..65535) port else 22, s("command", "reboot"))
    }
}
