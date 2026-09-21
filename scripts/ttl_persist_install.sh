#!/bin/bash
# ttl_persist_install.sh — deja fijas las reglas TTL/Hop-Limit en el módem Notion 5G.
#
# Por qué se borraban: el firewall es fw3 (iptables). Cada vez que la interfaz celular (wan0/ccinet0)
# hace ifup — y eso pasa en cada re-attach 5G<->4G — el hotplug /etc/hotplug.d/iface/20-firewall
# ejecuta `fw3 reload`, que vacía y recrea las cadenas. Cualquier regla puesta a mano con iptables muere ahí.
# /etc/firewall.user es read-only (squashfs), así que no sirve para persistir.
#
# Solución: script en /system/etc (overlay escribible) registrado como `include` de fw3 con reload=1,
# de modo que fw3 lo vuelve a ejecutar en cada reload. La config uci vive en /system/etc/config (persistente).
#
# Uso:  scripts/ttl_persist_install.sh [TTL]        (default 64 = el TTL de un celular)
#       scripts/ttl_persist_install.sh --remove
set -e
HERE="$(cd "$(dirname "$0")" && pwd)"
RSH="$HERE/rsh.sh"
REMOTE=/system/etc/firewall.notion-ttl.sh

if [ "${1:-}" = "--remove" ]; then
  "$RSH" "uci -q delete firewall.notion_ttl; uci commit firewall; sh $REMOTE remove 2>/dev/null; rm -f $REMOTE; fw3 -q reload; iptables -t mangle -S | grep -c notion-ttl || true"
  echo "reglas TTL removidas"
  exit 0
fi

TTL="${1:-64}"

"$RSH" "cat > $REMOTE" <<EOF
#!/bin/sh
# Generado por ttl_persist_install.sh — ejecutado por fw3 en cada start/reload (include, reload=1).
# POSTROUTING corre DESPUÉS del decremento de forwarding, así que el valor fijado aquí es el TTL
# real que sale por la antena. 64 = igual que un celular Android/iOS.
TTL=$TTL
del() { \$1 -t mangle -D \$2 2>/dev/null; }
add() { \$1 -t mangle -I \$2; }
R4="POSTROUTING -o ccinet+ -m comment --comment notion-ttl -j TTL --ttl-set \$TTL"
R6="POSTROUTING -o ccinet+ -m comment --comment notion-ttl -j HL --hl-set \$TTL"
del iptables "\$R4"; del ip6tables "\$R6"
[ "\$1" = "remove" ] && exit 0
add iptables "\$R4"
add ip6tables "\$R6"
logger -t notion-ttl "TTL/HL=\$TTL aplicado en ccinet+"
EOF

"$RSH" "chmod +x $REMOTE
uci -q delete firewall.notion_ttl
uci set firewall.notion_ttl=include
uci set firewall.notion_ttl.type=script
uci set firewall.notion_ttl.path=$REMOTE
uci set firewall.notion_ttl.reload=1
uci set firewall.notion_ttl.family=any
uci commit firewall
fw3 -q reload
echo '--- reglas activas:'
iptables -t mangle -S POSTROUTING | grep notion-ttl
ip6tables -t mangle -S POSTROUTING | grep notion-ttl
echo '--- uci:'
uci show firewall.notion_ttl"
echo
echo "Verificación de persistencia: scripts/rsh.sh 'fw3 reload; iptables -t mangle -S POSTROUTING | grep notion-ttl'"
