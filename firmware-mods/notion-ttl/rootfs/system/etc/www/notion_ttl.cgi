#!/bin/sh
# notion_ttl.cgi — lee/guarda la configuración de TTL desde la interfaz web.
#
#   GET  /notion_ttl.cgi                         -> JSON con config + reglas activas + contadores
#   POST /notion_ttl.cgi  enabled=1&ttl=64&ipv6=1 -> valida, guarda en uci notion_ttl, aplica, JSON
#
# Seguridad:
#   - Requiere una sesión web válida: cookie CGISID cuyo archivo /tmp/cgisess_<id> (libcgi, el mismo
#     que crea login.cgi) contenga log_status=login. Sin sesión -> 401.
#   - POST exige el header X-Requested-With (lo pone jQuery). Un formulario de otro sitio no puede
#     ponerlo sin preflight CORS, así que esto bloquea CSRF.
#   - Valores validados estrictamente (enteros, rangos) antes de tocar uci.

APPLY=/system/etc/firewall.notion-ttl.sh

out() { # status, json
	printf 'Status: %s\r\nContent-Type: application/json\r\nCache-Control: no-store\r\n\r\n%s\n' "$1" "$2"
	exit 0
}

# --- sesión -----------------------------------------------------------------------------------------
sid="$(printf '%s' "$HTTP_COOKIE" | tr ';' '\n' | sed -n 's/^ *CGISID=\([A-Za-z0-9]*\).*/\1/p' | head -n1)"
if [ -z "$sid" ] || ! grep -q 'log_status=login' "/tmp/cgisess_$sid" 2>/dev/null; then
	out "401 Unauthorized" '{"result":"fail","error":"no_session"}'
fi

# --- helpers ----------------------------------------------------------------------------------------
cfg() { uci -q get "notion_ttl.main.$1"; }

pkts() { # bin -> paquetes en la regla notion-ttl (vacío si no hay regla)
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

param() { # nombre -> valor crudo desde el body urlencoded (se valida después, nunca se interpreta)
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

	en="$(param enabled)"; ttl="$(param ttl)"; v6="$(param ipv6)"
	case "$en" in 0|1) ;; *) out "400 Bad Request" '{"result":"fail","error":"enabled"}' ;; esac
	case "$v6" in 0|1) ;; '') v6=1 ;; *) out "400 Bad Request" '{"result":"fail","error":"ipv6"}' ;; esac
	case "$ttl" in ''|*[!0-9]*) out "400 Bad Request" '{"result":"fail","error":"ttl"}' ;; esac
	# Quitar ceros a la izquierda con sed, NO con $((ttl+0)): el shell lee "064" como octal (=52).
	ttl="$(printf '%s' "$ttl" | sed 's/^0*//')"
	if [ -z "$ttl" ] || [ "${#ttl}" -gt 3 ] || [ "$ttl" -lt 1 ] || [ "$ttl" -gt 255 ]; then
		out "400 Bad Request" '{"result":"fail","error":"ttl"}'
	fi

	uci -q get notion_ttl.main >/dev/null || uci set notion_ttl.main=ttl
	uci set notion_ttl.main.enabled="$en"
	uci set notion_ttl.main.ttl="$ttl"
	uci set notion_ttl.main.ipv6="$v6"
	uci commit notion_ttl
	logger -t notion-ttl "web: enabled=$en ttl=$ttl ipv6=$v6 (desde ${REMOTE_ADDR:-?})"

	sh "$APPLY" apply >/dev/null 2>&1 || out "500 Internal Server Error" '{"result":"fail","error":"apply"}'
	out "200 OK" "$(status_json)"
	;;
*)
	out "405 Method Not Allowed" '{"result":"fail","error":"method"}'
	;;
esac
