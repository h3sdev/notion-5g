import 'package:flutter/material.dart';
import 'package:geolocator/geolocator.dart';
import 'package:intl/intl.dart';

import '../api_client.dart';
import '../location_beacon.dart';
import '../location_service.dart';
import '../models.dart';
import '../settings_store.dart';
import 'settings_screen.dart';

class HomeScreen extends StatefulWidget {
  const HomeScreen({super.key});

  @override
  State<HomeScreen> createState() => _HomeScreenState();
}

enum _RunState { idle, gettingLocation, sendingCommand, waitingResult, error, done }

class _HomeScreenState extends State<HomeScreen> {
  final _store = SettingsStore();
  final _location = LocationService();

  AppSettings _settings = const AppSettings();
  bool _loadingSettings = true;

  _RunState _runState = _RunState.idle;
  String? _statusMessage;
  Position? _lastPosition;
  int? _lastCommandId;

  List<Measurement> _measurements = [];
  List<CommandStatus> _commands = [];
  bool _loadingHistory = false;

  // Modo "en movimiento": manda la ubicación cada 15s mientras esté
  // activo, para el perfil de ping+ubicación del router (ver LocationBeacon).
  LocationBeacon? _beacon;
  bool _beaconOn = false;
  DateTime? _lastBeaconTick;
  String? _lastBeaconError;

  @override
  void initState() {
    super.initState();
    _loadSettings();
  }

  @override
  void dispose() {
    _beacon?.stop();
    super.dispose();
  }

  void _toggleBeacon(bool on) {
    if (!_settings.isConfigured) {
      _showSnack('Primero configura el backend y el router en Ajustes.');
      return;
    }
    _beacon ??= LocationBeacon(
      ApiClient(_settings),
      _location,
      onTick: () {
        if (!mounted) return;
        setState(() {
          _lastBeaconTick = DateTime.now();
          _lastBeaconError = null;
        });
      },
      onError: (msg) {
        if (!mounted) return;
        setState(() => _lastBeaconError = msg);
      },
    );
    setState(() => _beaconOn = on);
    if (on) {
      _beacon!.start();
    } else {
      _beacon!.stop();
    }
  }

  Future<void> _loadSettings() async {
    final s = await _store.load();
    setState(() {
      _settings = s;
      _loadingSettings = false;
    });
    if (s.isConfigured) _refreshHistory();
  }

  Future<void> _openSettings() async {
    final updated = await Navigator.of(context).push<AppSettings>(
      MaterialPageRoute(builder: (_) => SettingsScreen(initial: _settings)),
    );
    if (updated != null) {
      // El ApiClient del beacon quedó armado con la config vieja (backend_url/
      // device_id/api_key); más seguro apagarlo y que el usuario lo prenda de
      // nuevo (así se reconstruye con la config actual) que dejarlo mandando
      // datos a la URL/router equivocado sin avisar.
      final wasOn = _beaconOn;
      _beacon?.stop();
      _beacon = null;
      setState(() {
        _settings = updated;
        _beaconOn = false;
      });
      if (wasOn) _showSnack('Ajustes cambiados: vuelve a activar "modo en movimiento".');
      _refreshHistory();
    }
  }

  Future<void> _refreshHistory() async {
    if (!_settings.isConfigured) return;
    setState(() => _loadingHistory = true);
    try {
      final client = ApiClient(_settings);
      final measurements = await client.recentMeasurements();
      final commands = await client.recentCommands();
      if (!mounted) return;
      setState(() {
        _measurements = measurements;
        _commands = commands;
      });
    } on ApiException catch (e) {
      if (mounted) _showSnack(e.message);
    } finally {
      if (mounted) setState(() => _loadingHistory = false);
    }
  }

  Future<void> _runTest() async {
    if (!_settings.isConfigured) {
      _showSnack('Primero configura el backend y el router en Ajustes.');
      return;
    }
    setState(() {
      _runState = _RunState.gettingLocation;
      _statusMessage = 'Obteniendo ubicación GPS (fused)...';
    });
    try {
      final pos = await _location.getCurrentFusedLocation();
      setState(() {
        _lastPosition = pos;
        _runState = _RunState.sendingCommand;
        _statusMessage = 'Enviando comando de prueba al backend...';
      });

      final client = ApiClient(_settings);
      final id = await client.createSpeedtestCommand(
        lat: pos.latitude,
        lon: pos.longitude,
        accuracyM: pos.accuracy,
      );
      setState(() {
        _lastCommandId = id;
        _runState = _RunState.waitingResult;
        _statusMessage = 'Comando #$id creado. El router lo recoge en su próximo ciclo de cron.';
      });
      await _refreshHistory();
      setState(() => _runState = _RunState.done);
    } on LocationException catch (e) {
      setState(() {
        _runState = _RunState.error;
        _statusMessage = e.message;
      });
    } on ApiException catch (e) {
      setState(() {
        _runState = _RunState.error;
        _statusMessage = e.message;
      });
    } catch (e) {
      setState(() {
        _runState = _RunState.error;
        _statusMessage = 'Error inesperado: $e';
      });
    }
  }

  void _showSnack(String msg) {
    ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(msg)));
  }

  @override
  Widget build(BuildContext context) {
    if (_loadingSettings) {
      return const Scaffold(body: Center(child: CircularProgressIndicator()));
    }
    return Scaffold(
      appBar: AppBar(
        title: const Text('Notion 5G — pruebas de campo'),
        actions: [
          IconButton(icon: const Icon(Icons.settings), onPressed: _openSettings),
        ],
      ),
      body: RefreshIndicator(
        onRefresh: _refreshHistory,
        child: ListView(
          padding: const EdgeInsets.all(16),
          children: [
            if (!_settings.isConfigured)
              Card(
                color: Colors.amber.shade100,
                child: const Padding(
                  padding: EdgeInsets.all(12),
                  child: Text('Falta configurar el backend y el router objetivo. Toca el ícono de ajustes.'),
                ),
              ),
            _buildRunCard(),
            const SizedBox(height: 16),
            _buildBeaconCard(),
            const SizedBox(height: 24),
            _buildSectionHeader('Última mediciones — ${_settings.routerDeviceId}'),
            if (_loadingHistory) const LinearProgressIndicator(),
            ..._measurements.map(_buildMeasurementTile),
            if (_measurements.isEmpty && !_loadingHistory)
              const Padding(
                padding: EdgeInsets.symmetric(vertical: 8),
                child: Text('Sin mediciones todavía.'),
              ),
            const SizedBox(height: 24),
            _buildSectionHeader('Comandos recientes'),
            ..._commands.map(_buildCommandTile),
          ],
        ),
      ),
    );
  }

  Widget _buildSectionHeader(String text) =>
      Padding(padding: const EdgeInsets.only(bottom: 8), child: Text(text, style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 16)));

  Widget _buildRunCard() {
    final busy = _runState == _RunState.gettingLocation ||
        _runState == _RunState.sendingCommand ||
        _runState == _RunState.waitingResult;
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            FilledButton.icon(
              onPressed: busy ? null : _runTest,
              icon: busy
                  ? const SizedBox(width: 18, height: 18, child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white))
                  : const Icon(Icons.speed),
              label: Text(busy ? 'En curso...' : 'Correr prueba en el router'),
            ),
            if (_statusMessage != null) ...[
              const SizedBox(height: 12),
              Text(
                _statusMessage!,
                style: TextStyle(color: _runState == _RunState.error ? Colors.red : null),
              ),
            ],
            if (_lastCommandId != null) ...[
              const SizedBox(height: 4),
              Text('Último comando enviado: #$_lastCommandId', style: Theme.of(context).textTheme.bodySmall),
            ],
            if (_lastPosition != null) ...[
              const SizedBox(height: 8),
              Text(
                'Ubicación: ${_lastPosition!.latitude.toStringAsFixed(6)}, '
                '${_lastPosition!.longitude.toStringAsFixed(6)} (±${_lastPosition!.accuracy.toStringAsFixed(1)} m)',
                style: Theme.of(context).textTheme.bodySmall,
              ),
            ],
          ],
        ),
      ),
    );
  }

  Widget _buildBeaconCard() {
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Row(
              children: [
                const Expanded(
                  child: Text(
                    'Modo en movimiento',
                    style: TextStyle(fontWeight: FontWeight.bold),
                  ),
                ),
                Switch(value: _beaconOn, onChanged: _toggleBeacon),
              ],
            ),
            Text(
              'Manda tu ubicación cada 15s mientras esté activo, para que el '
              'perfil de ping del router (un heartbeat por minuto) tenga con '
              'qué correlacionar posición. Déjalo prendido durante la prueba en carretera.',
              style: Theme.of(context).textTheme.bodySmall,
            ),
            if (_beaconOn) ...[
              const SizedBox(height: 8),
              if (_lastBeaconError != null)
                Text('Último intento falló: $_lastBeaconError', style: const TextStyle(color: Colors.red))
              else if (_lastBeaconTick != null)
                Text('Última ubicación mandada: ${_formatTs(_lastBeaconTick!.toIso8601String())}',
                    style: Theme.of(context).textTheme.bodySmall)
              else
                const Text('Mandando la primera ubicación...'),
            ],
          ],
        ),
      ),
    );
  }

  Widget _buildMeasurementTile(Measurement m) {
    final ts = m.ts != null ? _formatTs(m.ts!) : '?';
    return ListTile(
      dense: true,
      leading: const Icon(Icons.signal_cellular_alt),
      title: Text('${m.operator_ ?? '?'} ${m.rat ?? ''} B${m.bandLte ?? '?'}  RSRP ${m.rsrpDbm ?? '?'} dBm'),
      subtitle: Text('$ts · ↓${m.downMbps ?? '-'} Mbps ↑${m.upMbps ?? '-'} Mbps · fuente: ${m.source ?? '?'}'),
    );
  }

  Widget _buildCommandTile(CommandStatus c) {
    final color = switch (c.status) {
      'done' => Colors.green,
      'failed' => Colors.red,
      'claimed' => Colors.orange,
      _ => Colors.grey,
    };
    return ListTile(
      dense: true,
      leading: Icon(Icons.circle, color: color, size: 12),
      title: Text('Comando #${c.id} · ${c.type}'),
      subtitle: Text('estado: ${c.status}${c.createdAt != null ? ' · ${_formatTs(c.createdAt!)}' : ''}'),
    );
  }

  String _formatTs(String iso) {
    try {
      return DateFormat('yyyy-MM-dd HH:mm:ss').format(DateTime.parse(iso).toLocal());
    } catch (_) {
      return iso;
    }
  }
}
