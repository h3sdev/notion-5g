import 'package:geolocator/geolocator.dart';

/// Ubicación "ultraprecisa" vía Fused Location Provider de Google Play
/// Services (lo que expone `geolocator` en Android bajo LocationAccuracy.best),
/// NO la API de geolocalización de Google Cloud (esa estima por celda/Wi-Fi
/// sin GPS y requiere billing; se descartó a propósito, ver conversación).
class LocationException implements Exception {
  final String message;
  LocationException(this.message);
  @override
  String toString() => message;
}

class LocationService {
  /// Pide permiso si hace falta y devuelve la posición actual.
  /// Lanza LocationException con un mensaje presentable si el usuario niega
  /// el permiso, el GPS está apagado, o hay timeout.
  Future<Position> getCurrentFusedLocation() async {
    await ensurePermission();
    try {
      return await Geolocator.getCurrentPosition(
        locationSettings: const LocationSettings(
          accuracy: LocationAccuracy.best, // fused: GPS + red, lo más preciso que da Android
          timeLimit: Duration(seconds: 20),
        ),
      );
    } catch (e) {
      throw LocationException('No se pudo obtener la ubicación: $e');
    }
  }

  /// Verifica que la ubicación esté encendida y que haya permiso (lo pide si
  /// hace falta). Alcanza con el permiso "mientras se usa la app": el modo en
  /// movimiento arranca su servicio en primer plano con la app abierta, y así
  /// Android le sigue dando ubicación con la pantalla apagada.
  Future<void> ensurePermission() async {
    final serviceEnabled = await Geolocator.isLocationServiceEnabled();
    if (!serviceEnabled) {
      throw LocationException('La ubicación del teléfono está apagada. Actívala e intenta de nuevo.');
    }

    var permission = await Geolocator.checkPermission();
    if (permission == LocationPermission.denied) {
      permission = await Geolocator.requestPermission();
      if (permission == LocationPermission.denied) {
        throw LocationException('Se negó el permiso de ubicación. La app lo necesita para etiquetar las pruebas.');
      }
    }
    if (permission == LocationPermission.deniedForever) {
      throw LocationException(
          'El permiso de ubicación está bloqueado permanentemente. Actívalo desde Ajustes del sistema > Apps.');
    }
  }
}
