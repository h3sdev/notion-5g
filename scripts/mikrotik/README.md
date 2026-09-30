# MikroTik hAP ac2 como sonda A/B (celular por cable)

El MikroTik decide por cuál router sale el celular: **A = Notion 5G** o **B = Notion 4G**.
El celular cambia una sola regla (`phone-probe`) antes de cada prueba. Además, por un camino
aparte que no toca las mediciones, el celular entra por SSH a cada router (para reiniciarlo
cuando el backend lo pide) y le hace un ping de salud. Diseño completo en
[`server/docs/CONTRATO-SONDA-AB.md`](../../server/docs/CONTRATO-SONDA-AB.md) §3 y §6.1.

| Archivo | Para qué |
|---|---|
| `apply_probe.py` | Herramienta de Windows: respaldo, plan (dry-run), aplicar, verificar, mover la regla a mano |
| `probe-ab.rsc` | Lo mismo como script RouterOS, para pegar en WinBox si no hay Python (generado con `--emit-rsc`) |
| `test_apply_probe.py` | Pruebas sin equipo (protocolo de la API y un RouterOS falso) |

**Estado (2026-09-29):** la configuración de §3 ya está aplicada en el hAP (A sale por
`179.19.72.14` (BOG) y B por `186.102.123.189` (Movistar, MDE)) y el celular está en `ether3`.
**§6.1 ya está aplicado** (reinicio remoto y salud por router; lo aplicó `admin` a mano el
2026-09-29 20:54, respaldo `flash/pre-mgmt-20260929-205428.rsc`): las 6 reglas en orden, los
scripts WAN (idénticos a los que genera la herramienta), sin la NAT vieja y `test` en
`probe-api`. **Una sola diferencia:** en el equipo `probe:mgmt-A`/`probe:mgmt-B` quedaron con
`table=main` (como decía el contrato) y la herramienta las quiere en `to-A`/`to-B` (ver abajo
por qué), así que `--dry-run` muestra 2 cambios (`/routing rule set ... table=to-A`/`to-B`) y
`--verify` sale con 1. Aplicarlos (`--apply`) necesita el visto bueno de Diego.

## 1. Cableado (ya hecho, no mover)

| Puerto del hAP | Va a | Qué hace |
|---|---|---|
| `ether1` | Puerto LAN del **router B** (Notion 4G) | WAN B, tabla `to-B`. B entrega `192.168.2.x` por DHCP |
| `ether2` | Puerto LAN del **router A** (Notion 5G) | WAN A, tabla `to-A`. A entrega `192.168.1.x` por DHCP |
| `ether3` | **Celular** (adaptador USB-C a Ethernet) | Red propia `192.168.89.0/24`; el celular recibe `192.168.89.10–50` |
| `ether4` | libre | Administración (`192.168.88.0/24`) |
| `ether5` | **PC** (adaptador ASIX, Windows ifIndex 82) | Administración; el PC entra por IPv6 link-local o, desde el 2026-09-29, por `192.168.88.1` (el PC tiene `192.168.88.254` en ese adaptador y la WSL también llega) |
| WiFi `MKT` | — | Administración (misma red que `ether4`/`ether5`) |

Los cables van a un puerto **LAN** de cada router (no al WAN ni a otro puerto especial). Si
algún día se cambian de puerto A y B, hay que cambiar `WAN` en `apply_probe.py`: el celular
no sabe de puertos, solo de las tablas `to-A`/`to-B`.

### Conectar el celular

1. Usa un adaptador **USB-C con Ethernet y carga (PD)**: con la sonda activa, el teléfono
   sin cargar se descarga en pocas horas.
2. Cable de red del adaptador a **`ether3`**. En el hAP se enciende la luz de `ether3`.
3. En el celular aparece "Ethernet" conectado (Ajustes → Conexiones → Más ajustes). Queda
   con una IP `192.168.89.x`. El WiFi de la oficina debe **seguir encendido**: por ahí la
   app habla con el backend local (la IP del PC en esa WiFi: `192.168.40.22` en la red de la
   oficina, `192.168.2.60` en "Pipito" el 2026-09-29; cambia con el DHCP, confirmarla con
   `ip -4 addr` en la WSL o `ipconfig` en Windows).
4. En el teléfono apaga las actualizaciones automáticas de Play Store y las copias en la
   nube: con el cable, Ethernet queda como red por defecto y ese tráfico ensucia la medición.
5. En la app, **Ajustes de la sonda → MikroTik**: host `192.168.89.1`, puerto `8728`,
   usuario `phone-probe`, contraseña = la de
   `C:\Users\diego\mikrotik-probe\phone-probe-password.txt`. Luego **"Probar MikroTik"**:
   debe decir identidad `hap-sonda`, versión 7.6, regla en `main` y rutas `to-A`
   (`…%ether2`) y `to-B` (`…%ether1`) activas.

## 2. Usar la herramienta

**Lo más simple (verificado 2026-09-29):** desde la WSL, por la LAN de administración, sin copiar
nada: `MIKROTIK_PASSWORD=admin python3 apply_probe.py --dry-run --host 192.168.88.1` (igual con
`--verify`). Funciona mientras el PC tenga `192.168.88.254` en el adaptador de `ether5`.

Si eso no llega, se copia a `C:\Users\diego\mikrotik-probe\` y se corre con el Python de
Windows por link-local (WSL no llega a direcciones link-local). Desde WSL:

```bash
cp /home/ubudev/notion-5g/scripts/mikrotik/{apply_probe.py,test_apply_probe.py,probe-ab.rsc} /mnt/c/Users/diego/mikrotik-probe/
cd /mnt/c && powershell.exe -NoProfile -Command 'cd C:\Users\diego\mikrotik-probe; python apply_probe.py --password admin'
```

La contraseña de `admin` también se puede pasar en la variable de entorno `MIKROTIK_PASSWORD`
(no queda en el historial de comandos): `$env:MIKROTIK_PASSWORD='...'; python apply_probe.py`.

| Comando | Qué hace | ¿Cambia algo? |
|---|---|---|
| `python apply_probe.py --password admin` | **Dry-run**: lee el equipo y muestra cada cambio como línea de consola RouterOS | No |
| `... --apply` | Respaldo obligatorio y luego aplica solo lo que difiere. Al final vuelve a comparar (debe dar cero cambios) y prueba | Sí |
| `... --verify` | Compara con lo esperado, pinga el gateway de cada router y mira la IP pública de salida por `to-A` y `to-B` | Solo reglas temporales `probe:tmp-verify`, que borra al terminar (`--no-egress-test` para no crearlas) |
| `... --backup-only` | Solo el respaldo | Deja un `.backup` en el router |
| `... --switch A` / `B` / `fallback` | Mueve la regla `phone-probe` a `to-A` / `to-B` / `main` (pruebas a mano) | Sí; el vigilante la devuelve a `main` a los 10 min |
| `... --emit-rsc > probe-ab.rsc` | Regenera el script `.rsc` | No se conecta |

**Ojo al aplicar:** con la sonda detenida (`--apply` pide `--force` si `phone-probe` no está en
`main`). Cambiar el `script` de un DHCP client WAN no lo reinició en 7.6 (el 2026-09-29 20:54 el
log no muestra "lost IP address" tras el cambio), pero igual no se hace si el texto ya coincide.

Opciones útiles: `--host` (por defecto `fe80::de2c:6eff:fef7:7fe9%82`; también sirve
`192.168.88.1` si el PC tiene IP de administración), `--gw-a`/`--gw-b` (gateway si un
router está apagado), `--check-gateway arp` (si un router no responde ping desde su LAN),
`--rule-watchdog-min N`, `--phone-password`/`--rotate-phone-password`, `--force` (aplicar
aunque la regla no esté en `main`; **no** usar con la sonda midiendo), `--verbose`,
`--show-password` (imprime la contraseña nueva de `phone-probe`; por defecto solo se guarda en
`phone-probe-password.txt`, para que no quede en registros de consola).

**Respaldo** (antes de cualquier cambio, si falla no toca nada): `/export` y
`/system backup save` en la carpeta `flash/` del router, bajada por FTP a
`C:\Users\diego\mikrotik-probe\backups\` (se comprueba el tamaño), volcado JSON de los menús
que se tocan, y se borra del router solo el `.rsc` bajado (hay ~1,6 MB de flash libre; cada
`.backup` ocupa ~35 KB; la herramienta avisa si hay más de 10). **Volver atrás:** en la terminal
de WinBox, `/system backup load name=flash/pre-probe-<fecha>.backup` (reinicia el equipo).

En el hAP ac2 **solo lo que está bajo `flash/` sobrevive a un reinicio**: la raíz de Files es RAM.
Los respaldos de antes de esta corrección (`pre-probe-20260929-180510`, `-180606`, `-181959`)
quedaron en la raíz y se pierden al apagar el equipo; sus copias están en la carpeta `backups\`
del PC. Para volver a uno de esos, súbelo a `flash/` con WinBox (Files) y cárgalo desde ahí.

## 3. Qué configura

- **MAC fija del bridge** (su link-local es la dirección por la que entra el PC).
- `ether2` y `ether3` **fuera del bridge**; `ether1`/`ether2` en la lista `WAN`, `ether3` en `LAN`.
- Administración `192.168.88.1/24` (DHCP sin gateway ni DNS, para que el PC no salga a
  Internet por los routers bajo prueba). Celular `192.168.89.1/24` con DHCP propio.
- Un **cliente DHCP por WAN** con `add-default-route=no` y un script que, al recibir
  concesión, pone el gateway real en sus rutas y en su regla `probe:mgmt-A`/`probe:mgmt-B`
  (`<gateway>/32`), cada cosa solo si cambió.
- Tablas `to-A`/`to-B`, cada una con su ruta por defecto `gateway=<gw>%ether2` /
  `<gw>%ether1` **sin** `check-gateway` (forzado a un router caído, el tráfico falla en vez
  de irse por el otro). En `main`, A con distancia 1 y B con 2, ambas con `check-gateway=ping`.
- Reglas de ruteo, **en este orden** (todas `action=lookup-only-in-table`, que termina la
  búsqueda: por eso las de gestión y salud van antes de `phone-probe`):

  | `comment` | Regla | Para qué |
  |---|---|---|
  | `probe:phone-local` | `dst-address=192.168.89.0/24 table=main` | Lo que va al propio celular (API, DHCP) usa `main` |
  | `probe:mgmt-A` | `dst-address=<gateway de A>/32 table=to-A` | SSH del celular al Notion 5G, sin importar a qué tabla esté forzada `phone-probe` |
  | `probe:mgmt-B` | `dst-address=<gateway de B>/32 table=to-B` | Igual para el Notion 4G |
  | `probe:health-A` | `dst-address=9.9.9.9/32 table=to-A` | Ping de salud: siempre sale por A |
  | `probe:health-B` | `dst-address=149.112.112.112/32 table=to-B` | Ping de salud: siempre sale por B |
  | **`phone-probe`** | `src-address=192.168.89.0/24`, `table` = `main` en reposo, `to-A`/`to-B` en una prueba | La que mueve el celular |

  Las reglas nuevas se crean ya en su lugar (`place-before` de la siguiente que exista); si
  el orden quedara mal, la herramienta lo corrige con `/routing rule move`. Nunca cambia la
  `table` de `phone-probe`: eso lo hace el celular. Las de gestión usan la tabla **del propio
  router** (no `main`, como decía el contrato): si ese router está caído el SSH falla en vez de
  irse por el otro router (y mandar la clave `root` por la red celular de B), y sigue
  funcionando aunque A y B usaran la misma LAN. El SSH va por la LAN del router y el ping de
  salud son 3 paquetes por router por minuto: no gasta datos ni altera las mediciones.
  **Consecuencia:** todo lo que el celular mande a `9.9.9.9`, `149.112.112.112` o al gateway de
  un router sale siempre por ese router, aunque `phone-probe` esté forzada al otro. Ninguna
  prueba de medición debe usar esas IP como destino (DNS, ping, etc.).
- NAT `masquerade` por la lista `WAN`. La regla vieja de fábrica (`masquerade
  src-address=192.168.1.0/24 out-interface=ether1`) se **borra**: ya no aplica y la sonda pide
  reglas mínimas.
- Firewall: el de fábrica, **con `defconf: fasttrack` habilitado y `hw-offload=yes`**. La
  herramienta no lo cambia; `--dry-run`/`--verify` avisan si falta el fasttrack, si está
  deshabilitado o sin `hw-offload`, y si hay reglas de filtro, mangle, raw o NAT ajenas (que no
  son `defconf` ni `probe:`). `--verify` sale con código 1 si el fasttrack tiene problemas.
- Usuario `phone-probe` (grupo `probe-api`: solo `read,write,api,test` —`test` es para `/ping`
  por la API, la salud por router—, y solo desde `192.168.89.0/24`). El servicio `api` solo acepta `192.168.88.0/24`, `192.168.89.0/24` y
  `fe80::/10` (la herramienta se niega a quitar `fe80::/10`, y tras cambiarlo prueba una
  conexión nueva; si no entra, lo revierte).
- Vigilante `probe-rule-watchdog` (cada minuto): si la regla lleva 10 minutos fuera de `main`
  **sin actividad del celular** (ningún login nuevo de `phone-probe` en `/log` y sin cambio de
  tabla), la devuelve a `main` y deja `phone-probe: regla en to-X por 10 min sin actividad del
  celular, vuelta a main` en `/log`. Así cubre "el celular murió a mitad de prueba" sin cortar
  pruebas seguidas por el mismo router. Corre con `policy=read,write,policy,test`: con solo
  `read,write`, RouterOS 7.6 no conserva sus variables `:global` entre corridas y el vigilante
  nunca actuaba (probado en el equipo).
- DNS `1.1.1.1,8.8.8.8`, NTP por IP (`time.cloudflare.com`), zona `America/Bogota`,
  identidad `hap-sonda`.

## 4. Cómo comprobar que funcionó

`python apply_probe.py --password admin --verify` debe mostrar:

- `Configuración: coincide con el contrato.`
- Los dos `dhcp-client` en `bound`, cada uno con su gateway (hoy A `192.168.1.1`, B `192.168.2.1`).
- Las reglas en orden `probe:phone-local`, `probe:mgmt-A`, `probe:mgmt-B`, `probe:health-A`,
  `probe:health-B` y `phone-probe` con `table=main`.
- `fasttrack .id=*8 chain=forward hw-offload=true …` y una línea `fasttrack: N MB acelerados`.
- `salud A (9.9.9.9 por to-A): recibidos 3/3` y lo mismo para B (`149.112.112.112 por to-B`).
- `probe:A-default` y `probe:B-default` con `active=true`; en `main`, `probe:main-A` activa.
- `ping gateway A/B: recibidos 2/2`.
- `por to-A ... salida <IP>` y `por to-B ... salida <otra IP>`: **las dos IP deben ser
  distintas** (si son iguales, algo no está forzando la ruta).
- Con el celular conectado: `celular: ether3 con enlace` y una línea `lease 192.168.89.x`.
  Sin cable dice `SIN enlace` y el DHCP `probe-phone` aparece `inválido`: es normal, se
  arregla solo al conectar el celular.
- Sin avisos sobre la puerta de enlace del PC. Si aparece uno, en Windows:
  `ipconfig /renew "Ethernet 7"` (la red de administración ya no entrega gateway).

Prueba manual de la regla (sin la app): `--switch A`, `--verify` (la regla sigue en `to-A`,
no es diferencia) y `--switch fallback`.

## 5. Si los dos routers usan la misma LAN (p. ej. los dos 192.168.1.0/24)

Hoy no pasa: A usa `192.168.1.0/24` y B `192.168.2.0/24`. Si alguno cambia (reset de
fábrica, otro router B), en principio **sigue funcionando**, porque cada ruta nombra su
puerto (`192.168.1.1%ether2` y `192.168.1.1%ether1`): RouterOS resuelve el gateway en esa
interfaz, cada puerto tiene su propia tabla ARP y el `masquerade` usa la IP del puerto de
salida. El script del DHCP de cada WAN actualiza el gateway solo.

Lo que se vuelve ambiguo es `main`: tendría dos rutas conectadas al mismo prefijo y el
MikroTik no sabría por cuál puerto llegar a "192.168.1.x" (por ejemplo, para abrir la web de
un router desde el PC). Nada de la sonda depende de eso, pero si algo se comporta raro
(ping al gateway de uno falla, rutas inactivas, A y B con la misma IP de salida):

1. Entra a la web del router que menos se use (normalmente B) y cambia su LAN a otra
   subred libre, por ejemplo `192.168.10.1/24` (DHCP `192.168.10.100–200`).
   **No uses** `192.168.2.0/24` si A queda en `192.168.1.0/24` y el WiFi de la oficina
   "Pipito" sigue en `192.168.2.0/24`; tampoco `192.168.88/89.0/24` (son del MikroTik) ni
   `192.168.40.0/24` (backend).
2. Desconecta y conecta el cable de ese router en el hAP (o espera la renovación) para que
   su `dhcp-client` quede *bound* con el gateway nuevo.
3. Corre `--dry-run`: si el script WAN ya corrigió las rutas no habrá cambios; si no,
   `--apply` pone el gateway nuevo. Luego `--verify`.

Nota: B hoy usa `192.168.2.0/24`, igual que el WiFi "Pipito" de la oficina. No choca: son
redes distintas, y el celular ata cada conexión a una red (Ethernet o WiFi), nunca a la red
por defecto.

## 6. Problemas comunes

| Síntoma | Qué hacer |
|---|---|
| `No pude conectar a fe80::…%82` | ¿Cable del PC en `ether5`? El ifIndex cambia si cambias de adaptador: `Get-NetAdapter` en PowerShell y usa `--host fe80::de2c:6eff:fef7:7fe9%<ifIndex>` |
| `La regla phone-probe está en to-A` al aplicar | La sonda está midiendo: detenla en la app, o espera a que termine. `--force` solo si sabes que el celular no está midiendo |
| La regla quedó en `to-A`/`to-B` y el celular está apagado | El vigilante la devuelve a `main` a los 10 min sin logins del celular; o `--switch fallback` |
| Ruta `probe:main-A` inactiva con A encendido | El Notion no responde ping desde su LAN: `--apply --check-gateway arp` |
| A y B salen con la misma IP | Revisa el orden de las reglas (`--verify`) y que ningún otro cambio haya tocado las tablas |
| Algo quedó a medias (se cortó la sesión) | `--dry-run` muestra lo que falta; `--apply` otra vez (todo es idempotente) |

Recomendado, fuera del alcance de esta herramienta: cambiar la contraseña `admin` del hAP
(luego se usa la nueva en `--password`).

## 7. Sin Python: el script `.rsc`

`probe-ab.rsc` hace lo mismo en un solo bloque. **No hace respaldo** (hazlo a mano antes:
`/export file=flash/pre-probe-manual` y `/system backup save name=flash/pre-probe-manual`, y baja
los dos archivos desde Files). No se ha probado con `/import` en el equipo (solo su sintaxis y
estructura); lo probado en el equipo es `apply_probe.py`. Cambia `CAMBIAR-ESTA-CLAVE` (solo se usa si `phone-probe` no existe),
súbelo a Files y corre `/import file-name=probe-ab.rsc`. A diferencia de la herramienta,
reescribe los valores aunque ya estén bien y no hace las pruebas finales: después corre
`--verify`. No lo edites a mano: se regenera con `--emit-rsc` (una prueba comprueba que el
archivo del repo está al día).

## 8. Pruebas sin equipo

```bash
cd /home/ubudev/notion-5g/scripts/mikrotik && python3 -m unittest test_apply_probe -v
```

Cubren el codificador de largos de la API (todos los límites), `!re`/`!trap`/`!done`/
`!fatal`/`!empty`, y un RouterOS falso sembrado con el estado de fábrica real del hAP:
aplicar y volver a planear da cero cambios; la regla `phone-probe` movida por el celular no
cuenta como diferencia; el orden de las reglas se corrige; el gateway sigue al DHCP y se
conserva con el WAN caído; la MAC del bridge se fija antes de sacar `ether2`; el respaldo va a
`flash/` y aborta con poca flash; `--apply` aborta si no puede leer un menú (en vez de crear
duplicados); el vigilante tiene la policy y las condiciones de reinicio correctas; la contraseña
nueva no se imprime salvo `--show-password`. De §6.1: desde la instalación de §3 las 4 reglas
nuevas se crean con `place-before` de `phone-probe` (y, si el equipo no aceptara
`place-before`, se agregan al final y se mueven); 60 órdenes al azar de las 6 reglas (con una
regla ajena en medio) quedan bien tras un `--apply`; un `dst-address` sin `/32` no es
diferencia; el script WAN actualiza solo su regla `probe:mgmt-*` y es idéntico al aplicado en
el equipo; A y B con el mismo gateway avisan; un `/ip firewall filter` ilegible no se reporta
como "falta el fasttrack"; solo se borra la NAT vieja de
fábrica y las reglas ajenas de filtro/mangle/NAT avisan sin borrarse; faltar el fasttrack,
tenerlo deshabilitado o sin `hw-offload` avisa y hace fallar `--verify`, sin tocar el firewall;
`--apply` respalda antes de la primera escritura.
