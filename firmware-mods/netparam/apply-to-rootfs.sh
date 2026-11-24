#!/bin/sh
# apply-to-rootfs.sh - adds the "network parameter" feature to a firmware root filesystem tree, before the
# tree is packed into the image. Use it when your own index.html, Messages_*.properties or config/firewall
# differ from the ones in files/ (otherwise just copy files/). It merges the changes into YOUR versions of
# those files and adds the 5 new files. Idempotent.
#
#   ./apply-to-rootfs.sh <rootfs>
#
# <rootfs> is the directory that becomes "/" on the device: it must contain system/etc/www/index.html and
# system/etc/config/firewall. Nothing outside <rootfs> is touched. Needs a POSIX shell and GNU sed.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
I18N=$HERE/i18n
# new files: rootfs/ next to this script (H3S repository) or files/ one level up (OEM package, merge/)
if [ -d "$HERE/rootfs" ]; then NEW=$HERE/rootfs; else NEW=$(cd "$HERE/../files" && pwd); fi
[ $# -eq 1 ] || { echo "usage: $0 <rootfs>" >&2; exit 2; }
R=${1%/}; E=$R/system/etc; W=$E/www

for f in "$W/index.html" "$W/properties/Messages_es.properties" "$W/properties/Messages_en.properties" \
         "$W/properties/Messages_cn.properties" "$E/config/firewall"; do
	[ -f "$f" ] || { echo "ERROR: missing $f (is $R the rootfs?)" >&2; exit 1; }
done
grep -q 'js/panel/router/sipalg.js' "$W/index.html" ||
	{ echo "ERROR: anchor line js/panel/router/sipalg.js not found in $W/index.html" >&2; exit 1; }

echo "1) new files"
add() { # source (relative to rootfs/), mode
	install -D -m "$2" "$NEW/$1" "$R/$1"; echo "   $2  /$1"
}
add system/etc/config/netparam                    0644
add system/etc/firewall.netparam.sh               0755
add system/etc/www/netparam.cgi                   0755
add system/etc/www/html/router/netparam.html      0644
add system/etc/www/js/panel/router/netparam.js    0644

echo "2) existing web files"
if ! grep -q 'js/panel/router/netparam.js' "$W/index.html"; then
	sed -i '\#js/panel/router/sipalg.js#a\		<script type="text/javascript" src="js/panel/router/netparam.js" language="javascript"></script>' "$W/index.html"
fi
echo "   /system/etc/www/index.html: $(grep -c 'js/panel/router/netparam.js' "$W/index.html") <script> line for netparam.js"
for l in es en cn; do
	f=$W/properties/Messages_$l.properties
	sed -i '/^lt_NetParam_/d' "$f"                       # replace, so re-running updates the strings
	[ -z "$(tail -c1 "$f")" ] || printf '\n' >> "$f"     # the factory files have no final newline
	cat "$I18N/Messages_$l.properties" >> "$f"
	echo "   /system/etc/www/properties/Messages_$l.properties: $(grep -c '^lt_NetParam_' "$f") lt_NetParam_* strings"
done

echo "3) firewall include"
if ! grep -q "^config include 'netparam'" "$E/config/firewall"; then
	printf "\nconfig include 'netparam'\n\toption type 'script'\n\toption path '/system/etc/firewall.netparam.sh'\n\toption reload '1'\n\toption family 'any'\n" >> "$E/config/firewall"
fi
echo "   /system/etc/config/firewall: $(grep -c "^config include 'netparam'" "$E/config/firewall") include section"

echo "done. Do NOT modify xml/ui_mifi.xml or the Diagnostics files (see REQUIREMENTS.md)."
