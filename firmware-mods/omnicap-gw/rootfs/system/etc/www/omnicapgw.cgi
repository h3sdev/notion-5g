#!/bin/sh
# omnicapgw.cgi - web UI backend for the captive portal (omnicap-gw). Same rules as netparam.cgi.
#
#   GET  /omnicapgw.cgi                 -> JSON: settings (never the secrets) + live status from the daemon
#   POST /omnicapgw.cgi  <settings>     -> validate, save to uci omnicapgw, restart the daemon, JSON
#   POST action=cert&cert_pem=..&key_pem=..   -> install a certificate for the secure login, restart
#   POST action=fetchcert               -> download the certificate from the omnicap agent now
#   POST action=kick&mac=..             -> disconnect one client (RADIUS Stop, Admin-Reset)
#   POST action=register&install_url=..&identity=..  -> register this unit in omnicap with an install key
#                                          (omnicap panel > location > install) and fill in every setting
#
# Security: valid web session (CGISID -> /tmp/cgisess_<id> with log_status=login) or 401; POST needs
# X-Requested-With (CSRF); every value is validated before it reaches uci. Secrets (RADIUS secret, agent
# token) are write-only: an empty field keeps the stored value.

D=/data/omnicap-gw
CTL=/usr/sbin/ocgw.sh; [ -x "$CTL" ] || CTL=$D/ocgw.sh
BIN=/usr/sbin/omnicap-gw; [ -x "$BIN" ] || BIN=$D/omnicap-gw
PKG=omnicapgw

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
jstr() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\n\r'; }
csv() { uci -q get "$PKG.main.$1" | tr ' ' ','; }
urldecode() { printf '%b' "$(printf '%s' "$1" | sed 's/+/ /g; s/\\/%5C/g; s/%\([0-9A-Fa-f][0-9A-Fa-f]\)/\\x\1/g')"; }
param() { urldecode "$(printf '%s' "$BODY" | tr '&' '\n' | sed -n "s/^$1=//p" | head -n1)"; }
match() { printf '%s' "$1" | grep -Eq "$2"; }

# lan_ports: every port of the LAN bridge, read from the device (nothing assumed), one per line:
#   ifname <TAB> kind (wifi|cable|usb|other) <TAB> band <TAB> guest <TAB> enabled <TAB> ssid
# Wi-Fi: wifi-iface sections of uci wireless attached to network lan (the first SSID of each radio is the
# main one, the others are guest SSIDs). Wired: uci network.lan ifname + current bridge members that exist.
lan_ports() {
	wl=" "; devs=" "
	for s in $(uci -q show wireless | sed -n 's/^wireless\.\([^.=]*\)=wifi-iface$/\1/p'); do
		[ "$(uci -q get wireless.$s.network)" = lan ] || continue
		i="$(uci -q get wireless.$s.ifname)"; [ -n "$i" ] || continue
		dev="$(uci -q get wireless.$s.device)"
		g=false; case "$devs" in *" $dev "*) g=true ;; *) devs="$devs$dev " ;; esac
		e=true; [ "$(uci -q get wireless.$s.disabled)" = 1 ] && e=false
		printf '%s\twifi\t%s\t%s\t%s\t%s\n' "$i" "$(uci -q get wireless.$dev.band)" "$g" "$e" "$(uci -q get wireless.$s.ssid)"
		wl="$wl$i "
	done
	for i in $(uci -q get network.lan.ifname) $(ls /sys/class/net/br-lan/brif 2>/dev/null); do
		case "$wl" in *" $i "*) continue ;; esac
		[ -e "/sys/class/net/$i" ] || continue
		wl="$wl$i "
		k=other; case "$i" in eth*) k=cable ;; usb*) k=usb ;; esac
		printf '%s\t%s\t-\tfalse\ttrue\t-\n' "$i" "$k"
	done
}

status_json() {
	st="$(wget -q -T 3 -O - http://127.0.0.1:8009/admin/status 2>/dev/null)"
	[ -n "$st" ] || st='{"running":false}'
	sec=false; [ -n "$(get radius_secret)" ] && sec=true
	tok=false; [ -n "$(get agent_token)" ] && tok=true
	ifj="$(lan_ports | while IFS='	' read -r i k b g e ss; do
		printf '{"if":"%s","kind":"%s","band":"%s","guest":%s,"enabled":%s,"ssid":"%s"},' \
			"$(jstr "$i")" "$k" "$(jstr "$b")" "$g" "$e" "$(jstr "$ss")"
	done)"
	printf '{"result":"ok","enabled":%s,"portal_url":"%s","nas_identifier":"%s","radius_server":"%s","radius_auth_port":%s,"radius_acct_port":%s,"secret_set":%s,"https":%s,"login_host":"%s","https_port":%s,"cert_source":"%s","agent_url":"%s","token_set":%s,"walled_garden":"%s","bypass_mac":"%s","exclude_if":"%s","lan_ports":[%s],"status":%s}' \
		"$(get enabled || echo 0)" "$(jstr "$(get portal_url)")" "$(jstr "$(get nas_identifier)")" \
		"$(jstr "$(get radius_server)")" "$(get radius_auth_port || echo 1812)" "$(get radius_acct_port || echo 1813)" \
		"$sec" "$(get https || echo 0)" "$(jstr "$(get login_host)")" "$(get https_port || echo 8443)" \
		"$(jstr "$(get cert_source)")" "$(jstr "$(get agent_url)")" "$tok" \
		"$(jstr "$(csv walled_garden)")" "$(jstr "$(csv bypass_mac)")" "$(jstr "$(csv exclude_if)")" \
		"${ifj%,}" "$st"
}

RE_URL='^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/-]*)?$'
RE_HOST='^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$'
RE_NAS='^[A-Za-z0-9_.:-]{1,64}$'
RE_SECRET='^[A-Za-z0-9!#$%()*+,./:;<=>?@^_{|}~-]{8,128}$'
RE_TOKEN='^[A-Za-z0-9._~+/=-]{8,255}$'
RE_WG='^[A-Za-z0-9.-]+(/[0-9]{1,2})?$'
RE_MAC='^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$'
RE_INSTALL='^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?/agent/v1/install/[0-9a-fA-F]{16,128}/?$'
RE_IDENT='^[A-Za-z0-9_.-]{1,64}$'
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
register)
	iu="$(param install_url)"; id="$(param identity)"
	match "$iu" "$RE_INSTALL" || fail install_url
	match "$id" "$RE_IDENT" || fail identity
	r="$("$BIN" -uci $PKG -install "$iu" -identity "$id" 2>/dev/null | tail -n1)"
	case "$r" in
	*'"result":"ok"'*)
		logger -t omnicap-gw "web: registered in omnicap as $id (from ${REMOTE_ADDR:-?})"
		restart
		st="$(status_json)"
		out "200 OK" "${st%\}},\"register\":$r}"
		;;
	*)
		logger -t omnicap-gw "web: registration as $id failed (from ${REMOTE_ADDR:-?})"
		[ -n "$r" ] || r='{"result":"fail","error":"no answer"}'
		out "400 Bad Request" "{\"result\":\"fail\",\"error\":\"register\",\"register\":$r}"
		;;
	esac
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
excl="$(param exclude_if)"

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
# exclude_if: comma list of LAN ports whose clients skip the portal; only ports this device really has,
# and at least one port must stay captured (to capture none, disable the portal)
ports="$(lan_ports | cut -f1)"
exl=""; for x in $(printf '%s' "$excl" | tr ', ' '\n\n'); do
	printf '%s\n' "$ports" | grep -qxF -- "$x" || fail exclude_if
	case " $exl " in *" $x "*) ;; *) exl="$exl $x" ;; esac
done
left=0; for x in $ports; do case " $exl " in *" $x "*) ;; *) left=$((left+1)) ;; esac; done
[ "$left" -ge 1 ] || fail exclude_if
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
uci -q delete $PKG.main.exclude_if; for x in $exl; do uci add_list $PKG.main.exclude_if="$x"; done
uci commit $PKG
logger -t omnicap-gw "web: settings saved enabled=$en https=$https nas=$nas exclude_if=${exl:- none} (from ${REMOTE_ADDR:-?})"

if [ "$en" = 1 ]; then restart; else sh "$CTL" disable >/dev/null 2>&1; fi
out "200 OK" "$(status_json)"
