#!/bin/bash
# make-oem-package.sh - builds the single zip for the manufacturer with the three H3S features:
#   network parameter (netparam), management VPN (tunlite + OpenVPN) and captive portal (omnicap-gw).
#   -> dist/h3s-firmware-oem-<date>.zip  (replaces the earlier netparam-oem-*.zip: same netparam files)
#
# Zip contents (English, except the UI strings):
#   IMPLEMENTATION.md  REQUIREMENTS.md  screenshots/  VERSION.txt
#   files/   final files at their device path = factory originals + the 3 features applied
#   merge/   apply scripts for when the manufacturer's sources of the shared files differ
# For H3S only (not in the zip): dist/MANIFEST.md5 (expected MD5 on the device, read by tools/verify-firmware.sh).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
FM="$(cd "$HERE/.." && pwd)"
NAME="h3s-firmware-oem-$(date +%Y-%m-%d)"
OUT="$HERE/dist/$NAME"
NEW="$HERE/dist/new"
rm -rf "$OUT" "$OUT.zip" "$NEW"; mkdir -p "$OUT" "$NEW"

# 1) new files of the two modules, at their device path (binaries built/taken here)
echo "building omnicap-gw ..."
( cd "$FM/omnicap-gw" && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
	-o "$NEW/usr/sbin/omnicap-gw" . )
install -D -m 0755 "$FM/tunlite/bin/openvpn"           "$NEW/usr/sbin/openvpn"
install -D -m 0755 "$FM/omnicap-gw/device/ocgw.sh"     "$NEW/usr/sbin/ocgw.sh"
install -D -m 0755 "$FM/tunlite/device/tunvpn.sh"      "$NEW/usr/sbin/tunvpn.sh"
install -D -m 0644 "$FM/tunlite/bin/tunlite.ko"        "$NEW/lib/modules/5.4.288+/tunlite.ko"
for m in omnicap-gw tunlite; do cp -a "$FM/$m/rootfs/system" "$NEW/"; done

# 2) merge/: apply scripts + strings, usable on the manufacturer's own sources
mkdir -p "$OUT/merge/netparam" "$OUT/merge/vpn-portal/i18n"
cp -a "$FM/netparam/apply-to-rootfs.sh" "$FM/netparam/i18n" "$OUT/merge/netparam/"
sed -i 's#NEW=$(cd "$HERE/../files" \&\& pwd)#NEW=$(cd "$HERE/../../files" \&\& pwd)#' "$OUT/merge/netparam/apply-to-rootfs.sh"
grep -q '"$HERE/../../files"' "$OUT/merge/netparam/apply-to-rootfs.sh"
cp "$HERE/apply-to-rootfs.sh" "$OUT/merge/vpn-portal/"
sed -i 's#NEW=$(cd "$HERE/../files" \&\& pwd)#NEW=$(cd "$HERE/../../files" \&\& pwd)#' "$OUT/merge/vpn-portal/apply-to-rootfs.sh"
grep -q '"$HERE/../../files"' "$OUT/merge/vpn-portal/apply-to-rootfs.sh"
cp -a "$FM/tunlite/i18n" "$OUT/merge/vpn-portal/i18n/tunvpn"
cp -a "$FM/omnicap-gw/i18n" "$OUT/merge/vpn-portal/i18n/omnicapgw"
chmod 0755 "$OUT"/merge/*/apply-to-rootfs.sh

# 3) files/ = factory originals of the shared files + the 3 features
mkdir -p "$OUT/files"
cp -a "$FM/netparam/factory/system" "$OUT/files/"
cp -a "$HERE/factory/system" "$OUT/files/"
chmod 0644 "$OUT"/files/system/etc/www/index.html "$OUT"/files/system/etc/www/properties/*.properties \
	"$OUT/files/system/etc/config/firewall" "$OUT/files/system/etc/www/xml/ui_mifi.xml"
"$FM/netparam/apply-to-rootfs.sh" "$OUT/files" >/dev/null
# our apply script with the staged new files and strings
mkdir -p "$HERE/dist/stage-merge/i18n"; cp "$HERE/apply-to-rootfs.sh" "$HERE/dist/stage-merge/"
rm -rf "$HERE/dist/stage-merge/new"; ln -s "$NEW" "$HERE/dist/stage-merge/new"
cp -a "$FM/tunlite/i18n" "$HERE/dist/stage-merge/i18n/tunvpn" 2>/dev/null || true
cp -a "$FM/omnicap-gw/i18n" "$HERE/dist/stage-merge/i18n/omnicapgw" 2>/dev/null || true
sh "$HERE/dist/stage-merge/apply-to-rootfs.sh" "$OUT/files"
rm -rf "$HERE/dist/stage-merge" "$NEW"

# 4) docs
cp "$HERE/IMPLEMENTATION.md" "$HERE/REQUIREMENTS.md" "$OUT/"
mkdir -p "$OUT/screenshots"; cp "$HERE"/screenshots/*.png "$OUT/screenshots/"

# H3S only: MANIFEST.md5 = every delivered file the system never rewrites (not uci configs, not crontab)
( cd "$OUT/files" && find . -type f ! -path './system/etc/config/*' ! -path './system/etc/crontabs/*' | sort \
	| xargs md5sum | sed 's#  \./#  /#'
  cat "$FM/netparam/factory/UNCHANGED.md5" | grep -v ui_mifi.xml ) > "$HERE/dist/MANIFEST.md5"

rev=$(git -C "$HERE" log -1 --format='%h %cd' --date=short)
dirty=$(git -C "$FM" status --porcelain . | grep -v 'dist/' || true)
printf 'H3S firmware features (network parameter, management VPN, captive portal) - package %s\nSource: H3S repository, commit %s%s\nStart with IMPLEMENTATION.md. Replaces netparam-oem-*.zip.\n' \
	"$NAME" "$rev" "${dirty:+ + uncommitted changes}" > "$OUT/VERSION.txt"

python3 - "$HERE/dist" "$NAME" <<'PY'
import os, sys, zipfile
base, name = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(os.path.join(base, name + ".zip"), "w", zipfile.ZIP_DEFLATED) as z:
    for root, dirs, files in os.walk(os.path.join(base, name)):
        dirs.sort()
        for f in sorted(files):
            p = os.path.join(root, f)
            zi = zipfile.ZipInfo.from_file(p, os.path.relpath(p, base))
            zi.compress_type = zipfile.ZIP_DEFLATED
            with open(p, "rb") as fh:
                z.writestr(zi, fh.read())
PY
echo "$OUT.zip"
( cd "$OUT" && find . -type f | sort | sed 's#^\./#    #' )
