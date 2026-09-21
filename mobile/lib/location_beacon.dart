import 'dart:async';

import 'api_client.dart';
import 'location_service.dart';

/// Modo "en movimiento": manda la ubicación fused al backend cada
/// [interval] mientras esté activo, para que el "perfil de ping con
/// ubicación" del router (heartbeats cada 1 min, ver
/// cmd/routeragent/main.go) tenga con qué correlacionar posición -- el
/// módem no tiene GPS propio, así que sin esto sus heartbeats no llevarían
/// coordenadas.
///
/// Cada tick es independiente: un error de GPS o de red en un tick (el
/// celular se metió a un túnel, perdió señal, lo que sea) no detiene el
/// beacon ni tumba la app -- solo se reporta por [onError] y se reintenta
/// en el siguiente tick.
class LocationBeacon {
  final ApiClient _api;
  final LocationService _location;
  final Duration interval;
  final void Function(String message)? onError;
  final void Function()? onTick;

  Timer? _timer;
  bool _tickInFlight = false;

  LocationBeacon(
    this._api,
    this._location, {
    this.interval = const Duration(seconds: 15),
    this.onError,
    this.onTick,
  });

  bool get isRunning => _timer != null;

  void start() {
    if (isRunning) return;
    _tick(); // no esperar al primer intervalo para el primer envío
    _timer = Timer.periodic(interval, (_) => _tick());
  }

  void stop() {
    _timer?.cancel();
    _timer = null;
  }

  Future<void> _tick() async {
    // Si el tick anterior sigue esperando el GPS/la red (p. ej. timeout
    // largo), no se acumulan ticks superpuestos.
    if (_tickInFlight) return;
    _tickInFlight = true;
    try {
      final pos = await _location.getCurrentFusedLocation();
      await _api.postLocation(lat: pos.latitude, lon: pos.longitude, accuracyM: pos.accuracy);
      onTick?.call();
    } catch (e) {
      onError?.call(e.toString());
    } finally {
      _tickInFlight = false;
    }
  }
}
