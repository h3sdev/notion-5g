package com.h3s.notion5g.notion5g_field

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.app.job.JobInfo
import android.app.job.JobParameters
import android.app.job.JobScheduler
import android.app.job.JobService
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.net.wifi.WifiManager
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import android.os.IBinder
import android.os.Looper
import android.os.PowerManager
import android.os.SystemClock
import android.util.Log
import org.json.JSONObject

/// Sonda A/B con el celular por cable (forma A, contrato §2): servicio nativo
/// en primer plano, separado de BeaconService (que sigue igual). Mide la
/// velocidad saliendo por el router A o B según la regla del MikroTik que el
/// mismo celular cambia antes de cada prueba, guarda todo en probe.db y lo sube
/// al backend de la sonda cuando puede.
///
/// Dos hilos: probe-worker (ProbeWorker: mide y habla con el MikroTik) y
/// probe-control (ProbeControl: todo el HTTP al backend). Sobrevive como
/// BeaconService: START_STICKY, BootReceiver y ProbeWatchdog (JobScheduler,
/// cada 15 min, id 43).
class ProbeService : Service() {
    companion object {
        private const val TAG = "ProbeService"
        private const val CHANNEL_ID = "probe"
        private const val NOTIF_ID = 2
        private const val WATCHDOG_JOB_ID = 43

        @Volatile var instance: ProbeService? = null
            private set

        val isRunning: Boolean get() = instance?.started == true

        fun startIfEnabled(context: Context, why: String) {
            val prefs = ProbePrefs(context)
            if (!prefs.enabled || isRunning) return
            try {
                context.startForegroundService(Intent(context, ProbeService::class.java))
                Log.i(TAG, "arrancado por $why")
            } catch (e: Exception) {
                val msg = "No se pudo relanzar la sonda ($why): ${e.javaClass.simpleName}"
                prefs.recordRestartFailed(msg)
                ProbeDb.get(context).event("error", "stopped", null, msg)
                Log.w(TAG, "no se pudo relanzar ($why)", e)
            }
        }

        fun scheduleWatchdog(context: Context) {
            context.getSystemService(JobScheduler::class.java).schedule(
                JobInfo.Builder(WATCHDOG_JOB_ID, ComponentName(context, ProbeWatchdog::class.java))
                    .setPeriodic(15 * 60 * 1000L)
                    .setPersisted(true)
                    .build()
            )
        }

        fun cancelWatchdog(context: Context) {
            context.getSystemService(JobScheduler::class.java).cancel(WATCHDOG_JOB_ID)
        }
    }

    @Volatile private var started = false
    @Volatile private var stopping = false
    private lateinit var prefs: ProbePrefs
    private lateinit var db: ProbeDb
    private var net: NetPaths? = null
    private var cp: ControlPlane? = null
    var worker: ProbeWorker? = null
        private set
    var control: ProbeControl? = null
        private set
    private var workerThread: HandlerThread? = null
    var controlHandler: Handler? = null
        private set
    private var controlThread: HandlerThread? = null
    private var wakeLock: PowerManager.WakeLock? = null
    private var wifiLock: WifiManager.WifiLock? = null
    private val main = Handler(Looper.getMainLooper())
    private var lastNotifEl = 0L
    private var notifPending = false
    private val publishLock = Any()

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        prefs = ProbePrefs(this)
        db = ProbeDb.get(this)
        instance = this
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // startForeground primero, también si luego se aborta (Android lo exige
        // tras startForegroundService).
        try {
            val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) ServiceInfo.FOREGROUND_SERVICE_TYPE_LOCATION else 0
            startForeground(NOTIF_ID, buildNotification("Sonda: iniciando…"), type)
        } catch (e: Exception) {
            val msg = if (checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) != PackageManager.PERMISSION_GRANTED)
                "Falta permiso de ubicación: Android no deja arrancar la sonda" else "Android no dejó arrancar la sonda: ${e.javaClass.simpleName}"
            prefs.recordRestartFailed(msg)
            Thread { db.event("error", "stopped", null, msg) }.start()
            Log.w(TAG, "startForeground falló", e)
            stopSelf()
            return START_NOT_STICKY
        }
        if (!prefs.enabled || stopping) {
            if (!started) {
                stopForeground(STOP_FOREGROUND_REMOVE)
                stopSelf()
            }
            return START_NOT_STICKY
        }
        if (!started) startAll()
        return START_STICKY
    }

    private fun startAll() {
        started = true
        prefs.clearRestartFailed()
        val st = ProbeState
        st.alerts.remove("restart-failed")
        st.serviceStartedMs = System.currentTimeMillis()
        st.seq.set(0)
        st.setPhase("idle")
        st.running = true

        wakeLock = (getSystemService(Context.POWER_SERVICE) as PowerManager)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "notion5g:probe").apply { acquire() }
        try {
            val wm = applicationContext.getSystemService(Context.WIFI_SERVICE) as WifiManager
            @Suppress("DEPRECATION")
            val mode = if (Build.VERSION.SDK_INT >= 34) WifiManager.WIFI_MODE_FULL_LOW_LATENCY else WifiManager.WIFI_MODE_FULL_HIGH_PERF
            wifiLock = wm.createWifiLock(mode, "notion5g:probe").apply { setReferenceCounted(false); acquire() }
        } catch (e: Exception) {
            Log.w(TAG, "WifiLock", e)
        }

        val ua = "notion5g-probe/${ProbeUtil.appVersion(this)}"
        val wt = HandlerThread("probe-worker").also { it.start() }
        val ct = HandlerThread("probe-control").also { it.start() }
        workerThread = wt
        controlThread = ct
        val wh = Handler(wt.looper)
        val ch = Handler(ct.looper)
        controlHandler = ch

        val n = NetPaths(this) { ch.post { publish(true) }; worker?.wake() }
        net = n
        st.net = n
        val plane = ControlPlane({ n.eth?.network }, { n.wifi?.network }, { NetPaths.defaultNetworkType(this) }, ua)
        cp = plane
        st.control = plane
        val c = ProbeControl(this, db, prefs, plane, ch, onOrders = { worker?.wake() }, buildStatus = { buildAndPublish() })
        control = c
        worker = ProbeWorker(this, db, prefs, n, plane, wh, kickControl = { c.kick() }, publish = { changed -> publish(changed) })
        n.start()
        Thread { db.event("info", "idle", null, "Sonda iniciada (${prefs.snapshot().probeId}, ${prefs.phoneId})") }.start()
        worker?.start()
        c.start()
    }

    /// Arma el estado, lo publica al EventChannel y actualiza la notificación.
    fun buildAndPublish(): JSONObject? = try {
        synchronized(publishLock) {
            val s = ProbeStatus.build(this)
            ProbeBus.publish(s)
            updateNotification()
            s
        }
    } catch (e: Exception) {
        Log.w(TAG, "estado", e)
        null
    }

    /// Lo llama el worker en cada fase/avance; en cambio de fase también va al backend.
    fun publish(phaseChanged: Boolean) {
        buildAndPublish()
        if (phaseChanged) control?.statusSoon()
    }

    private fun notifText(): String {
        val st = ProbeState
        val o = st.currentOrder
        val who = o?.let { it.optStrN("slot") ?: it.optStrN("target") }
        val base = ProbeStatus.phaseName(st.phase).lowercase()
        return "Sonda: " + (if (who != null && st.phase !in setOf("idle", "backoff")) "$base $who" else base) +
            (st.phaseDetail?.let { " · $it" } ?: "")
    }

    private fun updateNotification() {
        val now = SystemClock.elapsedRealtime()
        val wait = 2000 - (now - lastNotifEl)
        if (wait > 0) {
            if (!notifPending) {
                notifPending = true
                main.postDelayed({ notifPending = false; lastNotifEl = SystemClock.elapsedRealtime(); notifyNow() }, wait)
            }
            return
        }
        lastNotifEl = now
        notifyNow()
    }

    private fun notifyNow() {
        if (!started || stopping) return
        try {
            getSystemService(NotificationManager::class.java).notify(NOTIF_ID, buildNotification(notifText()))
        } catch (_: Exception) {
        }
    }

    private fun buildNotification(text: String): Notification {
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL_ID) == null) {
            nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "Sonda A/B", NotificationManager.IMPORTANCE_LOW))
        }
        val open = PendingIntent.getActivity(
            this, 2, Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_notify_sync)
            .setContentTitle("Notion 5G: sonda A/B")
            .setContentText(text)
            .setContentIntent(open)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .build()
    }

    /// stop (§2.10): aborta la prueba en curso (la orden queda interrupted),
    /// restaura la regla, vacía outbox (5 s como mucho) y se detiene.
    fun requestStop() {
        if (stopping) return
        stopping = true
        val w = worker
        val c = control
        val ch = controlHandler
        if (w == null || c == null || ch == null) {
            stopSelf()
            return
        }
        w.abortReason = "detenida"
        w.stopping = true
        // Red de seguridad: si algo se cuelga, se detiene igual.
        main.postDelayed({ finishStop() }, 30_000)
        w.handler.post {
            try {
                w.restoreRoute(prefs.snapshot(), strict = false)
            } catch (_: Exception) {
            }
            ch.post {
                c.flushOnStop()
                main.post { finishStop() }
            }
        }
    }

    private fun finishStop() {
        if (!started) return
        db.let { Thread { it.event("info", "stopped", null, "Sonda detenida") }.start() }
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        val wasStarted = started
        started = false
        ProbeState.running = false
        ProbeState.setPhase("stopped")
        ProbeState.currentOrder = null
        ProbeState.busy = false
        worker?.let { it.stopping = true; it.abortReason = "detenida" }
        control?.stop()
        net?.stop()
        ProbeState.net = null
        ProbeState.control = null
        workerThread?.quitSafely()
        controlThread?.quitSafely()
        wakeLock?.takeIf { it.isHeld }?.release()
        try { wifiLock?.takeIf { it.isHeld }?.release() } catch (_: Exception) {}
        if (instance === this) instance = null
        main.removeCallbacksAndMessages(null)
        if (wasStarted) {
            val ctx = applicationContext
            Thread {
                try { ProbeBus.publish(ProbeStatus.build(ctx)) } catch (_: Exception) {}
            }.start()
            // Se volvió a activar mientras se detenía: arrancar de nuevo.
            if (stopping && prefs.enabled) main.post { startIfEnabled(ctx, "reactivada") }
        }
        super.onDestroy()
    }
}

/// Red de seguridad cada 15 min (como BeaconWatchdog): si Android mató el
/// servicio y START_STICKY no lo revivió, lo vuelve a arrancar.
class ProbeWatchdog : JobService() {
    override fun onStartJob(params: JobParameters?): Boolean {
        if (!ProbePrefs(this).enabled) {
            ProbeService.cancelWatchdog(this)
        } else {
            ProbeService.startIfEnabled(this, "watchdog")
        }
        return false
    }

    override fun onStopJob(params: JobParameters?): Boolean = false
}
