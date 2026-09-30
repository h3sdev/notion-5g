package com.h3s.notion5g.notion5g_field

import android.net.Network
import android.os.SystemClock
import com.jcraft.jsch.ChannelExec
import com.jcraft.jsch.JSch
import com.jcraft.jsch.JSchException
import com.jcraft.jsch.Session
import com.jcraft.jsch.SocketFactory
import com.jcraft.jsch.UIKeyboardInteractive
import com.jcraft.jsch.UserInfo
import java.io.InputStream
import java.io.OutputStream
import java.net.InetSocketAddress
import java.net.Socket

/// Reinicio de un router por SSH (contrato §6.2), desde el celular y atado a
/// Ethernet: el socket sale por el cable al MikroTik, que lo lleva por la LAN
/// del router (regla `probe:mgmt-<slot>`), nunca por el WiFi (cuya subred puede
/// coincidir con la del router B) ni por datos celulares.
///
/// El dropbear del Notion es viejo: exige `ssh-rsa` y kex
/// `diffie-hellman-group1-sha1`/`group14-sha1`, que JSch (fork mwiede) trae
/// apagados; se agregan al final de las listas, así un servidor moderno sigue
/// negociando lo mejor. La contraseña nunca se registra ni sale del teléfono.
object RouterSsh {
    class Outcome(
        /// Autenticó y mandó el comando.
        val ok: Boolean,
        /// `ssh-auth` | `ssh-connect` | null.
        val error: String?,
        /// Detalle legible del error (sin secretos).
        val detail: String?,
        /// Huella de la llave del equipo (se acepta sin verificar, pero se guarda).
        val hostKeyFp: String?,
        val hostKeyType: String?,
        val authenticated: Boolean,
        /// Código de salida del comando si llegó antes de que se cortara la sesión.
        val exitStatus: Int?,
    )

    private const val CONNECT_MS = 8000
    private const val WAIT_MS = 10_000L

    /// Socket atado a la red dada, con tope de conexión (JSch no aplica el
    /// timeout de connect cuando hay socketFactory).
    private class BoundFactory(private val network: Network) : SocketFactory {
        override fun createSocket(host: String, port: Int): Socket {
            val s = network.socketFactory.createSocket()
            try {
                s.connect(InetSocketAddress(network.getByName(host), port), CONNECT_MS)
                s.tcpNoDelay = true
            } catch (e: Exception) {
                try { s.close() } catch (_: Exception) {}
                throw e
            }
            return s
        }

        override fun getInputStream(socket: Socket): InputStream = socket.getInputStream()
        override fun getOutputStream(socket: Socket): OutputStream = socket.getOutputStream()
    }

    /// Responde con la contraseña a "password" y a "keyboard-interactive".
    private class Creds(private val password: String) : UserInfo, UIKeyboardInteractive {
        override fun getPassphrase(): String? = null
        override fun getPassword(): String = password
        override fun promptPassword(message: String?): Boolean = true
        override fun promptPassphrase(message: String?): Boolean = false
        override fun promptYesNo(message: String?): Boolean = true
        override fun showMessage(message: String?) {}
        override fun promptKeyboardInteractive(
            destination: String?, name: String?, instruction: String?, prompt: Array<out String>?, echo: BooleanArray?,
        ): Array<String> = Array(prompt?.size ?: 0) { password }
    }

    private fun withLegacy(key: String, vararg extra: String): String {
        val list = (JSch.getConfig(key) ?: "").split(',').map { it.trim() }.filter { it.isNotEmpty() }.toMutableList()
        for (e in extra) if (e !in list) list.add(e)
        return list.joinToString(",")
    }

    private fun configure(session: Session) {
        session.setConfig("StrictHostKeyChecking", "no")
        session.setConfig("PreferredAuthentications", "password,keyboard-interactive")
        session.setConfig("server_host_key", withLegacy("server_host_key", "ssh-rsa", "ssh-dss"))
        session.setConfig("PubkeyAcceptedAlgorithms", withLegacy("PubkeyAcceptedAlgorithms", "ssh-rsa"))
        session.setConfig("kex", withLegacy("kex", "diffie-hellman-group14-sha1", "diffie-hellman-group1-sha1"))
        for (dir in listOf("c2s", "s2c")) {
            session.setConfig("cipher.$dir", withLegacy("cipher.$dir", "aes128-cbc", "aes192-cbc", "aes256-cbc", "3des-cbc"))
            session.setConfig("mac.$dir", withLegacy("mac.$dir", "hmac-sha1", "hmac-md5"))
        }
    }

    private fun isAuthError(e: JSchException): Boolean {
        val m = e.message ?: return false
        return m.contains("Auth fail", ignoreCase = true) || m.contains("Auth cancel", ignoreCase = true) ||
            m.contains("USERAUTH fail", ignoreCase = true) || m.contains("Too many authentication", ignoreCase = true)
    }

    private fun describe(e: Throwable): String {
        val cause = e.cause
        val base = "${e.javaClass.simpleName}${e.message?.let { ": " + it.take(160) } ?: ""}"
        return if (cause != null && cause !== e) "$base (${cause.javaClass.simpleName}${cause.message?.let { ": " + it.take(80) } ?: ""})" else base
    }

    /// Conecta, ejecuta `command`, espera ≤10 s a que termine (un `reboot` suele
    /// cortar la sesión antes) y cierra. No se aborta a mitad: una vez enviado el
    /// comando, el resultado (ssh_ok) tiene que llegar al backend.
    fun run(network: Network, host: String, ssh: ProbeSettings.Ssh): Outcome {
        val jsch = JSch()
        var session: Session? = null
        var authenticated = false
        var fp: String? = null
        var keyType: String? = null
        fun hostKey(s: Session?) {
            try {
                val hk = s?.hostKey ?: return
                fp = hk.getFingerPrint(jsch)
                keyType = hk.type
            } catch (_: Exception) {
            }
        }
        try {
            val s = jsch.getSession(ssh.user, host, ssh.port)
            session = s
            configure(s)
            s.setPassword(ssh.password)
            s.userInfo = Creds(ssh.password)
            s.setSocketFactory(BoundFactory(network))
            s.timeout = CONNECT_MS
            try {
                s.connect(CONNECT_MS)
            } catch (e: JSchException) {
                hostKey(s)
                return Outcome(false, if (isAuthError(e)) "ssh-auth" else "ssh-connect", describe(e), fp, keyType, false, null)
            }
            authenticated = true
            hostKey(s)
            val ch = s.openChannel("exec") as ChannelExec
            ch.setCommand(ssh.command)
            ch.setInputStream(null)
            val out = ch.inputStream
            try {
                ch.connect(CONNECT_MS)
            } catch (e: JSchException) {
                return Outcome(false, "ssh-connect", "no se pudo ejecutar el comando: ${describe(e)}", fp, keyType, true, null)
            }
            // Esperar el fin del comando o el corte de la sesión (el equipo se apaga).
            val end = SystemClock.elapsedRealtime() + WAIT_MS
            val buf = ByteArray(1024)
            while (SystemClock.elapsedRealtime() < end && !ch.isClosed && s.isConnected) {
                try {
                    while (out.available() > 0 && out.read(buf) >= 0) { /* se descarta */ }
                } catch (_: Exception) {
                    break
                }
                Thread.sleep(200)
            }
            val exit = if (ch.isClosed) ch.exitStatus.takeIf { it >= 0 } else null
            try { ch.disconnect() } catch (_: Exception) {}
            return Outcome(true, null, null, fp, keyType, true, exit)
        } catch (e: Exception) {
            hostKey(session)
            return Outcome(false, "ssh-connect", describe(e), fp, keyType, authenticated, null)
        } finally {
            try { session?.disconnect() } catch (_: Exception) {}
        }
    }
}
