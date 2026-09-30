# Pendiente — el celular lee la señal del router por SSH antes de cada prueba

> **HECHO (2026-09-30, app 1.5.0+12, `mobile/.../RouterSignal.kt`).** Primeras mediciones en
> producción: 5G a las 18:04 UTC por `ssh-ubus` (0,8 s): WOM, LTE-CA, banda 7, PCI 412, RSRP −57
> (el heartbeat del agente del mismo minuto: −58); 4G a las 18:01 UTC por `router-web` (1,8 s,
> el puerto 22 sigue cerrado): Movistar, LTE, banda 2, PCI 433, RSRP −77. Los nombres de campo
> de `ubus` del agente sirvieron tal cual (sin `signal_error: parse`). Sin hacer: la lectura de
> cierre `signal_end` y lo de §7. `nr_band` sin confirmar hasta que el 5G enganche NR.

> Escrito el 2026-09-30 desde el backend (VPS). Es para el Claude que tiene conexión con el
> celular (adb, `C:\dev\notion5g_mobile`) y con los routers (SSH). Todo lo del backend está
> verificado contra el código y contra la base de producción de hoy; lo del router es lo que ya
> hace el agente `cmd/routeragent` y falta confirmar en el 4G.

## 1. Problema

Las mediciones de la sonda A/B (`source = phone-probe`) llegan sin datos del módem: en las
293 mediciones de hoy (147 del 5G, 146 del 4G) `operator`, `rat`, `band_lte`, `nr_band`, `pci`,
`rsrp_dbm`, `rsrq_db`, `sinr_db`, `rssi_dbm` y `uptime_s` están en NULL. La app no los manda
porque no los tiene. Consecuencia en el dashboard, en los dos equipos: la tabla "Últimas
mediciones" muestra guiones en Operador, Banda, RSRP/RSRQ/SINR y Uptime; "Bandas LTE
probadas: ninguna"; el mapa no colorea por RSRP.

Antes de la sonda el 5G sí los tenía porque la prueba la corría el propio agente del router
(`source = router`, 44 mediciones viejas con señal). El 4G nunca los tuvo (no tiene agente).

## 2. Qué se pide

Que el celular, **justo antes de empezar cada prueba** de un slot, entre por SSH al router de
ese slot, lea el estado del módem con los mismos comandos que usa el agente, y **mande los
campos planos dentro del mismo resultado** (`POST /api/v1/probes/{id}/results`). Opcional:
una segunda lectura al terminar, en un objeto aparte.

**El backend no necesita cambios para lo básico.** `InsertPhoneResult` ya deserializa el
resultado en `store.Measurement` y guarda en columnas `operator`, `rat`, `band_lte`, `nr_band`,
`pci`, `rsrp_dbm`, `rsrq_db`, `sinr_db`, `rssi_dbm`, `eps_reg`, `nr_reg`, `uptime_s` si vienen
en el JSON (`internal/store/phoneprobe.go` ~1246). Las únicas claves que el servidor borra o
sobreescribe del resultado son `net_route`, `net_asn`, `net`, `client_ip`, `egress_asn`,
`slot`, `device_id_client`, `probe_id`, `device_id`, `ts`, `test_started_at`. Todo lo demás se
conserva en `raw`, que es lo que lee el dashboard.

## 3. Cómo leerlo en el router (copiar del agente, no inventar)

`cmd/routeragent/main.go`, función `readLocalStatus()` (líneas ~405-540). Por SSH es
exactamente lo mismo que `STATUS_REMOTE` de `scripts/notion5g.py`. Un solo comando remoto:

```sh
ubus -t 6 call cm get_zcainfo; echo '@@'; ubus -t 6 call cm get_link_context; echo '@@'; ubus -t 6 call util_wan get_network_mode; echo '@@'; cat /proc/uptime
```

Y del JSON sacar, con estos nombres de campo (son los que el backend y el dashboard ya
entienden):

| Campo del resultado | De dónde | Conversión |
|---|---|---|
| `band_lte` | `get_zcainfo` → `lte.p_band` | tal cual (entero) |
| `pci` | `lte.p_pci` | tal cual |
| `rsrp_dbm` | `lte.p_rsrp` | índice CESQ: `-141 + idx`; `255` = sin dato (no mandar) |
| `rsrq_db` | `lte.p_rsrq` | `-19.5 + idx*0.5`; `255` = sin dato |
| `sinr_db` | `lte.p_sinr` | tal cual |
| `rssi_dbm` | `lte.p_rssi` | `-111 + idx`; `99` = sin dato |
| `nr_band`, `nr_pci`, `nr_arfcn`, `nr_rsrp_dbm`, `nr_sinr_db` | sub-objeto NR de `get_zcainfo` (probar `nr`, `nr5g`, `NR`, `endc`, `sa`, `nsa`; campos `n_band`/`band`, `n_pci`/`pci`, `n_rsrp`/`rsrp`, `n_sinr`/`sinr`) | tal cual. Si hay bloque NR pero no se encuentra la banda, adjuntar el bloque crudo como `zcainfo_nr_raw` (igual que el agente) |
| `operator` | `get_link_context` → `celluar_basic_info.network_name` (sic, "celluar") | tal cual |
| `eps_reg` | `celluar_basic_info.RegStatus` | entero |
| `rat` | `celluar_basic_info.sys_mode` | `1` → `2G/3G`, `2` → `LTE`, `3` → `LTE-CA`, `4` → `5G-SA`, `5` → `5G-NSA`; si falta y hay `band_lte` → `LTE` |
| `nw_mode`, `prefer_mode`, `nr_mode` | `get_network_mode` → `net_mode.*` | tal cual |
| `uptime_s` | `/proc/uptime`, primer número | entero (segundos) |

Un valor `0` o `-1` en banda/PCI/ARFCN significa "sin dato": no mandarlo (el dashboard
mostraría "banda 0").

**No usar** `serial_atcmd` ni el puerto AT: es un canal único compartido con el firmware y
puede dejarlo bloqueado (comentario en el agente, ~línea 485). Solo `ubus`.

Además, agregar al resultado:

- `signal_source`: `"ssh-ubus"` (o `"router-web"` si se usó el respaldo de §5).
- `signal_read_at`: RFC 3339 UTC de la lectura.
- `signal_error`: texto corto si la lectura falló (`ssh-connect`, `ssh-auth`, `timeout`,
  `parse`); en ese caso no mandar ninguno de los campos de arriba. **La prueba se corre igual.**
- Opcional, lectura al terminar: objeto `signal_end` con los mismos campos (no va a columnas,
  queda en `raw`).

## 4. Cómo conectar (reusar el camino del reinicio, §6.2 del contrato)

Igual que la orden `reboot_router`: gateway del slot leído de `/ip/route` de `to-A`/`to-B`
(hoy 192.168.1.1 para A/5G y 192.168.2.1 para B/4G), JSch `com.github.mwiede:jsch` con
`ssh-rsa` y kex legacy, socket **atado a Ethernet**, usuario/clave/puerto de los ajustes por
slot (`ssh_user_A|B`, `ssh_password_A|B`, `ssh_port_A|B`; la clave nunca sale del celular).
Solo cambia el comando: en vez de `reboot`, el de §3.

Reglas:

1. **Antes** de `test_started_at`, nunca durante la prueba. Presupuesto total ≤ 8 s (conexión +
   comando); si se vence, `signal_error: "timeout"` y arrancar la prueba.
2. Una sola sesión SSH por prueba (dos si se hace la lectura de cierre). El tráfico es de unos
   pocos KB por la LAN del router, no toca el enlace celular.
3. Si la orden trae `slot` distinto del esperado o el título del router no coincide
   (`identity_ok=false`), no leer: la prueba de ese tramo ya no sirve.
4. En el 5G el agente del router sigue corriendo sus heartbeats con los mismos `ubus`; dos
   lectores a la vez sobre `ubus` no es problema (la web del equipo hace lo mismo).

## 5. Verificar primero en el 4G

El 4G nunca tuvo agente y **no está confirmado** que su firmware tenga `ubus call cm
get_zcainfo` con los mismos nombres. El reinicio por SSH sí le funciona (último: 2026-09-29
22:02, `done`), así que SSH está. Antes de tocar la app, desde el PC:

```sh
NOTION_PASS=… scripts/rsh.sh 192.168.2.1 'ubus -t 6 call cm get_zcainfo; ubus -t 6 call cm get_link_context; ubus -t 6 call util_wan get_network_mode; cat /proc/uptime'
```

Si los objetos existen pero cambian nombres de campo, anotarlos acá y en el agente. Si no
existen, respaldo: la app ya lee la señal del 4G de la página web del router para
`routers.B.signal` del estado en vivo (operador, `rat`, `band_lte`, `rsrp_dbm`, `rsrq_db`,
`sinr_db`, `pci`, `uptime_s`). Con hacer esa misma lectura justo antes de la prueba y copiar
los campos al resultado con `signal_source: "router-web"` alcanza.

Hacer la misma prueba manual en el 5G (192.168.1.1) para medir cuánto tarda y ajustar el
presupuesto de §4.

## 6. Cómo comprobar que quedó

1. Instalar la app, esperar un ciclo (5 min, alterna A y B).
2. `GET /api/v1/measurements?probe_id=hap-oficina&limit=2` con `X-API-Key`: las dos últimas
   deben traer `operator`, `rat`, `band_lte`, `rsrp_dbm`, `uptime_s` y `signal_source`.
3. En el dashboard, `/#/device/router-R023-4g` y `/#/device/router-R524260829000001`: la tabla
   llena Operador/Banda/RSRP/Uptime y "Bandas LTE probadas" deja de decir "ninguna".
4. Comparar el `rsrp_dbm` de una medición del 5G con el heartbeat del agente del mismo minuto
   (`GET /api/v1/heartbeats?device_id=router-R524260829000001&limit=3`): deben estar a pocos
   dB. Hoy el agente reporta WOM, RSRP −67, y el celular ve al 4G en Movistar, LTE banda 2,
   RSRP −69.

## 7. Fuera de alcance (queda para el backend, si se quiere)

- Rellenar las 293 mediciones ya guardadas con la lectura más cercana (heartbeat del agente
  para el 5G, `routers.B.signal` del estado del celular para el 4G). Se puede hacer con una
  pasada sobre la base; no depende de la app.
- El aviso "Red de salida: sin determinar" del dashboard mira la tabla `device_egress`, que
  solo llena el agente del router. Para equipos medidos por sonda debería mirar `net` de la
  última medición (`egress_asn`, `reason: probe-asn-match`), que sí está. Es un cambio de
  dashboard/backend, no de la app.
