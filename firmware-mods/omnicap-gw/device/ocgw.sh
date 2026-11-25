#!/bin/sh
# ocgw.sh - keeps omnicap-gw (captive portal gateway) running on the Notion 5G.
# Installed at /data/omnicap-gw/ocgw.sh, run every minute by cron (/etc/crontabs/root):
#   * * * * * /data/omnicap-gw/ocgw.sh check
#
#   ocgw.sh check    start the daemon if enabled and not running
#   ocgw.sh start    start even when disabled
#   ocgw.sh stop     stop the daemon (clients already logged in stay authorized)
#   ocgw.sh disable  stop, remove every firewall/tc rule (open network) and disable
#   ocgw.sh status   one-line status
D=/data/omnicap-gw
LOG=/tmp/omnicap-gw.log
TAG=omnicap-gw

pid() { pidof omnicap-gw; }

start() {
	[ -n "$(pid)" ] && return 0
	(cd "$D" && ./omnicap-gw -config "$D/config.json" >> "$LOG" 2>&1 </dev/null &)
	logger -t $TAG "started"
}

case "$1" in
check)
	[ -f "$D/enabled" ] || exit 0
	start
	[ -f "$LOG" ] && [ "$(wc -c < "$LOG")" -gt 262144 ] && tail -c 65536 "$LOG" > "$LOG.1" && mv "$LOG.1" "$LOG"
	;;
start) touch "$D/enabled"; start ;;
stop) p=$(pid); [ -n "$p" ] && kill $p ;;
disable)
	rm -f "$D/enabled"
	p=$(pid); [ -n "$p" ] && kill $p
	sleep 1
	"$D/omnicap-gw" -config "$D/config.json" -cleanup
	logger -t $TAG "disabled, rules removed"
	;;
status)
	n=$(ipset list ocgw_auth 2>/dev/null | sed -n '/Members:/,$p' | grep -v Members | grep -c ':')
	echo "enabled=$([ -f "$D/enabled" ] && echo yes || echo no) pid=$(pid) clients=$n"
	;;
*) echo "usage: $0 check|start|stop|disable|status" >&2; exit 2 ;;
esac
