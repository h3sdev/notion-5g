#!/bin/sh
# ocgw.sh - keeps omnicap-gw (captive portal gateway) running on the Notion 5G.
# Installed at /data/omnicap-gw/ocgw.sh, run every minute by cron (/etc/crontabs/root):
#   * * * * * /data/omnicap-gw/ocgw.sh check
# and registered as a fw3 include (firewall.ocgw, run with no argument on every firewall start/reload).
# Settings live in uci (package omnicapgw, edited from the web UI: Router > Advanced > Captive portal).
#
#   ocgw.sh check    start the daemon if enabled and not running; if it does not stay up, block (fail closed)
#   ocgw.sh restart  restart (reloads the settings; logged-in clients stay authorized)
#   ocgw.sh stop     stop the daemon (clients already logged in stay authorized)
#   ocgw.sh disable  stop and remove every firewall/tc rule (open network again); does not touch uci
#   ocgw.sh status   one-line status
#   ocgw.sh          (fw3 include) enabled and the portal rules are not there: block until the daemon is up
# CGI (lighttpd) and cron run with a reduced PATH: uci, iptables, tc, insmod live in /sbin and /usr/sbin
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
D=/data/omnicap-gw         # writable: certificate for the secure login (from omnicap or the web UI)
LOG=/tmp/omnicap-gw.log
# Binary: in the firmware image or, on a unit installed in the field, in $D.
BIN=/usr/sbin/omnicap-gw; [ -x "$BIN" ] || BIN=$D/omnicap-gw
mkdir -p "$D" && chmod 700 "$D"
TAG=omnicap-gw
DOWN=OCGW_DOWN             # the daemon deletes this chain once its own rules are in place
EXEMPT=0x800000/0x800000   # ebtables mark of the excluded ports (main.go exemptMark)

# the daemon only: "omnicap-gw -install" (CGI) and "-cleanup" have the same process name
pid() { for p in $(pidof omnicap-gw); do grep -q -e '-install' -e '-cleanup' /proc/$p/cmdline 2>/dev/null || echo $p; done; }
enabled() { [ "$(uci -q get omnicapgw.main.enabled)" = 1 ]; }

# block: forwarding from the LAN is rejected except for excluded ports and bypass MACs (the router itself,
# DHCP, DNS and the web UI stay reachable: INPUT is not touched). Idempotent.
block() {
	for ipt in iptables ip6tables; do
		$ipt -N $DOWN 2>/dev/null; $ipt -F $DOWN
		$ipt -A $DOWN -o br-lan -j RETURN
		$ipt -A $DOWN -m mark --mark $EXEMPT -j RETURN
		for m in $(uci -q get omnicapgw.main.bypass_mac); do $ipt -A $DOWN -m mac --mac-source "$m" -j RETURN; done
		$ipt -A $DOWN -j REJECT
		$ipt -C FORWARD -i br-lan -j $DOWN 2>/dev/null || $ipt -I FORWARD 1 -i br-lan -j $DOWN
	done
}
unblock() {
	for ipt in iptables ip6tables; do
		while $ipt -D FORWARD -i br-lan -j $DOWN 2>/dev/null; do :; done
		$ipt -F $DOWN 2>/dev/null; $ipt -X $DOWN 2>/dev/null
	done
}

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
'')
	# fw3 include: at boot (before cron starts the daemon) and after a firewall restart that flushed the
	# portal rules, the LAN stays closed until the daemon has put them back
	# (no exit here: fw3 may run the includes in one shell)
	if enabled && ! iptables -C FORWARD -i br-lan -j OCGW_FWD 2>/dev/null; then
		block
		# daemon up: it puts its rules back now (and lifts the block) instead of at its next 10 s tick
		[ -n "$(pid)" ] && wget -q -T 8 -O /dev/null http://127.0.0.1:8009/admin/reapply 2>/dev/null
	fi
	;;
check)
	enabled || exit 0
	if [ -z "$(pid)" ]; then
		start
		sleep 3
		if [ -z "$(pid)" ]; then
			block
			logger -t $TAG "daemon not running: LAN blocked until it starts (see $LOG)"
		fi
	fi
	# rotate in place: the daemon keeps the file open (O_APPEND), a mv would leave it writing to a deleted file
	[ -f "$LOG" ] && [ "$(wc -c < "$LOG")" -gt 262144 ] && tail -c 65536 "$LOG" > "$LOG.1" && cat "$LOG.1" > "$LOG"; rm -f "$LOG.1"
	;;
restart) stop; enabled && start ;;
start) start ;;
stop) stop ;;
disable)
	stop
	"$BIN" -uci omnicapgw -cleanup 2>/dev/null
	unblock
	rm -f /tmp/hosts/omnicap-gw; killall -HUP dnsmasq 2>/dev/null
	logger -t $TAG "disabled, rules removed"
	;;
status)
	n=$(ipset list ocgw_auth 2>/dev/null | sed -n '/Members:/,$p' | grep -v Members | grep -c ':')
	b=no; iptables -C FORWARD -i br-lan -j $DOWN 2>/dev/null && b=yes
	echo "enabled=$(enabled && echo yes || echo no) pid=$(pid) clients=$n blocked=$b"
	;;
*) echo "usage: $0 check|restart|start|stop|disable|status" >&2; exit 2 ;;
esac
