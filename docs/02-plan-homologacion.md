# Plan de homologación y pruebas de campo — Módem Notion 5G

Objetivo: caracterizar el módem (cobertura, velocidad, latencia, estabilidad, TTL) con distintos operadores
colombianos, compararlo contra un celular de referencia y dejar todo automatizado y reproducible.

Todo corre desde el PC (WSL) contra el módem por SSH. Herramientas en `scripts/`, resultados en `results/`.

## 0. Preparación del banco de pruebas

1. PC conectado al módem **por Ethernet** (o su Wi‑Fi), con el **Wi‑Fi del portátil apagado** y **sin VPN activa** durante las mediciones.
   - Windows elige la ruta por métrica: Ethernet→módem (4230) gana al Wi‑Fi (4255), pero cualquier túnel VPN (metric ~20) gana a ambos.
   - Hallazgo 2026‑09‑17: el túnel WireGuard **"OFC H3S"** tiene kill‑switch (bloquea todo lo que no va por el túnel). Con él activo
     ni siquiera un socket atado a la IP del lado del módem (`speedtest -i 192.168.1.x`, `curl --interface`) sale: "Network is unreachable".
     Desde WSL tampoco sirve atar la IP: Linux enruta por la tabla principal (eth6 = túnel) y no hay `sudo` para policy routing.
   - `notion5g.py speed` hace una **comprobación previa**: descarga 2 MB y verifica con los contadores de `ccinet0` que el módem los vio;
     si no, imprime por dónde está saliendo Windows (`Find-NetRoute`), qué clientes VPN corren, y aborta (código 2). `--force` mide igual y
     deja `via_modem=False` en la fila. `drive` hace la misma comprobación pero solo avisa.
2. CLI oficial de Ookla para comparar 1:1 con la app del celular. `winget install Ookla.Speedtest.CLI` falló por red en este PC, así que se
   descomprimió el zip portable en `C:\Users\diego\speedtest-cli\speedtest.exe`; `notion5g.py` lo encuentra solo (PATH → WSL `~/.local/bin` →
   esa carpeta; o `NOTION_SPEEDTEST=/ruta`). Si el CLI falla, la fila lleva `ookla_error` con el mensaje real (antes salía "Expecting value").
3. Etiquetar cada sesión con `NOTION_TAG` (operador + sitio), p. ej. `NOTION_TAG=claro-chapinero`.
4. Probar conectividad: `python3 scripts/notion5g.py status`.

## 1. Métricas que se registran en cada muestra

| Métrica | Fuente | Umbral de referencia |
|---|---|---|
| Operador, RAT (LTE / NR‑NSA / NR‑SA), registro EPS/5G | `AT+COPS?`, `AT+CEREG?`, `AT+C5GREG?` | debe registrar en 5G donde el celular lo haga |
| Banda, PCI, EARFCN, ancho de banda, CA | `cm get_zcainfo`, `at*bandind?` | — |
| RSRP | `cm get_zcainfo` (índice 3GPP → dBm) | ≥ −80 excelente, −80…−90 buena, −90…−100 regular, < −100 mala |
| RSRQ | idem | ≥ −10 buena, < −15 mala |
| SINR | idem | > 20 excelente, 13–20 buena, 0–13 regular, < 0 mala |
| eNB / sector / TAC | `+CEREG` (ECI) | para ubicar la celda en un mapa |
| Latencia y pérdida | `ping` desde el PC | < 40 ms LTE, < 25 ms NR; pérdida 0 % |
| Velocidad ↓/↑ | HTTP multi‑hilo (Cloudflare) + Ookla | ver criterio de aceptación §5 |
| Temperatura SoC | `thermal_zone0` | vigilar > 75 °C en carga sostenida |
| Reglas TTL activas | `iptables -t mangle` | debe ser 1 siempre |
| Posición | GPS externo (§2) | — |

## 2. Cómo obtener GPS si el módem no lo tiene

El ASR1901 de este equipo no expone GNSS (ver inventario §7). Opciones, de más simple a más precisa:

| Opción | Cómo | Precisión |
|---|---|---|
| **A. Ubicación de Windows** | `notion5g.py drive --gps win`. Usa `scripts/win_location.ps1` (Windows Location API). Activar Configuración → Privacidad → Ubicación. Si el portátil no tiene GPS, Windows usa Wi‑Fi/IP: sirve en ciudad, no en carretera | 10–100 m con GPS del portátil; peor sin GPS |
| **B. Celular como GPS por UDP** | App Android “GPS2IP”, “Share GPS” o “NetGPS” enviando NMEA por UDP al IP del PC (mismo Wi‑Fi del módem o hotspot). `notion5g.py drive --gps nmea:10110` | 3–10 m |
| **C. Track separado** | Grabar con Strava/OsmAnd/GPX Logger en el celular y cruzar por timestamp con `results/drive.csv` (columna `ts`) | 3–10 m |

Recomendación: B para pruebas en vehículo, A para caminatas urbanas. Sincronizar la hora del celular y del PC (ambos NTP).

## 3. Protocolo multi‑operador sin doble SIM

El equipo tiene una sola USIM física (las dos eSIM internas son perfiles de prueba de China Unicom y no registran en Colombia).
Por eso las comparaciones son **secuenciales**, controlando lugar y hora:

1. Definir 4–6 sitios fijos (oficina, interior, exterior urbano, periferia, vehículo) y una ruta de conducción.
2. Por cada operador (Tigo, Claro, Movistar, WOM): insertar la SIM, esperar 2 min a que registre, y correr en cada sitio:
   ```bash
   export NOTION_TAG=claro-sitio1
   python3 scripts/notion5g.py speed --runs 3 --ookla-server <ID>
   ```
   Usar **el mismo ID de servidor Ookla** para todos los operadores y para el celular.
3. Cambiar de operador y repetir en el mismo sitio dentro de la misma franja horaria (< 1 h de diferencia). La hora importa más que el día.
4. Drive test por operador: `python3 scripts/notion5g.py drive --interval 10 --gps nmea:10110 --speed-every 30`.
5. Resumir: `python3 scripts/summarize.py --md` y `python3 scripts/summarize.py --drive --md`.

Pruebas adicionales por operador (una vez, en sitio con 5G):

- Forzar solo LTE vs. permitir NR y comparar (`util_wan set_network_mode`; anotar el valor original `nw_mode=9, prefer_mode=5, nr_mode=0` para restaurar).
- Verificar que registra en n78/n28 donde el celular muestra 5G (`AT+C5GREG?` debe pasar a `…,1` o `…,5`).
- Bloqueo de celda (`cm set_cell_lock`) solo si hace falta aislar una banda.

## 4. Comparación de velocidad con un celular

Se busca saber si el módem entrega lo mismo que un teléfono en el mismo punto, no un valor absoluto.

Protocolo por sitio y operador:

1. Misma SIM en ambos, o dos SIM del mismo operador y mismo plan (verificar que el plan no esté limitado en velocidad).
2. Celular y módem a < 1 m, misma altura, sin cuerpo entre ambos. Módem alimentado por la powerbank como se venderá.
3. Alternar: módem (3 corridas) → celular (3 corridas) → módem (3) → celular (3). Todo en ≤ 20 min.
   - Módem: `notion5g.py speed --runs 3 --ookla-server <ID>` desde el PC por Ethernet, Wi‑Fi del PC apagado y VPN desconectada
     (la herramienta aborta si detecta que el tráfico no sale por el módem).
   - Celular: app Speedtest, **servidor fijo** con el mismo ID, modo “conexión múltiple”.
4. Anotar del celular: RAT (4G/4G+/5G), banda(s) y RSRP/SINR (Android: `*#*#4636#*#*` o app “Network Cell Info”; iPhone: Field Test `*3001#12345#*`).
5. Comparar **medianas**. Criterio: módem ≥ 80 % de la mediana del celular en ↓ y ↑, latencia ≤ celular + 10 ms.
6. Explicar diferencias por hardware: un flagship usa 4×4 MIMO, CA de 3+ portadoras y 5G SA; este módem soporta LTE 2/4/5/7/8/28 y NR n8/n28/n78 con CA reportada en `s_status`. Si el celular agrega bandas que el módem no tiene (p. ej. B1/B3 no aplican en Colombia, pero n2/n5 sí podrían), la brecha es esperable y hay que documentarla como limitación de producto.

Prueba de Wi‑Fi del módem (lo que verá el cliente): repetir §4.3 con el PC conectado al Wi‑Fi 5 GHz del módem a 2 m y a 8 m. La caída respecto a Ethernet es la pérdida del Wi‑Fi, no de la red celular.

## 5. Criterios de aceptación propuestos

| Criterio | Meta |
|---|---|
| Registro en red por operador | LTE en 100 % de sitios; NR donde el celular de referencia lo tenga |
| Velocidad vs. celular | ≥ 80 % de la mediana del celular (↓ y ↑) por sitio |
| Latencia | mediana ≤ 45 ms LTE / ≤ 30 ms NR; pérdida < 1 % en drive test |
| Estabilidad | 0 caídas de `ccinet0` en 8 h (`cm get_network_duration`, `stat_get_connect_time`) |
| Handover 5G↔4G | reglas TTL siguen activas (`ttl_rules=1` en cada muestra) y la IP WAN no cambia más de lo que cambia en el celular |
| Térmico | < 75 °C en descarga sostenida de 15 min |
| Autonomía | medir con cronómetro: powerbank llena → módem apagado, con 2 clientes y `drive --speed-every 6` |

## 6. TTL configurable ("Parámetro de red")

**Estado actual (2026-11-24):** instalado en el módem de pruebas y empaquetado para la casa matriz.

- **En la interfaz:** Router → Configuración del sistema → **Diagnóstico**, sección **"Parámetro de red"** debajo del
  ping. No tiene entrada propia en el menú y la pantalla nunca dice "TTL". Botones: **Aplicar** (en gris hasta que
  cambies algo), **Valor recomendado** (64, activo) y **Como de fábrica** (quita las reglas); los dos últimos piden
  Sí/No. Casilla **"Aplicar también al tráfico de entrada"** (marcada por defecto): hace que el ping muestre el valor
  configurado; es solo para confirmarlo, porque el operador no ve esos paquetes.
- **En el equipo (todo en `/system/etc`, persistente):** `config/netparam` (enabled/value/ipv6/iface/inbound),
  `firewall.netparam.sh` (include de fw3 `firewall.netparam` con `reload=1`), `www/netparam.cgi`,
  `www/html/router/netparam.html`, `www/js/panel/router/netparam.js`, 15 textos `lt_NetParam_*` en
  `www/properties/Messages_{es,en,cn}.properties` y respaldo de fábrica en `netparam-backup/`. Reglas en mangle
  POSTROUTING (salida) + INPUT/FORWARD (entrada), comentario `netparam`; log con etiqueta `netparam`.
  Nada se escribe solo en la flash: solo al guardar desde la web (105 bytes).
- **Verificar:** `scripts/rsh.sh 'sh /system/etc/firewall.netparam.sh status'`, o `ping 8.8.8.8` (muestra el valor).
- **Cambiar sin la web:** `scripts/rsh.sh 'uci set netparam.main.value=65; uci commit netparam; sh /system/etc/firewall.netparam.sh'`.
- **Instalar/actualizar en otro equipo:** `NOTION_PASS=... firmware-mods/netparam/tools/install.sh` (`--remove` para quitar).
  `scripts/ttl_persist_install.sh` es la versión vieja de TTL fijo: está bloqueado, no usarlo.

**Dónde está cada cosa:**

| Qué | Dónde |
|---|---|
| Código, pruebas e instalador | `firmware-mods/netparam/` (README.md explica cada archivo) |
| Instrucción de implementación y requisitos del estado final, para la casa matriz (inglés) | `firmware-mods/netparam/IMPLEMENTATION.md` y `REQUIREMENTS.md` |
| Especificación para la casa matriz (inglés / copia interna en español) | `firmware-mods/netparam/OEM-SPEC.md` / `OEM-SPEC.es.md` |
| Zip para la casa matriz | `firmware-mods/netparam/dist/netparam-oem-2026-11-24.zip` (no va a git; se regenera con `make-oem-package.sh`); copia en `C:\Users\diego\Downloads\` |
| Pruebas para el firmware que entregue la casa matriz | `firmware-mods/netparam/tools/verify.sh` (estado del equipo, R0–R8; primero `make-oem-package.sh` para el manifiesto), `tools/test_cgi.py` (backend) y `tools/test_ui.py` (interfaz en Chromium) |

**Bitácora:**

- 2026-09-16: primera versión, TTL fijo 64 (`scripts/ttl_persist_install.sh`). Sobrevive a reinicios y cambios 4G/5G
  gracias al include de fw3 con `reload=1`.
- 2026-11-23: valor en archivo de configuración y editable desde la web (página propia en Configuraciones
  avanzadas). Probado: reinicio x2, `fw3 reload`, cambio de modo, interfaz en Chromium. "Solo 5G" (SA) no se pudo
  probar: Claro no tiene SA en la oficina. Spec para la casa matriz con manifiesto de archivos; en inglés (código,
  comentarios y logs también), interfaz en español.
- 2026-11-24:
  - Se movió adentro de Diagnóstico, sin entrada de menú y sin la palabra "TTL" en pantalla (para no tener problemas
    con operadores).
  - Botones Valor recomendado / Como de fábrica con confirmación, y Aplicar en gris hasta que haya un cambio.
  - Nombres internos neutros: `notion_ttl` → `netparam`. El instalador migra los equipos con la versión anterior.
  - Casilla de tráfico de entrada (el ping muestra el valor).
  - Verificado con captura en `ccinet0`: con valor 70 los paquetes salen con TTL 70. Reinicio con 66: siguió en 66.
  - Zip para la casa matriz. Incluye `IMPLEMENTATION.md` y `REQUIREMENTS.md` (estado final R0–R12), y `files/`, con los
    10 archivos finales en su ruta del equipo, listos para pegar (en OpenWrt: carpeta `files/` del buildroot). También
    trae `merge/` para el caso de que sus archivos difieran, y `tools/verify.sh`. Los archivos de fábrica se sacaron de
    la squashfs del equipo. Verificado: copiar `files/`, aplicar el patch y correr `apply-to-rootfs.sh` dan archivos
    idénticos, y el router reinstalado desde el zip pasa `verify.sh` con todos los MD5 y `test_cgi.py`.
  - Bug encontrado con `verify.sh` y corregido: si el script corría dos veces a la vez (al arrancar, o en un re-attach
    junto con la web), quedaban reglas duplicadas. Se reprodujo con 5 ejecuciones en paralelo (9 y 11 reglas). Ahora
    usa un candado (`lock` de busybox, máximo 10 s): con 3 rondas de 8 en paralelo quedan siempre 3+3, y tampoco hay
    duplicados al reiniciar. El tráfico nunca salió con otro valor, porque los duplicados tenían el mismo valor.
  - El zip se simplificó: la casa matriz solo copia y compila, y H3S prueba. Lleva `IMPLEMENTATION.md`,
    `REQUIREMENTS.md`, la captura, `VERSION.txt` y `files/`. La spec larga, `tools/` y el manifiesto se quedan en el repo.
  - Ojo: el módem no detecta una SIM que se pone con el equipo encendido (`AT+CPIN?` → `CME ERROR: 10`). Hay que
    reiniciarlo. Después del reinicio se reinició solo una vez más; no se vio la causa.
- Pendiente: cuando la casa matriz entregue el firmware, correr `tools/verify.sh`, `tools/test_cgi.py` y
  `tools/test_ui.py` sobre él.

## 7. Comandos rápidos

```bash
python3 scripts/notion5g.py status                 # snapshot legible
python3 scripts/notion5g.py status --json --save   # y lo anexa a results/status.csv
python3 scripts/notion5g.py speed --runs 3 --ookla-server 12345
python3 scripts/notion5g.py drive --interval 10 --gps win
python3 scripts/notion5g.py at 'AT+C5GREG?'
python3 scripts/notion5g.py ubus cm get_zcainfo
python3 scripts/summarize.py --md
scripts/rsh.sh 'logread -f'                        # log en vivo del módem (handovers, cm, netparam)
```

## 8. Pendientes / ideas

- ~~Ingeniería inversa de `login.cgi`~~ HECHO (2026-09-18): el API vivo del equipo es XML sobre `POST /xml_action.cgi?method=set` (los `json_*.cgi` están muertos, siempre devuelven `{"result":"fail"}`). Autenticación HTTP Digest con nonce/realm fijos ("1000"/"Highwmg"); credenciales admin/admin. Cliente de referencia en `scripts/modem_http_api.py` (ver docstring para el protocolo completo). Esto habilita que la futura app de celular lea la señal del módem por HTTP normal, sin SSH.
  **Cuidado:** el login bloquea temporalmente tras varios intentos fallidos (`status=5,left_time=N` en el header `WWW-Authenticate`); no automatizar reintentos de contraseña.
- Cerrar credenciales por defecto, `country=CO`, zona horaria GMT‑5 y decidir sobre TR‑069 antes de la venta.
- Explorar `sim set_double_sim_status` con perfiles eSIM reales de Notion si se van a comercializar.
- Si hace falta posición nativa, evaluar un GNSS externo por USB/UART al SoC (hay `ttyS1` libre) como opción de hardware.
