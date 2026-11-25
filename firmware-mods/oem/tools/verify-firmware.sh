#!/bin/bash
# verify-firmware.sh - H3S check of the firmware delivered by the manufacturer (REQUIREMENTS.md F1-F5, files).
# Run on a unit freshly flashed with that firmware, BEFORE provisioning it:
#
#   NOTION_PASS=... firmware-mods/oem/tools/verify-firmware.sh
#
# Then provision and test it through the web UI only:
#   python3 firmware-mods/lib/test_provision_ui.py provision.json   (U3, U4: factory state -> provisioned)
#   python3 firmware-mods/omnicap-gw/tools/test_cgi.py              (U2)
#   python3 firmware-mods/lib/test_ui_modules.py --kick-mac <client> (U1, U4)
# Needs dist/MANIFEST.md5 from make-oem-package.sh of the package that was sent.
set -uo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
RSH="$HERE/../../scripts/rsh.sh"
M="$HERE/dist/MANIFEST.md5"
[ -f "$M" ] || { echo "missing $M: run oem/make-oem-package.sh for the delivered package" >&2; exit 2; }
fails=0
ok() { echo "OK   $1"; }
ko() { echo "FAIL $1"; fails=$((fails + 1)); }

echo "== files (MANIFEST.md5)"
bad=$("$RSH" 'md5sum -c - 2>/dev/null | grep -v ": OK$"' < "$M" || true)
[ -z "$bad" ] && ok "every delivered file has the expected content" || { ko "files differ or are missing:"; echo "$bad"; }

echo "== factory state"
out=$("$RSH" 'uci -q get tunvpn.main.enabled; uci -q get omnicapgw.main.enabled
for k in tunvpn.main.remote tunvpn.main.username tunvpn.main.password omnicapgw.main.portal_url omnicapgw.main.nas_identifier \
         omnicapgw.main.radius_server omnicapgw.main.radius_secret omnicapgw.main.login_host omnicapgw.main.agent_url omnicapgw.main.agent_token; do
	[ -n "$(uci -q get $k)" ] && echo "SET $k"; done
ls /data/tunvpn /data/omnicap-gw 2>/dev/null | grep -E "crt|key|auth|conf" | sed "s/^/DATA /"
iptables -S 2>/dev/null | grep -c OCGW')
[ "$(echo "$out" | sed -n 1p)" = 0 ] && [ "$(echo "$out" | sed -n 2p)" = 0 ] && ok "F1 both features disabled" || ko "F1 not disabled"
echo "$out" | grep -q '^SET ' && ko "F1 provisioning values present: $(echo "$out" | grep '^SET ' | tr '\n' ' ')" || ok "F1 no provisioning values"
echo "$out" | grep -q '^DATA ' && ko "F2 provisioned files present" || ok "F2 no certificates or credentials"
[ "$(echo "$out" | tail -1)" = 0 ] && ok "F3 no captive portal rules (open network)" || ko "F3 captive portal rules present"

echo "== TUN driver (F4)"
t=$("$RSH" 'grep -q " tun$" /proc/misc && echo builtin-or-loaded; ls /lib/modules/$(uname -r)/tun.ko 2>/dev/null; zcat /proc/config.gz 2>/dev/null | grep "^CONFIG_TUN="')
if echo "$t" | grep -q -e 'tun.ko' -e builtin -e 'CONFIG_TUN='; then ok "F4 kernel TUN driver available ($t)"; else ko "F4 no kernel TUN driver (CONFIG_TUN missing; tunlite.ko only works on the 2026-08-19 kernel)"; fi
echo "== ebtables for port selection (F5)"
e=$("$RSH" 'ebtables -t nat -L >/dev/null 2>&1 && echo ebtables; K=/lib/modules/$(uname -r); for m in ebt_mark xt_mark; do lsmod | grep -q "^$m " && echo $m || { [ -f $K/$m.ko ] && echo $m; }; done' | tr '\n' ' ')
case "$e" in *ebtables*ebt_mark*xt_mark*) ok "F5 ebtables + ebt_mark + xt_mark ($e)" ;; *) ko "F5 missing for port selection: need ebtables, ebt_mark, xt_mark (found: $e)" ;; esac

echo "== menu and services"
m=$("$RSH" 'grep -c -e "mMgmtVPN" -e "mCaptivePortal" /system/etc/www/xml/ui_mifi.xml; grep -c -e "/usr/sbin/tunvpn.sh check" -e "/usr/sbin/ocgw.sh check" /etc/crontabs/root; /usr/sbin/openvpn --version | head -1')
[ "$(echo "$m" | sed -n 1p)" = 2 ] && ok "U1 two menu entries" || ko "U1 menu entries"
[ "$(echo "$m" | sed -n 2p)" = 2 ] && ok "boot/watchdog cron lines" || ko "cron lines"
echo "$m" | grep -q 'OpenVPN 2.6' && ok "openvpn runs on the device" || ko "openvpn does not run"

[ $fails = 0 ] && echo "ALL OK" || echo "$fails CHECK(S) FAILED"
exit $fails
