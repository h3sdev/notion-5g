import 'dart:async';
import 'dart:math' as math;

import 'package:geolocator/geolocator.dart';

import 'api_client.dart';
import 'device_status.dart';
import 'location_service.dart';

/// Modo "en movimiento": manda la ubicación fused al backend mientras esté
/// activo, para que el "perfil de ping con ubicación" del router (heartbeats
/// cada 1 min, ver cmd/routeragent/main.go) tenga con qué correlacionar
/// posición -- el módem no tiene GPS propio.
///
/// Corre como servicio en primer plano de Android (notificación fija +
/// wake lock parcial), así que sigue funcionando con la pantalla apagada: la
/// pantalla encendida era lo que agotaba la batería. Un Timer de Dart sin
/// servicio lo pausaba Android apenas se apagaba la pantalla.
///
/// Para gastar menos datos y batería no se manda cada punto: se toma uno cada
/// [fixInterval] y se envía solo si hubo desplazamiento real (más de
/// [minMoveMeters] y más que la precisión del propio punto, para que el ruido
/// del GPS estando quieto no cuente como movimiento) o si pasó [maxSilence]
/// desde el último envío. Quieto, eso da un envío cada [maxSilence], muy por
/// dentro de los 10 min que el backend acepta una ubicación (maxLocationAge).
///
/// Un error de GPS o de red no detiene el beacon: se reporta por [onError] y
/// el punto siguiente vuelve a intentar.
class LocationBeacon {
  final ApiClient _api;
  final LocationService _location;
  final Duration fixInterval;
  final double minMoveMeters;
  final Duration maxSilence;
  final void Function(String message)? onError;
  final void Function()? onTick;

  StreamSubscription<Position>? _sub;
  Position? _lastSent;
  DateTime? _lastSentAt;
  bool _sendInFlight = false;

  LocationBeacon(
    this._api,
    this._location, {
    this.fixInterval = const Duration(seconds: 15),
    this.minMoveMeters = 50,
    this.maxSilence = const Duration(minutes: 2),
    this.onError,
    this.onTick,
  });

  bool get isRunning => _sub != null;

  /// Lanza LocationException si no hay permiso o la ubicación está apagada.
  /// Hay que llamarlo con la app en pantalla: Android no deja arrancar un
  /// servicio de ubicación en primer plano desde segundo plano.
  Future<void> start() async {
    if (isRunning) return;
    await _location.ensurePermission();
    final settings = AndroidSettings(
      accuracy: LocationAccuracy.high,
      distanceFilter: 0, // el filtro por distancia se hace en _onFix, junto con maxSilence
      intervalDuration: fixInterval,
      foregroundNotificationConfig: const ForegroundNotificationConfig(
        notificationTitle: 'Notion 5G: modo en movimiento',
        notificationText: 'Enviando la ubicación al backend, también con la pantalla apagada.',
        notificationChannelName: 'Modo en movimiento',
        enableWakeLock: true,
        setOngoing: true,
      ),
    );
    _lastSent = null;
    _lastSentAt = null;
    _sub = Geolocator.getPositionStream(locationSettings: settings).listen(
      _onFix,
      onError: (Object e) => onError?.call('GPS: $e'),
    );
  }

  Future<void> stop() async {
    final sub = _sub;
    _sub = null;
    await sub?.cancel(); // sin suscriptores, geolocator detiene el servicio y quita la notificación
  }

  void _onFix(Position pos) {
    if (_sendInFlight || !_isDue(pos)) return;
    _send(pos);
  }

  bool _isDue(Position pos) {
    final last = _lastSent;
    final lastAt = _lastSentAt;
    if (last == null || lastAt == null) return true;
    if (DateTime.now().difference(lastAt) >= maxSilence) return true;
    final moved = Geolocator.distanceBetween(last.latitude, last.longitude, pos.latitude, pos.longitude);
    return moved >= math.max(minMoveMeters, pos.accuracy);
  }

  Future<void> _send(Position pos) async {
    _sendInFlight = true;
    try {
      // Batería y red del celular viajan con la ubicación: el backend las
      // guarda como historial (phone_log) para ver el consumo en el tiempo.
      final status = await DeviceStatus.read();
      await _api.postLocation(lat: pos.latitude, lon: pos.longitude, accuracyM: pos.accuracy, extra: status);
      // Solo cuenta como enviado si llegó: si falló, el siguiente punto reintenta.
      _lastSent = pos;
      _lastSentAt = DateTime.now();
      onTick?.call();
    } catch (e) {
      onError?.call(e.toString());
    } finally {
      _sendInFlight = false;
    }
  }
}
