import 'package:flutter/services.dart';

/// Batería y red del propio celular (ver MainActivity.kt, canal "notion5g/device").
/// Nunca lanza: si el canal falla se manda la ubicación sin estos datos.
class DeviceStatus {
  static const _channel = MethodChannel('notion5g/device');

  static Future<Map<String, dynamic>> read() async {
    try {
      final m = await _channel.invokeMapMethod<String, dynamic>('status');
      return m ?? const {};
    } catch (_) {
      return const {};
    }
  }
}
