#!/bin/sh
# tunvpn.cgi - web UI backend for the management OpenVPN tunnel (tunlite + openvpn). Same rules as netparam.cgi.
#
#   GET  /tunvpn.cgi                    -> JSON: settings (never the password) + live status
#   POST /tunvpn.cgi  <settings>        -> validate, save to uci tunvpn, restart openvpn, JSON
#   POST action=ca&ca_pem=..            -> install the server CA certificate, restart
#   POST action=restart                 -> reconnect now
#
# Security: valid web session (CGISID -> /tmp/cgisess_<id> with log_status=login) or 401; POST needs
# X-Requested-With (CSRF); every value is validated before it reaches uci. The password is write-only:
# blank keeps the stored value.

D=/data/tunvpn
CTL=/usr/sbin/tunvpn.sh; [ -x "$CTL" ] || CTL=$D/tunvpn.sh
PKG=tunvpn
LOG=/tmp/tunvpn.log

# lighttpd runs CGIs with a reduced PATH: uci, iptables, ip live in /sbin and /usr/sbin
export PATH=/usr/sbin:/usr/bin:/sbin:/bin

out() {
	printf 'Status: %s\r\nContent-Type: application/json\r\nCache-Control: no-store\r\n\r\n%s\n' "$1" "$2"
	exit 0
}
fail() { out "400 Bad Request" "{\"result\":\"fail\",\"error\":\"$1\"}"; }

sid="$(printf '%s' "$HTTP_COOKIE" | tr ';' '\n' | sed -n 's/^ *CGISID=\([A-Za-z0-9]*\).*/\1/p' | head -n1)"
if [ -z "$sid" ] || ! grep -q 'log_status=login' "/tmp/cgisess_$sid" 2>/dev/null; then
	out "401 Unauthorized" '{"result":"fail","error":"no_session"}'
fi

get() { uci -q get "$PKG.main.$1"; }
jstr() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\n\r\t'; }
urldecode() { printf '%b' "$(printf '%s' "$1" | sed 's/+/ /g; s/\\/%5C/g; s/%\([0-9A-Fa-f][0-9A-Fa-f]\)/\\x\1/g')"; }
param() { urldecode "$(printf '%s' "$BODY" | tr '&' '\n' | sed -n "s/^$1=//p" | head -n1)"; }
match() { printf '%s' "$1" | grep -Eq "$2"; }

status_json() {
	pid=""; [ -f /var/run/tunvpn.pid ] && kill -0 "$(cat /var/run/tunvpn.pid)" 2>/dev/null && pid=$(cat /var/run/tunvpn.pid)
	mod=false; grep -q ' tun$' /proc/misc && mod=true
	ip4="$(ip -4 -o addr show dev tun0 2>/dev/null | awk '{print $4}')"
	conn=false
	if [ -n "$pid" ] && [ -f "$LOG" ]; then
		last="$(grep -E 'Initialization Sequence Completed|process restarting|Restart pause|Connection reset|AUTH_FAILED' "$LOG" | tail -n1)"
		printf '%s' "$last" | grep -q 'Initialization Sequence Completed' && conn=true
	fi
	since="$(grep 'Initialization Sequence Completed' "$LOG" 2>/dev/null | tail -n1 | cut -c1-19)"
	rx=0; tx=0
	if [ -n "$ip4" ]; then
		rx=$(cat /sys/class/net/tun0/statistics/rx_bytes 2>/dev/null || echo 0)
		tx=$(cat /sys/class/net/tun0/statistics/tx_bytes 2>/dev/null || echo 0)
	fi
	logl=""
	if [ -f "$LOG" ]; then
		logl="$(tail -n 8 "$LOG" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\r\t' | awk '{printf "%s\"%s\"", (NR>1?",":""), $0}')"
	fi
	pw=false; [ -n "$(get password)" ] && pw=true
	ca=false; [ -s "$D/ca.crt" ] && ca=true
	printf '{"result":"ok","enabled":%s,"remote":"%s","port":%s,"proto":"%s","username":"%s","password_set":%s,"cipher":"%s","auth":"%s","route_nopull":%s,"ca_set":%s,"status":{"module":%s,"running":%s,"connected":%s,"ip":"%s","since":"%s","rx":%s,"tx":%s,"log":[%s]}}' \
		"$(get enabled || echo 0)" "$(jstr "$(get remote)")" "$(get port || echo 1194)" "$(jstr "$(get proto || echo tcp)")" \
		"$(jstr "$(get username)")" "$pw" "$(jstr "$(get cipher || echo AES-256-CBC)")" "$(jstr "$(get auth || echo SHA1)")" \
		"$(get route_nopull || echo 1)" "$ca" "$mod" "$([ -n "$pid" ] && echo true || echo false)" "$conn" \
		"$(jstr "$ip4")" "$(jstr "$since")" "$rx" "$tx" "$logl"
}

RE_HOST='^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$'
RE_USER='^[A-Za-z0-9._@-]{1,64}$'
RE_PASS='^[A-Za-z0-9!#$%()*+,./:;<=>?@^_{|}~-]{1,128}$'

case "$REQUEST_METHOD" in
GET) out "200 OK" "$(status_json)" ;;
POST) ;;
*) out "405 Method Not Allowed" '{"result":"fail","error":"method"}' ;;
esac

[ -n "$HTTP_X_REQUESTED_WITH" ] || out "403 Forbidden" '{"result":"fail","error":"csrf"}'
len="${CONTENT_LENGTH:-0}"
case "$len" in ''|*[!0-9]*) len=0 ;; esac
[ "$len" -gt 0 ] && [ "$len" -le 16384 ] || fail body
BODY="$(head -c "$len")"

case "$(param action)" in
restart)
	logger -t tunvpn "web: restart (from ${REMOTE_ADDR:-?})"
	sh "$CTL" restart >/dev/null 2>&1
	sleep 2
	out "200 OK" "$(status_json)"
	;;
ca)
	c="$(param ca_pem)"
	printf '%s' "$c" | grep -q -- '-----BEGIN CERTIFICATE-----' || fail ca
	printf '%s' "$c" | grep -q -- 'PRIVATE KEY' && fail ca
	mkdir -p "$D" && chmod 700 "$D"
	printf '%s\n' "$c" > "$D/ca.crt.tmp" && mv "$D/ca.crt.tmp" "$D/ca.crt" || fail write
	logger -t tunvpn "web: CA certificate replaced (from ${REMOTE_ADDR:-?})"
	sh "$CTL" restart >/dev/null 2>&1
	sleep 2
	out "200 OK" "$(status_json)"
	;;
'') ;;
*) fail action ;;
esac

en="$(param enabled)"; rem="$(param remote)"; port="$(param port)"; proto="$(param proto)"
user="$(param username)"; pass="$(param password)"; ciph="$(param cipher)"; auth="$(param auth)"; nop="$(param route_nopull)"

case "$en" in 0|1) ;; *) fail enabled ;; esac
case "$proto" in tcp|udp) ;; *) fail proto ;; esac
case "$ciph" in AES-256-CBC|AES-128-CBC|AES-256-GCM|AES-128-GCM|CHACHA20-POLY1305|none) ;; *) fail cipher ;; esac
case "$auth" in SHA1|SHA256|SHA512|none) ;; *) fail auth ;; esac
case "$nop" in 0|1) ;; *) fail route_nopull ;; esac
match "$port" '^[0-9]{1,5}$' && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || fail port
if [ "$en" = 1 ] || [ -n "$rem" ]; then match "$rem" "$RE_HOST" || fail remote; fi
[ "$en" = 1 ] && [ ! -s "$D/ca.crt" ] && fail ca_missing
[ -z "$user" ] || match "$user" "$RE_USER" || fail username
[ -z "$pass" ] || match "$pass" "$RE_PASS" || fail password
if [ -n "$user" ] && [ -z "$pass" ] && [ -z "$(get password)" ]; then fail password; fi

uci -q get $PKG.main >/dev/null || uci set $PKG.main=tunvpn
uci set $PKG.main.enabled="$en"
uci set $PKG.main.remote="$rem"
uci set $PKG.main.port="$port"
uci set $PKG.main.proto="$proto"
uci set $PKG.main.username="$user"
[ -n "$pass" ] && uci set $PKG.main.password="$pass"
[ -z "$user" ] && uci -q delete $PKG.main.password
uci set $PKG.main.cipher="$ciph"
uci set $PKG.main.auth="$auth"
uci set $PKG.main.route_nopull="$nop"
uci commit $PKG
logger -t tunvpn "web: settings saved enabled=$en remote=$rem:$port/$proto user=$user (from ${REMOTE_ADDR:-?})"

sh "$CTL" restart >/dev/null 2>&1
sleep 2
out "200 OK" "$(status_json)"
