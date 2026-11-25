#!/usr/bin/env python3
"""analyze.py - summary of a ratwatch/pc_watch field run.
Usage: python3 scripts/field/analyze.py [results/field-YYYYMMDD-HHMM]   (default: results/field-latest)
Reports: modem reboots, RAT/cell/IP changes, whether the netparam rules were present (and how long they were
missing) around each change, the TTL seen by ping, data gaps, and the firewall reload -> netparam apply
delay from syslog."""
import csv, glob, os, re, sys
from datetime import datetime

root = sys.argv[1] if len(sys.argv) > 1 else open("results/field-latest").read().strip()
fmt = lambda t: datetime.fromtimestamp(t).strftime("%H:%M:%S")

ups = [l.strip().split(",") for l in open(os.path.join(root, "modem_uptime.csv"))] if os.path.exists(os.path.join(root, "modem_uptime.csv")) else []
boots = sorted(glob.glob(os.path.join(root, "modem", "boot-*")))
print(f"== run {root}: {len(boots)} modem boot(s) seen")
for i in range(1, len(ups)):
    if float(ups[i][1]) < float(ups[i - 1][1]):
        print(f"  !! MODEM REBOOT between {fmt(int(ups[i-1][0]))} and {fmt(int(ups[i][0]))}")

for b in boots:
    f = os.path.join(b, "samples.csv")
    if not os.path.exists(f):
        continue
    rows = list(csv.DictReader(open(f)))
    if not rows:
        continue
    print(f"\n== {os.path.basename(b)}: {len(rows)} samples {fmt(int(rows[0]['ts']))} -> {fmt(int(rows[-1]['ts']))}")
    exp = lambda r: (3, 3) if r["np_enabled"] == "1" else (0, 0)
    state = lambda r: ("5G-NSA" if r["nr_band"] not in ("", "0") else "4G") + f" B{r['lte_band']}/{r['lte_pci']}" + (f" n{r['nr_band']}/{r['nr_pci']}" if r["nr_band"] not in ("", "0") else "")
    prev, gap_start, bad_start, t_by = None, None, None, {}
    for i, r in enumerate(rows):
        t = int(r["ts"])
        s = state(r)
        t_by[s.split()[0]] = t_by.get(s.split()[0], 0) + (t - int(rows[i - 1]["ts"]) if i else 0)
        if i and t - int(rows[i - 1]["ts"]) > 15:
            print(f"  gap of {t - int(rows[i-1]['ts'])} s in samples before {fmt(t)}")
        if prev is None or s != prev[0] or r["ip"] != prev[1]:
            print(f"  {fmt(t)}  {s:28} ip={r['ip'] or '-':18} lte_rsrp={r['lte_rsrp']} nr_rsrp={r['nr_rsrp'] or '-'} rules={r['rules4']}/{r['rules6']} ttl={r['ping_ttl'] or '-'}")
            prev = (s, r["ip"])
        ok = (int(r["rules4"] or 0), int(r["rules6"] or 0)) == exp(r)
        if not ok and bad_start is None:
            bad_start = t; print(f"  !! {fmt(t)} rules {r['rules4']}/{r['rules6']} (expected {exp(r)}) value={r['np_value']}")
        if ok and bad_start is not None:
            print(f"     rules back after {t - bad_start} s"); bad_start = None
        if r["ping_ttl"] and r["np_enabled"] == "1" and r["ping_ttl"] != r["np_value"]:
            print(f"  !! {fmt(t)} ping ttl={r['ping_ttl']} but value={r['np_value']} ({s})")
        if not r["ping_ttl"] and gap_start is None:
            gap_start = t
        if r["ping_ttl"] and gap_start is not None:
            if t - gap_start > 3:
                print(f"  no internet from the modem {fmt(gap_start)} -> {fmt(t)} ({t - gap_start} s)")
            gap_start = None
    if bad_start is not None:
        print(f"  !! rules still wrong at end of run (since {fmt(bad_start)})")
    print("  time per mode: " + ", ".join(f"{k} {v//60}m{v%60:02d}s" for k, v in t_by.items()))
    nr = [r for r in rows if r["nr_band"] not in ("", "0")]
    if nr:
        best = max(nr, key=lambda r: int(r["nr_rsrp"] or -200))
        print(f"  best NR: n{best['nr_band']} PCI {best['nr_pci']} RSRP {best['nr_rsrp']} SINR {best['nr_sinr']} at {fmt(int(best['ts']))}")
    else:
        print("  NR never attached in this boot")

    sl = os.path.join(b, "syslog.log")
    if os.path.exists(sl):
        reload_t = None
        for line in open(sl, errors="replace"):
            m = re.match(r"\w{3} (\w{3} +\d+ [\d:]+) \d{4}", line)
            if not m:
                continue
            if "Reloading firewall" in line:
                reload_t = m.group(1); print(f"  syslog {reload_t}  {line.split(': ',1)[-1].strip()}")
            elif "netparam" in line or "reboot" in line.lower() or "usim" in line.lower():
                print(f"  syslog {m.group(1)}  {line.split(': ',1)[-1].strip()[:110]}")

pc = os.path.join(root, "pc_ping.csv")
if os.path.exists(pc):
    rows = list(csv.DictReader(open(pc)))
    lost = [r for r in rows if not r["inet_ttl"]]
    print(f"\n== PC side: {len(rows)} pings, {len(lost)} without internet reply, {sum(1 for r in rows if not r['lan_ms'])} without modem LAN reply")
    ttls = sorted({r["inet_ttl"] for r in rows if r["inet_ttl"]})
    print(f"  TTL of 8.8.8.8 replies seen by the PC: {ttls}  (the value, e.g. 64, = went through the modem with"
          " inbound=1; anything else = the PC used another route: Wi-Fi/VPN)")
