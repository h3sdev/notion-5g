#!/bin/sh
# netparam.cgi - reads/saves the "network parameter" setting for the web UI (see OEM-SPEC.md).
#
#   GET  /netparam.cgi                          -> JSON: config + active rules + packet counters
#   POST /netparam.cgi  enabled=1&value=64&ipv6=1 -> validate, save to uci netparam, apply, JSON
#   POST /netparam.cgi  action=restore          -> back to the factory defaults (enabled, 64, IPv6), apply
#   POST /netparam.cgi  action=remove           -> disable and delete the rules (traffic leaves unmodified)
#
# Security:
#   - Requires a valid web session: the CGISID cookie must point to /tmp/cgisess_<id> (libcgi, the same
#     file login.cgi creates) containing log_status=login. No session -> 401.
#   - POST requires the X-Requested-With header (jQuery sets it). A form on another site cannot set it
#     without a CORS preflight, so this blocks CSRF.
#   - Values are strictly validated (integers, ranges) before touching uci.

APPLY=/system/etc/firewall.netparam.sh
# Factory defaults, the same as rootfs/system/etc/config/netparam and firewall.netparam.sh.
DEF_VALUE=64
DEF_IPV6=1
DEF_IFACE='ccinet+'

out() { # status, json
	printf 'Status: %s\r\nContent-Type: application/json\r\nCache-Control: no-store\r\n\r\n%s\n' "$1" "$2"
	exit 0
}

# --- session ----------------------------------------------------------------------------------------
sid="$(printf '%s' "$HTTP_COOKIE" | tr ';' '\n' | sed -n 's/^ *CGISID=\([A-Za-z0-9]*\).*/\1/p' | head -n1)"
if [ -z "$sid" ] || ! grep -q 'log_status=login' "/tmp/cgisess_$sid" 2>/dev/null; then
	out "401 Unauthorized" '{"result":"fail","error":"no_session"}'
fi

# --- helpers ----------------------------------------------------------------------------------------
cfg() { uci -q get "netparam.main.$1"; }

pkts() { # bin -> packet count of the netparam rule (empty if there is no rule)
	$1 -t mangle -L POSTROUTING -v -n -x 2>/dev/null | awk '/netparam/ {print $1; exit}'
}

status_json() {
	en="$(cfg enabled)"; [ -n "$en" ] || en=1
	val="$(cfg value)";    [ -n "$val" ] || val=64
	v6="$(cfg ipv6)";    [ -n "$v6" ] || v6=1
	ifc="$(cfg iface)";  [ -n "$ifc" ] || ifc='ccinet+'
	p4="$(pkts iptables)"; p6="$(pkts ip6tables)"
	a4=false; [ -n "$p4" ] && a4=true
	a6=false; [ -n "$p6" ] && a6=true
	printf '{"result":"ok","enabled":%s,"value":%s,"ipv6":%s,"iface":"%s","active_v4":%s,"active_v6":%s,"pkts_v4":%s,"pkts_v6":%s}' \
		"$en" "$val" "$v6" "$ifc" "$a4" "$a6" "${p4:-0}" "${p6:-0}"
}

param() { # name -> raw value from the urlencoded body (validated later, never evaluated)
	printf '%s' "$BODY" | tr '&' '\n' | sed -n "s/^$1=//p" | head -n1
}

# --- dispatch ---------------------------------------------------------------------------------------
case "$REQUEST_METHOD" in
GET)
	out "200 OK" "$(status_json)"
	;;
POST)
	[ -n "$HTTP_X_REQUESTED_WITH" ] || out "403 Forbidden" '{"result":"fail","error":"csrf"}'
	len="${CONTENT_LENGTH:-0}"
	case "$len" in ''|*[!0-9]*) len=0 ;; esac
	[ "$len" -gt 0 ] && [ "$len" -le 256 ] || out "400 Bad Request" '{"result":"fail","error":"body"}'
	BODY="$(head -c "$len")"

	action="$(param action)"
	case "$action" in
	'') ;;
	restore|remove)
		[ "$action" = restore ] && en=1 || en=0
		uci -q get netparam.main >/dev/null || uci set netparam.main=netparam
		uci set netparam.main.enabled="$en"
		uci set netparam.main.value="$DEF_VALUE"
		uci set netparam.main.ipv6="$DEF_IPV6"
		uci set netparam.main.iface="$DEF_IFACE"
		uci commit netparam
		logger -t netparam "web: $action (enabled=$en value=$DEF_VALUE ipv6=$DEF_IPV6) (from ${REMOTE_ADDR:-?})"
		sh "$APPLY" apply >/dev/null 2>&1 || out "500 Internal Server Error" '{"result":"fail","error":"apply"}'
		out "200 OK" "$(status_json)"
		;;
	*) out "400 Bad Request" '{"result":"fail","error":"action"}' ;;
	esac

	en="$(param enabled)"; val="$(param value)"; v6="$(param ipv6)"
	case "$en" in 0|1) ;; *) out "400 Bad Request" '{"result":"fail","error":"enabled"}' ;; esac
	case "$v6" in 0|1) ;; '') v6=1 ;; *) out "400 Bad Request" '{"result":"fail","error":"ipv6"}' ;; esac
	case "$val" in ''|*[!0-9]*) out "400 Bad Request" '{"result":"fail","error":"value"}' ;; esac
	# Strip leading zeros with sed, NOT with $((x+0)): the shell reads "064" as octal (= 52).
	val="$(printf '%s' "$val" | sed 's/^0*//')"
	if [ -z "$val" ] || [ "${#val}" -gt 3 ] || [ "$val" -lt 1 ] || [ "$val" -gt 255 ]; then
		out "400 Bad Request" '{"result":"fail","error":"value"}'
	fi

	uci -q get netparam.main >/dev/null || uci set netparam.main=netparam
	uci set netparam.main.enabled="$en"
	uci set netparam.main.value="$val"
	uci set netparam.main.ipv6="$v6"
	uci commit netparam
	logger -t netparam "web: enabled=$en value=$val ipv6=$v6 (from ${REMOTE_ADDR:-?})"

	sh "$APPLY" apply >/dev/null 2>&1 || out "500 Internal Server Error" '{"result":"fail","error":"apply"}'
	out "200 OK" "$(status_json)"
	;;
*)
	out "405 Method Not Allowed" '{"result":"fail","error":"method"}'
	;;
esac
