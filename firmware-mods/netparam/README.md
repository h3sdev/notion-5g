# Web-configurable outbound TTL (Notion 5G)

Replaces the fixed TTL of `scripts/ttl_persist_install.sh`. The value is stored in a configuration file
(`/etc/config/netparam`) and changed from the router's web UI:
**Router → System Settings → Diagnostics**, in the "Network parameter" section below the ping form (Spanish UI:
Router → Configuración del sistema → Diagnóstico → "Parámetro de red"). There is no menu entry of its own, and
the UI text never says "TTL".

Internal names are neutral on purpose (`netparam`: files, uci config, fw3 include, iptables comment, syslog tag,
JSON fields). The first version used `notion_ttl` / `notion-ttl`; `install.sh` migrates a unit that has it
(keeps the value and the factory backup, deletes every old file, rule, string and include).

- `OEM-SPEC.md`: specification for the manufacturer. Section 0 lists exactly which file goes where in the
  firmware image. `OEM-SPEC.es.md` is the Spanish translation for internal use.
- `install.sh`: installs or updates it on a running unit over SSH (`NOTION_PASS=... firmware-mods/netparam/install.sh`).
  `--remove` restores the factory web UI from the backup in `/system/etc/netparam-backup/`.
- `test_ui.py`: tests the section in Chromium (Playwright) the way a user would: placement inside Diagnostics,
  no "TTL" on screen, apply, invalid values, IPv6, disable/enable and es/en/cn, checking every change on the device.
- `test_cgi.py`: end-to-end test of the backend (session, CSRF, validation, value change, `fw3 reload`).
  Also checks that the files served to the browser never mention ttl. Leaves the device at value 64.

Change the value without the web UI:
`uci set netparam.main.value=65; uci commit netparam; sh /system/etc/firewall.netparam.sh`.

Show state and packet counters: `sh /system/etc/firewall.netparam.sh status`.

Everything lives in `/system/etc`, which is writable and survives reboots. A factory reset that wipes that overlay
removes it; that is why it should go into the firmware.
