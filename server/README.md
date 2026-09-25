# server — backend de pruebas de campo del Notion 5G

Backend en Go (SQLite embebido, sin CGO) que recibe mediciones del módem desde
`scripts/notion5g.py` (PC), desde `cmd/routeragent` (corriendo en el propio
router) y, más adelante, desde la app de celular.

## Arrancar

```bash
cp .env.example .env   # define API_KEY
docker compose up -d --build
curl http://localhost:8080/healthz
```

Sin Docker, para desarrollo:

```bash
DB_PATH=./dev.db API_KEY=dev-key LISTEN_ADDR=:8080 go run ./cmd/server
```

## Exposición pública (para que el router llegue por su propia SIM)

El backend necesita una URL alcanzable desde Internet — el router no tiene
cliente VPN, así que un túnel corporativo del lado del PC no le sirve de nada.

**Ya desplegado en producción (2026-09-18): https://notion.h3s-iot.com**, en el
VPS de H3S, con su propio Cloudflare Tunnel (mismo patrón que otros proyectos
de la casa: `cf-tunnel.sh`, token-based, sin puerto HTTP publicado al host,
red docker aislada). Código sincronizado en ese VPS en `~/notion-5g-server/`;
la API key real vive solo en el `.env` de esa máquina, no en este repo.

Si hace falta reconstruir el túnel desde cero (otro entorno, otra zona DNS):
copiar `cf-tunnel.sh` de cualquier otro proyecto de la casa (h9303-fleet, orca,
doppio) a este directorio, ajustar `SERVICE`/`TUNNEL_NAME` por defecto, y
correr `./cf-tunnel.sh <host>.h3s-iot.com` — crea/reusa el túnel, configura el
ingress y el DNS por la API de Cloudflare, y escribe el token en `.env` (600).
Requiere `CLOUDFLARE_API_TOKEN` (scopeado a la zona), normalmente ya presente
en `~/.config/h3s/credentials.env` en el VPS.

## Endpoints

| Método y ruta | Qué hace |
|---|---|
| `GET /healthz` | Sin auth. Chequeo de vida. |
| `POST /api/v1/measurements` | Inserta una medición (objeto) o varias (arreglo). Body = el mismo dict que ya produce `notion5g.py`. |
| `GET /api/v1/measurements` | Lista/filtra por `device_id`, `tag`, `operator`, `since`, `limit`. |
| `GET /api/v1/measurements/summary` | Promedio/mediana de `down_mbps`/`up_mbps`/`rsrp_dbm` agrupado por `operator`\|`tag`\|`device_id`. |
| `POST /api/v1/heartbeat` | Ping liviano: uptime + señal, sin prueba de velocidad. |
| `GET /api/v1/heartbeats` | Lista heartbeats por `device_id`. |
| `POST /api/v1/commands` | El celular pide una prueba: `{"device_id":"router-xxx","type":"run_speedtest","duration_s":10,"lat":...,"lon":...,"gps_accuracy_m":...,"gps_source":"android-fused","requested_by":"..."}`. |
| `GET /api/v1/commands/next?device_id=` | El router hace *polling* aquí (no puede recibir conexiones entrantes por su NAT celular) y se lleva el comando pendiente más viejo, marcado `claimed`. |
| `POST /api/v1/commands/{id}/complete` | El router reporta el resultado: `{"status":"done","measurement":{...}}` o `{"status":"failed","error":"..."}`. La ubicación del comando original se fusiona automáticamente en la medición (el módem no tiene GPS propio). |
| `GET /api/v1/commands` | Lista/filtra comandos por `device_id`/`status`, para depurar. |
| `GET /api/v1/speedtest/download?bytes=N` | Sink de descarga (por defecto 10 MB, tope 500 MB) para medir velocidad contra este mismo servidor. Devuelve `X-Speedtest-Concurrent`: cuántas pruebas están corriendo a la vez contra este backend. |
| `POST /api/v1/speedtest/upload` | Sink de subida: lee y descarta el body, devuelve `received_bytes` y `concurrent`. |

Todo excepto `/healthz` requiere el header `X-API-Key` si `API_KEY` está
definida (siempre debería estarlo fuera de desarrollo local).

## Agente del router (`cmd/routeragent`)

Binario corto pensado para correr **en el propio equipo** (ASR1901/OpenWrt),
invocado por cron — nunca como demonio persistente, porque el router solo
tiene ~30 MB de RAM libres. En una corrida mide ~8.7 MB de RSS pico.

Cross-compilar (sin CGO, no depende de la libc del router):

```bash
cd server
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
    -o routeragent-arm ./cmd/routeragent
```

Config en el router, `/data/notion5g-agent.json` (partición escribible; `/etc`
es solo lectura en este equipo):

```json
{"backend_url": "https://notion.h3s-iot.com", "api_key": "...", "device_id": "router-<serie>"}
```

`device_id` es opcional: si falta, el agente lee el número de serie del propio
equipo (`ubus call cm get_link_context`) y lo guarda de vuelta en el archivo.

Cada corrida hace **una sola cosa** y termina: si hay un comando `run_speedtest`
pendiente para este `device_id`, lo ejecuta (mide señal local vía `ubus` — nunca
`serial_atcmd`, el puerto AT es un canal único compartido con el firmware y un
cron externo no debe competir por él — corre una descarga y subida cronometradas
contra los endpoints de este backend, y reporta) y lo marca `done`/`failed`; si
no hay ninguno, manda un heartbeat liviano (uptime + señal, sin prueba de velocidad).

Entrada de cron sugerida (cada 5 minutos):

```
*/5 * * * * /data/routeragent-arm >> /tmp/log/notion5g-agent.log 2>&1
```

**Instalado y corriendo en el módem físico desde 2026-09-18**: binario en
`/data/routeragent-arm`, config apuntando a https://notion.h3s-iot.com, cron
cada 5 min en `/etc/crontabs/root` (persiste reinicios: esa ruta es symlink a
`/system/etc/crontabs`, un overlay escribible, no al squashfs de solo lectura).

## Notas de diseño

- Cada medición se guarda con su JSON completo (columna `raw`) además de las
  columnas indexadas que se usan para filtrar/agregar: el esquema no se rompe
  si un cliente nuevo manda campos que todavía no existían.
- `SetMaxOpenConns(1)` + SQLite en modo WAL: un solo escritor a la vez, sin
  "database is locked"; suficiente para el volumen de datos esperado (unos
  pocos dispositivos reportando cada minuto, no miles).
- La prueba de velocidad propia (`/api/v1/speedtest/*`) existe para tener una
  ruta con latencia representativa del mismo operador cuando el servidor está
  en Colombia; para medir el **techo real** de velocidad (que puede superar la
  capacidad de subida del servidor de oficina) se sigue recomendando un
  servidor Ookla público de alta capacidad, como ya hace `notion5g.py speed`.

## Probar dos equipos en paralelo

El backend nunca fue el problema: comandos, heartbeats y mediciones están
indexados por `device_id` y cada router hace su propio polling, así que dos (o
diez) equipos reportando al mismo tiempo no se pisan. Lo que sí hay que tener
en cuenta en campo:

- **El dashboard maneja un equipo por pestaña.** `state.selectedDeviceId` es
  uno solo, y entrar a otro equipo apaga el modo en movimiento del anterior.
  Para seguir dos equipos a la vez hay que abrir **dos pestañas o dos
  celulares** — uno conectado al WiFi de cada router. Desde 2026-09-19 la
  vista de detalle tiene URL propia (`/#/device/<device_id>`), así que ese link
  se guarda, se comparte y sobrevive a un refresh sin volver a la lista.
- **Dos pruebas contra `/api/v1/speedtest/*` al mismo tiempo se reparten el
  enlace del VPS**, y las dos mediciones salen bajas sin que se note. El
  servidor ahora cuenta las pruebas en vuelo y lo devuelve en el header
  `X-Speedtest-Concurrent` (y en el JSON de `/upload`); el dashboard lo lee, lo
  avisa en pantalla y lo deja anotado en la medición (`concurrent_tests`).
- Para medir en paralelo de verdad está el botón **"Prueba contra Internet
  (Cloudflare)"**: mide contra `speed.cloudflare.com` (CDN global), así que ni
  topea por el enlace del VPS ni compite con el otro equipo. Guarda la medición
  igual, con `tag=browser-speedtest-cloudflare` y además `ping_ms`/`jitter_ms`.

### ¿Y fast.com?

No se puede desde el navegador: los endpoints de Netflix (`api.fast.com` y sus
CDN) **no mandan cabeceras CORS** (verificado 2026-09-19: la respuesta no trae
`Access-Control-Allow-Origin`), así que el navegador bloquea la lectura desde
esta página. Proxearlo por el backend tampoco sirve: mediría el enlace del VPS
contra Netflix, no el del equipo. `speed.cloudflare.com` es el equivalente que
sí permite CORS (`Access-Control-Allow-Origin: *` en `__down` y en `__up`) y es
lo que se usa en su lugar.
