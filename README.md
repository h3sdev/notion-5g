# notion-5g — homologación del módem Notion 5G (ASR1901, OpenWrt)

Equipo bajo prueba: CPE/módem 5G con powerbank externa, firmware `H3S_P52HL_KSA00174_V001`, en `192.168.1.1`.

| Carpeta | Contenido |
|---|---|
| `docs/01-inventario-equipo.md` | Inventario técnico completo (SoC, RAM/flash, bandas, SIM, servicios, hallazgo de GPS, TTL, medidas de referencia) |
| `docs/02-plan-homologacion.md` | Plan de pruebas: métricas, GPS externo, protocolo multi‑operador, comparación con celular, criterios de aceptación |
| `scripts/rsh.sh` | SSH con clave al módem (`NOTION_HOST/USER/PASS`) |
| `scripts/notion5g.py` | Herramienta: `status`, `speed`, `drive`, `at`, `ubus`, `heartbeat` → escribe `results/*.csv|jsonl` y, si se configura `NOTION_REPORT_URL`, reporta a `server/` (backend Go) |
| `scripts/modem_http_api.py` | Cliente del API web nativo del router (XML sobre `xml_action.cgi`, sin SSH) — referencia para la app de celular |
| `scripts/summarize.py` | Tablas resumen por operador/sitio (texto o Markdown) |
| `scripts/ttl_persist_install.sh` | Instala/quita reglas TTL persistentes en el módem |
| `scripts/win_location.ps1` | Posición vía Windows Location API (GPS externo para el drive test) |
| `firmware-notes/` | Volcados crudos del equipo (ubus, uci, dmesg, red, sistema, scripts internos) |
| `results/` | Mediciones (CSV/JSONL) |

Requisitos en el PC: Python 3, `ssh`, `ping`. Opcional: CLI de Ookla `speedtest` (PATH, `~/.local/bin`, `C:\Users\<u>\speedtest-cli\` o `NOTION_SPEEDTEST`).

Antes de medir: **Wi‑Fi del PC apagado y VPN desconectada** (el túnel WireGuard "OFC H3S" tiene kill‑switch y bloquea la salida por el módem).
`speed` comprueba con los contadores de `ccinet0` que el tráfico sale por el módem y aborta si no (`--force` para registrar igual).

```bash
python3 scripts/notion5g.py status
NOTION_TAG=tigo-oficina python3 scripts/notion5g.py speed --runs 3
python3 scripts/notion5g.py drive --interval 10 --gps win
python3 scripts/summarize.py --md
```
