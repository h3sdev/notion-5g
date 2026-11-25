# Implementation instructions: H3S firmware features

**Product:** Notion 5G CPE (H3S P52HL, ASR1901) · **Requested by:** H3S (Colombia)

This package adds three features to the firmware. It **replaces** the earlier `netparam-oem-*.zip` (the network
parameter files are included here, unchanged), because the three features change the same web UI files.

| Feature | Where in the web UI | What it does |
|---|---|---|
| Network parameter | Router → System Settings → Diagnostics, section "Parámetro de red" | configurable value applied to outbound mobile traffic (as before) |
| Management VPN | Router → Advanced Settings → "VPN de gestión" (below "VPN") | OpenVPN client to the H3S management server |
| Captive portal | Router → Advanced Settings → "Portal cautivo" (below "VPN") | Wi-Fi/LAN clients must log in on the H3S portal (RADIUS) before browsing |

Provisioning of the captive portal is one step in the web UI: the technician pastes the install URL of the
location (from the H3S omnicap panel) and the device identifier, presses "Registrar en omnicap", and the device
registers itself and receives all its settings (this needs the omnicap version with OpenWrt support).

The firmware must ship **without any H3S data**: no VPN server, user, password or CA certificate, and no portal
URL, RADIUS server, secret or certificate. H3S enters all of that in the web UI when the unit is provisioned.
The default configuration files in this package are already empty; do not fill them in.

Everything was tested on a unit with the firmware built 2026-08-17 (kernel built 2026-08-19): provisioning from
an empty configuration using only the web UI, client logins, reboots, firewall reloads and 4G/5G changes.
Screenshots of the two new pages (after provisioning) are in `screenshots/`.

## Step 1 — kernel: enable TUN (required)

The management VPN needs the kernel TUN driver (`/dev/net/tun`), which the current kernel does not have.
Please enable it:

```
CONFIG_TUN=m        (or =y)
```

and include the module in the image (`kmod-tun` in OpenWrt). Nothing else in the kernel changes.

The package also contains `tunlite.ko`, a small TUN driver H3S built for the **current** kernel only
(`5.4.288+ #1 SMP PREEMPT Wed Aug 19 11:26:06 UTC 2026`). It is used only if the kernel has no TUN driver and
the running kernel is exactly that build; on any other kernel it is never loaded. If you enable `CONFIG_TUN`,
you may leave `tunlite.ko` out.

## Step 2 — copy `files/` into the image

`files/` mirrors the device filesystem: `files/X` must end up as `/X` on the device, with the same mode.

```
files/usr/sbin/omnicap-gw                                new       0755   captive portal daemon (static ARM binary)
files/usr/sbin/openvpn                                   new       0755   OpenVPN 2.6.23 (mbed TLS 3.6.8, needs only libc and libgcc_s)
files/usr/sbin/ocgw.sh                                   new       0755   start/stop/watchdog of the captive portal
files/usr/sbin/tunvpn.sh                                 new       0755   start/stop/watchdog of the management VPN
files/lib/modules/5.4.288+/tunlite.ko                    new       0644   only for the current kernel, see Step 1
files/system/etc/config/omnicapgw                        new       0644   captive portal settings (empty)
files/system/etc/config/tunvpn                           new       0644   management VPN settings (empty)
files/system/etc/config/netparam                         new       0644   network parameter (as before)
files/system/etc/config/firewall                         replaces  0644   network parameter (as before)
files/system/etc/firewall.netparam.sh                    new       0755   network parameter (as before)
files/system/etc/crontabs/root                           replaces  0644   + 2 lines
files/system/etc/www/omnicapgw.cgi                       new       0755
files/system/etc/www/tunvpn.cgi                          new       0755
files/system/etc/www/netparam.cgi                        new       0755
files/system/etc/www/html/router/omnicapgw.html          new       0644
files/system/etc/www/html/router/tunvpn.html             new       0644
files/system/etc/www/html/router/netparam.html           new       0644
files/system/etc/www/js/panel/router/omnicapgw.js        new       0644
files/system/etc/www/js/panel/router/tunvpn.js           new       0644
files/system/etc/www/js/panel/router/netparam.js         new       0644
files/system/etc/www/index.html                          replaces  0644   + 3 <script> lines
files/system/etc/www/xml/ui_mifi.xml                     replaces  0644   + 2 menu entries
files/system/etc/www/properties/Messages_es.properties   replaces  0644   + strings
files/system/etc/www/properties/Messages_en.properties   replaces  0644   + strings
files/system/etc/www/properties/Messages_cn.properties   replaces  0644   + strings
```

In an OpenWrt buildroot, copy it into the top-level `files/` directory, whose contents go into the root
filesystem of the image as they are:

```
cp -a files/. <openwrt>/files/
```

Or copy it into your root filesystem staging directory before the image is packed.

The files marked "replaces" are the factory files of the 2026-08-17 firmware with additions. **If your current
sources of any of them are different** from that firmware, do not replace them: run the two scripts in `merge/`
on your root filesystem tree instead. They add the new files and merge the same additions into your versions:

```
merge/netparam/apply-to-rootfs.sh   <rootfs>
merge/vpn-portal/apply-to-rootfs.sh <rootfs>
```

The additions are:

- `index.html`: 3 `<script>` lines (netparam.js after `sipalg.js`; tunvpn.js and omnicapgw.js after `vpn.js`);
- `xml/ui_mifi.xml`: 2 lines after the `mVPN` entry:
  `<ThirdlevelMenu id='mMgmtVPN' implFunction='objMgmtVPN' type='submenuabsent'/>` and
  `<ThirdlevelMenu id='mCaptivePortal' implFunction='objCaptivePortal' type='submenuabsent'/>`;
- `Messages_*.properties`: the lines from `merge/*/i18n` at the end;
- `config/firewall`: 1 section at the end (network parameter, as before);
- `crontabs/root`: 2 lines: `* * * * * /usr/sbin/tunvpn.sh check` and `* * * * * /usr/sbin/ocgw.sh check`.
  These start both services at boot (within one minute) and restart them if they stop. They do nothing while
  the feature is disabled in the web UI, which is the factory state.

## Step 3 — do not change anything else

- Do not edit, rebuild or translate the files of this package (the UI strings are already in es/en/cn).
- Do not put any server, user, password, certificate, URL or secret in the image.
- The services write only to `/data/tunvpn/` and `/data/omnicap-gw/` (created at run time) and to `/tmp`.
- Factory reset must restore `/system/etc/config/tunvpn` and `/system/etc/config/omnicapgw` to the files of this
  package and delete `/data/tunvpn/` and `/data/omnicap-gw/` (they hold the provisioned certificates).

## Step 4 — build and send us the version

Build the firmware as usual and send H3S the new firmware version. H3S will verify it with its own test tools.
