import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:geolocator/geolocator.dart';

import '../probe_channel.dart';
import '../settings_store.dart';
import 'probe_settings_screen.dart';

/// "Sonda A/B": qué está haciendo en este momento el celular que mide por el
/// router A o B a través del MikroTik (contrato §2.11). Funciona igual con el
/// servicio detenido: muestra la fase "Detenida" y lo guardado.
class ProbeScreen extends StatefulWidget {
  final AppSettings general;
  const ProbeScreen({super.key, required this.general});

  @override
  State<ProbeScreen> createState() => _ProbeScreenState();
}

class _ProbeScreenState extends State<ProbeScreen> {
  Map<String, dynamic>? _st;
  List<Map<String, dynamic>> _results = [];
  List<Map<String, dynamic>> _events = [];
  List<Map<String, dynamic>> _reboots = [];
  String? _lastRebootStep;
  StreamSubscription<Map<String, dynamic>>? _sub;
  Timer? _poll;
  Timer? _tick;
  String? _lastPhase;
  String? _lastResultId;
  bool _busy = false;
  String? _mkMsg;
  String? _syncMsg;

  @override
  void initState() {
    super.initState();
    _refreshStatus();
    _refreshLists();
    try {
      _sub = ProbeChannel.events().listen(_onStatus, onError: (_) {});
    } catch (_) {}
    // Por si el stream se corta.
    _poll = Timer.periodic(const Duration(seconds: 5), (_) => _refreshStatus());
    // Para que "tiempo en la fase" avance.
    _tick = Timer.periodic(const Duration(seconds: 1), (_) {
      if (mounted) setState(() {});
    });
  }

  @override
  void dispose() {
    _sub?.cancel();
    _poll?.cancel();
    _tick?.cancel();
    super.dispose();
  }

  Future<void> _refreshStatus() async {
    final s = await ProbeChannel.status();
    if (s != null) _onStatus(s);
  }

  void _onStatus(Map<String, dynamic> s) {
    if (!mounted) return;
    final phase = s['phase'] as String?;
    final rid = (s['last_result'] as Map?)?['result_id'] as String?;
    final rstep = (s['reboot'] as Map?)?['step'] as String?;
    final changed = phase != _lastPhase || rid != _lastResultId || rstep != _lastRebootStep;
    _lastPhase = phase;
    _lastResultId = rid;
    _lastRebootStep = rstep;
    setState(() => _st = s);
    if (changed) _refreshLists();
  }

  Future<void> _refreshLists() async {
    try {
      final r = await ProbeChannel.recentResults(limit: 10);
      final e = await ProbeChannel.recentEvents(limit: 100);
      final rb = await ProbeChannel.recentReboots(limit: 5);
      if (!mounted) return;
      setState(() {
        _results = r;
        _events = e;
        _reboots = rb;
      });
    } catch (_) {}
  }

  void _snack(String msg) {
    if (!mounted) return;
    ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(msg)));
  }

  String _err(Object e) => e is PlatformException ? (e.message ?? e.code) : e.toString();

  Future<void> _run(Future<void> Function() f) async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      await f();
    } catch (e) {
      _snack(_err(e));
    } finally {
      if (mounted) setState(() => _busy = false);
      _refreshStatus();
      _refreshLists();
    }
  }

  Future<void> _toggle(bool on) => _run(() async {
        if (on) {
          var perm = await Geolocator.checkPermission();
          if (perm == LocationPermission.denied) perm = await Geolocator.requestPermission();
          if (perm == LocationPermission.denied || perm == LocationPermission.deniedForever) {
            throw Exception('La sonda necesita el permiso de ubicación (Android no deja arrancar el servicio sin él).');
          }
          await ProbeChannel.start();
        } else {
          await ProbeChannel.stop();
        }
      });

  Future<void> _runNow(String target) => _run(() async {
        await ProbeChannel.runNow(target);
        _snack(target == 'wifi' ? 'Medición por WiFi en cola' : 'Medición en $target en cola');
      });

  Future<void> _testMikrotik() => _run(() async {
        final r = await ProbeChannel.testMikrotik();
        setState(() => _mkMsg = r['ok'] == true
            ? 'OK: ${r['identity'] ?? '?'} ${r['version'] ?? ''} · regla en ${(r['rule'] as Map?)?['table'] ?? '?'}'
            : 'Falló: ${r['error']}');
      });

  Future<void> _restore() => _run(() async {
        final r = await ProbeChannel.restoreFallback();
        setState(() => _mkMsg = r['ok'] == true ? 'Regla en ${r['table']}' : 'No se pudo: ${r['error']}');
      });

  Future<void> _sync() => _run(() async {
        final r = await ProbeChannel.syncNow();
        setState(() => _syncMsg = r['error'] == null
            ? 'Subidos: ${r['uploaded']} · rechazados: ${r['rejected']}'
            : 'Subidos: ${r['uploaded']} · error: ${r['error']}');
      });

  Future<void> _openSettings() async {
    await Navigator.of(context).push(MaterialPageRoute(builder: (_) => ProbeSettingsScreen(general: widget.general)));
    _refreshStatus();
  }

  Map<String, dynamic> _m(String k) => (_st?[k] as Map?)?.cast<String, dynamic>() ?? const {};

  @override
  Widget build(BuildContext context) {
    final st = _st;
    return Scaffold(
      appBar: AppBar(
        title: const Text('Sonda A/B'),
        actions: [
          IconButton(icon: const Icon(Icons.settings), tooltip: 'Ajustes de la sonda', onPressed: _openSettings),
        ],
      ),
      body: st == null
          ? const Center(child: CircularProgressIndicator())
          : RefreshIndicator(
              onRefresh: () async {
                await _refreshStatus();
                await _refreshLists();
              },
              child: ListView(
                padding: const EdgeInsets.fromLTRB(12, 8, 12, 32),
                children: [
                  ..._alerts(),
                  _phaseCard(),
                  _routersCard(),
                  _routeCard(),
                  _netCard(),
                  _gpsCard(),
                  _queueCard(),
                  _runCard(),
                  _resultsCard(),
                  _logCard(),
                ],
              ),
            ),
    );
  }

  // ---------------------------------------------------------------- alertas

  List<Widget> _alerts() {
    final list = (_st?['alerts'] as List?) ?? const [];
    return list.whereType<Map>().map((a) {
      final code = a['code'] as String? ?? '';
      final isErr = a['level'] == 'error';
      Widget? action;
      switch (code) {
        case 'battery-optimized':
          action = TextButton(onPressed: () => ProbeChannel.openBatterySettings(), child: const Text('Quitar optimización'));
        case 'no-background-location':
          action = TextButton(onPressed: () => Geolocator.openAppSettings(), child: const Text('Abrir ajustes de la app'));
        case 'route-not-restored':
          action = TextButton(onPressed: _busy ? null : _restore, child: const Text('Volver a respaldo'));
        case 'backend-auth':
        case 'probe-not-configured':
          action = TextButton(onPressed: _openSettings, child: const Text('Abrir ajustes de la sonda'));
        case 'no-location-permission':
          action = TextButton(
              onPressed: () async {
                await Geolocator.requestPermission();
                _refreshStatus();
              },
              child: const Text('Dar permiso'));
      }
      return Card(
        color: isErr ? Colors.red.shade50 : Colors.amber.shade50,
        child: Padding(
          padding: const EdgeInsets.fromLTRB(12, 8, 8, 8),
          child: Row(
            children: [
              Icon(isErr ? Icons.error : Icons.warning_amber, color: isErr ? Colors.red : Colors.orange.shade800),
              const SizedBox(width: 8),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(a['msg'] as String? ?? code),
                    Text('desde ${ProbeText.ago(a['since'] as String?)}', style: Theme.of(context).textTheme.bodySmall),
                  ],
                ),
              ),
              if (action != null) action,
            ],
          ),
        ),
      );
    }).toList();
  }

  // ------------------------------------------------------------------ fase

  IconData _phaseIcon(String p) => switch (p) {
        'stopped' => Icons.stop_circle_outlined,
        'idle' => Icons.hourglass_empty,
        'waiting_ethernet' => Icons.settings_ethernet,
        'switching_route' || 'verifying_route' || 'restoring_route' => Icons.alt_route,
        'egress_check' => Icons.public,
        'gps_fix' => Icons.gps_fixed,
        'ping' => Icons.timer,
        'download' => Icons.download,
        'upload' => Icons.upload,
        'storing' => Icons.save,
        'uploading' => Icons.cloud_upload,
        'backoff' => Icons.cloud_off,
        'rebooting' => Icons.restart_alt,
        _ => Icons.sync,
      };

  String _since(String? iso) {
    final t = iso == null ? null : DateTime.tryParse(iso);
    if (t == null) return '';
    final s = DateTime.now().difference(t).inSeconds;
    if (s < 0) return '';
    return s < 60 ? '$s s' : '${s ~/ 60} min ${s % 60} s';
  }

  Widget _phaseCard() {
    final st = _st!;
    final phase = st['phase'] as String? ?? 'stopped';
    final enabled = st['enabled'] == true;
    final running = st['running'] == true;
    final detail = st['phase_detail'] as String?;
    final o = (st['current_order'] as Map?)?.cast<String, dynamic>();
    final stepIdx = ProbeText.steps.indexWhere((s) => s[0] == phase);
    final color = running ? (phase == 'idle' ? Colors.blue : Colors.green) : Colors.grey;
    String? orderLine;
    if (o != null && o['type'] == 'reboot_router') {
      final label = o['label'] as String?;
      orderLine = 'Reiniciando router ${o['slot'] ?? o['target']}${label != null ? ' ($label)' : ''}'
          '${o['requested_by'] != null ? ' · pedido por ${o['requested_by']}' : ''}';
    } else if (o != null) {
      final slot = o['slot'] as String?;
      final label = o['label'] as String?;
      final table = o['routing_table'] as String?;
      final reason = ProbeText.reason(o['selection_reason'] as String?);
      if (o['net_path'] == 'wifi') {
        orderLine = 'Midiendo por WiFi (no se atribuye a ningún router) · $reason';
      } else if (slot != null) {
        orderLine = 'Midiendo router $slot${label != null ? ' ($label)' : ''}'
            '${table != null ? ' · tabla $table' : ''} · $reason';
      } else {
        orderLine = 'Orden ${o['target']} · $reason';
      }
      if ((o['attempt'] as num? ?? 1) > 1) orderLine += ' · intento ${o['attempt']}';
    }
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Row(
              children: [
                const Expanded(child: Text('Sonda activa', style: TextStyle(fontWeight: FontWeight.bold, fontSize: 16))),
                Switch(value: enabled, onChanged: _busy ? null : _toggle),
              ],
            ),
            if (enabled && !running)
              const Text('El servicio no está corriendo; se relanza solo en unos minutos.',
                  style: TextStyle(color: Colors.orange)),
            const SizedBox(height: 8),
            Row(
              children: [
                Icon(_phaseIcon(phase), size: 40, color: color),
                const SizedBox(width: 12),
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text(
                        '${ProbeText.phase(phase)}${phase == 'download' || phase == 'upload' ? '…' : ''}'
                        '${detail != null ? ' $detail' : ''}',
                        style: Theme.of(context).textTheme.titleLarge,
                      ),
                      if (running && st['phase_since'] != null)
                        Text('hace ${_since(st['phase_since'] as String?)} en esta fase',
                            style: Theme.of(context).textTheme.bodySmall),
                    ],
                  ),
                ),
              ],
            ),
            if (orderLine != null) ...[
              const SizedBox(height: 8),
              Text(orderLine, style: const TextStyle(fontWeight: FontWeight.w500)),
            ],
            const SizedBox(height: 10),
            if (phase != 'rebooting')
            Wrap(
              spacing: 4,
              runSpacing: 4,
              children: [
                for (var i = 0; i < ProbeText.steps.length; i++)
                  Container(
                    padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 3),
                    decoration: BoxDecoration(
                      color: i == stepIdx
                          ? Colors.green
                          : (stepIdx >= 0 && i < stepIdx ? Colors.green.shade100 : Colors.grey.shade200),
                      borderRadius: BorderRadius.circular(4),
                    ),
                    child: Text(ProbeText.steps[i][1],
                        style: TextStyle(fontSize: 12, color: i == stepIdx ? Colors.white : Colors.black87)),
                  ),
              ],
            ),
          ],
        ),
      ),
    );
  }

  // ------------------------------------------------- salud de los routers

  /// Salud de A y B (pings del MikroTik, §6.2), el reinicio en curso con sus
  /// pasos y los últimos reinicios pedidos por el backend.
  Widget _routersCard() {
    final routers = _m('routers');
    final reboot = (_st?['reboot'] as Map?)?.cast<String, dynamic>();
    final slots = routers.keys.toList()..sort();
    return _section('Salud de los routers', [
      if (slots.isEmpty)
        const Text('Sin verificar todavía (se mide en reposo, cada lectura del MikroTik).',
            style: TextStyle(color: Colors.black54)),
      for (final slot in slots) _healthRow(slot, (routers[slot] as Map?)?.cast<String, dynamic>() ?? const {}),
      if (reboot != null) ...[
        const Divider(),
        _rebootProgress(reboot),
      ],
      if (_reboots.isNotEmpty) ...[
        const Divider(),
        const Text('Reinicios pedidos', style: TextStyle(fontWeight: FontWeight.w500)),
        for (final r in _reboots) _rebootTile(r),
      ],
    ]);
  }

  Widget _healthRow(String slot, Map<String, dynamic> h) {
    final ok = h['internet_ok'];
    final gw = h['gateway_ok'];
    final down = h['down_since'] as String?;
    final Color color = ok == true ? Colors.green.shade700 : (ok == false ? Colors.red : Colors.grey);
    String main;
    if (ok == true) {
      main = 'con Internet · ${ProbeText.num1(h['rtt_ms'])} ms'
          '${(h['loss_pct'] as num? ?? 0) > 0 ? ' · pérdida ${ProbeText.num1(h['loss_pct'])} %' : ''}';
    } else if (ok == false) {
      main = 'SIN Internet${down != null ? ' desde hace ${ProbeText.duration(down)}' : ''}'
          '${gw == false ? ' · la puerta de enlace no responde (¿apagado?)' : (gw == true ? ' · la puerta de enlace responde' : '')}';
    } else {
      main = 'sin dato';
    }
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 3),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Icon(Icons.circle, size: 14, color: color),
          const SizedBox(width: 8),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text('Router $slot: $main', style: TextStyle(color: color, fontWeight: FontWeight.w500)),
                Text(
                    '${h['health_ip'] != null ? 'ping a ${h['health_ip']}' : 'sin IP de salud'}'
                    '${h['gateway'] != null ? ' · gateway ${h['gateway']}' : ''}'
                    ' · verificado ${ProbeText.ago(h['checked_at'] as String?)}',
                    style: Theme.of(context).textTheme.bodySmall),
                if (h['error'] != null)
                  Text('${h['error']}', style: const TextStyle(color: Colors.orange, fontSize: 12)),
              ],
            ),
          ),
        ],
      ),
    );
  }

  Widget _rebootProgress(Map<String, dynamic> r) {
    final raw = r['step'] as String? ?? 'received';
    // reading_gateway es parte de "Recibida" (todavía no hay SSH).
    final step = raw == 'reading_gateway' ? 'received' : raw;
    final failed = step == 'failed';
    var idx = ProbeText.rebootSteps.indexWhere((s) => s[0] == step);
    if (failed) idx = -1;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text('Reiniciando router ${r['slot']}${r['label'] != null ? ' (${r['label']})' : ''}'
            '${r['gateway_ip'] != null ? ' · ${r['gateway_ip']}' : ''}',
            style: const TextStyle(fontWeight: FontWeight.w500)),
        const SizedBox(height: 4),
        Wrap(spacing: 4, runSpacing: 4, children: [
          for (var i = 0; i < ProbeText.rebootSteps.length; i++)
            Container(
              padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 3),
              decoration: BoxDecoration(
                color: i == idx ? Colors.deepOrange : (idx >= 0 && i < idx ? Colors.orange.shade100 : Colors.grey.shade200),
                borderRadius: BorderRadius.circular(4),
              ),
              child: Text(ProbeText.rebootSteps[i][1],
                  style: TextStyle(fontSize: 12, color: i == idx ? Colors.white : Colors.black87)),
            ),
        ]),
        if (r['elapsed_s'] != null)
          Text('${r['elapsed_s']} s desde el comando'
              '${r['gateway_back_s'] != null ? ' · volvió a los ${r['gateway_back_s']} s' : ''}'
              '${r['internet_back_s'] != null ? ' · Internet a los ${r['internet_back_s']} s' : ''}',
              style: Theme.of(context).textTheme.bodySmall),
        if (r['error'] != null)
          Text('Error: ${ProbeText.rebootError(r['error'] as String?)}', style: const TextStyle(color: Colors.red, fontSize: 12)),
      ],
    );
  }

  Widget _rebootTile(Map<String, dynamic> r) {
    final status = r['status'] as String?;
    final res = (r['result'] as Map?)?.cast<String, dynamic>();
    final Color color = switch (status) {
      'done' => Colors.green.shade700,
      'failed' || 'interrupted' => Colors.red,
      'running' || 'delivered' || 'pending' => Colors.deepOrange,
      _ => Colors.grey,
    };
    final back = res == null
        ? ''
        : '${res['gateway_back_s'] != null ? ' · volvió a los ${res['gateway_back_s']} s' : ''}'
            '${res['internet_back_s'] != null ? ' · Internet a los ${res['internet_back_s']} s' : ''}';
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 3),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('${ProbeText.hhmm(r['created_at'] as String?)} · router ${r['slot'] ?? '?'} · ${ProbeText.orderStatus(status)}$back',
              style: TextStyle(color: color, fontWeight: FontWeight.w500)),
          Text(
              '${r['requested_by'] != null ? 'pedido por ${r['requested_by']}' : 'pedido desde el backend'}'
              '${res?['host_key_fp'] != null ? ' · llave ${res!['host_key_fp']}' : ''}',
              style: Theme.of(context).textTheme.bodySmall),
          if (r['error'] != null)
            Text('Error: ${ProbeText.rebootError(r['error'] as String?)}'
                '${res?['ssh_detail'] != null ? ' (${res!['ssh_detail']})' : ''}',
                style: const TextStyle(color: Colors.red, fontSize: 12)),
        ],
      ),
    );
  }

  // ---------------------------------------------------------------- MikroTik

  Widget _section(String title, List<Widget> children) => Card(
        child: Padding(
          padding: const EdgeInsets.all(12),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(title, style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 15)),
              const SizedBox(height: 6),
              ...children,
            ],
          ),
        ),
      );

  Widget _kv(String k, String v, {Color? color}) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 1),
        child: Row(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            SizedBox(width: 130, child: Text(k, style: const TextStyle(color: Colors.black54))),
            Expanded(child: Text(v, style: TextStyle(color: color))),
          ],
        ),
      );

  Widget _routeCard() {
    final mk = _m('mikrotik');
    final routes = (mk['routes'] as Map?)?.cast<String, dynamic>() ?? const {};
    final reachable = mk['reachable'];
    final lines = <Widget>[];
    String? mainVia;
    routes.forEach((table, v) {
      final r = (v as Map?) ?? const {};
      final gw = r['gateway'] as String?;
      final active = r['active'] == true;
      final m = RegExp(r'^to-([A-Z])$').firstMatch(table);
      if (m != null) {
        final slot = m.group(1);
        lines.add(_kv('Router $slot', active ? 'enlace arriba · ${gw ?? '?'}' : 'enlace caído${gw != null ? ' ($gw)' : ''}',
            color: active ? Colors.green.shade800 : Colors.red));
      } else if (active && gw != null) {
        // Respaldo: por cuál router sale hoy (según la interfaz del gateway).
        routes.forEach((t2, v2) {
          final g2 = (v2 as Map?)?['gateway'] as String?;
          final m2 = RegExp(r'^to-([A-Z])$').firstMatch(t2);
          if (m2 != null && g2 != null && g2.contains('%') && gw.contains('%') && g2.split('%').last == gw.split('%').last) {
            mainVia = m2.group(1);
          }
        });
        mainVia ??= gw;
      }
    });
    return _section('Ruta (MikroTik)', [
      _kv('Alcanzable', reachable == null ? 'sin leer' : (reachable == true ? 'sí' : 'no'),
          color: reachable == false ? Colors.red : null),
      _kv('Tabla actual', mk['current_table'] == null ? 'sin leer' : ProbeText.table(mk['current_table'] as String?)),
      ...lines,
      if (mainVia != null) _kv('Respaldo', 'sale hoy por $mainVia'),
      if (mk['identity'] != null) _kv('Equipo', '${mk['identity']} ${mk['version'] ?? ''}'),
      if (mk['error'] != null) _kv('Último error', '${mk['error']}', color: Colors.red),
      _kv('Última lectura', mk['last_ok'] == null ? '-' : '${ProbeText.hhmm(mk['last_ok'] as String?)} (${ProbeText.ago(mk['last_ok'] as String?)})'),
      if (_mkMsg != null) Padding(padding: const EdgeInsets.only(top: 4), child: Text(_mkMsg!)),
      Wrap(spacing: 8, children: [
        OutlinedButton(onPressed: _busy ? null : _testMikrotik, child: const Text('Probar MikroTik')),
        OutlinedButton(onPressed: _busy ? null : _restore, child: const Text('Volver a respaldo')),
      ]),
    ]);
  }

  // -------------------------------------------------------------------- red

  Widget _netCard() {
    final n = _m('net');
    final eth = (n['ethernet'] as Map?) ?? const {};
    final wifi = (n['wifi'] as Map?) ?? const {};
    final req = n['require_ethernet'] == true;
    final ethUp = eth['up'] == true;
    return _section('Red', [
      _kv('Ethernet', ethUp ? 'arriba · ${eth['ip'] ?? 'sin IP'}${eth['iface'] != null ? ' (${eth['iface']})' : ''}' : 'sin cable',
          color: ethUp ? Colors.green.shade800 : (req ? Colors.red : null)),
      _kv('WiFi', wifi['up'] == true ? 'arriba · ${wifi['ip'] ?? 'sin IP'}' : 'apagado'),
      _kv('Red por defecto', '${n['default_network'] ?? '-'}'),
      _kv('Camino al backend', '${n['control_path'] ?? '-'}'),
      _kv('Exigir Ethernet', req ? 'sí' : 'no'),
      if (req && !ethUp)
        const Text('Se exige Ethernet y no hay cable: las órdenes esperan.', style: TextStyle(color: Colors.red)),
    ]);
  }

  // -------------------------------------------------------------------- GPS

  Widget _gpsCard() {
    final g = _m('gps');
    final status = g['status'] as String?;
    final color = switch (status) { 'fresh' => Colors.green.shade800, 'stale' => Colors.orange.shade800, _ => Colors.grey };
    return _section('GPS (de la última prueba)', [
      _kv('Estado', switch (status) { 'fresh' => 'Fresco', 'stale' => 'Viejo', _ => 'Sin fijo' }, color: color),
      if (g['age_s'] != null) _kv('Edad', '${ProbeText.num1(g['age_s'])} s'),
      if (g['accuracy_m'] != null) _kv('Precisión', '±${ProbeText.num1(g['accuracy_m'])} m'),
      if (g['source'] != null) _kv('Fuente', '${g['source']}'),
      if (g['satellites_used'] != null) _kv('Satélites', '${g['satellites_used']}'),
      if (g['at'] != null) _kv('Hora del fijo', ProbeText.hhmm(g['at'] as String?)),
    ]);
  }

  // ------------------------------------------------------------------- cola

  Widget _queueCard() {
    final q = _m('queue');
    final b = _m('backend');
    final lp = _m('local_plan');
    final skew = b['clock_skew_s'];
    return _section('Cola', [
      _kv('Órdenes', '${q['orders_pending'] ?? 0} pendientes'
          '${q['next_order_at'] != null ? ' · próxima ${q['next_order_type'] == 'reboot_router' ? 'reiniciar ' : ''}${q['next_order_target'] ?? ''} a las ${ProbeText.hhmm(q['next_order_at'] as String?)}' : ''}'),
      _kv('Resultados', '${q['results_pending'] ?? 0} por subir · ${q['results_rejected'] ?? 0} rechazados'),
      _kv('Estados por mandar', '${q['outbox_pending'] ?? 0}'),
      _kv('Backend', '${b['url'] ?? ''}'),
      _kv('Alcanzable', b['reachable'] == null ? 'sin probar' : (b['reachable'] == true ? 'sí' : 'no'),
          color: b['reachable'] == false ? Colors.red : null),
      _kv('Último contacto', b['last_ok'] == null ? 'nunca' : ProbeText.ago(b['last_ok'] as String?)),
      if (b['last_error'] != null) _kv('Último error', '${b['last_error']}', color: Colors.red),
      if (skew is num && skew.abs() > 5) _kv('Desfase de reloj', '${ProbeText.num1(skew)} s', color: Colors.orange.shade800),
      if (lp['active'] == true)
        Text('Sin backend: plan local cada ${((lp['interval_s'] as num? ?? 900) / 60).round()} min'
            '${lp['next_at'] != null ? ' (próxima ${ProbeText.hhmm(lp['next_at'] as String?)})' : ''}',
            style: const TextStyle(color: Colors.orange)),
      if (_syncMsg != null) Text(_syncMsg!),
      OutlinedButton.icon(
          onPressed: _busy ? null : _sync, icon: const Icon(Icons.cloud_upload), label: const Text('Sincronizar ahora')),
    ]);
  }

  // ----------------------------------------------------------- medir ahora

  Widget _runCard() {
    final n = _m('net');
    final ethUp = (n['ethernet'] as Map?)?['up'] == true;
    final req = n['require_ethernet'] == true;
    final enabled = _st?['enabled'] == true;
    final can = enabled && !_busy;
    return _section('Medir ahora', [
      if (!enabled) const Text('Activa la sonda para medir.'),
      Wrap(spacing: 8, runSpacing: 4, children: [
        FilledButton(onPressed: can ? () => _runNow('A') : null, child: const Text('Medir A')),
        FilledButton(onPressed: can ? () => _runNow('B') : null, child: const Text('Medir B')),
        OutlinedButton(onPressed: can ? () => _runNow('next') : null, child: const Text('Siguiente')),
        if (!ethUp && !req)
          OutlinedButton.icon(
              onPressed: can ? () => _runNow('wifi') : null, icon: const Icon(Icons.wifi), label: const Text('Medir por WiFi')),
      ]),
    ]);
  }

  // ------------------------------------------------------------ resultados

  Widget _badge(String text, Color color) => Container(
        margin: const EdgeInsets.only(right: 4, top: 2),
        padding: const EdgeInsets.symmetric(horizontal: 5, vertical: 1),
        decoration: BoxDecoration(color: color.withValues(alpha: 0.15), borderRadius: BorderRadius.circular(4)),
        child: Text(text, style: TextStyle(fontSize: 11, color: color)),
      );

  Widget _resultsCard() {
    return _section('Últimos resultados', [
      if (_results.isEmpty) const Text('Sin resultados todavía.'),
      for (final r in _results) _resultTile(r),
    ]);
  }

  Widget _resultTile(Map<String, dynamic> r) {
    final ok = r['test_status'] == 'done';
    final wifi = r['net_path'] == 'wifi';
    final who = wifi ? 'WiFi' : 'Router ${r['slot'] ?? '?'}';
    final loc = r['location_status'] as String?;
    final sync = r['sync_status'] as String?;
    final verdict = r['server_reason'] != null ? ProbeText.serverReason(r['server_reason'] as String?) : null;
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('${ProbeText.hhmm((r['test_started_at'] ?? r['created_at']) as String?)} · $who · '
              '↓${ProbeText.num1(r['down_mbps'])} ↑${ProbeText.num1(r['up_mbps'])} Mbps · ping ${ProbeText.num1(r['ping_ms'])} ms',
              style: const TextStyle(fontWeight: FontWeight.w500)),
          Wrap(children: [
            _badge(ok ? 'COMPLETADA' : 'FALLIDA', ok ? Colors.green : Colors.red),
            _badge(
                '${ProbeText.gps(loc)}${r['gps_age_s'] != null ? ' ${ProbeText.num1(r['gps_age_s'])} s' : ''}',
                loc == 'fresh' ? Colors.green : (loc == 'stale' ? Colors.orange : Colors.grey)),
            _badge(ProbeText.sync(sync), sync == 'synced' ? Colors.blue : (sync == 'rejected' ? Colors.red : Colors.orange)),
            if (r['captive_portal'] == true) _badge('PORTAL CAUTIVO${r['portal_host'] != null ? ' ${r['portal_host']}' : ''}', Colors.purple),
            if (r['executed_offline'] == true) _badge('SIN BACKEND', Colors.brown),
            if ((r['attempt'] as num? ?? 1) > 1) _badge('RESPALDO', Colors.teal),
          ]),
          if (r['egress_ip'] != null)
            Text('Salida ${r['egress_ip']}${r['egress_asn_client'] != null ? ' AS${r['egress_asn_client']}' : ''}'
                '${r['egress_as_org_client'] != null ? ' ${r['egress_as_org_client']}' : ''}',
                style: Theme.of(context).textTheme.bodySmall),
          if (verdict != null) Text('Servidor: $verdict', style: Theme.of(context).textTheme.bodySmall),
          if (r['error'] != null) Text('Error: ${r['error']}', style: const TextStyle(color: Colors.red, fontSize: 12)),
          if (sync == 'rejected' && r['sync_error'] != null)
            Text('Rechazo: ${r['sync_error']}', style: const TextStyle(color: Colors.red, fontSize: 12)),
        ],
      ),
    );
  }

  // --------------------------------------------------------------- registro

  Widget _logCard() {
    return Card(
      child: ExpansionTile(
        title: Text('Registro (${_events.length})', style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 15)),
        children: [
          for (final e in _events)
            ListTile(
              dense: true,
              visualDensity: VisualDensity.compact,
              leading: Icon(
                e['level'] == 'error' ? Icons.error : (e['level'] == 'warn' ? Icons.warning_amber : Icons.info_outline),
                size: 16,
                color: e['level'] == 'error' ? Colors.red : (e['level'] == 'warn' ? Colors.orange : Colors.blueGrey),
              ),
              title: Text('${e['msg']}', style: const TextStyle(fontSize: 13)),
              subtitle: Text('${ProbeText.hhmm(e['at'] as String?)}${e['phase'] != null ? ' · ${ProbeText.phase(e['phase'] as String?)}' : ''}',
                  style: const TextStyle(fontSize: 11)),
            ),
        ],
      ),
    );
  }
}
