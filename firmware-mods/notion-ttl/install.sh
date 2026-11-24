#!/bin/bash
# install.sh — instala (o quita) en el módem Notion 5G el TTL configurable desde la interfaz web.
#
#   firmware-mods/notion-ttl/install.sh            instala / actualiza (idempotente)
#   firmware-mods/notion-ttl/install.sh --remove   deja la web como estaba de fábrica y quita las reglas
#
# Todo vive en /system/etc (overlay escribible y persistente). Los 5 archivos originales de la web que se
# parchean se respaldan UNA vez en /system/etc/notion-ttl-backup/ antes de tocarlos.
# Requiere NOTION_PASS (clave SSH root del equipo), igual que scripts/rsh.sh.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
RSH="$HERE/../../scripts/rsh.sh"
ROOT="$HERE/rootfs"
BK=/system/etc/notion-ttl-backup
WWW=/system/etc/www
PATCHED="index.html xml/ui_mifi.xml properties/Messages_es.properties properties/Messages_en.properties properties/Messages_cn.properties"

put() { # archivo local -> ruta remota, modo
	"$RSH" "cat > '$2.tmp' && chmod $3 '$2.tmp' && mv '$2.tmp' '$2'" < "$1"
}

if [ "${1:-}" = "--remove" ]; then
	"$RSH" "set -e
sh /system/etc/firewall.notion-ttl.sh remove 2>/dev/null || true
uci -q delete firewall.notion_ttl || true; uci commit firewall
for f in $PATCHED; do [ -f $BK/\$f ] && cp $BK/\$f $WWW/\$f; done
rm -f $WWW/notion_ttl.cgi $WWW/html/router/notion_ttl.html $WWW/js/panel/router/notion_ttl.js
rm -f /system/etc/firewall.notion-ttl.sh /system/etc/config/notion_ttl
echo 'web restaurada desde $BK (el respaldo se conserva)'"
	exit 0
fi

# 1) respaldo único de los originales
"$RSH" "set -e; mkdir -p $BK/xml $BK/properties
for f in $PATCHED; do [ -f $BK/\$f ] || cp $WWW/\$f $BK/\$f; done; ls -R $BK | head -20"

# 2) archivos nuevos
put "$ROOT/system/etc/firewall.notion-ttl.sh"            /system/etc/firewall.notion-ttl.sh 755
put "$ROOT/system/etc/www/notion_ttl.cgi"                $WWW/notion_ttl.cgi 755
put "$ROOT/system/etc/www/html/router/notion_ttl.html"   $WWW/html/router/notion_ttl.html 644
put "$ROOT/system/etc/www/js/panel/router/notion_ttl.js" $WWW/js/panel/router/notion_ttl.js 644
# config: solo si no existe, para no pisar el valor que ya haya puesto el usuario
"$RSH" "[ -f /system/etc/config/notion_ttl ] || cat > /system/etc/config/notion_ttl" < "$ROOT/system/etc/config/notion_ttl"

# 3) textos: se reemplazan (borrar los nuestros + agregar al final) para que una reinstalación los actualice
for lang in es en cn; do
	"$RSH" "f=$WWW/properties/Messages_$lang.properties
cat > /tmp/notion_ttl_i18n
sed -i -e '/^mNotionTTL /d' -e '/^lt_NotionTTL_/d' \$f
[ -z \"\$(tail -c1 \$f)\" ] || printf '\\n' >> \$f
cat /tmp/notion_ttl_i18n >> \$f; rm -f /tmp/notion_ttl_i18n" < "$HERE/i18n/Messages_$lang.properties"
done

# 4) menú + <script>, y registro como include de fw3 (re-aplica en cada fw3 reload)
"$RSH" 'sh -s' <<'REMOTE'
set -e
WWW=/system/etc/www
grep -q mNotionTTL $WWW/xml/ui_mifi.xml || sed -i "/id='mWifiDDNS'/a\                    <ThirdlevelMenu id='mNotionTTL' implFunction='objNotionTTL' type='submenuabsent'/>" $WWW/xml/ui_mifi.xml
grep -q notion_ttl.js $WWW/index.html || sed -i '\#js/panel/router/sipalg.js#a\		<script type="text/javascript" src="js/panel/router/notion_ttl.js" language="javascript"></script>' $WWW/index.html
uci -q delete firewall.notion_ttl || true
uci set firewall.notion_ttl=include
uci set firewall.notion_ttl.type=script
uci set firewall.notion_ttl.path=/system/etc/firewall.notion-ttl.sh
uci set firewall.notion_ttl.reload=1
uci set firewall.notion_ttl.family=any
uci commit firewall
sh /system/etc/firewall.notion-ttl.sh apply
echo '--- menú:';   grep -n mNotionTTL $WWW/xml/ui_mifi.xml
echo '--- script:'; grep -n notion_ttl.js $WWW/index.html
echo '--- config:'; uci show notion_ttl
echo '--- reglas:'; sh /system/etc/firewall.notion-ttl.sh status
REMOTE
