package com.h3s.notion5g.notion5g_field

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel

class MainActivity : FlutterActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Sin este permiso (Android 13+) el servicio del modo en movimiento corre
        // igual, pero su notificación no se ve y no hay forma de saber que está activo.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1001)
        }
    }

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        MethodChannel(flutterEngine.dartExecutor.binaryMessenger, "notion5g/device").setMethodCallHandler { call, result ->
            if (call.method == "status") result.success(DeviceInfo.status(this)) else result.notImplemented()
        }
        // Sonda A/B con el celular por cable (ProbeService): ver ProbeChannels.
        ProbeChannels.register(this, flutterEngine.dartExecutor.binaryMessenger)
        // Modo en movimiento: lo corre BeaconService (nativo) para que siga vivo
        // aunque Android mate el proceso de Flutter; Dart solo lo prende/apaga.
        MethodChannel(flutterEngine.dartExecutor.binaryMessenger, "notion5g/beacon").setMethodCallHandler { call, result ->
            val prefs = BeaconService.Prefs(this)
            when (call.method) {
                "start" -> {
                    prefs.saveConfig(
                        call.argument<String>("backend_url") ?: "",
                        call.argument<String>("api_key") ?: "",
                        call.argument<String>("device_id") ?: "",
                    )
                    prefs.enabled = true
                    BeaconService.scheduleWatchdog(this)
                    try {
                        startForegroundService(Intent(this, BeaconService::class.java))
                        result.success(null)
                    } catch (e: Exception) {
                        prefs.enabled = false
                        result.error("start_failed", e.toString(), null)
                    }
                }
                "stop" -> {
                    prefs.enabled = false
                    BeaconService.cancelWatchdog(this)
                    stopService(Intent(this, BeaconService::class.java))
                    result.success(null)
                }
                "status" -> result.success(mapOf(
                    "enabled" to prefs.enabled,
                    "running" to BeaconService.running,
                    "last_ok_ms" to prefs.lastOkMs,
                    "last_error" to prefs.lastError,
                    "last_error_ms" to prefs.lastErrorMs,
                ))
                else -> result.notImplemented()
            }
        }
    }
}
