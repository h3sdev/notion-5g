#!/bin/bash
# install.sh - field installer: installs (or removes) the "network parameter" feature (outbound TTL,
# see OEM-SPEC.md) on a running Notion 5G unit over SSH. Not needed for the firmware build (see OEM-SPEC.md section 0).
#
#   firmware-mods/netparam/install.sh            install / update (idempotent)
#   firmware-mods/netparam/install.sh --remove   restore the factory web UI and remove the rules
#
# Everything lives in /system/etc (writable, persistent overlay). The 5 factory web files that get patched
# are backed up ONCE to /system/etc/netparam-backup/ before they are touched.
# Requires NOTION_PASS (device root SSH password), same as scripts/rsh.sh.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
RSH="$HERE/../../scripts/rsh.sh"
ROOT="$HERE/rootfs"
BK=/system/etc/netparam-backup
WWW=/system/etc/www
PATCHED="index.html xml/ui_mifi.xml properties/Messages_es.properties properties/Messages_en.properties properties/Messages_cn.properties"

put() { # local file -> remote path, mode
	"$RSH" "cat > '$2.tmp' && chmod $3 '$2.tmp' && mv '$2.tmp' '$2'" < "$1"
}

if [ "${1:-}" = "--remove" ]; then
	"$RSH" "set -e
sh /system/etc/firewall.netparam.sh remove 2>/dev/null || true
uci -q delete firewall.netparam || true; uci commit firewall
for f in $PATCHED; do [ -f $BK/\$f ] && cp $BK/\$f $WWW/\$f; done
rm -f $WWW/netparam.cgi $WWW/html/router/netparam.html $WWW/js/panel/router/netparam.js
rm -f /system/etc/firewall.netparam.sh /system/etc/config/netparam
echo 'web UI restored from $BK (backup kept)'"
	exit 0
fi

# 0) migrate units that have the first version (internal name notion_ttl / notion-ttl):
#    keep the user's value and the factory backup, then delete every old file, rule, string and include.
"$RSH" 'sh -s' <<'MIGRATE'
OLD_BK=/system/etc/notion-ttl-backup; NEW_BK=/system/etc/netparam-backup; WWW=/system/etc/www
[ -d $OLD_BK ] && [ ! -d $NEW_BK ] && mv $OLD_BK $NEW_BK && echo "migrated factory backup -> $NEW_BK"
if [ -f /system/etc/config/notion_ttl ] && [ ! -f /system/etc/config/netparam ]; then
	en=$(uci -q get notion_ttl.main.enabled); v=$(uci -q get notion_ttl.main.ttl)
	v6=$(uci -q get notion_ttl.main.ipv6); ifc=$(uci -q get notion_ttl.main.iface)
	printf "\nconfig netparam 'main'\n\toption enabled '%s'\n\toption value '%s'\n\toption ipv6 '%s'\n\toption iface '%s'\n" \
		"${en:-1}" "${v:-64}" "${v6:-1}" "${ifc:-ccinet+}" > /system/etc/config/netparam
	echo "migrated config: enabled=${en:-1} value=${v:-64} ipv6=${v6:-1}"
fi
for bin in iptables ip6tables; do
	$bin -t mangle -S POSTROUTING 2>/dev/null | grep -- "--comment notion-ttl " | sed 's/^-A /-D /' |
	while read -r rule; do $bin -t mangle $rule; done
done
if uci -q get firewall.notion_ttl >/dev/null; then uci delete firewall.notion_ttl; uci commit firewall; fi
rm -f /system/etc/firewall.notion-ttl.sh /system/etc/config/notion_ttl $WWW/notion_ttl.cgi \
	$WWW/html/router/notion_ttl.html $WWW/js/panel/router/notion_ttl.js
sed -i '/js\/panel\/router\/notion_ttl\.js/d' $WWW/index.html
sed -i "/id='mNotionTTL'/d" $WWW/xml/ui_mifi.xml
for l in es en cn; do sed -i -e '/^mNotionTTL /d' -e '/^lt_NotionTTL_/d' $WWW/properties/Messages_$l.properties; done
MIGRATE

# 1) one-time backup of the factory files
"$RSH" "set -e; mkdir -p $BK/xml $BK/properties
for f in $PATCHED; do [ -f $BK/\$f ] || cp $WWW/\$f $BK/\$f; done; ls -R $BK | head -20"

# 2) new files
put "$ROOT/system/etc/firewall.netparam.sh"            /system/etc/firewall.netparam.sh 755
put "$ROOT/system/etc/www/netparam.cgi"                $WWW/netparam.cgi 755
put "$ROOT/system/etc/www/html/router/netparam.html"   $WWW/html/router/netparam.html 644
put "$ROOT/system/etc/www/js/panel/router/netparam.js" $WWW/js/panel/router/netparam.js 644
# config: only if missing, so a value already set by the user is not overwritten
"$RSH" "[ -f /system/etc/config/netparam ] || cat > /system/etc/config/netparam" < "$ROOT/system/etc/config/netparam"

# 3) UI strings: replaced (delete ours + append) so a reinstall updates them
for lang in es en cn; do
	"$RSH" "f=$WWW/properties/Messages_$lang.properties
cat > /tmp/netparam_i18n
sed -i -e '/^mNetParam /d' -e '/^lt_NetParam_/d' \$f
[ -z \"\$(tail -c1 \$f)\" ] || printf '\\n' >> \$f
cat /tmp/netparam_i18n >> \$f; rm -f /tmp/netparam_i18n" < "$HERE/i18n/Messages_$lang.properties"
done

# 4) <script> include, and register as a fw3 include (re-applied on every fw3 reload)
"$RSH" 'sh -s' <<'REMOTE'
set -e
WWW=/system/etc/www
# no menu entry: the section is appended to the Diagnostics page by netparam.js.
grep -q netparam.js $WWW/index.html || sed -i '\#js/panel/router/sipalg.js#a\		<script type="text/javascript" src="js/panel/router/netparam.js" language="javascript"></script>' $WWW/index.html
uci -q delete firewall.netparam || true
uci set firewall.netparam=include
uci set firewall.netparam.type=script
uci set firewall.netparam.path=/system/etc/firewall.netparam.sh
uci set firewall.netparam.reload=1
uci set firewall.netparam.family=any
uci commit firewall
sh /system/etc/firewall.netparam.sh apply
echo '--- old names left (expected 0):'; grep -rIl -i 'notion.ttl\|NotionTTL' $WWW /system/etc/config /system/etc/*.sh 2>/dev/null | wc -l
echo '--- script:'; grep -n netparam.js $WWW/index.html
echo '--- config:'; uci show netparam
echo '--- rules:'; sh /system/etc/firewall.netparam.sh status
REMOTE
