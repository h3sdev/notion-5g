#!/bin/sh
# netparam: sets the TTL (IPv4) / Hop Limit (IPv6) of traffic leaving through the cellular interface.
#
# Value and state are read from /etc/config/netparam (uci, section netparam.main):
#   enabled  0|1        apply or not
#   value    1..255     TTL / Hop Limit to set (64 = same as an Android/iOS handset)
#   ipv6     0|1        also set the IPv6 Hop Limit
#   inbound  0|1        also rewrite incoming packets (INPUT + FORWARD), so a ping shows the configured value.
#                       Cosmetic, for confirming the setting: the operator only ever sees outgoing packets.
#   iface    ccinet+    egress interface(s) (iptables pattern)
#
# fw3 runs this on every start/reload (uci firewall.netparam: type=script, reload=1), because every
# 4G<->5G re-attach does an ifup of wan0 -> hotplug -> fw3 reload, which flushes manually added rules.
# /www/netparam.cgi also runs it after saving from the web UI.
#
# Usage: firewall.netparam.sh [apply|remove|status]

TAG=netparam

cfg() { uci -q get "netparam.main.$1"; }

ENABLED="$(cfg enabled)"; [ -n "$ENABLED" ] || ENABLED=1
TTL="$(cfg value)";       [ -n "$TTL" ] || TTL=64
IPV6="$(cfg ipv6)";       [ -n "$IPV6" ] || IPV6=1
IFACE="$(cfg iface)";     [ -n "$IFACE" ] || IFACE='ccinet+'
INBOUND="$(cfg inbound)"; [ -n "$INBOUND" ] || INBOUND=1

# Delete EVERY rule tagged netparam, in any mangle chain, whatever value it was created with.
flush() {
	for bin in iptables ip6tables; do
		$bin -t mangle -S 2>/dev/null | grep -- "--comment $TAG " | sed 's/^-A /-D /' |
		while read -r rule; do
			# shellcheck disable=SC2086
			$bin -t mangle $rule 2>/dev/null
		done
	done
}

valid() {
	case "$TTL" in ''|*[!0-9]*) return 1 ;; esac
	[ "$TTL" -ge 1 ] && [ "$TTL" -le 255 ] || return 1
	case "$IFACE" in ''|*[!A-Za-z0-9_.+-]*) return 1 ;; esac
	return 0
}

case "${1:-apply}" in
remove)
	flush
	logger -t $TAG "rules removed"
	;;
status)
	echo "enabled=$ENABLED value=$TTL ipv6=$IPV6 iface=$IFACE inbound=$INBOUND"
	for bin in iptables ip6tables; do
		for chain in POSTROUTING INPUT FORWARD; do
			$bin -t mangle -L $chain -v -n -x 2>/dev/null | grep "$TAG" | sed "s/^/$chain /"
		done
	done
	;;
*)
	flush
	if [ "$ENABLED" != 1 ]; then
		logger -t $TAG "disabled (netparam.main.enabled=$ENABLED)"
		exit 0
	fi
	if ! valid; then
		logger -t $TAG "invalid config (value=$TTL iface=$IFACE), not applied"
		exit 1
	fi
	# Outgoing: POSTROUTING runs after the forwarding decrement, so the operator sees exactly $TTL.
	iptables -t mangle -I POSTROUTING -o "$IFACE" -m comment --comment $TAG -j TTL --ttl-set "$TTL"
	[ "$IPV6" = 1 ] && ip6tables -t mangle -I POSTROUTING -o "$IFACE" -m comment --comment $TAG -j HL --hl-set "$TTL"
	# Incoming (optional): INPUT for packets to the device itself (Diagnostics ping), FORWARD for packets
	# to LAN clients. FORWARD runs after the kernel decrements, so clients also see exactly $TTL.
	if [ "$INBOUND" = 1 ]; then
		for chain in INPUT FORWARD; do
			iptables -t mangle -I $chain -i "$IFACE" -m comment --comment $TAG -j TTL --ttl-set "$TTL"
			[ "$IPV6" = 1 ] && ip6tables -t mangle -I $chain -i "$IFACE" -m comment --comment $TAG -j HL --hl-set "$TTL"
		done
	fi
	logger -t $TAG "value=$TTL applied on $IFACE (ipv6=$IPV6 inbound=$INBOUND)"
	;;
esac
exit 0
