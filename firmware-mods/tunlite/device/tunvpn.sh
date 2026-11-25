#!/bin/sh
# tunvpn.sh - keeps the management OpenVPN tunnel of the Notion 5G up.
# Installed at /data/tunvpn/tunvpn.sh, run every minute by cron (/etc/crontabs/root):
#   * * * * * /data/tunvpn/tunvpn.sh check
# /etc/rc.d is read-only on this firmware, so cron is the boot hook and the watchdog at the same time.
#
#   tunvpn.sh check   load tunlite.ko if needed, start openvpn if it is not running (enabled only)
#   tunvpn.sh start   same as check, even when disabled
#   tunvpn.sh stop    stop openvpn (the module stays loaded)
#   tunvpn.sh status  one-line status
#
# Files in /data/tunvpn: tunlite.ko, openvpn, client.conf, ca.crt, auth.txt (600), enabled (flag file).
D=/data/tunvpn
PIDF=/var/run/tunvpn.pid
TAG=tunvpn

running() { [ -f "$PIDF" ] && kill -0 "$(cat "$PIDF")" 2>/dev/null; }

load_module() {
	grep -q '^tunlite ' /proc/modules && return 0
	insmod "$D/tunlite.ko" || { logger -t $TAG "insmod tunlite.ko failed"; return 1; }
	logger -t $TAG "tunlite loaded"
}

start() {
	load_module || return 1
	[ -c /dev/net/tun ] || { mkdir -p /dev/net; mknod /dev/net/tun c 10 200; }
	running && return 0
	"$D/openvpn" --config "$D/client.conf" --cd "$D" --daemon tunvpn --writepid "$PIDF" \
		--log-append /tmp/tunvpn.log || { logger -t $TAG "openvpn failed to start"; return 1; }
	logger -t $TAG "openvpn started"
}

case "$1" in
check)
	[ -f "$D/enabled" ] || exit 0
	start
	# keep the log small (RAM-backed /tmp)
	[ -f /tmp/tunvpn.log ] && [ "$(wc -c < /tmp/tunvpn.log)" -gt 262144 ] && tail -c 65536 /tmp/tunvpn.log > /tmp/tunvpn.log.1 && mv /tmp/tunvpn.log.1 /tmp/tunvpn.log
	;;
start) start ;;
stop)
	running && kill "$(cat "$PIDF")"
	rm -f "$PIDF"
	;;
status)
	m=no; grep -q '^tunlite ' /proc/modules && m=yes
	p=no; running && p="yes(pid $(cat "$PIDF"))"
	a=$(ip -4 -o addr show dev tun0 2>/dev/null | awk '{print $4}')
	echo "enabled=$([ -f "$D/enabled" ] && echo yes || echo no) module=$m openvpn=$p tun0=${a:-none}"
	;;
*) echo "usage: $0 check|start|stop|status" >&2; exit 2 ;;
esac
