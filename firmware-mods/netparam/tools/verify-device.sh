#!/bin/sh
# verify-device.sh - checks a unit against REQUIREMENTS.md. Runs ON the device (busybox sh).
#   from a PC:  NOTION_PASS=<root password> tools/verify.sh
#   or:         tools/rsh.sh 'sh -s' < tools/verify-device.sh
# Prints one PASS/FAIL line per check and a summary. Read-only, except check R7, which runs "fw3 reload"
# (the same thing every 4G/5G re-attach does).
E=/system/etc
W=$E/www
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { # description, command...
	d=$1; shift
	if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi
}
mode() { ls -l "$1" 2>/dev/null | cut -c1-10; }

echo "== R1 new files"
for f in config/netparam:-rw-r--r-- firewall.netparam.sh:-rwxr-xr-x www/netparam.cgi:-rwxr-xr-x \
         www/html/router/netparam.html:-rw-r--r-- www/js/panel/router/netparam.js:-rw-r--r--; do
	p=$E/${f%%:*}; m=${f##*:}
	if [ -f "$p" ] && [ "$(mode "$p")" = "$m" ]; then pass "R1 $p ($m)"; else fail "R1 $p: expected $m, got '$(mode "$p")'"; fi
done

echo "== R2 existing web files"
n=$(grep -c 'src="js/panel/router/netparam.js"' $W/index.html)
[ "$n" = 1 ] && pass "R2 index.html loads netparam.js once" || fail "R2 index.html: netparam.js loaded $n times (expected 1)"
a=$(grep -n 'js/panel/router/sipalg.js' $W/index.html | cut -d: -f1); b=$(grep -n 'js/panel/router/netparam.js' $W/index.html | cut -d: -f1)
[ -n "$a" ] && [ "$b" = $((a + 1)) ] && pass "R2 netparam.js line right after sipalg.js" || fail "R2 netparam.js line is not right after sipalg.js"
for l in es en cn; do
	n=$(grep -c '^lt_NetParam_' $W/properties/Messages_$l.properties)
	[ "$n" = 15 ] && pass "R2 Messages_$l.properties: 15 lt_NetParam_* strings" || fail "R2 Messages_$l.properties: $n lt_NetParam_* strings (expected 15)"
done
check "R2 xml/ui_mifi.xml has no netparam menu entry" sh -c "! grep -qi netparam $W/xml/ui_mifi.xml"

echo "== R3 configuration"
check "R3 uci netparam.main exists" uci -q get netparam.main
for o in enabled value ipv6 iface inbound; do
	v=$(uci -q get netparam.main.$o); [ -n "$v" ] && pass "R3 netparam.main.$o = $v" || fail "R3 netparam.main.$o missing"
done
[ "$(uci -q get firewall.netparam)" = include ] && pass "R3 firewall.netparam is an include" || fail "R3 firewall.netparam include missing"
[ "$(uci -q get firewall.netparam.path)" = $E/firewall.netparam.sh ] && pass "R3 include path" || fail "R3 include path: $(uci -q get firewall.netparam.path)"
[ "$(uci -q get firewall.netparam.reload)" = 1 ] && pass "R3 include reload=1" || fail "R3 include reload must be 1"

echo "== R4 kernel support"
check "R4 iptables TTL target" grep -qx TTL /proc/net/ip_tables_targets
check "R4 ip6tables HL target" grep -qx HL /proc/net/ip6_tables_targets

rules_ok() { # expected rule count for the current config
	en=$(uci -q get netparam.main.enabled); v=$(uci -q get netparam.main.value)
	v6=$(uci -q get netparam.main.ipv6); in=$(uci -q get netparam.main.inbound)
	n4=$(iptables -t mangle -S | grep -- '--comment netparam ' | grep -c -- "--ttl-set $v\$")
	n6=$(ip6tables -t mangle -S | grep -- '--comment netparam ' | grep -c -- "--hl-set $v\$")
	t4=$(iptables -t mangle -S | grep -c -- '--comment netparam '); t6=$(ip6tables -t mangle -S | grep -c -- '--comment netparam ')
	if [ "$en" != 1 ]; then e4=0; e6=0; else
		e4=1; [ "$in" = 1 ] && e4=3
		e6=0; [ "$v6" = 1 ] && e6=$e4
	fi
	echo "enabled=$en value=$v ipv6=$v6 inbound=$in: IPv4 rules $n4/$t4 (expected $e4), IPv6 rules $n6/$t6 (expected $e6)"
	[ "$n4" = "$e4" ] && [ "$t4" = "$e4" ] && [ "$n6" = "$e6" ] && [ "$t6" = "$e6" ]
}

echo "== R5 rules"
out=$(rules_ok) && pass "R5 $out" || fail "R5 $out"

echo "== R6 ping shows the value (needs mobile data)"
if [ "$(uci -q get netparam.main.enabled)" = 1 ] && [ "$(uci -q get netparam.main.inbound)" = 1 ]; then
	t=$(ping -c 1 -W 3 8.8.8.8 2>/dev/null | sed -n 's/.*ttl=\([0-9]*\).*/\1/p')
	if [ -z "$t" ]; then echo "SKIP  R6 no reply from 8.8.8.8 (no mobile data?)"
	elif [ "$t" = "$(uci -q get netparam.main.value)" ]; then pass "R6 ping 8.8.8.8 shows ttl=$t"
	else fail "R6 ping 8.8.8.8 shows ttl=$t, expected $(uci -q get netparam.main.value)"; fi
else echo "SKIP  R6 (feature disabled or inbound=0)"; fi

echo "== R7 rules survive fw3 reload"
fw3 -q reload >/dev/null 2>&1
out=$(rules_ok) && pass "R7 after fw3 reload: $out" || fail "R7 after fw3 reload: $out"

echo "== R8 nothing visible names the feature"
for f in $W/js/panel/router/netparam.js $W/html/router/netparam.html; do
	check "R8 $f has no 'ttl' / 'hop limit'" sh -c "! grep -qi -e ttl -e 'hop limit' $f"
done
for l in es en cn; do
	check "R8 Messages_$l.properties: lt_NetParam_* strings have no 'ttl' / 'hop limit'" \
		sh -c "! grep '^lt_NetParam_' $W/properties/Messages_$l.properties | grep -qi -e ttl -e 'hop limit'"
done

echo
if [ "$fails" = 0 ]; then echo "ALL OK"; else echo "$fails FAILED"; fi
exit "$fails"
