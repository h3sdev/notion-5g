#!/bin/sh
# omnicapgw.cgi - web UI backend for the captive portal (omnicap-gw). Same rules as netparam.cgi.
#
#   GET  /omnicapgw.cgi                 -> JSON: settings (never the secrets) + live status from the daemon
#   POST /omnicapgw.cgi  <settings>     -> validate, save to uci omnicapgw, restart the daemon, JSON
#   POST action=cert&cert_pem=..&key_pem=..   -> install a certificate for the secure login, restart
#   POST action=fetchcert               -> download the certificate from the omnicap agent now
#   POST action=kick&mac=..             -> disconnect one client (RADIUS Stop, Admin-Reset)
#
# Security: valid web session (CGISID -> /tmp/cgisess_<id> with log_status=login) or 401; POST needs
# X-Requested-With (CSRF); every value is validated before it reaches uci. Secrets (RADIUS secret, agent
# token) are write-only: an empty field keeps the stored value.

D=/data/omnicap-gw
CTL=/usr/sbin/ocgw.sh; [ -x "$CTL" ] || CTL=$D/ocgw.sh
PKG=omnicapgw

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
jstr() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\n\r'; }
csv() { uci -q get "$PKG.main.$1" | tr ' ' ','; }
urldecode() { printf '%b' "$(printf '%s' "$1" | sed 's/+/ /g; s/\\/%5C/g; s/%\([0-9A-Fa-f][0-9A-Fa-f]\)/\\x\1/g')"; }
param() { urldecode "$(printf '%s' "$BODY" | tr '&' '\n' | sed -n "s/^$1=//p" | head -n1)"; }
match() { printf '%s' "$1" | grep -Eq "$2"; }

status_json() {
	st="$(wget -q -T 3 -O - http://127.0.0.1:8009/admin/status 2>/dev/null)"
	[ -n "$st" ] || st='{"running":false}'
	sec=false; [ -n "$(get radius_secret)" ] && sec=true
	tok=false; [ -n "$(get agent_token)" ] && tok=true
	printf '{"result":"ok","enabled":%s,"portal_url":"%s","nas_identifier":"%s","radius_server":"%s","radius_auth_port":%s,"radius_acct_port":%s,"secret_set":%s,"https":%s,"login_host":"%s","https_port":%s,"cert_source":"%s","agent_url":"%s","token_set":%s,"walled_garden":"%s","bypass_mac":"%s","status":%s}' \
		"$(get enabled || echo 0)" "$(jstr "$(get portal_url)")" "$(jstr "$(get nas_identifier)")" \
		"$(jstr "$(get radius_server)")" "$(get radius_auth_port || echo 1812)" "$(get radius_acct_port || echo 1813)" \
		"$sec" "$(get https || echo 0)" "$(jstr "$(get login_host)")" "$(get https_port || echo 8443)" \
		"$(jstr "$(get cert_source)")" "$(jstr "$(get agent_url)")" "$tok" \
		"$(jstr "$(csv walled_garden)")" "$(jstr "$(csv bypass_mac)")" "$st"
}

RE_URL='^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/-]*)?$'
RE_HOST='^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$'
RE_NAS='^[A-Za-z0-9_.:-]{1,64}$'
RE_SECRET='^[A-Za-z0-9!#$%()*+,./:;<=>?@^_{|}~-]{8,128}$'
RE_TOKEN='^[A-Za-z0-9._~+/=-]{8,255}$'
RE_WG='^[A-Za-z0-9.-]+(/[0-9]{1,2})?$'
RE_MAC='^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$'
port_ok() { match "$1" '^[0-9]{1,5}$' && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }

restart() { sh "$CTL" restart >/dev/null 2>&1; sleep 2; }

case "$REQUEST_METHOD" in
GET) out "200 OK" "$(status_json)" ;;
POST) ;;
*) out "405 Method Not Allowed" '{"result":"fail","error":"method"}' ;;
esac

[ -n "$HTTP_X_REQUESTED_WITH" ] || out "403 Forbidden" '{"result":"fail","error":"csrf"}'
len="${CONTENT_LENGTH:-0}"
case "$len" in ''|*[!0-9]*) len=0 ;; esac
[ "$len" -gt 0 ] && [ "$len" -le 32768 ] || fail body
BODY="$(head -c "$len")"

case "$(param action)" in
kick)
	mac="$(param mac)"; match "$mac" "$RE_MAC" || fail mac
	wget -q -T 5 -O /dev/null "http://127.0.0.1:8009/admin/kick?mac=$mac"
	logger -t omnicap-gw "web: kick $mac (from ${REMOTE_ADDR:-?})"
	out "200 OK" "$(status_json)"
	;;
fetchcert)
	r="$(wget -q -T 40 -O - http://127.0.0.1:8009/admin/fetchcert 2>/dev/null)"
	out "200 OK" "$(status_json)"
	;;
cert)
	c="$(param cert_pem)"; k="$(param key_pem)"
	printf '%s' "$c" | grep -q -- '-----BEGIN CERTIFICATE-----' || fail cert
	printf '%s' "$k" | grep -q -- '-----BEGIN .*PRIVATE KEY-----' || fail key
	mkdir -p "$D" && chmod 700 "$D"
	printf '%s\n' "$c" > "$D/login.crt.tmp" && printf '%s\n' "$k" > "$D/login.key.tmp" && chmod 600 "$D/login.key.tmp" \
		&& mv "$D/login.key.tmp" "$D/login.key" && mv "$D/login.crt.tmp" "$D/login.crt" || fail write
	uci set $PKG.main.cert_source=manual; uci commit $PKG
	logger -t omnicap-gw "web: certificate uploaded (from ${REMOTE_ADDR:-?})"
	restart
	out "200 OK" "$(status_json)"
	;;
'') ;;
*) fail action ;;
esac

en="$(param enabled)"; url="$(param portal_url)"; nas="$(param nas_identifier)"; srv="$(param radius_server)"
ap="$(param radius_auth_port)"; cp="$(param radius_acct_port)"; sec="$(param radius_secret)"
https="$(param https)"; lh="$(param login_host)"; hp="$(param https_port)"; src="$(param cert_source)"
aurl="$(param agent_url)"; tok="$(param agent_token)"; wg="$(param walled_garden)"; byp="$(param bypass_mac)"

case "$en" in 0|1) ;; *) fail enabled ;; esac
case "$https" in 0|1) ;; *) fail https ;; esac
case "$src" in manual|agent) ;; *) fail cert_source ;; esac
if [ "$en" = 1 ]; then
	match "$url" "$RE_URL" && [ "${#url}" -le 300 ] || fail portal_url
	match "$nas" "$RE_NAS" || fail nas_identifier
	match "$srv" "$RE_HOST" || fail radius_server
	[ -n "$sec" ] || [ -n "$(get radius_secret)" ] || fail radius_secret
fi
[ -z "$url" ] || { match "$url" "$RE_URL" && [ "${#url}" -le 300 ]; } || fail portal_url
[ -z "$nas" ] || match "$nas" "$RE_NAS" || fail nas_identifier
[ -z "$srv" ] || match "$srv" "$RE_HOST" || fail radius_server
port_ok "$ap" || fail radius_auth_port
port_ok "$cp" || fail radius_acct_port
[ -z "$sec" ] || match "$sec" "$RE_SECRET" || fail radius_secret
if [ "$https" = 1 ]; then match "$lh" "$RE_HOST" || fail login_host; fi
[ -z "$lh" ] || match "$lh" "$RE_HOST" || fail login_host
port_ok "$hp" || fail https_port
case "$hp" in 80|443|22|53|8002|8009) fail https_port ;; esac
if [ "$src" = agent ]; then
	match "$aurl" "$RE_URL" || fail agent_url
	[ -n "$tok" ] || [ -n "$(get agent_token)" ] || fail agent_token
fi
[ -z "$aurl" ] || match "$aurl" "$RE_URL" || fail agent_url
[ -z "$tok" ] || match "$tok" "$RE_TOKEN" || fail agent_token
wgl=""; for x in $(printf '%s' "$wg" | tr ', ' '\n\n'); do match "$x" "$RE_WG" || fail walled_garden; wgl="$wgl $x"; done
bpl=""; for x in $(printf '%s' "$byp" | tr ', ' '\n\n'); do match "$x" "$RE_MAC" || fail bypass_mac; bpl="$bpl $x"; done

uci -q get $PKG.main >/dev/null || uci set $PKG.main=omnicapgw
uci set $PKG.main.enabled="$en"
uci set $PKG.main.portal_url="$url"
uci set $PKG.main.nas_identifier="$nas"
uci set $PKG.main.radius_server="$srv"
uci set $PKG.main.radius_auth_port="$ap"
uci set $PKG.main.radius_acct_port="$cp"
[ -n "$sec" ] && uci set $PKG.main.radius_secret="$sec"
uci set $PKG.main.https="$https"
uci set $PKG.main.login_host="$lh"
uci set $PKG.main.https_port="$hp"
uci set $PKG.main.cert_source="$src"
uci set $PKG.main.agent_url="$aurl"
[ -n "$tok" ] && uci set $PKG.main.agent_token="$tok"
uci -q delete $PKG.main.walled_garden; for x in $wgl; do uci add_list $PKG.main.walled_garden="$x"; done
uci -q delete $PKG.main.bypass_mac; for x in $bpl; do uci add_list $PKG.main.bypass_mac="$x"; done
uci commit $PKG
logger -t omnicap-gw "web: settings saved enabled=$en https=$https nas=$nas (from ${REMOTE_ADDR:-?})"

if [ "$en" = 1 ]; then restart; else sh "$CTL" disable >/dev/null 2>&1; fi
out "200 OK" "$(status_json)"
