#!/bin/bash
# webui.sh - shared helpers to add/remove a page in the Notion 5G web UI over SSH (same approach as netparam):
# menu entry in xml/ui_mifi.xml (right after the factory "VPN" entry under Router > Advanced Settings), <script>
# tag in index.html (after the factory vpn.js), strings appended to properties/Messages_{es,en,cn}.properties.
# Only our own lines are added or removed, so other modules (netparam) are not affected. The factory files are
# backed up once to /system/etc/webui-backup/ before the first change.
#
# Usage (sourced): webui_add <menu_id> <implFunction> <js_path> <i18n_dir> <key_prefix>...
#                  webui_remove <menu_id> <js_path> <key_prefix>...
# Needs RSH (path to scripts/rsh.sh, NOTION_PASS set).

WWW=/system/etc/www

webui_put() { # local file -> remote path, mode
	"$RSH" "cat > '$2.tmp' && chmod $3 '$2.tmp' && mv '$2.tmp' '$2'" < "$1"
}

# Remote side: a small sh script uploaded once, called with arguments (no nested quoting).
WEBUI_REMOTE='#!/bin/sh
# webui-edit.sh add|remove <menu_id> <implFunction> <js_path> <i18n_file_prefix> <key_prefix>...
set -e
WWW=/system/etc/www; BK=/system/etc/webui-backup
op=$1; id=$2; fn=$3; js=$4; i18n=$5; shift 5
mkdir -p $BK/xml $BK/properties
for f in index.html xml/ui_mifi.xml properties/Messages_es.properties properties/Messages_en.properties properties/Messages_cn.properties; do
	[ -f $BK/$f ] || cp $WWW/$f $BK/$f
done
x=$WWW/xml/ui_mifi.xml; i=$WWW/index.html
grep -v "id=.$id." $x > $x.tmp && cat $x.tmp > $x && rm -f $x.tmp
grep -v -F "$js" $i > $i.tmp && cat $i.tmp > $i && rm -f $i.tmp
for l in es en cn; do
	p=$WWW/properties/Messages_$l.properties
	for k in "$@"; do grep -v "^$k" $p > $p.tmp || true; cat $p.tmp > $p; rm -f $p.tmp; done
done
[ "$op" = add ] || exit 0
grep -q "id=.mVPN." $x
awk -v line="                    <ThirdlevelMenu id=\x27$id\x27 implFunction=\x27$fn\x27 type=\x27submenuabsent\x27/>" \
	"{print} /id=.mVPN./{print line}" $x > $x.tmp && cat $x.tmp > $x && rm -f $x.tmp
awk -v line="		<script type=\"text/javascript\" src=\"$js\" language=\"javascript\"></script>" \
	"{print} /js\/panel\/router\/vpn.js/{print line}" $i > $i.tmp && cat $i.tmp > $i && rm -f $i.tmp
grep -q -F "$js" $i
for l in es en cn; do
	p=$WWW/properties/Messages_$l.properties
	[ -z "$(tail -c1 $p)" ] || printf "\n" >> $p
	cat ${i18n}_$l.properties >> $p
	rm -f ${i18n}_$l.properties
done
'

webui_add() {
	local id="$1" fn="$2" js="$3" i18n="$4"; shift 4
	printf '%s' "$WEBUI_REMOTE" | "$RSH" "cat > /tmp/webui-edit.sh"
	for lang in es en cn; do
		"$RSH" "cat > /tmp/webui-i18n-${id}_${lang}.properties" < "$i18n/Messages_$lang.properties"
	done
	"$RSH" "sh /tmp/webui-edit.sh add '$id' '$fn' '$js' /tmp/webui-i18n-$id $*; rm -f /tmp/webui-edit.sh"
}

webui_remove() {
	local id="$1" js="$2"; shift 2
	printf '%s' "$WEBUI_REMOTE" | "$RSH" "cat > /tmp/webui-edit.sh"
	"$RSH" "sh /tmp/webui-edit.sh remove '$id' - '$js' - $*; rm -f /tmp/webui-edit.sh"
}
