import 'dart:async';

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

  // Modo "en movimiento": lo corre un servicio nativo que sigue vivo sin la
  // app (ver LocationBeacon); aquí solo se refleja su estado.
  BeaconStatus _beacon = const BeaconStatus();
  bool _backgroundLocation = true;
  Timer? _beaconPoll;

  @override
  void initState() {
    super.initState();
    _loadSettings();
    _refreshBeacon();
    _beaconPoll = Timer.periodic(const Duration(seconds: 5), (_) => _refreshBeacon());
  }

  @override
  void dispose() {
    _beaconPoll?.cancel(); // el servicio sigue: apagarlo es solo con el interruptor
    super.dispose();
  }

  Future<void> _refreshBeacon() async {
    final st = await LocationBeacon.status();
    final perm = await Geolocator.checkPermission();
    if (!mounted) return;
    setState(() {
      _beacon = st;
      _backgroundLocation = perm == LocationPermission.always;
    });
  }

  Future<void> _toggleBeacon(bool on) async {
    if (!on) {
      await LocationBeacon.stop();
      await _refreshBeacon();
      return;
    }
    if (!_settings.isConfigured) {
      _showSnack('Primero configura el backend y el router en Ajustes.');
      return;
    }
    try {
      await _location.ensurePermission();
      await LocationBeacon.start(_settings);
    } catch (e) {
      _showSnack(e.toString());
    }
    await _refreshBeacon();
  }

  /// Sin "Permitir todo el tiempo" el servicio no recibe ubicación cuando
  /// Android lo relanza solo (tras matar la app o al reiniciar el teléfono).
  /// En Android 11+ esto abre la pantalla de permisos de la app.
  Future<void> _requestBackgroundLocation() async {
    await Geolocator.requestPermission();
    await _refreshBeacon();
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
      setState(() => _settings = updated);
      // El servicio guarda su propia copia de la config: si está activo, se le
      // pasa la nueva para que no siga mandando al backend/router anterior.
      if (_beacon.enabled && updated.isConfigured) await LocationBeacon.start(updated);
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
                Switch(value: _beacon.enabled, onChanged: _toggleBeacon),
              ],
            ),
            Text(
              'Manda tu ubicación al backend para que el perfil de ping del router '
              '(un heartbeat por minuto) tenga con qué correlacionar posición. '
              'Sigue funcionando con la pantalla apagada mientras se vea la '
              'notificación "Notion 5G: modo en movimiento", y vuelve solo si '
              'Android cierra la app o se reinicia el teléfono. Envía cuando te '
              'mueves más de 50 m, y cada 2 min si estás quieto.\n'
              'Para que Samsung no la cierre: Ajustes > Apps > notion5g_field > '
              'Batería > "Sin restricciones".',
              style: Theme.of(context).textTheme.bodySmall,
            ),
            if (_beacon.enabled) ...[
              const SizedBox(height: 8),
              if (!_backgroundLocation) ...[
                const Text(
                  'Falta el permiso de ubicación "Permitir todo el tiempo": sin él no '
                  'vuelve solo si Android cierra la app.',
                  style: TextStyle(color: Colors.orange),
                ),
                TextButton(onPressed: _requestBackgroundLocation, child: const Text('Dar permiso')),
              ],
              if (!_beacon.running)
                const Text('El servicio no está corriendo; se relanza solo en unos minutos.',
                    style: TextStyle(color: Colors.orange)),
              if (_beacon.currentError != null)
                Text('Último intento falló: ${_beacon.currentError}', style: const TextStyle(color: Colors.red))
              else if (_beacon.lastOk != null)
                Text('Última ubicación mandada: ${_formatTs(_beacon.lastOk!.toIso8601String())}',
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
