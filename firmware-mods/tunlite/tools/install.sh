#!/bin/bash
# install.sh - installs tunlite.ko + OpenVPN + the management tunnel on a running Notion 5G over SSH.
#
#   NOTION_PASS=... firmware-mods/tunlite/tools/install.sh <auth.txt>   install/update and enable
#   NOTION_PASS=... firmware-mods/tunlite/tools/install.sh --remove     stop, unload and delete everything
#
# <auth.txt>: two lines (username, password) of the user created for this unit on the OpenVPN server.
# Binaries come from firmware-mods/tunlite/bin/ (built as described in BUILD.md).
set -e
HERE="$(cd "$(dirname "$0")/.." && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
RSH="$REPO/scripts/rsh.sh"
RPUT="$REPO/scripts/rput.sh"
D=/data/tunvpn
CRON_LINE="* * * * * $D/tunvpn.sh check"

if [ "$1" = "--remove" ]; then
	"$RSH" "$D/tunvpn.sh stop 2>/dev/null; rmmod tunlite 2>/dev/null; grep -v '$D/tunvpn.sh' /etc/crontabs/root > /tmp/cr.new; cat /tmp/cr.new > /etc/crontabs/root; rm -f /tmp/cr.new; rm -rf $D; /etc/init.d/cron restart; echo removed"
	exit 0
fi

AUTH="${1:?usage: install.sh <auth.txt> | --remove}"
[ "$(grep -c . "$AUTH")" -ge 2 ] || { echo "auth file must have username and password lines" >&2; exit 1; }
for f in bin/tunlite.ko bin/openvpn device/tunvpn.sh device/client.conf device/ca.crt; do
	[ -f "$HERE/$f" ] || { echo "missing $HERE/$f" >&2; exit 1; }
done

"$RSH" "mkdir -p $D && chmod 700 $D"
"$RPUT" "$HERE/bin/tunlite.ko" "$D/tunlite.ko" 644
"$RPUT" "$HERE/bin/openvpn" "$D/openvpn" 755
"$RPUT" "$HERE/device/tunvpn.sh" "$D/tunvpn.sh" 755
"$RPUT" "$HERE/device/client.conf" "$D/client.conf" 644
"$RPUT" "$HERE/device/ca.crt" "$D/ca.crt" 644
"$RPUT" "$AUTH" "$D/auth.txt" 600
"$RSH" "touch $D/enabled; grep -qF '$D/tunvpn.sh check' /etc/crontabs/root || echo '$CRON_LINE' >> /etc/crontabs/root; /etc/init.d/cron restart; $D/tunvpn.sh start; sleep 8; $D/tunvpn.sh status; tail -5 /tmp/tunvpn.log"
