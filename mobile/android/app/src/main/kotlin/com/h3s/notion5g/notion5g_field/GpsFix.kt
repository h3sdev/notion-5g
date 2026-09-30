package com.h3s.notion5g.notion5g_field

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import android.location.GnssStatus
import android.location.Location
import android.location.LocationManager
import android.os.Build
import android.os.CancellationSignal
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/// Fix fresco por prueba (contrato §2.4): se pide al empezar (paso a) y se
/// espera en el paso e. La edad se mide con el reloj monótono
/// (elapsedRealtimeNanos), nunca restando horas de pared.
class GpsFix(private val context: Context) {
    class Result(
        val location: Location?,
        val satellitesUsed: Int?,
        /// "gps-off", "no-permission" o null.
        val problem: String?,
    )

    /// Pedido en curso: corre en su propio hilo; await() espera con tope.
    class Pending(private val latch: CountDownLatch, private val holder: Array<Result?>, private val cancel: () -> Unit) {
        fun await(timeoutMs: Long): Result {
            try { latch.await(timeoutMs, TimeUnit.MILLISECONDS) } catch (_: InterruptedException) {}
            return holder[0] ?: Result(null, null, null).also { cancel() }
        }
        val done: Boolean get() = latch.count == 0L
        fun peek(): Result? = holder[0]
    }

    private val lm = context.getSystemService(Context.LOCATION_SERVICE) as LocationManager

    fun hasPermission(): Boolean =
        context.checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) == PackageManager.PERMISSION_GRANTED

    fun gpsEnabled(): Boolean = try { lm.isProviderEnabled(LocationManager.GPS_PROVIDER) } catch (_: Exception) { false }

    fun start(timeoutS: Int): Pending {
        val latch = CountDownLatch(1)
        val holder = arrayOfNulls<Result>(1)
        val cancelSignal = CancellationSignal()
        if (!hasPermission()) {
            holder[0] = Result(null, null, "no-permission"); latch.countDown()
            return Pending(latch, holder) {}
        }
        if (!gpsEnabled()) {
            holder[0] = Result(null, null, "gps-off"); latch.countDown()
            return Pending(latch, holder) {}
        }
        val exec = Executors.newSingleThreadExecutor()
        exec.execute {
            val sats = AtomicInteger(-1)
            val gnssCb = object : GnssStatus.Callback() {
                override fun onSatelliteStatusChanged(status: GnssStatus) {
                    var used = 0
                    for (i in 0 until status.satelliteCount) if (status.usedInFix(i)) used++
                    sats.set(used)
                }
            }
            val mainHandler = Handler(Looper.getMainLooper())
            try {
                registerGnss(gnssCb, mainHandler)
                var loc = request(LocationManager.GPS_PROVIDER, timeoutS * 1000L, cancelSignal)
                if (loc == null && !cancelSignal.isCanceled) {
                    val fused = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S && lm.allProviders.contains(LocationManager.FUSED_PROVIDER))
                        LocationManager.FUSED_PROVIDER else null
                    val alt = fused ?: if (lm.allProviders.contains(LocationManager.NETWORK_PROVIDER) &&
                        lm.isProviderEnabled(LocationManager.NETWORK_PROVIDER)) LocationManager.NETWORK_PROVIDER else null
                    if (alt != null) loc = request(alt, 5000L, cancelSignal)
                }
                val s = sats.get()
                holder[0] = Result(loc, if (s >= 0 && loc?.provider == LocationManager.GPS_PROVIDER) s else null, null)
            } catch (e: Exception) {
                holder[0] = Result(null, null, null)
            } finally {
                try { lm.unregisterGnssStatusCallback(gnssCb) } catch (_: Exception) {}
                latch.countDown()
                exec.shutdown()
            }
        }
        return Pending(latch, holder) { cancelSignal.cancel() }
    }

    @SuppressLint("MissingPermission")
    private fun registerGnss(cb: GnssStatus.Callback, h: Handler) {
        try { lm.registerGnssStatusCallback(cb, h) } catch (_: Exception) {}
    }

    /// Un fix nuevo del proveedor, con tope. API 30+: getCurrentLocation; antes,
    /// la última conocida (la edad se valida después igual).
    @SuppressLint("MissingPermission")
    private fun request(provider: String, timeoutMs: Long, outerCancel: CancellationSignal): Location? {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) {
            return try { lm.getLastKnownLocation(provider) } catch (_: Exception) { null }
        }
        val latch = CountDownLatch(1)
        val out = arrayOfNulls<Location>(1)
        val cs = CancellationSignal()
        outerCancel.setOnCancelListener { cs.cancel(); latch.countDown() }
        val exec = Executors.newSingleThreadExecutor()
        try {
            lm.getCurrentLocation(provider, cs, exec) { l ->
                out[0] = l
                latch.countDown()
            }
            if (!latch.await(timeoutMs, TimeUnit.MILLISECONDS)) cs.cancel()
        } catch (_: Exception) {
            cs.cancel()
        } finally {
            outerCancel.setOnCancelListener(null)
            exec.shutdown()
        }
        return out[0]
    }

    companion object {
        /// Edad del fix (s) en el instante `atElapsedNs` (reloj monótono).
        fun ageSeconds(loc: Location, atElapsedNs: Long = SystemClock.elapsedRealtimeNanos()): Double =
            maxOf(0.0, (atElapsedNs - loc.elapsedRealtimeNanos) / 1e9)
    }
}
