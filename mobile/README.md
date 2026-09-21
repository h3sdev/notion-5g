# mobile — app de campo para el Notion 5G (Flutter/Android)

App que un técnico lleva en el celular durante las pruebas de campo del
módem (que corre con powerbank, sin GPS propio). Da la ubicación
ultraprecisa del teléfono (Fused Location de Android, GPS + red — **no** la
API de Google Cloud Geolocation, que estima por celda/Wi-Fi sin GPS real y
necesita billing) y le pide al backend (`server/`) que dispare una prueba en
el router.

## Dónde vive el código

Este directorio (`C:\dev\notion5g_mobile` en Windows) es el proyecto
**buildable** con Flutter/Gradle (el SDK de Flutter está instalado del lado
Windows, no en WSL). Su código versionable se sincroniza a
`notion-5g/mobile/` dentro del repo en WSL:

```bash
rsync -a --exclude='.dart_tool' --exclude='build' --exclude='.gradle' \
  --exclude='.idea' --exclude='*.iml' --exclude='.metadata' \
  --exclude='android/local.properties' \
  /mnt/c/dev/notion5g_mobile/ /home/ubudev/notion-5g/mobile/
```

Si `C:\dev\notion5g_mobile` no existe (o se quiere recrear desde cero):

```powershell
flutter create C:\dev\notion5g_mobile --project-name notion5g_field --org com.h3s.notion5g --platforms=android
```

y luego copiar encima `lib/`, `test/`, `pubspec.yaml` desde `notion-5g/mobile/`,
y aplicar los permisos de `android/app/src/main/AndroidManifest.xml` (ver
abajo) y las dependencias de `pubspec.yaml`.

**Ojo:** el sync va en el sentido Windows → repo. Si se edita directo en
`notion-5g/mobile/` hay que copiarlo de vuelta a `C:\dev\notion5g_mobile`
antes de compilar, o el próximo `rsync` en este sentido lo pisa.

## Qué hace la app

- **Pantalla principal**: botón "Correr prueba en el router" → pide permiso
  de ubicación si hace falta, toma la posición fused actual, y crea un
  comando en el backend: `POST /api/v1/commands` con
  `{"device_id": "<router configurado>", "type": "run_speedtest", "duration_s": N,
  "lat", "lon", "gps_accuracy_m", "gps_source": "android-fused", "requested_by": "<id de esta instalación>"}`.
  El router lo recoge en su siguiente ciclo de cron (ver `server/README.md`,
  sección "Agente del router") y reporta el resultado; la ubicación se
  fusiona automáticamente en esa medición porque el módem no tiene GPS.
- Lista las últimas mediciones (`GET /api/v1/measurements?device_id=`) y el
  estado de los comandos recientes (`GET /api/v1/commands?device_id=`) para
  ese router, con pull-to-refresh.
- **Ajustes**: URL del backend, API key (`X-API-Key`), `device_id` del router
  objetivo, duración de la prueba, y un botón "Probar conexión"
  (`GET /healthz`). Todo persistido con `shared_preferences`.
- **Modo en movimiento** (interruptor en la pantalla principal): mientras esté
  activo, manda la ubicación fused cada 15s a
  `POST /api/v1/devices/{device_id}/location` (`ApiClient.postLocation`,
  `lib/location_beacon.dart`). El backend la guarda como "última ubicación
  conocida" de ese router y la usa para completar sus heartbeats (uno por
  minuto, con ping — ver `server/cmd/routeragent`), que no traen GPS propio.
  Esto es lo que le da sentido al perfil de ping+ubicación cuando el equipo
  va en el carro: cada tick del celular es independiente, un error de GPS o
  de red en un tick no detiene el beacon ni tumba la app, solo se muestra y
  se reintenta en el siguiente. **Ojo:** si cambias algo en Ajustes con el
  modo activo, se apaga solo (para no seguir mandando datos a la config
  vieja) y hay que volver a prenderlo.

## Permisos Android agregados

En `android/app/src/main/AndroidManifest.xml`: `INTERNET`,
`ACCESS_NETWORK_STATE`, `ACCESS_FINE_LOCATION`, `ACCESS_COARSE_LOCATION`.

## Qué está probado y qué no

Probado en esta máquina (sin dispositivo/emulador Android disponible):

- `flutter analyze` → **0 issues**.
- `flutter test` → **2/2 tests pasando**:
  - `test/widget_test.dart`: smoke test de que la app arranca y muestra la
    pantalla principal (usa el mock oficial `SharedPreferences.setMockInitialValues({})`).
  - `test/manual/api_client_live_test.dart`: prueba de integración **real**
    (red de verdad, no mockeada) del `ApiClient` contra un backend `server/`
    corriendo de verdad. Por defecto se salta ("SKIP: define
    NOTION_BACKEND_URL...") si no hay backend configurado, para no romper
    `flutter test` en cualquier máquina; se activa así:
    ```
    NOTION_BACKEND_URL=http://<host>:<puerto> NOTION_API_KEY=<clave> NOTION_DEVICE_ID=<device> \
      flutter test test/manual/api_client_live_test.dart
    ```
    Corrida el 2026-09-17 contra un backend real en `127.0.0.1:8098`: creó un
    comando (`comando creado: #1`), lo confirmó en `GET /api/v1/commands`, y
    listó mediciones — **todo pasó** (`00:00 +1: All tests passed!`). Esto
    valida que `ApiClient` de verdad habla el protocolo del backend, más allá
    de que solo compile.
- `flutter build apk --debug` → **compila y genera el APK**
  (`build/app/outputs/flutter-apk/app-debug.apk`, ~145 MB sin optimizar/sin
  ofuscar, normal para un build debug).

Ronda 2026-09-18 (agregado `postLocation` + "modo en movimiento" + columnas de
estado/ping/ubicación en el dashboard): `flutter analyze` → 0 issues,
`flutter test test/widget_test.dart` → 1/1 pasando, `flutter build apk --debug`
→ compila. Probado end-to-end del lado del backend (no del celular real): un
`POST /api/v1/devices/{id}/location` de prueba + un `POST /api/v1/heartbeat`
sin ubicación propia sí quedó con la ubicación fusionada en `raw` y en las
columnas indexadas (verificado con curl contra un backend local).

**NO probado** (requiere un teléfono o emulador Android real, no disponible
en este entorno):

- El diálogo real de permisos de ubicación de Android y sus distintos
  estados (negado, negado permanentemente, servicio de ubicación apagado).
- Que `geolocator` efectivamente devuelva una posición fused real en un
  dispositivo físico.
- La UI completa corriendo en pantalla (botones, navegación a Ajustes,
  refresco de listas) — el smoke test solo verifica que el árbol de widgets
  se construya, no interacción real de usuario.
- Background/segundo plano: **sigue sin implementarse de verdad**. El "modo
  en movimiento" usa un `Timer.periodic` de Dart, que solo corre mientras la
  app está en primer plano y el proceso vivo — Android puede pausarlo o
  matarlo si la pantalla se apaga o el usuario cambia de app, sobre todo con
  ahorro de batería agresivo (bastante común en Xiaomi/Huawei/Samsung). Para
  que el beacon sobreviva con la pantalla apagada durante todo el trayecto
  haría falta un foreground service real (`flutter_foreground_task` o
  `workmanager`, con `FOREGROUND_SERVICE_LOCATION` en Android 14+) — se dejó
  fuera de este alcance por lo mismo que antes: cambia el ciclo de vida de la
  app y merece su propia revisión. **Para la prueba en carretera, mientras
  tanto: dejar la pantalla del celular encendida y la app en primer plano.**
  No se pudo probar en un dispositivo real si el Timer efectivamente aguanta
  con la pantalla apagada por unos segundos (comportamiento normal de
  Android) ni por cuánto.</br>
  Tampoco implementado: que `createSpeedtestCommand` se repita solo cada N
  minutos (la prueba de velocidad completa sigue siendo manual, por el
  botón); el modo en movimiento solo automatiza la ubicación del perfil de
  ping, no dispara pruebas de velocidad repetidas.
- Ícono/nombre de la app, firma de release, y publicación: se usó el
  scaffold por defecto de `flutter create`.

## Dependencias añadidas

`geolocator` (ubicación fused), `shared_preferences` (config local), `http`
(cliente REST), `intl` (formateo de fechas en la lista de mediciones).
