#!/bin/sh
# notion_ttl.cgi - reads/saves the outbound TTL setting for the web UI.
#
#   GET  /notion_ttl.cgi                          -> JSON: config + active rules + packet counters
#   POST /notion_ttl.cgi  enabled=1&ttl=64&ipv6=1 -> validate, save to uci notion_ttl, apply, JSON
#   POST /notion_ttl.cgi  action=restore          -> back to the factory defaults (enabled, 64, IPv6), apply
#   POST /notion_ttl.cgi  action=remove           -> disable and delete the rules (traffic leaves unmodified)
#
# Security:
#   - Requires a valid web session: the CGISID cookie must point to /tmp/cgisess_<id> (libcgi, the same
#     file login.cgi creates) containing log_status=login. No session -> 401.
#   - POST requires the X-Requested-With header (jQuery sets it). A form on another site cannot set it
#     without a CORS preflight, so this blocks CSRF.
#   - Values are strictly validated (integers, ranges) before touching uci.

APPLY=/system/etc/firewall.notion-ttl.sh
# Factory defaults, the same as rootfs/system/etc/config/notion_ttl and firewall.notion-ttl.sh.
DEF_TTL=64
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
cfg() { uci -q get "notion_ttl.main.$1"; }

pkts() { # bin -> packet count of the notion-ttl rule (empty if there is no rule)
	$1 -t mangle -L POSTROUTING -v -n -x 2>/dev/null | awk '/notion-ttl/ {print $1; exit}'
}

status_json() {
	en="$(cfg enabled)"; [ -n "$en" ] || en=1
	ttl="$(cfg ttl)";    [ -n "$ttl" ] || ttl=64
	v6="$(cfg ipv6)";    [ -n "$v6" ] || v6=1
	ifc="$(cfg iface)";  [ -n "$ifc" ] || ifc='ccinet+'
	p4="$(pkts iptables)"; p6="$(pkts ip6tables)"
	a4=false; [ -n "$p4" ] && a4=true
	a6=false; [ -n "$p6" ] && a6=true
	printf '{"result":"ok","enabled":%s,"ttl":%s,"ipv6":%s,"iface":"%s","active_v4":%s,"active_v6":%s,"pkts_v4":%s,"pkts_v6":%s}' \
		"$en" "$ttl" "$v6" "$ifc" "$a4" "$a6" "${p4:-0}" "${p6:-0}"
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
		uci -q get notion_ttl.main >/dev/null || uci set notion_ttl.main=ttl
		uci set notion_ttl.main.enabled="$en"
		uci set notion_ttl.main.ttl="$DEF_TTL"
		uci set notion_ttl.main.ipv6="$DEF_IPV6"
		uci set notion_ttl.main.iface="$DEF_IFACE"
		uci commit notion_ttl
		logger -t notion-ttl "web: $action (enabled=$en ttl=$DEF_TTL ipv6=$DEF_IPV6) (from ${REMOTE_ADDR:-?})"
		sh "$APPLY" apply >/dev/null 2>&1 || out "500 Internal Server Error" '{"result":"fail","error":"apply"}'
		out "200 OK" "$(status_json)"
		;;
	*) out "400 Bad Request" '{"result":"fail","error":"action"}' ;;
	esac

	en="$(param enabled)"; ttl="$(param ttl)"; v6="$(param ipv6)"
	case "$en" in 0|1) ;; *) out "400 Bad Request" '{"result":"fail","error":"enabled"}' ;; esac
	case "$v6" in 0|1) ;; '') v6=1 ;; *) out "400 Bad Request" '{"result":"fail","error":"ipv6"}' ;; esac
	case "$ttl" in ''|*[!0-9]*) out "400 Bad Request" '{"result":"fail","error":"ttl"}' ;; esac
	# Strip leading zeros with sed, NOT with $((ttl+0)): the shell reads "064" as octal (= 52).
	ttl="$(printf '%s' "$ttl" | sed 's/^0*//')"
	if [ -z "$ttl" ] || [ "${#ttl}" -gt 3 ] || [ "$ttl" -lt 1 ] || [ "$ttl" -gt 255 ]; then
		out "400 Bad Request" '{"result":"fail","error":"ttl"}'
	fi

	uci -q get notion_ttl.main >/dev/null || uci set notion_ttl.main=ttl
	uci set notion_ttl.main.enabled="$en"
	uci set notion_ttl.main.ttl="$ttl"
	uci set notion_ttl.main.ipv6="$v6"
	uci commit notion_ttl
	logger -t notion-ttl "web: enabled=$en ttl=$ttl ipv6=$v6 (from ${REMOTE_ADDR:-?})"

	sh "$APPLY" apply >/dev/null 2>&1 || out "500 Internal Server Error" '{"result":"fail","error":"apply"}'
	out "200 OK" "$(status_json)"
	;;
*)
	out "405 Method Not Allowed" '{"result":"fail","error":"method"}'
	;;
esac
