import 'package:flutter/services.dart';

import 'settings_store.dart';

/// Modo "en movimiento": manda la ubicación al backend para que el perfil de
/// ping del router (heartbeats cada 1 min, ver cmd/routeragent/main.go) tenga
/// con qué correlacionar posición -- el módem no tiene GPS propio.
///
/// Lo corre un servicio nativo (BeaconService.kt), no Dart: si Android mata el
/// proceso de la app el servicio vuelve solo, y también al reiniciar el
/// teléfono. Desde aquí solo se prende, se apaga y se consulta el estado; el
/// interruptor queda guardado del lado nativo, así que sobrevive a la app.
class BeaconStatus {
  final bool enabled;
  final bool running;
  final DateTime? lastOk;
  final String? lastError;
  final DateTime? lastErrorAt;

  const BeaconStatus({
    this.enabled = false,
    this.running = false,
    this.lastOk,
    this.lastError,
    this.lastErrorAt,
  });

  /// El error solo importa si es posterior al último envío que sí llegó.
  String? get currentError {
    if (lastError == null) return null;
    if (lastOk != null && lastErrorAt != null && lastErrorAt!.isBefore(lastOk!)) return null;
    return lastError;
  }
}

class LocationBeacon {
  static const _channel = MethodChannel('notion5g/beacon');

  /// Guarda la config y arranca el servicio. Hay que llamarlo con la app en
  /// pantalla y con permiso de ubicación ya concedido (ver LocationService).
  /// Si ya estaba corriendo, solo actualiza la config.
  static Future<void> start(AppSettings s) => _channel.invokeMethod('start', {
        'backend_url': s.backendUrl,
        'api_key': s.apiKey,
        'device_id': s.routerDeviceId,
      });

  static Future<void> stop() => _channel.invokeMethod('stop');

  static Future<BeaconStatus> status() async {
    try {
      final m = await _channel.invokeMapMethod<String, dynamic>('status') ?? const {};
      DateTime? ts(Object? ms) => (ms is int && ms > 0) ? DateTime.fromMillisecondsSinceEpoch(ms) : null;
      return BeaconStatus(
        enabled: m['enabled'] == true,
        running: m['running'] == true,
        lastOk: ts(m['last_ok_ms']),
        lastError: m['last_error'] as String?,
        lastErrorAt: ts(m['last_error_ms']),
      );
    } catch (_) {
      return const BeaconStatus();
    }
  }
}
