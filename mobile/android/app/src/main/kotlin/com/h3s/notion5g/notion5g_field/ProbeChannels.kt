package com.h3s.notion5g.notion5g_field

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import io.flutter.plugin.common.BinaryMessenger
import io.flutter.plugin.common.EventChannel
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

/// Canales Flutter ↔ Kotlin de la sonda (contrato §2.10):
/// MethodChannel("notion5g/probe") y EventChannel("notion5g/probe/events").
/// Nada de SQLite ni red en el hilo principal: todo corre en un executor de
/// fondo (o en el hilo del servicio que corresponde) y responde en el principal.
object ProbeChannels {
    private val exec = Executors.newCachedThreadPool()
    private val main = Handler(Looper.getMainLooper())

    private class ChannelError(val code: String, msg: String) : Exception(msg)

    fun register(activity: Activity, messenger: BinaryMessenger) {
        MethodChannel(messenger, "notion5g/probe").setMethodCallHandler { call, result -> handle(activity, call, result) }
        EventChannel(messenger, "notion5g/probe/events").setStreamHandler(object : EventChannel.StreamHandler {
            override fun onListen(arguments: Any?, events: EventChannel.EventSink?) {
                ProbeBus.setSink(events)
                val ctx = activity.applicationContext
                exec.execute {
                    try {
                        val svc = ProbeService.instance
                        if (svc != null && ProbeService.isRunning) svc.buildAndPublish() else ProbeBus.publish(ProbeStatus.build(ctx))
                    } catch (_: Exception) {
                    }
                }
            }

            override fun onCancel(arguments: Any?) {
                ProbeBus.setSink(null)
            }
        })
    }

    private fun reply(result: MethodChannel.Result, block: () -> Any?) {
        exec.execute {
            try {
                val v = block()
                main.post { result.success(v) }
            } catch (e: ChannelError) {
                main.post { result.error(e.code, e.message, null) }
            } catch (e: Exception) {
                main.post { result.error("error", "${e.javaClass.simpleName}: ${e.message}", null) }
            }
        }
    }

    /// Corre `block` en el hilo dado (worker/control del servicio) y espera con tope.
    private fun <T> onThread(h: Handler, timeoutS: Long, block: () -> T): T {
        val latch = CountDownLatch(1)
        var out: Result<T>? = null
        h.post {
            out = try { Result.success(block()) } catch (e: Exception) { Result.failure(e) }
            latch.countDown()
        }
        if (!latch.await(timeoutS, TimeUnit.SECONDS)) throw ChannelError("timeout", "La sonda está ocupada: intenta de nuevo en un momento")
        return out!!.getOrThrow()
    }

    @SuppressLint("BatteryLife")
    private fun handle(activity: Activity, call: MethodCall, result: MethodChannel.Result) {
        val ctx = activity.applicationContext
        val prefs = ProbePrefs(ctx)
        when (call.method) {
            "getConfig" -> reply(result) { prefs.all() }
            "saveConfig" -> reply(result) {
                @Suppress("UNCHECKED_CAST")
                val m = (call.arguments as? Map<String, Any?>) ?: emptyMap()
                prefs.save(m)?.let { throw ChannelError("invalid_config", it) }
                ProbeService.instance?.control?.pollNow()
                null
            }
            "start" -> {
                if (ctx.checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) != PackageManager.PERMISSION_GRANTED) {
                    result.error("start_failed", "Falta permiso de ubicación", null)
                    return
                }
                prefs.enabled = true
                ProbeService.scheduleWatchdog(ctx)
                try {
                    ctx.startForegroundService(Intent(ctx, ProbeService::class.java))
                    result.success(null)
                } catch (e: Exception) {
                    prefs.enabled = false
                    ProbeService.cancelWatchdog(ctx)
                    result.error("start_failed", "Android no dejó arrancar la sonda: ${e.javaClass.simpleName}", null)
                }
            }
            "stop" -> {
                prefs.enabled = false
                ProbeService.cancelWatchdog(ctx)
                val svc = ProbeService.instance
                if (svc != null) {
                    svc.requestStop()
                } else {
                    // Servicio detenido: igual se intenta dejar la regla en respaldo.
                    exec.execute {
                        try {
                            val (eth, _) = NetPaths.findNow(ctx)
                            if (eth != null) MikrotikOps.restore(ProbeDb.get(ctx), prefs.snapshot(), eth.network)
                        } catch (_: Exception) {
                        }
                    }
                }
                result.success(null)
            }
            "status" -> reply(result) {
                val svc = ProbeService.instance
                val s = if (svc != null && ProbeService.isRunning) svc.buildAndPublish() ?: ProbeStatus.build(ctx) else ProbeStatus.build(ctx)
                JsonConv.toPlatform(s)
            }
            "runNow" -> {
                val target = call.argument<String>("target") ?: ""
                if (!Regex("^([A-Z]|next|wifi)$").matches(target)) {
                    result.error("invalid_target", "Objetivo inválido: $target", null)
                    return
                }
                if (!prefs.enabled) {
                    result.error("not_enabled", "Activa la sonda primero", null)
                    return
                }
                reply(result) {
                    val db = ProbeDb.get(ctx)
                    val offset = db.kvLong("clock_offset_ms") ?: 0L
                    val now = System.currentTimeMillis() + offset
                    val id = "local-" + ProbeUtil.uuid()
                    db.tx { d -> db.insertLocalOrder(d, id, target, now, now + 10 * 60_000L, "manual-app", "app", prefs.int("duration_s")) }
                    db.event("info", ProbeState.phase, id, "Medición pedida desde la app: $target")
                    val svc = ProbeService.instance
                    if (svc != null && ProbeService.isRunning) svc.worker?.wake()
                    else main.post { ProbeService.startIfEnabled(ctx, "runNow") }
                    mapOf("order_id" to id)
                }
            }
            "testMikrotik" -> reply(result) {
                val svc = ProbeService.instance
                val w = svc?.worker
                when {
                    w != null && ProbeService.isRunning && ProbeState.busy -> MikrotikOps.lastRead()
                    w != null && ProbeService.isRunning -> onThread(w.handler, 40) { w.runTestMikrotik() }
                    else -> {
                        val (eth, _) = NetPaths.findNow(ctx)
                        MikrotikOps.test(ProbeDb.get(ctx), prefs.snapshot(), eth?.network)
                    }
                }
            }
            "restoreFallback" -> reply(result) {
                val svc = ProbeService.instance
                val w = svc?.worker
                when {
                    w != null && ProbeService.isRunning && ProbeState.busy -> throw ChannelError("busy", "Hay una prueba en curso: la regla vuelve sola al terminar")
                    w != null && ProbeService.isRunning -> onThread(w.handler, 40) { w.runRestore() }
                    else -> {
                        val (eth, _) = NetPaths.findNow(ctx)
                        val r = MikrotikOps.restore(ProbeDb.get(ctx), prefs.snapshot(), eth?.network)
                        mapOf("ok" to r.ok, "table" to r.table, "error" to r.error)
                    }
                }
            }
            "syncNow" -> reply(result) {
                val svc = ProbeService.instance
                val c = svc?.control
                val h = svc?.controlHandler
                if (c != null && h != null && ProbeService.isRunning) {
                    onThread(h, 120) { c.syncNow() }
                } else {
                    tempControl(ctx, prefs).syncNow()
                }
            }
            "testBackend" -> reply(result) {
                val svc = ProbeService.instance
                val cp = ProbeState.control
                if (svc != null && ProbeService.isRunning && cp != null) ProbeControl.testBackend(prefs.snapshot(), cp)
                else ProbeControl.testBackend(prefs.snapshot(), tempPlane(ctx))
            }
            "openBatterySettings" -> {
                try {
                    activity.startActivity(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:${ctx.packageName}")))
                } catch (_: Exception) {
                    try { activity.startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)) } catch (_: Exception) {}
                }
                result.success(null)
            }
            "recentResults" -> reply(result) { ProbeDb.get(ctx).recentResults(call.argument<Int>("limit") ?: 20) }
            "recentEvents" -> reply(result) { ProbeDb.get(ctx).recentEvents(call.argument<Int>("limit") ?: 100) }
            "recentReboots" -> reply(result) { ProbeDb.get(ctx).recentReboots(call.argument<Int>("limit") ?: 5) }
            else -> result.notImplemented()
        }
    }

    /// Camino al backend de un solo uso, con el servicio detenido (sin regla
    /// confirmada: nunca por Ethernet hacia un backend privado).
    private fun tempPlane(ctx: android.content.Context): ControlPlane {
        val (eth, wifi) = NetPaths.findNow(ctx)
        return ControlPlane({ eth?.network }, { wifi?.network }, { NetPaths.defaultNetworkType(ctx) }, "notion5g-probe/${ProbeUtil.appVersion(ctx)}")
    }

    private fun tempControl(ctx: android.content.Context, prefs: ProbePrefs) =
        ProbeControl(ctx, ProbeDb.get(ctx), prefs, tempPlane(ctx), null)
}
