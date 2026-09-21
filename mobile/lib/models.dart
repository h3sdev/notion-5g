/// Modelos de datos que espejan lo que expone el backend (server/internal/api).
library;

class Measurement {
  final Map<String, dynamic> raw;
  Measurement(this.raw);

  String? get ts => raw['ts'] as String?;
  String? get operator_ => raw['operator'] as String?;
  String? get rat => raw['rat'] as String?;
  num? get bandLte => raw['band_lte'] as num?;
  num? get rsrpDbm => raw['rsrp_dbm'] as num?;
  num? get downMbps => raw['down_mbps'] as num?;
  num? get upMbps => raw['up_mbps'] as num?;
  double? get lat => (raw['lat'] as num?)?.toDouble();
  double? get lon => (raw['lon'] as num?)?.toDouble();
  String? get source => raw['source'] as String?;

  factory Measurement.fromJson(Map<String, dynamic> json) => Measurement(json);
}

class CommandStatus {
  final Map<String, dynamic> raw;
  CommandStatus(this.raw);

  int get id => (raw['id'] as num).toInt();
  String get status => raw['status'] as String? ?? 'pending';
  String get type => raw['type'] as String? ?? '';
  String get deviceId => raw['device_id'] as String? ?? '';
  String? get createdAt => raw['created_at'] as String?;

  factory CommandStatus.fromJson(Map<String, dynamic> json) => CommandStatus(json);
}
