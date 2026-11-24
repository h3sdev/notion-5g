#!/bin/bash
# verify.sh - checks a unit against REQUIREMENTS.md, from a PC connected to it:
#   NOTION_PASS=<root password> tools/verify.sh
# 1) file contents: MANIFEST.md5 (package root) checked with md5sum -c on the device;
# 2) tools/verify-device.sh run on the device (files, modes, config, rules, ping, fw3 reload, naming).
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PKG="$(cd "$HERE/.." && pwd)"
rc=0
if [ -f "$PKG/MANIFEST.md5" ]; then
	echo "== R0 file contents (MANIFEST.md5)"
	"$HERE/rsh.sh" 'cat > /tmp/netparam.md5; md5sum -c /tmp/netparam.md5; r=$?; rm -f /tmp/netparam.md5; exit $r' \
		< "$PKG/MANIFEST.md5" || rc=1
fi
"$HERE/rsh.sh" 'sh -s' < "$HERE/verify-device.sh" || rc=1
exit $rc
