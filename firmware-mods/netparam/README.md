# Web-configurable outbound TTL (Notion 5G)

Replaces the fixed TTL of `scripts/ttl_persist_install.sh`. The value is stored in a configuration file
(`/etc/config/netparam`) and changed from the router's web UI:
**Router → System Settings → Diagnostics**, in the "Network parameter" section below the ping form (Spanish UI:
Router → Configuración del sistema → Diagnóstico → "Parámetro de red"). There is no menu entry of its own, and
the UI text never says "TTL".

Internal names are neutral on purpose (`netparam`: files, uci config, fw3 include, iptables comment, syslog tag,
JSON fields). The first version used `notion_ttl` / `notion-ttl`; `tools/install.sh` migrates a unit that has it
(keeps the value and the factory backup, deletes every old file, rule, string and include).

- `OEM-SPEC.md`: specification for the manufacturer. Section 0 lists exactly which file goes where in the
  firmware image. `OEM-SPEC.es.md` is the Spanish translation for internal use.
- `IMPLEMENTATION.md`: step-by-step instructions for the manufacturer (English), with a sign-off checklist.
- `REQUIREMENTS.md`: final state of the device (R0-R12) and how each requirement is checked.
- `make-oem-package.sh`: builds the zip for the manufacturer (`dist/netparam-oem-<date>.zip`, not in git). In the zip:
  `files/` = final files at their device path (factory originals + the feature), `merge/` = the change alone
  (apply script, strings, patch), `tools/`, the three English documents, `MANIFEST.md5` and `VERSION.txt`.
  `OEM-SPEC.es.md` and this README are not shipped.
- `factory/`: untouched factory files from the squashfs of the 2026-08-17 firmware (the 5 files we modify, plus
  `UNCHANGED.md5` for the 3 that must not change). Used only to build `files/` and `MANIFEST.md5`.
- `apply-to-rootfs.sh`: applies the feature to a firmware root filesystem tree (used by the manufacturer).
- `tools/verify.sh`: checks a unit against REQUIREMENTS.md R0-R8 (`MANIFEST.md5` only when run from the zip).
- `tools/install.sh`: installs or updates it on a running unit over SSH (`NOTION_PASS=... firmware-mods/netparam/tools/install.sh`).
  `--remove` restores the factory web UI from the backup in `/system/etc/netparam-backup/`.
- `tools/test_ui.py`: tests the section in Chromium (Playwright) the way a user would: placement inside Diagnostics,
  no "TTL" on screen, apply, invalid values, IPv6, disable/enable and es/en/cn, checking every change on the device.
- `tools/test_cgi.py`: end-to-end test of the backend (session, CSRF, validation, value change, `fw3 reload`).
  Also checks that the files served to the browser never mention ttl. Leaves the device at value 64.

Change the value without the web UI:
`uci set netparam.main.value=65; uci commit netparam; sh /system/etc/firewall.netparam.sh`.

Show state and packet counters: `sh /system/etc/firewall.netparam.sh status`.

Everything lives in `/system/etc`, which is writable and survives reboots. A factory reset that wipes that overlay
removes it; that is why it should go into the firmware.

Status, where everything is and the history of this feature: `docs/02-plan-homologacion.md`, section 6.
