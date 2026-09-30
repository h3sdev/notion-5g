package com.h3s.notion5g.notion5g_field

import android.app.job.JobParameters
import android.app.job.JobService
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/// Relanza el modo en movimiento y la sonda A/B al prender el teléfono o al
/// instalar una versión nueva de la app, si estaban activos (ver BeaconService
/// y ProbeService).
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED, Intent.ACTION_MY_PACKAGE_REPLACED -> {
                if (BeaconService.Prefs(context).enabled) BeaconService.scheduleWatchdog(context)
                BeaconService.startIfEnabled(context, intent.action ?: "boot")
                // Sonda A/B: misma receta (BOOT_COMPLETED y MY_PACKAGE_REPLACED
                // están exentos de la restricción de arranque en segundo plano).
                if (ProbePrefs(context).enabled) ProbeService.scheduleWatchdog(context)
                ProbeService.startIfEnabled(context, intent.action ?: "boot")
            }
        }
    }
}

/// Red de seguridad cada 15 min: si Android mató el servicio y START_STICKY no
/// lo revivió (Samsung a veces no lo hace), lo vuelve a arrancar.
class BeaconWatchdog : JobService() {
    override fun onStartJob(params: JobParameters?): Boolean {
        if (!BeaconService.Prefs(this).enabled) {
            BeaconService.cancelWatchdog(this)
        } else {
            BeaconService.startIfEnabled(this, "watchdog")
        }
        return false
    }

    override fun onStopJob(params: JobParameters?): Boolean = false
}
