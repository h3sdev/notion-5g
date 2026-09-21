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

## 6. TTL persistente (ya instalado)

- Por qué se borraba: cada `ifup` de `wan0` (re‑attach 5G↔4G) dispara `fw3 reload`, que reconstruye iptables.
- Instalado: `/system/etc/firewall.notion-ttl.sh` registrado como `include` con `reload=1`. Fija **TTL 64** e **IPv6 Hop Limit 64** en salida por `ccinet+`. Se ejecuta en cada arranque y cada reload.
- Verificar: `scripts/rsh.sh 'iptables -t mangle -S POSTROUTING | grep notion-ttl'` o columna `ttl_rules` de cualquier muestra.
- Cambiar valor: `scripts/ttl_persist_install.sh 65`. Quitar: `scripts/ttl_persist_install.sh --remove`.
- Nota: la regla está en POSTROUTING (después del decremento de reenvío), así que el valor configurado es el TTL real que sale por la antena; 64 es lo que emite un celular.

## 7. Comandos rápidos

```bash
python3 scripts/notion5g.py status                 # snapshot legible
python3 scripts/notion5g.py status --json --save   # y lo anexa a results/status.csv
python3 scripts/notion5g.py speed --runs 3 --ookla-server 12345
python3 scripts/notion5g.py drive --interval 10 --gps win
python3 scripts/notion5g.py at 'AT+C5GREG?'
python3 scripts/notion5g.py ubus cm get_zcainfo
python3 scripts/summarize.py --md
scripts/rsh.sh 'logread -f'                        # log en vivo del módem (handovers, cm, notion-ttl)
```

## 8. Pendientes / ideas

- ~~Ingeniería inversa de `login.cgi`~~ HECHO (2026-09-18): el API vivo del equipo es XML sobre `POST /xml_action.cgi?method=set` (los `json_*.cgi` están muertos, siempre devuelven `{"result":"fail"}`). Autenticación HTTP Digest con nonce/realm fijos ("1000"/"Highwmg"); credenciales admin/admin. Cliente de referencia en `scripts/modem_http_api.py` (ver docstring para el protocolo completo). Esto habilita que la futura app de celular lea la señal del módem por HTTP normal, sin SSH.
  **Cuidado:** el login bloquea temporalmente tras varios intentos fallidos (`status=5,left_time=N` en el header `WWW-Authenticate`); no automatizar reintentos de contraseña.
- Cerrar credenciales por defecto, `country=CO`, zona horaria GMT‑5 y decidir sobre TR‑069 antes de la venta.
- Explorar `sim set_double_sim_status` con perfiles eSIM reales de Notion si se van a comercializar.
- Si hace falta posición nativa, evaluar un GNSS externo por USB/UART al SoC (hay `ttyS1` libre) como opción de hardware.
