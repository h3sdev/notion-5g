# omnicap-gw: omnicap captive portal on the Notion 5G

`omnicap-gw` makes the Notion 5G behave like the RouterOS hotspot that omnicap drives on the bus MikroTiks,
so the unit is an omnicap NAS in **fleet mode** (RADIUS over the internet, identified by `NAS-Identifier`,
shared fleet secret). One Go binary (3.7 MB, ~4 MB RSS, no net/http), no kernel changes.

| RouterOS hotspot (omnicap) | omnicap-gw |
|---|---|
| `login.html` trampoline with `$(mac)`, `$(link-login-only)`, ... | same 12 fields, values filled by the daemon (`/login`) |
| `alogin.html` trampoline to `/conectado` with `link_redirect` | same, after Access-Accept |
| `login-by=http-pap`: browser POSTs `username`/`password`/`dst` | `POST http://<router>:8002/login` |
| `/radius` to the fleet address, `NAS-Identifier` = identity | PAP + Message-Authenticator, `NAS-Identifier`, `Calling-Station-Id` (MAC upper case), `Framed-IP-Address`, `NAS-Port-Type` Wireless-802.11, `Called-Station-Id` hotspot1 |
| Accounting Start / Interim / Stop, `Class` echoed | same, octets from iptables counters, gigawords, `Acct-Terminate-Cause` |
| `Session-Timeout`, `Idle-Timeout`, `Acct-Interim-Interval` | enforced by the daemon (10 s tick) |
| `Mikrotik-Rate-Limit` rx/tx | HTB class per client: download under the `br-lan` root, upload via fwmark under the `ccinet0` root |
| `Mikrotik-Total-Limit`, `Mikrotik-Xmit-Limit` (+ gigawords) | enforced locally, Stop with NAS-Request |
| walled garden | `ocgw_wg` ipset (hosts resolved every 5 min; dnsmasq here has no ipset support) |

Firewall: own chains `OCGW_FWD`/`OCGW_ACCT` (filter), `OCGW_NAT` (nat: HTTP -> 8002, DNS -> router),
`OCGW_MARK` (mangle), ipsets `ocgw_auth` (hash:mac), `ocgw_wg`, `ocgw_bypass`. Unauthenticated TCP gets a
reset, the rest is rejected. Traffic to the router itself (SSH, web UI, DNS) and LAN-to-LAN is never touched.
If fw3 flushes the tables (`firewall restart`), the daemon puts everything back within 10 s; sessions are kept
in `/tmp/omnicap-gw.state` (survive a daemon restart, not a reboot: after a reboot the user registers again
and omnicap hands back the same credential with the remaining time).

The vendor fastpath is bypassed on this firmware (`/sys/kernel/fastpath/fp_forward/bypass_fastpath` = 1),
so iptables and tc see every forwarded packet. The vendor QoS daemon owns the HTB roots; omnicap-gw only adds
classes 0x101.. under them and never writes `/etc/config/qos`.

## Web UI (configured like netparam)

**Router > Configuraciones avanzadas > Portal cautivo** (es/en/cn), right below the factory "VPN" entry:
enable, portal URL, NAS-Identifier, RADIUS server/ports/secret, secure login (HTTPS name/port, certificate
downloaded from omnicap or pasted as PEM), walled garden, MACs that bypass the portal, live status (login URL,
last RADIUS error, certificate subject/issuer/expiry) and the connected clients with a "Desconectar" button
(RADIUS Stop, Admin-Reset). Settings are stored in uci `omnicapgw` (`/etc/config/omnicapgw`); the RADIUS secret
and the agent token are write-only (never returned by the CGI, blank keeps them).

## Secure login (HTTPS)

Like RouterOS with the omnicap certificate: the trampoline's `link_login_only` becomes
`https://login.h3s-iot.com:8443/login` (443 is the web UI), the daemon serves TLS 1.2+ there with a certificate
it reloads on change, and dnsmasq answers `login.h3s-iot.com` with the router address (`/tmp/hosts/omnicap-gw`,
nothing persistent). The certificate comes from the omnicap agent exactly as on the MikroTiks
(`GET <agent>/agent/v1/hotspot-cert/onmicap-portal.{crt,key}`, `Authorization: Bearer <device agent_token>`,
checked again every 6 h) or is pasted in the page. It is installed only if the key matches and it is valid for
the login name. HTTPS is used only when a valid certificate is loaded; otherwise the login falls back to HTTP.

## Files

- `main.go`, `tlsadmin.go`: the daemon. Build: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath
  -ldflags="-s -w"` (6.4 MB; no net/http).
- `device/ocgw.sh`: watchdog/boot hook, cron `* * * * * /data/omnicap-gw/ocgw.sh check` (`/etc/rc.d` is read
  only); `restart` reloads uci; `disable` removes every rule (open network again).
- `rootfs/system/etc/`: `config/omnicapgw` (defaults), `www/omnicapgw.cgi`, `www/html/router/omnicapgw.html`,
  `www/js/panel/router/omnicapgw.js`; `i18n/Messages_{es,en,cn}.properties`.
- `tools/install.sh <binary>` / `--remove`: installs over SSH (menu entry, script tag and strings through
  `../lib/webui.sh`, which only touches its own lines). Never keeps two copies of the binary in `/data`.
- `tools/test_cgi.py [--apply cfg.json]`: session/CSRF/validation checks of the CGI, optionally saves a config.
- `../lib/test_ui_modules.py`: drives both pages (portal and management VPN) in Chromium.

## Tested (2026-11-25) against a local copy of omnicap

omnicap `8c8f23e` running on Diego's PC (WSL + Docker: Postgres 18, FreeRADIUS 3.2.10 with the repo's
config, `onmicap-api --role all`), location `notion5g-lab` with registration splash and free package
24 h 512/512, device in fleet mode (`nas_identifier=NOTION5G_LAB_1`, `radius_source_ip` NULL,
`device_config.radius_enabled=true`). Client: a browser (Chromium) on the PC, which is on the modem LAN.

- trampoline -> registration -> credential POST to the modem -> Access-Accept (Session-Timeout 86400,
  Idle-Timeout 300, Mikrotik-Rate-Limit 512k/512k) -> `/conectado`; Accounting-Start stored with Class and
  voucher.
- logout -> Stop (User-Request); re-login with the same voucher -> remaining Session-Timeout.
- `firewall reload` keeps the rules; `firewall restart` -> rules and session back within 10 s.
- daemon restart keeps the session; full reboot: portal and VPN come back alone, same voucher reissued.

- a real Android phone (moto g6 plus, captive-portal WebView) registered through the modem Wi-Fi; omnicap got
  Interim-Updates with real octets (8.4 MB up / 3.5 MB down in 15 min).

2026-11-25, web UI and HTTPS (same local omnicap; certificate from a lab CA served by its agent):
`test_cgi.py` 18/18 (401 without session, 403 without X-Requested-With, secrets never returned, 10 invalid
inputs rejected), `test_ui_modules.py` 20/20 (both pages, live status, kick, VPN reconnect, es/en/cn, no JS
errors), credential POST to `https://login.h3s-iot.com:8443/login` (TLS 1.3, chain verified), full reboot with
everything configured from the UI.

## Missing for production

1. **Production data**: device with `nas_identifier`, `device_config.radius_enabled=true`, splash, the fleet
   secret/address, and an `agent_token` for the certificate download. Then set the page to the production URLs.
2. **Agent pull** (optional): an `openwrt` platform in omnicap `internal/agentapi` answering JSON, so portal ref
   and walled garden come from the panel instead of the page.
3. `/data` has ~1.7 MB free with everything installed (the old `routeragent-arm.bak` takes 6.9 MB).
