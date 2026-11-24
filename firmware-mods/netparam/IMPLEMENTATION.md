# Implementation instructions: "Network parameter" (configurable outbound TTL)

**Product:** Notion 5G CPE (H3S P52HL, ASR1901) · **Requested by:** H3S (Colombia)

Please add the files of this package to the next firmware build and send us the new firmware version.
H3S will test it. The files are complete and already tested on a unit with the firmware built 2026-08-17:
nothing needs to be written or changed, only copied.

- `REQUIREMENTS.md` describes how the device must look when it is done.
- `screenshot-netparam.png` shows the result in the web UI: a "Parámetro de red" section at the bottom of
  Router → System Settings → Diagnostics.

## Step 1 — copy `files/` into the image

`files/` mirrors the device filesystem: `files/X` must end up as `/X` on the device, with the same mode.

```
files/system/etc/config/netparam                         new       0644
files/system/etc/config/firewall                         replaces  0644
files/system/etc/firewall.netparam.sh                    new       0755
files/system/etc/www/netparam.cgi                        new       0755
files/system/etc/www/index.html                          replaces  0644
files/system/etc/www/html/router/netparam.html           new       0644
files/system/etc/www/js/panel/router/netparam.js         new       0644
files/system/etc/www/properties/Messages_es.properties   replaces  0644
files/system/etc/www/properties/Messages_en.properties   replaces  0644
files/system/etc/www/properties/Messages_cn.properties   replaces  0644
```

In an OpenWrt buildroot, copy it into the top-level `files/` directory, whose contents go into the root
filesystem of the image as they are:

```
cp -a files/. <openwrt>/files/
```

Or copy it into your root filesystem staging directory before the image is packed.

The 5 files marked "replaces" are the factory files of the 2026-08-17 firmware with a small addition. **If your
current sources of any of these 5 files are different** from that firmware, do not replace them: add the same
change to your version instead, as described in `REQUIREMENTS.md` R2 and R3. The change is:

- `index.html`: 1 `<script>` line;
- `Messages_*.properties`: 15 lines at the end;
- `config/firewall`: 1 section at the end.

## Step 2 — do not change anything else

- Do not add a menu entry: `/system/etc/www/xml/ui_mifi.xml` stays as it is.
- Do not modify `diagnostics.html` or `diagnostics.js`: the new section adds itself to the Diagnostics page.
- Do not edit or translate the files of this package. In particular, the UI text must never contain the
  words "TTL" or "Hop Limit".

## Step 3 — build and send us the version

Build the firmware as usual. The kernel features it needs are already in the current firmware (iptables
`TTL` target, ip6tables `HL` target, `comment` match). Send H3S the new firmware version.
