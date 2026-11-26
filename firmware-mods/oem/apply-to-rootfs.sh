#!/bin/sh
# apply-to-rootfs.sh - adds the "Management VPN" and "Captive portal" features to a firmware root filesystem
# tree, before the tree is packed into the image. Use it when your own index.html, xml/ui_mifi.xml,
# Messages_*.properties, crontabs/root or config/firewall differ from the ones in files/ (otherwise just copy
# files/). It adds
# the new files and merges the changes into YOUR versions of those files. Idempotent.
#
#   ./apply-to-rootfs.sh <rootfs>
#
# <rootfs> is the directory that becomes "/" on the device. Nothing outside <rootfs> is touched.
# Needs a POSIX shell, awk and GNU sed. Apply the network parameter feature (apply-netparam.sh) too.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
# new files: next to this script in the H3S repository (new/), or ../files in the OEM package
if [ -d "$HERE/new" ]; then NEW=$HERE/new; else NEW=$(cd "$HERE/../files" && pwd); fi
I18N=$HERE/i18n
[ $# -eq 1 ] || { echo "usage: $0 <rootfs>" >&2; exit 2; }
R=${1%/}; E=$R/system/etc; W=$E/www

for f in "$W/index.html" "$W/xml/ui_mifi.xml" "$W/properties/Messages_es.properties" \
         "$W/properties/Messages_en.properties" "$W/properties/Messages_cn.properties" "$E/config/firewall"; do
	[ -f "$f" ] || { echo "ERROR: missing $f (is $R the rootfs?)" >&2; exit 1; }
done
grep -q 'js/panel/router/vpn.js' "$W/index.html" || { echo "ERROR: js/panel/router/vpn.js not found in index.html" >&2; exit 1; }
grep -q "id='mVPN'" "$W/xml/ui_mifi.xml" || { echo "ERROR: menu entry id='mVPN' not found in ui_mifi.xml" >&2; exit 1; }

echo "1) new files"
add() { # path relative to rootfs, mode
	[ "$NEW/$1" -ef "$R/$1" ] || install -D -m "$2" "$NEW/$1" "$R/$1"; chmod "$2" "$R/$1"; echo "   $2  /$1"
}
add usr/sbin/omnicap-gw                          0755
add usr/sbin/openvpn                             0755
add usr/sbin/ocgw.sh                             0755
add usr/sbin/tunvpn.sh                           0755
add lib/modules/5.4.288+/tunlite.ko              0644
add system/etc/config/omnicapgw                  0644
add system/etc/config/tunvpn                     0644
add system/etc/www/omnicapgw.cgi                 0755
add system/etc/www/tunvpn.cgi                    0755
add system/etc/www/html/router/omnicapgw.html    0644
add system/etc/www/html/router/tunvpn.html       0644
add system/etc/www/js/panel/router/omnicapgw.js  0644
add system/etc/www/js/panel/router/tunvpn.js     0644

echo "2) index.html: 2 <script> lines after js/panel/router/vpn.js"
for js in js/panel/router/tunvpn.js js/panel/router/omnicapgw.js; do
	grep -v -F "$js" "$W/index.html" > "$W/index.html.tmp"; cat "$W/index.html.tmp" > "$W/index.html"; rm -f "$W/index.html.tmp"
done
awk '{print} /js\/panel\/router\/vpn.js/{
	print "\t\t<script type=\"text/javascript\" src=\"js/panel/router/tunvpn.js\" language=\"javascript\"></script>"
	print "\t\t<script type=\"text/javascript\" src=\"js/panel/router/omnicapgw.js\" language=\"javascript\"></script>"}' \
	"$W/index.html" > "$W/index.html.tmp"; cat "$W/index.html.tmp" > "$W/index.html"; rm -f "$W/index.html.tmp"

echo "3) xml/ui_mifi.xml: 2 menu entries after id='mVPN' (Router > Advanced Settings)"
X=$W/xml/ui_mifi.xml
grep -v "id='mMgmtVPN'" "$X" | grep -v "id='mCaptivePortal'" > "$X.tmp"; cat "$X.tmp" > "$X"; rm -f "$X.tmp"
awk "{print} /id='mVPN'/{
	print \"                    <ThirdlevelMenu id='mMgmtVPN' implFunction='objMgmtVPN' type='submenuabsent'/>\"
	print \"                    <ThirdlevelMenu id='mCaptivePortal' implFunction='objCaptivePortal' type='submenuabsent'/>\"}" \
	"$X" > "$X.tmp"; cat "$X.tmp" > "$X"; rm -f "$X.tmp"

echo "4) Messages_{es,en,cn}.properties: UI strings appended"
for l in es en cn; do
	p=$W/properties/Messages_$l.properties
	sed -i -e '/^mMgmtVPN /d' -e '/^lt_TVPN_/d' -e '/^mCaptivePortal /d' -e '/^lt_OCGW_/d' "$p"
	[ -z "$(tail -c1 "$p")" ] || printf '\n' >> "$p"
	cat "$I18N/tunvpn/Messages_$l.properties" "$I18N/omnicapgw/Messages_$l.properties" >> "$p"
	echo "   $(grep -c -e '^mMgmtVPN ' -e '^lt_TVPN_' -e '^mCaptivePortal ' -e '^lt_OCGW_' "$p") lines in Messages_$l.properties"
done

echo "5) crontabs/root: 2 watchdog lines (start at boot, restart if stopped)"
C=$E/crontabs/root
mkdir -p "$E/crontabs"; touch "$C"
grep -v -e '/usr/sbin/tunvpn.sh check' -e '/usr/sbin/ocgw.sh check' "$C" > "$C.tmp" || true
cat "$C.tmp" > "$C"; rm -f "$C.tmp"
[ -z "$(tail -c1 "$C")" ] || printf '\n' >> "$C"
printf '%s\n' '* * * * * /usr/sbin/tunvpn.sh check' '* * * * * /usr/sbin/ocgw.sh check' >> "$C"
chmod 0644 "$C"

echo "6) config/firewall: include section 'ocgw' (captive portal closed at boot and on firewall reloads)"
F=$E/config/firewall
if ! grep -q "^config include 'ocgw'" "$F"; then
	[ -z "$(tail -c1 "$F")" ] || printf '\n' >> "$F"
	printf "\nconfig include 'ocgw'\n\toption type 'script'\n\toption path '/usr/sbin/ocgw.sh'\n\toption reload '1'\n\toption family 'any'\n" >> "$F"
fi
echo "   $(grep -c "^config include 'ocgw'" "$F") include section"
echo "done"
