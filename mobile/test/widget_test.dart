// Smoke test: la app arranca y muestra la pantalla principal sin tronar.
// SharedPreferences necesita su mock oficial en tests (no hay platform
// channel real); no hay red real tampoco, así que el historial queda vacío
// hasta que el usuario configure un backend.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'package:notion5g_field/main.dart';

void main() {
  setUp(() {
    SharedPreferences.setMockInitialValues({});
  });

  testWidgets('la app arranca y muestra el título', (WidgetTester tester) async {
    await tester.pumpWidget(const Notion5gFieldApp());
    await tester.pumpAndSettle();

    expect(find.text('Notion 5G — pruebas de campo'), findsOneWidget);
    expect(find.byIcon(Icons.settings), findsOneWidget);
  });
}
