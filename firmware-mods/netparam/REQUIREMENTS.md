# Requirements: final state of the device

This document defines how a Notion 5G unit (H3S P52HL, ASR1901) must look and behave **after the new
firmware is flashed**, with the "Network parameter" feature (configurable outbound TTL) included.
`IMPLEMENTATION.md` explains how to get there; `OEM-SPEC.md` explains why.

Every requirement has an ID. `tools/verify.sh` checks R0–R8 automatically on a unit; R9–R12 are checked in
the web UI (`tools/test_ui.py` automates them).

The reference base is the firmware built **2026-08-17**. The checksums in `MANIFEST.md5` refer to it.

## R0 — File contents

The files of the image must be **byte-identical** to the ones in `files/` of this package, unless your base
firmware is newer and your own `index.html` / `Messages_*.properties` / `config/firewall` differ. In that case,
the changes described in R2 and R3 must be merged into your versions, and only those files may differ.

`MANIFEST.md5` lists the expected MD5 of every file the system never rewrites at runtime, as absolute device
paths. Check on the unit with `md5sum -c MANIFEST.md5` (busybox), or run `tools/verify.sh`. The two
configuration files (`config/netparam`, `config/firewall`) are not in the manifest, because uci rewrites them
when settings are saved; R3 checks them by value.

## R1 — New files

| Path on the device | Mode | Owner | Purpose |
|---|---|---|---|
| `/system/etc/config/netparam` | 0644 | root | uci configuration, factory default |
| `/system/etc/firewall.netparam.sh` | 0755 | root | applies the iptables/ip6tables rules |
| `/system/etc/www/netparam.cgi` | 0755 | root | web backend (shell CGI, JSON) |
| `/system/etc/www/html/router/netparam.html` | 0644 | root | web UI section markup |
| `/system/etc/www/js/panel/router/netparam.js` | 0644 | root | web UI section logic |

`/www` and `/etc/config` are symlinks into `/system/etc`, so these files are also reachable as
`/etc/config/netparam`, `/www/netparam.cgi`, and so on. Do not create copies elsewhere.

## R2 — Existing web files (modified)

| Path on the device | Required change |
|---|---|
| `/system/etc/www/index.html` | exactly one line `<script type="text/javascript" src="js/panel/router/netparam.js" language="javascript"></script>`, placed right after the line that loads `js/panel/router/sipalg.js` |
| `/system/etc/www/properties/Messages_es.properties` | ends with the 15 `lt_NetParam_*` lines of `merge/i18n/Messages_es.properties` |
| `/system/etc/www/properties/Messages_en.properties` | ends with the 15 `lt_NetParam_*` lines of `merge/i18n/Messages_en.properties` |
| `/system/etc/www/properties/Messages_cn.properties` | ends with the 15 `lt_NetParam_*` lines of `merge/i18n/Messages_cn.properties` |

Everything else in these files stays as in the base firmware.

**Must NOT change** (byte-identical to the base firmware):

- `/system/etc/www/xml/ui_mifi.xml`: no menu entry for this feature;
- `/system/etc/www/html/router/diagnostics.html` and `/system/etc/www/js/panel/router/diagnostics.js`.

## R3 — Configuration

Factory default `/system/etc/config/netparam`:

```
config netparam 'main'
	option enabled '1'
	option value '64'
	option ipv6 '1'
	option iface 'ccinet+'
	option inbound '1'
```

The factory default `/system/etc/config/firewall` ends with this section, in addition to everything already in it:

```
config include 'netparam'
	option type 'script'
	option path '/system/etc/firewall.netparam.sh'
	option reload '1'
	option family 'any'
```

`reload '1'` is mandatory: without it the rules are lost on the first 4G/5G re-attach. A factory reset must
bring both files back to these defaults. Recommended: include `/etc/config/netparam` in the configuration
backup and restore.

## R4 — Kernel support

`/proc/net/ip_tables_targets` lists `TTL` and `/proc/net/ip6_tables_targets` lists `HL`, built in or as
modules. The `comment` match is also needed. All of these are present in the base firmware.

## R5 — Rules after boot

With the factory defaults, right after boot and once the cellular data connection is up, the mangle table has
exactly these 6 rules tagged `netparam` and no others:

```
iptables  -t mangle -S | grep netparam
-A FORWARD -i ccinet+ -m comment --comment netparam -j TTL --ttl-set 64
-A INPUT -i ccinet+ -m comment --comment netparam -j TTL --ttl-set 64
-A POSTROUTING -o ccinet+ -m comment --comment netparam -j TTL --ttl-set 64
ip6tables -t mangle -S | grep netparam
-A FORWARD -i ccinet+ -m comment --comment netparam -j HL --hl-set 64
-A INPUT -i ccinet+ -m comment --comment netparam -j HL --hl-set 64
-A POSTROUTING -o ccinet+ -m comment --comment netparam -j HL --hl-set 64
```

Depending on the configuration:

- `ipv6=0`: no ip6tables rules.
- `inbound=0`: only the POSTROUTING rules.
- `enabled=0`: no rules at all.
- The number after `--ttl-set` / `--hl-set` is always `netparam.main.value`.

## R6 — The value is on the wire

- Packets leaving through `ccinet+` carry TTL / Hop Limit = `value`. Checked with a capture on `ccinet0`: with
  value 70, the outgoing ICMP echo requests have TTL 70.
- With `inbound=1`, `ping 8.8.8.8` on the device (and from a LAN client) shows `ttl=<value>`.

## R7 — Persistence

The rules come back by themselves, with the stored value:

- after `fw3 reload`;
- after every cellular re-attach (4G↔5G, signal loss, SIM change);
- after a reboot.

The value set in the web UI survives a reboot. Nothing is written to flash automatically: only saving from
the web UI rewrites `/etc/config/netparam`.

## R8 — Nothing visible names the feature

- Visible UI text, in every language: no "TTL" and no "Hop Limit".
- Served files `netparam.js` and `netparam.html`: no "ttl" and no "hop limit", not even in comments.
- JSON returned by `netparam.cgi`: none of these words either.
- Every internal name is `netparam`: files, uci config `netparam` (option `value`), fw3 include
  `firewall.netparam`, iptables comment `netparam`, syslog tag `netparam`, element ids `NetParam*`,
  string keys `lt_NetParam_*`.

## R9 — Web UI location and look

Router → System Settings (Configuración del sistema) → **Diagnostics (Diagnóstico)**. Below the existing
ping / nslookup / traceroute form there is a section titled **"Parámetro de red"** (Spanish, the main UI
language). `screenshot-netparam.png` shows it. Its contents, top to bottom:

- a short note;
- checkbox "Activar parámetro de red";
- field "Valor:" with the hint "(1-255, recomendado 64)";
- checkbox "Aplicar también a IPv6";
- checkbox "Aplicar también al tráfico de entrada (para verlo al hacer ping)";
- line "Estado:" with the value and packet counters;
- buttons "Como de fábrica", "Valor recomendado" and "Aplicar".

There is no separate menu entry. The ping / nslookup / traceroute function of the page works as before.

Languages: the strings are exactly those of `merge/i18n/`. In English the section is "Network parameter";
in Chinese, "网络参数". Switching the language with the top selector translates the whole section, including
the status line.

## R10 — Web UI behaviour

- The section loads the current values from the device.
- **Aplicar:**
  - it is disabled (grey) until a field changes, and disabled again after a successful save;
  - it validates the value: an integer from 1 to 255. Anything else shows "El valor debe ser un número entre
    1 y 255." and changes nothing;
  - it saves, applies at once and shows the firmware's success dialog.
- **Valor recomendado:** asks Yes/No; "Yes" sets enabled, 64, IPv6 on and incoming on.
- **Como de fábrica:** asks Yes/No; "Yes" disables the feature and removes all its rules, so each device
  keeps its own TTL, as in the original firmware. The stored value goes back to 64.
- Unchecking "Activar parámetro de red" greys out the other fields.

## R11 — Backend (`/www/netparam.cgi`)

- **Session:** it accepts only a logged-in web session, the same `CGISID` cookie as the rest of the UI. A
  request without a valid session gets HTTP 401.
- **GET** returns `{"result":"ok","enabled":…,"value":…,"ipv6":…,"inbound":…,"iface":…,"active_v4":…,
  "active_v6":…,"active_in":…,"pkts_v4":…,"pkts_v6":…}`.
- **POST** `enabled=0|1&value=1..255&ipv6=0|1&inbound=0|1`:
  - it validates strictly and returns 400 on any invalid value, without changing anything;
  - leading zeros are accepted and never read as octal (`064` is 64);
  - it saves with uci, applies, and returns the same JSON as GET.
- **POST** `action=restore` / `action=remove`: the two buttons of R10. Any other `action` returns 400.
- **CSRF:** a POST without the `X-Requested-With` header returns 403.
- **Logging:** every save is logged to syslog with the tag `netparam`.

## R12 — Factory reset

After a factory reset, R3 (defaults) and R5 (6 rules, value 64) hold again.
