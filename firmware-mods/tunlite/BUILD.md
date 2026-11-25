# tunlite + OpenVPN on the Notion 5G stock firmware

The stock firmware (OpenWrt 24.10-SNAPSHOT, kernel `5.4.288+`, target `mmp/asr1901`) has no `CONFIG_TUN`, no
`tun.ko` and no `openvpn` binary. The web UI help text mentions OpenVPN, but the VPN page only offers
PPTP/L2TP/GRE. This directory adds a management OpenVPN tunnel without a new firmware:

- `src/tunlite.c`: a minimal `/dev/net/tun` (layer 3 TUN only: `TUNSETIFF` with `IFF_TUN`/`IFF_NO_PI`,
  `TUNGETIFF`, `TUNSETPERSIST`, `TUNGETFEATURES`; read/write/poll). It is enough for OpenVPN and `ip tuntap`.
- `bin/tunlite.ko`, `bin/openvpn`: the built module and OpenVPN 2.6.23 (mbed TLS 3.6.8, libcap-ng 0.8.5,
  only libc/libgcc_s dynamic, 710 KB).
- `device/`: what goes to `/data/tunvpn` on the unit (`tunvpn.sh`, `client.conf`, `ca.crt`; `auth.txt` is
  per unit and is not in git).
- `tools/install.sh`: installs over SSH (`NOTION_PASS=... tools/install.sh <auth.txt>`, `--remove` undoes it).

The proper fix is still for the manufacturer to enable `CONFIG_TUN=m` and ship `openvpn` in the image; then
`tunlite.ko` is no longer needed.

## State on the unit (2026-11-24)

- `/data/tunvpn/` with the files above; cron line `* * * * * /data/tunvpn/tunvpn.sh check`
  (`/etc/rc.d` is read-only, so cron is the boot hook and the watchdog: it loads the module and starts
  OpenVPN within one minute of boot or of a crash).
- Server: `onmicap-lab-openvpn` on h3s-dev (`~/openvpn-lab-server`), TCP/1194 published at
  `190.145.155.2`. User `notion5g-modem` added to `user-pass.txt` (host file and the running container).
  The unit gets `10.77.88.3/24`; from h3s-dev `ping`/SSH/HTTP to `10.77.88.3` work. The tunnel leaves through
  the SIM (`ccinet0`), `route-nopull`, so user traffic is not affected.
- Verified: insmod, `ip tuntap add/del`, OpenVPN loopback test (two instances on the unit), real tunnel,
  watchdog restart after `rmmod` + killing OpenVPN, no oops in `dmesg`. Each OpenVPN uses ~1.3 MB RSS.
- Not tested yet: a full reboot of the unit.

## Why a module could be built at all

Stock modules have no `__versions` (no `CONFIG_MODVERSIONS`) and are unsigned, so the kernel only checks the
vermagic: `5.4.288+ SMP preempt mod_unload ARMv7 thumb2 p2v8`. The hard part is that the vendor changed
structures, and a module built from upstream headers would corrupt memory. The layout was recovered from the
unit itself and checked instruction by instruction:

1. Toolchain: OpenWrt 24.10.8 `sunxi/cortexa7` toolchain = the same `arm-openwrt-linux-muslgnueabi-gcc 13.3.0`
   that built the stock kernel (`/proc/version`).
2. Tree: `linux-5.4.288` + OpenWrt 21.02 `target/linux/generic/{backport,pending,hack}-5.4` patches
   (482 apply; the 8 that fail are backports already in .288), config `kernel/config-asr1901-5.4.288`.
3. Reference: the 200 stock modules (`/lib/modules/5.4.288+`), `/proc/kallsyms` (real addresses) and the kernel
   image read from `/dev/mtdblock11` (Marvell TIMH header, zImage at 0x1000, gzip payload at 0x6354, loads at
   `0xc0008000`). `kernel/kdis.sh <func>` disassembles any stock kernel function (needs `REF` with
   `kallsyms.txt` and `vmlinux.elf` made with `objcopy -I binary --change-addresses 0xc0008000`).
4. Compare: `kernel/cmpmod.py` (per-function diff of a stock `.ko` vs our `.o`), `kernel/pairs.py`
   (offset correspondences), `kernel/probe_gen.py` (`offsetof` table of our tree).
5. `kernel/apply-vendor-layout.py <tree>` applies what was found:

| Structure | Vendor difference | Proof |
|---|---|---|
| `sk_buff` | +20 bytes after `headers_end` (before `tail`) | `xt_mac.ko` identical |
| `struct module` | `module_kobject` +36, `arch.unwind[3]`, size 512 | `init`/`exit` relocs at 0x108/0x1b4 like stock |
| `net_device` | `NETFILTER_INGRESS`; +24 after `broadcast`; aligned size 1792 | `__dev_open`, `dev_get_stats`, `dev_set_mtu_ext` identical; `alloc_netdev_mqs` |
| config | no PAN, no stack protector, `-O2`, `JUMP_LABEL`, `ARM_MODULE_PLTS`, `HZ=1000` | `ppp_write` identical |

Still different and therefore never touched by `tunlite.c`: fields of `net_device` after `broadcast`
(including `netdev_priv()`), `netdev_queue`, `Qdisc`, `struct sock`, `inode`, `task_struct`, one bit of the
second `sk_buff` bitfield group. The ops tables and the `miscdevice` are followed by zero padding in case the
vendor appended members. `__kmalloc` is used instead of `kzalloc` (which would inline an index into
`kmalloc_caches[]`).

Build the module:

```sh
export PATH=<toolchain>/bin:$PATH STAGING_DIR=/tmp
make -C <tree> ARCH=arm CROSS_COMPILE=arm-openwrt-linux-muslgnueabi- olddefconfig modules_prepare
make -C <tree> ARCH=arm CROSS_COMPILE=arm-openwrt-linux-muslgnueabi- M=$PWD/src KBUILD_MODPOST_WARN=1 modules
```

Before loading a rebuilt module check: vermagic as above, `.gnu.linkonce.this_module` size 0x200 with init at
0x108 and exit at 0x1b4, `.plt`/`.init.plt` present, and every `nm -u` symbol present in the unit's
`/proc/kallsyms` (data symbols are not listed there; avoid them).

OpenVPN: mbed TLS `make lib` with `-Os -march=armv7-a -mthumb`; libcap-ng `--enable-static --disable-shared`;
OpenVPN `--with-crypto-library=mbedtls --disable-lzo --disable-lz4 --disable-plugins --disable-dco
--disable-management` with a stub `pkg-config` and `MBEDTLS_*`/`LIBCAPNG_*` set by hand.
