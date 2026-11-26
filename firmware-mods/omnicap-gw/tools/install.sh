#!/bin/bash
# install.sh - installs or updates the captive portal (omnicap-gw) and its web UI page on a running Notion 5G.
#
#   NOTION_PASS=... firmware-mods/omnicap-gw/tools/install.sh <omnicap-gw ARM binary>
#   NOTION_PASS=... firmware-mods/omnicap-gw/tools/install.sh --remove
#
# Settings are made in the web UI (Router > Advanced Settings > Captive portal) and stored in uci omnicapgw;
# an existing /system/etc/config/omnicapgw is never overwritten. Build the binary as described in README.md.
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
RSH="$REPO/scripts/rsh.sh"
. "$REPO/firmware-mods/lib/webui.sh"
D=/data/omnicap-gw
CRON="* * * * * $D/ocgw.sh check"

if [ "${1:-}" = "--remove" ]; then
	"$RSH" "[ -x $D/ocgw.sh ] && sh $D/ocgw.sh disable; grep -vF '$D/ocgw.sh' /etc/crontabs/root > /tmp/cr.new; cat /tmp/cr.new > /etc/crontabs/root; rm -f /tmp/cr.new; /etc/init.d/cron restart
uci -q delete firewall.ocgw; uci commit firewall
rm -rf $D $WWW/omnicapgw.cgi $WWW/html/router/omnicapgw.html $WWW/js/panel/router/omnicapgw.js /system/etc/config/omnicapgw"
	webui_remove mCaptivePortal js/panel/router/omnicapgw.js mCaptivePortal lt_OCGW_
	echo removed
	exit 0
fi

BIN="${1:?usage: install.sh <omnicap-gw ARM binary> | --remove}"
R="$HERE/rootfs/system/etc"
"$RSH" "mkdir -p $D && chmod 700 $D"
# /data is small (~15 MB, shared): never keep two copies of the 6 MB binary. Skip it when unchanged;
# otherwise stop the daemon and delete the old one before writing the new one.
if [ "$("$RSH" "md5sum $D/omnicap-gw 2>/dev/null | cut -d' ' -f1")" != "$(md5sum "$BIN" | cut -d' ' -f1)" ]; then
	"$RSH" "[ -x $D/ocgw.sh ] && sh $D/ocgw.sh stop; rm -f $D/omnicap-gw $D/omnicap-gw.tmp; true"
	"$RSH" "cat > $D/omnicap-gw && chmod 755 $D/omnicap-gw" < "$BIN" || { "$RSH" "rm -f $D/omnicap-gw"; echo "upload failed (space in /data?)" >&2; exit 1; }
fi
webui_put "$HERE/device/ocgw.sh" $D/ocgw.sh 755
webui_put "$R/www/omnicapgw.cgi" $WWW/omnicapgw.cgi 755
webui_put "$R/www/html/router/omnicapgw.html" $WWW/html/router/omnicapgw.html 644
webui_put "$R/www/js/panel/router/omnicapgw.js" $WWW/js/panel/router/omnicapgw.js 644
"$RSH" "[ -f /system/etc/config/omnicapgw ] || cat > /system/etc/config/omnicapgw" < "$R/config/omnicapgw"
webui_add mCaptivePortal objCaptivePortal js/panel/router/omnicapgw.js "$HERE/i18n" mCaptivePortal lt_OCGW_
# fw3 include: at boot and after every firewall reload (each 4G/5G change) the LAN stays closed until the
# daemon has its rules back (fail closed); ocgw.sh with no argument does nothing while the portal is disabled
"$RSH" "rm -f $D/config.json $D/enabled
uci set firewall.ocgw=include; uci set firewall.ocgw.type=script; uci set firewall.ocgw.path=$D/ocgw.sh
uci set firewall.ocgw.reload=1; uci set firewall.ocgw.family=any; uci commit firewall
grep -qF '$D/ocgw.sh check' /etc/crontabs/root || echo '$CRON' >> /etc/crontabs/root; /etc/init.d/cron restart
sh $D/ocgw.sh restart; sleep 3; sh $D/ocgw.sh status; tail -3 /tmp/omnicap-gw.log"
