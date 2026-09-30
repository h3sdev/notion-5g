#!/bin/bash
# Prueba de punta a punta de la sonda A/B (forma A) con el backend LOCAL en Docker.
# Ver server/docs/HANDOVER.md, addendum 2026-09-29, y CONTRATO-SONDA-AB.md §5.
#
# Uso (desde la WSL):
#   probe_e2e.sh install <ip:puerto>   conecta adb (depuración inalámbrica), instala el APK
#                                      release con install -r (conserva datos y permisos),
#                                      muestra los servicios antes/después y toma captura
#   probe_e2e.sh order [A|B|next]      crea una orden desde el "dashboard" dos veces con el
#                                      mismo order_id (la 2.a debe dar existing:true) y la
#                                      sigue hasta que se cierre
#   probe_e2e.sh status                estado en vivo que mandó el celular
#   probe_e2e.sh results               últimos resultados del celular en el backend
#
# Nunca escribe en producción: todo va a http://localhost:8080.
set -u
REPO=/home/ubudev/notion-5g
OUT=${OUT:-/tmp/probe_e2e}
ADB="/mnt/c/Users/diego/AppData/Local/Android/Sdk/platform-tools/adb.exe -P 5039"
APK='C:\dev\notion5g_mobile\build\app\outputs\flutter-apk\app-release.apk'
PKG=com.h3s.notion5g.notion5g_field
KEY=$(grep '^API_KEY=' $REPO/server/.env | cut -d= -f2)
PROBE=${PROBE:-hap-oficina}
BASE=http://localhost:8080/api/v1
API=$BASE/probes/$PROBE
mkdir -p "$OUT"
cd /mnt/c  # adb.exe de Windows se queja si el cwd es una ruta de la WSL

j() { python3 -m json.tool 2>/dev/null || cat; }

order() {
  local target=${1:-A}
  OID=$(python3 -c 'import uuid;print(uuid.uuid4())')
  echo "== Prueba en $target: $OID"
  for i in 1 2; do
    curl -s -H "X-API-Key: $KEY" -X POST "$API/orders" \
      -d "{\"order_id\":\"$OID\",\"target\":\"$target\",\"requested_by\":\"e2e-local\"}" | head -c 300; echo
  done
  for i in $(seq 1 60); do
    st=$(curl -s -H "X-API-Key: $KEY" "$API/orders?view=history&limit=10" |
      python3 -c "import json,sys;o=[x for x in json.load(sys.stdin)['orders'] if x['order_id']=='$OID'];o=o[0] if o else {};print(o.get('status'),o.get('result_id'),o.get('measurement_id'),o.get('error'))")
    echo "$(date -u +%T) $st"
    case "$st" in done*|failed*|expired*|interrupted*|cancelled*) break;; esac
    sleep 10
  done
  results
}

results() {
  curl -s -H "X-API-Key: $KEY" "$BASE/measurements?probe_id=$PROBE&limit=5" |
    python3 -c "
import json,sys
d=json.load(sys.stdin); d=d if isinstance(d,list) else d.get('measurements',d.get('items',[]))
for m in d:
    r=m.get('raw') or {}
    print(m.get('ts'), m.get('device_id'), 'down', m.get('down_mbps'), 'up', m.get('up_mbps'), 'ping', m.get('ping_ms'),
          m.get('net_route'), m.get('reason') or r.get('reason'))
" 2>/dev/null || curl -s -H "X-API-Key: $KEY" "$BASE/measurements?probe_id=$PROBE&limit=5" | head -c 2000
}

case "${1:-}" in
  install)
    [ -n "${2:-}" ] && $ADB connect "$2"
    $ADB devices
    echo "== antes: servicios"; $ADB shell dumpsys activity services $PKG | grep -E "ServiceRecord|isForeground"
    $ADB install -r "$APK"
    sleep 8
    echo "== después (BeaconService debe volver solo por MY_PACKAGE_REPLACED)"
    $ADB shell dumpsys activity services $PKG | grep -E "ServiceRecord|isForeground"
    $ADB shell dumpsys package $PKG | grep -E "versionName|lastUpdateTime"
    $ADB shell am start -n $PKG/.MainActivity
    sleep 4; $ADB exec-out screencap -p > "$OUT/after_install.png"
    echo "Captura en $OUT/after_install.png. Configura la sonda en la app (Sonda A/B > engranaje) y luego: $0 order A"
    ;;
  order) order "${2:-A}" ;;
  status) curl -s -H "X-API-Key: $KEY" "$API/status" | j ;;
  results) results ;;
  *) sed -n 2,16p "$0"; exit 1 ;;
esac
