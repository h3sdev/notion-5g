# Plan — app móvil de pruebas de campo (Android / iOS)

> Escrito el **2026-09-19**, a pedido de Diego ("¿la app va a funcionar en Android y iOS, puede medir
> con la pantalla apagada y mostrar en la notificación en tiempo real qué está midiendo?").
> Todo lo que este documento afirma sobre el código de este repo fue verificado línea por línea (las
> referencias `archivo:línea` son reales). Todo lo que afirma sobre Android/iOS está marcado como
> verificado contra documentación oficial o como **confianza baja**. Si algo contradice al código,
> gana el código.

---

## Respuesta corta

**En iOS no se puede hacer lo que imaginás, y no es cuestión de presupuesto ni de esfuerzo: Apple no
expone la señal celular por ninguna API pública.** No hay RSRP, RSRQ, SINR, PCI, banda ni Cell ID.
`CTCarrier` y `serviceSubscriberCellularProviders` están deprecados desde iOS 16 y devuelven valores
ficticios desde iOS 16.4; lo único público es `serviceCurrentRadioAccessTechnology`, que dice `"LTE"`,
`"NRNSA"` o `"NR"` y nada más. Usar APIs privadas es rechazo automático en App Store (Guideline 2.5.1).
Una app iOS **sí** puede correr en segundo plano con la pantalla apagada (Core Location con
`CLServiceSession`) y **sí** puede mostrar un panel en vivo (Live Activity), pero midiendo solamente
GPS, velocidad y latencia — es decir, lo mismo que ya hace el dashboard web, más background — y el
panel en vivo lo termina el sistema a las 8 horas. Un drive-test de señal en iPhone no existe.

**En Android sí se puede, con límites conocidos.** Un Foreground Service declarado con tipo
`location` y arrancado con la app en pantalla mantiene el acceso a ubicación cuando el usuario apaga
la pantalla, conserva red sin restricciones durante Doze, y su notificación se puede reescribir en
vivo cada 15 s sin ningún problema de rate. Además no necesita `ACCESS_BACKGROUND_LOCATION`, que es
justamente el permiso que dispara la revisión dura de Google Play. Los límites reales: la sesión
**siempre** la tiene que arrancar Diego con la app abierta (no hay auto-arranque útil), con Ahorro de
batería activo algunos equipos dejan de entregar fixes aunque el servicio siga vivo, los wakelocks
cuentan contra Android vitals aunque estés en un foreground service, los fabricantes chinos matan
servicios legítimos, y **queda sin verificar contra documentación oficial** si `TelephonyManager`
entrega datos de celda *frescos* (no cacheados) con la pantalla apagada. Ese último punto es el que
decide si la app mide señal o solamente GPS.

**Pero la recomendación de este plan es no empezar por la app.** El mejor instrumento de radio ya va
en el auto: el router. Y hoy el backend tira ese dato a la basura. Verificado: `sendHeartbeat`
(`cmd/routeragent/main.go:231`) manda **5 de los 18 campos** que el propio agente ya leyó por `ubus`
(`readLocalStatus`, `cmd/routeragent/main.go:412-502`) — se pierden SINR, PCI, banda, EARFCN, ancho de
canal, CA, temperatura y estado de registro, un minuto sí y otro también; y `maxLocationAge = 10 *
time.Minute` (`internal/store/store.go:221`) pega ubicaciones de hasta diez minutos de antigüedad a
las muestras, tanto en `insertMeasurement` (`:374`) como en `InsertHeartbeat` (`:431`): a 60 km/h eso
es georreferenciar con hasta 10 km de error, en silencio. Y no existe tabla de traza: `device_locations`
tiene `device_id` como PRIMARY KEY (`internal/store/store.go:126`), o sea que solo se guarda la
**última** ubicación. Arreglar eso cuesta ~7 días-persona, no publica nada en ninguna tienda, y da un
drive-test de verdad. La app Android queda como **fase condicional**, decidida por un chequeo de diez
minutos contra el router.

---

## Qué permite cada plataforma

| Capacidad | Android | iOS |
|---|---|---|
| **Ciclo cada 15 s con la app en segundo plano** | SÍ, dentro de un Foreground Service, con un reloj propio (coroutine + `delay`). **No** con WorkManager: `MIN_PERIODIC_INTERVAL_MILLIS` = 15 **minutos** (verificado). **No** anclado al callback de ubicación: sin fix no hay callback. El jitter real con el SoC suspendido **hay que medirlo en el teléfono** (confianza baja) | APROXIMADO. El proceso sigue vivo por Core Location y un `DispatchSourceTimer` corre, pero el sistema no garantiza ninguna cadencia. `BGAppRefreshTask`/`BGProcessingTask` **no sirven** como motor: los agenda el sistema, `earliestBeginDate` es solo un piso, Low Power Mode los desactiva (verificado) |
| **Seguir midiendo con la pantalla apagada** | SÍ. Doc oficial, textual: la app conserva el acceso "when the user presses the Home button or turns their device's display off" mientras corra un FGS de tipo `location` (verificado). **Salvedad no opcional:** con Ahorro de batería activo hay que consultar `getLocationPowerSaverMode()`; si no devuelve `LOCATION_MODE_FOREGROUND_ONLY`, el servicio sigue vivo pero dejan de llegar fixes (verificado) | SÍ, con `CLServiceSession` (iOS 18+) / `CLBackgroundActivitySession` (iOS 17+) creadas **en foreground**, y `pausesLocationUpdatesAutomatically = false` — viene en `true` y, si el sistema pausa, **no reanuda solo** (verificado) |
| **Ubicación en segundo plano sin permiso "siempre"** | SÍ. Un FGS `location` arrancado con la app visible cuenta como uso en primer plano: alcanza `ACCESS_FINE_LOCATION` "mientras se usa" (verificado). Esto es lo que hace publicable la app | NO. Para background continuo hace falta autorización **Always** (`When In Use` no sostiene la sesión de manejo) |
| **Notificación persistente actualizable en vivo** | SÍ. Mismo `NotificationCompat.Builder`, mismo id, `setOngoing(true)` + `setOnlyAlertOnce(true)`, `setProgress()` para la barra del speedtest. **Dos salvedades:** desde Android 14 el usuario puede deslizarla para descartar (el servicio sigue corriendo), aunque sigue siendo no descartable con el teléfono bloqueado (verificado); y si niega `POST_NOTIFICATIONS`, el servicio corre pero la notificación no se ve (confianza media). El límite de ~10 updates/s **no está en documentación oficial de desarrollador** (confianza baja), pero el margen contra 1 update cada 15 s es enorme | LIMITADO. Solo Live Activity (ActivityKit), actualizable desde la propia app en background (verificado), con techo **duro de 8 h**; después el sistema la termina y queda en pantalla de bloqueo hasta 4 h más. Payload ≤ 4 KB |
| **Leer RSRP / RSRQ / SINR / banda / PCI** | SÍ, vía `registerTelephonyCallback` (API 31+; `listen()` está deprecado) + `CellInfoNr`/`CellIdentityNr.getBands()/getNci()/getNrarfcn()`. Requiere `ACCESS_FINE_LOCATION`. **Confianza baja en dos puntos:** que los datos lleguen *frescos* con la pantalla apagada (a prototipar) y qué campos NR completa cada chipset. `PhysicalChannelConfig` (ancho, desglose de CA) queda **descartado**: exige `READ_PRECISE_PHONE_STATE`, privilegiado | NO. Nunca. Ni RSRP, ni banda, ni PCI, ni Cell ID. Solo operador y una etiqueta gruesa de tecnología |
| **Prueba de velocidad en segundo plano** | SÍ, con el FGS corriendo la red no tiene restricciones en Doze (verificado). El CPU **no** queda despierto por el solo hecho de ser FGS: hay que tomar `PARTIAL_WAKE_LOCK` alrededor de la transferencia, con `acquire(timeout)` y `release()` en `finally` — y contarlo, porque Android vitals mide wakelocks **aunque estés en un FGS** (≥2 h por 24 h en >5 % de sesiones afecta la visibilidad en Play) | SÍ, pero **solo con `URLSession` normal**. `URLSession` background es inservible para medir: todas sus transferencias iniciadas en background son discrecionales por decisión del sistema y pueden correr por Wi-Fi horas después (verificado) |
| **Arranque automático (sin tocar el teléfono)** | NO, en la práctica. El servicio arranca (el tipo `location` no está en la lista bloqueada desde `BOOT_COMPLETED`), pero desde Android 14 un FGS arrancado desde background **se queda sin acceso a ubicación**: "Foreground service started from background can not have location/camera/microphone access". La única vía real es `ACCESS_BACKGROUND_LOCATION` + revisión dura de Play | NO. La `CLServiceSession` y la Live Activity se crean con la app en foreground |
| **Duración máxima de la sesión** | Sin timeout **hoy**: los 6 h por cada 24 h de Android 15 aplican a `dataSync` y `mediaProcessing`, y el contador es **por tipo** (verificado). Por eso el servicio declara **solo `location`**, nunca `location|dataSync`. Ojo: la doc dice "*currently*" — es una propiedad de la versión actual, no una garantía | 8 h para el panel en vivo (la medición puede seguir, el panel no) |

**Fuentes principales** (todas las filas marcadas "verificado" salen de acá):
`developer.android.com/develop/background-work/services/fgs/{service-types,timeout,restrictions-bg-start,changes}`,
`developer.android.com/develop/sensors-and-location/location/permissions`,
`developer.android.com/topic/performance/power/power-details`,
`developer.android.com/reference/androidx/work/PeriodicWorkRequest`,
`developer.android.com/about/versions/14/behavior-changes-all`,
`support.google.com/googleplay/android-developer/answer/13392821`,
`developer.apple.com/documentation/corelocation/handling-location-updates-in-the-background`,
`developer.apple.com/documentation/corelocation/cllocationmanager/pauseslocationupdatesautomatically`,
`developer.apple.com/documentation/foundation/urlsessionconfiguration/isdiscretionary`,
`developer.apple.com/documentation/activitykit/displaying-live-data-with-live-activities`,
`developer.apple.com/app-store/review/guidelines/`.

---

## Lo que NO se puede (y por qué)

1. **Medir señal celular en iOS.** No hay API pública, y no es un olvido: Apple DTS lo dice en sus
   foros ("no general-purpose API that returns Wi-Fi or cellular signal strength… it's safe to assume
   that it's not an accidental omission but a deliberate design choice",
   `developer.apple.com/forums/thread/721067`). `MXCellularConditionMetric` de MetricKit es un resumen
   posterior, no un valor en tiempo real. APIs privadas tipo `FTMInternal-4` = rechazo (Guideline 2.5.1).
2. **Una notificación en iOS que se reescriba indefinidamente.** Live Activity: 8 h y el sistema la
   termina. No existe el equivalente a la notificación de un foreground service.
3. **Que la medición arranque sola** (al encender el teléfono, al detectar movimiento, por push).
   Android 14 le quita el acceso a ubicación a cualquier FGS arrancado desde background sin
   `ACCESS_BACKGROUND_LOCATION`; iOS exige foreground para crear la sesión. No gastar ni un día
   probando FCM de alta prioridad, alarmas exactas o geofences: arrancan el servicio y devuelven
   mediciones **sin ubicación**.
4. **Usar WorkManager como motor del ciclo de 15 s.** Mínimo periódico 15 minutos, y desde Android 16
   los jobs lanzados desde un FGS consumen cuota. Sirve solo para el flush diferido de la cola.
5. **Medir velocidad con `URLSession` background en iOS.** Medirías el planificador de iOS, y podría
   correr por Wi-Fi.
6. **Pedir por API la exención de Doze** (`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`). Play lo
   prohíbe salvo una lista taxativa que no incluye drive-testing. Con el FGS corriendo no hace falta.
   Sí se puede ofrecer, opcional y no bloqueante, el deep link a `ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS`.
7. **Declarar el servicio como `location|dataSync`** creyendo que el tipo "bueno" protege al "malo".
   El límite de 6 h se lleva por tipo. Si hay que subir un lote grande al cerrar, va en un **segundo**
   servicio de vida corta o en un `OneTimeWorkRequest`.
8. **Garantizar la cadencia de 15 s como contrato.** Es un objetivo. El jitter real con el SoC
   suspendido hay que medirlo en el teléfono concreto antes de prometerlo en un informe de campo.
9. **Reutilizar la app Flutter vieja.** No está en esta máquina, y aunque estuviera: la cadena de
   telefonía hay que escribirla en Kotlin igual (no hay plugin mantenido en pub.dev sobre
   `registerTelephonyCallback`), y si iOS no mide señal, Flutter pierde su razón de ser.

---

## Arquitectura recomendada

**Tesis: el teléfono no es el medidor. Es el GPS, el testigo y el enlace.** El instrumento de radio
es el router, que no tiene Doze, ni Ahorro de batería, ni tipos de foreground service, ni revisión de
tienda. Eso reduce la superficie de riesgo de plataforma a casi cero y permite entregar valor de
campo en días, no en meses.

```
┌──────────────────────────────────────┐
│ ROUTER 5G (ASR1901) — MIDE SEÑAL     │ cron 1 min → agente con sub-bucle:
│ cmd/routeragent (ya instalado)       │ 4 muestras de 15 s por corrida y exit
│  · ubus cm/get_zcainfo, util_wan,    │ RSRP/RSRQ/SINR/PCI/banda/EARFCN/BW/
│    cm/get_link_context, sim          │ CA/temp_c/rat/eps_reg  (ya se leen)
│  · ping 8.8.8.8 (ya existe)          │
│  · speedtest bajo comando            │ cada 3-5 min, capado POR BYTES
│  · spool /data/*.ndjson      [NUEVO] │ buffer cuando no hay cobertura
└───────────────┬──────────────────────┘
                │ POST /api/v1/heartbeat  (X-API-Key / token por dispositivo)
                ▼
┌──────────────────────────────────────────────┐
│ BACKEND Go (VPS + Cloudflare Tunnel)         │
│  · tabla locations append-only       [NUEVO] │
│  · merge por timestamp MÁS CERCANO  [CAMBIO] │  ← mata maxLocationAge = 10 min
│  · tabla sessions + watchdog "stalled"[NUEVO]│
│  · GET /api/v1/live (texto ya armado)[NUEVO] │
│  · push a ntfy                       [NUEVO] │
└───────────────▲───────────────┬──────────────┘
                │               │ push
  POST /devices/{id}/location   ▼
┌───────────────┴──────┐   ┌──────────────────────────────┐
│ CELULAR — SOLO GPS   │   │ ntfy (app publicada, Android  │
│ app de terceros ya   │   │ + iOS): estado y alertas con  │
│ publicada (OwnTracks │   │ el texto que arma el backend  │
│ / GPSLogger): FGS    │   └──────────────────────────────┘
│ location, cola off-  │
│ line, 0 código nuestro│  ┌──────────────────────────────┐
│ + dashboard como PWA │   │ FASE 5 (condicional):         │
│   cuando hay pantalla│   │ app Android propia en Kotlin  │
└──────────────────────┘   └──────────────────────────────┘
```

**Quién mide qué:**

| Dato | Quién | Cómo llega |
|---|---|---|
| RSRP, RSRQ, SINR, RSSI, PCI, banda LTE, EARFCN, BW, CA, `temp_c`, `rat`, `eps_reg` | Router (`ubus`, ya se leen hoy) | `POST /api/v1/heartbeat` cada 15 s |
| Métricas NR reales (SS-RSRP, banda n78, PCI NR) | **Solo si `cm/get_zcainfo` trae bloque `nr`. Verificado: hoy el agente lee únicamente `zca["lte"]` (`cmd/routeragent/main.go:414`). Si no lo trae, hoy NADIE las mide en el auto** | gate de fase 0 |
| Velocidad, jitter, ping, pérdida | Router (`handleSpeedtest`, `pingHost`, ya existen) | cola de comandos, ya existe |
| **Ubicación con pantalla apagada** | App de terceros en el celular | `POST /api/v1/devices/{id}/location` |
| Estado de sesión y alertas | Backend → ntfy | push |

**Por qué el teléfono no debe medir señal en esta arquitectura:** sería otra SIM, otra antena y otro
módem que el equipo que se está homologando. El dato no sería comparable con el del router.

### La notificación en vivo: qué se logra y qué no

**Lo que NO se logra por esta vía, dicho de frente:** una sola notificación propia que se reescriba
cada 15 s con RSRP y velocidad. Eso exige foreground service propio (fase 5). En iOS no se logra ni
con app propia más allá de 8 h.

**Lo que sí se logra sin escribir una app:** (a) la notificación persistente de la app de logging
—prueba de vida, visible con la pantalla apagada y el teléfono bloqueado— y (b) avisos de ntfy con el
texto armado por el backend en `GET /api/v1/live`. Texto exacto por estado:

**MIDIENDO** (cada 2 min, prioridad baja, sin sonido):
```
Notion 5G · drive-test activo · 01:12:40
12:41:07 · 5G-NSA · B3 (PCI 137) · RSRP -92 dBm · SINR 14 dB
Bajada 47.3 · Subida 12.1 Mbps · Ping 24 ms · perdida 0%
GPS +-6 m (hace 4 s) · punto 312 · 18.4 km · cola 0 · router 41 C
```

**MIDIENDO VELOCIDAD** (mientras corre el speedtest, prioridad baja):
```
Notion 5G · midiendo velocidad
bajando... 8 s · 47.3 Mbps · 512 MB usados de 5 GB en la sesion
```

**ESPERANDO** (sesión viva, sin speedtest en curso; se muestra si pasaron >5 min sin prueba):
```
Notion 5G · drive-test activo · sin prueba de velocidad hace 4 min
Proxima prueba automatica 12:45 · señal y GPS siguen registrandose
punto 312 · 18.4 km · cola 0
```

**SIN COBERTURA** (el backend lo detecta por ausencia de heartbeats; el aviso llega recién cuando
vuelve la red, y lo dice):
```
Notion 5G · se recupero la señal
Estuvimos sin backhaul 3 min 12 s (12:38:04 - 12:41:16).
El router guardo 12 muestras en su spool y ya las envio. Cola 0.
```

**SIN GPS** (prioridad alta, con sonido — esto es lo que importa manejando):
```
AVISO · Notion 5G se quedo sin GPS
Sin ubicacion hace 78 s. Las muestras se estan guardando SIN coordenadas.
Revisa que la app de ubicacion siga viva en el celular.
```

**BATERÍA BAJA / SESIÓN CAÍDA** (prioridad alta, con sonido):
```
AVISO · el celular esta en 14% de bateria
Con Ahorro de bateria activo puede dejar de mandar ubicacion aunque la app
siga abierta. Enchufa el telefono.
```
```
AVISO · el router dejo de reportar
Ultima muestra 12:39:41 (hace 2 min 14 s). Revisa alimentacion del router.
```

Notas honestas sobre estos textos: el nivel de batería del celular depende de que la app de terceros
lo reporte (OwnTracks manda `batt` en su payload; **a verificar en el prototipo de fase 0**, no está
confirmado). Y la detección de "sin cobertura" es necesariamente retroactiva: si no hay red, no hay
push. Por eso la alerta audible de sesión caída también tiene que existir del lado del celular (ntfy
la dispara cuando vuelve la conectividad, y el spool del router garantiza que el dato no se perdió).

**Descartado explícitamente:** usar Tasker para reescribir una notificación propia poleando
`/api/v1/live`. Funcionaría en teoría, pero es configuración no versionable, depende del propio FGS
de Tasker y de su exención de batería, y **no está verificado contra documentación su comportamiento
en Android 15/16**. Si el requisito literal pesa, la respuesta correcta es la fase 5, no un truco.

---

## Cambios necesarios en el backend

Todos en `/home/h3s/notion-5g-server`. Ninguno rompe a `notion5g.py` ni al agente ya instalado.

| # | Archivo (línea verificada) | Cambio | Por qué |
|---|---|---|---|
| 1 | `cmd/routeragent/main.go:231` | Mandar **todos** los campos de `readLocalStatus()` en el heartbeat, no los 5 actuales | `InsertHeartbeat` ya guarda el payload completo en `raw` (`internal/store/store.go:444`): es un cambio de una línea, sin migración. Hoy se tiran SINR, PCI, banda, EARFCN, BW, CA, `temp_c` y `eps_reg` cada minuto. Mayor retorno por esfuerzo de todo el proyecto |
| 2 | `internal/store/store.go:103` + `:144` | Columnas indexadas nuevas en `heartbeats`: `sinr_db, rsrq_db, rssi_dbm, band_lte, earfcn, bw_mhz, pci, temp_c, eps_reg, gps_lag_s`, usando el idiom `addColumnIfMissing` que ya existe (`:168`) | Para graficar y filtrar sin parsear `raw`. **Obligatorio** registrarlas también en el bucle de `migrate()`: `CREATE TABLE IF NOT EXISTS` no agrega columnas a una tabla vieja (HANDOVER §5.1, ya mordió en producción) |
| 3 | `internal/store/store.go:126` | **Tabla `locations` append-only** (`device_id, ts, lat, lon, acc, speed_mps, heading_deg, source, client_uid`). `device_locations` se conserva tal cual como "última conocida" | Verificado: `device_locations` tiene `device_id` como PRIMARY KEY, o sea que la traza del recorrido **no se guarda en ninguna parte**. Hoy los drive-tests no producen un drive-test |
| 4 | `internal/store/store.go:221`, `:374`, `:431` | **Eliminar `maxLocationAge = 10 * time.Minute`.** Reemplazar `cachedLocation()` por "el fix cuyo `ts` esté más cerca del `ts` de la muestra, ventana ±20 s", y guardar `gps_lag_s` en la fila | El bug silencioso más caro del sistema: a 60 km/h, 10 minutos son 10 km de error de georreferenciación, aplicado tanto a `measurements` como a `heartbeats` |
| 5 | `internal/api/api.go:63` | `POST /api/v1/ingest/location`: adaptador que acepta el formato OwnTracks (`_type/lat/lon/tst/acc/vel/batt`) y el de GPSLogger, con auth por **Basic** o `?k=` además del header, y `client_uid` para idempotencia | Es lo único que hace falta para hablar con una app que no escribimos. Las apps de terceros no siempre permiten headers arbitrarios |
| 6 | `internal/store/store.go:44` + `internal/api/api.go:98` | **Idempotencia:** columna `client_id TEXT` en `measurements` y en `locations`, más `CREATE UNIQUE INDEX IF NOT EXISTS ... WHERE client_id IS NOT NULL` (índice **parcial**: un UNIQUE normal choca con las filas históricas en NULL), e `INSERT ... ON CONFLICT DO NOTHING` | Sin esto, cada reenvío del spool duplica puntos en silencio y sesga el análisis de cobertura. `InsertRaw` hoy no tiene ningún dedup |
| 7 | `internal/api/api.go:98` | Respuesta **por ítem** en el batch: `{"results":[{"client_id":"…","status":"ok\|dup\|error"}]}`, loop envuelto en **una** transacción, y `readBody` de `1<<20` a `8<<20` solo para ese endpoint | Hoy devuelve `{inserted, errors[]}` sin decir cuáles entraron: el spool del router no puede purgar su cola con precisión. Y un flush largo de túnel no entra en 1 MiB |
| 8 | `internal/store/store.go` (tabla nueva) + `internal/api/api.go` | **Tabla `sessions`** (`session_id, device_id, started_at, ended_at, end_reason` ∈ `user\|budget\|killed\|unknown`, `last_seen_at`, `status`) + `POST /api/v1/sessions`, `POST /api/v1/sessions/{id}/end`. Barrido cada 60 s que marca `stalled` si `last_seen_at` > 3 min | Es la única forma de distinguir "Diego terminó el drive-test" de "el router se quedó sin corriente" o "la app del celular murió". Sin esto, un hueco de 40 min en el recorrido no se explica nunca |
| 9 | `internal/api/api.go` | `GET /api/v1/live?device_id=` → última muestra + salud de sesión + cola pendiente + **el texto de notificación ya formateado por el servidor** | Una sola fuente de verdad para ntfy, dashboard y cualquier cliente futuro (incluida la app de fase 5) |
| 10 | `internal/api/api.go` | Watchdog + push a ntfy: sin heartbeat >90 s, sin fix GPS >60 s, `gps_lag_s` alto, batería del celular <20 %, o presupuesto de datos agotado | Diego maneja; no va a mirar la pantalla |
| 11 | `internal/api/api.go` (reusa `handleCreateCommand`, `:304`) | Auto-encolar `run_speedtest` cada N minutos mientras haya sesión activa | El ciclo de velocidad ya existe entero; solo falta el reloj |
| 12 | `internal/api/api.go` + `internal/store/store.go` | Columnas de contexto de la muestra: `session_id, seq, trigger` (`interval\|distance\|timeout\|nogps`), `sample_kind` (`signal\|throughput`), `dist_from_prev_m`, `dt_from_prev_s`, `st_server`, `st_bytes`, `st_dur_ms`, `st_concurrent` | Guardar la distancia recorrida desde la muestra anterior no cuesta nada en captura y permite después hacer binning espacial (que es lo correcto para mapas de cobertura) sin rehacer la recolección |
| 13 | `internal/api/api.go:437` (`handleSpeedDownload`) | `GET /api/v1/speedtest/profile` → `{recommended_bytes, max_bytes, server_id, server_uplink_mbps, concurrent}`, y **medir una vez el techo del VPS detrás del túnel de Cloudflare y documentarlo** | Si el túnel topa, por ejemplo, en 300 Mbps, todo speedtest 5G por encima de eso mide el backend y no la red: comparar Bogotá contra Medellín compara el VPS consigo mismo |
| 14 | `internal/api/api.go:75` (`auth`) | **Credencial por dispositivo:** tabla `device_tokens(token_hash, device_id, label, created_at, revoked_at)` + `POST /api/v1/devices/enroll` (protegido con la key maestra), y `auth()` acepta la maestra **o** un token revocable | Verificado: hoy hay una sola `X-API-Key` compartida comparada con `subtle.ConstantTimeCompare`, y HANDOVER §5.4 ya documenta que viaja en el JS del dashboard. Con una app de terceros, esa clave queda en un archivo de configuración de software que no controlamos |
| 15 | `internal/api/api.go` | `GET /api/v1/measurements/export?session_id=&format=csv` | Ya estaba en pendientes (HANDOVER §7); con miles de puntos por sesión deja de ser opcional |

**En el agente del router** (`cmd/routeragent/main.go`):

| # | Cambio | Por qué |
|---|---|---|
| 16 | Sub-bucle: `-loop 55s -every 15s` → 4 muestras y `exit`, con **lockfile** (`/var/run/notion5g-agent.lock`) para que dos corridas de cron no se solapen si una se cuelga en un POST sin cobertura | cron no baja de 1 minuto. Esto da 15 s sin romper la regla de diseño del agente (sin demonio: el equipo tiene ~30 MB libres) |
| 17 | Corregir la bifurcación de `run()` (`:91-97`): hoy, si hay un comando pendiente, la corrida hace el speedtest **en vez** del heartbeat | Con el sub-bucle, el minuto del speedtest perdería sus 4 muestras de señal. Hay que hacer las dos cosas |
| 18 | **Spool `/data/notion5g-spool.ndjson`**, append en cada fallo de POST, flush al inicio de la corrida siguiente, tope duro por bytes (p. ej. 500 KB) descartando lo más viejo | Sin backhaul no hay POST: hoy las muestras de túnel y de mala cobertura —las más valiosas del drive-test— se pierden en silencio |
| 19 | Presupuesto de datos **acumulado por sesión**, no solo por prueba, con corte automático y aviso | Precisión sobre lo que ya hay: `timedDownload` (`:318`) ya pide `bytes=200_000_000`, así que la descarga está capada en 200 MB por prueba; la **subida** (`timedUpload`, `:351`) solo está acotada por la duración y por `maxSpeedtestBytes = 500 MiB` (`internal/api/api.go:22`). Lo que falta es el tope de la tarde entera |
| 20 | Marcar (no promediar) las muestras tomadas bajo throttling térmico, usando `temp_c` que **ya se lee** (`:497`) | Router al sol en el parabrisas: el throttling falsea el throughput. Marcarlo permite excluirlo del análisis en vez de contaminar el promedio |

**Sin cambios:** el dashboard sigue sirviendo como panel de control y vista en vivo cuando hay
pantalla. Solo se le agrega dibujar la traza, mostrar `gps_lag_s` y el estado de sesión.

---

## Plan por fases

| Fase | Objetivo | Entregable | Criterio de aceptación verificable | d-p |
|---|---|---|---|---|
| **0. Gate** | Decidir con datos, no con suposiciones, si hace falta app | Resultado de `ssh root@<router> 'ubus call cm get_zcainfo; ubus list'` + prueba de 1 h en el celular real de Diego con la app de logging y la pantalla apagada (incluyendo 10 min en modo avión) | (a) Queda escrito si `get_zcainfo` devuelve o no un bloque `nr` con `n_rsrp`/`n_band`/`n_pci`. (b) Tras 1 h con pantalla apagada, el backend tiene ≥ 200 fixes con huecos < 30 s, y los 10 min de modo avión se recuperan completos al volver la red | 0,5 |
| **1. El dato completo** (probable en campo el mismo día) | Que lo que ya se mide deje de perderse y quede bien georreferenciado | Cambios 1, 2, 3, 4 + app de terceros configurada + traza en el dashboard | Una vuelta de 20 min en auto produce: ≥ 75 heartbeats con `sinr_db`, `pci` y `band_lte` **no nulos**; `gps_lag_s` < 20 s en el 95 % de las filas; y la traza dibujada en el mapa coincide con el recorrido real | 2,0 |
| **2. Cadencia y huecos** | 15 s reales y cero pérdida sin cobertura | Cambios 16, 17, 18, 19, 20 | Drive-test de 1 h con al menos un túnel o zona sin señal: el delta entre muestras consecutivas es ≤ 20 s en el 95 % de los casos, y el hueco de cobertura aparece completo en la base después del flush (contado contra el log local del router) | 1,5 |
| **3. Sesiones y notificación** | Saber en tiempo real qué está pasando, sin mirar la pantalla | Cambios 8, 9, 10, 11 + ntfy instalado en el celular con los textos de este documento | Provocar a mano los cuatro estados (matar la app de GPS, desenchufar el router, agotar el presupuesto de datos, bajar la batería) y recibir el push correcto en < 90 s en cada caso, con el texto exacto de arriba | 1,5 |
| **4. Higiene: idempotencia, seguridad, export** | Que el sistema aguante reenvíos y que la credencial no viva en un APK ajeno | Cambios 6, 7, 12, 13, 14, 15 | Reenviar dos veces el mismo spool no crea filas duplicadas (`SELECT count(*)` estable); un token de dispositivo revocado deja de autenticar en el siguiente request; el CSV de una sesión abre en una hoja de cálculo con las columnas nuevas | 2,0 |
| — | **Subtotal sin publicar nada** | | | **7,5** |
| **5. (Condicional) App Android mínima** | Solo si el gate de fase 0 muestra que el router **no** expone NR, o si la notificación propia en vivo es innegociable | App Kotlin nativa, un módulo: FGS tipo **`location` y nada más**, sin `ACCESS_BACKGROUND_LOCATION`; reloj propio (coroutine) alimentado por el último fix; `registerTelephonyCallback` + `CellInfoNr` guardando `cellinfo_age_ms` de `CellInfo.getTimeStamp()`, con `CellInfo.UNAVAILABLE` → `null` y nunca 0; cola Room; notificación en vivo; chequeo de `getLocationPowerSaverMode()`; **sin speedtest** en la v1 | Prototipo primero (1 día, bloqueante): 30 min con la pantalla apagada logueando `getAllCellInfo()` cada 15 s, y verificar con `getTimeStamp()` que los datos son frescos y no cacheados. Si son rancios, la fase se cancela ese mismo día. Aceptación de la app: 2 h de drive-test sin un solo hueco > 60 s, notificación actualizándose cada 15 s, y los datos de radio coincidiendo en orden de magnitud con los del router en el mismo punto | 8–10 |
| **6. (No recomendada) App iOS** | Nada que la fase 1 no dé ya | GPS + throughput con `CLServiceSession` + Live Activity | Sin RSRP, sin banda, sin PCI; panel en vivo muerto a las 8 h | 10 |

Si el presupuesto es lo que manda: los cambios **1, 3, 4 y 18** solos (≈ 3 d-p) ya sirven hoy, con el
celular en el parabrisas, pantalla encendida y el dashboard actual.

---

## Distribución y publicación

Para ~5 dispositivos de campo, una herramienta interna de H3S:

| | Recomendado | Costo anual | Tiempo hasta tener el equipo andando |
|---|---|---|---|
| **Fases 0–4** | **No se publica nada.** Apps de terceros ya publicadas (logging de GPS + ntfy) y el dashboard como PWA ("agregar a pantalla de inicio"). El agente se despliega como hoy (HANDOVER §6) | USD 0 | Inmediato |
| **Android, fase 5** | **App privada en Managed Google Play** (distribución interna a la organización), o *internal app sharing* / *closed testing* para iterar. Evita la declaración de tipos de foreground service en App Content con descripción + **video demostrativo** (todos los tipos pasan por revisión humana) y está exenta del requisito de target API | USD 25 **una sola vez** (cuenta de desarrollador). **Cuenta de organización, no personal** | Días. Con Play público: sumar 2–3 días de trabajo (video, política de privacidad, Data Safety) más al menos una iteración de rechazo. Y si se abre una cuenta **personal** nueva, Google exige cerrar un test con 12 testers opt-in continuos durante 14 días antes de poder pedir producción |
| **iOS, fase 6** | **TestFlight** (builds de 90 días, hasta 100 testers internos). El Apple Developer Enterprise Program solo si necesitan distribuir fuera de TestFlight | USD 99/año (Apple Developer Program); Enterprise USD 299/año + D-U-N-S, solo empleados | TestFlight evita la revisión pública. Si van a App Store: hay que justificar `UIBackgroundModes: location` como funcionalidad principal evidente (Guideline 2.5.4) |

En todos los casos, si se distribuye una app propia: política de privacidad y formulario de Data
Safety (recoge ubicación precisa continua + identificadores de red), y en Colombia aplica la Ley 1581
de 2012 si esas trazas se pueden asociar a una persona.

Decisión de versión para la fase 5: **targetear la API más alta vigente en el Play Console al momento
de subir**. Y diseñar la sesión **partida en tramos reanudables** con telemetría de "sesión cortada":
la doc de timeouts dice literalmente que la restricción *"currently"* solo aplica a `dataSync` y
`mediaProcessing`, y Google apretó los foreground services en cada release (14: tipos; 15: timeouts;
16: cuotas de jobs). Si algún día `location` gana timeout, el producto se degrada en vez de morir.

---

## Riesgos y cómo se mitigan

| Riesgo | Prob. | Impacto | Mitigación |
|---|---|---|---|
| `cm/get_zcainfo` no trae bloque `nr` y el router no expone métricas NR | Media | **Alto**: la arquitectura recomendada entrega ancla LTE + etiqueta `5G-NSA` + velocidad + GPS, pero ninguna métrica de radio 5G | Es el **gate de fase 0**, cuesta 10 minutos y va **primero**. Si falla, se ejecuta la fase 5 (app Android) con datos sobre la mesa, no con suposiciones |
| `TelephonyManager` no entrega `CellInfo` **fresco** con la pantalla apagada (**no verificado contra documentación oficial**) | Media | Alto, pero **solo sobre la fase 5**: la app mediría GPS y velocidad, no señal | Prototipo bloqueante de 1 día antes de escribir la app, verificando `CellInfo.getTimeStamp()`. La arquitectura recomendada no depende de esto |
| Un OEM (Xiaomi, Huawei, Oppo, Vivo) mata el servicio de ubicación del celular a mitad de sesión. **Sin documentación oficial, sin mitigación por código** | Media | Medio: huecos en la traza, la señal se sigue midiendo en el router | Probar en el teléfono concreto de Diego en fase 0; watchdog de sesión en el servidor (cambio 8) con alerta audible; instructivo de "autostart / sin restricciones de batería" por marca. Si es Pixel o Samsung reciente, el riesgo baja bastante |
| Ahorro de batería con pantalla apagada corta los fixes aunque el servicio siga vivo | Media | Medio | Teléfono **enchufado siempre** (cargando, el Ahorro de batería no se activa solo y el equipo queda fuera de App Standby); en la app propia, consultar `getLocationPowerSaverMode()` y advertir. **Nunca** pedir la exención de Doze por API |
| Dependencia de una app de terceros para el único eslabón crítico del celular (cadencia, cola, permanencia en la tienda) | Media | Medio | Prueba de 1 h en fase 0 incluyendo modo avión; detección de huecos en el servidor; plan B que ya funciona hoy (pantalla encendida + dashboard + Wake Lock); y la fase 5 como salida definitiva |
| El speedtest mide el VPS / el túnel de Cloudflare y no el enlace 5G | **Alta** | Alto para comparar ciudades | Medir una vez el techo del propio servidor y documentarlo; guardar `st_server`, `st_concurrent`, `st_bytes`, `st_dur_ms` en cada muestra; usar `speed.cloudflare.com` como comparador; evaluar un endpoint de alta capacidad separado |
| Consumo de datos fuera de control | Alta si no se capa | Alto | Presupuesto **acumulado por sesión en bytes** con corte automático y aviso (cambio 19), speedtest cada 3–5 min y no cada 15 s. Sin números medidos de mAh: no se inventan, el orden de magnitud es speedtest ≫ GPS continuo > lectura de señal > POST de 1 KB |
| Throttling térmico (router y/o celular al sol, enchufados) falsea el throughput | Alta | Medio | Registrar `temp_c` (router, ya se lee) y `thermal_status` (celular, fase 5) en cada muestra; **marcar** y no promediar las tomadas bajo throttling |
| Reenvíos del spool duplican puntos y sesgan el análisis | Alta sin el cambio 6 | Medio | Idempotencia con índice UNIQUE **parcial** + `ON CONFLICT DO NOTHING` + respuesta por ítem para que el cliente purgue su cola con precisión |
| La `X-API-Key` compartida queda en la configuración de una app de terceros o dentro de un APK | Alta | Alto | Tokens por dispositivo revocables (cambio 14) antes de que la clave salga de nuestras manos |
| Migración de esquema rompe producción con "no such column" | Media | Alto (ya pasó, HANDOVER §5.1) | Toda columna nueva va **también** en el bucle de `addColumnIfMissing` dentro de `migrate()`, y se verifica con `docker logs notion5g-server \| tail` después de desplegar |

---

## Decisión que hace falta de Diego

1. **¿Qué se está homologando: el módem 5G, o la experiencia de un usuario con su celular en esa
   red?** Es la pregunta que decide todo. Si es el módem —que es lo que dice todo el repo—, este plan
   es correcto y el teléfono nunca debió ser el instrumento. Si es la experiencia del celular, hay que
   ir directo a la fase 5 y iOS queda fuera del alcance de forma permanente.
2. **¿Hay iPhones en el equipo de campo, o son todos Android?** Si iOS es aspiracional, Flutter pierde
   su razón de ser y la fase 5 se escribe en Kotlin nativo, un solo módulo. Si hay iPhones que **tienen**
   que medir, hay que decirles que van a aportar GPS y velocidad, nunca señal.
3. **¿La notificación que se reescribe en vivo con RSRP y velocidad es un requisito literal, o alcanza
   con "saber sin mirar la pantalla que la medición sigue viva y qué valores está dando"?** Lo primero
   exige la fase 5 (~8–10 d-p más distribución). Lo segundo se cubre en la fase 3 con ntfy y los textos
   de este documento.
4. **¿Marca y modelo exactos del teléfono de campo?** Cambia el riesgo OEM de "bajo" a "alto" y hay
   que probarlo en fase 0, antes de comprometer cualquier desarrollo.
5. **¿Cuánto dato móvil se puede gastar por sesión y por SIM?** Ese número es el presupuesto en bytes
   del cambio 19; sin él, el speedtest automático no se activa.
6. **¿Cadencia objetivo real: 15 s fijos, o cada 100 m?** El muestreo por distancia es
   metodológicamente mejor para mapas de cobertura (no sobre-representa los semáforos), pero obliga a
   ponderar por `dt_from_prev_s` en todo promedio temporal y a reescribir consultas del dashboard.
   Propuesta intermedia, ya incluida en el cambio 12: **mantener el reloj de 15 s y guardar además
   `dist_from_prev_m`**, para poder hacer binning espacial después sin rehacer la recolección.
7. **¿Este VPS queda como fuente de verdad del código, o se sigue trabajando desde el repo `notion-5g`
   de la WSL?** Este directorio es una copia sincronizada por `rsync` (HANDOVER §1): si se edita acá,
   el próximo `rsync` lo pisa.
