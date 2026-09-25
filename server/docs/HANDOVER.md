# Handover — backend de pruebas de campo del módem Notion 5G

> Escrito al cierre de la sesión del **2026-09-18** (la sesión en la que se construyó todo esto de
> cero: backend, agente del router, dashboard, app Flutter). Si algo de aquí contradice al código,
> gana el código: verifícalo antes de creerle a este documento.

---

## 1. Qué es esto en una frase

Backend en Go para las pruebas de campo del módem 5G que H3S está homologando: recibe señal,
operador, ubicación y velocidad desde el router físico y desde un navegador/celular, y expone un
dashboard web para verlo y disparar pruebas — todo publicado en **https://notion.h3s-iot.com**.

El proyecto completo (no solo este servidor) vive en el repo `notion-5g` de otra máquina (WSL de
Diego), carpeta `server/`. **Este directorio en el VPS (`~/notion-5g-server/`) es una copia
sincronizada por `rsync` para desplegar — no es un repo git, no edites aquí directamente si vas a
seguir trabajando desde el repo, o el próximo `rsync` te lo pisa.** Si vas a seguir trabajando
**desde este VPS** en vez de desde el repo original, dilo explícitamente y hay que decidir cuál
lado queda como fuente de verdad.

---

## 2. Qué leer, y en qué orden

| Archivo | Para qué |
|---|---|
| Este documento | Panorama completo y gotchas reales que ya mordieron en producción |
| `README.md` (este directorio) | Referencia de los endpoints HTTP, uno por uno |
| `internal/store/store.go` | Esquema de datos real (measurements, heartbeats, commands, device_locations) |
| `internal/api/api.go` | Handlers HTTP |
| `cmd/routeragent/main.go` | El binario que corre EN el módem físico (no aquí) |
| `internal/web/static/` | El dashboard (HTML/CSS/JS vanilla, embebido en el binario) |

No hay un `docs/02-plan-*` ni specs largas como en otros proyectos de la casa — este es un proyecto
chico, nacido y crecido en una sola sesión larga. La documentación real está en el código y en este
handover.

---

## 3. Dónde estamos

```
Backend Go              DONE   corriendo en producción, probado con datos reales del router
Agente del router       DONE   instalado en el módem físico, cron cada 1 min, sin comandos AT
Dashboard web           DONE   estado en línea, ping, ubicación, banda, uptime, disparo de pruebas
App Flutter             DONE, NO USADA   construida y verificada, pero Diego decidió que el
                                navegador (geolocalización + "modo en movimiento" ahí mismo)
                                reemplaza el caso de uso; no se instaló en ningún celular
Túnel de Cloudflare     DONE   propio, token-based, mismo patrón que el resto de la casa
```

Todo lo de arriba está **en producción real**, no es una demo. El módem que reporta es un H3S
P52HL / ASR1901, `router-R524260829000001` (device_id derivado de su número de serie), en manos de
Diego para pruebas de campo con SIM de Tigo.

---

## 4. Arquitectura

**Backend** (`cmd/server`, `internal/api`, `internal/store`): Go puro, SQLite embebido vía
`modernc.org/sqlite` (**sin CGO** — importa porque el `Dockerfile` cross-compila sin toolchain C).
Un solo binario, sirve la API HTTP y el dashboard estático. Auth: header `X-API-Key` en todo menos
`/healthz` y el propio dashboard (`GET /`, `/assets/*`).

Modelo de datos (`internal/store/store.go`): tres cosas que llegan por HTTP —
- **measurements**: una prueba completa (operador, banda, señal, velocidad, ubicación). Las manda
  `notion5g.py` (PC, por SSH al módem) o el agente del router al completar un comando.
- **heartbeats**: ping liviano y MUY frecuente (cada 1 min desde el router: señal + uptime + ping +
  ubicación, sin velocidad). Es la unidad del "perfil de ping con ubicación" en movimiento.
- **commands**: cola de trabajo. El celular/navegador crea un comando `run_speedtest` para un
  `device_id`; el router (detrás de NAT celular, no puede recibir conexiones entrantes) hace
  *polling* (`GET /api/v1/commands/next?device_id=`), lo ejecuta, y lo cierra con el resultado
  (`POST .../complete`). Si un comando queda `claimed` sin cerrarse (el router se cayó a mitad de la
  prueba) se puede volver a tomar solo a los 3 minutos (`staleClaimAfter` en `ClaimNextCommand`).
- **device_locations**: la ubicación MÁS RECIENTE de cada `device_id`, mandada por un navegador o
  celular (`POST /api/v1/devices/{id}/location`). El módem no tiene GPS propio; esta tabla es lo que
  permite que sus heartbeats/mediciones lleven coordenadas de todas formas — se fusiona
  automáticamente si el reporte del router llega sin `lat`/`lon` (y no está más vieja que
  `maxLocationAge`, 10 min).

Todo lo guardado conserva su JSON completo en una columna `raw`, además de columnas indexadas para
filtrar/agregar — el esquema no se rompe si un cliente nuevo manda campos que no existían (ver §5.1
para el matiz importante de esto).

**Agente del router** (`cmd/routeragent`): binario Go corto, cross-compilado
`GOOS=linux GOARCH=arm GOARM=7` sin CGO, corre **en el propio módem** por cron cada 1 minuto — nunca
como demonio (el equipo tiene ~96 MB de RAM, solo ~30 MB libres; picos medidos de ~8.7 MB de RSS).
Lee señal local con `ubus` — **nunca con `serial_atcmd`/comandos AT directos**: el puerto AT es un
canal único que comparte con el propio firmware del módem, y un cron externo compitiendo por él
puede dejarlo bloqueado (esto lo pidió Diego explícitamente al ver el diseño original). Manda un
`ping -c3 -W2 8.8.8.8` real en cada heartbeat (parsea tanto formato busybox como iputils).

**Dashboard** (`internal/web/static/`): HTML/CSS/JS vanilla sin build step ni dependencias externas,
embebido en el binario Go con `embed.FS` y servido en `/` y `/assets/*` **sin autenticación** (la
API key se pide/guarda en la propia página — ver el gotcha de seguridad en §5.4). Lista de equipos
con estado en línea/fuera de línea, señal, banda, ping, ubicación, velocidad y uptime; vista de
detalle por equipo con historial de mediciones/heartbeats/comandos; botón para disparar una prueba
(toma la ubicación del propio navegador si el usuario da permiso); interruptor "modo en movimiento"
que manda la ubicación del navegador cada 15s mientras esté prendido (reemplaza a la app nativa para
ese caso de uso).

**App Flutter** (`mobile/` en el repo, no en este VPS): construida, `flutter analyze`/`build`/`test`
en verde, pero **Diego decidió no instalarla** — la geolocalización del navegador + el dashboard
cubren el mismo caso de uso sin instalar nada. Queda documentada por si se retoma.

---

## 5. Gotchas reales (todos pasaron de verdad esta sesión, no son hipotéticos)

### 5.1 — Migraciones de esquema: `CREATE TABLE IF NOT EXISTS` NO agrega columnas

Al agregarle `ping_ms`/`loss_pct`/`lat`/`lon`/`gps_accuracy_m`/`gps_source` a `heartbeats`, la tabla
YA EXISTÍA en producción (de un despliegue anterior) con el esquema viejo — `CREATE TABLE IF NOT
EXISTS` es un no-op sobre una tabla existente, no le agrega nada. Esto **rompió el dashboard en
producción** (`/api/v1/devices` daba `{"error":"error de consulta"}`, log real: `no such column:
ping_ms`) hasta que Diego reportó "no veo el equipo ni datos". Arreglado con `addColumnIfMissing()`
en `migrate()`: intenta `ALTER TABLE ... ADD COLUMN` y traga el error si ya existe (`strings.Contains(err.Error(), "duplicate column name")`)
— el idiom estándar en SQLite sin un framework de migraciones.
**Regla de aquí en adelante: cualquier columna nueva en una tabla que ya pudo haber existido en
producción va TAMBIÉN a la lista de `addColumnIfMissing` en `migrate()`.** Antes de desplegar un
cambio de esquema, simular la tabla vieja localmente y confirmar que migra limpio (hay un ejemplo de
cómo hacerlo en el historial de esta sesión: crear la tabla a mano con `sqlite3`/`python3` con el
esquema viejo, insertar una fila, y arrancar el servidor encima).

### 5.2 — El `docker-compose.yml`/`cf-tunnel.sh` de este VPS NO son los del repo

El repo (`server/docker-compose.yml`) tiene una versión **genérica** pensada para desarrollo local
(puerto publicado, un contenedor iperf3 opcional). Este VPS usa una versión **distinta**, adaptada
al patrón de la casa: red docker aislada `notion5g`, sin puerto HTTP publicado al host (VPS
compartido con otros ~20 contenedores de otros proyectos), `container_name` fijos,
`docker-compose.yml`/`cf-tunnel.sh` locales a este directorio, volumen nombrado
`notion-5g-server_notion5g_data` (`external: true` en el compose, para no perderlo si el proyecto
cambia de nombre otra vez).

**Ya pasó de verdad**: un `rsync` sin excluir estos dos archivos sobreescribió el compose bueno con
el genérico del repo, tumbó los contenedores un momento y chocó de puerto con `cadvisor` (que ya
usa `127.0.0.1:8080` en este VPS). Los datos no se perdieron (volumen nombrado, sobrevive a
`docker compose down` sin `-v`), pero el susto fue real.

**Comando de sync correcto, siempre así**:
```bash
rsync -az --exclude 'data' --exclude '.env' --exclude 'docker-compose.yml' --exclude 'cf-tunnel.sh' \
  -e 'ssh -o BatchMode=yes -o ConnectTimeout=6' \
  <ruta al repo>/server/ h3s@192.168.40.214:/home/h3s/notion-5g-server/
```

### 5.3 — Cloudflare cachea `/assets/*` e ignora el `Cache-Control` del origen

Después de CUALQUIER cambio al dashboard (`internal/web/static/`), no basta con `docker compose up
-d --build server` — Cloudflare sirve la versión vieja cacheada (hasta 4h) porque **no respeta** el
header `Cache-Control: no-cache` que el propio servidor ya manda para esas rutas (`internal/web/embed.go`).
Hace falta purgar el caché a mano después de cada despliegue del dashboard:
```bash
set -a; . ~/.config/h3s/credentials.env; set +a
ZID=$(curl -fsS -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones?name=h3s-iot.com" | python3 -c "import sys,json;print(json.load(sys.stdin)['result'][0]['id'])")
curl -fsS -X POST -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "Content-Type: application/json" \
  "https://api.cloudflare.com/client/v4/zones/$ZID/purge_cache" \
  --data '{"files":["https://notion.h3s-iot.com/assets/app.js","https://notion.h3s-iot.com/assets/style.css","https://notion.h3s-iot.com/"]}'
```
(El `CLOUDFLARE_API_TOKEN` de `~/.config/h3s/credentials.env` sí tiene permiso de purga, confirmado
en esta sesión — está scopeado solo a la zona `h3s-iot.com`.)

### 5.4 — La API key vive escrita en el propio JS del dashboard, a propósito

`internal/web/static/assets/app.js`, constante `DEFAULT_API_KEY`. Diego pidió explícitamente que el
dashboard funcionara sin escribir nada (ni siquiera en modo incógnito) porque salía a una prueba de
campo con prisa, y **aceptó conscientemente** (se le preguntó directo) que esto deja la clave visible
en el código fuente de la página para cualquiera con el link. Hoy el link no está compartido
públicamente, así que el riesgo real es bajo, pero **si el link llega a compartirse más ampliamente,
hay que rotar `API_KEY` en `.env` de este VPS y quitar (o cambiar el criterio de) este default**
antes de eso — no es un descuido, es una decisión tomada con el trade-off explícito sobre la mesa.

### 5.5 — El túnel de Cloudflare, cómo se hizo

Se usó el mismo patrón que ya usan `carta-h3s`/`h9303-fleet`/`orca`/`doppio`/`omnicap` en este VPS:
**un túnel propio por proyecto**, token-based, con el ingress/DNS administrado por la API de
Cloudflare (no en un `config.yml` local). Se creó con el script estándar de la casa, copiado y
adaptado a este proyecto: `cf-tunnel.sh notion.h3s-iot.com` (ver el archivo en este directorio) —
crea/reusa el túnel, fija el ingress `notion.h3s-iot.com → http://notion5g-server:8080`, apunta el
CNAME, y escribe `CLOUDFLARE_TUNNEL_TOKEN` en `.env` (600) sin que nadie tenga que ver el valor del
token a mano. Requiere `CLOUDFLARE_API_TOKEN` (Tunnel a nivel de cuenta + DNS a nivel de zona,
scopeado a `h3s-iot.com`), que por defecto se lee de `~/.config/h3s/credentials.env` si no está en
el entorno. Túnel: nombre `notion-5g`, id `42aa8253-b655-4259-8b94-3e491da23519`. **No es el mismo
túnel que usa `omnicap`** (`captive.h3s-iot.com` tiene el suyo aparte) ni se tocó su configuración.

### 5.6 — Limitación real de "modo en movimiento" (app y navegador, ambos)

Tanto el interruptor del dashboard como el que tenía la app Flutter dependen de un timer que solo
corre con la pestaña/app en primer plano y la pantalla encendida. Los navegadores (y Android en
segundo plano) frenan o pausan `setInterval`/`Timer` para ahorrar batería — no hay forma de evitarlo
sin un Service Worker con push (navegador) o un foreground service real (Android), ninguno de los
dos se construyó (se decidió así explícitamente, ver conversación). El dashboard al menos avisa en
pantalla (`visibilitychange`) cuando la pestaña deja de estar visible, en vez de fallar en silencio.

---

## 6. Cómo desplegar un cambio (resumen operativo)

```bash
# 1. sync del código (SIEMPRE excluyendo compose/cf-tunnel, ver §5.2)
rsync -az --exclude 'data' --exclude '.env' --exclude 'docker-compose.yml' --exclude 'cf-tunnel.sh' \
  -e 'ssh -o BatchMode=yes -o ConnectTimeout=6' <repo>/server/ h3s@192.168.40.214:/home/h3s/notion-5g-server/

# 2. rebuild + restart (--build es obligatorio, "up -d" solo no reconstruye la imagen)
ssh h3s@192.168.40.214 'cd ~/notion-5g-server && docker compose up -d --build server'

# 3. si tocaste el dashboard (internal/web/static/), purgar Cloudflare (ver §5.3)

# 4. si tocaste el esquema (internal/store/store.go), confirmar que migró sin
#    error: docker logs notion5g-server | tail, buscando "no such column"
```

VPS: `h3s@192.168.40.214` (alcanzable por la VPN "OFC H3S"; llave SSH ya autorizada, sin contraseña).
Directorio: `~/notion-5g-server/`. Contenedores: `notion5g-server`, `notion5g-cloudflared`. Red
docker: `notion5g`. Volumen de datos: `notion-5g-server_notion5g_data` (persistente, sobrevive a
`docker compose down` sin `-v`).

---

## 7. Pendientes / ideas

- El agente del router y `notion5g.py` (PC, vía SSH) reportan con nombres de campo casi idénticos
  pero no 100% iguales en todos los casos (p. ej. `down_error`/`up_error` en el router no están
  indexados en `Measurement`, solo viven en `raw`) — no es un bug, pero conviene tenerlo presente si
  se agregan más columnas indexadas más adelante.
- No hay un endpoint de exportación (CSV/JSON masivo) para análisis fuera del dashboard — si hace
  falta comparar operadores/sitios en una hoja de cálculo, hoy hay que pegarle a `/api/v1/measurements`
  con `limit` alto y procesar a mano.
- La app Flutter (`mobile/`) quedó completa pero sin uso; si más adelante se necesita background real
  (pantalla apagada), ese es el punto de partida — ver su propio README para lo que falta
  (`flutter_foreground_task`/`workmanager`, `FOREGROUND_SERVICE_LOCATION`). **Ojo: ese directorio no
  está en el checkout del VPS ni en la máquina de Diego, solo el backend.** Antes de retomarlo, leer
  `docs/PLAN-APP-MOVIL.md` (2026-09-19): la conclusión es que iOS no expone señal celular por ninguna
  API pública, y que el camino de mejor relación costo/beneficio no es la app sino dejar de tirar los
  datos que el agente del router YA lee (ver `readLocalStatus` vs `sendHeartbeat`).
- Considerar mover `DEFAULT_API_KEY` (§5.4) a algo menos expuesto si el link del dashboard se
  comparte más ampliamente.

## Addendum 2026-09-19 — pruebas de dos equipos en paralelo

Revisión disparada por la pregunta de campo: "¿qué pasa si quiero probar dos equipos a la vez?".

**Lo que ya funcionaba:** el backend es multi-equipo de nacimiento — comandos, heartbeats y
mediciones van por `device_id`, y cada router hace su propio polling de `/commands/next`, así que
dos routers reportando en simultáneo no se pisan ni compiten por nada.

**Lo que no:**

1. El dashboard mantiene un solo `state.selectedDeviceId` y no tenía URL por equipo, así que no
   había forma de fijar una pestaña a un equipo (un refresh volvía a la lista). Se agregó ruteo por
   hash: `/#/device/<device_id>`. Dos equipos = dos pestañas (o dos celulares, uno por router, que
   es lo que hace falta igual: un navegador está conectado a un solo router a la vez).
2. Dos pruebas de velocidad simultáneas contra `/api/v1/speedtest/*` se reparten el enlace del VPS
   y las dos salen bajas, sin ninguna señal de que pasó. Ahora el servidor cuenta las pruebas en
   vuelo (`Server.speedInFlight`) y lo publica en `X-Speedtest-Concurrent` (descarga) y en el JSON
   de `/upload`. El dashboard toma el máximo de los dos — el contador de la descarga se lee al
   empezar, así que el primero de los dos equipos vería 1 si el otro entra después — lo avisa en
   pantalla y lo guarda en la medición como `concurrent_tests`.
3. Se agregó el botón **"Prueba contra Internet (Cloudflare)"** (`speed.cloudflare.com`), que es la
   forma correcta de medir dos equipos en paralelo: cada navegador llega a un edge distinto del CDN,
   no topea por el enlace del VPS y da además ping/jitter. Se guarda con
   `tag=browser-speedtest-cloudflare`.

**fast.com quedó descartado, no pendiente:** sus endpoints no mandan cabeceras CORS (verificado con
`curl -H Origin:` el 2026-09-19), así que el navegador no deja leer la respuesta. Proxearlo por el
backend mediría el enlace del VPS contra Netflix, no el del equipo — no responde la pregunta.

## Addendum 2026-09-24 — combinaciones NSA en el resumen de bandas

Diego reportó que el resumen de bandas no muestra qué combinaciones NSA se midieron. Eran dos
problemas distintos, uno de UI y uno de fondo:

1. **UI:** `renderBandSummary` aplanaba `band_lte` y `nr_band` en dos conjuntos independientes, así
   que el emparejamiento se perdía. Ahora cuenta además cada PAR observado en las mediciones cuyo
   `rat` es NSA/ENDC y lo muestra como "Combinaciones NSA medidas" (`B2 + n78 ×4`). De paso, las
   bandas ahora se ordenan por número: antes el orden alfabético daba B2, B28, B4, B7.

2. **De fondo — por esto estaba vacío:** el agente leía SOLO el sub-objeto `lte` de `get_zcainfo`
   (`cmd/routeragent/main.go:415`), así que `nr_band` llegaba **siempre** nulo. Verificado contra
   producción: 0 de 74 mediciones tenían `nr_band`, y había 4 marcadas `5G-NSA` con `band_lte=2` y
   NR nulo. El agente ahora lee también el bloque NR.

**Ojo con el bloque NR:** no hay documentación del firmware de este equipo y no se pudo inspeccionar
un `get_zcainfo` con NR activo (el router venía midiendo en LTE), así que **los nombres de los campos
NR son candidatos, no están confirmados**. El código prueba varios (`n_band`/`band`/`nr_band`/`p_band`
dentro de `nr`/`nr5g`/`endc`/...) y, si no encuentra la banda pero sí el bloque, adjunta el objeto
crudo en `zcainfo_nr_raw` — que termina en la columna `raw` del backend. Con la primera medición que
enganche 5G se ven los nombres reales; ahí se reemplaza por los exactos y se borra el volcado.

Hasta que eso pase, el dashboard muestra honestamente `B2 + NR sin identificar` y explica por qué,
en vez de omitir la combinación o inventar una banda.

**Esto requiere reinstalar el agente en el router** (no alcanza con redesplegar el backend):

```bash
cd ~/notion-5g-server
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
    -o routeragent-arm ./cmd/routeragent
# copiar a /data/routeragent-arm en el equipo (cron ya lo invoca cada 5 min)
```

## Addendum 2026-09-25 — estado del despliegue y pendientes

**Desplegado en producción el 2026-09-24** (`docker compose up -d --build server` + purga de
Cloudflare), con copia previa de la base en el scratchpad de esa sesión
(`backup-prod-20260924-022439/`, con WAL; 74 mediciones / 1505 heartbeats / 43 comandos):

- Ruteo por hash, aviso de pruebas concurrentes y prueba contra Cloudflare (addendum 2026-09-19).
- **Identificación de la red de salida** (`internal/api/clientip.go`, `netclass.go`, `asn.go`):
  el servidor cruza la IP de `CF-Connecting-IP` contra la IP pública del router (de sus heartbeats,
  tabla `device_egress`), el ASN por DNS de Team Cymru y `navigator.connection.type`, y guarda
  `net_route`/`net_asn` con confianza `high`/`medium`/`unknown` (nunca `router` por defecto; CGNAT y
  mismo /48 IPv6 cuentan como señal débil). Lo que manda el cliente en `net_route`/`net_asn` se
  ignora. El ASN del router se resuelve **perezosamente** (no en la ingesta del heartbeat): que salga
  `null` al principio es esperado. Diagnóstico: `GET /api/v1/netinfo?debug=1`.
  Gate verificado en vivo: `cf_connecting_ip` llega por el túnel, `trusted_proxy: true`.
- Combinaciones NSA en el resumen de bandas (UI, addendum 2026-09-24).

Migración limpia (sin `no such column`), sin pérdida de datos.

**Pendientes, en orden:**

1. ~~**Reinstalar el agente del router.**~~ **Hecho el 2026-09-25.** Recompilado
   `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w"` desde el repo ya
   sincronizado (ver punto 4), copiado a `/data/routeragent-arm.new` por scp con los mismos parámetros
   legacy de `scripts/rsh.sh`, checksum MD5 verificado idéntico contra el binario local antes de
   instalarlo. Swap atómico en el equipo: binario viejo → `/data/routeragent-arm.bak` (queda de
   respaldo para rollback), nuevo → `/data/routeragent-arm`. Corrido a mano en el propio equipo:
   sale con código 0 y sin salida por stderr (el agente solo imprime en error; silencio = éxito,
   mismo comportamiento que el binario viejo). El cron no se tocó. `/data` quedó con 6.1 MB libres
   con los dos binarios presentes. Falta la confirmación de fondo: recién se ve `nr_band` real
   cuando el equipo enganche 5G (ver punto 2, sigue abierto).
2. **Con el agente nuevo y el router en 5G**, leer `zcainfo_nr_raw` en la columna `raw`, reemplazar
   los nombres candidatos del bloque NR por los reales y borrar el volcado. Esto además responde el
   gate de la fase 0 de `docs/PLAN-APP-MOVIL.md` (¿el router expone banda/PCI NR?), que decide si la
   app Android vale la pena.
3. **Probar la identificación de red desde el celular en campo:** `netinfo?debug=1` una vez por datos
   móviles y otra por el WiFi del router. Si salen la misma IP, es CGNAT compartido o se está saliendo
   por el router sin querer — cambia la confianza que se va a ver.
4. ~~**Fuente de verdad desincronizada.**~~ **Resuelto el 2026-09-25.** Los cambios hechos directo en
   este VPS desde el 2026-09-19 (identificación de red, NSA, paralelismo, `docs/PLAN-APP-MOVIL.md`) se
   trajeron de vuelta al repo `notion-5g` de la WSL de Diego con el rsync en sentido inverso descrito
   acá (excluyendo `data`/`.env`/`docker-compose.yml`/`cf-tunnel.sh`), revisados a mano (`go build`,
   `go vet`, `go test ./...` en verde, sin secretos en los archivos nuevos) y commiteados/pusheados a
   `origin/main` (`9d21dd3`). Sigue habiendo un solo sentido de verdad real: la próxima vez que se edite
   algo directo acá hay que repetir este mismo procedimiento antes del próximo despliegue.
5. ~~Aclarar la frecuencia real del cron del agente en el router.~~ **Resuelto el 2026-09-25:**
   `crontab -l` (`/etc/crontabs/root`) en el equipo confirma `*/1 * * * *` — es cada 1 minuto, el
   addendum 2026-09-24 que decía cada 5 estaba desactualizado.
6. Siguen abiertos los de §7 (exportación CSV, `DEFAULT_API_KEY` expuesta).

## Addendum 2026-09-25 (segunda parte) — app con pantalla apagada, batería del celular, sondas MikroTik

Sesión larga desde la WSL de Diego. Todo quedó **commiteado en `origin/main`** y **desplegado en el VPS**
(cada despliegue con respaldo previo de la base y purga de Cloudflare). El repo es otra vez la fuente de
verdad: el VPS quedó idéntico al último commit (verificado por md5 antes de cada despliegue).

| Commit | Qué |
|---|---|
| `9d21dd3` | Traer al repo lo editado directo en el VPS desde el 19 (ver punto 4 de arriba) |
| `598f482`, `2017d6d` | App: modo en movimiento **funciona con la pantalla apagada** |
| `c7686c7`, `f6d23f5` | Historial de **batería y red del celular** (`phone_log`) |
| `dd87bfa` | Backend: **sondas MikroTik** en la cola de comandos (modo híbrido) |
| `95ee74a` | Dashboard: controles de sondas |
| `9e844d3`, `7c630b4` | Dashboard: consumo de batería por tendencia real; ignora la falsa carga inalámbrica |

### 1. Agente del router reinstalado (NR)

Hecho (ver punto 1 de la lista de arriba). Binario viejo de respaldo en el equipo:
`/data/routeragent-arm.bak` (para volver atrás: `mv` de vuelta). Sigue pendiente ver `nr_band` real con el
equipo en 5G.

### 2. App móvil (`mobile/`): pantalla apagada + batería

- **Pantalla apagada:** `lib/location_beacon.dart` ya no usa `Timer.periodic`; usa
  `Geolocator.getPositionStream` con `AndroidSettings.foregroundNotificationConfig` (servicio en primer
  plano de geolocator, notificación fija "Notion 5G: modo en movimiento", wake lock parcial). Toma un punto
  cada 15 s y **envía solo si se movió >50 m (y más que la precisión del punto) o si pasaron 2 min**.
  Alcanza con el permiso de ubicación "mientras se usa la app" (el servicio arranca con la app en
  pantalla); no hace falta "Permitir todo el tiempo". Permisos nuevos en el manifiesto:
  `FOREGROUND_SERVICE`, `FOREGROUND_SERVICE_LOCATION`, `WAKE_LOCK`, `POST_NOTIFICATIONS` (este se pide en
  `MainActivity.onCreate`). **Probado:** 14 min con la pantalla apagada (`Dozing`), todos los heartbeats del
  router con ubicación nueva. Falta probar en movimiento y por varias horas. Si se desliza la app fuera de
  "recientes", el servicio se detiene.
- **Batería/red:** canal `notion5g/device` en `MainActivity.kt` (APIs de Android, sin dependencias) +
  `lib/device_status.dart`; cada envío de ubicación lleva `battery_pct`, `battery_status`
  (charging/discharging/full/not_charging), `plugged`, `battery_temp_c`, `net_type`
  (ethernet/wifi/cellular). Backend: tabla nueva **`phone_log`** (append-only),
  `GET /api/v1/devices/{id}/phone_log`. Dashboard, detalle del equipo → "Celular acompañante": estado,
  **consumo por regresión lineal de la última hora** (no por el flag de carga) y horas restantes.
- **Compilar:** con el Flutter de **Windows** (`C:\development\flutter\bin\flutter.bat build apk --release`
  desde `C:\dev\notion5g_mobile`, vía `powershell.exe`). Desde WSL el script `flutter` falla por finales de
  línea CRLF. APK release: ~46 MB (el debug pesa 160 MB y se corta al instalar). **`C:\dev\notion5g_mobile`
  y `mobile/` del repo quedaron idénticos**; si se edita en uno, copiar al otro.
- **Instalada en un Samsung Galaxy S20+ ("S20+ de betty", `SM-G985F`, serial `R58N65V3FKA`)**, no en el
  Motorola. App configurada con `https://notion.h3s-iot.com` + `router-R524260829000001` + la API key del
  dashboard.

**Cómo llegar al celular con adb (lo que funcionó, después de varios intentos que no):**
- USB hacia WSL con `usbipd` **se corta** a mitad de transferencias grandes. El adb de WSL es v28, muy viejo
  para depuración inalámbrica.
- El adb de Windows (`C:\Users\diego\AppData\Local\Android\Sdk\platform-tools\adb.exe`, v36) **no puede
  usar el puerto 5037** (algo lo tiene tomado con la red WSL en modo *mirrored*): arrancarlo con **`-P 5039`**
  (`Start-Process ... -ArgumentList '-P','5039','start-server'`, para que no muera con la consola).
- Por USB el teléfono nunca aceptó la llave del adb de Windows. **Se usó depuración inalámbrica**: quedó
  **vinculado** (`adb -P 5039 pair`) y se conecta con
  `adb -P 5039 connect <ip>:<puerto>` (puerto de `adb -P 5039 mdns services`, cambia al reactivar la
  depuración inalámbrica). Tiene que estar en la misma WiFi que el PC (ese día `192.168.2.92`).

### 3. Hallazgos de campo con el celular (2026-09-24/25)

- **Consumo del S20+ con el modo en movimiento:** ~11–12 %/h por WiFi, **~15 %/h por cable Ethernet**
  (adaptador USB-C **AX88179A** alimentado por el propio teléfono; `plugged: none`).
- **El adaptador se reinicia en bucle** (se desconecta y reconecta cada 2–5 s) cuando está mal asentado o el
  cable hace palanca. Se estabilizó al reconectarlo firme. Se ve con
  `adb shell logcat -d -s EthernetTracker:I`.
- **El teléfono reporta "carga inalámbrica" (`Wireless powered: true`) aunque no esté en el cargador**, aun
  en la mano. El dashboard lo ignora cuando la batería baja ≥1 %/h (`falseWireless` en `app.js`). Mientras
  estuvo de verdad en un cargador inalámbrico, igual bajaba ~11 %/h y el Ethernet no levantaba.
- **Se "murió" con 36 %:** último registro en `phone_log` a las **02:13 hora de Colombia (07:13 UTC) del
  25**, con 36 % y por Ethernet. Después no mandó nada más; el router siguió reportando toda la noche sin
  cortes (sin ubicación desde las 02:24). Hipótesis: batería degradada que se apaga antes de tiempo bajo
  carga, o Samsung cerró la app. **Pendiente:** al prenderlo, leer el motivo del último apagado:
  `adb shell getprop sys.boot.reason; adb shell getprop persist.sys.boot.reason.history`.
- **Conclusión práctica:** con cable no contar con más de ~4 h desde carga completa. Para pruebas más
  largas: adaptador USB-C con Ethernet **y entrada de carga (PD passthrough)** + power bank.

### 4. Sondas MikroTik (modo híbrido) — backend y dashboard listos, falta el script

Idea (decidida con Diego): un **hAP ac2** con un puerto por equipo bajo prueba y una tabla de ruteo por
puerto (`to-notion`, `to-4g`...), alimentado con un mini UPS/power bank (**entra por jack de 10–28 V**, no
por USB). La ruta de salida queda garantizada por el cable. **Al segundo equipo hay que cambiarle la subred
LAN** (los dos traen 192.168.1.1). Se descartó que el celular cambie de WiFi solo (Android 10+ no lo deja
sin tocar la pantalla) y el mAP 2 (puertos de 100 Mbps, WiFi solo en 2.4 GHz).

- **Backend (`internal/store/probes.go`, `internal/api/probes.go`):** tablas `probes` y `probe_targets`;
  `commands` tiene ahora `runner` (`agent`/`probe`, NULL = agent en los viejos), `probe_id`, `routing_table`
  (agregadas con `addColumnIfMissing`; migración probada contra una copia de la base real). **El agente del
  router solo toma comandos de agente**; la sonda consulta `GET /api/v1/commands/next?probe_id=` y recibe
  `routing_table`. `POST /api/v1/probes/{id}/cycle` encola una prueba por equipo en orden (así alternan, sin
  duplicar); un planificador cada 30 s encola el ciclo cuando pasó `interval_s`, **solo si la sonda está en
  línea (consultó en los últimos 3 min) y terminó el ciclo anterior**. Mediciones de sonda:
  `net_route=router`, razón `measured-by-probe`, `source=mikrotik-probe`. Endpoints en `server/README.md`.
- **Dashboard:** sección "Sondas (MikroTik)" en la lista (configurar/editar, ciclo automático
  apagado/5 min…1 h, "Ciclo ahora", estado de cada equipo) y botón **"Prueba vía sonda"** en el detalle del
  equipo (solo si está en una sonda). Probado con Playwright contra un servidor local (15 verificaciones).
- **En producción no hay ninguna sonda configurada** (no se crearon datos de prueba en la base real).
- **Falta el script de RouterOS** (necesita **RouterOS v7**, confirmar la versión del hAP): consultar la
  cola con `?probe_id=`, medir descarga (`/tool fetch` a `/api/v1/speedtest/download` por la tabla) y ping
  (`/ping routing-table=`), cerrar con `POST /commands/{id}/complete`, y heartbeats para los equipos con
  `send_heartbeat`. Validar antes el techo de CPU del hAP con `fetch` por TLS: en 5G puede quedar corto. La
  subida es el punto débil de RouterOS (`fetch` no genera cuerpos grandes): btest o un cliente en otro
  puerto.

### 5. Cómo se desplegó (el procedimiento de §6 sigue valiendo, con estos agregados)

1. Comprobar que el VPS es igual al último commit (md5 de los archivos contra `git show HEAD:server/...`).
   Si difiere, alguien editó allá: traerlo antes.
2. Respaldo: `docker cp notion5g-server:/data/notion5g.db{,-wal,-shm}` a `~/notion5g-backups/<nombre>/`.
   Respaldos de esta sesión: `pre-phonelog-20260925-0233`, `pre-probes-20260925-031525`,
   `pre-probe-ui-20260925-040212`.
3. `rsync` excluyendo `.env`, `.env.example`, `docker-compose.yml`, `cf-tunnel.sh`, `data/`, `.git/`;
   `docker compose up -d --build server`.
4. Purga de Cloudflare de `/`, `/index.html`, `/assets/app.js`, `/assets/style.css` con el token de
   `~/.config/h3s/credentials.env` del VPS, **leído desde un script Python en el propio VPS** (el token nunca
   sale de ahí).

Al cerrar la sesión el VPS no respondía desde la WSL ("No route to host", probablemente cambió la red del
PC). Este addendum quedó en git; si no está en `~/notion-5g-server/docs/`, copiarlo en el próximo
despliegue (el rsync normal lo lleva).

### Pendientes, en orden

1. **Script RouterOS para el hAP ac2** (arriba). Antes: versión de RouterOS y techo de CPU con `fetch`.
2. **Motivo del apagado del S20+ con 36 %** (`sys.boot.reason`) apenas se prenda.
3. **Adaptador Ethernet con PD passthrough** para pruebas de más de ~4 h con cable.
4. Ver `nr_band` real con el router en 5G y reemplazar los nombres candidatos del bloque NR (punto 2 de la
   lista anterior).
5. Probar el modo en movimiento de la app en carretera, varias horas.
6. Siguen los de §7 (exportación CSV, `DEFAULT_API_KEY` expuesta en `app.js`).
