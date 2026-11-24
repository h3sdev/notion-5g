# TTL configurable desde la interfaz web (Notion 5G)

Reemplaza al TTL fijo de `scripts/ttl_persist_install.sh`. Con esto el valor queda en un archivo de configuración
(`/etc/config/notion_ttl`) y se cambia desde la interfaz web del router:
**Router → Configuraciones avanzadas → TTL de salida**.

- `OEM-SPEC.md`: especificación para la casa matriz (en inglés). Explica qué hay que meter en el firmware, las
  dos formas de integrarlo y las pruebas de aceptación.
- `install.sh`: lo instala o actualiza en un equipo por SSH (`NOTION_PASS=... firmware-mods/notion-ttl/install.sh`).
  Con `--remove` deja la web como venía de fábrica, usando el respaldo en `/system/etc/notion-ttl-backup/`.
- `test_cgi.py`: prueba de punta a punta (sesión, CSRF, validación, cambio de valor, `fw3 reload`). Al terminar
  deja el equipo en TTL 64.

Cambiar el valor sin entrar a la interfaz web:
`uci set notion_ttl.main.ttl=65; uci commit notion_ttl; sh /system/etc/firewall.notion-ttl.sh`.

Ver el estado y los contadores: `sh /system/etc/firewall.notion-ttl.sh status`.

Todo vive en `/system/etc`, que es escribible y sobrevive al reinicio. Un reset de fábrica que limpie el overlay
lo borra; para eso está la versión de firmware.
