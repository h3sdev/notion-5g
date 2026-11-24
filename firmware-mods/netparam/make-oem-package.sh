#!/bin/bash
# make-oem-package.sh - builds the zip for the manufacturer: dist/netparam-oem-<date>.zip
#
# Layout of the zip (everything in English except the UI strings):
#   IMPLEMENTATION.md REQUIREMENTS.md OEM-SPEC.md screenshot-netparam.png MANIFEST.md5 VERSION.txt
#   files/   final files at their device path = factory/ + apply-to-rootfs.sh (new files from rootfs/)
#   merge/   apply-to-rootfs.sh, i18n/, web-ui-existing-files.patch (regenerated from factory/ vs files/)
#   tools/   verify.sh, verify-device.sh, test_cgi.py, test_ui.py, install.sh, rsh.sh, web_login.py
# Not shipped: OEM-SPEC.es.md and README.md (internal), factory/ (the manufacturer has the originals).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
NAME="netparam-oem-$(date +%Y-%m-%d)"
OUT="$HERE/dist/$NAME"
rm -rf "$OUT" "$OUT.zip"; mkdir -p "$OUT"

cp "$HERE"/{IMPLEMENTATION.md,REQUIREMENTS.md,OEM-SPEC.md,screenshot-netparam.png} "$OUT/"

# files/: factory originals + the feature applied to them
mkdir -p "$OUT/files"; cp -a "$HERE/factory/system" "$OUT/files/"
chmod 0644 "$OUT"/files/system/etc/www/index.html "$OUT"/files/system/etc/www/properties/*.properties \
	"$OUT/files/system/etc/config/firewall"
"$HERE/apply-to-rootfs.sh" "$OUT/files" >/dev/null

# merge/: the change alone; the patch is regenerated so it always matches files/
mkdir -p "$OUT/merge"
cp -a "$HERE/apply-to-rootfs.sh" "$HERE/i18n" "$OUT/merge/"
( cd "$HERE/factory/system/etc" && for f in www/index.html www/properties/Messages_cn.properties \
	www/properties/Messages_en.properties www/properties/Messages_es.properties config/firewall; do
	diff -u --label "a/$f" --label "b/$f" "$f" "$OUT/files/system/etc/$f" || true
done ) > "$HERE/web-ui-existing-files.patch"
cp "$HERE/web-ui-existing-files.patch" "$OUT/merge/"

mkdir -p "$OUT/tools"
cp -a "$HERE"/tools/{verify.sh,verify-device.sh,test_cgi.py,test_ui.py,install.sh,rsh.sh,web_login.py} "$OUT/tools/"

# MANIFEST.md5: every file the system never rewrites (not the uci configs) + the files that must not change
( cd "$OUT/files" && find system -type f ! -path 'system/etc/config/*' | sort | xargs md5sum | sed 's#  system/#  /system/#'
  cat "$HERE/factory/UNCHANGED.md5" ) > "$OUT/MANIFEST.md5"

rev=$(git -C "$HERE" log -1 --format='%h %cd' --date=short)
dirty=$(git -C "$HERE" status --porcelain . | grep -v '^?? .*dist/' || true)
printf 'Network parameter (configurable outbound TTL) - OEM package %s\nSource: H3S repository, commit %s%s\nStart with IMPLEMENTATION.md.\n' \
	"$NAME" "$rev" "${dirty:+ + uncommitted changes}" > "$OUT/VERSION.txt"

# zip with the unix modes kept (busybox-friendly, no zip binary needed)
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
