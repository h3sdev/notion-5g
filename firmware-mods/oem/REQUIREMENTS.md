# Requirements: H3S firmware features (what H3S checks on the delivered firmware)

Network parameter: requirements R0–R12 of the earlier netparam package, unchanged.

## Factory state

- F1. `uci show tunvpn` and `uci show omnicapgw` show the files of this package: both disabled, no server, user,
  password, URL, NAS identifier, RADIUS server, secret, login name, omnicap URL or token.
- F2. `/data/tunvpn/` and `/data/omnicap-gw/` have no certificate, password or generated configuration.
- F3. No firewall rule, ipset or tc class of the captive portal exists (`iptables -S | grep OCGW` is empty): the
  network is open as in the current firmware.
- F4. `/dev/net/tun` can be created: `/proc/misc` lists `tun` after `tunvpn.sh check`/start, using the kernel TUN
  driver (`CONFIG_TUN`).
- F6. `/etc/config/firewall` has the include `ocgw` (path `/usr/sbin/ocgw.sh`, reload 1); the vendor fastpath
  bypass file is writable (`/sys/kernel/fastpath/fp_forward/bypass_fastpath`).
- F5. The captive portal's kernel support is there: `ebtables -t nat -L` and `ip6tables -S` run, and `ebt_mark`,
  `xt_mark`, `xt_mac`, `xt_multiport`, `ip_set_hash_ipmac` are loaded or loadable.

## Web UI

- U1. Router → Advanced Settings shows "VPN de gestión" and "Portal cautivo" right below "VPN", in Spanish, English
  and Chinese; the other menu entries and pages are unchanged.
- U2. Both pages work only with a logged-in web session (the CGIs answer 401 without it and 403 to a POST
  without `X-Requested-With`) and validate every value; passwords, secrets and tokens are never shown.
- U3. Management VPN: the CA certificate is installed from the page; the VPN cannot be enabled without it; after
  entering server, port, protocol, user and password it connects and shows its IP, the traffic and the log.
- U5. Captive portal registration: with the install URL of a location (omnicap panel) and an identifier, the
  page registers the device in omnicap and fills in every setting by itself; the portal runs with the secure
  login and the certificate downloaded from omnicap.
- U4. Captive portal: after entering the portal URL, NAS identifier, RADIUS server/secret and the secure login
  (name, port, certificate downloaded from omnicap or pasted), the page shows "Funcionando", the login URL
  (`https://<name>:<port>/login`), the certificate and the connected clients, and can disconnect a client.

## Behavior

- B1. Provisioned values survive reboot; both services are running within 2 minutes of power-on.
- B2. The VPN reconnects by itself after the mobile link drops or changes (4G/5G).
- B3. A Wi-Fi/LAN client is redirected to the portal, logs in (credential sent over HTTPS), browses with the
  time and speed limits of its package, and is reported to RADIUS (start, interim, stop).
- B4. Firewall reloads/restarts do not open the network: the LAN stays closed (excluded ports and bypass MACs
  apart) until the portal has restored its rules, within 10 seconds.
- B6. If the captive portal is enabled but cannot run, Wi-Fi/LAN clients cannot browse (fail closed) and the page
  says so; at boot the LAN is closed from the first firewall start, not open until the portal starts.
- B7. Enabling the portal or registering it from a browser on a captured port without a bypass MAC is refused
  with a message (the technician would lose the web UI).
- B5. Disabling a feature in the web UI stops it and removes all of its rules.
