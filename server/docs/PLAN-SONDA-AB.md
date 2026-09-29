# Plan: un celular que mide por dos routers (A/B) a través de un MikroTik, con modo sin Internet

Estado: **propuesta, nada implementado** (2026-09-25, versión 3). Las referencias
`archivo:línea` son del código de ese día.

## 0. Decisiones de Diego (2026-09-25)

- **A y B son dos routers bajo prueba**: el Notion 5G y un segundo router que se va a
  conseguir. El MikroTik existe justamente para tener los dos.
- **Hay dos formas de medir, y conviven:**
  - **Forma A (nueva):** un **celular conectado por cable** al MikroTik mide la
    velocidad. El MikroTik le garantiza por cuál router sale (A o B).
  - **Forma B (la actual):** el agente del router mide contra el servidor
    (`/api/v1/speedtest/*`) y el backend guarda el resultado. Sigue igual.
- El MikroTik sale a Internet por los mismos routers que se prueban.
- **No se activa nada más en el Notion** (ni NTP ni cola en el agente), para no afectar
  el desempeño que se mide. El agente que ya está instalado sigue igual.
- El MikroTik **sincroniza la hora por NTP al arrancar**.

## 1. Cómo funciona hoy

| Pieza | Qué hace hoy | Dónde |
|---|---|---|
| Agente del router (Go, cron cada 1 min) | Si hay comando `run_speedtest`: mide descarga/subida contra `/api/v1/speedtest/*` del backend, ping y señal (ubus), y cierra el comando. Si no: heartbeat con señal + ping a 8.8.8.8. | `server/cmd/routeragent/main.go:224-313` |
| Cola de comandos | Tabla `commands`, el ejecutor hace polling (`/commands/next?device_id=` o `?probe_id=`). Un comando tomado y no cerrado en 3 min se puede volver a tomar. | `store.go:87-103`, `:1298-1357` |
| Sonda MikroTik (solo backend) | Tablas `probes`/`probe_targets`, una tabla de ruteo por equipo, un ciclo que encola una prueba por equipo en orden (A, B, A, B…), cada `interval_s`, solo si la sonda está en línea y terminó el ciclo anterior. | `store/probes.go:34-53`, `:278-369`; `api/probes.go:16-33` |
| Dashboard | Lista de equipos (en línea/sin señal, ping, ubicación, velocidad), detalle con mapa, heartbeats, batería del celular y comandos; sección de sondas con "Ciclo ahora" y "Prueba vía sonda". | `server/internal/web/static/` |
| App del celular | Modo en movimiento: manda la ubicación cada vez que se mueve 50 m, o cada 2 min si está quieto. Desde hoy lo corre un **servicio nativo** que vuelve solo si Android mata la app o se reinicia el teléfono. | `mobile/.../BeaconService.kt` |
| Ubicación en el backend | Solo guarda **la última** posición por equipo (`device_locations`) y la pega a cualquier heartbeat o medición que llegue sin lat/lon, **si tiene menos de 10 min** (`maxLocationAge`). No guarda la hora del fix ni su edad, ni marca si es vieja. | `store.go:129-136`, `:313`, `:418-436` |

### Lo que ya se puede reutilizar

- Probes, targets, tabla de ruteo por equipo y el ciclo que alterna A/B: ya resuelven
  "prueba en A", "prueba en B" y "alternar" **mientras hay Internet**.
- `POST /measurements` ya acepta un arreglo: sirve de base para subir lotes.
- El campo `ts` se guarda tal como lo manda el cliente, separado de `received_at`: ya
  se distingue la hora de la prueba de la hora en que llegó.
- La unión de ubicación a la medición (`mergeIntoRaw`, `cachedLocation`).
- El servicio nativo de la app, que es donde va a vivir el registro de GPS.

### Lo que falta (confirmado en el código)

1. **Nada funciona sin Internet.** Ni el agente, ni la app, ni el script de la sonda
   guardan resultados para subirlos después. Si el POST falla, el dato se pierde
   (`routeragent/main.go:90-93`: si no puede consultar la cola, ni siquiera manda el
   heartbeat).
2. **No hay idempotencia.** Los comandos no tienen un id generado por el cliente.
   `CompleteCommand` no revisa el estado actual: un cierre repetido inserta **dos
   mediciones** (`store.go:1366-1424`). `test_id` solo vive dentro del JSON crudo.
3. **Los comandos no vencen.** No hay `execute_at` ni `expires_at`: un comando
   pendiente espera para siempre.
4. **La ubicación no dice qué tan vieja es.** Se reutiliza hasta 10 min sin marcarla,
   y la de un comando es la de cuando se creó, aunque haya esperado en la cola.
5. **No hay "por qué se eligió este equipo"** más allá de `requested_by`.
6. **No existe el script RouterOS** de la sonda.
7. **Detalle de relojes:** el agente manda `ts` en hora local sin zona horaria
   (`main.go:413`), y en el camino de error en UTC (`:227`).

## 2. Arquitectura

```
                 Backend / Web  (notion.h3s-iot.com)
                    ▲                     ▲
                    │ SIM del operador 1  │ SIM del operador 2
             ┌──────┴──────┐       ┌──────┴──────┐
             │ Router A    │       │ Router B    │   equipos bajo prueba
             │ (Notion 5G) │       │ (2.º router)│   (forma B: agente propio)
             └──────┬──────┘       └──────┬──────┘
             ether1 │ tabla to-A    tabla to-B │ ether2
                    └────────┬────────────────┘
                      ┌──────┴──────┐
                      │  MikroTik   │  decide por cuál sale el celular
                      └──────┬──────┘
                             │ ether3, cable USB-Ethernet
                      ┌──────┴──────┐
                      │  Celular    │  mide, GPS propio, cola local
                      └─────────────┘
```

### Cómo el MikroTik garantiza por cuál router sale

- Cada router tiene su tabla de ruteo (`to-A`, `to-B`). Esto ya está modelado en el
  backend como `probe_targets.routing_table`.
- Una regla `/routing rule` con `src-address=<IP fija del celular>` manda todo su
  tráfico a **una sola tabla**. Sin respaldo automático: si ese router no tiene datos,
  la prueba falla. Así se evita medir el router equivocado sin darse cuenta.
- **Quién cambia la regla: el celular**, por la API REST de RouterOS v7 (`/rest`, dentro
  de la LAN, con un usuario propio del MikroTik). Por cada prueba:
  1. Poner la regla en la tabla del router que toca, **leerla de vuelta** y confirmar.
  2. Medir.
  3. Devolver la regla a modo "respaldo" (sale por el router que tenga datos) para
     subir resultados y consultar órdenes.

  Así el cambio de router y la prueba los hace el mismo programa, en orden, sin carreras.
  Si fuera un script del MikroTik con su propio horario, el cambio podría caer a mitad
  de una prueba y quedaría la duda de qué router se midió.
- **Doble verificación de qué router se midió:** el backend ya ve la IP pública y la
  clasifica por ASN (`netclass.go`). Cada resultado guarda la tabla que el celular
  configuró **y** el ASN por el que salió. Si no coinciden con el operador esperado de
  ese router, la medición queda marcada. Esto cubre "no confundir A con B".

### Lo que hace falta construir en la app (hoy no mide nada)

- Un ejecutor de pruebas dentro del servicio nativo que ya sobrevive a que Android mate
  la app (`BeaconService`): descarga y subida contra `/api/v1/speedtest/*` (ya existen
  en el backend) y ping. Atado a la red Ethernet (`Network.bindSocket`) para que Android
  no lo mande por WiFi ni por datos móviles.
- Un cliente de la API REST del MikroTik para cambiar y leer la regla.
- Una base local (SQLite) para órdenes, resultados y fixes de GPS.
- Opcional: leer la señal del Notion en el momento de la prueba por su API HTTP
  (`xml_action.cgi`, ver `scripts/modem_http_api.py`). Es solo lectura, no instala nada
  en el router. Nunca se reintenta la contraseña automáticamente, porque el router
  bloquea el login tras varios fallos. El agente ya manda la señal cada minuto cuando hay
  Internet.

### El segundo router

- Si admite el agente (es OpenWrt/ARM como el Notion), también se le puede instalar: así
  mide con la forma B y manda su señal.
- Si no lo admite, solo se mide con la forma A (el celular), y la señal se toma de lo que
  ese router exponga.

## 3. Dónde se guardan los resultados: en el celular

- RouterOS **no puede recibir** resultados: no tiene un servidor HTTP donde el celular
  pueda hacer POST, y su almacenamiento es limitado (16 MB de flash). Por eso el celular
  guarda todo en su SQLite, con reloj confiable (GPS y red celular), y lo sube en cuanto
  hay salida.
- **Con dos routers, "sin Internet" ya no es todo o nada.** Si A cae, el celular sube
  por B, y al revés. Solo queda sin salida si caen los dos a la vez; entonces espera.
- La forma B (agente del router) sigue sin cola: si su router no tiene datos, ese
  heartbeat o prueba se pierde. Agregarle cola sería cambiar lo instalado en el Notion,
  y lo descartaste. Queda anotado como limitación.

## 4. Turnos A/B y órdenes que sobreviven sin Internet

### Con Internet: manda el backend

Se reutiliza lo que ya existe para la sonda (`probes`, `probe_targets` con
`routing_table`, el ciclo que alterna, "Ciclo ahora", "Prueba vía sonda"). El ejecutor
ahora es el celular (`runner = "phone"`), en vez del script de RouterOS, que ya no hace
falta.

Se extiende `commands` (campos nuevos también en `addColumnIfMissing`):

| Campo | Para qué |
|---|---|
| `order_id` TEXT UNIQUE | UUID generado por quien crea la orden. Crear la misma orden dos veces devuelve la existente. |
| `target` | `A`, `B`, `next` (siguiente disponible) o `sequence`. |
| `preferred_device`, `fallback_device` | Si el router preferido no tiene datos al ejecutar, se usa el otro, y queda anotado. |
| `execute_at` | Cuándo ejecutar; vacío = ya. |
| `not_after` | Pasada esta hora **no se ejecuta**: se cierra como `expired`. |
| `selection_reason` | Por qué ese router: `requested`, `alternation`, `fallback:A-sin-datos`, `offline-schedule`. |
| Estados | `pending → delivered → running → done / failed / expired / interrupted`. |

- El celular consulta `GET /api/v1/probes/{id}/orders?horizon=6h`. Recibe las órdenes de
  las próximas horas, las guarda en su SQLite y confirma cuáles tiene (`delivered`).
- Una "secuencia de N alternadas" se convierte en N órdenes con `execute_at`
  escalonado, alternando A y B.
- "Siguiente disponible" = el router con datos que lleve más tiempo sin medirse.

### Sin salida al backend: plan local

- El celular guarda el plan (`interval_s`, orden A/B, duración) y, si pasa cierto tiempo
  sin llegar al backend, sigue alternando solo. Los resultados llevan
  `selection_reason = offline-schedule`.
- No hace falta lógica en el MikroTik. Si el MikroTik se reinicia sin hora, no importa:
  la hora la pone el celular.

### Idempotencia

- Cada resultado lleva `result_id` (UUID del celular) y, si viene de una orden, su
  `order_id`. El backend hace `INSERT … ON CONFLICT(result_id) DO NOTHING`, así que
  reenviar el mismo lote no duplica nada.
- `CompleteCommand` pasa a ignorar un segundo cierre, en vez de insertar otra medición
  como hoy.
- Antes de medir, el celular marca la orden `running`. Si Android lo mata a mitad de la
  prueba, al volver la cierra como `interrupted` y **no la repite**.
- Una orden con `not_after` vencida no se ejecuta.

## 5. Qué se mide cuando un router no tiene datos

- **No hay prueba de velocidad posible por ese router.** Se registra la caída: ping con
  100 % de pérdida, DNS fallido, puerta de enlace del router alcanzable o no, y la señal
  si se puede leer. Eso también es un resultado de homologación.
- Si solo falla el backend pero hay Internet, se mide contra un destino alterno (la
  descarga de Cloudflare que ya usa `notion5g.py`) y se guarda `target_host`.

## 6. GPS fresco y no reutilizado

### Qué da Android (verificable, sin suposiciones)

- `Location.getTime()`: hora UTC del fix.
- `Location.getElapsedRealtimeNanos()`: momento del fix en reloj **monótono**. Es la
  forma confiable de calcular la edad
  (`SystemClock.elapsedRealtimeNanos() - loc.elapsedRealtimeNanos`), porque no depende
  de que la hora del teléfono esté bien.
- `getAccuracy()`, `getProvider()` (`gps` o `fused`), `getSpeed()`.
- `GnssStatus` dice cuántos satélites se **usaron en el fix**: distingue un fix GNSS
  real de uno estimado por WiFi o celdas.
- `getLastKnownLocation()` devuelve la posición en caché. Nunca se usa sin calcular
  su edad.
- `LocationManager.getCurrentLocation()` (API 30+) pide un fix nuevo con tiempo
  límite. Con fused puede devolver uno reciente en caché, así que igual se valida la
  edad.
- **El GNSS funciona sin datos.** Los datos solo aceleran el primer fix (A-GNSS/SUPL y
  efemérides descargadas que duran días). Sin datos y en frío, el primer fix puede
  tardar de decenas de segundos a unos minutos, pero después funciona normal.

### Diseño

Como el celular que mide es el mismo que tiene el GPS, cada prueba toma su propio fix:

1. Pedir un fix nuevo (`getCurrentLocation`, tiempo límite configurable
   `location_fix_timeout_seconds = 20`).
2. Validar la edad con el reloj monótono (`elapsedRealtimeNanos`) y la precisión.
3. Medir.
4. Guardar con la prueba:
   `lat, lon, gps_accuracy_m, gps_ts, gps_age_s (= inicio de la prueba - gps_ts),
   location_source (gps/fused, satélites usados), location_status`.

`location_status`:
- `fresh`: edad ≤ `location_max_age_seconds` y precisión ≤ `location_max_accuracy_m`.
- `stale`: hay fix pero más viejo. **Se guarda, pero no se muestra como posición
  actual.**
- `unavailable`: no hubo fix dentro del tiempo límite.

**Valores iniciales propuestos, configurables:**
- `location_max_age_seconds = 30`. A 60 km/h, 30 s son unos 500 m, y la prueba dura
  10–20 s.
- `location_max_age_stationary_seconds = 300`, si la velocidad del fix es menor a
  1 m/s.
- `location_max_accuracy_m = 100`.

**Para la forma B** (el agente del router, que no tiene GPS): el backend cruza la hora de
la prueba con el **historial** de fixes del celular (no solo la última posición) y
aplica los mismos umbrales y estados. Se retira la regla actual que pega la última
ubicación si tiene menos de 10 min (`maxLocationAge`) sin marcarla.

## 7. Relojes

- Las horas de las pruebas de la forma A las pone el celular, que tiene hora confiable
  sin Internet. El NTP del MikroTik al arrancar alcanza para lo suyo; su reloj no entra
  en los resultados.
- La forma B usa la hora del router. El agente manda `ts` sin zona horaria
  (`main.go:413`); se corrige cuando se reinstale el agente por otra razón, porque
  también es tocar el Notion.

## 8. ¿SIM/eSIM en el celular?

- **No hace más fresco el GPS.** El GNSS funciona sin datos; la SIM solo acorta el
  primer fix (A-GNSS).
- **Con dos routers, casi no hace falta como respaldo:** el celular ya sube por el que
  tenga datos.
- **Tiene un riesgo:** que una prueba salga por la SIM del celular. Se evita atando la
  prueba a Ethernet y verificando el ASN, pero sin SIM ese riesgo no existe.
- Recomendación: **sin SIM de datos**, o con los datos móviles apagados.

| Situación | Qué pasa |
|---|---|
| GPS sí, un router sin datos | Se mide la caída de ese router con posición fresca; el otro router sigue y sirve para subir. |
| GPS sí, los dos sin datos | Pruebas de caída en el plan local; todo queda en SQLite hasta que vuelva uno. |
| Internet sí, GPS sin fix | La prueba corre con `location_status = unavailable`; no se inventa posición. |
| Se pierde el GPS un rato | Las pruebas de ese tramo quedan `stale` o `unavailable`, marcadas. |
| El MikroTik se reinicia | El celular reintenta la API REST; la prueba no corre hasta confirmar la regla. |

## 9. Trazabilidad y web

Cada medición queda con:
`result_id, order_id, probe_id (MikroTik), device_id (router A o B), measured_by
(phone-<id> o router-agent), routing_table, egress_asn, selection_reason,
test_started_at, test_finished_at, lat/lon/accuracy, gps_ts, gps_age_s, location_status,
location_source, executed_offline, received_at (= sincronizada)` y los números de la
prueba.

**Dashboard:**
- **Router A y router B** por separado (ya existe la lista por equipo): `ONLINE`/
  `OFFLINE`, última prueba de cada forma, última ubicación con su edad.
- **El celular:** en línea, batería (ya existe `phone_log`), IP local, estado del GPS,
  resultados pendientes de subir y a qué router apunta en este momento.
- Por prueba: `TEST RUNNING`/`TEST COMPLETED`, `GPS FRESH`/`STALE`/`UNAVAILABLE` con la
  edad, `SYNC PENDING`/`SYNCED`, y "hecha sin Internet, sincronizada a las HH:MM".
- Controles: "Prueba en A", "Prueba en B", "Siguiente disponible", "Secuencia de N
  alternadas", "Programar (vence a las …)", y el plan local.

## 10. Preguntas que quedan

1. ¿Te sirve que **el celular cambie la regla del MikroTik** por su API REST antes de cada
   prueba (recomendado), o prefieres que el MikroTik alterne solo con un script? Con el
   script, el cambio puede caer a mitad de una prueba.
2. **¿Qué modelo es el segundo router?** Define si se le puede poner el agente (forma B)
   y cómo leer su señal.
3. **¿Qué versión de RouterOS tiene el hAP ac2?** Hace falta v7 para la API REST.
4. ¿El celular va sin SIM de datos, o con los datos móviles apagados?

## 11. Orden de implementación propuesto

1. ✅ **Hecho:** el modo en movimiento es un servicio nativo que vuelve solo
   (verificado: tras reinstalar la app, retomó el envío en 19 s sin tocarla).
2. **Backend:** órdenes con `order_id`/`execute_at`/`not_after`/`selection_reason`,
   `runner = "phone"`, `result_id` único con subida por lotes idempotente,
   `CompleteCommand` idempotente, columnas de GPS con edad y cruce con el historial para
   la forma B. Se retira `maxLocationAge`. No depende de hardware.
3. **App:** SQLite, ejecutor de pruebas atado a Ethernet, fix fresco antes de medir,
   cliente REST del MikroTik, consulta de órdenes, plan local y subida por lotes.
4. **MikroTik:** tablas `to-A`/`to-B`, IP fija del celular, regla por `src-address`,
   usuario para la API REST, NTP al arrancar.
5. **Dashboard:** estados, controles y vista del celular.

El paso 2 se puede empezar ya. El 3 se puede avanzar con el S20+ y un solo router
(apuntando siempre a A) antes de tener el hAP y el segundo router.
