#!/bin/bash
# verify.sh - checks a unit against REQUIREMENTS.md, from a PC connected to it:
#   NOTION_PASS=<root password> tools/verify.sh
# 1) file contents: dist/MANIFEST.md5 (written by make-oem-package.sh) checked with md5sum -c on the device;
# 2) tools/verify-device.sh run on the device (files, modes, config, rules, ping, fw3 reload, naming).
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PKG="$(cd "$HERE/.." && pwd)"
rc=0
# MANIFEST.md5 is written by make-oem-package.sh to dist/
M="$PKG/dist/MANIFEST.md5"
if [ -f "$M" ]; then
	echo "== R0 file contents (dist/MANIFEST.md5)"
	"$HERE/rsh.sh" 'cat > /tmp/netparam.md5; md5sum -c /tmp/netparam.md5; r=$?; rm -f /tmp/netparam.md5; exit $r' \
		< "$M" || rc=1
else
	echo "SKIP  R0 no dist/MANIFEST.md5 (run make-oem-package.sh first)"
fi
"$HERE/rsh.sh" 'sh -s' < "$HERE/verify-device.sh" || rc=1
exit $rc
