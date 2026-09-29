package com.h3s.notion5g.notion5g_field

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.app.job.JobInfo
import android.app.job.JobScheduler
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.location.Location
import android.location.LocationListener
import android.location.LocationManager
import android.os.Build
import android.os.HandlerThread
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean

/// Modo "en movimiento": manda la ubicación al backend
/// (POST /api/v1/devices/{id}/location) para que el perfil de ping del router
/// tenga con qué correlacionar posición -- el módem no tiene GPS propio.
///
/// Es un servicio nativo, no Dart: antes lo hacía geolocator dentro del proceso
/// de Flutter y cuando Android mataba ese proceso (pasó el 2026-09-25 con
/// SIGKILL, el teléfono al 100 % en el cargador) nada lo revivía y el celular
/// quedaba mudo hasta que alguien abriera la app. Ahora:
///  - el interruptor queda guardado ([Prefs.enabled]) junto con la config;
///  - START_STICKY: si Android mata el proceso, vuelve a crear el servicio;
///  - [BootReceiver] lo arranca al prender el teléfono o al actualizar la app;
///  - [BeaconWatchdog] (JobScheduler, cada 15 min) lo relanza si no corre.
/// Arrancar un servicio en primer plano desde segundo plano solo se permite con
/// la app exenta de optimización de batería, y recibir ubicación ahí requiere el
/// permiso "Permitir todo el tiempo" (ACCESS_BACKGROUND_LOCATION).
///
/// No se manda cada punto: se toma uno cada [FIX_INTERVAL_MS] y se envía solo si
/// hubo desplazamiento real (más de [MIN_MOVE_M] y más que la precisión del
/// punto, para que el ruido del GPS estando quieto no cuente) o si pasó
/// [MAX_SILENCE_MS] desde el último envío, muy por dentro de los 10 min que el
/// backend acepta una ubicación (maxLocationAge).
class BeaconService : Service(), LocationListener {
    companion object {
        private const val TAG = "BeaconService"
        const val ACTION_STOP = "com.h3s.notion5g.beacon.STOP"
        private const val CHANNEL_ID = "beacon"
        private const val NOTIF_ID = 1
        private const val FIX_INTERVAL_MS = 15_000L
        private const val MAX_SILENCE_MS = 120_000L
        private const val MIN_MOVE_M = 50f
        private const val WATCHDOG_JOB_ID = 42
        private const val HTTP_TIMEOUT_MS = 15_000

        @Volatile var running = false
            private set

        /// Arranca el servicio si el modo está activo. Desde segundo plano
        /// Android puede negarlo: se registra el error y no revienta.
        fun startIfEnabled(context: Context, why: String) {
            val prefs = Prefs(context)
            if (!prefs.enabled || running) return
            try {
                context.startForegroundService(Intent(context, BeaconService::class.java))
                Log.i(TAG, "arrancado por $why")
            } catch (e: Exception) {
                prefs.recordError("No se pudo relanzar ($why): ${e.javaClass.simpleName}")
                Log.w(TAG, "no se pudo relanzar ($why)", e)
            }
        }

        fun scheduleWatchdog(context: Context) {
            val js = context.getSystemService(JobScheduler::class.java)
            js.schedule(
                JobInfo.Builder(WATCHDOG_JOB_ID, ComponentName(context, BeaconWatchdog::class.java))
                    .setPeriodic(15 * 60 * 1000L)
                    .setPersisted(true)
                    .build()
            )
        }

        fun cancelWatchdog(context: Context) {
            context.getSystemService(JobScheduler::class.java).cancel(WATCHDOG_JOB_ID)
        }
    }

    /// Estado persistido: la config con la que se manda (la escribe la app al
    /// activar el modo) y el resultado del último intento, para mostrarlo en la UI.
    class Prefs(context: Context) {
        private val p: SharedPreferences = context.getSharedPreferences("beacon", Context.MODE_PRIVATE)
        var enabled: Boolean
            get() = p.getBoolean("enabled", false)
            set(v) = p.edit().putBoolean("enabled", v).apply()
        val backendUrl: String get() = p.getString("backend_url", "") ?: ""
        val apiKey: String get() = p.getString("api_key", "") ?: ""
        val deviceId: String get() = p.getString("device_id", "") ?: ""
        val lastOkMs: Long get() = p.getLong("last_ok_ms", 0)
        val lastError: String? get() = p.getString("last_error", null)
        val lastErrorMs: Long get() = p.getLong("last_error_ms", 0)

        fun saveConfig(backendUrl: String, apiKey: String, deviceId: String) {
            p.edit().putString("backend_url", backendUrl.trim()).putString("api_key", apiKey.trim())
                .putString("device_id", deviceId.trim()).apply()
        }

        fun recordOk() {
            p.edit().putLong("last_ok_ms", System.currentTimeMillis()).remove("last_error").apply()
        }

        fun recordError(msg: String) {
            p.edit().putString("last_error", msg).putLong("last_error_ms", System.currentTimeMillis()).apply()
        }
    }

    private lateinit var prefs: Prefs
    private var thread: HandlerThread? = null
    private var wakeLock: PowerManager.WakeLock? = null
    private val sender = Executors.newSingleThreadExecutor()
    private val sendInFlight = AtomicBoolean(false)
    private var provider = LocationManager.GPS_PROVIDER
    // Solo se tocan desde el hilo del sender.
    private var lastSent: Location? = null
    private var lastSentAtMs = 0L

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        prefs = Prefs(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP || !prefs.enabled) {
            stopSelf()
            return START_NOT_STICKY
        }
        // Android exige llamar startForeground pronto tras startForegroundService,
        // también cuando se va a abortar por falta de permiso.
        try {
            val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) ServiceInfo.FOREGROUND_SERVICE_TYPE_LOCATION else 0
            startForeground(NOTIF_ID, buildNotification("Esperando la primera ubicación..."), type)
        } catch (e: Exception) {
            prefs.recordError("Android no dejó arrancar el servicio: ${e.javaClass.simpleName}")
            Log.w(TAG, "startForeground falló", e)
            stopSelf()
            return START_NOT_STICKY
        }
        if (!running) startTracking()
        return START_STICKY
    }

    private fun startTracking() {
        if (checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) != PackageManager.PERMISSION_GRANTED) {
            prefs.recordError("Sin permiso de ubicación")
            stopSelf()
            return
        }
        running = true
        wakeLock = (getSystemService(Context.POWER_SERVICE) as PowerManager)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "notion5g:beacon").apply { acquire() }
        val lm = getSystemService(Context.LOCATION_SERVICE) as LocationManager
        // "fused" (GPS + red, API 31+) es lo mismo que usaba geolocator vía Play Services.
        provider = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S && lm.hasProvider(LocationManager.FUSED_PROVIDER))
            LocationManager.FUSED_PROVIDER else LocationManager.GPS_PROVIDER
        val t = HandlerThread("beacon-location").also { it.start() }
        thread = t
        try {
            lm.requestLocationUpdates(provider, FIX_INTERVAL_MS, 0f, this, t.looper)
        } catch (e: SecurityException) {
            prefs.recordError("Sin permiso de ubicación")
            stopSelf()
        }
    }

    override fun onLocationChanged(location: Location) {
        if (!sendInFlight.compareAndSet(false, true)) return
        sender.execute {
            try {
                if (isDue(location)) send(location)
            } finally {
                sendInFlight.set(false)
            }
        }
    }

    private fun isDue(loc: Location): Boolean {
        val last = lastSent ?: return true
        if (System.currentTimeMillis() - lastSentAtMs >= MAX_SILENCE_MS) return true
        return last.distanceTo(loc) >= maxOf(MIN_MOVE_M, loc.accuracy)
    }

    private fun send(loc: Location) {
        val base = prefs.backendUrl.trimEnd('/')
        val deviceId = prefs.deviceId
        if (base.isEmpty() || deviceId.isEmpty()) {
            prefs.recordError("Falta configurar backend o router")
            return
        }
        // Batería y red del celular viajan con la ubicación: el backend las
        // guarda como historial (phone_log). Lo de abajo no se puede pisar.
        val body = JSONObject(DeviceInfo.status(this))
            .put("lat", loc.latitude)
            .put("lon", loc.longitude)
            .put("gps_accuracy_m", loc.accuracy.toDouble())
            .put("gps_source", if (provider == LocationManager.GPS_PROVIDER) "android-gps" else "android-fused")
        var conn: HttpURLConnection? = null
        try {
            conn = URL("$base/api/v1/devices/${URLEncoder.encode(deviceId, "UTF-8")}/location")
                .openConnection() as HttpURLConnection
            conn.requestMethod = "POST"
            conn.connectTimeout = HTTP_TIMEOUT_MS
            conn.readTimeout = HTTP_TIMEOUT_MS
            conn.doOutput = true
            conn.setRequestProperty("Content-Type", "application/json")
            if (prefs.apiKey.isNotEmpty()) conn.setRequestProperty("X-API-Key", prefs.apiKey)
            conn.outputStream.use { it.write(body.toString().toByteArray()) }
            val code = conn.responseCode
            if (code !in 200..299) {
                prefs.recordError("El backend respondió $code")
                return
            }
            // Solo cuenta como enviado si llegó: si falló, el siguiente punto reintenta.
            lastSent = loc
            lastSentAtMs = System.currentTimeMillis()
            prefs.recordOk()
            updateNotification("Última ubicación enviada: " +
                SimpleDateFormat("HH:mm:ss", Locale.US).format(Date(lastSentAtMs)))
        } catch (e: Exception) {
            prefs.recordError("Sin conexión con el backend: ${e.javaClass.simpleName}")
        } finally {
            conn?.disconnect()
        }
    }

    private fun buildNotification(text: String): Notification {
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL_ID) == null) {
            nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "Modo en movimiento", NotificationManager.IMPORTANCE_LOW))
        }
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_menu_mylocation)
            .setContentTitle("Notion 5G: modo en movimiento")
            .setContentText(text)
            .setContentIntent(open)
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(text: String) {
        getSystemService(NotificationManager::class.java).notify(NOTIF_ID, buildNotification(text))
    }

    override fun onDestroy() {
        if (running) {
            (getSystemService(Context.LOCATION_SERVICE) as LocationManager).removeUpdates(this)
        }
        running = false
        thread?.quitSafely()
        sender.shutdown()
        wakeLock?.takeIf { it.isHeld }?.release()
        super.onDestroy()
    }
}
