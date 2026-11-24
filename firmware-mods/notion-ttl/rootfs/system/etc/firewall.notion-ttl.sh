#!/bin/sh
# notion-ttl: fija el TTL (IPv4) / Hop Limit (IPv6) del tráfico que sale por la interfaz celular.
#
# Valor y estado se leen de /etc/config/notion_ttl (uci, sección notion_ttl.main):
#   enabled  0|1        aplicar o no
#   ttl      1..255     valor a fijar (64 = igual que un celular Android/iOS)
#   ipv6     0|1        aplicar también Hop Limit en IPv6
#   iface    ccinet+    interfaz(es) de salida (patrón iptables)
#
# Lo ejecuta fw3 en cada start/reload (uci firewall.notion_ttl: type=script, reload=1), porque cada
# re-attach 4G<->5G hace ifup de wan0 -> hotplug -> fw3 reload, que borra las reglas manuales.
# También lo ejecuta /www/notion_ttl.cgi al guardar desde la interfaz web.
#
# Uso: firewall.notion-ttl.sh [apply|remove|status]

TAG=notion-ttl

cfg() { uci -q get "notion_ttl.main.$1"; }

ENABLED="$(cfg enabled)"; [ -n "$ENABLED" ] || ENABLED=1
TTL="$(cfg ttl)";         [ -n "$TTL" ] || TTL=64
IPV6="$(cfg ipv6)";       [ -n "$IPV6" ] || IPV6=1
IFACE="$(cfg iface)";     [ -n "$IFACE" ] || IFACE='ccinet+'

# Borra TODAS las reglas con el comentario notion-ttl, sin importar el valor con que se crearon.
flush() {
	for bin in iptables ip6tables; do
		$bin -t mangle -S POSTROUTING 2>/dev/null | grep -- "--comment $TAG " | sed 's/^-A /-D /' |
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
	logger -t $TAG "reglas removidas"
	;;
status)
	echo "enabled=$ENABLED ttl=$TTL ipv6=$IPV6 iface=$IFACE"
	iptables -t mangle -L POSTROUTING -v -n -x 2>/dev/null | grep "$TAG"
	ip6tables -t mangle -L POSTROUTING -v -n -x 2>/dev/null | grep "$TAG"
	;;
*)
	flush
	if [ "$ENABLED" != 1 ]; then
		logger -t $TAG "deshabilitado (notion_ttl.main.enabled=$ENABLED)"
		exit 0
	fi
	if ! valid; then
		logger -t $TAG "config inválida (ttl=$TTL iface=$IFACE), no se aplica"
		exit 1
	fi
	iptables -t mangle -I POSTROUTING -o "$IFACE" -m comment --comment $TAG -j TTL --ttl-set "$TTL"
	[ "$IPV6" = 1 ] && ip6tables -t mangle -I POSTROUTING -o "$IFACE" -m comment --comment $TAG -j HL --hl-set "$TTL"
	logger -t $TAG "TTL/HL=$TTL aplicado en $IFACE (ipv6=$IPV6)"
	;;
esac
exit 0
