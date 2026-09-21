import 'package:flutter/material.dart';

import '../api_client.dart';
import '../settings_store.dart';

class SettingsScreen extends StatefulWidget {
  final AppSettings initial;
  const SettingsScreen({super.key, required this.initial});

  @override
  State<SettingsScreen> createState() => _SettingsScreenState();
}

class _SettingsScreenState extends State<SettingsScreen> {
  late final TextEditingController _backendUrlCtrl;
  late final TextEditingController _apiKeyCtrl;
  late final TextEditingController _deviceIdCtrl;
  late int _durationS;
  final _store = SettingsStore();
  String? _testResult;
  bool _testing = false;

  @override
  void initState() {
    super.initState();
    _backendUrlCtrl = TextEditingController(text: widget.initial.backendUrl);
    _apiKeyCtrl = TextEditingController(text: widget.initial.apiKey);
    _deviceIdCtrl = TextEditingController(text: widget.initial.routerDeviceId);
    _durationS = widget.initial.testDurationSeconds;
  }

  @override
  void dispose() {
    _backendUrlCtrl.dispose();
    _apiKeyCtrl.dispose();
    _deviceIdCtrl.dispose();
    super.dispose();
  }

  AppSettings _current() => widget.initial.copyWith(
        backendUrl: _backendUrlCtrl.text,
        apiKey: _apiKeyCtrl.text,
        routerDeviceId: _deviceIdCtrl.text,
        testDurationSeconds: _durationS,
      );

  Future<void> _testConnection() async {
    setState(() {
      _testing = true;
      _testResult = null;
    });
    try {
      final ok = await ApiClient(_current()).healthCheck();
      setState(() => _testResult = ok ? 'Conexión OK' : 'El backend respondió, pero algo no cuadra.');
    } catch (e) {
      setState(() => _testResult = e.toString());
    } finally {
      setState(() => _testing = false);
    }
  }

  Future<void> _save() async {
    final s = _current();
    await _store.save(s);
    if (mounted) Navigator.of(context).pop(s);
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Ajustes')),
      body: ListView(
        padding: const EdgeInsets.all(16),
        children: [
          const Text('Backend (server/)', style: TextStyle(fontWeight: FontWeight.bold)),
          const SizedBox(height: 8),
          TextField(
            controller: _backendUrlCtrl,
            decoration: const InputDecoration(
              labelText: 'URL del backend',
              hintText: 'https://xxxx.trycloudflare.com',
              border: OutlineInputBorder(),
            ),
            keyboardType: TextInputType.url,
          ),
          const SizedBox(height: 12),
          TextField(
            controller: _apiKeyCtrl,
            decoration: const InputDecoration(
              labelText: 'API key (X-API-Key)',
              border: OutlineInputBorder(),
            ),
            obscureText: true,
          ),
          const SizedBox(height: 20),
          const Text('Router bajo prueba', style: TextStyle(fontWeight: FontWeight.bold)),
          const SizedBox(height: 8),
          TextField(
            controller: _deviceIdCtrl,
            decoration: const InputDecoration(
              labelText: 'device_id del router',
              hintText: 'router-R524260829000001',
              border: OutlineInputBorder(),
            ),
          ),
          const SizedBox(height: 20),
          Text('Duración de la prueba: $_durationS s', style: const TextStyle(fontWeight: FontWeight.bold)),
          Slider(
            value: _durationS.toDouble(),
            min: 5,
            max: 30,
            divisions: 25,
            label: '$_durationS s',
            onChanged: (v) => setState(() => _durationS = v.round()),
          ),
          const SizedBox(height: 20),
          OutlinedButton.icon(
            onPressed: _testing ? null : _testConnection,
            icon: _testing
                ? const SizedBox(width: 16, height: 16, child: CircularProgressIndicator(strokeWidth: 2))
                : const Icon(Icons.wifi_tethering),
            label: const Text('Probar conexión'),
          ),
          if (_testResult != null) ...[
            const SizedBox(height: 8),
            Text(_testResult!, style: TextStyle(color: _testResult == 'Conexión OK' ? Colors.green : Colors.red)),
          ],
          const SizedBox(height: 24),
          FilledButton(onPressed: _save, child: const Text('Guardar')),
        ],
      ),
    );
  }
}
