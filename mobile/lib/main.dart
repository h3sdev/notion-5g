import 'package:flutter/material.dart';

import 'screens/home_screen.dart';

void main() {
  runApp(const Notion5gFieldApp());
}

class Notion5gFieldApp extends StatelessWidget {
  const Notion5gFieldApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Notion 5G Field',
      theme: ThemeData(colorSchemeSeed: Colors.indigo, useMaterial3: true),
      home: const HomeScreen(),
    );
  }
}
