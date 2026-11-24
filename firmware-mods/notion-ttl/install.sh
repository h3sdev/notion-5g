#!/bin/bash
# install.sh - field installer: installs (or removes) the web-configurable outbound TTL on a running
# Notion 5G unit over SSH. Not needed for the firmware build (see OEM-SPEC.md section 0).
#
#   firmware-mods/notion-ttl/install.sh            install / update (idempotent)
#   firmware-mods/notion-ttl/install.sh --remove   restore the factory web UI and remove the rules
#
# Everything lives in /system/etc (writable, persistent overlay). The 5 factory web files that get patched
# are backed up ONCE to /system/etc/notion-ttl-backup/ before they are touched.
# Requires NOTION_PASS (device root SSH password), same as scripts/rsh.sh.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
RSH="$HERE/../../scripts/rsh.sh"
ROOT="$HERE/rootfs"
BK=/system/etc/notion-ttl-backup
WWW=/system/etc/www
PATCHED="index.html xml/ui_mifi.xml properties/Messages_es.properties properties/Messages_en.properties properties/Messages_cn.properties"

put() { # local file -> remote path, mode
	"$RSH" "cat > '$2.tmp' && chmod $3 '$2.tmp' && mv '$2.tmp' '$2'" < "$1"
}

if [ "${1:-}" = "--remove" ]; then
	"$RSH" "set -e
sh /system/etc/firewall.notion-ttl.sh remove 2>/dev/null || true
uci -q delete firewall.notion_ttl || true; uci commit firewall
for f in $PATCHED; do [ -f $BK/\$f ] && cp $BK/\$f $WWW/\$f; done
rm -f $WWW/notion_ttl.cgi $WWW/html/router/notion_ttl.html $WWW/js/panel/router/notion_ttl.js
rm -f /system/etc/firewall.notion-ttl.sh /system/etc/config/notion_ttl
echo 'web UI restored from $BK (backup kept)'"
	exit 0
fi

# 1) one-time backup of the factory files
"$RSH" "set -e; mkdir -p $BK/xml $BK/properties
for f in $PATCHED; do [ -f $BK/\$f ] || cp $WWW/\$f $BK/\$f; done; ls -R $BK | head -20"

# 2) new files
put "$ROOT/system/etc/firewall.notion-ttl.sh"            /system/etc/firewall.notion-ttl.sh 755
put "$ROOT/system/etc/www/notion_ttl.cgi"                $WWW/notion_ttl.cgi 755
put "$ROOT/system/etc/www/html/router/notion_ttl.html"   $WWW/html/router/notion_ttl.html 644
put "$ROOT/system/etc/www/js/panel/router/notion_ttl.js" $WWW/js/panel/router/notion_ttl.js 644
# config: only if missing, so a value already set by the user is not overwritten
"$RSH" "[ -f /system/etc/config/notion_ttl ] || cat > /system/etc/config/notion_ttl" < "$ROOT/system/etc/config/notion_ttl"

# 3) UI strings: replaced (delete ours + append) so a reinstall updates them
for lang in es en cn; do
	"$RSH" "f=$WWW/properties/Messages_$lang.properties
cat > /tmp/notion_ttl_i18n
sed -i -e '/^mNotionTTL /d' -e '/^lt_NotionTTL_/d' \$f
[ -z \"\$(tail -c1 \$f)\" ] || printf '\\n' >> \$f
cat /tmp/notion_ttl_i18n >> \$f; rm -f /tmp/notion_ttl_i18n" < "$HERE/i18n/Messages_$lang.properties"
done

# 4) <script> include, and register as a fw3 include (re-applied on every fw3 reload)
"$RSH" 'sh -s' <<'REMOTE'
set -e
WWW=/system/etc/www
# no menu entry: the section is appended to the Diagnostics page by notion_ttl.js.
# Remove the entry older versions of this installer added.
sed -i "/id='mNotionTTL'/d" $WWW/xml/ui_mifi.xml
grep -q notion_ttl.js $WWW/index.html || sed -i '\#js/panel/router/sipalg.js#a\		<script type="text/javascript" src="js/panel/router/notion_ttl.js" language="javascript"></script>' $WWW/index.html
uci -q delete firewall.notion_ttl || true
uci set firewall.notion_ttl=include
uci set firewall.notion_ttl.type=script
uci set firewall.notion_ttl.path=/system/etc/firewall.notion-ttl.sh
uci set firewall.notion_ttl.reload=1
uci set firewall.notion_ttl.family=any
uci commit firewall
sh /system/etc/firewall.notion-ttl.sh apply
echo '--- menu (expected: none):'; grep -c mNotionTTL $WWW/xml/ui_mifi.xml || true
echo '--- script:'; grep -n notion_ttl.js $WWW/index.html
echo '--- config:'; uci show notion_ttl
echo '--- rules:'; sh /system/etc/firewall.notion-ttl.sh status
REMOTE
