#!/bin/sh
# ocgw.sh - keeps omnicap-gw (captive portal gateway) running on the Notion 5G.
# Installed at /data/omnicap-gw/ocgw.sh, run every minute by cron (/etc/crontabs/root):
#   * * * * * /data/omnicap-gw/ocgw.sh check
# Settings live in uci (package omnicapgw, edited from the web UI: Router > Advanced > Captive portal).
#
#   ocgw.sh check    start the daemon if enabled and not running
#   ocgw.sh restart  restart (reloads the settings; logged-in clients stay authorized)
#   ocgw.sh stop     stop the daemon (clients already logged in stay authorized)
#   ocgw.sh disable  stop and remove every firewall/tc rule (open network again); does not touch uci
#   ocgw.sh status   one-line status
# CGI (lighttpd) and cron run with a reduced PATH: uci, iptables, tc, insmod live in /sbin and /usr/sbin
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
D=/data/omnicap-gw         # writable: certificate for the secure login (from omnicap or the web UI)
LOG=/tmp/omnicap-gw.log
# Binary: in the firmware image or, on a unit installed in the field, in $D.
BIN=/usr/sbin/omnicap-gw; [ -x "$BIN" ] || BIN=$D/omnicap-gw
mkdir -p "$D" && chmod 700 "$D"
TAG=omnicap-gw

pid() { pidof omnicap-gw; }
enabled() { [ "$(uci -q get omnicapgw.main.enabled)" = 1 ]; }

start() {
	[ -n "$(pid)" ] && return 0
	(cd "$D" && "$BIN" -uci omnicapgw >> "$LOG" 2>&1 </dev/null &)
	logger -t $TAG "started"
}

stop() {
	p=$(pid)
	[ -n "$p" ] || return 0
	kill $p
	for i in 1 2 3 4 5; do [ -z "$(pid)" ] && return 0; sleep 1; done
	kill -9 $p 2>/dev/null
}

case "$1" in
check)
	enabled || exit 0
	start
	[ -f "$LOG" ] && [ "$(wc -c < "$LOG")" -gt 262144 ] && tail -c 65536 "$LOG" > "$LOG.1" && mv "$LOG.1" "$LOG"
	;;
restart) stop; enabled && start ;;
start) start ;;
stop) stop ;;
disable)
	stop
	"$BIN" -uci omnicapgw -cleanup 2>/dev/null
	rm -f /tmp/hosts/omnicap-gw; killall -HUP dnsmasq 2>/dev/null
	logger -t $TAG "disabled, rules removed"
	;;
status)
	n=$(ipset list ocgw_auth 2>/dev/null | sed -n '/Members:/,$p' | grep -v Members | grep -c ':')
	echo "enabled=$(enabled && echo yes || echo no) pid=$(pid) clients=$n"
	;;
*) echo "usage: $0 check|restart|start|stop|disable|status" >&2; exit 2 ;;
esac
