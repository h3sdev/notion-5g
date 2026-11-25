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

## Files

- `main.go`: the daemon. Build: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w"`.
- `device/ocgw.sh`: watchdog/boot hook, cron `* * * * * /data/omnicap-gw/ocgw.sh check` (`/etc/rc.d` is read
  only). `ocgw.sh disable` removes every rule (open network again).
- `/data/omnicap-gw/config.json` on the unit (not in git: it has the fleet secret):

```json
{"portal_url": "https://cautivo.h3s-iot.com/portal/<ref>", "nas_identifier": "NOTION5G_LAB_1",
 "radius_server": "<fleet address>", "radius_secret": "<fleet secret>",
 "walled_garden": [], "bypass_macs": ["<management PC>"]}
```

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

Not tested yet: a real phone on the modem Wi-Fi (capture of its internet traffic, measured 512 kbit/s
shaping, Interim-Update with real octets), and production omnicap.

## Missing for production

1. **HTTPS login.** The production portal is HTTPS; posting the credential to `http://192.168.1.1:8002`
   makes Chrome warn ("not secure"). RouterOS uses `https://login.h3s-iot.com` (DNS name -> router) with the
   certificate omnicap distributes (`GET /agent/v1/hotspot-cert/*`). omnicap-gw needs TLS (+~1.5 MB) and a
   dnsmasq entry for that name.
2. **Agent pull.** Today the config is a local file. omnicap needs an `openwrt` platform in `internal/agentapi`
   that answers JSON (portal ref, walled garden, RADIUS, certificate) instead of a RouterOS script.
3. **Production data**: device with `nas_identifier`, `radius_enabled`, the fleet secret and address.
