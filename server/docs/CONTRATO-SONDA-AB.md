# Contrato de implementación: sonda A/B con celular por cable (forma A)

Estado: **contrato para construir** (2026-09-29, **revisión 2**: tras una revisión
adversaria contra el código y RouterOS 7.6 se agregaron `batch_id`, `closed_by`,
`updated_at`, `slot` en `commands`, el estado `cancelled` y `POST .../orders/cancel`, la
lista `closed` en `GET .../orders`, finales blandos, `alerts`/`seq` en el estado en vivo,
la limpieza de conntrack y la lectura de rutas en el MikroTik, y se reordenó la máquina de
estados: la orden pasa a `running` solo tras confirmar la ruta y el resultado se guarda
antes de restaurarla). **Revisión 3** (misma fecha, segunda revisión adversaria): dos hilos
en el servicio (`probe-worker` mide y habla con el MikroTik, `probe-control` hace todo el
HTTP al backend) con transiciones por comparación-y-cambio; `requestNetwork` también para
Ethernet; relectura de la regla al final de la prueba (`mikrotik_rule_end`, razón
`probe-route-changed`); vigilante de la regla en el MikroTik (`probe:rule-watchdog`);
WAN por DHCP por defecto; `truncated` y reconciliación de órdenes ausentes en `GET
.../orders`; `order_id` con sufijo `-N` reservado y `409`; `/results` nunca aprende la IP
de la petición; `end_location` ignorado en órdenes del celular; un solo dominio de reloj
en `probe.db`; UUID sin `crypto.randomUUID` en contexto no seguro; alertas con nivel fijo.
Implementa la "forma A" de
[`PLAN-SONDA-AB.md`](PLAN-SONDA-AB.md) (leerlo antes). Lo siguen **cuatro constructores en
paralelo que no hablan entre sí**: backend (Go), dashboard (JS estático), app (Flutter +
Kotlin) y MikroTik (script RouterOS + herramienta de Windows). Si algo no está acá, se
decide lo más simple que no rompa este contrato y se anota en el resumen final para el
líder. **Nadie cambia un nombre, una ruta, una clave JSON ni un valor de enumeración de
este documento.**

## ⚠ Hallazgos reales del MikroTik (2026-09-29, leídos en el equipo; PREVALECEN sobre cualquier otra parte de este contrato)

Diego conectó el hAP y confirmó el cableado. **No se mueven cables**; el diseño se adapta:

| Puerto | Equipo | Qué se vio |
|---|---|---|
| `ether1` | **Router B = Notion 4G** | DHCP client `defconf` *bound*: `192.168.2.100/24`, gateway **`192.168.2.1`** (MAC `00:AB:AB:AB:AB:AB`). Su LAN es **192.168.2.0/24** (su web en 192.168.2.1 es la del Notion). **Coincide por casualidad** con la subred del WiFi de la oficina "Pipito" (mesh, también 192.168.2.1), al que están conectados el PC y el celular: son redes distintas, el MikroTik solo ve la del 4G. Por eso la subred del celular en `ether3` y la LAN de gestión **no pueden ser 192.168.2.0/24** (el celular tiene WiFi en Pipito). Actualización 2026-09-29: Diego cambió la SIM de B; ahora sale con datos por **AS3816 (Movistar / Colombia Telecomunicaciones)**, IP pública 186.102.123.189, ping 36 ms a 1.1.1.1 — ese es el ASN esperado para B. Antes: la SIM (WOM) redirigía a un portal cautivo (`startapp.portalwom.co`), o sea B aún no tiene datos: las pruebas por B deben registrar la caída/portal sin romperse (detectar redirecciones HTTP 302 a un portal y marcarlo). |
| `ether2` | **Router A = Notion 5G** | Hoy dentro del bridge (MAC vista `5E:8A:73:A2:0F:1C`); su LAN probable es 192.168.1.0/24 con gateway 192.168.1.1, **que choca con el bridge del MikroTik (192.168.1.1/24)**. Hay que sacar ether2 del bridge, cambiar la LAN de gestión del MikroTik a una subred libre (p. ej. 192.168.88.1/24) y averiguar la subred/gateway real de A por DHCP client en ether2. |
| `ether3` | libre → celular (USB-Ethernet) | |
| `ether5` | PC (adaptador ASIX, Windows ifIndex **82**, no 22) | Recibe DHCP del MikroTik (hoy 192.168.1.254). Acceso por `fe80::de2c:6eff:fef7:7fe9%82`. |

**YA APLICADO en el MikroTik (2026-09-29 18:06, por el agente principal, a mano con `C:\Users\diego\mikrotik-probe\mkt_setup.py`, exactamente según §3.2):** respaldos `pre-probe-20260929-180510/-180606` (`.rsc` + `.backup`) en `C:\Users\diego\mikrotik-probe\backups\`; tablas `to-A`/`to-B`, ether2/ether3 fuera del bridge, administración `192.168.88.1/24` (PC = `192.168.88.254`, sin gateway), celular `192.168.89.1/24` + DHCP `probe-phone`, DHCP clients WAN con script, rutas `probe:*`, reglas `probe:phone-local` y `phone-probe` (table=main), NAT `probe:masq`, grupo `probe-api` + usuario **`phone-probe`** (contraseña en `C:\Users\diego\mikrotik-probe\phone-probe-password.txt`), vigilante, `api` limitada, DNS/NTP (hora OK), identidad `hap-sonda`. **Verificado:** A (`ether2`) bound `192.168.1.221/24` gw `192.168.1.1` → sale por **AS271773 Partners Telecom (WOM)**, IP `179.19.72.14`, ping 38 ms; B (`ether1`) gw `192.168.2.1` → **AS3816 Movistar**, `186.102.123.189`, ping 57 ms. En 7.6 `/ping` por la API **no acepta `routing-table`** (usar una regla temporal por `dst-address` para probar una tabla). `/ip/dhcp-server/network/unset` no sirve: se vacía con `set gateway=`. **Operador de A confirmado por el propio Notion 5G** (heartbeat del agente en producción 2026-09-29 23:10Z: `operator=WOM`, LTE banda 4, RSRP −104 dBm, uptime 54 min → la SIM WOM se pasó del 4G al 5G). **ASN esperado: A = AS271773 (WOM), B = AS3816 (Movistar)**; son los valores iniciales de `expected_asn` de cada target. El 4G no tiene agente (solo forma A). La herramienta `apply_probe.py` del constructor debe dar **cero cambios** en `--dry-run` sobre este estado.

Consecuencias obligatorias para todos los constructores:
- **Slot A = `ether2` (to-A), slot B = `ether1` (to-B).** Donde el contrato diga ether1=A / ether2=B, está invertido.
- **Los dos WAN NO están en la misma subred.** No usar IPs fijas 192.168.1.201/.202 ni suponer `192.168.1.1`: cada WAN usa DHCP client con `add-default-route=no`, y las rutas `to-A`/`to-B`/`main` se crean con el gateway que entregue el DHCP de cada puerto (script del DHCP client o configuración calculada por `mkt_apply.py` leyendo `/ip/dhcp-client`), manteniendo el `%interfaz`. Si ambos resultaran en la misma subred, sigue valiendo la variante con `%interfaz`.
- La app y el dashboard no deben tener gateways ni puertos fijos: muestran lo que lea el MikroTik.
- Hora del MikroTik: dec/09/2025, sin NTP ni DNS configurados (hay que configurarlos; con NTP usar servidores por IP o DNS por los WAN).
- Estado actual: RouterOS 7.6, 1,6 MB libres de flash, CPU 6 %, `api` 8728 habilitada sin restricción de dirección, solo usuario `admin` (password `admin`), sin rutas/tablas/reglas extra, NAT masquerade 192.168.1.0/24 por ether1.

## 0. Reglas para todos

- **Producción no se toca.** Nada de ssh/rsync a `192.168.40.214`, nada de escribir en
  `https://notion.h3s-iot.com`. Esa URL solo se usa como destino de **descarga** de la
  prueba de velocidad (lectura). Nada de `git commit` ni `git push` (lo hace el líder).
  Nada de borrar datos del usuario (ni `docker compose down -v`, ni bases, ni respaldos).
- **Backend local en Docker**, desde `/home/ubudev/notion-5g/server`:
  `docker compose -p notion5g-local up -d --build server` (puerto 8080, API key en
  `server/.env`). Desde el celular el PC es `http://192.168.40.22:8080`.
- Textos de interfaz, mensajes y documentación: **español de Colombia, neutro** (sin
  voseo en textos nuevos). Comentarios de código: como el código que los rodea.
- Horas: **siempre RFC 3339 en UTC con `Z` y sin fracciones** (`2026-09-29T21:04:05Z`), en
  el backend, en el celular y en el JSON. Valores `float` de segundos (`gps_age_s`,
  `clock_skew_s`) sí llevan decimales. **Al recibir**, el backend es tolerante: parsea con
  `time.RFC3339` (acepta fracciones y desfases como `-05:00`), normaliza a UTC sin
  fracciones antes de guardar y **compara siempre como `time.Time`**, nunca como texto (una
  hora con fracciones o con desfase ordenada como texto da resultados falsos). Una hora
  que no parsea en un campo obligatorio = `400` (o ítem `error` en `/results`).
- **Signo de los desfases (uno solo en todo el contrato):** `kv.clock_offset_ms` del celular
  = hora del servidor − hora local; `clock_skew_s` (en resultados y en el estado) = hora
  local − hora del servidor = `−clock_offset_ms / 1000`. Un celular adelantado 2 s tiene
  `clock_skew_s = 2.0`.
- **Relojes:** el backend decide vencimientos y barridos con **su** hora (y con
  `updated_at`, que es hora del servidor), nunca con horas que mandó el celular. El celular
  compara `execute_at`/`not_after` con `ahora + desfase`, donde `desfase = server_time −
  hora local` medido en la última respuesta del backend (ver §2.3); las horas que el
  celular **escribe** en resultados y estados son las de su propio reloj, sin corregir, y
  el resultado lleva `clock_skew_s` para poder corregir después.
- **Secretos** (`api_key`, `prod_api_key`, `mikrotik_password`) nunca van en el estado en
  vivo, en `events`, en resultados ni en logs. Solo `getConfig` (§2.10) los devuelve.
- Identificadores generados por clientes (`order_id`, `result_id`): UUID v4 en minúsculas
  (`6b0e1c2a-...`). El backend los valida con `^[A-Za-z0-9-]{8,64}$` (mismo criterio que
  `validTestID` en `netclass.go`).
- Autenticación: todo `/api/v1/*` exige `X-API-Key` (igual que hoy). `401` si falta.

### Dueños de archivos (nadie edita fuera de lo suyo)

| Constructor | Archivos |
|---|---|
| Backend | `server/internal/store/**`, `server/internal/api/**` (con tests), sección de endpoints de `server/README.md` |
| Dashboard | `server/internal/web/static/**` |
| App | `mobile/**` (y su copia de compilación `C:\dev\notion5g_mobile`) |
| MikroTik | `scripts/mikrotik/**` (nuevo) y `C:\Users\diego\mikrotik-probe\` en Windows |

### Valores fijos de esta instalación

| Qué | Valor |
|---|---|
| `probe_id` | `hap-oficina` |
| Slot A | router A = Notion 5G, `device_id` `router-R524260829000001`, tabla `to-A`, puerto **`ether2`** (ver hallazgos) |
| Slot B | router B = **Notion 4G** (SIM Movistar, ASN esperado **3816**; la SIM WOM anterior daba portal cautivo), `device_id` `router-R023-4g` (identificador provisional, heredado de la revisión 1; si el Notion 4G tiene agente propio, se cambia desde el dashboard por su `router-<serial>`), tabla `to-B`, puerto **`ether1`** |
| Gateways de A y B | **No son fijos**: los entrega el DHCP de cada router (hoy B = `192.168.2.1`, A probablemente `192.168.1.1`). Nadie los escribe en código; el celular los lee del MikroTik (§3.6) |
| Tabla de respaldo | `main` |
| Regla que mueve el celular | `/routing/rule` con `comment=phone-probe` |
| Subred del celular | `192.168.89.0/24`, MikroTik `192.168.89.1`, puerto `ether3` |
| Subred de administración | `192.168.88.0/24`, MikroTik `192.168.88.1` (bridge: `ether4`, `ether5`, WiFi) |
| Usuario de la API del MikroTik para el celular | `phone-probe`, grupo `probe-api` |
| Backend local | `http://192.168.40.22:8080` (IP del PC en la WiFi de la oficina; **el 2026-09-29 el PC quedó en `192.168.2.60`** en la WiFi "Pipito", igual que el celular: en la app se configura la IP que tenga el PC ese día) |
| `measured_by` del celular | `phone-` + primeros 8 caracteres de `Settings.Secure.ANDROID_ID` en minúsculas |

---

## 1. Backend (Go)

Todo cambio de esquema en tablas que ya existen va con `addColumnIfMissing` (el `CREATE
TABLE IF NOT EXISTS` no agrega columnas: ver HANDOVER §5.1). Tablas nuevas sí pueden ir con
`CREATE TABLE IF NOT EXISTS`. Archivos nuevos sugeridos: `internal/store/phoneprobe.go`,
`internal/api/phoneprobe.go`, `internal/api/phoneprobe_test.go`.

### 1.1 Esquema

**`probes`** (columna nueva):

| Columna | Tipo | Significado |
|---|---|---|
| `runner` | TEXT | `probe` (script RouterOS, lo de hoy; también NULL) o `phone` (celular por cable) |

**`probe_targets`** (columnas nuevas):

| Columna | Tipo | Significado |
|---|---|---|
| `slot` | TEXT | Letra del equipo dentro de la sonda: `A`, `B`, `C`… Única por sonda. Si llega vacía, se conserva la que ya tenía ese `device_id` en la sonda; si no tenía, el backend asigna la primera letra libre por posición (0→`A`, 1→`B`…). |
| `expected_asn` | INTEGER | ASN esperado de la salida de ese router (opcional). Si la clave **no viene** en el PUT, se conserva el valor que ya tenía ese `device_id`; `null` explícito lo borra. Si no hay valor, se usa el ASN de `device_egress` del equipo (si existe). |

Estas dos reglas de "conservar" existen porque `UpsertProbe` hoy borra y vuelve a insertar
todos los `probe_targets`, y el dashboard actual (`probePayload` en `app.js`, usado por el
selector de intervalo de la tarjeta) no manda `slot` ni `expected_asn`: sin ellas, cambiar
el intervalo borraría los ASN esperados. `UpsertProbe` lee los valores viejos **dentro de la
misma transacción**, antes del `DELETE`.

**`commands`** (columnas nuevas; las órdenes del celular viven en esta misma tabla con
`runner='phone'`):

| Columna | Tipo | Significado |
|---|---|---|
| `order_id` | TEXT | UUID de la orden. Índice `CREATE UNIQUE INDEX IF NOT EXISTS idx_cmd_order_id ON commands(order_id)` (los NULL de los comandos viejos no chocan). |
| `batch_id` | TEXT | El `order_id` que mandó el cliente en el `POST .../orders`. En una orden suelta es igual a `order_id`; en una secuencia, todas sus órdenes (`<id>-1`…`<id>-N`) llevan `batch_id = <id>`. Índice `idx_cmd_batch ON commands(batch_id)`. Es la clave de idempotencia de la creación (§1.4). |
| `target` | TEXT | `A`, `B` (o cualquier `slot`) o `next`. Las secuencias se expanden en órdenes `A`/`B`. |
| `slot` | TEXT | Slot resuelto: igual a `target` en `A`/`B`; en `next`, NULL hasta que se cierra con el slot medido. |
| `allow_fallback` | INTEGER | 1 = si el router pedido no tiene datos, medir el otro. |
| `preferred_device`, `fallback_device` | TEXT | Los resuelve el backend al crear (para `A`/`B`). NULL en `next`. |
| `execute_at` | TEXT | No ejecutar antes. Nunca NULL en órdenes nuevas (por defecto = creación). |
| `not_after` | TEXT | Pasada esta hora no se ejecuta: se cierra `expired`. Nunca NULL en órdenes nuevas. |
| `selection_reason` | TEXT | Ver §1.3. |
| `delivered_at`, `started_at` | TEXT | `delivered_at` = hora del **servidor** al recibir el `ack`; `started_at` = hora del **celular** (`at` del estado `running`). |
| `result_id` | TEXT | El que mandó el celular en `running` y, al cerrar, el resultado que la cerró. |
| `closed_by` | TEXT | Quién la llevó a un estado final: `phone` (resultado o estado del celular), `server` (barrido de §1.4) o `dashboard` (cancelación). |
| `updated_at` | TEXT | Hora del **servidor** del último cambio de estado. Los barridos usan esta columna, no las horas del celular. |

Estados de `commands.status` (los viejos siguen valiendo para el agente y el script):
`pending`, `claimed` (solo agente/script), `delivered`, `running`, `done`, `failed`,
`expired`, `interrupted`, `cancelled`.

- **Finales duros:** `done`, `failed`, `cancelled`, y `expired`/`interrupted` con
  `closed_by = phone`. Nada los cambia.
- **Finales blandos:** `expired` e `interrupted` con `closed_by = server` (los puso el
  barrido porque el celular no avisó a tiempo, p. ej. por estar sin backend). Un
  **resultado** que llegue después para esa orden la cierra igual (§1.4, regla 7). Un
  cambio de estado (`/state`) no.

Para una orden `next`, `commands.device_id` (NOT NULL) se guarda con el `probe_id`, y al
cerrarse se reemplaza por el `device_id` que de verdad se midió.

**`measurements`** (columnas nuevas, todas con `addColumnIfMissing`):

| Columna | Tipo | Viene de |
|---|---|---|
| `result_id` | TEXT | celular. Índice `CREATE UNIQUE INDEX IF NOT EXISTS idx_meas_result_id ON measurements(result_id)` |
| `order_id` | TEXT | celular (NULL si no vino de una orden del backend) |
| `probe_id` | TEXT | celular (validado contra la ruta) |
| `measured_by` | TEXT | celular (`phone-xxxxxxxx`) |
| `routing_table` | TEXT | celular (confirmada en el MikroTik) |
| `slot` | TEXT | **servidor**, resuelto desde `routing_table` |
| `selection_reason` | TEXT | celular |
| `test_started_at`, `test_finished_at` | TEXT | celular |
| `test_status` | TEXT | celular: `done` o `failed` |
| `loss_pct` | REAL | celular |
| `gps_ts` | TEXT | celular: hora UTC del fix |
| `gps_age_s` | REAL | celular: `test_started_at − gps_ts`, medido con el reloj monótono |
| `location_status` | TEXT | celular: `fresh`, `stale`, `unavailable` |
| `location_source` | TEXT | celular: `gps`, `fused`, `none` |
| `executed_offline` | INTEGER | celular |
| `net_path` | TEXT | celular: `ethernet` o `wifi` |
| `target_host` | TEXT | celular: host contra el que midió |
| `egress_ip` | TEXT | celular (IP pública vista por Cloudflare; es la IP del router, no de una persona) |

El ASN de salida **no es una columna nueva**: va en `net_asn` (lo resuelve el servidor) y
en `raw.net.egress_asn`. Índice nuevo `idx_meas_probe ON measurements(probe_id, id)`.
Todo lo demás del resultado (`mikrotik_rule`, `mikrotik_rule_end`, `gateway_ip`,
`gateway_iface`, `captive_portal`, `portal_host`, `attempt`, `related_order_id`,
`other_traffic_bytes`, `clock_skew_s`…) vive **solo en `raw`**: sin columnas nuevas, así
el `ALTER TABLE` se limita a las de arriba y el dashboard lo lee de `GET /measurements`.

Estos resultados **no** entran por `insertMeasurement` tal como está (su `INSERT` no conoce
las columnas nuevas y siempre rellena lat/lon con `cachedLocation`). Van por una función
nueva del store (p. ej. `InsertProbeResult(ctx, raw, cols, nc)`) que: escribe las columnas
viejas **y** las nuevas en un solo `INSERT ... ON CONFLICT(result_id) DO NOTHING`; **no**
llama a `cachedLocation`; y decide "insertado o duplicado" por `RowsAffected()` (con `DO
NOTHING`, `LastInsertId()` devuelve un id viejo y no sirve). Si es duplicado, lee el `id`
con `SELECT id FROM measurements WHERE result_id = ?`. Borrar una medición
(`DELETE /measurements/{id}`) libera su `result_id`: si el celular lo reenvía, entra de
nuevo (aceptado).

**Tabla nueva `probe_status`** (último estado en vivo del celular, una fila por sonda):

```sql
CREATE TABLE IF NOT EXISTS probe_status (
	probe_id      TEXT PRIMARY KEY,
	received_at   TEXT NOT NULL,
	measured_by   TEXT,
	phase         TEXT,
	current_table TEXT,
	raw           TEXT NOT NULL
);
```

### 1.2 Configuración de la sonda (compatible con lo de hoy)

`PUT /api/v1/probes/{probe_id}` acepta además `runner` y, por equipo, `slot` y
`expected_asn`. Todo lo viejo sigue igual (sin `runner` = `probe`).

```json
{
  "label": "hAP ac2 oficina",
  "runner": "phone",
  "interval_s": 0,
  "duration_s": 10,
  "targets": [
    {"slot": "A", "device_id": "router-R524260829000001", "routing_table": "to-A", "label": "Notion 5G (Tigo)", "expected_asn": null},
    {"slot": "B", "device_id": "router-R023-4g", "routing_table": "to-B", "label": "Notion 4G (Movistar)", "expected_asn": 3816}
  ]
}
```

Validación: `runner` ∈ {`""`, `probe`, `phone`}; `slot` `^[A-Z]$` y único; `expected_asn`
entero > 0 o null; en una sonda `phone`, `routing_table` **única** dentro de la sonda (la
atribución de §1.4 regla 3 va de tabla a equipo: dos equipos con la misma tabla la harían
ambigua) y distinta de `main` (esa es la de respaldo, no la de un router). En sondas
`probe` no se agrega esta validación (compatibilidad). `400` con `{"error": "..."}` si no
cumple.

**Claves ausentes se conservan:** si el PUT no trae `runner` (o trae `""`), la sonda
conserva el que tenía; una sonda nueva sin `runner` queda `probe`. Así el selector de
intervalo del dashboard de hoy no convierte una sonda de celular en sonda de script. Lo
mismo para `slot` y `expected_asn` por equipo (§1.1). Cambiar `runner` de `phone` a `probe`
con órdenes abiertas: se permite, y las órdenes abiertas se cancelan (`cancelled`,
`closed_by = dashboard`, `error = "cambio-de-runner"`).

`GET /api/v1/probes`, `GET /api/v1/probes/{id}` y `GET /api/v1/probes/{id}/config`
devuelven lo de hoy más `runner`, `slot`, `expected_asn` y, si la sonda tiene estado en
vivo, `phone_status` (mismo objeto que `GET .../status`, §1.8):

```json
{
  "probe_id": "hap-oficina", "label": "hAP ac2 oficina", "runner": "phone",
  "interval_s": 0, "enabled": true, "duration_s": 10,
  "last_seen": "2026-09-29T21:04:30Z", "last_cycle_at": "", "online": true,
  "targets": [
    {"slot": "A", "device_id": "router-R524260829000001", "routing_table": "to-A", "label": "Notion 5G (Tigo)", "send_heartbeat": false, "enabled": true, "expected_asn": 27831},
    {"slot": "B", "device_id": "router-R023-4g", "routing_table": "to-B", "label": "Notion 4G (Movistar)", "send_heartbeat": false, "enabled": true}
  ],
  "phone_status": {"received_at": "2026-09-29T21:04:30Z", "age_s": 4, "online": true, "status": {"...": "ver §1.8"}}
}
```

(El ASN 27831 es solo un ejemplo: se confirma con el primer resultado real, que trae el
ASN observado.)

### 1.3 Órdenes: forma y valores

Una orden, tal como la devuelve cualquier endpoint:

```json
{
  "id": 57,
  "order_id": "3f7d2a10-8f2e-4c1b-9a55-0c8d6e1b2f44",
  "batch_id": "3f7d2a10-8f2e-4c1b-9a55-0c8d6e1b2f44",
  "probe_id": "hap-oficina",
  "runner": "phone",
  "type": "run_speedtest",
  "target": "A",
  "slot": "A",
  "device_id": "router-R524260829000001",
  "routing_table": "to-A",
  "allow_fallback": false,
  "preferred_device": "router-R524260829000001",
  "fallback_device": null,
  "duration_s": 10,
  "execute_at": "2026-09-29T21:05:00Z",
  "not_after": "2026-09-29T21:35:00Z",
  "selection_reason": "requested",
  "requested_by": "dashboard",
  "status": "pending",
  "created_at": "2026-09-29T21:04:40Z",
  "delivered_at": null,
  "started_at": null,
  "completed_at": null,
  "updated_at": "2026-09-29T21:04:40Z",
  "closed_by": null,
  "error": null,
  "result_id": null,
  "measurement_id": null
}
```

Para `target: "next"`: `slot`, `routing_table`, `preferred_device` y `fallback_device` van
`null` y `device_id` es el `probe_id` hasta que se cierre. Al cerrarse, `slot`,
`device_id` y `routing_table` pasan a ser los del equipo medido. Las claves ausentes o sin
valor van **siempre** como `null` (nunca se omiten) en este objeto; en el `Command` de
`GET /api/v1/commands` sí se omiten (§1.5).

`selection_reason` (enumeración cerrada):

| Valor | Quién lo pone |
|---|---|
| `requested` | Dashboard: "Prueba en A/B", "Programar", "Prueba vía sonda" |
| `next` | Dashboard: "Siguiente disponible" |
| `sequence` | Dashboard: "Secuencia de N alternadas" |
| `alternation` | Ciclo: botón "Ciclo ahora" y planificador automático |
| `offline-schedule` | Celular: plan local sin backend |
| `manual-app` | Celular: botones "Medir A/B/…" de la app |
| `fallback:A-sin-datos`, `fallback:B-sin-datos` | Celular: el router pedido no tenía datos y midió el otro |

### 1.4 Endpoints nuevos

Todos con `X-API-Key`. `404 {"error":"sonda no configurada"}` si el `probe_id` no existe.
`400 {"error":"..."}` si el cuerpo no es válido. Tope de cuerpo: 16 KiB salvo resultados
(1 MiB). Solo **crear** órdenes exige `runner: "phone"`; traer órdenes, `ack`, `state`,
`results` y `status` funcionan con cualquier `runner` (así un cambio de `runner` no hace
que el celular pierda resultados ya medidos).

#### `POST /api/v1/probes/{probe_id}/orders` — crear órdenes (dashboard)

Solo para sondas `runner: "phone"` (si no: `400 "la sonda no la ejecuta un celular"`).

```json
{"order_id": "3f7d2a10-8f2e-4c1b-9a55-0c8d6e1b2f44", "target": "A", "allow_fallback": false,
 "duration_s": 10, "execute_at": null, "not_after": null, "requested_by": "dashboard"}
```

```json
{"order_id": "b1c9...", "target": "sequence", "count": 6, "spacing_s": 300, "first": "A",
 "execute_at": "2026-09-29T22:00:00Z", "duration_s": 10, "requested_by": "dashboard"}
```

Reglas:
- La sonda debe estar `enabled` (si no: `400 "la sonda está desactivada"`).
- `target` ∈ slots **activos** de la sonda, `next` o `sequence`. `order_id` opcional (si
  falta, el servidor genera un UUID v4 con `crypto/rand`). Con `sequence`, el `order_id`
  base puede tener como mucho 60 caracteres (para que `<id>-48` quepa en 64). Un
  `order_id` del cliente que termine en `-` + 1 o 2 dígitos (`-[0-9]{1,2}$`) se rechaza con
  `400 "order_id reservado para secuencias"`: si no, una orden suelta `X-1` y una secuencia
  `X` chocarían en el índice único (un UUID nunca termina así: su último grupo tiene 12
  caracteres).
- `execute_at` por defecto = ahora. `not_after` por defecto = `execute_at + 30 min`; debe
  ser mayor que `execute_at`. `duration_s` por defecto = `probes.duration_s` o 10; rango 3–60.
- `sequence`: `count` 2–48, `spacing_s` 60–86400, `first` = slot inicial (por defecto `A`).
  Se crean `count` órdenes que alternan los slots activos en orden (`A, B, A, B…`), con
  `order_id` = `<order_id>-1` … `<order_id>-N`, `execute_at` escalonado cada `spacing_s`,
  `not_after` = `execute_at` de cada una + `spacing_s`, y `selection_reason = sequence`.
- `A`/`B` → `selection_reason = requested`, `preferred_device` = equipo del slot,
  `fallback_device` = el otro equipo solo si `allow_fallback`. `next` → `next`.
- **Idempotencia:** la clave es `batch_id` = el `order_id` del cuerpo. Si ya existe alguna
  orden con `batch_id = <order_id>` **o** con `order_id = <order_id>`, no se crea nada y se
  devuelven todas las de ese `batch_id` (ordenadas por `id`) con `"existing": true`,
  aunque el cuerpo nuevo sea distinto (el cuerpo repetido se ignora, no se compara). La
  verificación y los `INSERT` van en **una sola transacción** (con `SetMaxOpenConns(1)`
  eso los serializa); si igual salta el índice único de `order_id` y la fila que choca
  **es** de ese `batch_id`, se trata como `existing: true`; si es de otro `batch_id` (otra
  sonda u otro lote), `409 {"error":"order_id en uso por otra orden"}` sin crear nada.
  Nunca `500` por esto. La idempotencia es por sonda **y** global: un `order_id` repetido
  en otra sonda también es `409`.

Respuesta `200`:

```json
{"existing": false, "orders": [ { "...orden completa (§1.3)...": "" } ]}
```

#### `GET /api/v1/probes/{probe_id}/orders?horizon=6h` — el celular trae sus órdenes

- `horizon`: duración de Go (`30m`, `6h`) o segundos enteros; por defecto `6h`, máximo `24h`.
- Antes de responder, el servidor **barre** (con su hora): órdenes `phone` de esa sonda en
  `pending`/`delivered` con `not_after < ahora` → `expired` (`error: "vencida-sin-ejecutar"`,
  `completed_at` = ahora, `closed_by = server`); en `running` con `updated_at` más viejo que
  30 min → `interrupted` (`error: "sin-cierre"`, `closed_by = server`). El mismo barrido
  corre también en el planificador cada 30 s (§1.5), así el dashboard ve las vencidas
  aunque el celular no consulte.
- Devuelve las órdenes `phone` de la sonda en `pending`, `delivered` o `running` con
  `execute_at <= ahora + horizon`, ordenadas por `execute_at, id`, máximo 200, y
  `"truncated": true` si había más (el celular entonces **no** aplica la reconciliación
  por ausencia de §2.3).
- Devuelve además `closed`: los `order_id` de esa sonda que el **servidor o el dashboard**
  cerraron (`closed_by` ∈ {`server`, `dashboard`}) con `updated_at` en las últimas 24 h
  **o** con `not_after` todavía en el futuro (una orden de una secuencia larga cancelada
  hace días sigue saliendo mientras podría ejecutarse), máximo 500. El celular los usa
  para no ejecutar una orden cancelada o vencida que ya tenía guardada (§2.3).
- **Cuenta como señal de vida** (`TouchProbe`).
- Incluye la configuración de la sonda para no hacer otra llamada.

```json
{
  "server_time": "2026-09-29T21:04:41Z",
  "probe": { "...igual que GET /probes/{id}, sin phone_status...": "" },
  "orders": [ { "...orden (§1.3)...": "" } ],
  "truncated": false,
  "closed": [ {"order_id": "b1c9...-3", "status": "cancelled"} ]
}
```

Con `?view=history&limit=50` (dashboard) devuelve **todas** las órdenes `phone` de la sonda,
más nuevas primero (por `id`; `limit` por defecto 50, máximo 500), **sin** barrer ni tocar
`last_seen`. Misma forma de respuesta, sin `probe` ni `closed`.

#### `POST /api/v1/probes/{probe_id}/orders/ack` — el celular confirma que las tiene

```json
{"order_ids": ["3f7d2a10-...", "b1c9...-1"]}
```

Pasa a `delivered` (con `delivered_at` y `updated_at` = ahora) solo las que estaban
`pending`. Máximo 200 ids por llamada. Respuesta `200`:

```json
{"acked": ["3f7d2a10-..."], "unknown": [], "unchanged": ["b1c9...-1"]}
```

#### `POST /api/v1/probes/{probe_id}/orders/cancel` — cancelar órdenes (dashboard)

```json
{"order_ids": ["b1c9...-3", "b1c9...-4"]}
```
o `{"batch_id": "b1c9..."}` (todas las de una secuencia) o `{"all_open": true}`.

Pasa a `cancelled` (`closed_by = dashboard`, `completed_at` = ahora, `error = "cancelada"`)
solo las que están en `pending` o `delivered`. Una orden en `running` **no** se cancela (el
celular ya está midiendo; se deja terminar). Respuesta `200`:

```json
{"cancelled": ["b1c9...-3"], "unknown": [], "unchanged": ["b1c9...-4"]}
```

El celular se entera por la lista `closed` de `GET .../orders` (arriba) y no las ejecuta.
Si el celular estaba sin backend y ya la ejecutó, su resultado se guarda igual (con
`order_linked: false` en la respuesta, porque la orden estaba final dura).

#### `POST /api/v1/probes/{probe_id}/orders/{order_id}/state` — cambios de estado sin resultado

```json
{"status": "running", "at": "2026-09-29T21:05:02Z", "result_id": "6b0e1c2a-...", "error": null}
```

- `status` ∈ {`delivered`, `running`, `interrupted`, `expired`}. `done`/`failed` se cierran
  **solo** subiendo el resultado (`400 "done/failed se cierran con /results"`).
- Transiciones válidas: `pending→delivered|running|interrupted|expired`,
  `delivered→running|interrupted|expired`, `running→interrupted`. `running` guarda
  `started_at = at` y `result_id`. Toda transición aplicada pone `updated_at` = ahora del
  servidor y, si es final, `completed_at` = ahora y `closed_by = phone`. Cualquier otra
  (repetir el mismo estado, retroceder, o tocar una orden ya final, dura o blanda) **no
  cambia nada** y responde `200` con `"ignored": true` y el `status` actual.
- `404 {"error":"orden desconocida"}` si no existe en esa sonda. El celular, ante `200`
  (aplicado o `ignored`) o `404`, borra ese ítem de su `outbox`; ante error de red o `5xx`,
  reintenta; ante otro `4xx`, lo borra y anota un evento `error`.

```json
{"order_id": "3f7d2a10-...", "status": "running", "ignored": false}
```

#### `POST /api/v1/probes/{probe_id}/results` — subida idempotente de resultados

Cuerpo: `{"results": [ ... ]}` (también se acepta el arreglo solo). Máximo 50 por lote
(más: `400`). Un resultado (§2.3 y §2.6 definen cómo lo arma el celular):

```json
{
  "result_id": "6b0e1c2a-4d3f-4e8a-9b1c-2a3d4e5f6a7b",
  "order_id": "3f7d2a10-8f2e-4c1b-9a55-0c8d6e1b2f44",
  "local_order_id": null,
  "related_order_id": null,
  "attempt": 1,
  "probe_id": "hap-oficina",
  "device_id": "router-R524260829000001",
  "slot": "A",
  "measured_by": "phone-3fa9c2d1",
  "source": "phone-probe",
  "tag": "probe-ab",
  "ts": "2026-09-29T21:05:20Z",
  "test_started_at": "2026-09-29T21:05:20Z",
  "test_finished_at": "2026-09-29T21:05:47Z",
  "test_status": "done",
  "error": null,
  "routing_table": "to-A",
  "mikrotik_confirmed": true,
  "mikrotik_rule": {"table": "to-A", "action": "lookup-only-in-table", "disabled": false, "inactive": false},
  "mikrotik_route_active": true,
  "mikrotik_rule_end": {"table": "to-A", "action": "lookup-only-in-table", "disabled": false, "inactive": false},
  "gateway_ip": "192.168.1.1",
  "gateway_iface": "ether2",
  "captive_portal": false,
  "portal_host": null,
  "net_path": "ethernet",
  "selection_reason": "requested",
  "executed_offline": false,
  "clock_skew_s": 0.4,
  "target_kind": "cloudflare",
  "target_host": "speed.cloudflare.com",
  "egress_ip": "181.49.10.20",
  "egress_ip_end": "181.49.10.20",
  "egress_colo": "BOG",
  "egress_asn_client": 27831,
  "egress_as_org_client": "Colombia Movil",
  "gw_reachable": true,
  "dns_ok": true,
  "internet_ok": true,
  "ping_ms": 38.2, "jitter_ms": 4.1, "loss_pct": 0, "ping_method": "tcp-connect", "ping_samples": 10,
  "down_mbps": 85.3, "down_bytes": 106625000, "down_s": 10.0,
  "up_mbps": 20.1, "up_bytes": 25125000, "up_s": 10.0,
  "concurrent_tests": null,
  "other_traffic_bytes": 18230,
  "duration_s": 10,
  "lat": 4.65123, "lon": -74.05321, "gps_accuracy_m": 6.0, "gps_source": "android-gps",
  "gps_ts": "2026-09-29T21:05:17Z", "gps_age_s": 3.2,
  "location_status": "fresh", "location_source": "gps",
  "gps_satellites_used": 9, "gps_speed_mps": 0.1,
  "battery_pct": 76,
  "app_version": "1.1.0+2"
}
```

Campos que no aplican van `null`. En particular: con `net_path = wifi`, `routing_table`,
`slot`, `mikrotik_rule`, `mikrotik_rule_end`, `mikrotik_route_active`, `gateway_ip` y
`gateway_iface` van `null` y `mikrotik_confirmed = false`. `mikrotik_rule_end` = la regla
leída de nuevo **al terminar** la prueba, antes de restaurarla (§2.3 paso h); `null` si esa
lectura falló (no invalida la prueba). `gateway_ip`/`gateway_iface` = lo que el MikroTik
dijo de la ruta de esa tabla (`192.168.1.1%ether2` → `192.168.1.1` y `ether2`).
`captive_portal` = `true` si el chequeo de portal (§2.3 paso d) vio una redirección o una
respuesta que no es la esperada; `portal_host` = host del `Location` de la redirección
(p. ej. `startapp.portalwom.co`), `null` si no hubo. `order_id` va `null` en órdenes locales (entonces
`local_order_id` = `local-<uuid>`) y en el intento fallido previo a un respaldo (entonces
`related_order_id` = la orden, `attempt = 1`, y el intento que cierra la orden lleva
`attempt = 2`). `clock_skew_s` = hora del celular − hora del servidor según la última
respuesta del backend antes de la prueba (`null` si nunca hubo). `other_traffic_bytes` =
bytes que movieron **otras apps** durante la prueba (§2.6), `null` si no se pudo medir.

Reglas del servidor, por resultado:

1. Valida `result_id` (regex), `probe_id` = el de la ruta, `test_status` ∈ {`done`,`failed`},
   `net_path` ∈ {`ethernet`,`wifi`}, `test_started_at` en RFC 3339. Si falla: ítem `error`
   con `retryable: false`. `ts` se reemplaza por `test_started_at`.
2. Borra del payload las claves reservadas (`stripReserved`: `net_route`, `net_asn`, `net`,
   `client_ip`) y además `egress_asn`, `slot`, `device_id_client` (los pone el servidor).
3. **Atribución al router (la decide el servidor, no el celular):**
   - `net_path = ethernet`, `mikrotik_confirmed = true` y la regla **no cambió** durante la
     prueba (`mikrotik_rule_end` es `null` o su `table` = `routing_table`): se busca en
     `probe_targets` el equipo cuya `routing_table` = la del resultado (esté activo o no:
     desactivar un equipo después de medir no borra la atribución). Ese es el `device_id` y
     el `slot` que se guardan. Si `mikrotik_rule_end.table` ≠ `routing_table`, se trata
     como "Ethernet sin confirmar" (siguiente viñeta). Si el `device_id` que mandó el celular es otro, se guarda el del servidor y
     el del celular queda en `raw.device_id_client`. Si la tabla no es de ningún equipo:
     `device_id` = `measured_by`, `slot` NULL.
   - `net_path = wifi` (o Ethernet sin confirmar): `device_id` = `measured_by`, `slot` NULL.
     **No se atribuye a A ni a B.** (El `device_id` `phone-xxxxxxxx` aparecerá como un
     equipo más en `GET /devices`; es esperado.)
   - `device_id` y `slot` resueltos se escriben **también en `raw`** (con
     `withServerFields`), porque el dashboard lee `raw` (`GET /measurements`).
   - Con `net_path = wifi`, `egress_ip`/`egress_ip_end` son la IP pública de la oficina,
     no la de un router: se guardan recortadas a `privacyPrefixStr` (como las IP de
     celulares en el resto del backend). Con Ethernet se guardan completas.
4. **Clasificación de red** (va a `net_route`/`net_confidence`/`net_asn` por
   `store.NetClassification`, nunca desde el JSON). Si hay `egress_ip`, el servidor resuelve
   su ASN con `s.asn.Lookup` (Team Cymru, ya cacheado), una vez por IP única del lote, con
   3 s por IP y **8 s en total por lote** (lo que no alcance queda `not-resolved`; no se
   reclasifica después). `egress_asn_client` (lo que dijo Cloudflare) solo va a `raw`,
   nunca decide. ASN esperado = `probe_targets.expected_asn`, o si falta, el ASN de
   `device_egress` del equipo. Primera regla que aplique:

   | Condición | `net_route` | `confidence` | `reason` |
   |---|---|---|---|
   | `net_path = wifi` | `other-network` | `high` | `phone-wifi` |
   | Ethernet sin `mikrotik_confirmed` | `unknown` | `low` | `probe-route-unconfirmed` |
   | Ethernet con `mikrotik_rule_end.table` ≠ `routing_table` (alguien o el vigilante del MikroTik movió la regla a mitad de la prueba) | `unknown` | `low` | `probe-route-changed` |
   | `egress_ip` ≠ `egress_ip_end` (ambas presentes) | `unknown` | `low` | `network-changed-mid-test` |
   | ASN resuelto = esperado, pero ese ASN también es el esperado de otro slot de la sonda (dos routers del mismo operador: el ASN no los distingue) | `router` | `medium` | `probe-asn-ambiguous` |
   | ASN resuelto y esperado conocido, iguales | `router` | `high` | `probe-asn-match` |
   | ASN resuelto = ASN esperado de **otro** slot de la sonda | `other-network` | `high` | `probe-asn-other-router` |
   | ASN resuelto y esperado conocido, distintos | `unknown` | `low` | `probe-asn-mismatch` |
   | Resto (sin IP, sin ASN, sin esperado, o `test_status = failed`) | `router` | `medium` | `probe-table-confirmed` |

   `net_asn` = ASN resuelto. En `raw` se escribe (con `withServerFields`) `net_route` y
   `net = {"method":"phone-probe","route","confidence","reason","egress_asn",
   "egress_asn_name","expected_asn","asn_status":"ok|not-resolved|no-ip"}`.
   La IP de la petición HTTP **no se usa** (llega por el WiFi de la oficina). Por eso este
   camino **nunca** llama a `noteDeviceMeasurement`, `SetDeviceEgress`,
   `enqueueNetEnrich` ni al `ledger` de `test_id`: `noteDeviceMeasurement` guardaría la IP
   pública de la oficina como IP de salida del router A/B en `device_egress` y
   envenenaría la clasificación de la forma B. Tampoco se actualiza `device_egress` con
   el `egress_ip` del resultado (esa tabla es de la forma B).
5. **Ubicación:** este camino **no** rellena lat/lon desde `device_locations`
   (`cachedLocation`) ni desde la orden. Sin fix, la fila queda sin posición.
6. Inserta con `INSERT ... ON CONFLICT(result_id) DO NOTHING` (ver "Estos resultados no
   entran por `insertMeasurement`" en §1.1). Si no insertó, es `duplicate` y se devuelve el
   `measurement_id` existente; **no** se reevalúa la orden (el primer envío ya lo hizo).
7. Si trae `order_id` de esta sonda y se insertó: cierra la orden (`status` =
   `test_status`, `completed_at` y `updated_at` = ahora del servidor, `closed_by = phone`,
   `measurement_id`, `result_id`, `error`, y `slot`/`device_id`/`routing_table` del equipo
   medido), **solo** si la orden está abierta (`pending`/`delivered`/`running`) o en final
   **blando** (`closed_by = server`) con `test_started_at <= not_after` (tolerancia de
   `clock_skew_s` si viene: se compara `test_started_at − clock_skew_s`). Si estaba en
   final duro, no se toca y la respuesta dice `order_linked: false` con el `order_status`
   actual; la medición **sí** conserva `order_id` en su columna (la orden existe y es de
   esta sonda: sirve para ver qué se midió tras una cancelación). `order_id` desconocido o
   de otra sonda: se guarda la medición igual, sin `order_id` en la columna (queda en
   `raw.order_id_client`) y `order_linked: false`. Si el resultado no quedó atribuido a un
   equipo (WiFi o ruta sin confirmar), la orden se cierra igual pero **conserva** su
   `slot`/`device_id`/`routing_table` pedidos (en una `next`, `slot` y `routing_table`
   siguen `NULL` y `device_id` = `probe_id`); el dashboard ve en la medición que no se
   atribuyó. Insertar la medición y cerrar la orden van en **la misma transacción**.
8. `related_order_id` (intento fallido previo a un respaldo) no cierra nada: solo queda en
   `raw`.

Respuesta `200` (siempre que el cuerpo sea JSON válido; el estado va por ítem):

```json
{"results": [
  {"result_id": "6b0e1c2a-...", "status": "inserted", "measurement_id": 812,
   "order_id": "3f7d2a10-...", "order_linked": true, "order_status": "done",
   "device_id": "router-R524260829000001", "slot": "A",
   "net_route": "router", "confidence": "high", "reason": "probe-asn-match",
   "egress_asn": 27831, "egress_asn_name": "Colombia Movil"},
  {"result_id": "0d4c...", "status": "duplicate", "measurement_id": 790},
  {"result_id": "zz", "status": "error", "error": "result_id inválido", "retryable": false}
]}
```

`status` por ítem: `inserted`, `duplicate` o `error`. Un error de base de datos es
`retryable: true`. El celular marca como subidos (`synced`) los `inserted` y `duplicate`,
como `rejected` los `error` con `retryable: false`, y deja `pending` el resto. Un `400` de
todo el lote (JSON inválido, más de 50) no marca nada: el celular parte el lote a la mitad y
reintenta; un solo ítem que siga dando `400` queda `rejected`.

#### `POST /api/v1/probes/{probe_id}/status` — estado en vivo del celular

El celular lo manda cada 15 s y en cada cambio de fase (como mucho uno cada 2 s). Cuerpo =
el objeto de estado de §2.5 (máximo 16 KiB). El servidor hace upsert en `probe_status`
(`raw` = cuerpo tal cual, más `phase`, `current_table` = `mikrotik.current_table`,
`measured_by`) y `TouchProbe`. Si llega un cuerpo con `seq` menor que el guardado **y** el
mismo `service_started_at` **y** el mismo `measured_by`, se descarta (llegó tarde, fuera de
orden) y se responde igual `ok`. `seq` y `service_started_at` se comparan **solo** entre
cuerpos del mismo servicio (nunca se ordenan horas del celular contra horas del servidor).
Hay **un solo celular por sonda**: el backend no reparte órdenes entre celulares; si llega
un `measured_by` distinto del guardado, el nuevo reemplaza al anterior y el servidor deja
una línea en su log ("sonda hap-oficina: cambió el celular de X a Y"), y el dashboard
muestra el `measured_by` en el panel para que se note. Respuesta:

```json
{"status": "ok", "server_time": "2026-09-29T21:05:03Z"}
```

#### `GET /api/v1/probes/{probe_id}/status` — lo que lee el dashboard

```json
{"probe_id": "hap-oficina", "received_at": "2026-09-29T21:05:03Z", "age_s": 4, "online": true,
 "status": { "...el cuerpo del último POST...": "" }}
```

`online` = `age_s <= 60`. Sin estado todavía: `200` con `"status": null`.

### 1.5 Endpoints existentes que cambian

- `GET /api/v1/measurements`: nuevo filtro `probe_id` (y el `ListFilter` gana `ProbeID`).
- `POST /api/v1/commands` con `"runner":"probe"`: si la sonda resuelta es `runner: "phone"`,
  **se crea una orden de celular** (UUID del servidor, `target` = slot del equipo,
  `selection_reason = requested`, `not_after` = +30 min) en vez de un comando `probe`, y la
  respuesta agrega `order_id`: `{"id": 57, "status": "pending", "order_id": "..."}`. Así el
  botón "Prueba vía sonda" del detalle del equipo sigue funcionando. La ubicación del
  navegador que traiga el comando **no** se fusiona en el resultado del celular (el celular
  toma su propio fix). `batch_id` = ese mismo `order_id`; `requested_by` = el del comando.
- `POST /api/v1/probes/{id}/cycle` y el planificador (`EnqueueDueProbeCycles`): si la sonda
  es `runner: "phone"`, encolan órdenes de celular (una por equipo activo, en orden de
  `slot`, UUID del servidor, `execute_at` = ahora, `not_after` = ahora + `max(interval_s,
  1800)` s, `selection_reason = alternation`). Se consideran "abiertas" las órdenes en
  `pending`, `delivered` o `running` (esta última si su `updated_at` —hora del servidor—
  tiene menos de 30 min; nunca `started_at`, que es hora del celular).
  La respuesta de `/cycle` agrega `order_ids` además de `command_ids` (en una sonda de
  celular, `command_ids` trae los mismos `id` numéricos de esas órdenes). El planificador
  (`probeSchedulerLoop`, cada 30 s) además corre el barrido de vencidas/sin cierre de §1.4
  para todas las sondas de celular.
- `GET /api/v1/commands`: el `Command` JSON agrega (con `omitempty`) `order_id`,
  `batch_id`, `target`, `slot`, `execute_at`, `not_after`, `selection_reason`,
  `delivered_at`, `started_at`, `result_id`, `closed_by`, `updated_at`. Son **solo de
  salida**: `CreateCommand` hace `json.Unmarshal` del cuerpo sobre el mismo struct, así que
  debe ignorarlos explícitamente (limpiarlos después del `Unmarshal`) para que un `POST
  /commands` no pueda fijar `order_id` ni estados. El agente del router ignora campos
  desconocidos: sigue funcionando.
- `CompleteCommand` (`POST /commands/{id}/complete`) **pasa a ser idempotente**: si el
  comando ya está en `done`/`failed`/`expired`/`interrupted`/`cancelled`, no inserta otra
  medición y responde `200 {"status":"ok","ignored":true}`. Sobre una orden
  `runner='phone'` responde `400 "las órdenes del celular se cierran con /probes/{id}/results"`
  (y no inserta nada).
- `POST /commands/{id}/end_location` sobre una orden `runner='phone'`: responde `200
  {"status":"ok","ignored":true}` y **no** toca la medición. Hoy el detalle del equipo lo
  llama al ver `done` tras "Prueba vía sonda" y escribiría la ubicación del navegador
  como `lat_end/lon_end` de la medición del celular (que tiene su propio fix).
- `ClaimNextCommand` (agente) y `ClaimNextProbeCommand` (script) **no** cambian: nunca
  toman órdenes `runner='phone'` (sus `WHERE` ya filtran por `runner`; hay test).
- Mux (Go 1.22+): las rutas nuevas no chocan con las existentes (`/orders`,
  `/orders/ack`, `/orders/cancel`, `/orders/{order_id}/state`, `/results`, `/status` bajo
  `/api/v1/probes/{probe_id}/`). No crear `GET /api/v1/probes/{probe_id}/orders/{order_id}`
  (no hace falta).

**Fuera de alcance en esta pasada:** retirar `maxLocationAge` y cruzar la forma B con el
historial de fixes (§6 del plan). No se toca. Tampoco se cambia `ListDeviceSummaries`: un
resultado del celular atribuido a un router cuenta como "visto" para ese router con su
`received_at` (hora de subida), aunque la prueba haya fallado o se haya hecho horas antes
sin Internet. El dashboard debe mostrar el estado del router con su lista de mediciones, no
solo con "en línea".

### 1.6 Tests mínimos del backend (`go test ./...` en verde)

1. Crear orden dos veces con el mismo `order_id` → una sola fila, segunda respuesta `existing: true`. Lo mismo con una secuencia repetida (mismo `batch_id`) → las mismas N órdenes.
2. Secuencia de 4 → órdenes `-1..-4`, alternando A/B, `execute_at` escalonado, todas con el mismo `batch_id`.
3. `GET orders?horizon=1h` no devuelve las que empiezan en 2 h; vence las que pasaron `not_after` (`closed_by = server`) y las lista en `closed`.
4. `ack` pasa `pending→delivered`; `state running`; `state` repetido o sobre una final → `ignored`.
5. Subir el mismo resultado dos veces → `inserted` y luego `duplicate` con el mismo `measurement_id`; la orden queda `done` una sola vez.
6. Resultado sobre orden `expired` por el servidor con `test_started_at <= not_after` → la cierra `done`; sobre una `expired`/`interrupted` puesta por el celular, o `cancelled` → no la toca (`order_linked: false`).
7. Clasificación: wifi → `other-network/high/phone-wifi`; Ethernet confirmado sin ASN → `router/medium`; con ASN esperado igual → `router/high` (inyectar el resolvedor o precargar su caché, sin red); ASN de otro slot → `other-network`; mismo ASN esperado en A y B → `probe-asn-ambiguous`.
8. Resultado sin fix no toma la ubicación de `device_locations` (precargar una ubicación fresca para ese `device_id` en el test).
9. `CompleteCommand` dos veces → una sola medición; sobre una orden `phone` → `400`.
10. Agente (`?device_id=`) y script (`?probe_id=`) no toman órdenes `phone`.
11. Migración sobre una **copia** de `server/data/live-test.db` en el scratchpad (nunca el original): arranca sin "no such column".
12. `PUT` sin `runner` ni `expected_asn` sobre una sonda `phone` con ASN esperados → sigue `phone` y conserva los ASN (lo que hace hoy el selector de intervalo del dashboard).
13. `cancel` pasa `pending`/`delivered` a `cancelled` y no toca `running`.
14. `POST /commands` con `"runner":"probe"` sobre un equipo de una sonda `phone` → crea orden de celular (con `order_id` en la respuesta) y no un comando `probe`.
15. `order_id` `X-1` en una orden suelta → `400`; mismo `order_id` en otra sonda → `409`.
16. `GET orders` con más de 200 abiertas → `truncated: true`; una orden de secuencia cancelada hace más de 24 h con `not_after` futuro sigue en `closed`.
17. Resultado Ethernet confirmado con `mikrotik_rule_end.table` distinta → `unknown/low/probe-route-changed`, `device_id` = `measured_by`, `slot` NULL.
18. Subir resultados **no** crea ni cambia filas de `device_egress` (precargar una fila para el equipo A y comprobar que sigue igual).
19. `POST /commands/{id}/end_location` sobre una orden `phone` → `ignored: true` y la medición sin `lat_end`.
20. `PUT` de una sonda `phone` con dos equipos en la misma `routing_table` (o en `main`) → `400`.

---

## 2. App (Flutter + Kotlin)

### 2.1 Arquitectura

Todo lo que mide vive en **un servicio nativo nuevo, `ProbeService`**, separado de
`BeaconService` (que no se toca: sigue igual y sigue mandando a producción). Dart solo
configura, prende/apaga y muestra.

Archivos Kotlin nuevos (paquete `com.h3s.notion5g.notion5g_field`):

| Archivo | Qué hace |
|---|---|
| `ProbeService.kt` | Servicio en primer plano `foregroundServiceType="location"`, notificación propia (canal `probe`, id 2; el texto muestra la fase, p. ej. "Sonda: midiendo A · descarga 42 MB", actualizado como mucho cada 2 s), `START_STICKY`, wake lock parcial y `WifiLock` (`WIFI_MODE_FULL_HIGH_PERF`; en API 34+ `WIFI_MODE_FULL_LOW_LATENCY`) mientras está activo. **Dos hilos** (§2.3): `probe-worker` (`HandlerThread`) corre la máquina de estados, las pruebas, el plan local, los vencimientos locales y **todas** las sesiones con el MikroTik (también "Probar MikroTik", "Volver a respaldo" y la lectura en reposo, que se le encolan); es el único que mide, así que **nunca hay dos pruebas a la vez**. `probe-control` (`HandlerThread`) hace **todo** el HTTP al backend (traer órdenes, `ack`, `outbox`, resultados, `POST status`, "Sincronizar ahora") y nunca bloquea al worker: una descarga de 60 s no retrasa el estado en vivo ni un `running`. Los dos comparten `ProbeDb`; toda transición de una orden es una **comparación-y-cambio** (`UPDATE orders SET status=? ... WHERE order_id=? AND status IN (...)` y se mira `rowsAffected`): el worker solo pasa a `running` una orden que siga `pending`/`delivered`, y el control solo cancela o vence (por `closed`) una que siga `pending`/`delivered`; si el `UPDATE` no afecta filas, el otro hilo ganó y se respeta. Incluye `ProbeWatchdog` (JobService, id **43**, cada 15 min; `BeaconWatchdog` usa el 42) con el mismo patrón que `BeaconWatchdog`. `startForeground` se llama primero, como en `BeaconService`, también si luego se aborta. Sin permiso de ubicación el servicio **igual mide** (con `location_status = unavailable`) pero Android no deja arrancar un servicio de tipo `location` sin `ACCESS_FINE_LOCATION`: entonces `start` devuelve `start_failed` "Falta permiso de ubicación". |
| `ProbePrefs.kt` | SharedPreferences `"probe"` (claves de §2.7). |
| `ProbeDb.kt` | `SQLiteOpenHelper` sobre `probe.db` (esquema §2.2). Sin plugins de Flutter. **Una sola instancia por proceso** (singleton `ProbeDb.get(context)`), con `setWriteAheadLoggingEnabled(true)`; la usan el servicio y los manejadores del `MethodChannel` (mismo proceso: el servicio **no** lleva `android:process`). Nada de SQLite en el hilo principal: el `MethodChannel` hace las consultas en un `Executor` de fondo y responde con `result.success` en el hilo principal. Cada paso que cambia una orden y guarda un resultado va en **una transacción**. |
| `NetPaths.kt` | Sigue las redes con `ConnectivityManager`: `requestNetwork` para Ethernet (`TRANSPORT_ETHERNET`) **y** para WiFi (`TRANSPORT_WIFI`), las dos con `NET_CAPABILITY_INTERNET` y **sin** exigir `VALIDATED` (sin `timeoutMs`: la petición queda esperando hasta que aparezca la red). Una petición explícita —no un simple `registerNetworkCallback`, que solo escucha— es lo que evita que Android dé de baja una red que no es la por defecto: el WiFi cuando Ethernet está validada, y **Ethernet cuando queda sin validar** (forzada a un router sin datos o con el portal cautivo de B) y el WiFi pasa a ser la por defecto. Requiere `CHANGE_NETWORK_STATE`. Expone `ethernet: Network?`, `wifi: Network?`, IP e interfaz de cada una (`LinkProperties`). Se desregistra al detener el servicio. |
| `RouterOsApi.kt` | Cliente de la API clásica (TCP 8728), §3.6. |
| `SpeedTester.kt` | Ping TCP, descarga, subida, consulta de IP de salida, chequeo de puerta de enlace y DNS, todo atado a un `Network`. |
| `GpsFix.kt` | Fix fresco por prueba (§2.4). |
| `ControlPlane.kt` | HTTP al backend eligiendo camino (§2.8). |
| `ProbeBus.kt` | Objeto estático del proceso: guarda el último estado (§2.5) y lo publica al `EventChannel` en el hilo principal si hay un oyente (si la actividad no existe, solo lo guarda). Lo alimentan el servicio y, con el servicio detenido, el `status` del canal. |

Cambios en archivos existentes: `MainActivity.kt` (canales nuevos), `BeaconRestart.kt`
(`BootReceiver` además programa `ProbeWatchdog` si `enabled` y llama
`ProbeService.startIfEnabled`), `AndroidManifest.xml` (servicio `ProbeService` con
`foregroundServiceType="location"`, job `ProbeWatchdog` con `BIND_JOB_SERVICE`, permisos
`CHANGE_NETWORK_STATE`, `ACCESS_WIFI_STATE` y `REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`,
`android:usesCleartextTraffic="true"` porque el backend local es `http://`; no afecta a
`BeaconService`, que usa `https`), `pubspec.yaml` (versión `1.1.0+2`), `home_screen.dart`
(botón en la AppBar, icono `Icons.compare_arrows`, que abre "Sonda A/B"; el icono lleva un
punto de color según el último `status` —consultado cada 10 s mientras el inicio está a la
vista—: verde = midiendo, azul = en espera, ámbar = hay `alerts` `warn`, rojo = hay
`alerts` `error`, sin punto = detenida. Así se ve desde el inicio si la sonda está viva sin
abrir la pantalla).

**Arranque en segundo plano** (watchdog, reinicio): Android 12+ solo deja arrancar un
servicio en primer plano desde segundo plano en casos exentos; `BOOT_COMPLETED` y
`MY_PACKAGE_REPLACED` lo son, el `JobService` no, salvo que la app esté **fuera de la
optimización de batería**. La pantalla muestra si lo está
(`PowerManager.isIgnoringBatteryOptimizations`) y ofrece el botón que abre
`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`. Un fallo al relanzar se registra en
`events` y en `alerts` (no revienta), igual que `BeaconService.startIfEnabled`. Tomar la
ubicación con el servicio relanzado en segundo plano exige "Permitir todo el tiempo"
(`ACCESS_BACKGROUND_LOCATION`), igual que el modo en movimiento.

**`BeaconService` no se toca** y sigue usando la red por defecto de Android. Ojo: con el
cable puesto, la red por defecto suele ser **Ethernet**, así que el modo en movimiento (y
las demás apps del teléfono) salen por el MikroTik y por el router que esté forzado en ese
momento. Es aceptado; la prueba registra `other_traffic_bytes` para detectar si otro
tráfico ensució la medición (§2.6).

Dart nuevo: `lib/probe_channel.dart`, `lib/screens/probe_screen.dart`,
`lib/screens/probe_settings_screen.dart`. **No se agregan dependencias** a `pubspec.yaml`.

Todas las conexiones HTTP de la prueba llevan `instanceFollowRedirects = false` (un 3xx es
un dato —portal cautivo, como el que daba la SIM WOM de B—, no algo que seguir: si se siguiera,
la "descarga" mediría la página del portal). Todas las atadas a Ethernet llevan
`Connection: close` y se abren **después** de confirmar la regla: al cambiar la regla del MikroTik, una conexión que ya existía sigue
con el NAT del router anterior (el conntrack del MikroTik la recuerda). Por lo mismo, tras
cambiar la regla el celular borra en el MikroTik las conexiones rastreadas de su IP (§3.6,
paso "limpiar conntrack") y espera 1 s antes de medir.

### 2.2 SQLite del celular (`probe.db`, versión 1)

```sql
CREATE TABLE orders (
  order_id         TEXT PRIMARY KEY,          -- del backend, o "local-<uuid>"
  origin           TEXT NOT NULL,             -- 'server' | 'local'
  server_id        INTEGER,                   -- commands.id, para ordenar
  target           TEXT NOT NULL,             -- 'A' | 'B' | ... | 'next' | 'wifi'
  slot             TEXT,                      -- el que se midió al final
  device_id        TEXT,
  routing_table    TEXT,
  allow_fallback   INTEGER NOT NULL DEFAULT 0,
  duration_s       INTEGER NOT NULL DEFAULT 10,
  execute_at_ms    INTEGER NOT NULL,          -- reloj corregido (ver abajo)
  not_after_ms     INTEGER,                   -- reloj corregido
  selection_reason TEXT NOT NULL,
  requested_by     TEXT,
  status           TEXT NOT NULL,             -- pending|delivered|running|done|failed|expired|interrupted|cancelled
  acked            INTEGER NOT NULL DEFAULT 0,
  result_id        TEXT,                      -- se genera al pasar a running (tras confirmar la ruta)
  started_ms       INTEGER,                   -- reloj de pared al pasar a running
  error            TEXT,                      -- motivo final, o el de la última falla antes de running (sin-ethernet, mikrotik-no-confirma...)
  created_ms       INTEGER NOT NULL,
  updated_ms       INTEGER NOT NULL
);
CREATE INDEX idx_orders_due ON orders(status, execute_at_ms);

CREATE TABLE results (
  result_id         TEXT PRIMARY KEY,
  order_id          TEXT,                      -- el de la orden local (server o local-...), también en intentos fallidos
  attempt           INTEGER NOT NULL DEFAULT 1,
  created_ms        INTEGER NOT NULL,
  payload           TEXT NOT NULL,             -- JSON de §1.4 tal cual se sube
  sync_status       TEXT NOT NULL DEFAULT 'pending',  -- pending | synced | rejected
  sync_attempts     INTEGER NOT NULL DEFAULT 0,
  last_sync_error   TEXT,
  synced_ms         INTEGER,
  measurement_id    INTEGER,
  server_net_route  TEXT,
  server_confidence TEXT,
  server_reason     TEXT
);
CREATE INDEX idx_results_sync ON results(sync_status, created_ms);

CREATE TABLE outbox (                          -- cambios de estado de órdenes por mandar
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id   TEXT NOT NULL,
  body       TEXT NOT NULL,                    -- JSON de POST .../orders/{id}/state
  created_ms INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT
);

CREATE TABLE gps_fixes (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  result_id           TEXT,
  taken_ms            INTEGER NOT NULL,        -- reloj de pared al recibirlo
  fix_time_ms         INTEGER,                 -- Location.getTime()
  elapsed_realtime_ns INTEGER,                 -- Location.getElapsedRealtimeNanos()
  lat REAL, lon REAL, accuracy_m REAL, speed_mps REAL,
  provider            TEXT,                    -- gps | fused
  satellites_used     INTEGER,
  status              TEXT NOT NULL            -- fresh | stale | unavailable
);

CREATE TABLE events (                          -- el "registro" que ve el usuario
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts_ms    INTEGER NOT NULL,
  level    TEXT NOT NULL,                      -- info | warn | error
  phase    TEXT,
  order_id TEXT,
  msg      TEXT NOT NULL                       -- en español, una línea
);

CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT);   -- probe_config (JSON), last_backend_ok_ms,
                                                -- clock_offset_ms (server − local),
                                                -- last_slot_local, last_done_ms_A/B...
```

**Un solo dominio de reloj para decidir:** `execute_at_ms` y `not_after_ms` están **siempre**
en "reloj corregido" = `System.currentTimeMillis() + kv.clock_offset_ms` (0 si nunca hubo
backend). Las del backend se parsean de su texto (ya son hora del servidor); las locales
(`runNow`, plan local) se crean con el reloj corregido del momento. Así "elegir la más
antigua" y "¿venció?" comparan peras con peras aunque haya órdenes de los dos orígenes.
`created_ms`, `updated_ms`, `started_ms` y los `*_ms` de las demás tablas son reloj local
sin corregir (informativos). Las horas que salen en el JSON de resultados siguen siendo las
del reloj local (§0).

El esquema se crea en `onCreate` con versión 1; cualquier cambio futuro sube la versión y
usa `ALTER TABLE ... ADD COLUMN` en `onUpgrade` (nunca borrar la base: son datos de campo).

Retención: `events` se recorta a las últimas 2000 filas. `orders`, `results` y `gps_fixes`
**no se borran** (son datos de campo).

### 2.3 Máquina de estados de una prueba

Fases (valor de `phase` en el estado y en el backend): `stopped` (servicio detenido; solo
la reporta `status` del canal), `idle`, `waiting_ethernet`, `preparing`, `switching_route`,
`verifying_route`, `egress_check`, `gps_fix`, `ping`, `download`, `upload`, `storing`,
`restoring_route`, `uploading`, `backoff`.

"Ahora" para decidir si una orden del backend está debida o vencida es
`System.currentTimeMillis() + kv.clock_offset_ms` (desfase medido con `server_time` de la
última respuesta de `GET orders`, corregido por la mitad del tiempo de ida y vuelta). Las
órdenes locales también se guardan en ese reloj corregido (§2.2), así que hay **una sola**
comparación para todas. Las duraciones (fase, prueba, edad del GPS) se miden con
`SystemClock.elapsedRealtime()`, nunca restando horas de pared.

Hay dos bucles, uno por hilo (§2.1). El paso 2 es del hilo `probe-control`; los demás, del
`probe-worker` (cada 5 s mientras está activo, y de inmediato cuando el control guarda
órdenes nuevas o llega un `runNow`):

1. **Al arrancar el servicio:** toda orden local en `running` pasa a `interrupted`
   (`error: "app-reiniciada"`), se encola su cambio de estado en `outbox` y **no se repite**.
   Si esa orden ya tenía su resultado guardado en `results` (el proceso murió después de
   `storing`), no se toca: queda `done`/`failed` (por eso `storing` guarda resultado y
   estado de la orden en una sola transacción). Se intenta dejar la regla del MikroTik en
   `main` (§3.6) y se sigue aunque falle (se reintenta en cada ciclo, ver paso j).
   **En reposo** (sin prueba en curso), cada `mikrotik_idle_check_s` el worker lee la regla
   y las rutas; si la regla **no** está en `fallback_table`, la devuelve a `main` (ese es el
   estado de reposo que el celular garantiza; si alguien la forzó a mano en WinBox, el
   celular la revierte y deja un evento `warn` "Regla estaba en to-X sin prueba: vuelta a
   respaldo").
2. **Control (hilo `probe-control`, cada `poll_interval_s`):** `GET orders?horizon=6h` y,
   con la respuesta:
   - guarda `kv.clock_offset_ms`, `kv.last_backend_ok_ms` y `probe` en `kv.probe_config`;
   - upsert en `orders`: una orden **nueva** entra `pending`, salvo que el servidor la mande
     ya en `running` (el celular perdió su base, p. ej. al borrar datos de la app): entonces
     entra `interrupted` (`error: "estado-perdido"`) + `outbox`, y no se ejecuta. Una que ya
     existe solo actualiza `execute_at`/`not_after` si sigue `pending`/`delivered`; **nunca
     se retrocede un estado local**;
   - cada `order_id` de `closed` que localmente siga `pending`/`delivered` pasa al estado
     que dice el servidor (`cancelled` o `expired`), **sin** `outbox` (comparación-y-cambio,
     §2.1); si está `running`, se deja terminar;
   - **reconciliación por ausencia** (solo si `truncated = false`): una orden local de
     origen `server` en `pending`/`delivered` con `execute_at <= ahora + horizon` que **no**
     vino en `orders` ni en `closed` ya no está abierta en el servidor (se cerró fuera de
     la ventana de `closed`, o la base del servidor se restauró): pasa a `cancelled` con
     `error: "cerrada-en-servidor"`, sin `outbox`, y un evento `warn`. Así un celular que
     estuvo días sin backend no ejecuta una orden que el dashboard canceló;
   - si el backend volvió y hay órdenes **locales** `offline-schedule` en `pending` (el plan
     local ya no aplica, §2.9), pasan a `cancelled` con `error: "backend-volvio"` (no se
     suben: nunca existieron en el servidor);
   - `ack` de **todas** las órdenes locales de origen `server` con `acked = 0` (no solo las
     nuevas: si un `ack` anterior falló, se repite); las que el servidor devuelve en
     `acked` o `unchanged` quedan `acked = 1` (y `delivered` si estaban `pending`);
   - luego vacía `outbox` en orden de `id` (reglas de respuesta en §1.4, `/state`) y
     después sube resultados `pending` (lotes de 20, más viejos primero).
   Además, el control vacía `outbox` y sube resultados **también durante una prueba** (por
   WiFi, §2.8) apenas el worker los encola: así el dashboard ve `running` mientras se mide
   y no al final. Cada `status_interval_s` y en cada cambio de fase (el worker publica en
   `ProbeBus` y el control envía el último, sin cola: si hay varios pendientes, solo sale
   el más nuevo): `POST status` (§2.8). Si el backend falla, reintento con espera
   exponencial de 5 s a 300 s (fase `backoff` solo si no hay nada que medir; las pruebas
   **no esperan** al backend).
3. **Vencidas:** `pending`/`delivered` con `not_after` < ahora → `expired`
   (`error: "vencida"`, o `"sin-ethernet"` si estaba esperando el cable, o
   `"mikrotik-no-confirma"` si la última falla fue la regla) + `outbox`.
4. **Elegir:** la orden `pending`/`delivered` con `execute_at <= ahora` más antigua (por
   `execute_at_ms`, luego `server_id`, luego `created_ms`). Si no hay y el plan local aplica
   (§2.9), se crea una orden local. Una orden de `runNow` (§2.10) entra con `execute_at` =
   ahora y se ejecuta cuando termine la prueba en curso (nunca en paralelo). Si el slot de
   la orden (`A`, `B`, `C`…) no está en `kv.probe_config` (el equipo se borró de la sonda
   después de crear la orden), no hay tabla que forzar: la orden pasa a `interrupted` con
   `error: "slot-desconocido"` + `outbox` (transición válida desde `pending`/`delivered`) y
   no se mide.
5. **Ejecutar**, en este orden. **Hasta el paso c la orden no se consume**: si algo falla
   antes de confirmar la ruta, la orden vuelve a esperar (sigue `pending`/`delivered`, con
   el error en el estado y en `events`) y se reintenta en el siguiente ciclo hasta
   `not_after`. **No se mide sin confirmar.**

| Paso | Fase | Qué hace | Si falla |
|---|---|---|---|
| a | `preparing` | Decide el camino: si hay Ethernet → `net_path = ethernet`. Si no hay y `require_ethernet` → fase `waiting_ethernet`, la orden no se consume. Si no hay y no se exige, o `target = wifi` → `net_path = wifi` y se saltan b–c. Empieza a pedir el fix de GPS en paralelo (§2.4). Anota `TrafficStats` de inicio (§2.6). | — |
| b | `switching_route` | Resuelve el slot: `A`/`B` directo; `next` = el slot **activo** con el `done` más viejo (`kv.last_done_ms_<slot>`; sin dato = más viejo), empate → `A`. Tabla = la del slot en `kv.probe_config` (si no hay config, `table_A`/`table_B`). Pone la regla `phone-probe` en esa tabla (§3.6). Anota `forced_since` (reloj monótono): **la regla nunca queda forzada más de 8 min** seguidos; si una prueba llega a ese tope (no debería: con los topes de §2.6 dura menos de 5 min), se aborta como `failed` con `error = "tope-de-tiempo"` y se restaura. El vigilante del MikroTik (§3.2) la devuelve a `main` a los 10 min por si el celular murió. | 3 intentos con 2 s de espera; si no, la orden no se consume (ver arriba). |
| c | `verifying_route` | Lee la regla de vuelta: `table` = la pedida, `action = lookup-only-in-table`, `disabled = false`, → `mikrotik_confirmed = true` (`inactive` se registra en `mikrotik_rule` pero **no** bloquea: con el router desconectado lo que corresponde es medir y registrar la caída). Lee la ruta por defecto de esa tabla (`mikrotik_route_active`, `gateway_ip`, `gateway_iface`). Limpia el conntrack de la IP del celular (§3.6) y espera 1 s. **Recién aquí** genera `result_id` (UUID), marca la orden `running` local (con `started_ms`) + `outbox` `{"status":"running","at","result_id"}`, en una transacción. En `net_path = wifi` esto mismo se hace al final de a. | Como b. |
| d | `egress_check` | Atado a la red elegida, primero el **chequeo de portal cautivo**: `GET http://connectivitycheck.gstatic.com/generate_204` (HTTP plano, sin seguir redirecciones, 5 s): `204` → `captive_portal = false`; `3xx` → `captive_portal = true`, `portal_host` = host del `Location`; `200` u otro código con cuerpo → `captive_portal = true`, `portal_host = null`; error de red → `captive_portal = null`. Con portal, las pruebas HTTPS siguientes suelen fallar por TLS: es lo esperado. Luego `GET https://speed.cloudflare.com/meta` (JSON: `clientIp`, `asn`, `asOrganization`, `colo`) → `egress_ip`, `egress_asn_client`, `egress_as_org_client`, `egress_colo`; si falla, `GET https://www.cloudflare.com/cdn-cgi/trace` (`ip=`, `colo=`). Puerta de enlace: conexión TCP a `gateway_ip:80` bound a Ethernet, 2 s (`gw_reachable = true` si conecta **o** si la rechaza con RST —`ECONNREFUSED`: el equipo respondió—; `false` si vence el tiempo o da `EHOSTUNREACH`/`ENETUNREACH`, que es lo que pasa con el router desconectado); con `gateway_check` en ajustes se usa ese `host:puerto` en vez de `gateway_ip:80`; en `wifi`, `null`. DNS: `network.getAllByName(target_host)` → `dns_ok`. `internet_ok` = llegó `/meta` o la traza **y** `captive_portal ≠ true`. | Si `internet_ok = false` (con portal, `error = "portal-cautivo"` en vez de `"sin-internet"`): si la orden es `next` o tiene `allow_fallback`, hay otro slot activo y todavía es el intento 1, se cierra este intento como resultado `failed` (`error = "sin-internet"`, `order_id = null`, `related_order_id` = la orden, `attempt = 1`, sin pasos e–h pero con GPS si ya llegó), se vuelve a b con el otro slot, `attempt = 2` y `selection_reason = fallback:<slot pedido>-sin-datos`. Si no, sigue a e para registrar la caída. |
| e | `gps_fix` | Espera el fix pedido en a (tope `location_fix_timeout_s`). | Sigue con `location_status = unavailable`. |
| f | `ping` | Marca `test_started_at`. 10 conexiones TCP a `target_host:443` (o host:puerto del backend si el destino efectivo es `local`), 200 ms entre cada una, 2 s de tope. `ping_ms` = mediana, `jitter_ms` = promedio de diferencias absolutas consecutivas, `loss_pct`. | Si `internet_ok = false` o `loss_pct = 100`: `test_status = failed`, `error = "sin-internet"`, se saltan g–h. |
| g | `download` | §2.6. | `test_status = failed`, `error = "descarga: <motivo>"`, se sigue a h. |
| h | `upload` | §2.6. Marca `test_finished_at`. Repite `/meta` (o la traza) → `egress_ip_end`. En Ethernet, **relee la regla** en el MikroTik (una sesión corta: login + `/routing/rule/print`, §3.6) → `mikrotik_rule_end`; si la `table` ya no es la pedida, evento `error` "La regla cambió durante la prueba" (el servidor no la atribuye: `probe-route-changed`). Si la lectura falla, `mikrotik_rule_end = null` y se sigue. | `test_status = failed` si también falló la descarga; si solo falla la subida: `done` con `up_mbps` null y `error = "subida: <motivo>"`. |
| i | `storing` | Arma el JSON (§1.4) y, **en una transacción**, lo guarda en `results` (`pending`), pasa la orden local a `done`/`failed` y actualiza `kv.last_done_ms_<slot>` si `done`. Si `test_finished_at` falta (se saltaron g–h), es `test_started_at` o, si tampoco, la hora del paso. | — |
| j | `restoring_route` | Regla `phone-probe` → `fallback_table` (`main`), lectura de vuelta. Va **después** de guardar: si el proceso muere aquí, el resultado ya está a salvo. | Evento `error` "Regla NO restaurada", alerta `route-not-restored`, se reintenta al inicio de cada ciclo hasta lograrlo (mientras tanto el plano de control solo usa WiFi); el estado muestra `current_table` real. |
| k | `uploading` | Intenta subirlo ya (si no hay backend queda para el próximo ciclo). | — |

Intento 2 (respaldo): la orden ya está `running`, así que en c solo se genera un
`result_id` nuevo (se guarda en `orders.result_id`, sin nuevo `outbox`). Si en el intento 2
fallan b o c, la orden ya no puede "volver a esperar": se guarda un resultado `failed` con
`error = "mikrotik-no-confirma"`, `mikrotik_confirmed = false`, `routing_table` = la tabla
que se intentó, sin pasos d–h, y la orden se cierra con él (el servidor lo clasifica
`probe-route-unconfirmed` y no lo atribuye a ningún router).

Cada paso escribe una línea en `events` (p. ej. `"MikroTik: regla phone-probe → to-A (confirmada)"`,
`"Descarga: 85,3 Mbps (106 MB en 10,0 s)"`, `"GPS: fijo fresco, 3 s, ±6 m, 9 satélites"`).

`executed_offline = true` si la orden es `offline-schedule` o si el último contacto
exitoso con el backend (`kv.last_backend_ok_ms`) fue hace más de 5 min al empezar la prueba.

El resultado `failed` del intento 1 de un respaldo (paso d) se guarda en `results` **en ese
momento** (en su propia transacción), no al final: si el proceso muere en el intento 2, ese
dato ya está a salvo y se sube.

`stop` (§2.10) durante una prueba: el hilo de trabajo la aborta en el siguiente punto de
control (entre bloques de 64 KB, o al vencer un tiempo de conexión/lectura: nunca más de
10 s), la orden queda `interrupted` (`error: "detenida"`) + `outbox`, restaura la regla, el
control intenta vaciar `outbox` una vez (5 s como mucho) y recién entonces se detiene el
servicio. Lo que no alcance a subir queda en la base y sale con "Sincronizar ahora" o al
volver a activar la sonda.

### 2.4 GPS fresco por prueba

- `LocationManager.getCurrentLocation(GPS_PROVIDER, ...)` (API 30+; el S20+ tiene
  Android 13) con un `CancellationSignal` que se cancela al vencer
  `location_fix_timeout_s` (20). Si devuelve null o vence, un intento con
  `FUSED_PROVIDER` (constante de API 31+, protegida con `Build.VERSION`) de 5 s, **solo si**
  está en `locationManager.allProviders` (no todos los equipos lo exponen; si no, se usa
  `NETWORK_PROVIDER` si está habilitado). Si el GPS está apagado en el sistema, se salta
  directo a `unavailable` con alerta `gps-off`. `getCurrentLocation` se llama con un
  `Executor` propio (no el hilo principal) y el worker espera con un `CountDownLatch`
  con tope: nunca bloquea el hilo principal ni al `probe-control`.
- Satélites usados en el fix: `registerGnssStatusCallback` mientras se espera el fix
  (cuenta de `usedInFix`), desregistrado al terminar.
- Edad al inicio de la prueba: `(elapsedRealtimeNanos() en test_started_at −
  loc.elapsedRealtimeNanos) / 1e9` → `gps_age_s` (negativa → 0: el fix llegó después de
  empezar, lo que es normal porque se pide en paralelo); `gps_ts` = `test_started_at −
  gps_age_s`. En `raw` va además `gps_fix_time` = `loc.time` (hora GNSS, sirve para
  detectar un reloj del teléfono corrido).
- En oficina (bajo techo) lo normal es `unavailable` o `fused`: no es un error de la prueba.
- `location_status`: `fresh` si `gps_age_s <= location_max_age_s` (30; o
  `location_max_age_stationary_s` = 300 si `speed < 1 m/s`) y `accuracy <=
  location_max_accuracy_m` (100); `stale` si hay fix pero no cumple; `unavailable` si no hubo.
- Solo si es `fresh` se mandan `lat`, `lon`, `gps_accuracy_m`, `gps_source`
  (`android-gps` o `android-fused`). Si es `stale` van como `stale_lat`, `stale_lon`,
  `stale_accuracy_m` (el backend las guarda solo en `raw`). `location_source` = `gps`,
  `fused` o `none`. Cada fix va a `gps_fixes`.

### 2.5 Estado en vivo (EventChannel, `status` y `POST .../status`)

El mismo objeto sirve para la app y para el backend (sin secretos, §0):

```json
{
  "measured_by": "phone-3fa9c2d1",
  "probe_id": "hap-oficina",
  "sent_at": "2026-09-29T21:05:34Z",
  "seq": 1284,
  "service_started_at": "2026-09-29T18:00:02Z",
  "enabled": true,
  "running": true,
  "phase": "download",
  "phase_since": "2026-09-29T21:05:30Z",
  "phase_detail": "42,1 MB · 4 s",
  "current_order": {
    "order_id": "3f7d2a10-...", "origin": "server", "result_id": "6b0e1c2a-...",
    "target": "A", "slot": "A", "device_id": "router-R524260829000001", "label": "Notion 5G (Tigo)",
    "routing_table": "to-A", "selection_reason": "requested", "requested_by": "dashboard",
    "attempt": 1, "net_path": "ethernet", "started_at": "2026-09-29T21:05:02Z"
  },
  "mikrotik": {
    "host": "192.168.89.1", "reachable": true, "rule_found": true,
    "current_table": "to-A", "identity": "hap-sonda", "version": "7.6",
    "routes": {"to-A": {"active": true, "gateway": "192.168.1.1%ether2"}, "to-B": {"active": false, "gateway": "192.168.2.1%ether1"}, "main": {"active": true, "gateway": "192.168.1.1%ether2"}},
    "last_ok": "2026-09-29T21:05:03Z", "error": null
  },
  "net": {
    "require_ethernet": true,
    "ethernet": {"up": true, "iface": "eth0", "ip": "192.168.89.12", "validated": true},
    "wifi": {"up": true, "ip": "192.168.2.92", "validated": true},
    "default_network": "ethernet",
    "control_path": "wifi"
  },
  "backend": {"url": "http://192.168.40.22:8080", "reachable": true, "last_ok": "2026-09-29T21:05:00Z", "last_error": null, "clock_skew_s": 0.4},
  "gps": {"status": "fresh", "age_s": 3.1, "accuracy_m": 6.0, "source": "gps", "satellites_used": 9, "lat": 4.65123, "lon": -74.05321, "at": "2026-09-29T21:05:17Z"},
  "queue": {"orders_pending": 2, "next_order_at": "2026-09-29T21:10:00Z", "next_order_target": "B", "results_pending": 3, "results_rejected": 0, "outbox_pending": 1},
  "local_plan": {"active": false, "interval_s": 900, "next_at": null},
  "last_result": {
    "result_id": "0d4c...", "at": "2026-09-29T20:50:47Z", "slot": "B", "device_id": "router-R023-4g",
    "test_status": "done", "down_mbps": 22.4, "up_mbps": 5.1, "ping_ms": 61.0,
    "location_status": "fresh", "sync_status": "synced", "server_reason": "probe-asn-match", "error": null
  },
  "battery": {"pct": 76, "status": "discharging", "plugged": "none", "temp_c": 31.5, "optimization_ignored": true},
  "permissions": {"location": true, "background_location": true, "notifications": true},
  "alerts": [
    {"code": "no-ethernet", "level": "warn", "msg": "Se exige Ethernet y no hay cable: las órdenes esperan", "since": "2026-09-29T21:01:10Z"}
  ],
  "app_version": "1.1.0+2"
}
```

- `sent_at` = hora del celular al armarlo; `seq` crece en cada objeto emitido desde que
  arrancó el servicio (`service_started_at`); con ambos el backend descarta envíos viejos.
- `control_path` ∈ {`wifi`, `ethernet`, `default`, `none`}; `default_network` ∈
  {`ethernet`, `wifi`, `cellular`, `none`} (la que Android usa por defecto, informativo).
- `mikrotik.routes` se actualiza con cada lectura de la regla (§3.6). Una clave por tabla
  de equipo (`to-A`, `to-B`…) con su ruta por defecto, más `main` = la ruta por defecto
  **activa** de `main` (dice por qué router sale hoy el modo respaldo; `gateway` con
  `%ether2` = A). En `to-A`/`to-B`, `active: false` = el enlace de ese puerto está caído
  (router apagado o cable suelto): esas rutas **no** llevan `check-gateway` (§3.2), así que
  un router encendido pero sin datos sigue `active: true` y eso lo dice la prueba, no la
  ruta. En `main`, las rutas sí llevan `check-gateway=ping`: la activa es la del router
  cuya puerta de enlace responde. `gateway` es lo que el MikroTik tiene configurado (lo
  pone el DHCP de cada router, §3.2): la app lo muestra tal cual, nunca lo supone.
- `alerts`: lista (vacía si todo va bien) de problemas que la pantalla y el dashboard
  muestran arriba, en rojo o ámbar. Códigos (enumeración cerrada; `msg` en español lo arma
  el celular): `no-ethernet`, `mikrotik-unreachable`, `mikrotik-login`,
  `mikrotik-rule-missing`, `route-not-restored`, `backend-unreachable`, `no-wifi`,
  `no-location-permission`, `no-background-location`, `gps-off`, `battery-optimized`,
  `battery-low` (< 20 % y descargando), `clock-skew` (|desfase| > 5 s),
  `restart-failed`, `results-rejected`, `backend-auth` (el backend respondió `401`: API
  key mal puesta), `probe-not-configured` (`404 "sonda no configurada"` en `GET orders`),
  `captive-portal` (la última prueba de un slot vio portal cautivo; `msg` nombra el slot y
  el `portal_host`).
  **Nivel fijo por código** (el celular no lo elige): `error` = `mikrotik-unreachable` (solo
  si se exige Ethernet y hay cable), `mikrotik-login`, `mikrotik-rule-missing`,
  `route-not-restored`, `backend-auth`, `probe-not-configured`, `no-location-permission`,
  `restart-failed`; `warn` = todos los demás. Cada alerta lleva además `since` (hora local
  RFC 3339 de cuando apareció) para que la pantalla diga "desde hace N min".
- Campos que no aplican van `null` (nunca se omiten). `current_order` es `null` en `idle`,
  `backoff`, `stopped` y `waiting_ethernet` sin orden. Con el servicio detenido, `status`
  (§2.10) devuelve este mismo objeto con `running: false`, `phase: "stopped"` y lo que se
  pueda leer de prefs y de la base (cola, último resultado, permisos, batería).

### 2.6 Prueba de velocidad

Destino según `speed_target`:

| `speed_target` | Descarga | Subida | `target_host` |
|---|---|---|---|
| `cloudflare` (por defecto) | `GET https://speed.cloudflare.com/__down?bytes=25000000&measId=<result_id>` | `POST https://speed.cloudflare.com/__up?measId=<result_id>`, cuerpo de 10 MB | `speed.cloudflare.com` |
| `prod-download` | `GET <prod_url>/api/v1/speedtest/download?bytes=50000000` con `X-API-Key: <prod_api_key>` (**solo lectura**, sin `test_id`; si `prod_api_key` está vacía, este destino no se puede usar y se mide con `cloudflare`) | la de Cloudflare (a producción **nunca** se le hace POST) | host de `prod_url` (la subida va en `raw.upload_host`) |
| `local` | `GET <backend_url>/api/v1/speedtest/download?bytes=50000000&test_id=<result_id>` | `POST <backend_url>/api/v1/speedtest/upload?test_id=<result_id>`, 10 MB | host del backend |

- Todo con `network.openConnection(url)` sobre la red de la prueba (Ethernet, o WiFi si
  `net_path = wifi`), `Connection: close`, `User-Agent: notion5g-probe/<versión>`, conexión
  5 s, lectura 10 s.
- **Destino efectivo:** `local` con `backend_url` en IP privada (RFC 1918) solo sirve por
  WiFi; con `net_path = ethernet` la prueba usa `cloudflare` y lo anota (`target_kind =
  cloudflare`, `raw.target_requested = local`). La pantalla de ajustes lo advierte ("el
  backend local no es alcanzable por Ethernet; se usará Cloudflare"). `target_kind` es
  siempre el destino **efectivo**.
- Descarga: repite pedidos hasta cumplir `duration_s` o `max_mb_per_phase`, leyendo en
  bloques de 64 KB; `down_mbps = bytes·8 / segundos` desde el primer byte hasta el fin
  (tiempo con `elapsedRealtimeNanos`).
- Subida: repite POST de 10 MB (`setFixedLengthStreamingMode`, cuerpo generado desde un
  búfer reutilizado, sin reservar 10 MB) hasta `duration_s` o el tope; `up_mbps` igual,
  contando los bytes escritos al socket hasta el fin.
- HTTP ≠ 2xx (p. ej. `429` de Cloudflare por muchas pruebas) = fallo de esa fase con
  motivo `http-<código>`. Las redirecciones **no** se siguen (§2.1): un `3xx` es
  `http-3xx` y, si el chequeo de portal no lo había visto, pone `captive_portal = true` y
  `portal_host` = host del `Location`. Un error de TLS (portal que intercepta HTTPS) es
  `tls: <clase de la excepción>`.
- `concurrent_tests` = header `X-Speedtest-Concurrent` si viene.
- `other_traffic_bytes`: `(TrafficStats.getTotalRxBytes() + getTotalTxBytes())` al final −
  al inicio, menos lo mismo con `getUidRxBytes/getUidTxBytes(Process.myUid())`. Es
  aproximado (suma todas las interfaces, WiFi incluido) pero muestra si otra app (Play
  Store, copias de seguridad) movió datos durante la prueba. `null` si alguna devuelve
  `UNSUPPORTED`.
- **Consumo de datos de las SIM de los routers:** con los topes por defecto una prueba puede
  gastar hasta ~400 MB (200 de bajada + 200 de subida). Una prueba cada 15 min por router
  son decenas de GB al día. La pantalla de ajustes muestra la estimación
  (`2 × max_mb_per_phase × pruebas por día`) junto a `local_plan_interval_s`.
- El hAP ac2 enruta con NAT a unos pocos cientos de Mbps; por encima de eso la medición la
  limita el MikroTik, no el router. Anotado para interpretar resultados muy altos del 5G.

### 2.7 Ajustes (SharedPreferences `"probe"`, dueño: nativo)

| Clave | Tipo | Por defecto |
|---|---|---|
| `enabled` | bool | `false` |
| `backend_url` | string | `http://192.168.40.22:8080` |
| `api_key` | string | `""` (la del backend **local**, `API_KEY` de `server/.env`; **no** se precarga: la de Ajustes generales es la de producción) |
| `probe_id` | string | `hap-oficina` |
| `phone_id` | string | `""` → `phone-<ANDROID_ID[0..8]>` |
| `mikrotik_host` | string | `192.168.89.1` |
| `mikrotik_port` | int | `8728` |
| `mikrotik_user` | string | `phone-probe` |
| `mikrotik_password` | string | `""` |
| `mikrotik_rule_comment` | string | `phone-probe` |
| `fallback_table` | string | `main` |
| `table_A`, `table_B` | string | `to-A`, `to-B` (solo si no hay config del backend en caché) |
| `require_ethernet` | bool | `true` ("Exigir Ethernet para medir") |
| `speed_target` | string | `cloudflare` (`cloudflare` \| `prod-download` \| `local`) |
| `prod_url` | string | `https://notion.h3s-iot.com` |
| `prod_api_key` | string | `""` (la pantalla la precarga con la API key de Ajustes generales, que es la de producción; vacía = `prod-download` no disponible) |
| `duration_s` | int | `10` (la orden manda si trae otro) |
| `max_mb_per_phase` | int | `200` |
| `gateway_check` | string | `""` (vacío = `gateway_ip:80` leído del MikroTik para la tabla de la prueba, §3.6; si se llena, `host:puerto` fijo para todos los slots) |
| `flush_conntrack` | bool | `true` (limpiar conexiones rastreadas del celular en el MikroTik tras cambiar la regla, §3.6) |
| `mikrotik_idle_check_s` | int | `60` (cada cuánto se lee la regla y las rutas con la sonda en reposo) |
| `poll_interval_s` | int | `30` |
| `status_interval_s` | int | `15` |
| `local_plan_enabled` | bool | `true` |
| `local_plan_interval_s` | int | `900` (si el backend mandó `interval_s > 0`, se usa ese) |
| `offline_after_s` | int | `600` |
| `location_fix_timeout_s` | int | `20` |
| `location_max_age_s` | int | `30` |
| `location_max_age_stationary_s` | int | `300` |
| `location_max_accuracy_m` | int | `100` |

### 2.8 Camino hacia el backend (plano de control)

Por cada llamada, en orden: 1) atada al **WiFi** si está arriba; 2) atada a **Ethernet**
solo si la regla está confirmada en `main` (nunca durante una prueba ni con la regla sin
restaurar) y el host del backend **no** es una IP privada (RFC 1918) — con el backend local
se salta; 3) la red por defecto, **solo** si la red por defecto no es Ethernet (o si lo es
y se cumplen las condiciones de 2). Tiempo de conexión 5 s, lectura 15 s.
`control_path` = el último camino que funcionó. Durante una prueba (fases `switching_route`
a `restoring_route`) solo se usa WiFi: `POST status` y cualquier otra llamada que no pueda
ir por WiFi se omite hasta el final. "Probar backend" usa este mismo camino.

### 2.9 Plan local sin backend

Si `local_plan_enabled`, no hay orden debida y el último contacto exitoso con el backend es
más viejo que `offline_after_s`: cada `local_plan_interval_s` se crea una orden
`origin = local`, `order_id = local-<uuid>`, `target` = el slot siguiente a
`kv.last_slot_local` (alterna A, B), `selection_reason = offline-schedule`,
`not_after` = `execute_at + intervalo`. Sus resultados salen con `order_id: null` y
`local_order_id`. Al volver el backend se deja de generar, las órdenes locales
`offline-schedule` que seguían `pending` se cancelan (`error: "backend-volvio"`, §2.3) y se
suben los resultados pendientes. Si la
config en caché dice `enabled: false`, no hay plan local. Solo alterna entre slots
**activos** de la config en caché (por defecto `A`, `B`). El plan local nunca se genera
mientras hay una orden `pending`/`delivered` guardada que todavía no venció (aunque no
esté debida): las órdenes del backend ya descargadas mandan.

### 2.10 Canales Flutter ↔ Kotlin

`MethodChannel("notion5g/probe")` (registrado en `MainActivity.configureFlutterEngine`):

| Método | Argumentos | Devuelve |
|---|---|---|
| `getConfig` | — | mapa con todas las claves de §2.7 (con los valores por defecto resueltos, `phone_id` incluido). Es el único lugar donde salen los secretos |
| `saveConfig` | mapa parcial de claves §2.7 | `null`, o error `invalid_config` con mensaje (URL mal formada, puerto fuera de rango, entero ≤ 0). Si el servicio corre, toma la config nueva al inicio del siguiente ciclo (nunca a mitad de una prueba) |
| `start` | — | `null`, o error `start_failed` con mensaje. Pone `enabled = true`, programa el watchdog y arranca `ProbeService` |
| `stop` | — | `null`. `enabled = false`, cancela watchdog; si hay prueba en curso la aborta como dice §2.3; restaura la regla a `main` (mejor esfuerzo) y detiene el servicio |
| `status` | — | objeto de §2.5 (también con el servicio detenido, armado desde prefs y la base) |
| `runNow` | `{"target": "A" \| "B" \| "next" \| "wifi"}` | `{"order_id": "local-..."}`; crea una orden local `manual-app` con `execute_at` = ahora y `not_after` = ahora + 10 min (`wifi` fuerza `net_path = wifi`). Funciona también con el servicio detenido: lo arranca si `enabled`, si no devuelve error `not_enabled` |
| `testMikrotik` | — | `{"ok": true, "identity": "hap-sonda", "version": "7.6", "rule": {"table": "main", "action": "lookup-only-in-table", "disabled": false, "inactive": false}, "routes": {"to-A": {"active": true, "gateway": "192.168.1.1%ether2"}, "to-B": {"active": true, "gateway": "192.168.2.1%ether1"}}, "error": null}`. Se encola al `probe-worker` (con el servicio detenido, lo corre un hilo de fondo de un solo uso; nunca en el hilo principal). Si hay prueba en curso, responde con lo último leído sin abrir otra sesión (así nunca hay dos sesiones del celular a la vez) |
| `restoreFallback` | — | `{"ok": true, "table": "main", "error": null}`. Se encola al `probe-worker` (servicio detenido: hilo de fondo de un solo uso). Con prueba en curso: error `busy` |
| `syncNow` | — | `{"uploaded": 3, "rejected": 0, "error": null}`. Lo corre el `probe-control` (también durante una prueba, por WiFi); con el servicio detenido, un hilo de fondo de un solo uso: los resultados guardados se pueden subir aunque la sonda esté apagada |
| `testBackend` | — | `{"ok": true, "path": "wifi", "status": 200, "probe_found": true, "runner": "phone", "server_time": "...", "error": null}`: `GET /healthz` y luego `GET /api/v1/probes/{probe_id}` (prueba API key y que la sonda exista y sea `phone`) |
| `openBatterySettings` | — | `null`; abre `ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` para esta app |
| `recentResults` | `{"limit": 20}` | lista de `{result_id, order_id, attempt, created_at, slot, device_id, test_status, down_mbps, up_mbps, ping_ms, location_status, gps_age_s, net_path, routing_table, selection_reason, executed_offline, sync_status, server_net_route, server_confidence, server_reason, error}` |
| `recentEvents` | `{"limit": 100}` | lista de `{id, at, level, phase, order_id, msg}`, más nuevas primero |

`EventChannel("notion5g/probe/events")`: emite el objeto de §2.5 en cada cambio (como
mucho 4 por segundo; el último siempre sale). Al suscribirse, emite de inmediato el último
estado guardado en `ProbeBus`. La pantalla además llama `status` cada 5 s por si el stream
se corta, y `recentEvents`/`recentResults` al abrir y cada vez que cambia `phase` o
`last_result.result_id`. Métodos que tardan (red, MikroTik) nunca bloquean el hilo
principal; los errores de canal usan `result.error(code, mensaje, null)` con mensajes en
español.

### 2.11 Pantalla "Sonda A/B"

Se abre desde el icono nuevo de la AppBar del inicio. De arriba abajo:

0. **Alertas** (`alerts` de §2.5), una tarjeta por alerta, roja (`error`) o ámbar
   (`warn`), con acción cuando la hay: `battery-optimized` → "Quitar optimización"
   (`openBatterySettings`), `no-background-location` → abrir ajustes de la app,
   `route-not-restored` → "Volver a respaldo", `backend-auth`/`probe-not-configured` →
   "Abrir ajustes de la sonda", `no-location-permission` → pedir el permiso.
1. **Interruptor "Sonda activa"** + fase actual en grande con icono y tiempo en la fase
   ("Descargando… 42,1 MB · 4 s"), y una barra de pasos (Ruta → Verificación → Salida →
   GPS → Ping → Bajada → Subida → Guardar → Respaldo) con el paso actual resaltado. Si hay
   orden en curso: "Midiendo router A (Notion 5G) · tabla to-A · pedida desde el
   dashboard" (o "respaldo: A sin datos, midiendo B"). Nombres de fase en español:
   `idle` "En espera", `waiting_ethernet` "Esperando el cable", `preparing` "Preparando",
   `switching_route` "Cambiando la ruta", `verifying_route` "Verificando la ruta",
   `egress_check` "Comprobando la salida", `gps_fix` "Esperando GPS", `ping` "Midiendo
   latencia", `download` "Descargando", `upload` "Subiendo", `storing` "Guardando",
   `restoring_route` "Volviendo a respaldo", `uploading` "Enviando resultado", `backoff`
   "Sin backend, reintentando", `stopped` "Detenida". El dashboard usa los mismos textos.
2. **Ruta (MikroTik):** alcanzable sí/no, tabla actual (`main` se muestra como "Respaldo
   (sale por el que tenga datos)", `to-A` como "Forzada a A", `to-B` como "Forzada a B"),
   estado de cada router según `mikrotik.routes` ("A: enlace arriba · 192.168.1.1" / "A:
   enlace caído"; nunca "A con datos": eso lo dice la última prueba), "Respaldo sale hoy
   por A/B" según `routes.main`, identidad y versión, último error, y la hora de la última
   lectura (`mikrotik.last_ok`). Botones "Probar MikroTik" y "Volver a
   respaldo".
3. **Red:** Ethernet (arriba/abajo, IP), WiFi (IP), red por defecto de Android, camino al
   backend, "Exigir Ethernet" (sí/no). Aviso en rojo si se exige Ethernet y no hay cable.
4. **GPS:** Fresco / Viejo / Sin fijo, edad, precisión, satélites.
5. **Cola:** órdenes pendientes y la próxima hora, resultados por subir, rechazados,
   backend alcanzable y último contacto, desfase de reloj si pasa de 5 s. Botón
   "Sincronizar ahora". Si el plan local está activo: "Sin backend: plan local cada 15 min".
6. **Medir ahora:** "Medir A", "Medir B", "Siguiente", y "Medir por WiFi" (visible cuando
   no hay Ethernet y no se exige).
7. **Últimos resultados** (10): hora, router, bajada/subida/ping, insignia GPS, insignia de
   subida (Pendiente / Subido / Rechazado), veredicto del servidor si ya subió
   ("vía A · ASN coincide"), error si falló.
8. **Registro** (últimas 100 líneas de `events`), desplegable.

Icono de engranaje → **Ajustes de la sonda** con todas las claves de §2.7 agrupadas
(Backend, MikroTik, Medición, GPS, Plan local), contraseñas y API keys en campos ocultos, y
botón "Probar backend" (`testBackend`, por el camino de §2.8). La pantalla funciona igual
con el servicio detenido (muestra `phase: stopped` y lo guardado).

---

## 3. MikroTik hAP ac2 (RouterOS 7.6)

### 3.1 Cableado (ya está hecho; **no se mueven cables**, ver hallazgos)

| Puerto | Qué va | Notas |
|---|---|---|
| `ether1` | Router **B** (Notion 4G, SIM Movistar), a un puerto LAN | WAN B, tabla `to-B`. Su LAN hoy es `192.168.2.0/24` (gateway `192.168.2.1`) |
| `ether2` | Router **A** (Notion 5G), a un puerto LAN | WAN A, tabla `to-A`. Hoy está **dentro del bridge**: hay que sacarlo. LAN probable `192.168.1.0/24` |
| `ether3` | Celular (adaptador USB-C Ethernet) | Subred propia `192.168.89.0/24` |
| `ether4`, `ether5`, WiFi | Administración (bridge) | El PC va en `ether5` (adaptador ASIX, Windows ifIndex **82**) |

### 3.2 Direcciones y tablas

| Qué | Configuración | `comment` |
|---|---|---|
| Fijar MAC del bridge (primer paso: la IPv6 link-local con la que entra el PC depende de ella) | `/interface bridge set bridge auto-mac=no admin-mac=<MAC actual>` (hoy `DC:2C:6E:F7:7F:E9`, la que da `fe80::de2c:6eff:fef7:7fe9`; la herramienta la lee, no la supone) | — |
| Sacar `ether2` y `ether3` del bridge | `/interface bridge port remove [find interface=ether2]`, ídem `ether3` | — |
| Listas | `ether1`, `ether2` en `WAN`; `ether3` en `LAN` (el bridge ya está en `LAN`) | `probe:wan-B`, `probe:wan-A`, `probe:phone` |
| WAN B (`ether1`) | El DHCP client que ya existe (`defconf`, hoy *bound* a `192.168.2.100/24`) se **conserva** y se ajusta: `add-default-route=no use-peer-dns=no use-peer-ntp=no script=<script WAN, abajo>` | `probe:wan-B` |
| WAN A (`ether2`) | `/ip dhcp-client add interface=ether2 add-default-route=no use-peer-dns=no use-peer-ntp=no script=<script WAN>` | `probe:wan-A` |
| Celular | `/ip address add address=192.168.89.1/24 interface=ether3`; pool `probe-phone` `192.168.89.10-192.168.89.50`; `/ip dhcp-server add name=probe-phone interface=ether3 address-pool=probe-phone lease-time=1h`; red `192.168.89.0/24 gateway=192.168.89.1 dns-server=1.1.1.1,8.8.8.8` | `probe:phone-net` |
| Administración | La dirección `192.168.1.1/24` del bridge pasa a `192.168.88.1/24`; su pool a `192.168.88.10-192.168.88.254`; su red DHCP a `192.168.88.0/24` **sin gateway y sin DNS** (para que el PC no tome una ruta por defecto hacia los routers bajo prueba). Ni la subred del celular ni la de administración pueden ser `192.168.1.0/24` (LAN de A) ni `192.168.2.0/24` (LAN de B y WiFi "Pipito" de la oficina) ni `192.168.40.0/24` (backend) | `probe:mgmt` |
| Tablas | `/routing table add name=to-A fib`, `name=to-B fib` | `probe:to-A`, `probe:to-B` |
| Rutas | `0.0.0.0/0 gateway=<gwA>%ether2 routing-table=to-A`; `0.0.0.0/0 gateway=<gwB>%ether1 routing-table=to-B` (**sin** `check-gateway`: forzado a un router caído, el tráfico debe fallar, no desviarse); en `main`: `gateway=<gwA>%ether2 distance=1 check-gateway=ping` y `gateway=<gwB>%ether1 distance=2 check-gateway=ping` | `probe:A-default`, `probe:B-default`, `probe:main-A`, `probe:main-B` |
| Script WAN (en cada DHCP client) | Al quedar *bound*, pone `gateway=$"gateway-address"%$interface` en las dos rutas de ese WAN (`probe:A-default` + `probe:main-A` para `ether2`; `probe:B-default` + `probe:main-B` para `ether1`) **solo si difiere** del actual (así no escribe la flash en cada renovación). Al perder la concesión no toca nada. Forma (para `ether2`; la herramienta genera la de `ether1` cambiando los comentarios): `:if ($bound=1) do={:local gw ($"gateway-address" . "%" . $interface); :foreach c in={"probe:A-default";"probe:main-A"} do={:foreach r in=[/ip route find comment=$c] do={:if ([:tostr [/ip route get $r gateway]] != $gw) do={/ip route set $r gateway=$gw; :log info "probe: $c -> $gw"}}}}` | (propiedad `script` del DHCP client) |
| NAT | Si ya hay una regla `chain=srcnat action=masquerade out-interface-list=WAN` **sin** `src-address`, se reutiliza; si no, `/ip firewall nat add chain=srcnat out-interface-list=WAN action=masquerade`. La regla que hay hoy (`masquerade` de `192.168.1.0/24` por `ether1`) no se toca (deja de aplicar al renumerar el bridge; queda en el volcado) | `probe:masq` (solo si se crea) |
| Reglas de ruteo, **en este orden** | 1) `dst-address=192.168.89.0/24 action=lookup-only-in-table table=main` (sin esta, las respuestas del propio MikroTik al celular — API, DHCP — saldrían por un router: su dirección `192.168.89.1` también cae en la regla 2) 2) `src-address=192.168.89.0/24 action=lookup-only-in-table table=main` | 1) `probe:phone-local` 2) **`phone-probe`** |
| Vigilante de la regla | `/system scheduler add name=probe-rule-watchdog interval=1m policy=read,write on-event=<script>`: si la regla `phone-probe` lleva **10 minutos seguidos** fuera de `main`, la devuelve a `main` y deja `:log warning`. Cuenta en una variable global (RAM, no flash): `:global probeForcedMin; :local r [/routing rule find comment="phone-probe"]; :if ([:len $r]=1) do={:local t [/routing rule get [:pick $r 0] table]; :if ($t="main") do={:set probeForcedMin 0} else={:if ([:typeof $probeForcedMin]!="num") do={:set probeForcedMin 0}; :set probeForcedMin ($probeForcedMin+1); :if ($probeForcedMin>=10) do={/routing rule set [:pick $r 0] table=main; :log warning "phone-probe: regla en $t por 10 min, vuelta a main"; :set probeForcedMin 0}}}`. Cubre el caso "el celular murió con la regla forzada" (sin él, todo el tráfico del teléfono, el modo en movimiento incluido, quedaría pegado a un router que quizá no tiene datos hasta que alguien abra la app). El celular nunca deja la regla forzada más de 8 min (§2.3 paso b), así que el vigilante no pisa una prueba sana; si igual lo hiciera, la relectura del paso h lo detecta (`probe-route-changed`). `--rule-watchdog-min N` (0 = no instalarlo) | `probe:rule-watchdog` |
| API | `/ip service set api disabled=no address=192.168.88.0/24,192.168.89.0/24,fe80::/10` (el `fe80::/10` es obligatorio: el PC entra por link-local). La herramienta **se niega** a aplicar un valor de `address` que no incluya `fe80::/10` (dejaría al PC afuera) | — |
| Usuario del celular | `/user group add name=probe-api policy=read,write,api` (sin `ssh`, `winbox`, `ftp`, `web`, `telnet`, `local`: solo puede entrar por la API; `write` es global en RouterOS, no se puede limitar a un menú, por eso además se limita por `address`); `/user add name=phone-probe group=probe-api address=192.168.89.0/24 password=<secreto>` | `probe:api-group`, `probe:phone-user` |
| Hora | `/ip dns set servers=1.1.1.1,8.8.8.8`; `/system ntp client set enabled=yes servers=162.159.200.1,162.159.200.123` (IP de `time.cloudflare.com`: no depende del DNS al arrancar; el reloj hoy marca dic/2025); `/system clock set time-zone-name=America/Bogota` | — |
| Nombre | `/system identity set name=hap-sonda` | — |
| Verificaciones (no cambian nada si ya están bien) | `/ip settings rp-filter` = `no` (el de fábrica; con `strict` el MikroTik descartaría respuestas asimétricas: si no es `no`, la herramienta avisa y lo pone en `no` solo con `--fix-rp-filter`). `ether3` sin dirección IPv6 global y sin anuncio de prefijo (`/ipv6/address`, `/ipv6/nd`): las reglas de ruteo son solo IPv4; si el celular tuviera IPv6 por Ethernet, ese tráfico no pasaría por la regla | — |

`<gwA>` y `<gwB>` salen del `gateway` que muestra `/ip/dhcp-client` para `ether2` y `ether1`
al correr la herramienta (hoy B = `192.168.2.1`); si un WAN no está *bound* (router apagado),
se usan `--gw-a`/`--gw-b` y, si faltan, la herramienta crea esa ruta con el gateway del
parámetro por defecto (`192.168.1.1` para A, `192.168.2.1` para B) y avisa: el script WAN la
corrige en la primera concesión. **Nada** del celular ni del dashboard tiene gateways
escritos: los leen del MikroTik (§3.6).

Por qué funciona aunque las LAN de A y B coincidan (hoy no coinciden, pero un router puede
cambiar de LAN): cada ruta nombra su interfaz (`%ether2`/`%ether1`; en v7 un gateway con
`%interfaz` se resuelve en esa interfaz y no necesita ruta conectada en `to-A`/`to-B`),
cada interfaz tiene su propia tabla ARP, y el `masquerade` toma la dirección de la
interfaz de salida. Si las dos LAN coincidieran, en `main` quedarían dos rutas conectadas
al mismo prefijo: nada debe depender de `main` para llegar a la LAN de un router, y la
prueba de puerta de enlace del celular se hace con la regla ya forzada (allí el gateway
cae en la ruta por defecto de esa tabla, que apunta al router correcto). Si algo se
comporta raro con LAN iguales, cambiar la LAN de uno de los routers (p. ej. a
`192.168.10.1/24`) y volver a correr la herramienta.

Sobre `192.168.2.x`: la LAN de B y la WiFi "Pipito" de la oficina usan la misma subred,
pero son redes distintas y el celular las ve en redes Android distintas (WiFi y Ethernet,
cada una con su tabla): una conexión atada a Ethernet hacia `192.168.2.1` va al MikroTik
(y de ahí a B); una atada al WiFi va a "Pipito". Por eso **toda** conexión del celular se
ata explícitamente a una red (§2.1, §2.8), nunca se deja a la red por defecto.

El firewall de fábrica (entrada solo desde `LAN`, `drop invalid`, fasttrack) no se toca.
`ether3` **debe** quedar en la lista `LAN` (si no, el `drop` de entrada bloquea la API y el
DHCP del celular); `ether2` pasa de estar en el bridge (`LAN`) a `WAN`: la LAN de A deja de
poder entrar al MikroTik, que es lo correcto. Con fasttrack, las conexiones ya
establecidas conservan el NAT del router por el que nacieron: por eso el celular abre
conexiones nuevas tras cada cambio de regla y limpia su conntrack (§3.6).

Tras cambiar la dirección del bridge, en Windows el adaptador del MikroTik (ifIndex 82) no
debe tener **puerta de enlace** (la red DHCP de administración no la entrega). Verificar con
`ipconfig`; si aparece una, el PC podría mandar su tráfico a Internet por los routers bajo
prueba: quitarla o subir la métrica de esa interfaz.

### 3.3 Qué significa cada modo

| Modo | Regla `phone-probe` | Efecto |
|---|---|---|
| Respaldo (`fallback`) | `table=main` | El celular sale por A; si la puerta de enlace de A no responde al ping, por B (distancias 1/2 + `check-gateway`). Es el estado de reposo. **Ojo:** solo detecta router apagado, no "router sin datos" ni portal cautivo. |
| Forzado A (`to-A`) | `table=to-A` | Todo el tráfico del celular sale por `ether2`. Si A no tiene datos, falla (no hay respaldo): así se mide la caída sin confundir routers. |
| Forzado B (`to-B`) | `table=to-B` | Igual por `ether1`. |

`action` siempre `lookup-only-in-table`, `disabled=no`. El celular solo cambia `table`; el
vigilante (§3.2) solo la devuelve a `main`.

### 3.4 Herramienta de Windows (`scripts/mikrotik/apply_probe.py`)

(La sección de hallazgos la llama `mkt_apply.py`: es la misma herramienta; el nombre del
archivo es `apply_probe.py`.)

- Python de Windows, **solo biblioteca estándar** (`socket`, `ftplib`, `json`,
  `argparse`, `secrets`). Se copia a `C:\Users\diego\mikrotik-probe\` y se corre con
  `powershell.exe -NoProfile -Command "cd C:\Users\diego\mikrotik-probe; python apply_probe.py ..."`.
- Conexión: `--host fe80::de2c:6eff:fef7:7fe9%82` (por defecto; el `%82` es el ifIndex de
  Windows del adaptador ASIX conectado a `ether5` —**no** 22—, va como `scope_id` del socket
  IPv6; puede cambiar si se cambia de adaptador: `Get-NetAdapter` lo muestra), `--user admin`,
  `--password` (obligatorio, nunca por defecto en el código). También acepta IPv4.
- Modos: **`--dry-run` (por defecto)** lee el estado, arma el plan y muestra cada cambio
  como línea de consola RouterOS equivalente (`/ip address add ...`), sin tocar nada;
  `--apply` hace el respaldo y aplica; `--emit-rsc` imprime el script `.rsc` equivalente
  (se guarda en el repo como `scripts/mikrotik/probe-ab.rsc`, para pegar a mano en WinBox);
  `--verify` solo compara el estado con lo esperado.
- **Idempotente:** cada objeto se busca por su `comment` (tabla §3.2) o por su clave
  natural (nombre de tabla, interfaz); si existe y coincide no se toca, si difiere se hace
  `set`, si falta `add`. Correrla dos veces seguidas: la segunda no cambia nada.
  **Excepción:** en la regla `phone-probe` se comparan `src-address`, `action` y
  `disabled`, **no** `table` (la mueve el celular; si quedó en `to-A` tras una caída, la
  herramienta no lo cuenta como diferencia). Solo al **crearla** se pone `table=main`. Aun
  así, `--apply` avisa y pide `--force` si la regla no está en `main` (señal de que el
  celular está midiendo): no correr `--apply` con la sonda activa. Tampoco cuenta como
  diferencia el `gateway` de las cuatro rutas `probe:*-default`/`probe:main-*` cuando el
  WAN está *bound* y el gateway coincide con el de su DHCP client (lo mantiene el script
  WAN); si no coincide, sí es diferencia y se corrige.
- Compara valores normalizados: los booleanos vuelven como `true`/`false` (y se mandan
  como `yes`/`no`), las listas (`address=` del servicio) se comparan como conjuntos, y las
  propiedades que RouterOS no devuelve cuando están en su valor por defecto se tratan como
  ese valor.
- **Respaldo obligatorio antes de cualquier cambio** (si falla, aborta sin tocar nada):
  1. `/export =show-sensitive= =file=pre-probe-<AAAAMMDD-HHMMSS>` por la API (si da
     `!trap`, sin `=show-sensitive=`; si sigue fallando, `/system/script` temporal con ese
     comando + `/system/script/run` y borrar el script); esperar hasta 20 s a que aparezca
     `pre-probe-....rsc` en `/file` con `size` estable;
  2. `/system/backup/save =name=pre-probe-<...> =dont-encrypt=yes` (queda también en el router);
  3. bajar los dos archivos por **FTP** (mismo usuario) a
     `C:\Users\diego\mikrotik-probe\backups\` y comprobar que el tamaño bajado = `size`.
     Con link-local, `ftplib` necesita una subclase que conecte el socket de datos a la
     misma dirección con el mismo `scope_id` (el `getpeername()` de Windows no conserva el
     `%82` en el texto). Si el servicio `ftp` está deshabilitado, se habilita **solo**
     durante la descarga con `address=fe80::/10` y se deja como estaba al terminar
     (cambio anotado en el volcado). **No** hay plan B leyendo `contents` por la API:
     RouterOS solo devuelve `contents` de archivos de menos de 4 KB y un export completo es
     más grande. Si el FTP falla: aborta, salvo `--skip-download` (entonces confía en el
     `.backup` que queda en el router y en el volcado JSON, y **no** borra el `.rsc`);
  4. guardar además un volcado JSON de todos los menús que se tocan (esto sí por la API);
  5. borrar del router solo el `.rsc` ya bajado (poco espacio: ~1,6 MB libres); el
     `.backup` se queda. Antes de guardar, comprobar `free-hdd-space` de
     `/system/resource` ≥ 300 KB; si no, abortar.
- Orden de aplicación: MAC del bridge → tablas → listas → puertos del bridge → direcciones
  → DHCP (servidor del celular, clientes WAN con su script) → esperar hasta 15 s a que
  `ether2` quede *bound* → rutas → reglas (verificando el orden; si está mal,
  `/routing/rule/move`) → NAT → grupo y usuario → vigilante → servicio API →
  DNS/NTP/hora/identidad → verificación.
- **Cuidado al renumerar el bridge y sacar `ether2`**: la sesión de la herramienta va por
  IPv6 link-local sobre el bridge (con MAC fija, paso 1), así que no se corta; nada de lo que
  aplica toca `ether5` ni el bridge como interfaz. Si la sesión se cayera a mitad, correr
  `--dry-run` de nuevo muestra lo que falta (todo es idempotente).
- Contraseña de `phone-probe`: si el usuario no existe, se genera con `secrets` (16
  caracteres) salvo `--phone-password`; se imprime al final y se guarda en
  `C:\Users\diego\mikrotik-probe\phone-probe-password.txt`. Si ya existe, no se cambia salvo
  `--rotate-phone-password`.
- Verificación final (solo avisa, los routers pueden no estar conectados): `/ping
  =address=<gwA> =interface=ether2 =count=2` y `/ping =address=<gwB> =interface=ether1
  =count=2` (gateways leídos de `/ip/dhcp-client`, nunca supuestos); imprime los DHCP
  client con su estado y gateway, la regla `phone-probe`, el orden de las reglas, las
  rutas con su estado `active`, `/ip arp` de `ether1`/`ether2`, el vigilante y la hora
  (`/system/clock`, estado de `/system/ntp/client`). Si una ruta de `main` queda inactiva
  con el router encendido (el router no responde ping desde su LAN), avisa y sugiere
  `--check-gateway arp`.
- El usuario `phone-probe` se prueba al final: la herramienta **no** puede loguearse con
  él desde el PC (está limitado a `192.168.89.0/24`); lo prueba el celular con "Probar
  MikroTik".
- Volver atrás (documentado en la ayuda del script, no automático):
  `/system backup load name=pre-probe-<...>.backup` (reinicia el equipo).
- Detalles de la API en 7.6 a tener en cuenta: el flag `fib` de `/routing/table/add` puede
  requerir `=fib=` o `=fib=yes` (probar el primero y si da `!trap` el segundo); los booleanos
  vuelven como `true`/`false`; las propiedades de "bandera" que están en falso (`inactive`,
  `disabled`, `dynamic`) a veces **no vienen** en la respuesta: ausente = `false`. Los
  scripts (WAN y vigilante) se mandan como un solo valor `=on-event=`/`=script=` con los
  `;` y comillas tal cual (la API no interpreta el contenido); `--emit-rsc` los escribe con
  el escapado de consola (`\$`, `\"`).

### 3.5 Lo que el celular necesita del MikroTik (y nada más)

- API en `192.168.89.1:8728` desde `192.168.89.0/24`, usuario `phone-probe`.
- Exactamente **una** regla con `comment=phone-probe`, **después** de `probe:phone-local`.
- Tablas `to-A` y `to-B` con su ruta por defecto, y `main` con las dos de respaldo.
- El celular **solo** cambia `table` de esa regla y borra entradas de conntrack de su
  propia IP. Nunca crea, borra ni reordena nada. Los únicos otros actores que tocan la
  regla son el vigilante (solo la devuelve a `main`, a los 10 min) y una persona en
  WinBox; el celular lo detecta releyendo la regla al final de cada prueba.

### 3.6 Protocolo que usa el celular (API clásica)

Palabras con prefijo de largo (igual que `C:\Users\diego\mkt_api.py`: 1 byte si < 0x80,
2 si < 0x4000, 3 si < 0x200000, 4 si < 0x10000000, 5 —`0xF0` + 4 bytes— si no), frase
terminada con una palabra vacía, texto UTF-8. Respuestas `!re`, `!done`, `!trap`
(`=message=`), `!fatal`; se lee hasta `!done` (un `!trap` va seguido de `!done`). El socket
se crea sin conectar con `ethernet.socketFactory.createSocket()` y luego
`connect(InetSocketAddress(host, port), 5000)` (atado a Ethernet), lectura 5 s. **Una sesión
por operación** (login → comandos → cerrar), nunca una sesión abierta entre pruebas: si el
MikroTik se reinicia, la siguiente operación simplemente vuelve a entrar. Todas las
sesiones las abre el `probe-worker` (§2.1): nunca hay dos sesiones del celular a la vez.
Sin Ethernet: `reachable = false`, alerta `mikrotik-unreachable` (si se exige Ethernet) y
no se intenta. Las propiedades de bandera que vienen ausentes (`inactive`, `disabled`) se
leen como `false`.

Cambio de regla (pasos b–c y j de §2.3), en **una** sesión:

```
/login  =name=phone-probe  =password=<secreto>
        → !done  (!trap → error "mikrotik-login"; no se reintenta en ese ciclo)
/routing/rule/print  ?comment=phone-probe  =.proplist=.id,table,action,disabled,inactive
        → exactamente un !re (0 o >1 → error "mikrotik-rule-missing")
/routing/rule/set  =.id=<id leído>  =table=to-A  =action=lookup-only-in-table  =disabled=no
        → !done
/routing/rule/print  ?comment=phone-probe  =.proplist=.id,table,action,disabled,inactive
        → confirmar table=to-A, action=lookup-only-in-table, disabled=false
          (inactive solo se registra)
/ip/route/print  ?routing-table=to-A  ?dst-address=0.0.0.0/0  =.proplist=gateway,active,disabled
        → gateway "192.168.1.1%ether2" → gateway_ip = lo anterior a "%", gateway_iface = lo
          posterior; active → mikrotik_route_active
          (sin !re o active=false: la prueba sigue; es la caída de ese router y se registra)
```

Relectura al final de la prueba (paso h de §2.3), en una sesión corta aparte (login +
`/routing/rule/print ?comment=phone-probe`, igual que arriba) → `mikrotik_rule_end`. Si
falla la sesión, `null`; no se reintenta.

El `.id` (`*3`, etc.) se lee **siempre** en la misma sesión; nunca se guarda entre
sesiones (cambia si alguien recrea la regla).

Limpiar conntrack (solo al forzar `to-A`/`to-B`, si `flush_conntrack`), en la misma sesión:

```
/ip/firewall/connection/print  =.proplist=.id,src-address,dst-address
        → filtrar en el celular: src-address empieza con "<IP del celular>:" y
          dst-address NO empieza con "192.168.89.1:" (no cortar la propia sesión de la API)
/ip/firewall/connection/remove  =.id=*A1,*B7,...        (lotes de hasta 50 ids)
        → !done (un !trap "no such item" = ya expiró: se ignora)
```

La API clásica no filtra por prefijo (`?` solo compara igualdad), por eso se filtra en el
celular; si la tabla trae más de 2000 entradas, se omite la limpieza y se anota un evento
`warn`. La limpieza es de mejor esfuerzo: si falla, la prueba sigue (las conexiones de la
prueba son nuevas de todos modos).

"Probar MikroTik" y la lectura en reposo (cada `mikrotik_idle_check_s`) hacen: login,
`/routing/rule/print` (arriba), `/ip/route/print ?dst-address=0.0.0.0/0
=.proplist=routing-table,gateway,active,distance` (llena `mikrotik.routes`: una entrada por
tabla de equipo y, para `main`, la ruta con `active=true`), y, en reposo, si la regla no
está en `main`, el `set` a `main` de §2.3 paso 1. Solo en "Probar
MikroTik" además `/system/identity/print` y `/system/resource/print` (`version`). Cada
login deja una línea en el log **en memoria** del MikroTik (no escribe la flash): a 1 por
minuto es aceptable.

---

## 4. Dashboard (JS estático)

Se valida contra los ejemplos JSON de este contrato mientras el backend se construye.

1. **Tarjeta de sonda con `runner: "phone"`** (sección "Sondas"): además de lo de hoy,
   un panel "Celular" con lo de `phone_status` (refresco cada 5 s mientras la lista esté a
   la vista y haya una sonda de celular): en línea/sin conexión con la edad, fase actual en
   español, orden en curso (router, tabla, motivo), MikroTik (alcanzable, tabla actual como
   "Respaldo"/"Forzada a A"/"Forzada a B"), GPS (FRESCO/VIEJO/SIN FIJO con edad), cola
   (órdenes pendientes, resultados por subir), batería, IP de Ethernet/WiFi, camino al
   backend y versión de la app, más las `alerts` del celular arriba del panel (mismos
   textos `msg`, color según `level`) y el estado de cada router según `mikrotik.routes`
   (mismos textos que §2.11: enlace arriba/caído y por dónde sale el respaldo). Nombres de fase: los
   mismos de §2.11. Si `age_s > 60`: "el celular no reporta hace N min" (y el resto del
   panel en gris: es el último estado conocido, no el actual).
2. **Controles** (solo sondas de celular): "Prueba en A", "Prueba en B", "Siguiente
   disponible", "Secuencia de N alternadas" (N y separación en minutos), "Programar"
   (fecha/hora de inicio y "vence a las"), casilla "si no tiene datos, medir el otro"
   (`allow_fallback`). Todos van a `POST /api/v1/probes/{id}/orders` con un `order_id`
   generado en el navegador **una vez por clic** y reutilizado si se reintenta ese mismo
   envío, para que un doble clic o un reintento no dupliquen. **No** usar solo
   `crypto.randomUUID()`: existe solo en contextos seguros (HTTPS o `localhost`) y el
   dashboard local se abre también como `http://192.168.40.22:8080` desde otro equipo, donde
   es `undefined`. Usar `crypto.randomUUID` si existe y, si no, armar un UUID v4 con
   `crypto.getRandomValues(new Uint8Array(16))` (que sí existe en `http://`), fijando los
   bits de versión (`b[6] = b[6] & 0x0f | 0x40`) y variante (`b[8] = b[8] & 0x3f | 0x80`).
   "Programar" usa `<input type="datetime-local">`, que da hora **local sin zona**: se
   convierte con `new Date(valor).toISOString()` y se recortan los milisegundos
   (`...:05Z`) antes de mandarla. Un `409` al crear se muestra tal cual ("order_id en uso")
   y genera un id nuevo para el siguiente clic. Botón "Cancelar pendientes" (`POST .../orders/cancel` con `all_open: true`,
   con confirmación) y, en cada orden `pending`/`delivered` de la lista, "Cancelar".
3. **Órdenes** (`GET .../orders?view=history&limit=20`): hora, objetivo, motivo, estado
   con insignia (`pending` "En cola", `delivered` "Recibida por el celular", `running`
   "Midiendo", `done` "Completada", `failed` "Fallida", `expired` "Vencida",
   `interrupted` "Interrumpida", `cancelled` "Cancelada"), error. Si `closed_by = server`:
   "(cerrada por el servidor)". Refresco cada 10 s mientras la tarjeta esté a la vista.
4. **Resultados** (`GET /api/v1/measurements?probe_id=...&limit=20`): hora de la prueba
   (`test_started_at`), router (slot + etiqueta), bajada/subida/ping/pérdida, insignias
   "PRUEBA COMPLETADA/FALLIDA", "GPS FRESCO/VIEJO/SIN FIJO" con `gps_age_s`, y si
   `executed_offline`: "hecha sin Internet, sincronizada a las HH:MM" (con `_received_at`).
   Veredicto de red con los textos nuevos de `reason`:
   `phone-wifi` "por WiFi, no se atribuye a ningún router", `probe-route-unconfirmed`
   "no se confirmó la ruta en el MikroTik", `probe-asn-match` "salió por el operador
   esperado", `probe-asn-other-router` "salió por el OTRO router", `probe-asn-mismatch`
   "el operador de salida no coincide", `probe-asn-ambiguous` "ruta confirmada; los dos
   routers son del mismo operador", `network-changed-mid-test` "la IP de salida cambió
   durante la prueba", `probe-route-changed` "la regla del MikroTik cambió durante la
   prueba: no se atribuye", `probe-table-confirmed` "ruta confirmada en el MikroTik (sin
   verificar operador)". Si `other_traffic_bytes` > 5 MB: aviso "otras apps usaron la red
   durante la prueba". Si `captive_portal = true`: insignia "PORTAL CAUTIVO" con
   `portal_host` (la SIM del router pide registro o saldo; no es una caída de señal).
5. **Editor de sonda:** selector "Quién mide" (`probe` = "Script del MikroTik", `phone` =
   "Celular por cable") y, por equipo, `slot` y `expected_asn`. **`probePayload` (que usan
   el editor y el selector de intervalo) debe mandar siempre `runner` y, por equipo,
   `slot` y `expected_asn`** con los valores actuales; el backend los conserva si faltan
   (§1.2), pero el dashboard no debe depender de eso.
6. "Prueba vía sonda" del detalle del equipo sigue igual; al sondear el comando trata
   `delivered`/`running` como "midiendo" y `expired`/`interrupted`/`cancelled` como
   finales. En la tabla de comandos del detalle, una orden de celular se muestra "vía
   celular <probe_id> (<routing_table>)".

---

## 5. Despliegue local y pruebas

### 5.1 Backend local

```bash
cd /home/ubudev/notion-5g/server
go test ./...
docker compose -p notion5g-local up -d --build server
curl -s http://localhost:8080/healthz
docker logs notion5g-local-server-1 2>&1 | tail   # sin "no such column"
KEY=$(grep '^API_KEY=' .env | cut -d= -f2)
curl -s -X PUT -H "X-API-Key: $KEY" http://localhost:8080/api/v1/probes/hap-oficina -d '{
  "label":"hAP ac2 oficina","runner":"phone","interval_s":0,"duration_s":10,
  "targets":[{"slot":"A","device_id":"router-R524260829000001","routing_table":"to-A","label":"Notion 5G (Tigo)"},
             {"slot":"B","device_id":"router-R023-4g","routing_table":"to-B","label":"Notion 4G (Movistar)","expected_asn":3816}]}'
```

Dashboard: `http://localhost:8080/`. Pruebas de humo con curl (sin celular): crear orden A
dos veces con el mismo `order_id`; `GET orders?horizon=6h`; `ack`; `state running`; subir
un resultado dos veces (`inserted`, `duplicate`); crear una secuencia y cancelarla; `POST
status` con el ejemplo de §2.5 y ver el panel del dashboard; cambiar el intervalo de la
sonda desde la tarjeta y comprobar que sigue `runner: "phone"`. Nunca `docker compose down
-v` (la base local vive en el volumen `notion5g-local_notion5g_data`).

### 5.2 App

```bash
rsync -a --delete --exclude build/ --exclude .dart_tool/ --exclude android/.gradle \
  /home/ubudev/notion-5g/mobile/ /mnt/c/dev/notion5g_mobile/
powershell.exe -NoProfile -Command "cd C:\dev\notion5g_mobile; C:\development\flutter\bin\flutter.bat build apk --release"
cd / && /mnt/c/Users/diego/AppData/Local/Android/Sdk/platform-tools/adb.exe -P 5039 install -r \
  'C:\dev\notion5g_mobile\build\app\outputs\flutter-apk\app-release.apk'
```

(`install -r` conserva datos y permisos. Comandos de shell con tuberías: escribirlos en un
`.sh` y pasarlo por stdin a `adb -P 5039 shell`.)

### 5.3 Sin MikroTik (modo WiFi) — se puede hacer ya

1. Tras instalar: el modo en movimiento sigue corriendo (su estado en la pantalla de
   inicio muestra un envío reciente) — **no debe haber regresión**.
2. Ajustes de la sonda: backend `http://192.168.40.22:8080`, API key **del backend local**
   (`API_KEY` de `server/.env`), `hap-oficina`, **"Exigir Ethernet" apagado**, destino
   `cloudflare`. "Probar backend" → OK con `probe_found: true`, `runner: phone`. Activar
   sonda. Si la app pide quitar la optimización de batería, aceptarlo.
3. En el dashboard aparece el panel del celular en línea con `control_path: wifi`,
   MikroTik no alcanzable, GPS con estado.
4. "Medir por WiFi" en la app → fases visibles en la pantalla y en el dashboard; resultado
   con `net_path: wifi`, `device_id` = `phone-...`, veredicto `phone-wifi`, subido.
5. "Prueba en A" desde el dashboard → la orden pasa `pending → delivered → running → done`;
   el resultado queda **sin atribuir** (`phone-wifi`).
6. Con "Exigir Ethernet" encendido: la orden queda "Esperando cable" y vence (`expired`).
7. Sin backend: `docker stop notion5g-local-server-1`, `offline_after_s = 120`,
   `local_plan_interval_s = 120` → la app muestra plan local y acumula resultados
   pendientes; `docker start ...` → se suben con `executed_offline: true`, sin duplicados.
8. Interrupción: durante una descarga, `am force-stop com.h3s.notion5g.notion5g_field` y
   reabrir → la orden queda `interrupted` en la app y en el dashboard, y no se repite. (Tras
   `force-stop` Android no relanza servicios solos: el modo en movimiento vuelve al abrir
   la app o con su watchdog; comprobarlo.)
9. Cancelación: "Secuencia de 4 alternadas" cada 5 min, esperar a que el celular las
   reciba (`delivered`), "Cancelar pendientes" → en ≤ `poll_interval_s` desaparecen de la
   cola de la app y no se ejecutan.
10. Nunca borrar los datos de la app (`pm clear`) para probar: `probe.db` son datos de
    campo.

### 5.4 Con MikroTik

1. El PC ya está en `ether5` (ifIndex 82). `apply_probe.py --password admin` (dry-run) →
   revisar el plan con el líder/usuario (debe mostrar los gateways leídos del DHCP de cada
   WAN, no supuestos). Luego `--apply` → confirmar el respaldo en
   `C:\Users\diego\mikrotik-probe\backups\`. Correr `--dry-run` otra vez: **cero cambios**.
2. **No se recablea** (A = Notion 5G en `ether2`, B = Notion 4G en `ether1`); solo se
   conecta el celular a `ether3`.
3. App: contraseña de `phone-probe`, "Exigir Ethernet" encendido. "Probar MikroTik" → OK,
   tabla `main`, `routes` con `to-A` (`…%ether2`) y `to-B` (`…%ether1`) activas y `main`
   saliendo por A. Recomendado: adaptador USB-C con Ethernet
   **y carga (PD)**, porque con el servicio activo el teléfono sin cargar se descarga en
   pocas horas; y en el teléfono apagar actualizaciones automáticas de Play Store y copias
   de seguridad en la nube (con el cable, Ethernet es la red por defecto y ese tráfico
   ensuciaría la medición; se ve en `other_traffic_bytes`).
4. "Medir A" y "Medir B": en el registro se ve la regla cambiar y confirmarse; las IP de
   salida (y normalmente el ASN) son distintas entre A y B; al final la regla vuelve a
   `main`. Si A y B dan la misma IP de salida, algo no está forzando la ruta: revisar el
   orden de las reglas y el conntrack antes de seguir.
5. Poner en `expected_asn` de cada equipo el ASN observado; nuevas pruebas →
   `probe-asn-match`.
6. Desconectar el cable de `ether2` (Notion 5G) y pedir "Prueba en A" → resultado
   `failed` (`gw_reachable: false`, pérdida 100 %, `mikrotik_route_active: false`); el
   celular sigue hablando con el backend por WiFi; `routes.main` pasa a salir por B. Con
   `allow_fallback` → además mide B con `fallback:A-sin-datos`.
7. Desconectar el WiFi del celular → el plano de control no llega al backend local (es
   privado): plan local; al volver el WiFi, todo se sube.
8. Vigilante: con la sonda **detenida** en la app, forzar a mano la regla a `to-B` en
   WinBox → a los ~10 min vuelve sola a `main` (línea `phone-probe: regla en to-B por 10
   min` en `/log`). Con la sonda activa, en cambio, el celular la devuelve en ≤
   `mikrotik_idle_check_s`.
9. Regla cambiada a mitad: durante una descarga forzada a A, poner a mano la regla en
   `main` en WinBox → el resultado sube con `mikrotik_rule_end.table = main` y el servidor
   lo marca `probe-route-changed`, sin atribuirlo a A.
10. Portal cautivo (si alguna SIM lo vuelve a dar): el resultado de ese slot sale `failed`
    con `captive_portal: true`, `portal_host` y `error: "portal-cautivo"`, y la app muestra
    la alerta `captive-portal`.

## Desviaciones

### MikroTik (`scripts/mikrotik/`, 2026-09-29)

- **`/ping =routing-table=` no existe en la API de RouterOS 7.6** (`!trap` "unknown parameter
  routing-table"). Para ver la salida por cada tabla, `apply_probe.py --verify` (y el final de
  `--apply`) crea reglas temporales `comment=probe:tmp-verify` (`dst-address=1.0.0.1/32 →
  to-A`, `1.1.1.1/32 → to-B`, solo afectan al tráfico del propio MikroTik: la regla del
  celular va antes y termina la búsqueda), hace `/ping` y `/tool/fetch
  https://<ip>/cdn-cgi/trace` y las borra en un `finally`; `--dry-run`/`--apply` borran
  cualquiera que haya quedado. `--no-egress-test` lo omite. Además se hace el ping a cada
  gateway por su interfaz, como pide §3.4.
- **Modos extra** (no cambian nada del contrato): `--backup-only`, `--switch A|B|fallback`
  (mueve la regla `phone-probe` a mano), `--no-egress-test`, `--backup-dir`, `--bridge`,
  `--api-address` (sigue negándose sin `fe80::/10`), `--verbose`.
- Red DHCP de administración: en 7.6 `/ip/dhcp-server/network/unset =value-name=gateway`
  da `!trap`; se usa `gateway=""` y `dns-server=""`. La propiedad `dns-none` no existe en 7.6
  (no sale en `print`), así que no se usa; sin gateway, un DNS entregado al PC no le da ruta
  a Internet.
- El DHCP `probe-phone` figura `invalid=true` mientras `ether3` no tiene enlace (sin
  celular): es normal; `--verify` solo avisa si sigue inválido con enlace.
- Estado real: la configuración ya quedó aplicada el 2026-09-29 18:06 con el script
  provisional del líder (`mkt_setup.py`). Contra el equipo, `apply_probe.py --dry-run` da
  **cero cambios** y `--verify` da OK: A sale por 179.19.72.14 (BOG), B por
  186.102.123.189 (MDE, Movistar). El bridge ya tenía `auto-mac=no
  admin-mac=DC:2C:6E:F7:7F:E9`. La contraseña de `phone-probe` está en
  `C:\Users\diego\mikrotik-probe\phone-probe-password.txt`.
- **Vigilante (corrige §3.2), revisión adversaria 2026-09-29.** Probado en el equipo:
  (1) con `policy=read,write`, RouterOS 7.6 **no conserva las variables `:global` entre
  corridas del scheduler**, así que el contador del script de §3.2 nunca pasaba de 1 y el
  vigilante **nunca** devolvía la regla a `main`. Ahora va con `policy=read,write,policy,test`
  (con eso sí se conservan). Se comprobó además que un usuario del grupo `probe-api` no puede
  usar ese scheduler para escalar: RouterOS ignora su cambio de `on-event` y recorta la
  `policy` de lo que él cree. (2) El contador de §3.2 sumaba pruebas seguidas por la misma tabla
  (A → `main` → A entre dos vueltas de 1 min sin que el vigilante viera `main`) y podía cortar
  una prueba sana a los 10 min acumulados. Ahora el contador vuelve a 0 si la regla está en
  `main`, **si cambió de tabla** o **si hay un login nuevo de `phone-probe` en `/log`** (el
  celular entra al forzar, al releer, al restaurar y cada `mikrotik_idle_check_s`). O sea: 10
  min forzada **sin actividad del celular**. Mensaje en `/log`: `phone-probe: regla en to-X por
  10 min sin actividad del celular, vuelta a main`. Nada cambia para la app. Probado en vivo con
  un scheduler temporal de 2 s: con logins cada segundo la regla sigue forzada; sin ellos, vuelve
  a `main` en ~3 s. Aplicado con `--apply`: el primer `--apply` real, con respaldo
  `pre-probe-20260929-183509`. Después, el `--dry-run` da cero cambios.
- **Respaldos en `flash/`.** En el hAP ac2 la raíz de Files es RAM: los `.backup` que §3.4
  dejaba "en el router" se perdían al reiniciar. Ahora el export y el backup van a
  `flash/pre-probe-…`, y se vuelve atrás con `/system backup load name=flash/pre-probe-….backup`.
  Los tres respaldos anteriores (`-180510`, `-180606`, `-181959`) siguen en la raíz (RAM); sus
  copias están en `C:\Users\diego\mikrotik-probe\backups\`.
- La contraseña nueva de `phone-probe` **no se imprime** por defecto (solo se guarda en el
  archivo) para que no quede en registros de consola; `--show-password` la imprime, y se imprime
  siempre si no se pudo guardar. `--password` también se puede tomar de `MIKROTIK_PASSWORD`.
  `--apply` aborta si no puede leer un menú, en vez de tratarlo como vacío y crear duplicados.

### Backend (`server/internal/{store,api}`, 2026-09-29)

Nada de nombres, rutas, claves ni enumeraciones cambió. Decisiones donde el contrato no
decía nada, y agregados compatibles:

- **Ethernet con la regla confirmada, pero en una tabla que no es de ningún equipo de la
  sonda:** `device_id = measured_by`, `slot` NULL (como dice §1.4) y se clasifica
  `unknown/low/probe-route-unconfirmed` (no hay router al que atribuirla; no se inventó un
  `reason` nuevo).
- La tabla de clasificación se aplica literal, "primera regla que aplique": un resultado
  `failed` con `egress_ip` resuelto pasa igual por las filas de ASN (así un fallo que salió
  por el otro router queda `probe-asn-other-router`). Sin IP/ASN cae en
  `probe-table-confirmed`.
- `POST .../orders/{id}/state` con `running` sin `at`: se usa la hora del servidor como
  `started_at` (un `at` presente que no parsea sí es `400`).
- `POST .../orders`: sin `requested_by` se guarda `"dashboard"`. En una secuencia,
  `allow_fallback` vale para cada orden; con más de dos slots el equipo de respaldo es el
  siguiente slot activo (dando la vuelta). Con `sequence` y sin `first`, si `A` no está
  activo se empieza por el primer slot activo.
- `POST .../orders/cancel` con un `batch_id` que no tiene órdenes: `unknown: [batch_id]`.
- `GET .../orders?view=history` devuelve `{"server_time","orders","truncated"}`.
- `POST .../results` acepta además un único resultado sin envolver (objeto con
  `result_id`).
- `phone_status` y `GET .../status` llevan también `probe_id`; `age_s`/`received_at` van
  `null` si no hay estado.
- En `targets`, `expected_asn` sale siempre (`null` si no hay), no se omite.
- Al cambiar `runner` de `phone` a `probe` se cancelan `pending`, `delivered` **y**
  `running` (literal "órdenes abiertas"); un resultado posterior de la que estaba `running`
  se guarda igual con `order_linked: false`.
- Las horas de órdenes se comparan en SQL como texto **solo** entre valores que escribió
  el backend normalizados (`FormatTS`: UTC, `Z`, sin fracciones, ancho fijo), lo que
  equivale a comparar `time.Time`; lo que manda el celular se parsea y normaliza antes de
  guardarse (también `test_started_at`, `test_finished_at` y `gps_ts` dentro de `raw`).
- Segunda barrera en `POST .../status`: se borran del cuerpo (a cualquier nivel) las claves
  `api_key`, `prod_api_key` y `mikrotik_password` antes de guardarlo.
- `CompleteCommand` se serializa con un mutex del proceso (chequeo de "ya cerrado" +
  `INSERT` no son atómicos en SQLite sin transacción).
- Sondas creadas antes de `slot`: la migración les asigna letra (la de su posición o la
  primera libre) solo donde `slot` es NULL.

### App (`mobile/`, 2026-09-29)

Nada de nombres, rutas, claves JSON ni enumeraciones cambió. Aclaraciones de lo que el
contrato dejaba abierto:

- **Archivos Kotlin extra** (solo organización): `ProbeWorker.kt` (hilo `probe-worker` y
  `MikrotikOps`), `ProbeControl.kt` (hilo `probe-control`), `ProbeChannels.kt` (canales de
  §2.10) y `ProbeUtil.kt` (RFC 3339, UUID, JSON). `ProbeWatchdog` vive en `ProbeService.kt`.
- **Reintento de una orden que no se consumió:** además de los 3 intentos con 2 s del paso
  b, la orden espera 30 s antes de volver a intentarse (5 s si espera el cable), para no
  loguearse en el MikroTik cada 5 s. Tras un fallo de ruta solo se alerta
  `route-not-restored` si la regla llegó a cambiarse (si el login falló, no).
- **Plan local:** "sin backend" se cuenta desde `max(last_backend_ok_ms, arranque del
  servicio)`, para que al arrancar (o la primera vez, sin contacto previo) el control tenga
  `offline_after_s` para llegar al backend antes de generar órdenes locales.
- **Sin Ethernet ni WiFi** con "Exigir Ethernet" apagado: la orden no se consume y queda
  con `error = "sin-wifi"` (valor nuevo solo en la base local; si vence, sube como
  `expired` con `error = "vencida"`).
- **`closed` con `status = interrupted`** (si el servidor lo mandara) también se aplica
  localmente, igual que `cancelled`/`expired`.
- **`runNow`** con un `target` que no es `[A-Z]`, `next` ni `wifi` responde el error
  `invalid_target`.
- **`recentResults`** devuelve, además de las claves de §2.10, `test_started_at`,
  `loss_pct`, `egress_ip`, `egress_asn_client`, `egress_as_org_client`, `captive_portal`,
  `portal_host`, `target_host` y `sync_error` (para la pantalla).
- **Estado `gps`** antes de la primera prueba del servicio: se arma con el último fix de
  `gps_fixes` (o `unavailable`).
- **Alerta `no-wifi`**: solo con la sonda activa.
- **Revisión adversaria del backend (2026-09-29):** los secretos (`api_key`, `prod_api_key`,
  `mikrotik_password` y cualquier `password` anidado) también se borran de cada resultado
  antes de guardarlo, no solo del estado en vivo. En la base local, el equipo A de
  `hap-oficina` quedó con `expected_asn = 271773` (WOM, confirmado por Team Cymru para
  179.19.72.14) y etiqueta "Notion 5G (WOM)"; antes estaba sin ASN esperado. `server/.env`
  (API key local) queda fuera de git con `server/.gitignore`.
