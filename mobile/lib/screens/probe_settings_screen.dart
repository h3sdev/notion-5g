import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../probe_channel.dart';
import '../settings_store.dart';

/// Ajustes de la sonda A/B (contrato §2.7). Los guarda el lado nativo
/// (SharedPreferences "probe"); con el servicio corriendo se toman al inicio
/// del siguiente ciclo, nunca a mitad de una prueba.
class ProbeSettingsScreen extends StatefulWidget {
  final AppSettings general;
  const ProbeSettingsScreen({super.key, required this.general});

  @override
  State<ProbeSettingsScreen> createState() => _ProbeSettingsScreenState();
}

class _ProbeSettingsScreenState extends State<ProbeSettingsScreen> {
  static const _secret = {'api_key', 'mikrotik_password', 'prod_api_key', 'ssh_password_A', 'ssh_password_B'};
  static const _ints = {
    'mikrotik_port',
    'duration_s',
    'max_mb_per_phase',
    'mikrotik_idle_check_s',
    'poll_interval_s',
    'status_interval_s',
    'local_plan_interval_s',
    'offline_after_s',
    'location_fix_timeout_s',
    'location_max_age_s',
    'location_max_age_stationary_s',
    'location_max_accuracy_m',
    'ssh_port_A',
    'ssh_port_B',
  };
  static const _bools = {'require_ethernet', 'flush_conntrack', 'local_plan_enabled'};

  Map<String, dynamic>? _cfg;
  final _ctrl = <String, TextEditingController>{};
  final _bool = <String, bool>{};
  String _speedTarget = 'cloudflare';
  bool _saving = false;
  bool _testing = false;
  String? _testMsg;
  bool _testOk = false;

  @override
  void initState() {
    super.initState();
    _load();
  }

  @override
  void dispose() {
    for (final c in _ctrl.values) {
      c.dispose();
    }
    super.dispose();
  }

  Future<void> _load() async {
    Map<String, dynamic> cfg;
    try {
      cfg = await ProbeChannel.getConfig();
    } catch (e) {
      cfg = {};
    }
    if (!mounted) return;
    setState(() {
      _cfg = cfg;
      cfg.forEach((k, v) {
        if (k == 'enabled') return;
        if (_bools.contains(k)) {
          _bool[k] = v == true;
        } else if (k == 'speed_target') {
          _speedTarget = (v as String?) ?? 'cloudflare';
        } else {
          _ctrl[k] = TextEditingController(text: v?.toString() ?? '');
        }
      });
      // La API key de Ajustes generales es la de producción: sirve solo para
      // la descarga de solo lectura (prod-download), nunca para el backend local.
      final prodKey = _ctrl['prod_api_key'];
      if (prodKey != null && prodKey.text.isEmpty && widget.general.backendUrl.contains('notion.h3s-iot.com')) {
        prodKey.text = widget.general.apiKey;
      }
    });
  }

  Map<String, dynamic> _collect() {
    final out = <String, dynamic>{};
    _ctrl.forEach((k, c) {
      // Las claves SSH se guardan tal cual (un espacio puede ser parte de la clave).
      final t = k.startsWith('ssh_password_') ? c.text : c.text.trim();
      out[k] = _ints.contains(k) ? (int.tryParse(t) ?? t) : t;
    });
    _bool.forEach((k, v) => out[k] = v);
    out['speed_target'] = _speedTarget;
    return out;
  }

  Future<bool> _save({bool pop = true}) async {
    setState(() => _saving = true);
    try {
      await ProbeChannel.saveConfig(_collect());
      if (pop && mounted) Navigator.of(context).pop(true);
      return true;
    } on PlatformException catch (e) {
      _snack(e.message ?? e.code);
      return false;
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  Future<void> _testBackend() async {
    if (!await _save(pop: false)) return;
    setState(() {
      _testing = true;
      _testMsg = null;
    });
    try {
      final r = await ProbeChannel.testBackend();
      setState(() {
        _testOk = r['ok'] == true;
        _testMsg = _testOk
            ? 'OK por ${r['path']}: la sonda existe y la ejecuta un celular (runner=${r['runner']})'
            : 'Falló${r['path'] != null ? ' (por ${r['path']})' : ''}: ${r['error'] ?? 'HTTP ${r['status']}'}';
      });
    } on PlatformException catch (e) {
      setState(() {
        _testOk = false;
        _testMsg = e.message ?? e.code;
      });
    } finally {
      if (mounted) setState(() => _testing = false);
    }
  }

  void _snack(String msg) {
    if (!mounted) return;
    ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(msg)));
  }

  Widget _field(String key, String label, {String? hint, String? help}) {
    final c = _ctrl[key];
    if (c == null) return const SizedBox.shrink();
    return Padding(
      padding: const EdgeInsets.only(bottom: 10),
      child: TextField(
        controller: c,
        obscureText: _secret.contains(key),
        keyboardType: _ints.contains(key) ? TextInputType.number : (key.endsWith('_url') ? TextInputType.url : TextInputType.text),
        decoration: InputDecoration(labelText: label, hintText: hint, helperText: help, helperMaxLines: 3, border: const OutlineInputBorder(), isDense: true),
        onChanged: (_) => setState(() {}),
      ),
    );
  }

  Widget _switch(String key, String label, {String? help}) => SwitchListTile(
        contentPadding: EdgeInsets.zero,
        title: Text(label),
        subtitle: help != null ? Text(help) : null,
        value: _bool[key] ?? false,
        onChanged: (v) => setState(() => _bool[key] = v),
      );

  Widget _header(String t) => Padding(
        padding: const EdgeInsets.only(top: 16, bottom: 8),
        child: Text(t, style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 16)),
      );

  bool get _backendIsPrivate {
    final host = Uri.tryParse(_ctrl['backend_url']?.text.trim() ?? '')?.host ?? '';
    return RegExp(r'^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)').hasMatch(host);
  }

  String _dataEstimate() {
    final mb = int.tryParse(_ctrl['max_mb_per_phase']?.text ?? '') ?? 200;
    final iv = int.tryParse(_ctrl['local_plan_interval_s']?.text ?? '') ?? 900;
    if (iv <= 0) return '';
    final perDay = 86400 / iv;
    final gb = 2 * mb * perDay / 1000;
    return 'Consumo máximo estimado con una prueba cada ${(iv / 60).round()} min: '
        '${gb.toStringAsFixed(1).replaceAll('.', ',')} GB/día entre los routers '
        '(2 × $mb MB por prueba). Revisa el saldo de las SIM.';
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Ajustes de la sonda')),
      body: _cfg == null
          ? const Center(child: CircularProgressIndicator())
          : ListView(
              padding: const EdgeInsets.fromLTRB(16, 8, 16, 32),
              children: [
                _header('Backend'),
                _field('backend_url', 'URL del backend de la sonda', hint: 'http://192.168.40.22:8080',
                    help: 'El backend local se alcanza por el WiFi de la oficina.'),
                _field('api_key', 'API key del backend local (X-API-Key)',
                    help: 'La API_KEY de server/.env del backend local, no la de producción.'),
                _field('probe_id', 'Id de la sonda', hint: 'hap-oficina'),
                _field('phone_id', 'Id de este celular (measured_by)'),
                _field('poll_interval_s', 'Consultar órdenes cada (s)'),
                _field('status_interval_s', 'Mandar estado cada (s)'),
                OutlinedButton.icon(
                  onPressed: _testing || _saving ? null : _testBackend,
                  icon: _testing
                      ? const SizedBox(width: 16, height: 16, child: CircularProgressIndicator(strokeWidth: 2))
                      : const Icon(Icons.wifi_tethering),
                  label: const Text('Probar backend'),
                ),
                if (_testMsg != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 6),
                    child: Text(_testMsg!, style: TextStyle(color: _testOk ? Colors.green.shade800 : Colors.red)),
                  ),
                _header('MikroTik'),
                _field('mikrotik_host', 'Dirección del MikroTik', hint: '192.168.89.1'),
                _field('mikrotik_port', 'Puerto de la API (clásica)', hint: '8728'),
                _field('mikrotik_user', 'Usuario', hint: 'phone-probe'),
                _field('mikrotik_password', 'Contraseña'),
                _field('mikrotik_rule_comment', 'Comentario de la regla', hint: 'phone-probe'),
                _field('fallback_table', 'Tabla de respaldo', hint: 'main'),
                _field('table_A', 'Tabla de A (si no hay config del backend)', hint: 'to-A'),
                _field('table_B', 'Tabla de B (si no hay config del backend)', hint: 'to-B'),
                _field('mikrotik_idle_check_s', 'Leer la regla en reposo cada (s)'),
                _field('gateway_check', 'Chequeo de puerta de enlace (host:puerto)',
                    help: 'Vacío = el gateway que el MikroTik tiene para la tabla de la prueba, puerto 80.'),
                _switch('flush_conntrack', 'Limpiar conexiones rastreadas al cambiar la ruta'),
                _header('Reinicio de routers (SSH)'),
                const Text(
                    'El backend puede pedir reiniciar un router; el celular entra por SSH a su puerta de enlace '
                    'a través del MikroTik (por el cable, nunca por WiFi). La clave nunca sale del teléfono.',
                    style: TextStyle(color: Colors.black54)),
                const SizedBox(height: 8),
                for (final slot in const ['A', 'B']) ...[
                  Text('Router $slot', style: const TextStyle(fontWeight: FontWeight.w500)),
                  const SizedBox(height: 6),
                  _field('ssh_user_$slot', 'Usuario SSH', hint: 'root'),
                  _field('ssh_password_$slot', 'Clave SSH'),
                  _field('ssh_port_$slot', 'Puerto SSH', hint: '22'),
                  _field('ssh_command_$slot', 'Comando de reinicio', hint: 'reboot'),
                ],
                _header('Medición'),
                _switch('require_ethernet', 'Exigir Ethernet para medir',
                    help: 'Apagado: sin cable se mide por WiFi y el resultado no se atribuye a A ni a B.'),
                const SizedBox(height: 4),
                DropdownButtonFormField<String>(
                  initialValue: _speedTarget,
                  decoration: const InputDecoration(labelText: 'Destino de la prueba de velocidad', border: OutlineInputBorder(), isDense: true),
                  items: const [
                    DropdownMenuItem(value: 'cloudflare', child: Text('Cloudflare (speed.cloudflare.com)')),
                    DropdownMenuItem(value: 'fast', child: Text('fast.com (Netflix, 5 conexiones)')),
                    DropdownMenuItem(value: 'prod-download', child: Text('Producción (solo descarga)')),
                    DropdownMenuItem(value: 'local', child: Text('Backend local')),
                  ],
                  onChanged: (v) => setState(() => _speedTarget = v ?? 'cloudflare'),
                ),
                if (_speedTarget == 'local' && _backendIsPrivate)
                  const Padding(
                    padding: EdgeInsets.only(top: 6),
                    child: Text('El backend local no es alcanzable por Ethernet; se usará Cloudflare en las pruebas por cable.',
                        style: TextStyle(color: Colors.orange)),
                  ),
                if (_speedTarget == 'prod-download')
                  const Padding(
                    padding: EdgeInsets.only(top: 6),
                    child: Text('A producción solo se le descarga (lectura); la subida va a Cloudflare.',
                        style: TextStyle(color: Colors.black54)),
                  ),
                const SizedBox(height: 10),
                _field('prod_url', 'URL de producción (solo descarga)'),
                _field('prod_api_key', 'API key de producción',
                    help: 'Vacía = el destino "Producción" no se puede usar y se mide con Cloudflare.'),
                _field('duration_s', 'Duración de cada fase (s)', help: 'La orden manda si trae otra (3-60 s).'),
                _field('max_mb_per_phase', 'Tope por fase (MB)'),
                _header('GPS'),
                _field('location_fix_timeout_s', 'Esperar el fijo hasta (s)'),
                _field('location_max_age_s', 'Edad máxima para "fresco" (s)'),
                _field('location_max_age_stationary_s', 'Edad máxima quieto (s)'),
                _field('location_max_accuracy_m', 'Precisión mínima (m)'),
                _header('Plan local (sin backend)'),
                _switch('local_plan_enabled', 'Alternar A/B solo si no hay backend'),
                _field('local_plan_interval_s', 'Una prueba cada (s)',
                    help: 'Si el backend manda un intervalo mayor que 0, se usa ese.'),
                _field('offline_after_s', 'Considerar sin backend tras (s)'),
                Text(_dataEstimate(), style: Theme.of(context).textTheme.bodySmall),
                const SizedBox(height: 24),
                FilledButton(onPressed: _saving ? null : () => _save(), child: const Text('Guardar')),
              ],
            ),
    );
  }
}
