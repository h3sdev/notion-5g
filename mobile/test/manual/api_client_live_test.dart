// Prueba MANUAL contra un backend real (server/) — hace red de verdad, no
// mockeada. NO forma parte de la suite normal (`flutter test` sin argumentos
// no la corre porque solo se ejecuta si se invoca este archivo explícito, y
// además arranca desactivada por defecto vía Platform.environment).
//
// Uso:
//   NOTION_BACKEND_URL=http://127.0.0.1:8098 \
//   NOTION_API_KEY=<clave> \
//   NOTION_DEVICE_ID=router-mobile-smoke-test \
//   flutter test test/manual/api_client_live_test.dart
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:notion5g_field/api_client.dart';
import 'package:notion5g_field/settings_store.dart';

void main() {
  final backendUrl = Platform.environment['NOTION_BACKEND_URL'];
  final apiKey = Platform.environment['NOTION_API_KEY'];
  final deviceId = Platform.environment['NOTION_DEVICE_ID'] ?? 'router-mobile-smoke-test';

  test('ApiClient contra un backend real (requiere NOTION_BACKEND_URL/NOTION_API_KEY)', () async {
    if (backendUrl == null || apiKey == null) {
      // No es un skip "silencioso": si falta config, se marca explícito.
      // ignore: avoid_print
      print('SKIP: define NOTION_BACKEND_URL y NOTION_API_KEY para correr esta prueba.');
      return;
    }
    final settings = AppSettings(
      backendUrl: backendUrl,
      apiKey: apiKey,
      routerDeviceId: deviceId,
      requestedBy: 'flutter-manual-test',
      testDurationSeconds: 5,
    );
    final client = ApiClient(settings);

    expect(await client.healthCheck(), isTrue);

    final id = await client.createSpeedtestCommand(lat: 4.6512, lon: -74.0567, accuracyM: 4.2);
    expect(id, greaterThan(0));
    // ignore: avoid_print
    print('comando creado: #$id');

    final cmds = await client.recentCommands();
    expect(cmds.any((c) => c.id == id), isTrue, reason: 'el comando recién creado debe aparecer en el listado');

    final meas = await client.recentMeasurements();
    // ignore: avoid_print
    print('${meas.length} mediciones existentes para $deviceId');
  });
}
