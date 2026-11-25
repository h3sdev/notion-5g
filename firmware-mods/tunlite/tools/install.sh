#!/bin/bash
# install.sh - installs or updates tunlite.ko + OpenVPN (management tunnel) and its web UI page on a running
# Notion 5G over SSH.
#
#   NOTION_PASS=... firmware-mods/tunlite/tools/install.sh       install / update
#   NOTION_PASS=... firmware-mods/tunlite/tools/install.sh --remove
#
# Settings (server, user, password, CA, cipher) are made in the web UI (Router > Advanced Settings >
# Management VPN) and stored in uci tunvpn; tunvpn.sh generates the OpenVPN files from it. A unit that has
# the first version (client.conf + auth.txt written by hand) is migrated to uci with the same values.
# Binaries come from bin/ (built as described in BUILD.md).
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
RSH="$REPO/scripts/rsh.sh"
. "$REPO/firmware-mods/lib/webui.sh"
D=/data/tunvpn
CRON="* * * * * $D/tunvpn.sh check"

if [ "${1:-}" = "--remove" ]; then
	"$RSH" "[ -x $D/tunvpn.sh ] && sh $D/tunvpn.sh stop; rmmod tunlite 2>/dev/null
grep -vF '$D/tunvpn.sh' /etc/crontabs/root > /tmp/cr.new; cat /tmp/cr.new > /etc/crontabs/root; rm -f /tmp/cr.new; /etc/init.d/cron restart
rm -rf $D $WWW/tunvpn.cgi $WWW/html/router/tunvpn.html $WWW/js/panel/router/tunvpn.js /system/etc/config/tunvpn"
	webui_remove mMgmtVPN js/panel/router/tunvpn.js mMgmtVPN lt_TVPN_
	echo removed
	exit 0
fi

R="$HERE/rootfs/system/etc"
"$RSH" "mkdir -p $D && chmod 700 $D"
"$RSH" "[ -f $D/tunlite.ko ] && cmp -s $D/tunlite.ko /dev/null; true"
webui_put "$HERE/bin/tunlite.ko" $D/tunlite.ko.new 644
webui_put "$HERE/bin/openvpn" $D/openvpn.new 755
"$RSH" "mv $D/tunlite.ko.new $D/tunlite.ko; mv $D/openvpn.new $D/openvpn"
webui_put "$HERE/device/tunvpn.sh" $D/tunvpn.sh 755
webui_put "$R/www/tunvpn.cgi" $WWW/tunvpn.cgi 755
webui_put "$R/www/html/router/tunvpn.html" $WWW/html/router/tunvpn.html 644
webui_put "$R/www/js/panel/router/tunvpn.js" $WWW/js/panel/router/tunvpn.js 644

# config: keep an existing uci tunvpn; otherwise migrate the hand-written first version, else defaults
"$RSH" 'sh -s' < <(cat <<MIGRATE
C=/system/etc/config/tunvpn; D=$D
if [ ! -f \$C ]; then
	cat > \$C <<'DEF'
$(cat "$R/config/tunvpn")
DEF
	if [ -f \$D/client.conf ] && grep -q '^remote ' \$D/client.conf; then
		set -- \$(grep '^remote ' \$D/client.conf)
		uci set tunvpn.main.remote="\$2"; uci set tunvpn.main.port="\${3:-1194}"
		grep -q '^proto udp' \$D/client.conf && uci set tunvpn.main.proto=udp
		if [ -f \$D/auth.txt ]; then
			uci set tunvpn.main.username="\$(sed -n 1p \$D/auth.txt)"
			uci set tunvpn.main.password="\$(sed -n 2p \$D/auth.txt)"
		fi
		[ -f \$D/enabled ] && uci set tunvpn.main.enabled=1
		uci commit tunvpn
		echo "migrated client.conf/auth.txt to uci tunvpn"
	fi
fi
rm -f \$D/enabled
MIGRATE
)
webui_add mMgmtVPN objMgmtVPN js/panel/router/tunvpn.js "$HERE/i18n" mMgmtVPN lt_TVPN_
"$RSH" "grep -qF '$D/tunvpn.sh check' /etc/crontabs/root || echo '$CRON' >> /etc/crontabs/root; /etc/init.d/cron restart
sh $D/tunvpn.sh restart; sleep 8; sh $D/tunvpn.sh status"
