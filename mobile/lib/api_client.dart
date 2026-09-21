import 'dart:convert';
import 'package:http/http.dart' as http;

import 'models.dart';
import 'settings_store.dart';

/// Excepción con mensaje ya pensado para mostrarle al usuario (sin stack
/// traces ni jerga HTTP), para que la UI nunca truene por un error de red.
class ApiException implements Exception {
  final String message;
  ApiException(this.message);
  @override
  String toString() => message;
}

/// Cliente del backend (server/internal/api/api.go). Todas las llamadas
/// llevan el header X-API-Key salvo /healthz.
class ApiClient {
  final AppSettings settings;
  final http.Client _http;
  static const _timeout = Duration(seconds: 15);

  ApiClient(this.settings, {http.Client? client}) : _http = client ?? http.Client();

  Uri _uri(String path, [Map<String, String>? query]) {
    final base = settings.backendUrl.trim();
    if (base.isEmpty) {
      throw ApiException('Falta configurar la URL del backend.');
    }
    final normalized = base.endsWith('/') ? base.substring(0, base.length - 1) : base;
    return Uri.parse('$normalized$path').replace(queryParameters: query);
  }

  Map<String, String> get _headers => {
        'Content-Type': 'application/json',
        if (settings.apiKey.isNotEmpty) 'X-API-Key': settings.apiKey,
      };

  Future<T> _guard<T>(Future<T> Function() body) async {
    try {
      return await body();
    } on ApiException {
      rethrow;
    } catch (e) {
      throw ApiException('No se pudo conectar con el backend: $e');
    }
  }

  /// Prueba de vida simple, para el botón "probar conexión" en Ajustes.
  Future<bool> healthCheck() => _guard(() async {
        final base = settings.backendUrl.trim();
        if (base.isEmpty) throw ApiException('Falta configurar la URL del backend.');
        final normalized = base.endsWith('/') ? base.substring(0, base.length - 1) : base;
        final resp = await _http.get(Uri.parse('$normalized/healthz')).timeout(_timeout);
        return resp.statusCode == 200;
      });

  /// Crea un comando run_speedtest para el router configurado, con la
  /// ubicación fused actual. Devuelve el id del comando creado.
  Future<int> createSpeedtestCommand({
    required double lat,
    required double lon,
    required double accuracyM,
  }) {
    return _guard(() async {
      final resp = await _http
          .post(
            _uri('/api/v1/commands'),
            headers: _headers,
            body: jsonEncode({
              'device_id': settings.routerDeviceId,
              'type': 'run_speedtest',
              'duration_s': settings.testDurationSeconds,
              'lat': lat,
              'lon': lon,
              'gps_accuracy_m': accuracyM,
              'gps_source': 'android-fused',
              'requested_by': settings.requestedBy,
            }),
          )
          .timeout(_timeout);
      _checkStatus(resp);
      final body = jsonDecode(resp.body) as Map<String, dynamic>;
      return (body['id'] as num).toInt();
    });
  }

  /// Manda la ubicación actual del celular como "última ubicación conocida"
  /// del router (server/internal/store/store.go: device_locations). El
  /// backend la usa para completar mediciones/heartbeats del router, que no
  /// tiene GPS propio -- esto es lo que le da sentido al "perfil de ping con
  /// ubicación" mientras el equipo va en movimiento (ver [LocationBeacon]).
  Future<void> postLocation({
    required double lat,
    required double lon,
    required double accuracyM,
  }) {
    return _guard(() async {
      final deviceId = settings.routerDeviceId.trim();
      if (deviceId.isEmpty) {
        throw ApiException('Falta configurar el device_id del router en Ajustes.');
      }
      final resp = await _http
          .post(
            _uri('/api/v1/devices/${Uri.encodeComponent(deviceId)}/location'),
            headers: _headers,
            body: jsonEncode({
              'lat': lat,
              'lon': lon,
              'gps_accuracy_m': accuracyM,
              'gps_source': 'android-fused',
            }),
          )
          .timeout(_timeout);
      _checkStatus(resp);
    });
  }

  Future<List<Measurement>> recentMeasurements({int limit = 20}) {
    return _guard(() async {
      final resp = await _http
          .get(
            _uri('/api/v1/measurements', {
              'device_id': settings.routerDeviceId,
              'limit': '$limit',
            }),
            headers: _headers,
          )
          .timeout(_timeout);
      _checkStatus(resp);
      final list = jsonDecode(resp.body) as List<dynamic>;
      return list.map((e) => Measurement.fromJson(e as Map<String, dynamic>)).toList();
    });
  }

  Future<List<CommandStatus>> recentCommands({int limit = 10}) {
    return _guard(() async {
      final resp = await _http
          .get(
            _uri('/api/v1/commands', {
              'device_id': settings.routerDeviceId,
              'limit': '$limit',
            }),
            headers: _headers,
          )
          .timeout(_timeout);
      _checkStatus(resp);
      final list = jsonDecode(resp.body) as List<dynamic>;
      return list.map((e) => CommandStatus.fromJson(e as Map<String, dynamic>)).toList();
    });
  }

  void _checkStatus(http.Response resp) {
    if (resp.statusCode == 401) {
      throw ApiException('API key inválida o ausente. Revisa Ajustes.');
    }
    if (resp.statusCode >= 400) {
      String detail = resp.body;
      try {
        final body = jsonDecode(resp.body);
        if (body is Map && body['error'] != null) detail = body['error'].toString();
      } catch (_) {}
      throw ApiException('Backend respondió ${resp.statusCode}: $detail');
    }
  }
}
