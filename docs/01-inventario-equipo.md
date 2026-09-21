# Inventario técnico — Módem Notion 5G (H3S P52HL)

Levantado por SSH el 2026-09-16 sobre el equipo en `192.168.1.1` (usuario `root`).
Los volcados crudos están en `firmware-notes/` (ubus, uci, dmesg, red, sistema, scripts).

## 1. Identificación

| Campo | Valor |
|---|---|
| Producto (uci/ubus) | `PRODUCT_TYPE=CPE`, hostname `cpe.baseline.com` |
| Firmware | `H3S_P52HL_KSA00174_V001` (interno `_INTERNAL_001`), build 2026‑08‑19 11:37 |
| Hardware | `P52HL_N10` |
| Baseband / CP | `1.171.010P12` |
| SO | OpenWrt 24.10‑SNAPSHOT `r0-ac162f84a7`, target `mmp/asr1901`, kernel 5.4.288 |
| Web UI | lighttpd 1.4.76, idioma `es`, `version_type=jp` |
| IMEI | 861219080002409 |
| SN | R524260829000001 |
| Chip UID | 08e14e440ea4b801 |
| Zona horaria configurada | `Asia/Shanghai` / `GMT+5` (**incorrecta para Colombia, debe ser GMT‑5**) |

## 2. Procesador y plataforma

| Campo | Valor |
|---|---|
| SoC | **ASR1901 “Kestrel”** (ASR Microelectronics, ex‑Marvell). Módem 5G NR Sub‑6 + LTE + UMTS + GSM integrado |
| CPU de aplicaciones | 4 × **ARM Cortex‑A7 r0p5** (armv7l, `CPU part 0xc07`), ~1.75 GHz (BogoMIPS 1785), NEON, VFPv4, LPAE |
| RAM | 96 MB utilizables por Linux (`DDR_SIZE=256` MB total; el resto es del procesador de comunicaciones/CP). Libre en reposo ≈ 27 MB. zram swap 23 MB |
| Flash | NAND 128 MB. Particiones MTD: bootloader, u‑boot, `cpimage` 26 MB (firmware del módem), `kernel` 5 MB, `rootfs` 50 MB (squashfs, 14.4 MB usados, solo lectura), `rootfs_data` 32 MB (UBI `/data`, 14.7 MB libres), `oem_data` 6 MB, `OTA` 29 MB, `MRVL_BBM` 7 MB |
| Sistemas de archivos escribibles | `/system/etc` (overlay, 2.4 MB libres; aquí vive `/etc/config`), `/NVM` (overlay), `/data` `/log` `/mnt` (UBI). **`/etc` y `/` son solo lectura** |
| Temperatura | sensor `tsen_max` 55–57 °C en reposo (sin carga) |
| Batería | **no reportada**: `get_battery_info` devuelve 0/0/0 y no hay `power_supply` en sysfs. La powerbank externa no está telemetrizada |

## 3. Radio celular

| Campo | Valor |
|---|---|
| Bandas LTE soportadas | **2, 4, 5, 7, 8, 28** |
| Bandas NR (FR1) soportadas | **n8, n28, n78** (sin FR2/mmWave) |
| Modo de red actual | `nw_mode=9`, `prefer_mode=5`, `nr_mode=0` (auto; ajustable con `util_wan set_network_mode`) |
| Registro observado | LTE (`+COPS: 0,0,"TIGO",7`), EPS registrado, **`+C5GREG: 3,0` = no registrado en 5G** en la oficina |
| Celda observada | B28 (EARFCN 9310, 20 MHz) PCI 393, eNB 614 sector 53, TAC 0x08CB; también B4 EARFCN 2150 PCI 272 |
| Señal en oficina | RSRP −70 dBm, RSRQ −12 dB, SINR 8–9 dB, RSSI −48 dBm (buena señal, SINR medio) |
| Carrier aggregation | campo `s_status` (portadora secundaria) disponible en `cm get_zcainfo`; en 0 durante la medición |
| APN activo | `web.colombiamovil.com.co` IPv4v6, perfil `default`, `auto_apn=1`, MTU 1500 |
| IMS/VoLTE | PDP IMS definido (CID 8) pero `ims_status=0` |
| Dirección WAN | CGNAT `10.x` + IPv6 GUA `2803:1800:…/64` |
| Roaming | `data_on_roaming=1` |

## 4. SIM

| Campo | Valor |
|---|---|
| Ranuras | **1 USIM física + 2 eSIM internas** (`usim_num=1`, `esim_num=2`). Driver propio `/dev/sim-notion` (major 249) y `libnotion.so` |
| Activa | `usim1` (física) — Tigo Colombia, IMSI `732111…`, ICCID `8957732111…` |
| eSIM1 / eSIM2 | Perfiles de prueba **China Unicom** (MCC/MNC 460‑06, ICCID `898606…`). No sirven en Colombia; útiles para saber que hay conmutación multi‑SIM (`sim set_double_sim_status`, CGI `json_mss_set_prefered_sim.cgi`) |
| PIN | deshabilitado, 10 intentos |

## 5. Wi‑Fi y LAN

| Campo | Valor |
|---|---|
| Wi‑Fi | chip **ASR5811** (Wi‑Fi 6, `mac80211`), `wlan0` 2.4 GHz HE40 + `wlan1` 5 GHz. SSID `H3SWiFi_81E9`, WPA2‑PSK, clave por defecto débil (8 dígitos). Hasta 32 clientes. `country=CN` (**debe ser CO**). Módulo `aic8800D80` también cargado |
| Ethernet | switch VLAN: `eth0.1` = WAN (puerto 4), `eth0.2` = LAN (puertos 0‑3). `DEVICE_LAN_PORT_NUM=2` |
| USB | gadget serie `ttyGS0/1`; `usbnet0`/`hsicnet0` en el bridge LAN (tethering USB). `usb_type=none` |
| LAN | `192.168.1.1/24`, dnsmasq + odhcpd, IPv6 prefijo delegado `/60` |
| Coexistencia NR/Wi‑Fi | cron cada minuto ejecuta `/etc/log/nr_channel_monitor.sh` (lee `at*bandind?` y ajusta canales Wi‑Fi) |

## 6. Software y servicios

| Área | Detalle |
|---|---|
| Procesos del módem | `rild`, `atcmdsrv`, `cm` (connection manager), `imsd`, `stk`, `audio_if`, `dxslic`, `nvmproxy`, `pppmodem`, `diagnose`, `ota`, `tr069`, `router`, `qos`, `traf_s`, `wireless` |
| Red | netifd, fw3 (**iptables**, no nftables), mwan3 (deshabilitado), miniupnpd, strongSwan/IPsec, xl2tpd, ddns, odhcpd |
| Gestión | web (lighttpd, `login.cgi` + CGIs `json_*.cgi`), SSH dropbear, **TR‑069 activo**, OTA |
| Herramientas a bordo | `wget`, `iperf` (v2), `tcpdump`, `nc`, `ubus`, `jsonfilter`, `ip`, `tc`. **No hay** `curl`, `iperf3`, `python`, `lua`, `opkg` funcional, ni `timeout` |
| API interna (ubus) | `sim`, `cm`, `util_wan`, `version`, `router`, `statistics`, `diagnostics`, `sms`, `ussd`, `stk`, `phonebook`, `ota`, `tr069`, `qos`. Lista completa en `firmware-notes/ubus-methods.txt` |
| Comandos AT | vía `serial_atcmd "AT+…"` (proxy a `atcmdsrv`). Soportados: `+CGMI/+CGMM/+CGMR/+CGSN/+CIMI`, `+COPS?`, `+CEREG?`, `+C5GREG?`, `+CSQ`, `+CESQ`, `+CGDCONT?`, `+CGPADDR`, `+CGCONTRDP`, `*BANDIND?`, `*BAND?`, `*SELECTSIMSLOT?`, `*EHSDPA?`. **No** soportados: `+CLAC`, `*CELLINFO?`, `+CNMP`, `+CCID` |
| CGIs web | sin sesión responden `{"result":"fail"}`; requieren login vía `login.cgi` (no se hizo ingeniería inversa; la automatización usa SSH+ubus) |

## 7. GPS / posicionamiento — **no existe GNSS en el equipo**

Evidencia recogida:

- No hay nodos `/dev/*gps*`, `*nmea*`, `*gnss*`; no hay `gpsd` ni scripts relacionados.
- 16 comandos AT de GNSS distintos (`+CGPS`, `+CGPSINFO`, `*GPS`, `+CGNSPWR`, `+CLBS`, `+CPOS`, `+CMOLR`, `*AGPS`, `*GNSS`, …) devuelven `ERROR`.
- `strings` sobre `rild`, `cm`, `router`, `libril*.so`: cero referencias a GPS/NMEA. En `atcmdsrv` solo aparecen `GPSTest` y `processPositionCommonMeasInd` (posicionamiento de plano de control LPP para la red, no un receptor usable).
- El objeto ubus `sms.unsol.gps` existe pero no expone métodos.

Consecuencia para la homologación: la posición durante las pruebas en movimiento debe venir de una fuente externa (GPS del portátil vía Windows Location API, o un celular enviando NMEA por UDP). La herramienta `scripts/notion5g.py drive` ya lo soporta. Ver plan.

## 8. Firewall y TTL

- fw3 recrea todas las cadenas en cada `fw3 reload`, y `/etc/hotplug.d/iface/20-firewall` dispara ese reload en cada `ifup` de una interfaz de la zona wan (`wan0`…`wan67`, `ccinet*`). Un re‑attach 5G→4G levanta de nuevo `wan0` y **borra cualquier regla puesta a mano**.
- `/etc/firewall.user` está en squashfs (solo lectura) y no sirve para persistir.
- Módulos disponibles: `xt_HL` (targets `TTL` y `HL`), `xt_hl`, `xt_TCPMSS`, `xt_FLOWOFFLOAD`, etc.
- **Solución instalada el 2026‑09‑16**: `/system/etc/firewall.notion-ttl.sh` + `firewall.notion_ttl=include` (`reload=1`). Fija TTL=64 e IPv6 Hop Limit=64 en `POSTROUTING -o ccinet+`. Verificado que sobrevive a `fw3 reload`. Instalador/desinstalador: `scripts/ttl_persist_install.sh`.

## 9. Rendimiento medido (referencia, oficina, Tigo LTE B28 20 MHz)

| Prueba | Resultado |
|---|---|
| HTTP 6 hilos desde PC por Ethernet (Cloudflare) | **↓ 142.9 Mbps / ↑ 16.7 Mbps**, ping 29 ms a 8.8.8.8 |
| Descarga de 30 MB verificada en contador `ccinet0` | 31 MB → el tráfico sí pasó por el módem |
| `wget` ejecutado **dentro del módem** (10 MB) | ~1.9 Mbps — **no usar el módem como cliente de prueba**: el `wget` de busybox en el Cortex‑A7 no representa la capacidad del enlace |
| Carga CPU en reposo | load average ≈ 2.0 (el `cm`/`rild` mantienen ocupados los núcleos; normal en esta plataforma) |

## 10. Observaciones para homologación / preventa

1. Credenciales por defecto `root/notion` con SSH abierto en LAN y clave Wi‑Fi de 8 dígitos: cambiar antes de vender.
2. `country=CN` en Wi‑Fi y zona horaria `Asia/Shanghai`: ajustar a Colombia (`CO`, `GMT‑5`) para canales legales y logs con hora correcta.
3. TR‑069 habilitado: confirmar a qué ACS apunta o desactivarlo.
4. CGIs responden `Access-Control-Allow-Origin: *`.
5. Sin telemetría de batería: la autonomía se mide externamente (cronómetro + powerbank).
6. Sin GNSS: cualquier función de rastreo del producto necesita hardware adicional o el celular del usuario.
7. Almacenamiento y RAM muy ajustados: no instalar agentes pesados en el equipo; toda la automatización corre desde el PC.
8. Sin VoLTE (`ims_status=0`) aunque hay `imsd`: si el producto no ofrece voz, es irrelevante; si sí, revisar.
