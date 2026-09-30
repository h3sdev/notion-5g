import 'dart:async';

import 'package:flutter/services.dart';

/// Sonda A/B con el celular por cable (forma A de server/docs/PLAN-SONDA-AB.md).
///
/// Todo lo que mide vive en un servicio nativo (ProbeService.kt) que sigue vivo
/// sin la app; desde aquí solo se configura, se prende/apaga y se muestra.
/// Contrato: server/docs/CONTRATO-SONDA-AB.md §2.10.
class ProbeChannel {
  static const _m = MethodChannel('notion5g/probe');
  static const _e = EventChannel('notion5g/probe/events');

  /// Estado en vivo (objeto de §2.5), un evento por cambio.
  static Stream<Map<String, dynamic>> events() =>
      _e.receiveBroadcastStream().map((e) => asMap(e) ?? const <String, dynamic>{});

  static Map<String, dynamic>? asMap(Object? v) {
    if (v is! Map) return null;
    return v.map((k, val) => MapEntry(k.toString(), _deep(val)));
  }

  static Object? _deep(Object? v) {
    if (v is Map) return asMap(v);
    if (v is List) return v.map(_deep).toList();
    return v;
  }

  static List<Map<String, dynamic>> _list(Object? v) =>
      v is List ? v.map((e) => asMap(e)).whereType<Map<String, dynamic>>().toList() : const [];

  static Future<Map<String, dynamic>> getConfig() async =>
      asMap(await _m.invokeMethod('getConfig')) ?? const {};

  /// Error `invalid_config` (PlatformException) con el mensaje en español.
  static Future<void> saveConfig(Map<String, dynamic> partial) => _m.invokeMethod('saveConfig', partial);

  static Future<void> start() => _m.invokeMethod('start');

  static Future<void> stop() => _m.invokeMethod('stop');

  /// null si el canal no existe (p. ej. en tests).
  static Future<Map<String, dynamic>?> status() async {
    try {
      return asMap(await _m.invokeMethod('status'));
    } catch (_) {
      return null;
    }
  }

  static Future<String?> runNow(String target) async =>
      asMap(await _m.invokeMethod('runNow', {'target': target}))?['order_id'] as String?;

  static Future<Map<String, dynamic>> testMikrotik() async =>
      asMap(await _m.invokeMethod('testMikrotik')) ?? const {};

  static Future<Map<String, dynamic>> restoreFallback() async =>
      asMap(await _m.invokeMethod('restoreFallback')) ?? const {};

  static Future<Map<String, dynamic>> syncNow() async => asMap(await _m.invokeMethod('syncNow')) ?? const {};

  static Future<Map<String, dynamic>> testBackend() async =>
      asMap(await _m.invokeMethod('testBackend')) ?? const {};

  static Future<void> openBatterySettings() => _m.invokeMethod('openBatterySettings');

  static Future<List<Map<String, dynamic>>> recentResults({int limit = 20}) async =>
      _list(await _m.invokeMethod('recentResults', {'limit': limit}));

  static Future<List<Map<String, dynamic>>> recentEvents({int limit = 100}) async =>
      _list(await _m.invokeMethod('recentEvents', {'limit': limit}));

  /// Órdenes de reinicio de routers (§6.2), abiertas primero.
  static Future<List<Map<String, dynamic>>> recentReboots({int limit = 5}) async =>
      _list(await _m.invokeMethod('recentReboots', {'limit': limit}));
}

/// Textos en español (los mismos del dashboard, contrato §2.11).
class ProbeText {
  static const phases = <String, String>{
    'idle': 'En espera',
    'waiting_ethernet': 'Esperando el cable',
    'preparing': 'Preparando',
    'switching_route': 'Cambiando la ruta',
    'verifying_route': 'Verificando la ruta',
    'egress_check': 'Comprobando la salida',
    'gps_fix': 'Esperando GPS',
    'ping': 'Midiendo latencia',
    'download': 'Descargando',
    'upload': 'Subiendo',
    'storing': 'Guardando',
    'restoring_route': 'Volviendo a respaldo',
    'uploading': 'Enviando resultado',
    'backoff': 'Sin backend, reintentando',
    'stopped': 'Detenida',
    'rebooting': 'Reiniciando router',
  };

  /// Pasos de un reinicio (§6.2): (paso, etiqueta corta).
  static const rebootSteps = <List<String>>[
    ['received', 'Recibida'],
    ['ssh', 'SSH'],
    ['waiting_down', 'Apagándose'],
    ['waiting_back', 'Esperando que vuelva'],
    ['waiting_internet', 'Internet'],
    ['done', 'Listo'],
  ];

  static String rebootError(String? e) => switch (e) {
        null => '',
        'ssh-auth' => 'usuario o clave SSH rechazados',
        'identity-mismatch' => 'en ese puerto hay otro router: no se reinició',
        'ssh-connect' => 'no se pudo conectar por SSH',
        'no-ethernet' => 'sin cable Ethernet',
        'mikrotik-unreachable' => 'no se alcanzó el MikroTik',
        'timeout-back' => 'no volvió en 5 min',
        'no-reboot' => 'siguió respondiendo: no se reinició',
        'detenida' => 'la sonda se detuvo',
        'vencida' => 'venció sin ejecutarse',
        'cancelada' => 'cancelada',
        _ => e,
      };

  static String orderStatus(String? s) => switch (s) {
        'pending' => 'pendiente',
        'delivered' => 'recibida',
        'running' => 'en curso',
        'done' => 'lista',
        'failed' => 'fallida',
        'expired' => 'vencida',
        'interrupted' => 'interrumpida',
        'cancelled' => 'cancelada',
        _ => s ?? '?',
      };

  /// "3 min" desde una hora RFC 3339 (sin "hace").
  static String duration(String? iso) {
    final t = iso == null ? null : DateTime.tryParse(iso);
    if (t == null) return '-';
    final d = DateTime.now().difference(t);
    if (d.inSeconds < 60) return '${d.inSeconds < 0 ? 0 : d.inSeconds} s';
    if (d.inMinutes < 60) return '${d.inMinutes} min';
    if (d.inHours < 48) return '${d.inHours} h ${d.inMinutes % 60} min';
    return '${d.inDays} días';
  }

  static String phase(String? p) => phases[p] ?? (p ?? '?');

  /// Barra de pasos: (fase, etiqueta corta).
  static const steps = <List<String>>[
    ['switching_route', 'Ruta'],
    ['verifying_route', 'Verificación'],
    ['egress_check', 'Salida'],
    ['gps_fix', 'GPS'],
    ['ping', 'Ping'],
    ['download', 'Bajada'],
    ['upload', 'Subida'],
    ['storing', 'Guardar'],
    ['restoring_route', 'Respaldo'],
  ];

  static String table(String? t, {String fallback = 'main'}) {
    if (t == null) return 'desconocida';
    if (t == fallback) return 'Respaldo (sale por el que tenga datos)';
    final m = RegExp(r'^to-([A-Z])$').firstMatch(t);
    if (m != null) return 'Forzada a ${m.group(1)}';
    return t;
  }

  static String reason(String? r) {
    switch (r) {
      case 'requested':
        return 'pedida desde el dashboard';
      case 'next':
        return 'siguiente disponible';
      case 'sequence':
        return 'secuencia alternada';
      case 'alternation':
        return 'ciclo A/B';
      case 'offline-schedule':
        return 'plan local sin backend';
      case 'manual-app':
        return 'pedida desde la app';
    }
    if (r != null && r.startsWith('fallback:')) {
      final slot = r.substring(9).split('-').first;
      return 'respaldo: $slot sin datos';
    }
    return r ?? '';
  }

  static String serverReason(String? r) {
    switch (r) {
      case 'phone-wifi':
        return 'por WiFi, no se atribuye a ningún router';
      case 'probe-route-unconfirmed':
        return 'no se confirmó la ruta en el MikroTik';
      case 'probe-asn-match':
        return 'salió por el operador esperado';
      case 'probe-asn-other-router':
        return 'salió por el OTRO router';
      case 'probe-asn-mismatch':
        return 'el operador de salida no coincide';
      case 'probe-asn-ambiguous':
        return 'ruta confirmada; los dos routers son del mismo operador';
      case 'network-changed-mid-test':
        return 'la IP de salida cambió durante la prueba';
      case 'probe-route-changed':
        return 'la regla del MikroTik cambió durante la prueba: no se atribuye';
      case 'probe-table-confirmed':
        return 'ruta confirmada en el MikroTik (sin verificar operador)';
    }
    return r ?? '';
  }

  static String gps(String? s) => switch (s) {
        'fresh' => 'GPS FRESCO',
        'stale' => 'GPS VIEJO',
        _ => 'SIN FIJO',
      };

  static String sync(String? s) => switch (s) {
        'synced' => 'Subido',
        'rejected' => 'Rechazado',
        _ => 'Pendiente',
      };

  static String num1(Object? v) {
    if (v is! num) return '-';
    return v.toStringAsFixed(1).replaceAll('.', ',');
  }

  /// "hace 3 min" a partir de una hora RFC 3339.
  static String ago(String? iso) {
    if (iso == null) return '-';
    final t = DateTime.tryParse(iso);
    if (t == null) return iso;
    final d = DateTime.now().difference(t);
    if (d.inSeconds < 60) return 'hace ${d.inSeconds < 0 ? 0 : d.inSeconds} s';
    if (d.inMinutes < 60) return 'hace ${d.inMinutes} min';
    if (d.inHours < 48) return 'hace ${d.inHours} h';
    return 'hace ${d.inDays} días';
  }

  static String hhmm(String? iso) {
    final t = iso == null ? null : DateTime.tryParse(iso)?.toLocal();
    if (t == null) return '-';
    String two(int n) => n.toString().padLeft(2, '0');
    return '${two(t.hour)}:${two(t.minute)}:${two(t.second)}';
  }
}
