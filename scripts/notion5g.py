#!/usr/bin/env python3
"""
notion5g.py — automatización de pruebas para el módem Notion 5G (ASR1901 / OpenWrt).

Subcomandos:
  status   Snapshot de radio/red del módem (operador, RAT, banda, PCI, RSRP, RSRQ, SINR, IP, temp).
  speed    Prueba de velocidad HTTP multi-hilo (Cloudflare) desde ESTE PC, más snapshot del módem.
           Antes comprueba (contadores de ccinet0) que el tráfico del PC sale por el módem; aborta si no (--force).
  drive    Bucle de campo: cada N segundos guarda snapshot + latencia + posición GPS externa (CSV).
  at        Envía un comando AT crudo (serial_atcmd) y muestra la respuesta.
  ubus      Llama un método ubus: notion5g.py ubus cm get_zcainfo
  heartbeat Ping ligero y periódico (uptime del módem + señal, sin velocidad) al servidor de reporte.

Todo sale por SSH (scripts/rsh.sh). Resultados en results/ (CSV + JSONL) y, si se configura,
también se reportan a un backend HTTP (server/, en Go) para consolidar múltiples PCs/operadores/sitios.

Vars de entorno:
  NOTION_HOST, NOTION_USER, NOTION_PASS   Acceso SSH al módem (ver rsh.sh).
  NOTION_TAG                              Etiqueta libre (operador/sitio) para cada muestra.
  NOTION_SPEEDTEST                        Ruta al CLI de Ookla si no está en PATH.
  NOTION_REPORT_URL                       Base URL del backend (server/), p. ej. http://192.168.1.50:8080
                                           Si no está definida, no se reporta nada (solo CSV/JSONL locales).
  NOTION_API_KEY                          X-API-Key del backend (debe igualar API_KEY del server).
  NOTION_DEVICE_ID                        Identificador de este PC/equipo (default: hostname).
"""
import argparse
import csv
import datetime as dt
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
RSH = os.path.join(HERE, "rsh.sh")
RESULTS = os.path.join(ROOT, "results")

RAT_NAMES = {0: "GSM", 1: "GSM-C", 2: "UTRAN", 3: "EDGE", 4: "HSDPA", 5: "HSUPA",
             6: "HSPA", 7: "LTE", 10: "NR-NSA(EUTRAN)", 11: "NR-5GCN", 12: "NG-RAN", 13: "NR-NSA"}

# ------------------------------------------------------------------ SSH helpers

def rsh(cmd, timeout=40):
    """Ejecuta cmd en el módem. Devuelve stdout (str). Lanza si falla la conexión."""
    p = subprocess.run([RSH, cmd], capture_output=True, text=True, timeout=timeout)
    if p.returncode == 255:
        raise RuntimeError("SSH al módem falló: " + p.stderr.strip())
    return p.stdout


def at(cmd, timeout=15):
    out = rsh(f'serial_atcmd "{cmd}"', timeout=timeout)
    return [l.strip() for l in out.replace("\r", "").splitlines() if l.strip()]


def ubus(obj, method, args=None, timeout=15):
    a = json.dumps(args) if args else ""
    out = rsh(f"ubus -t 8 call {obj} {method} '{a}'" if a else f"ubus -t 8 call {obj} {method}", timeout)
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return {"_raw": out.strip()}

# ------------------------------------------------------------------ conversions (3GPP 27.007)

def cesq_rsrp_dbm(idx):
    return None if idx in (None, 255) else -141 + idx

def cesq_rsrq_db(idx):
    return None if idx in (None, 255) else round(-19.5 + idx * 0.5, 1)

def cesq_rssi_dbm(idx):
    return None if idx in (None, 99) else -111 + idx  # +CESQ rxlev 0..63

# ------------------------------------------------------------------ status

STATUS_REMOTE = r'''
echo "@@ZCA"; ubus -t 6 call cm get_zcainfo 2>/dev/null
echo "@@CESQ"; ubus -t 6 call version get_cesq 2>/dev/null
echo "@@MODE"; ubus -t 6 call util_wan get_network_mode 2>/dev/null
echo "@@SIM"; ubus -t 6 call sim get_double_sim_status 2>/dev/null
echo "@@COPS"; serial_atcmd "AT+COPS?"
echo "@@CEREG"; serial_atcmd "AT+CEREG?"
echo "@@C5GREG"; serial_atcmd "AT+C5GREG?"
echo "@@BANDIND"; serial_atcmd "at*bandind?"
echo "@@CGPADDR"; serial_atcmd "AT+CGPADDR"
echo "@@ROUTE"; ip -4 route show default
echo "@@TEMP"; cat /sys/class/thermal/thermal_zone0/temp
echo "@@LOAD"; cat /proc/loadavg
echo "@@UPTIME"; cat /proc/uptime
echo "@@TTL"; iptables -t mangle -S POSTROUTING 2>/dev/null | grep -c -- "-j TTL"
echo "@@END"
'''

# Solo la parte AT: se reintenta una vez si serial_atcmd devolvió vacío/ERROR (canal AT ocupado bajo carga).
STATUS_AT_REMOTE = "\n".join(l for l in STATUS_REMOTE.splitlines() if "serial_atcmd" in l)


def split_sections(text):
    sec, cur = {}, None
    for line in text.replace("\r", "").splitlines():
        if line.startswith("@@"):
            cur = line[2:].strip()
            sec[cur] = []
        elif cur:
            sec[cur].append(line)
    return {k: "\n".join(v).strip() for k, v in sec.items()}


def jload(s):
    try:
        return json.loads(s) if s else {}
    except json.JSONDecodeError:
        return {}


def get_status():
    raw = rsh(STATUS_REMOTE, timeout=60)
    s = split_sections(raw)
    if "+COPS:" not in s.get("COPS", "") or "+CEREG:" not in s.get("CEREG", ""):
        time.sleep(1.5)
        s2 = split_sections(rsh(STATUS_AT_REMOTE, timeout=40))
        s.update({k: v for k, v in s2.items() if v.strip()})
        s["AT_RETRY"] = "1"
    zca = jload(s.get("ZCA")).get("lte", {})
    cesq = jload(s.get("CESQ")).get("product", {})
    mode = jload(s.get("MODE")).get("net_mode", {})
    sim = jload(s.get("SIM")).get("sim", {})

    st = {"ts": dt.datetime.now().isoformat(timespec="seconds"),
          "tag": os.environ.get("NOTION_TAG", "")}

    m = re.search(r'\+COPS: (\d+),(\d+),"([^"]*)",(\d+)', s.get("COPS", ""))
    if m:
        st["operator"] = m.group(3)
        st["act"] = int(m.group(4))
        st["rat"] = RAT_NAMES.get(int(m.group(4)), str(m.group(4)))
    m = re.search(r'\+CEREG: \d+,(\d+)(?:,"([0-9a-fA-F]+)","([0-9a-fA-F]+)",(\d+))?', s.get("CEREG", ""))
    if m:
        st["eps_reg"] = int(m.group(1))
        if m.group(2):
            st["tac_hex"] = m.group(2)
            st["eci_hex"] = m.group(3)
            eci = int(m.group(3), 16)
            st["enb_id"] = eci >> 8
            st["cell_sector"] = eci & 0xFF
    m = re.search(r'\+C5GREG: \d+,(\d+)(?:,"([0-9a-fA-F]+)","([0-9a-fA-F]+)",(\d+))?', s.get("C5GREG", ""))
    if m:
        st["nr_reg"] = int(m.group(1))
        if m.group(3):
            st["nr_tac_hex"], st["nr_ci_hex"] = m.group(2), m.group(3)
    m = re.search(r'\*BANDIND: (\d+), (\d+), (\d+), (\d+)', s.get("BANDIND", ""))
    if m:
        st["bandind_raw"] = ",".join(m.groups())
        st["band_lte"] = int(m.group(2))
    m = re.search(r'\+CGPADDR: 1, "([^"]+)"', s.get("CGPADDR", ""))
    if m:
        st["wan_ip"] = m.group(1)
    m = re.search(r'default via (\S+) dev (\S+)', s.get("ROUTE", ""))
    if m:
        st["gw"], st["wan_dev"] = m.group(1), m.group(2)

    # Serving cell (zcainfo = primary/secondary carrier)
    st["pci"] = zca.get("p_pci")
    st["band_zca"] = zca.get("p_band")
    st["earfcn"] = zca.get("p_dlEuArfcn")
    st["bw_mhz"] = zca.get("p_dlBandwitdh")
    st["rsrp_dbm"] = cesq_rsrp_dbm(zca.get("p_rsrp"))
    st["rsrq_db"] = cesq_rsrq_db(zca.get("p_rsrq"))
    st["sinr_db"] = zca.get("p_sinr")
    st["rssi_dbm"] = cesq_rssi_dbm(zca.get("p_rssi"))
    st["ca_secondary"] = bool(zca.get("s_status"))
    if st["rsrp_dbm"] is None:
        st["rsrp_dbm"] = cesq_rsrp_dbm(cesq.get("rsrp"))
        st["rsrq_db"] = cesq_rsrq_db(cesq.get("rsrq"))

    st["nw_mode"] = mode.get("nw_mode")
    st["prefer_mode"] = mode.get("prefer_mode")
    st["nr_mode"] = mode.get("nr_mode")
    st["sim_slot"] = sim.get("current_card")
    try:
        st["temp_c"] = int(s.get("TEMP", "0")) / 1000
    except ValueError:
        pass
    st["load1"] = (s.get("LOAD", "").split() or [None])[0]
    st["ttl_rules"] = int(s.get("TTL", "0") or 0)
    try:
        st["modem_uptime_s"] = round(float(s.get("UPTIME", "").split()[0]))
    except (ValueError, IndexError):
        pass
    if s.get("AT_RETRY"):
        st["at_retry"] = 1
    return st


def print_status(st):
    keys = ["ts", "tag", "operator", "rat", "eps_reg", "nr_reg", "band_lte", "pci", "earfcn", "bw_mhz",
            "rsrp_dbm", "rsrq_db", "sinr_db", "rssi_dbm", "ca_secondary", "enb_id", "cell_sector",
            "tac_hex", "wan_ip", "wan_dev", "nw_mode", "prefer_mode", "nr_mode", "sim_slot",
            "temp_c", "load1", "ttl_rules"]
    w = max(len(k) for k in keys)
    for k in keys:
        if k in st and st[k] not in (None, ""):
            print(f"{k.ljust(w)}  {st[k]}")

# ------------------------------------------------------------------ latency

def ping_stats(host="8.8.8.8", count=5):
    """Ping desde este PC (atraviesa el módem). Devuelve (min, avg, max, loss%)."""
    try:
        p = subprocess.run(["ping", "-c", str(count), "-W", "2", host], capture_output=True, text=True, timeout=30)
    except Exception:
        return None
    loss = re.search(r"(\d+)% packet loss", p.stdout)
    rtt = re.search(r"= ([\d.]+)/([\d.]+)/([\d.]+)", p.stdout)
    return {"ping_host": host,
            "loss_pct": int(loss.group(1)) if loss else None,
            "rtt_min": float(rtt.group(1)) if rtt else None,
            "rtt_avg": float(rtt.group(2)) if rtt else None,
            "rtt_max": float(rtt.group(3)) if rtt else None}

# ------------------------------------------------------------------ speed test (HTTP, Cloudflare)

DOWN_URL = "https://speed.cloudflare.com/__down?bytes={n}"
UP_URL = "https://speed.cloudflare.com/__up"


def _download_worker(nbytes, deadline, counter, lock):
    try:
        while time.time() < deadline:
            req = urllib.request.Request(DOWN_URL.format(n=nbytes), headers={"User-Agent": "notion5g"})
            with urllib.request.urlopen(req, timeout=20) as r:
                while time.time() < deadline:
                    chunk = r.read(65536)
                    if not chunk:
                        break
                    with lock:
                        counter[0] += len(chunk)
    except Exception as e:  # noqa
        with lock:
            counter[1] += 1


def _upload_worker(deadline, counter, lock):
    payload = b"\0" * (1024 * 1024)
    try:
        while time.time() < deadline:
            req = urllib.request.Request(UP_URL, data=payload, method="POST",
                                         headers={"User-Agent": "notion5g", "Content-Type": "application/octet-stream"})
            with urllib.request.urlopen(req, timeout=30) as r:
                r.read()
            with lock:
                counter[0] += len(payload)
    except Exception:
        with lock:
            counter[1] += 1


def modem_counters():
    """Bytes RX/TX de la interfaz celular ccinet0. Sirve para comprobar que el tráfico de la
    prueba pasó por el módem y no por el Wi-Fi del portátil (ambas rutas pueden estar activas)."""
    out = rsh("cat /sys/class/net/ccinet0/statistics/rx_bytes /sys/class/net/ccinet0/statistics/tx_bytes", timeout=15)
    try:
        rx, tx = [int(x) for x in out.split()[:2]]
        return rx, tx
    except ValueError:
        return None, None


# ------------------------------------------------------------------ ruta del PC: ¿el tráfico sale por el módem?

WIN_PS = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"


def _win_ps(cmd, timeout=25):
    """PowerShell de Windows desde WSL (modo mirrored). '' si no estamos en WSL o falla."""
    if not os.path.exists(WIN_PS):
        return ""
    try:
        p = subprocess.run([WIN_PS, "-NoProfile", "-Command", cmd], capture_output=True, text=True,
                           timeout=timeout, cwd="/mnt/c/")
        return p.stdout.replace("\r", "")
    except Exception:
        return ""


def modem_side_ip():
    """IP del PC en la LAN del módem (misma /24 que NOTION_HOST), o None."""
    host = os.environ.get("NOTION_HOST", "192.168.1.1")
    prefix = ".".join(host.split(".")[:3]) + "."
    try:
        out = subprocess.run(["ip", "-4", "-o", "addr", "show"], capture_output=True, text=True, timeout=5).stdout
    except Exception:
        return None
    for line in out.splitlines():
        m = re.search(r"inet (\d+\.\d+\.\d+\.\d+)/", line)
        if m and m.group(1).startswith(prefix) and m.group(1) != host:
            return m.group(1)
    return None


def pc_route_info():
    """Por dónde saldría el tráfico del PC: interfaz/IP origen que Windows elige para Internet y túneles VPN activos.
    Un túnel 'full' con kill-switch (WireGuard: 'Block untunneled traffic'; OpenVPN: bloqueo WFP) descarta TODO lo que
    no va por él, incluso sockets atados a la IP del lado del módem. Hay que desactivarlo durante las mediciones."""
    info = {"pc_modem_ip": modem_side_ip()}
    out = _win_ps("$r = Find-NetRoute -RemoteIPAddress 8.8.8.8; "
                  "Write-Output ('SRC=' + $r[0].IPAddress); Write-Output ('IF=' + $r[1].InterfaceAlias)")
    for line in out.splitlines():
        if line.startswith("SRC="):
            info["pc_src_ip"] = line[4:].strip()
        elif line.startswith("IF="):
            info["pc_default_if"] = line[3:].strip()
    out = _win_ps("Get-Process | Where-Object { $_.ProcessName -match 'wireguard|openvpn|ovpn|forticlient|"
                  "globalprotect|vpnagent|zerotier|tailscale' } | Select-Object -ExpandProperty ProcessName -Unique")
    procs = sorted({l.strip() for l in out.splitlines() if l.strip()})
    if procs:
        info["pc_vpn_procs"] = ";".join(procs)
    if info.get("pc_src_ip") and info.get("pc_modem_ip"):
        info["pc_route_via_modem"] = info["pc_src_ip"] == info["pc_modem_ip"]
    return info


def probe_path(nbytes=2_000_000):
    """Descarga corta y comprueba con los contadores de ccinet0 que de verdad salió por el módem."""
    res = {}
    c0 = modem_counters()
    got, err = 0, None
    try:
        req = urllib.request.Request(DOWN_URL.format(n=nbytes), headers={"User-Agent": "notion5g"})
        with urllib.request.urlopen(req, timeout=15) as r:
            while True:
                chunk = r.read(65536)
                if not chunk:
                    break
                got += len(chunk)
    except Exception as e:
        err = str(e)
    c1 = modem_counters()
    res["probe_mb"] = round(got / 1e6, 2)
    if err:
        res["probe_error"] = err
    if c0[0] is not None and c1[0] is not None:
        res["probe_modem_rx_mb"] = round((c1[0] - c0[0]) / 1e6, 2)
        res["probe_via_modem"] = got > 0 and res["probe_modem_rx_mb"] >= 0.8 * res["probe_mb"]
    return res


def explain_path(route, probe):
    """Texto para el usuario cuando la prueba no sale por el módem."""
    lines = ["El tráfico del PC NO está saliendo por el módem:"]
    if probe.get("probe_error"):
        lines.append(f"  - la descarga de prueba falló: {probe['probe_error']}")
    else:
        lines.append(f"  - descargados {probe.get('probe_mb')} MB, el módem vio {probe.get('probe_modem_rx_mb')} MB")
    if route.get("pc_default_if"):
        lines.append(f"  - Windows envía Internet por '{route['pc_default_if']}' (IP origen {route.get('pc_src_ip')}); "
                     f"la IP del lado del módem es {route.get('pc_modem_ip')}")
    if route.get("pc_vpn_procs"):
        lines.append(f"  - hay clientes VPN corriendo ({route['pc_vpn_procs']}). Un túnel con kill-switch bloquea "
                     "todo lo que no va por él: desactívalo antes de medir")
    lines.append("  Solución: desactivar el túnel VPN y/o apagar el Wi-Fi del PC; repetir. "
                 "Con --force se registra igual (via_modem=False).")
    return "\n".join(lines)


def http_speed(seconds=12, threads=6):
    lock = threading.Lock()
    res = {}
    c0 = modem_counters()
    for name, target in (("down", _download_worker), ("up", _upload_worker)):
        counter = [0, 0]
        deadline = time.time() + seconds
        t0 = time.time()
        ws = []
        for _ in range(threads):
            args = (50_000_000, deadline, counter, lock) if name == "down" else (deadline, counter, lock)
            th = threading.Thread(target=target, args=args, daemon=True)
            th.start()
            ws.append(th)
        for th in ws:
            th.join(timeout=seconds + 40)
        elapsed = max(time.time() - t0, 0.001)
        res[f"{name}_mbps"] = round(counter[0] * 8 / elapsed / 1e6, 2)
        res[f"{name}_errors"] = counter[1]
        res[f"{name}_mb"] = round(counter[0] / 1e6, 1)
    c1 = modem_counters()
    if c0[0] is not None and c1[0] is not None:
        res["modem_rx_mb"] = round((c1[0] - c0[0]) / 1e6, 1)
        res["modem_tx_mb"] = round((c1[1] - c0[1]) / 1e6, 1)
        # si el módem vio menos del 80% de lo descargado, el tráfico se fue por otra ruta (Wi-Fi del PC)
        res["via_modem"] = res["modem_rx_mb"] >= 0.8 * res["down_mb"] if res["down_mb"] else None
    return res


def find_speedtest():
    """CLI oficial de Ookla: NOTION_SPEEDTEST, PATH, o el zip portable descomprimido en Windows."""
    cand = [os.environ.get("NOTION_SPEEDTEST"), shutil.which("speedtest")]
    for base in ("/mnt/c/Users/*/speedtest-cli/speedtest.exe",
                 "/mnt/c/Users/*/AppData/Local/Microsoft/WinGet/Links/speedtest.exe",
                 "/mnt/c/Program Files/Speedtest/speedtest.exe"):
        import glob
        cand += sorted(glob.glob(base))
    for c in cand:
        if c and os.path.exists(c):
            return c
    return None


def ookla_speed(server_id=None):
    """Si está el CLI oficial de Ookla (`speedtest`), úsalo: es lo comparable con la app del celular.
    Devuelve None si no hay CLI; {'ookla_error': ...} si falló (p. ej. sin salida a Internet por el módem)."""
    exe = find_speedtest()
    if not exe:
        return None
    cmd = [exe, "--accept-license", "--accept-gdpr", "-f", "json"]
    if server_id:
        cmd += ["-s", str(server_id)]
    # el .exe de Windows no acepta cwd UNC (\\wsl.localhost): ejecutarlo desde C:\
    cwd = "/mnt/c/" if exe.lower().endswith(".exe") and os.path.isdir("/mnt/c/") else None
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=180, cwd=cwd)
    except Exception as e:
        return {"ookla_error": str(e)}
    result = None
    for line in (p.stdout or "").replace("\r", "").splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            j = json.loads(line)
        except json.JSONDecodeError:
            continue
        if j.get("type") == "result" or "download" in j:
            result = j
    if not result:
        errs = [l for l in (p.stderr or "").replace("\r", "").splitlines() if "[error]" in l or "rror" in l]
        msg = errs[-1].split("] ", 2)[-1] if errs else f"sin resultado (rc={p.returncode})"
        return {"ookla_error": msg[:200], "ookla_exe": exe}
    try:
        return {"ookla_server": f'{result["server"]["name"]} ({result["server"]["id"]})',
                "ookla_down_mbps": round(result["download"]["bandwidth"] * 8 / 1e6, 2),
                "ookla_up_mbps": round(result["upload"]["bandwidth"] * 8 / 1e6, 2),
                "ookla_ping_ms": result["ping"]["latency"],
                "ookla_jitter_ms": result["ping"]["jitter"],
                "ookla_url": result.get("result", {}).get("url"),
                "ookla_exe": exe}
    except (KeyError, TypeError) as e:
        return {"ookla_error": f"JSON inesperado: {e}", "ookla_exe": exe}

# ------------------------------------------------------------------ GPS externo (el módem NO tiene GNSS)

def gps_windows():
    """Lee la posición del Windows Location API vía PowerShell (WSL). Requiere Ubicación activada en Windows."""
    ps = shutil.which("powershell.exe") or "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
    script = os.path.join(HERE, "win_location.ps1")
    try:
        p = subprocess.run([ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File",
                            subprocess.run(["wslpath", "-w", script], capture_output=True, text=True).stdout.strip()],
                           capture_output=True, text=True, timeout=25)
        j = json.loads(p.stdout.strip().splitlines()[-1])
        return {"lat": j.get("lat"), "lon": j.get("lon"), "gps_acc_m": j.get("acc"), "gps_src": "windows"}
    except Exception:
        return {"lat": None, "lon": None, "gps_acc_m": None, "gps_src": "windows-fail"}


def gps_nmea_udp(port, wait=3.0):
    """Escucha NMEA por UDP (apps de celular tipo 'GPS2IP', 'Share GPS', 'NetGPS'). Devuelve la última $..RMC/GGA."""
    import socket
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(wait)
    s.bind(("0.0.0.0", port))
    lat = lon = None
    t_end = time.time() + wait
    try:
        while time.time() < t_end:
            data, _ = s.recvfrom(4096)
            for line in data.decode(errors="ignore").splitlines():
                m = re.match(r"\$G.(GGA|RMC),", line)
                if not m:
                    continue
                f = line.split(",")
                try:
                    if m.group(1) == "GGA" and f[2] and f[4]:
                        lat, lon = _nmea_deg(f[2], f[3]), _nmea_deg(f[4], f[5])
                    elif m.group(1) == "RMC" and f[3] and f[5]:
                        lat, lon = _nmea_deg(f[3], f[4]), _nmea_deg(f[5], f[6])
                except (ValueError, IndexError):
                    pass
    except OSError:
        pass
    finally:
        s.close()
    return {"lat": lat, "lon": lon, "gps_acc_m": None, "gps_src": f"nmea-udp:{port}"}


def _nmea_deg(v, hemi):
    d = float(v)
    deg = int(d / 100)
    mins = d - deg * 100
    val = deg + mins / 60
    return round(-val if hemi in ("S", "W") else val, 6)


def get_gps(mode):
    if mode == "win":
        return gps_windows()
    if mode.startswith("nmea:"):
        return gps_nmea_udp(int(mode.split(":")[1]))
    return {}

# ------------------------------------------------------------------ persistence

def report_row(row, endpoint="measurements"):
    """Envía `row` al backend HTTP (server/) si NOTION_REPORT_URL está definida.
    No propaga errores: el reporte es best-effort, nunca debe tumbar una medición local."""
    url = os.environ.get("NOTION_REPORT_URL")
    if not url:
        return
    payload = dict(row)
    payload.setdefault("device_id", os.environ.get("NOTION_DEVICE_ID") or socket.gethostname())
    payload.setdefault("source", "pc")
    # nombres de campo cortos usados en el CSV local -> nombres que espera el backend
    if "gps_acc_m" in payload:
        payload["gps_accuracy_m"] = payload.pop("gps_acc_m")
    if "gps_src" in payload:
        payload["gps_source"] = payload.pop("gps_src")
    if "modem_uptime_s" in payload:
        payload["uptime_s"] = payload.pop("modem_uptime_s")
    data = json.dumps(payload, ensure_ascii=False, default=str).encode("utf-8")
    req = urllib.request.Request(url.rstrip("/") + f"/api/v1/{endpoint}", data=data, method="POST",
                                 headers={"Content-Type": "application/json", "User-Agent": "notion5g"})
    key = os.environ.get("NOTION_API_KEY")
    if key:
        req.add_header("X-API-Key", key)
    try:
        with urllib.request.urlopen(req, timeout=8) as r:
            r.read()
    except Exception as e:
        print(f"  (aviso: no se pudo reportar al servidor: {e})", file=sys.stderr)


def append_row(name, row):
    os.makedirs(RESULTS, exist_ok=True)
    with open(os.path.join(RESULTS, f"{name}.jsonl"), "a") as f:
        f.write(json.dumps(row, ensure_ascii=False) + "\n")
    csv_path = os.path.join(RESULTS, f"{name}.csv")
    new = not os.path.exists(csv_path)
    # columnas estables: unión de lo visto en el archivo + fila actual
    cols = list(row.keys())
    if not new:
        with open(csv_path) as f:
            head = f.readline().strip().split(",")
        cols = head + [c for c in cols if c not in head]
        if cols != head:  # reescribir con cabecera ampliada
            rows = list(csv.DictReader(open(csv_path)))
            with open(csv_path, "w", newline="") as f:
                w = csv.DictWriter(f, fieldnames=cols)
                w.writeheader()
                w.writerows(rows)
    with open(csv_path, "a", newline="") as f:
        w = csv.DictWriter(f, fieldnames=cols)
        if new:
            w.writeheader()
        w.writerow(row)

# ------------------------------------------------------------------ CLI

def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("status", help="snapshot de radio/red")
    p.add_argument("--json", action="store_true")
    p.add_argument("--save", action="store_true", help="anexa a results/status.{csv,jsonl}")

    p = sub.add_parser("speed", help="prueba de velocidad desde este PC + snapshot")
    p.add_argument("--seconds", type=int, default=12)
    p.add_argument("--threads", type=int, default=6)
    p.add_argument("--ookla-server", type=int, default=None, help="ID de servidor Ookla (igualar con el del celular)")
    p.add_argument("--runs", type=int, default=1)
    p.add_argument("--note", default="")
    p.add_argument("--force", action="store_true",
                   help="medir aunque la comprobación previa diga que el tráfico no sale por el módem")
    p.add_argument("--no-ookla", action="store_true", help="no ejecutar el CLI de Ookla")

    p = sub.add_parser("drive", help="bucle de campo con GPS externo")
    p.add_argument("--interval", type=int, default=10)
    p.add_argument("--gps", default="none", help="none | win | nmea:<puerto_udp>")
    p.add_argument("--ping", default="8.8.8.8")
    p.add_argument("--speed-every", type=int, default=0, help="cada N muestras corre un speed corto (0=nunca)")

    p = sub.add_parser("at", help="comando AT crudo")
    p.add_argument("command")

    p = sub.add_parser("ubus", help="llamada ubus")
    p.add_argument("object")
    p.add_argument("method")
    p.add_argument("args", nargs="?", default=None, help="JSON opcional")

    p = sub.add_parser("heartbeat", help="ping periódico liviano: uptime + señal, sin prueba de velocidad")
    p.add_argument("--interval", type=int, default=60, help="segundos entre pings (default 60)")
    p.add_argument("--once", action="store_true", help="una sola vez y termina (para cron)")

    a = ap.parse_args()

    if a.cmd == "status":
        st = get_status()
        if a.save:
            append_row("status", st)
            report_row(st)
        print(json.dumps(st, indent=2, ensure_ascii=False) if a.json else "", end="")
        if not a.json:
            print_status(st)

    elif a.cmd == "speed":
        route = pc_route_info()
        probe = probe_path()
        if not probe.get("probe_via_modem"):
            print(explain_path(route, probe), file=sys.stderr)
            if not a.force:
                sys.exit(2)
        else:
            print(f"ruta OK: el módem vio {probe['probe_modem_rx_mb']} MB de {probe['probe_mb']} MB de prueba"
                  + (f" (Windows sale por '{route['pc_default_if']}')" if route.get("pc_default_if") else ""))
        for i in range(a.runs):
            st = get_status()
            row = dict(st)
            row["note"] = a.note
            row.update({k: v for k, v in route.items() if k in ("pc_default_if", "pc_src_ip", "pc_vpn_procs")})
            row.update(ping_stats() or {})
            row.update(http_speed(a.seconds, a.threads))
            ook = None if a.no_ookla else ookla_speed(a.ookla_server)
            if ook:
                row.update(ook)
            append_row("speed", row)
            report_row(row)
            print(f"[{i+1}/{a.runs}] {row.get('operator')} {row.get('rat')} B{row.get('band_lte')} PCI {row.get('pci')} "
                  f"RSRP {row.get('rsrp_dbm')} SINR {row.get('sinr_db')} | "
                  f"HTTP ↓{row['down_mbps']} ↑{row['up_mbps']} Mbps | ping {row.get('rtt_avg')} ms"
                  + (" | via módem OK" if row.get("via_modem") else " | ¡OJO: tráfico NO pasó por el módem (VPN/Wi-Fi del PC)!")
                  + (f" | Ookla ↓{ook.get('ookla_down_mbps')} ↑{ook.get('ookla_up_mbps')}" if ook and "ookla_down_mbps" in ook else "")
                  + (f" | Ookla ERROR: {ook.get('ookla_error')}" if ook and "ookla_error" in ook else ""))
            if i + 1 < a.runs:
                time.sleep(5)

    elif a.cmd == "drive":
        n = 0
        print(f"drive test cada {a.interval}s, GPS={a.gps}. Ctrl+C para terminar. -> results/drive.csv")
        probe = probe_path(500_000)
        if not probe.get("probe_via_modem"):
            print(explain_path(pc_route_info(), probe) + "\n(el drive sigue, pero ping/velocidad no serán del módem)",
                  file=sys.stderr)
        try:
            while True:
                n += 1
                row = {}
                try:
                    row.update(get_status())
                except Exception as e:
                    row.update({"ts": dt.datetime.now().isoformat(timespec="seconds"), "error": str(e)})
                row.update(get_gps(a.gps))
                row.update(ping_stats(a.ping, 3) or {})
                if a.speed_every and n % a.speed_every == 0:
                    row.update(http_speed(6, 4))
                append_row("drive", row)
                report_row(row)
                print(f"{row.get('ts')} {row.get('operator','?')} {row.get('rat','?')} B{row.get('band_lte','?')} "
                      f"PCI {row.get('pci','?')} RSRP {row.get('rsrp_dbm','?')} SINR {row.get('sinr_db','?')} "
                      f"rtt {row.get('rtt_avg','?')} lat/lon {row.get('lat')},{row.get('lon')}"
                      + (f" ↓{row.get('down_mbps')}" if 'down_mbps' in row else ""))
                time.sleep(a.interval)
        except KeyboardInterrupt:
            print("\nfin.")

    elif a.cmd == "at":
        print("\n".join(at(a.command)))

    elif a.cmd == "ubus":
        args = json.loads(a.args) if a.args else None
        print(json.dumps(ubus(a.object, a.method, args), indent=2, ensure_ascii=False))

    elif a.cmd == "heartbeat":
        def _beat():
            try:
                st = get_status()
                hb = {"ts": st.get("ts"), "uptime_s": st.get("modem_uptime_s"),
                      "operator": st.get("operator"), "rat": st.get("rat"), "rsrp_dbm": st.get("rsrp_dbm")}
            except Exception as e:
                hb = {"ts": dt.datetime.now().isoformat(timespec="seconds"), "error": str(e)}
            hb.setdefault("device_id", os.environ.get("NOTION_DEVICE_ID") or socket.gethostname())
            report_row(hb, endpoint="heartbeat")
            print(hb)
        _beat()
        if not a.once:
            try:
                while True:
                    time.sleep(a.interval)
                    _beat()
            except KeyboardInterrupt:
                print("\nfin.")


if __name__ == "__main__":
    main()
