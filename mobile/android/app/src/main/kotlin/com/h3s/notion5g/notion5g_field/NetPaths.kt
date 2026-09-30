package com.h3s.notion5g.notion5g_field

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.util.Log
import org.json.JSONObject
import java.net.Inet4Address

/// Sigue las redes Ethernet (cable al MikroTik) y WiFi (oficina) con
/// requestNetwork explícito (contrato §2.1): así Android no da de baja la que
/// no es la red por defecto -- el WiFi cuando Ethernet está validada, o
/// Ethernet cuando queda sin validar (forzada a un router sin datos). Nada se
/// deja a la red por defecto: cada conexión de la sonda se ata a una de estas.
class NetPaths(context: Context, private val onChange: () -> Unit) {
    class Link(val network: Network, @Volatile var caps: NetworkCapabilities?, @Volatile var lp: LinkProperties?) {
        val validated: Boolean get() = caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true
        val iface: String? get() = lp?.interfaceName
        val ipv4: String?
            get() = lp?.linkAddresses?.firstOrNull { it.address is Inet4Address }?.address?.hostAddress
    }

    companion object {
        private const val TAG = "NetPaths"

        /// Lectura de un solo uso, sin pedir redes (con el servicio detenido).
        fun findNow(context: Context): Pair<Link?, Link?> {
            val cm = context.getSystemService(ConnectivityManager::class.java)
            var eth: Link? = null
            var wifi: Link? = null
            try {
                @Suppress("DEPRECATION")
                for (n in cm.allNetworks) {
                    val c = cm.getNetworkCapabilities(n) ?: continue
                    if (eth == null && c.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) eth = Link(n, c, cm.getLinkProperties(n))
                    if (wifi == null && c.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) wifi = Link(n, c, cm.getLinkProperties(n))
                }
            } catch (e: Exception) {
                Log.w(TAG, "findNow", e)
            }
            return eth to wifi
        }

        fun defaultNetworkType(context: Context): String {
            val cm = context.getSystemService(ConnectivityManager::class.java)
            val c = cm.getNetworkCapabilities(cm.activeNetwork ?: return "none") ?: return "none"
            return when {
                c.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "ethernet"
                c.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "wifi"
                c.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "cellular"
                else -> "none"
            }
        }

        fun linkJson(l: Link?, withIface: Boolean): JSONObject {
            val o = JSONObject().put("up", l != null)
            if (withIface) o.putN("iface", l?.iface)
            o.putN("ip", l?.ipv4)
            o.putN("validated", l?.validated)
            return o
        }
    }

    private val cm = context.getSystemService(ConnectivityManager::class.java)
    @Volatile var eth: Link? = null
        private set
    @Volatile var wifi: Link? = null
        private set
    private var ethCb: ConnectivityManager.NetworkCallback? = null
    private var wifiCb: ConnectivityManager.NetworkCallback? = null

    private var lastSig: String? = null

    /// Avisa solo si cambió algo que importa (arriba/abajo, IP, validada): el
    /// WiFi manda onCapabilitiesChanged con cada cambio de señal.
    @Synchronized private fun changed() {
        val e = eth
        val w = wifi
        val sig = "${e?.network}|${e?.ipv4}|${e?.validated}|${w?.network}|${w?.ipv4}|${w?.validated}"
        if (sig == lastSig) return
        lastSig = sig
        onChange()
    }

    private fun callback(get: () -> Link?, set: (Link?) -> Unit) = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            set(Link(network, cm.getNetworkCapabilities(network), cm.getLinkProperties(network)))
            changed()
        }

        override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
            val l = get()
            if (l != null && l.network == network) l.caps = caps else set(Link(network, caps, cm.getLinkProperties(network)))
            changed()
        }

        override fun onLinkPropertiesChanged(network: Network, lp: LinkProperties) {
            val l = get()
            if (l != null && l.network == network) l.lp = lp else set(Link(network, cm.getNetworkCapabilities(network), lp))
            changed()
        }

        override fun onLost(network: Network) {
            if (get()?.network == network) set(null)
            changed()
        }
    }

    fun start() {
        // Sin VALIDATED y sin timeout: la petición espera a que aparezca la red.
        val ethReq = NetworkRequest.Builder()
            .addTransportType(NetworkCapabilities.TRANSPORT_ETHERNET)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        val wifiReq = NetworkRequest.Builder()
            .addTransportType(NetworkCapabilities.TRANSPORT_WIFI)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        try {
            ethCb = callback({ eth }, { eth = it }).also { cm.requestNetwork(ethReq, it) }
        } catch (e: Exception) {
            Log.w(TAG, "requestNetwork Ethernet", e)
        }
        try {
            wifiCb = callback({ wifi }, { wifi = it }).also { cm.requestNetwork(wifiReq, it) }
        } catch (e: Exception) {
            Log.w(TAG, "requestNetwork WiFi", e)
        }
    }

    fun stop() {
        for (cb in listOf(ethCb, wifiCb)) {
            if (cb != null) try { cm.unregisterNetworkCallback(cb) } catch (_: Exception) {}
        }
        ethCb = null
        wifiCb = null
        eth = null
        wifi = null
    }
}
