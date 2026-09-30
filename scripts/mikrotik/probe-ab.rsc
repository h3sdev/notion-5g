# probe-ab.rsc - sonda A/B del celular por cable (hAP ac2, RouterOS 7.6)
# GENERADO por: python apply_probe.py --emit-rsc   (no editar a mano; regenerar)
# Contrato: server/docs/CONTRATO-SONDA-AB.md, seccion 3.
#
# Lo recomendado es apply_probe.py --apply (hace respaldo, compara y solo cambia lo que
# difiere). Este script es la alternativa manual e idempotente (se puede correr varias
# veces; reescribe los valores aunque ya esten bien). NO hace respaldo: antes, en la
# terminal:  /export file=flash/pre-probe-manual  y  /system backup save name=flash/pre-probe-manual
# y baja los dos archivos (Files en WinBox).
#
# Uso: cambia CAMBIAR-ESTA-CLAVE (solo se usa si el usuario phone-probe no existe),
# sube el archivo a Files y corre  /import file-name=probe-ab.rsc   (o pegalo en la
# terminal de WinBox). Todo va en un solo bloque { }: si algo falla, se detiene.
#
# Diferencias con apply_probe.py: no compara (siempre hace set); la regla NAT se busca
# sin mirar src-address; no hace las verificaciones finales (usa --verify).

{
:local phonePass "CAMBIAR-ESTA-CLAVE"
:local gwA "192.168.1.1"
:local gwB "192.168.2.1"
:local boundA false
:local boundB false
:if (([:len [/user find where name="phone-probe"]] = 0) && ($phonePass = "CAMBIAR-ESTA-CLAVE")) do={:error "Cambia phonePass al comienzo del script antes de correrlo"}
:put "sonda A/B: 1/14 MAC fija del bridge"
{
:local b [/interface bridge find where name="bridge"]
:if ([:len $b] = 1) do={:if ([/interface bridge get [:pick $b 0] auto-mac]) do={/interface bridge set [:pick $b 0] auto-mac=no admin-mac=[/interface bridge get [:pick $b 0] mac-address]}}
}
:put "sonda A/B: 2/14 tablas"
:if ([:len [/routing table find where name="to-A"]] = 0) do={/routing table add name=to-A fib comment="probe:to-A"}
:if ([:len [/routing table find where name="to-B"]] = 0) do={/routing table add name=to-B fib comment="probe:to-B"}
:put "sonda A/B: 3/14 listas de interfaces"
:if ([:len [/interface list member find where list="WAN" and interface="ether1"]] = 0) do={/interface list member add list=WAN interface=ether1 comment="probe:wan-B"}
:if ([:len [/interface list member find where list="WAN" and interface="ether2"]] = 0) do={/interface list member add list=WAN interface=ether2 comment="probe:wan-A"}
:if ([:len [/interface list member find where list="LAN" and interface="ether3"]] = 0) do={/interface list member add list=LAN interface=ether3 comment="probe:phone"}
:put "sonda A/B: 4/14 sacar ether2 y ether3 del bridge"
/interface bridge port remove [find where interface="ether2"]
/interface bridge port remove [find where interface="ether3"]
:put "sonda A/B: 5/14 direcciones"
{
:local x [/ip address find where comment="probe:mgmt"]
:if ([:len $x] = 0) do={:set x [/ip address find where interface="bridge" and address="192.168.88.1/24"]}
:if ([:len $x] = 0) do={:set x [/ip address find where interface="bridge" and address="192.168.1.1/24"]}
:if ([:len $x] = 0) do={/ip address add address=192.168.88.1/24 network=192.168.88.0 interface=bridge comment=probe:mgmt} else={/ip address set [:pick $x 0] address=192.168.88.1/24 network=192.168.88.0 interface=bridge comment=probe:mgmt}
}
{
:local x [/ip address find where comment="probe:phone-net"]
:if ([:len $x] = 0) do={:set x [/ip address find where interface="ether3" and address="192.168.89.1/24"]}
:if ([:len $x] = 0) do={/ip address add address=192.168.89.1/24 interface=ether3 comment=probe:phone-net} else={/ip address set [:pick $x 0] address=192.168.89.1/24 interface=ether3 comment=probe:phone-net}
}
:put "sonda A/B: 6/14 DHCP"
{
:local s [/ip dhcp-server find where interface="bridge"]
:if ([:len $s] > 0) do={:local p [/ip dhcp-server get [:pick $s 0] address-pool]; /ip pool set [find where name=$p] ranges=192.168.88.10-192.168.88.254}
}
{
:local x [/ip pool find where name="probe-phone"]
:if ([:len $x] = 0) do={/ip pool add name=probe-phone ranges=192.168.89.10-192.168.89.50 comment=probe:phone-net} else={/ip pool set [:pick $x 0] name=probe-phone ranges=192.168.89.10-192.168.89.50 comment=probe:phone-net}
}
{
:local x [/ip dhcp-server network find where comment="probe:mgmt"]
:if ([:len $x] = 0) do={:set x [/ip dhcp-server network find where address="192.168.88.0/24"]}
:if ([:len $x] = 0) do={:set x [/ip dhcp-server network find where address="192.168.1.0/24"]}
:if ([:len $x] = 0) do={/ip dhcp-server network add address=192.168.88.0/24 gateway="" dns-server="" comment=probe:mgmt} else={/ip dhcp-server network set [:pick $x 0] address=192.168.88.0/24 gateway="" dns-server="" comment=probe:mgmt}
}
{
:local x [/ip dhcp-server network find where comment="probe:phone-net"]
:if ([:len $x] = 0) do={:set x [/ip dhcp-server network find where address="192.168.89.0/24"]}
:if ([:len $x] = 0) do={/ip dhcp-server network add address=192.168.89.0/24 gateway=192.168.89.1 dns-server=1.1.1.1,8.8.8.8 comment=probe:phone-net} else={/ip dhcp-server network set [:pick $x 0] address=192.168.89.0/24 gateway=192.168.89.1 dns-server=1.1.1.1,8.8.8.8 comment=probe:phone-net}
}
{
:local x [/ip dhcp-server find where name="probe-phone"]
:if ([:len $x] = 0) do={:set x [/ip dhcp-server find where interface="ether3"]}
:if ([:len $x] = 0) do={/ip dhcp-server add name=probe-phone interface=ether3 address-pool=probe-phone lease-time=1h disabled=no comment=probe:phone-net} else={/ip dhcp-server set [:pick $x 0] name=probe-phone interface=ether3 address-pool=probe-phone lease-time=1h disabled=no comment=probe:phone-net}
}
{
:local x [/ip dhcp-client find where comment="probe:wan-B"]
:if ([:len $x] = 0) do={:set x [/ip dhcp-client find where interface="ether1"]}
:if ([:len $x] = 0) do={/ip dhcp-client add interface=ether1 add-default-route=no use-peer-dns=no use-peer-ntp=no script=":if (\$bound=1) do={:local gw (\$\"gateway-address\" . \"%\" . \$interface); :foreach c in={\"probe:B-default\";\"probe:main-B\"} do={:foreach r in=[/ip route find comment=\$c] do={:if ([:tostr [/ip route get \$r gateway]] != \$gw) do={/ip route set \$r gateway=\$gw; :log info \"probe: \$c -> \$gw\"}}}}" disabled=no comment=probe:wan-B} else={/ip dhcp-client set [:pick $x 0] interface=ether1 add-default-route=no use-peer-dns=no use-peer-ntp=no script=":if (\$bound=1) do={:local gw (\$\"gateway-address\" . \"%\" . \$interface); :foreach c in={\"probe:B-default\";\"probe:main-B\"} do={:foreach r in=[/ip route find comment=\$c] do={:if ([:tostr [/ip route get \$r gateway]] != \$gw) do={/ip route set \$r gateway=\$gw; :log info \"probe: \$c -> \$gw\"}}}}" disabled=no comment=probe:wan-B}
}
{
:local x [/ip dhcp-client find where comment="probe:wan-A"]
:if ([:len $x] = 0) do={:set x [/ip dhcp-client find where interface="ether2"]}
:if ([:len $x] = 0) do={/ip dhcp-client add interface=ether2 add-default-route=no use-peer-dns=no use-peer-ntp=no script=":if (\$bound=1) do={:local gw (\$\"gateway-address\" . \"%\" . \$interface); :foreach c in={\"probe:A-default\";\"probe:main-A\"} do={:foreach r in=[/ip route find comment=\$c] do={:if ([:tostr [/ip route get \$r gateway]] != \$gw) do={/ip route set \$r gateway=\$gw; :log info \"probe: \$c -> \$gw\"}}}}" disabled=no comment=probe:wan-A} else={/ip dhcp-client set [:pick $x 0] interface=ether2 add-default-route=no use-peer-dns=no use-peer-ntp=no script=":if (\$bound=1) do={:local gw (\$\"gateway-address\" . \"%\" . \$interface); :foreach c in={\"probe:A-default\";\"probe:main-A\"} do={:foreach r in=[/ip route find comment=\$c] do={:if ([:tostr [/ip route get \$r gateway]] != \$gw) do={/ip route set \$r gateway=\$gw; :log info \"probe: \$c -> \$gw\"}}}}" disabled=no comment=probe:wan-A}
}
:put "sonda A/B: 7/14 esperando DHCP de los WAN (hasta 15 s)"
{
:local i 0
:while (($i < 15) && ([:len [/ip dhcp-client find where interface="ether2" and status="bound"]] = 0)) do={:delay 1s; :set i ($i + 1)}
}
{
:local c [/ip dhcp-client find where interface="ether2" and status="bound"]
:if ([:len $c] > 0) do={:set gwA [/ip dhcp-client get [:pick $c 0] gateway]; :set boundA true}
}
{
:local c [/ip dhcp-client find where interface="ether1" and status="bound"]
:if ([:len $c] > 0) do={:set gwB [/ip dhcp-client get [:pick $c 0] gateway]; :set boundB true}
}
:put "sonda A/B: 8/14 rutas"
{
:local x [/ip route find where comment="probe:A-default"]
:if ([:len $x] = 0) do={/ip route add dst-address=0.0.0.0/0 routing-table=to-A distance=1 gateway=($gwA . "%ether2") comment="probe:A-default"} else={/ip route set [:pick $x 0] dst-address=0.0.0.0/0 routing-table=to-A distance=1; :if ($boundA) do={/ip route set [:pick $x 0] gateway=($gwA . "%ether2")}}
}
{
:local x [/ip route find where comment="probe:B-default"]
:if ([:len $x] = 0) do={/ip route add dst-address=0.0.0.0/0 routing-table=to-B distance=1 gateway=($gwB . "%ether1") comment="probe:B-default"} else={/ip route set [:pick $x 0] dst-address=0.0.0.0/0 routing-table=to-B distance=1; :if ($boundB) do={/ip route set [:pick $x 0] gateway=($gwB . "%ether1")}}
}
{
:local x [/ip route find where comment="probe:main-A"]
:if ([:len $x] = 0) do={/ip route add dst-address=0.0.0.0/0 routing-table=main distance=1 check-gateway=ping gateway=($gwA . "%ether2") comment="probe:main-A"} else={/ip route set [:pick $x 0] dst-address=0.0.0.0/0 routing-table=main distance=1 check-gateway=ping; :if ($boundA) do={/ip route set [:pick $x 0] gateway=($gwA . "%ether2")}}
}
{
:local x [/ip route find where comment="probe:main-B"]
:if ([:len $x] = 0) do={/ip route add dst-address=0.0.0.0/0 routing-table=main distance=2 check-gateway=ping gateway=($gwB . "%ether1") comment="probe:main-B"} else={/ip route set [:pick $x 0] dst-address=0.0.0.0/0 routing-table=main distance=2 check-gateway=ping; :if ($boundB) do={/ip route set [:pick $x 0] gateway=($gwB . "%ether1")}}
}
:put "sonda A/B: 9/14 reglas de ruteo"
{
:local x [/routing rule find where comment="probe:phone-local"]
:if ([:len $x] = 0) do={/routing rule add dst-address=192.168.89.0/24 action=lookup-only-in-table table=main disabled=no comment=probe:phone-local} else={/routing rule set [:pick $x 0] dst-address=192.168.89.0/24 action=lookup-only-in-table table=main disabled=no comment=probe:phone-local}
}
{
:local x [/routing rule find where comment="phone-probe"]
:if ([:len $x] = 0) do={/routing rule add src-address=192.168.89.0/24 action=lookup-only-in-table disabled=no comment=phone-probe table=main} else={/routing rule set [:pick $x 0] src-address=192.168.89.0/24 action=lookup-only-in-table disabled=no comment=phone-probe}
}
{
:local seenProbe false
:local wrong false
:foreach r in=[/routing rule find] do={:local c [/routing rule get $r comment]; :if ($c = "phone-probe") do={:set seenProbe true}; :if (($c = "probe:phone-local") && $seenProbe) do={:set wrong true}}
:if ($wrong) do={/routing rule move [find where comment="probe:phone-local"] destination=[find where comment="phone-probe"]}
}
:put "sonda A/B: 10/14 NAT"
:if ([:len [/ip firewall nat find where chain="srcnat" and action="masquerade" and out-interface-list="WAN"]] = 0) do={/ip firewall nat add chain=srcnat action=masquerade out-interface-list=WAN comment="probe:masq"}
:put "sonda A/B: 11/14 grupo y usuario del celular"
{
:local x [/user group find where name="probe-api"]
:if ([:len $x] = 0) do={/user group add name=probe-api policy=read,write,api comment=probe:api-group} else={/user group set [:pick $x 0] name=probe-api policy=read,write,api comment=probe:api-group}
}
{
:local x [/user find where name="phone-probe"]
:if ([:len $x] = 0) do={/user add name=phone-probe group=probe-api address=192.168.89.0/24 password=$phonePass comment="probe:phone-user"} else={/user set [:pick $x 0] group=probe-api address=192.168.89.0/24 disabled=no comment="probe:phone-user"}
}
:put "sonda A/B: 12/14 vigilante de la regla"
{
:local x [/system scheduler find where name="probe-rule-watchdog"]
:if ([:len $x] = 0) do={/system scheduler add name=probe-rule-watchdog interval=1m policy=read,write,policy,test on-event=":global probeForcedMin; :global probeLastTable; :global probeLastLogin; :local r [/routing rule find comment=\"phone-probe\"]; :if ([:len \$r]=1) do={:local t [/routing rule get [:pick \$r 0] table]; :local l \"\"; :local ls [/log find where message~\"^user phone-probe logged in\"]; :if ([:len \$ls]>0) do={:set l [:tostr [:pick \$ls ([:len \$ls]-1)]]}; :if ([:typeof \$probeForcedMin]!=\"num\") do={:set probeForcedMin 0}; :if ((\$t=\"main\") || (\$t!=[:tostr \$probeLastTable]) || (\$l!=[:tostr \$probeLastLogin])) do={:set probeForcedMin 0} else={:set probeForcedMin (\$probeForcedMin+1); :if (\$probeForcedMin>=10) do={/routing rule set [:pick \$r 0] table=main; :log warning \"phone-probe: regla en \$t por 10 min sin actividad del celular, vuelta a main\"; :set probeForcedMin 0; :set t \"main\"}}; :set probeLastTable \$t; :set probeLastLogin \$l}" disabled=no comment=probe:rule-watchdog} else={/system scheduler set [:pick $x 0] name=probe-rule-watchdog interval=1m policy=read,write,policy,test on-event=":global probeForcedMin; :global probeLastTable; :global probeLastLogin; :local r [/routing rule find comment=\"phone-probe\"]; :if ([:len \$r]=1) do={:local t [/routing rule get [:pick \$r 0] table]; :local l \"\"; :local ls [/log find where message~\"^user phone-probe logged in\"]; :if ([:len \$ls]>0) do={:set l [:tostr [:pick \$ls ([:len \$ls]-1)]]}; :if ([:typeof \$probeForcedMin]!=\"num\") do={:set probeForcedMin 0}; :if ((\$t=\"main\") || (\$t!=[:tostr \$probeLastTable]) || (\$l!=[:tostr \$probeLastLogin])) do={:set probeForcedMin 0} else={:set probeForcedMin (\$probeForcedMin+1); :if (\$probeForcedMin>=10) do={/routing rule set [:pick \$r 0] table=main; :log warning \"phone-probe: regla en \$t por 10 min sin actividad del celular, vuelta a main\"; :set probeForcedMin 0; :set t \"main\"}}; :set probeLastTable \$t; :set probeLastLogin \$l}" disabled=no comment=probe:rule-watchdog}
}
:put "sonda A/B: 13/14 servicio api (incluye fe80::/10: sin eso el PC queda afuera)"
/ip service set [find where name="api"] disabled=no address=192.168.88.0/24,192.168.89.0/24,fe80::/10
:put "sonda A/B: 14/14 DNS, NTP, hora, identidad"
/ip dns set servers=1.1.1.1,8.8.8.8
/system ntp client set enabled=yes servers=162.159.200.1,162.159.200.123
/system clock set time-zone-name=America/Bogota
/system identity set name=hap-sonda
:if ([/ip settings get rp-filter] != "no") do={:put "AVISO: /ip settings rp-filter no es no"}
:put "sonda A/B: listo. Verifica con apply_probe.py --verify"
}
