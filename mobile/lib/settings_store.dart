import 'package:shared_preferences/shared_preferences.dart';

/// Configuración persistida localmente (SharedPreferences): a qué backend
/// hablar, con qué API key, y a qué router (device_id) dirigir las pruebas.
/// Ver server/internal/api/api.go para el contrato del backend.
class AppSettings {
  final String backendUrl;
  final String apiKey;
  final String routerDeviceId;
  final String requestedBy;
  final int testDurationSeconds;

  const AppSettings({
    this.backendUrl = '',
    this.apiKey = '',
    this.routerDeviceId = '',
    this.requestedBy = '',
    this.testDurationSeconds = 10,
  });

  AppSettings copyWith({
    String? backendUrl,
    String? apiKey,
    String? routerDeviceId,
    String? requestedBy,
    int? testDurationSeconds,
  }) {
    return AppSettings(
      backendUrl: backendUrl ?? this.backendUrl,
      apiKey: apiKey ?? this.apiKey,
      routerDeviceId: routerDeviceId ?? this.routerDeviceId,
      requestedBy: requestedBy ?? this.requestedBy,
      testDurationSeconds: testDurationSeconds ?? this.testDurationSeconds,
    );
  }

  bool get isConfigured =>
      backendUrl.isNotEmpty && apiKey.isNotEmpty && routerDeviceId.isNotEmpty;
}

class SettingsStore {
  static const _kBackendUrl = 'backend_url';
  static const _kApiKey = 'api_key';
  static const _kRouterDeviceId = 'router_device_id';
  static const _kRequestedBy = 'requested_by';
  static const _kDurationS = 'test_duration_s';

  Future<AppSettings> load() async {
    final p = await SharedPreferences.getInstance();
    var requestedBy = p.getString(_kRequestedBy);
    if (requestedBy == null || requestedBy.isEmpty) {
      // id estable por instalación, no PII: solo para trazar qué celular pidió la prueba.
      requestedBy = 'android-${DateTime.now().millisecondsSinceEpoch}';
      await p.setString(_kRequestedBy, requestedBy);
    }
    return AppSettings(
      backendUrl: p.getString(_kBackendUrl) ?? '',
      apiKey: p.getString(_kApiKey) ?? '',
      routerDeviceId: p.getString(_kRouterDeviceId) ?? '',
      requestedBy: requestedBy,
      testDurationSeconds: p.getInt(_kDurationS) ?? 10,
    );
  }

  Future<void> save(AppSettings s) async {
    final p = await SharedPreferences.getInstance();
    await p.setString(_kBackendUrl, s.backendUrl.trim());
    await p.setString(_kApiKey, s.apiKey.trim());
    await p.setString(_kRouterDeviceId, s.routerDeviceId.trim());
    await p.setInt(_kDurationS, s.testDurationSeconds);
  }
}
