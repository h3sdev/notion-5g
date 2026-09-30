#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Configura el MikroTik hAP ac2 (RouterOS 7.6) como sonda A/B del celular por cable.

Implementa la sección 3 de server/docs/CONTRATO-SONDA-AB.md ("forma A" de
PLAN-SONDA-AB.md). Solo biblioteca estándar de Python (corre en el Python de Windows,
porque WSL no alcanza direcciones IPv6 link-local).

Se copia a C:\\Users\\diego\\mikrotik-probe\\ y se corre así:

    powershell.exe -NoProfile -Command "cd C:\\Users\\diego\\mikrotik-probe; python apply_probe.py --password admin"

Modos (uno a la vez):
    (ninguno) / --dry-run   lee el estado y muestra el plan como líneas de consola RouterOS; no toca nada
    --apply                 respaldo obligatorio (export + backup + FTP + JSON) y luego aplica el plan
    --verify                compara el estado con lo esperado y hace pruebas (ping a cada gateway, IP de
                            salida por cada tabla con una regla temporal que se borra al terminar)
    --emit-rsc              imprime el script .rsc equivalente (scripts/mikrotik/probe-ab.rsc); no se conecta
    --backup-only           solo el respaldo (export + backup + FTP + JSON)
    --switch A|B|fallback   mueve la regla phone-probe a to-A / to-B / main (pruebas a mano)

Conexión: --host fe80::de2c:6eff:fef7:7fe9%82 por defecto (el %82 es el ifIndex de Windows del
adaptador conectado a ether5; lo muestra Get-NetAdapter). También acepta IPv4 (192.168.88.1).

Volver atrás (a mano, reinicia el equipo):
    /system backup load name=flash/pre-probe-<AAAAMMDD-HHMMSS>.backup
El .backup queda en el router (en flash/, que sobrevive a reinicios; la raíz de /file es RAM) y en
C:\\Users\\diego\\mikrotik-probe\\backups\\ junto al .rsc exportado y un volcado JSON de los menús que
se tocan. Si el .backup ya no está en el router, súbelo desde esa carpeta (WinBox > Files).
"""

import argparse
import datetime
import ftplib
import ipaddress
import json
import os
import re
import secrets
import socket
import string
import subprocess
import sys
import time

# ---------------------------------------------------------------------------------------------
# Valores fijos de la instalación (contrato §"Valores fijos" y §3.2)
# ---------------------------------------------------------------------------------------------

DEFAULT_HOST = "fe80::de2c:6eff:fef7:7fe9%82"
API_PORT = 8728
BRIDGE = "bridge"

PHONE_IFACE = "ether3"
PHONE_NET = "192.168.89.0/24"
PHONE_ADDR = "192.168.89.1/24"
PHONE_GW = "192.168.89.1"
PHONE_POOL = "probe-phone"
PHONE_POOL_RANGES = "192.168.89.10-192.168.89.50"

MGMT_ADDR = "192.168.88.1/24"
MGMT_NET = "192.168.88.0/24"
MGMT_POOL_RANGES = "192.168.88.10-192.168.88.254"
OLD_MGMT_ADDR = "192.168.1.1/24"
OLD_MGMT_NET = "192.168.1.0/24"
# Subredes que no pueden usarse ni para el celular ni para administración.
FORBIDDEN_NETS = ("192.168.1.0/24", "192.168.2.0/24", "192.168.40.0/24")

DNS_SERVERS = "1.1.1.1,8.8.8.8"
NTP_SERVERS = "162.159.200.1,162.159.200.123"  # time.cloudflare.com por IP
TIME_ZONE = "America/Bogota"
IDENTITY = "hap-sonda"

RULE_COMMENT = "phone-probe"
RULE_LOCAL_COMMENT = "probe:phone-local"
TMP_RULE_COMMENT = "probe:tmp-verify"
WATCHDOG_NAME = "probe-rule-watchdog"
# Con solo read,write, RouterOS 7.6 no conserva las variables :global entre corridas del
# scheduler (el contador del vigilante nunca pasaba de 1 y la regla nunca volvía a main);
# con policy+test sí. Probado en el equipo el 2026-09-29.
WATCHDOG_POLICY = "read,write,policy,test"
API_GROUP = "probe-api"
API_USER = "phone-probe"
API_ADDRESS_DEFAULT = "192.168.88.0/24,192.168.89.0/24,fe80::/10"

# Slot A = ether2 (Notion 5G), slot B = ether1 (Notion 4G). Ver "Hallazgos" del contrato.
WAN = {
    "A": {"iface": "ether2", "table": "to-A", "default_gw": "192.168.1.1",
          "route": "probe:A-default", "main": "probe:main-A", "main_distance": "1",
          "comment": "probe:wan-A", "egress_ip": "1.0.0.1"},
    "B": {"iface": "ether1", "table": "to-B", "default_gw": "192.168.2.1",
          "route": "probe:B-default", "main": "probe:main-B", "main_distance": "2",
          "comment": "probe:wan-B", "egress_ip": "1.1.1.1"},
}

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
PASSWORD_FILE = os.path.join(BASE_DIR, "phone-probe-password.txt")
MIN_FREE_HDD = 300 * 1024
# En el hAP ac2 (16 MB de flash) solo lo que está bajo flash/ sobrevive a un reinicio: la raíz
# de /file es RAM. El respaldo va a flash/ para que "/system backup load" sirva tras un corte.
ROUTER_DIR = "flash/"

PASSWORD = object()  # marcador: la contraseña de phone-probe se resuelve al ejecutar


def wan_script(slot):
    """Script del DHCP client de cada WAN (contrato §3.2): corrige el gateway de sus dos rutas."""
    w = WAN[slot]
    return (':if ($bound=1) do={:local gw ($"gateway-address" . "%" . $interface); '
            ':foreach c in={"ROUTE";"MAIN"} do={:foreach r in=[/ip route find comment=$c] do={'
            ':if ([:tostr [/ip route get $r gateway]] != $gw) do={/ip route set $r gateway=$gw; '
            ':log info "probe: $c -> $gw"}}}}').replace("ROUTE", w["route"]).replace("MAIN", w["main"])


def watchdog_script(minutes, user=API_USER):
    """Vigilante de la regla (contrato §3.2): la devuelve a main tras N minutos fuera de main
    SIN actividad del celular.

    El contador vuelve a 0 si la regla está en main, si cambió de tabla desde la vuelta anterior
    o si hay un login nuevo de phone-probe en /log (el celular entra por la API al forzar, al
    releer y al restaurar, y cada mikrotik_idle_check_s en reposo). Así, pruebas seguidas por la
    misma tabla (A -> main -> A entre dos vueltas de 1 min) no se suman: solo cuenta el tiempo
    sin sesiones del celular, que es el caso "el celular murió con la regla forzada"."""
    return (':global probeForcedMin; :global probeLastTable; :global probeLastLogin; '
            ':local r [/routing rule find comment="phone-probe"]; '
            ':if ([:len $r]=1) do={:local t [/routing rule get [:pick $r 0] table]; '
            ':local l ""; :local ls [/log find where message~"^user USER logged in"]; '
            ':if ([:len $ls]>0) do={:set l [:tostr [:pick $ls ([:len $ls]-1)]]}; '
            ':if ([:typeof $probeForcedMin]!="num") do={:set probeForcedMin 0}; '
            ':if (($t="main") || ($t!=[:tostr $probeLastTable]) || ($l!=[:tostr $probeLastLogin])) '
            'do={:set probeForcedMin 0} else={:set probeForcedMin ($probeForcedMin+1); '
            ':if ($probeForcedMin>=%d) do={/routing rule set [:pick $r 0] table=main; '
            ':log warning "phone-probe: regla en $t por %d min sin actividad del celular, vuelta a main"; '
            ':set probeForcedMin 0; :set t "main"}}; '
            ':set probeLastTable $t; :set probeLastLogin $l}' % (minutes, minutes)).replace("USER", user)


# ---------------------------------------------------------------------------------------------
# API clásica de RouterOS (TCP 8728): palabras con prefijo de largo, frases terminadas en "".
# ---------------------------------------------------------------------------------------------

class ApiError(Exception):
    """!trap de RouterOS (el comando falló; la sesión sigue viva)."""


class ApiFatal(Exception):
    """!fatal de RouterOS (el router cierra la sesión)."""


class ProtocolError(Exception):
    pass


def encode_length(n):
    if n < 0:
        raise ValueError("largo negativo")
    if n < 0x80:
        return bytes([n])
    if n < 0x4000:
        return (n | 0x8000).to_bytes(2, "big")
    if n < 0x200000:
        return (n | 0xC00000).to_bytes(3, "big")
    if n < 0x10000000:
        return (n | 0xE0000000).to_bytes(4, "big")
    if n < 0x100000000:
        return b"\xF0" + n.to_bytes(4, "big")
    raise ValueError("palabra demasiado larga")


def decode_length(read):
    """`read(n)` devuelve exactamente n bytes."""
    c = read(1)[0]
    if c < 0x80:
        return c
    if c < 0xC0:
        return ((c & 0x3F) << 8) | read(1)[0]
    if c < 0xE0:
        return ((c & 0x1F) << 16) | int.from_bytes(read(2), "big")
    if c < 0xF0:
        return ((c & 0x0F) << 24) | int.from_bytes(read(3), "big")
    if c == 0xF0:
        return int.from_bytes(read(4), "big")
    raise ProtocolError("byte de control no soportado 0x%02X" % c)


def encode_word(word):
    b = word.encode("utf-8")
    return encode_length(len(b)) + b


def encode_sentence(words):
    return b"".join(encode_word(w) for w in words) + b"\x00"


def parse_attrs(words):
    """['=a=1', '=.id=*2', '=b='] -> {'a': '1', '.id': '*2', 'b': ''}"""
    out = {}
    for w in words:
        if w.startswith("="):
            k, _, v = w[1:].partition("=")
            out[k] = v
    return out


def resolve_host(host, port):
    """'fe80::1%82' / '[fe80::1%82]' / '192.168.88.1' / nombre -> (familia, sockaddr)."""
    h = host.strip()
    if h.startswith("[") and h.endswith("]"):
        h = h[1:-1]
    addr, _, scope = h.partition("%")
    try:
        ip = ipaddress.ip_address(addr)
    except ValueError:
        info = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)[0]
        return info[0], info[4]
    if ip.version == 6:
        scope_id = 0
        if scope:
            scope_id = int(scope) if scope.isdigit() else socket.if_nametoindex(scope)
        if ip.is_link_local and not scope_id:
            raise ValueError("una dirección link-local necesita %%ifIndex (p. ej. %s)" % DEFAULT_HOST)
        return socket.AF_INET6, (addr, port, 0, scope_id)
    return socket.AF_INET, (addr, port)


class Api:
    """Sesión de la API clásica. `talk()` devuelve las filas !re; el !done queda en `self.done`."""

    def __init__(self, sock):
        self.sock = sock
        self.done = {}

    @classmethod
    def connect(cls, host, port=API_PORT, timeout=30.0):
        family, sockaddr = resolve_host(host, port)
        s = socket.socket(family, socket.SOCK_STREAM)
        s.settimeout(timeout)
        try:
            s.connect(sockaddr)
        except Exception:
            s.close()
            raise
        return cls(s)

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass

    def _read(self, n):
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise EOFError("el MikroTik cerró la conexión")
            buf += chunk
        return buf

    def read_sentence(self):
        words = []
        while True:
            n = decode_length(self._read)
            if n == 0:
                return words
            words.append(self._read(n).decode("utf-8", errors="replace"))

    def talk(self, words):
        self.sock.sendall(encode_sentence(words))
        rows, trap = [], None
        while True:
            sentence = self.read_sentence()
            if not sentence:
                continue
            kind, attrs = sentence[0], parse_attrs(sentence[1:])
            if kind == "!re":
                rows.append(attrs)
            elif kind == "!trap":
                if trap is None:
                    trap = attrs.get("message", "error sin mensaje")
            elif kind == "!fatal":
                raise ApiFatal(" ".join(sentence[1:]) or "fatal")
            elif kind == "!empty":
                pass  # RouterOS nuevos: print sin resultados; sigue un !done
            elif kind == "!done":
                self.done = attrs
                if trap is not None:
                    raise ApiError(trap)
                return rows
            else:
                raise ProtocolError("respuesta inesperada %r" % sentence)

    def login(self, user, password):
        self.talk(["/login", "=name=" + user, "=password=" + password])
        if "ret" in self.done:
            raise ApiError("el router pide el login viejo (anterior a 6.43): no soportado")


class RouterOS:
    """Operaciones de menú sobre una sesión (real o falsa, en las pruebas)."""

    def __init__(self, api):
        self.api = api

    def run(self, path, params=None, queries=None):
        words = [path]
        words += ["=%s=%s" % (k, v) for k, v in (params or {}).items()]
        words += ["?%s=%s" % (k, v) for k, v in (queries or {}).items()]
        return self.api.talk(words)

    def print(self, menu, queries=None, proplist=None):
        params = {".proplist": ",".join(proplist)} if proplist else None
        return self.run(menu + "/print", params, queries)

    def add(self, menu, props):
        self.run(menu + "/add", props)
        return self.api.done.get("ret")

    def set(self, menu, id_, props):
        params = dict(props)
        if id_ is not None:
            params = dict([(".id", id_)] + list(params.items()))
        self.run(menu + "/set", params)

    def remove(self, menu, id_):
        self.run(menu + "/remove", {"numbers": id_})

    def move(self, menu, id_, destination):
        self.run(menu + "/move", {"numbers": id_, "destination": destination})


# ---------------------------------------------------------------------------------------------
# Comparación de valores
# ---------------------------------------------------------------------------------------------

FLAG_PROPS = {"disabled", "dynamic", "inactive", "invalid"}
LIST_PROPS = {"servers", "dns-server", "address", "ranges"}
POLICY_PROPS = {"policy"}
SCRIPT_PROPS = {"script", "on-event"}


def norm(v):
    s = "" if v is None else str(v)
    low = s.lower()
    if low in ("true", "yes"):
        return "yes"
    if low in ("false", "no"):
        return "no"
    return s


def split_list(v):
    return {x.strip() for x in str(v or "").split(",") if x.strip()}


def positive_policy(v):
    return {x for x in split_list(v) if not x.startswith("!")}


def same(key, have, want):
    if have is None:
        have = "no" if key in FLAG_PROPS else ""
    if key in POLICY_PROPS:
        return positive_policy(have) == positive_policy(want)
    if key in LIST_PROPS:
        return split_list(have) == split_list(want)
    if key in SCRIPT_PROPS:
        return " ".join(str(have).split()) == " ".join(str(want).split())
    return norm(have) == norm(want)


def is_true(v):
    return norm(v) == "yes"


def mac_to_link_local(mac):
    b = bytearray(int(x, 16) for x in mac.split(":"))
    b[0] ^= 0x02
    eui = bytes(b[:3]) + b"\xff\xfe" + bytes(b[3:])
    return str(ipaddress.IPv6Address(b"\xfe\x80" + b"\x00" * 6 + eui))


# ---------------------------------------------------------------------------------------------
# Plan: acciones calculadas a partir del estado leído
# ---------------------------------------------------------------------------------------------

class Action:
    def __init__(self, op, menu, id_=None, props=None, label="", before=None, note=""):
        self.op = op          # add | set | remove | move | wait
        self.menu = menu
        self.id = id_
        self.props = props or {}
        self.label = label
        self.before = before or {}
        self.note = note

    def console(self, verbose=False):
        path = "/" + " ".join(p for p in self.menu.split("/") if p)
        if self.op == "wait":
            return "(esperar) " + self.note
        parts = [path, self.op]
        if self.id is not None:
            parts.append(str(self.id))
        for k, v in self.props.items():
            parts.append("%s=%s" % (k, show_value(k, v, verbose)))
        line = " ".join(parts)
        extra = []
        if self.before:
            extra.append("antes: " + " ".join("%s=%s" % (k, show_value(k, v, verbose))
                                              for k, v in self.before.items()))
        if self.note:
            extra.append(self.note)
        if extra:
            line += "    # " + "; ".join(extra)
        return line


def show_value(key, v, verbose=False):
    if v is PASSWORD or key == "password":
        return "<oculta>"
    s = "" if v is None else str(v)
    if not verbose and len(s) > 70:
        s = s[:67] + "..."
    if s == "" or re.search(r'[\s";${}\[\]]', s):
        return '"%s"' % s.replace('"', '\\"')
    return s


def rows(st, menu, **eq):
    return [r for r in st.get(menu, []) if all(r.get(k.replace("_", "-")) == v for k, v in eq.items())]


def static(rs):
    return [r for r in rs if not is_true(r.get("dynamic"))]


def first(*candidates):
    for c in candidates:
        if c:
            return c[0]
    return None


def ensure(menu, row, want, label, create_extra=None, singleton=False):
    """Una acción add (si falta) o set con solo lo que difiere (si existe)."""
    if row is None:
        if singleton:
            return []
        props = dict(want)
        props.update(create_extra or {})
        return [Action("add", menu, None, props, label)]
    diffs = {k: v for k, v in want.items() if v is not None and not same(k, row.get(k), v)}
    if not diffs:
        return []
    before = {k: row.get(k, "") for k in diffs}
    return [Action("set", menu, None if singleton else row.get(".id"), diffs, label, before)]


class Ctx:
    def __init__(self, args, mode):
        self.args = args
        self.mode = mode            # dry | apply | verify
        self.warnings = []
        self.phone_password = None
        self.password_generated = False
        self.password_saved = False

    def warn(self, msg):
        if msg not in self.warnings:
            self.warnings.append(msg)


# --- fases, en el orden del contrato §3.4 -----------------------------------------------------

def phase_bridge_mac(st, ctx):
    b = first(rows(st, "/interface/bridge", name=ctx.args.bridge))
    if b is None:
        ctx.warn("No existe el bridge '%s'." % ctx.args.bridge)
        return []
    mac = b.get("admin-mac") if norm(b.get("auto-mac")) == "no" else None
    host = ctx.args.host.split("%")[0].strip("[]").lower()
    current = b.get("mac-address", "")
    if current and host.startswith("fe80:"):
        try:
            if ipaddress.ip_address(host) != ipaddress.ip_address(mac_to_link_local(current)):
                ctx.warn("La dirección de --host (%s) no es la link-local del bridge (%s): se está "
                         "entrando por otra interfaz." % (host, mac_to_link_local(current)))
        except ValueError:
            pass
    if mac:
        return []
    return [Action("set", "/interface/bridge", b.get(".id"),
                   {"auto-mac": "no", "admin-mac": current}, "MAC fija del bridge",
                   {"auto-mac": b.get("auto-mac", "")},
                   "sin esto, al reiniciar el bridge puede tomar otra MAC y cambiar su link-local")]


def phase_tables(st, ctx):
    acts = []
    for slot in ("A", "B"):
        t = WAN[slot]["table"]
        r = first(rows(st, "/routing/table", name=t))
        if r is None:
            acts.append(Action("add", "/routing/table", None,
                               {"name": t, "fib": "", "comment": "probe:" + t}, "tabla " + t))
        elif "fib" not in r or norm(r.get("fib")) == "no":
            acts.append(Action("set", "/routing/table", r.get(".id"), {"fib": ""}, "tabla " + t))
    return acts


def phase_lists(st, ctx):
    acts = []
    for lst, iface, comment in (("WAN", WAN["B"]["iface"], "probe:wan-B"),
                                ("WAN", WAN["A"]["iface"], "probe:wan-A"),
                                ("LAN", PHONE_IFACE, "probe:phone")):
        if not rows(st, "/interface/list/member", list=lst, interface=iface):
            acts.append(Action("add", "/interface/list/member", None,
                               {"list": lst, "interface": iface, "comment": comment}, "lista " + lst))
        other = "LAN" if lst == "WAN" else "WAN"
        if rows(st, "/interface/list/member", list=other, interface=iface):
            ctx.warn("%s también está en la lista %s: revísalo a mano." % (iface, other))
    return acts


def phase_bridge_ports(st, ctx):
    acts = []
    for iface in (WAN["A"]["iface"], PHONE_IFACE):
        for r in rows(st, "/interface/bridge/port", interface=iface):
            acts.append(Action("remove", "/interface/bridge/port", r.get(".id"), {},
                               "sacar %s del bridge" % iface))
    return acts


def phase_addresses(st, ctx):
    cur = static(st.get("/ip/address", []))
    br = ctx.args.bridge
    mg = first([r for r in cur if r.get("comment") == "probe:mgmt"],
               [r for r in cur if r.get("interface") == br and r.get("address") == MGMT_ADDR],
               [r for r in cur if r.get("interface") == br and r.get("address") == OLD_MGMT_ADDR])
    acts = ensure("/ip/address", mg, {"address": MGMT_ADDR, "network": MGMT_NET.split("/")[0],
                                      "interface": br, "comment": "probe:mgmt"}, "administración")
    ph = first([r for r in cur if r.get("comment") == "probe:phone-net"],
               [r for r in cur if r.get("interface") == PHONE_IFACE and r.get("address") == PHONE_ADDR])
    acts += ensure("/ip/address", ph, {"address": PHONE_ADDR, "network": PHONE_NET.split("/")[0],
                                       "interface": PHONE_IFACE, "comment": "probe:phone-net"}, "celular")
    ours = {id(mg), id(ph)}
    for r in cur:
        if id(r) in ours or not r.get("address"):
            continue
        try:
            net = ipaddress.ip_interface(r["address"]).network
        except ValueError:
            continue
        if net.overlaps(ipaddress.ip_network(PHONE_NET)) or net.overlaps(ipaddress.ip_network(MGMT_NET)):
            ctx.warn("La dirección %s en %s choca con la subred del celular o de administración."
                     % (r["address"], r.get("interface")))
        elif r.get("interface") in (br, PHONE_IFACE) and any(
                net.overlaps(ipaddress.ip_network(n)) for n in FORBIDDEN_NETS):
            ctx.warn("La dirección %s en %s usa una subred prohibida (LAN de A/B u oficina)."
                     % (r["address"], r.get("interface")))
    return acts


def phase_dhcp(st, ctx):
    acts = []
    srv = first(rows(st, "/ip/dhcp-server", interface=ctx.args.bridge))
    if srv is None:
        ctx.warn("No hay servidor DHCP en el bridge: la LAN de administración queda sin DHCP.")
    else:
        pool = first(rows(st, "/ip/pool", name=srv.get("address-pool", "")))
        if pool is None:
            ctx.warn("El DHCP del bridge usa el pool '%s', que no existe." % srv.get("address-pool"))
        else:
            acts += ensure("/ip/pool", pool, {"ranges": MGMT_POOL_RANGES}, "pool de administración")
    acts += ensure("/ip/pool", first(rows(st, "/ip/pool", name=PHONE_POOL)),
                   {"name": PHONE_POOL, "ranges": PHONE_POOL_RANGES, "comment": "probe:phone-net"},
                   "pool del celular")
    nets = st.get("/ip/dhcp-server/network", [])
    mg = first([n for n in nets if n.get("comment") == "probe:mgmt"],
               [n for n in nets if n.get("address") == MGMT_NET],
               [n for n in nets if n.get("address") == OLD_MGMT_NET])
    # Sin gateway ni DNS: el PC no debe tomar una ruta por defecto hacia los routers bajo prueba.
    acts += ensure("/ip/dhcp-server/network", mg,
                   {"address": MGMT_NET, "gateway": "", "dns-server": "", "comment": "probe:mgmt"},
                   "red DHCP de administración")
    ph = first([n for n in nets if n.get("comment") == "probe:phone-net"],
               [n for n in nets if n.get("address") == PHONE_NET])
    acts += ensure("/ip/dhcp-server/network", ph,
                   {"address": PHONE_NET, "gateway": PHONE_GW, "dns-server": DNS_SERVERS,
                    "comment": "probe:phone-net"}, "red DHCP del celular")
    srvs = st.get("/ip/dhcp-server", [])
    ps = first([s for s in srvs if s.get("name") == PHONE_POOL],
               [s for s in srvs if s.get("interface") == PHONE_IFACE])
    acts += ensure("/ip/dhcp-server", ps,
                   {"name": PHONE_POOL, "interface": PHONE_IFACE, "address-pool": PHONE_POOL,
                    "lease-time": "1h", "disabled": "no", "comment": "probe:phone-net"},
                   "servidor DHCP del celular")
    clients = st.get("/ip/dhcp-client", [])
    for slot in ("B", "A"):
        w = WAN[slot]
        c = first([x for x in clients if x.get("comment") == w["comment"]],
                  [x for x in clients if x.get("interface") == w["iface"]])
        acts += ensure("/ip/dhcp-client", c,
                       {"interface": w["iface"], "add-default-route": "no", "use-peer-dns": "no",
                        "use-peer-ntp": "no", "script": wan_script(slot), "disabled": "no",
                        "comment": w["comment"]}, "WAN %s" % slot)
    return acts


def phase_wait_wan(st, ctx):
    if ctx.mode != "apply":
        return []
    c = first(rows(st, "/ip/dhcp-client", interface=WAN["A"]["iface"]))
    if c is not None and c.get("status") == "bound":
        return []
    return [Action("wait", "/ip/dhcp-client", note="hasta 15 s a que %s quede bound" % WAN["A"]["iface"])]


def wan_gateway(st, ctx, slot):
    """(gateway, bound) leído del DHCP client del WAN."""
    c = first(rows(st, "/ip/dhcp-client", interface=WAN[slot]["iface"]))
    if c is not None and c.get("status") == "bound" and c.get("gateway"):
        return c["gateway"], True
    return None, False


def phase_routes(st, ctx):
    acts = []
    routes = st.get("/ip/route", [])
    specs = [(WAN["A"]["route"], WAN["A"]["table"], "A", "1", None),
             (WAN["B"]["route"], WAN["B"]["table"], "B", "1", None),
             (WAN["A"]["main"], "main", "A", WAN["A"]["main_distance"], ctx.args.check_gateway),
             (WAN["B"]["main"], "main", "B", WAN["B"]["main_distance"], ctx.args.check_gateway)]
    for comment, table, slot, dist, chk in specs:
        w = WAN[slot]
        r = first([x for x in routes if x.get("comment") == comment])
        gw, bound = wan_gateway(st, ctx, slot)
        forced = getattr(ctx.args, "gw_" + slot.lower())
        if bound:
            gateway = "%s%%%s" % (gw, w["iface"])
        elif forced:
            gateway = "%s%%%s" % (forced, w["iface"])
        elif r is not None and r.get("gateway"):
            gateway = r["gateway"]  # WAN caído: se conserva; el script WAN lo corrige al volver
        else:
            gateway = "%s%%%s" % (w["default_gw"], w["iface"])
            ctx.warn("%s (%s) no está bound: la ruta %s se crea con %s por defecto; el script WAN "
                     "la corrige en la primera concesión (o usa --gw-%s)."
                     % (w["iface"], slot, comment, gateway, slot.lower()))
        want = {"dst-address": "0.0.0.0/0", "gateway": gateway, "routing-table": table,
                "distance": dist, "comment": comment}
        if chk:
            want["check-gateway"] = chk
        acts += ensure("/ip/route", r, want, "ruta " + comment)
        if not chk and r is not None and r.get("check-gateway") not in (None, "", "none"):
            # Forzado a un router caído, el tráfico debe fallar, no desviarse.
            acts.append(Action("set", "/ip/route", r.get(".id"), {"check-gateway": "none"},
                               "ruta " + comment, {"check-gateway": r.get("check-gateway")}))
    for r in static(routes):
        if (r.get("dst-address") == "0.0.0.0/0" and r.get("routing-table", "main") == "main"
                and not str(r.get("comment", "")).startswith("probe:")):
            ctx.warn("Hay otra ruta por defecto estática en main (%s, gateway %s): puede desviar el "
                     "respaldo." % (r.get(".id"), r.get("gateway")))
    return acts


def phase_rules(st, ctx):
    rl = st.get("/routing/rule", [])
    acts = []
    for r in rl:
        if r.get("comment") == TMP_RULE_COMMENT:
            acts.append(Action("remove", "/routing/rule", r.get(".id"), {}, "regla temporal olvidada"))
    probe = [r for r in rl if r.get("comment") == RULE_COMMENT]
    if len(probe) > 1:
        ctx.warn("Hay %d reglas con comment=%s; el celular exige exactamente una. Borra las sobrantes "
                 "a mano." % (len(probe), RULE_COMMENT))
    acts += ensure("/routing/rule", first([r for r in rl if r.get("comment") == RULE_LOCAL_COMMENT]),
                   {"dst-address": PHONE_NET, "action": "lookup-only-in-table", "table": "main",
                    "disabled": "no", "comment": RULE_LOCAL_COMMENT}, "regla local del celular")
    # En phone-probe NO se compara `table` (la mueve el celular); solo se pone main al crearla.
    acts += ensure("/routing/rule", first(probe),
                   {"src-address": PHONE_NET, "action": "lookup-only-in-table", "disabled": "no",
                    "comment": RULE_COMMENT}, "regla phone-probe", create_extra={"table": "main"})
    return acts


def phase_rules_order(st, ctx):
    rl = st.get("/routing/rule", [])
    ids = [r.get("comment") for r in rl]
    has_local = RULE_LOCAL_COMMENT in ids
    has_probe = RULE_COMMENT in ids
    if has_local and has_probe:
        li, pi = ids.index(RULE_LOCAL_COMMENT), ids.index(RULE_COMMENT)
        if li > pi:
            return [Action("move", "/routing/rule", rl[li].get(".id"),
                           {"destination": rl[pi].get(".id")}, "orden de reglas",
                           note="probe:phone-local debe ir antes de phone-probe")]
    elif has_probe and not has_local and ctx.mode != "apply":
        return [Action("move", "/routing/rule", "<probe:phone-local nueva>",
                       {"destination": first([r for r in rl if r.get("comment") == RULE_COMMENT]).get(".id")},
                       "orden de reglas", note="la regla nueva se crea al final y hay que subirla")]
    return []


def phase_nat(st, ctx):
    ok = [n for n in st.get("/ip/firewall/nat", [])
          if n.get("chain") == "srcnat" and n.get("action") == "masquerade"
          and n.get("out-interface-list") == "WAN" and not n.get("src-address")
          and not is_true(n.get("disabled"))]
    if ok:
        return []
    return [Action("add", "/ip/firewall/nat", None,
                   {"chain": "srcnat", "action": "masquerade", "out-interface-list": "WAN",
                    "comment": "probe:masq"}, "NAT")]


def phase_users(st, ctx):
    acts = ensure("/user/group", first(rows(st, "/user/group", name=API_GROUP)),
                  {"name": API_GROUP, "policy": "read,write,api", "comment": "probe:api-group"},
                  "grupo de la API")
    u = first(rows(st, "/user", name=API_USER))
    acts += ensure("/user", u, {"name": API_USER, "group": API_GROUP, "address": PHONE_NET,
                                "disabled": "no", "comment": "probe:phone-user"},
                   "usuario del celular", create_extra={"password": PASSWORD})
    if u is not None and ctx.args.rotate_phone_password:
        acts.append(Action("set", "/user", u.get(".id"), {"password": PASSWORD}, "rotar contraseña"))
    return acts


def phase_watchdog(st, ctx):
    n = ctx.args.rule_watchdog_min
    s = first(rows(st, "/system/scheduler", name=WATCHDOG_NAME))
    if n <= 0:
        if s is not None:
            ctx.warn("--rule-watchdog-min 0 pero el vigilante existe: no se toca (bórralo a mano).")
        return []
    return ensure("/system/scheduler", s,
                  {"name": WATCHDOG_NAME, "interval": "1m", "policy": WATCHDOG_POLICY,
                   "on-event": watchdog_script(n), "disabled": "no", "comment": "probe:rule-watchdog"},
                  "vigilante de la regla")


def phase_service(st, ctx):
    svc = first(rows(st, "/ip/service", name="api"))
    if svc is None:
        ctx.warn("No se encontró el servicio api.")
        return []
    return ensure("/ip/service", svc, {"disabled": "no", "address": ctx.args.api_address},
                  "servicio api")


def phase_system(st, ctx):
    acts = ensure("/ip/dns", first(st.get("/ip/dns", [])), {"servers": DNS_SERVERS}, "DNS",
                  singleton=True)
    acts += ensure("/system/ntp/client", first(st.get("/system/ntp/client", [])),
                   {"enabled": "yes", "servers": NTP_SERVERS}, "NTP", singleton=True)
    acts += ensure("/system/clock", first(st.get("/system/clock", [])),
                   {"time-zone-name": TIME_ZONE}, "zona horaria", singleton=True)
    acts += ensure("/system/identity", first(st.get("/system/identity", [])),
                   {"name": IDENTITY}, "identidad", singleton=True)
    return acts


def phase_checks(st, ctx):
    acts = []
    settings = first(st.get("/ip/settings", []))
    if settings is not None and norm(settings.get("rp-filter", "no")) != "no":
        if ctx.args.fix_rp_filter:
            acts.append(Action("set", "/ip/settings", None, {"rp-filter": "no"}, "rp-filter",
                               {"rp-filter": settings.get("rp-filter")}))
        else:
            ctx.warn("/ip settings rp-filter=%s: con 'strict' se descartan respuestas asimétricas. "
                     "Usa --fix-rp-filter para ponerlo en 'no'." % settings.get("rp-filter"))
    for a in st.get("/ipv6/address", []):
        if a.get("interface") == PHONE_IFACE and not is_true(a.get("link-local")):
            ctx.warn("%s tiene la dirección IPv6 %s: las reglas de ruteo son solo IPv4; quítala."
                     % (PHONE_IFACE, a.get("address")))
    return acts


PHASES = [
    ("MAC del bridge", phase_bridge_mac),
    ("tablas", phase_tables),
    ("listas de interfaces", phase_lists),
    ("puertos del bridge", phase_bridge_ports),
    ("direcciones", phase_addresses),
    ("DHCP", phase_dhcp),
    ("espera de WAN A", phase_wait_wan),
    ("rutas", phase_routes),
    ("reglas", phase_rules),
    ("orden de reglas", phase_rules_order),
    ("NAT", phase_nat),
    ("grupo y usuario", phase_users),
    ("vigilante", phase_watchdog),
    ("servicio API", phase_service),
    ("DNS, NTP, hora, identidad", phase_system),
    ("verificaciones", phase_checks),
]

MENUS = [
    "/interface/bridge", "/interface/bridge/port", "/interface/list/member", "/routing/table",
    "/ip/address", "/ip/pool", "/ip/dhcp-server", "/ip/dhcp-server/network", "/ip/dhcp-client",
    "/ip/route", "/routing/rule", "/ip/firewall/nat", "/user/group", "/user", "/system/scheduler",
    "/ip/service", "/ip/dns", "/system/ntp/client", "/system/clock", "/system/identity",
    "/ip/settings", "/ipv6/address",
]


OPTIONAL_MENUS = {"/ipv6/address", "/ip/settings"}  # solo se usan para avisos


def read_state(ros, ctx=None):
    """Lee todos los menús. Al aplicar, un menú que no se pudo leer aborta: tratarlo como vacío
    haría que el plan cree duplicados (reglas, rutas, usuarios) de lo que ya existe."""
    st = {}
    for m in MENUS:
        try:
            st[m] = ros.print(m)
        except ApiError as e:
            if ctx is not None and ctx.mode == "apply" and m not in OPTIONAL_MENUS:
                raise ApiError("no se pudo leer %s (%s): no aplico sobre un estado incompleto" % (m, e))
            st[m] = []
            if ctx is not None:
                ctx.warn("No se pudo leer %s: %s" % (m, e))
    return st


def plan(st, ctx):
    """[(fase, [acciones])] con todas las fases calculadas sobre el mismo estado (dry-run/verify)."""
    out = []
    for name, fn in PHASES:
        acts = fn(st, ctx)
        if acts:
            out.append((name, acts))
    return out


# ---------------------------------------------------------------------------------------------
# Ejecución
# ---------------------------------------------------------------------------------------------

def phone_password(ctx):
    if ctx.phone_password is None:
        if ctx.args.phone_password:
            ctx.phone_password = ctx.args.phone_password
        else:
            alphabet = string.ascii_letters + string.digits
            ctx.phone_password = "".join(secrets.choice(alphabet) for _ in range(16))
            ctx.password_generated = True
    return ctx.phone_password


def resolve_props(props, ctx):
    return {k: (phone_password(ctx) if v is PASSWORD else v) for k, v in props.items()}


def execute(ros, act, ctx, log=print):
    log("  [aplica] " + act.console(ctx.args.verbose))
    if act.op == "wait":
        for _ in range(15):
            c = first(ros.print("/ip/dhcp-client", {"interface": WAN["A"]["iface"]}))
            if c is not None and c.get("status") == "bound":
                log("    %s bound: %s gw %s" % (WAN["A"]["iface"], c.get("address"), c.get("gateway")))
                return
            time.sleep(1)
        ctx.warn("%s no quedó bound en 15 s (¿router A apagado?): sus rutas usan el gateway por "
                 "defecto o el anterior." % WAN["A"]["iface"])
        return
    props = resolve_props(act.props, ctx)
    if act.op == "add":
        try:
            ros.add(act.menu, props)
        except ApiError:
            if act.menu == "/routing/table" and props.get("fib") == "":
                props["fib"] = "yes"
                ros.add(act.menu, props)
            else:
                raise
        if "password" in props:
            save_password(ctx, log)
    elif act.op == "set":
        try:
            ros.set(act.menu, act.id, props)
        except ApiError:
            if props.get("check-gateway") == "none":
                props["check-gateway"] = ""
                ros.set(act.menu, act.id, props)
            else:
                raise
        if "password" in props:
            save_password(ctx, log)
    elif act.op == "remove":
        ros.remove(act.menu, act.id)
    elif act.op == "move":
        ros.move(act.menu, act.id, props["destination"])
    else:
        raise ValueError(act.op)


def save_password(ctx, log=print):
    try:
        with open(PASSWORD_FILE, "w", encoding="utf-8") as fh:
            fh.write(ctx.phone_password + "\n")
        ctx.password_saved = True
        log("    contraseña de %s guardada en %s" % (API_USER, PASSWORD_FILE))
    except OSError as e:
        ctx.warn("No se pudo guardar la contraseña en %s: %s" % (PASSWORD_FILE, e))


def apply_phases(ros, ctx, connect=None, log=print):
    """Aplica fase por fase releyendo el estado antes de cada una. Si la sesión se cae, vuelve a
    entrar (todo es idempotente) y sigue desde la fase que falló. Devuelve la sesión vigente."""
    i, restarts = 0, 0
    while i < len(PHASES):
        name, fn = PHASES[i]
        try:
            st = read_state(ros, ctx)
            acts = fn(st, ctx)
            if acts:
                log("== " + name)
            for act in acts:
                if name == "servicio API":
                    apply_service_safely(ros, act, ctx, connect, log)
                else:
                    execute(ros, act, ctx, log)
            if name == "orden de reglas" and acts:
                again = phase_rules_order(read_state(ros, ctx), ctx)
                if again:
                    raise ApiError("el orden de las reglas sigue mal después de /routing/rule/move")
            i += 1
        except (OSError, EOFError, ApiFatal) as e:
            restarts += 1
            if connect is None or restarts > 3:
                raise
            log("  !! se cayó la sesión (%s); vuelvo a entrar..." % e)
            ros.api.close()
            ros = reconnect(connect, log)
    return ros


def reconnect(connect, log=print, total_s=60):
    deadline = time.time() + total_s
    last = None
    while time.time() < deadline:
        try:
            return connect()
        except Exception as e:  # noqa: BLE001 - se reintenta cualquier falla de red
            last = e
            time.sleep(3)
    raise SystemExit("No pude volver a entrar al MikroTik en %d s: %s. Corre --dry-run para ver qué "
                     "falta (todo es idempotente)." % (total_s, last))


def apply_service_safely(ros, act, ctx, connect, log=print):
    """Cambia `address` del servicio api y prueba una conexión NUEVA; si no entra, lo revierte."""
    prev = dict(act.before)
    execute(ros, act, ctx, log)
    if connect is None:
        return
    try:
        s = connect()
        s.api.close()
        log("    conexión nueva a la API: OK")
    except Exception as e:  # noqa: BLE001
        log("    !! la conexión nueva falló (%s): revierto el servicio api" % e)
        ros.set(act.menu, act.id, {k: v for k, v in prev.items()})
        raise SystemExit("El cambio del servicio api dejaba al PC afuera; se revirtió.")


# ---------------------------------------------------------------------------------------------
# Respaldo (contrato §3.4): export + backup en el router, bajada por FTP, volcado JSON
# ---------------------------------------------------------------------------------------------

class ScopedFTP(ftplib.FTP):
    """FTP pasivo que abre el canal de datos contra el mismo host (con su %ifIndex). En Windows,
    getpeername() pierde el scope de una link-local y ftplib no podría conectar los datos."""

    def makepasv(self):
        if self.af == socket.AF_INET6:
            _, port = ftplib.parse229(self.sendcmd("EPSV"), self.sock.getpeername())
        else:
            _, port = ftplib.parse227(self.sendcmd("PASV"))
        return self.host, port


def wait_file(ros, name, timeout_s=20):
    last, stable = None, 0
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        f = first(ros.print("/file", {"name": name}))
        size = int(f.get("size", "0") or 0) if f else 0
        if size and size == last:
            stable += 1
            if stable >= 2:
                return size
        else:
            stable = 0
        last = size
        time.sleep(0.5)
    raise SystemExit("El archivo %s no apareció (o no dejó de crecer) en %d s." % (name, timeout_s))


def do_backup(ros, ctx, log=print):
    args = ctx.args
    res = first(ros.print("/system/resource"))
    free = int(res.get("free-hdd-space", "0")) if res else 0
    if free < MIN_FREE_HDD:
        raise SystemExit("Flash libre %d B < %d B: no hay espacio para el respaldo. Aborto sin tocar "
                         "nada." % (free, MIN_FREE_HDD))
    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    name = "pre-probe-" + stamp
    rname = router_dir(ros) + name     # ruta en el router (flash/... si existe)
    out_dir = args.backup_dir
    os.makedirs(out_dir, exist_ok=True)
    log("== respaldo %s (flash libre %d B)" % (name, free))

    # 1. export
    try:
        ros.run("/export", {"show-sensitive": "", "file": rname})
    except ApiError:
        try:
            ros.run("/export", {"file": rname})
        except ApiError:
            sid = ros.add("/system/script", {"name": "probe-export-tmp",
                                             "source": "/export file=%s" % rname})
            try:
                ros.run("/system/script/run", {"number": sid or "probe-export-tmp"})
            finally:
                for s in ros.print("/system/script", {"name": "probe-export-tmp"}):
                    ros.remove("/system/script", s[".id"])
    rsc = rname + ".rsc"
    rsc_size = wait_file(ros, rsc)
    # 2. backup binario (queda en el router, en flash/, para /system backup load)
    ros.run("/system/backup/save", {"name": rname, "dont-encrypt": "yes"})
    bkp = rname + ".backup"
    bkp_size = wait_file(ros, bkp)
    log("  en el router: %s (%d B), %s (%d B)" % (rsc, rsc_size, bkp, bkp_size))

    # 3. bajar por FTP
    downloaded = False
    ftp_restore_note = False
    if args.skip_download:
        ctx.warn("--skip-download: no se bajaron los archivos; el .rsc y el .backup quedan en el router.")
    else:
        ftp_svc = first(ros.print("/ip/service", {"name": "ftp"}))
        restore = None
        ftp_restore_note = False
        if ftp_svc is not None and is_true(ftp_svc.get("disabled")):
            restore = {"disabled": "yes", "address": ftp_svc.get("address", "")}
            ros.set("/ip/service", ftp_svc[".id"], {"disabled": "no", "address": ftp_allowed(args.host)})
            ftp_restore_note = True
            log("  (ftp habilitado temporalmente)")
        try:
            family, sockaddr = resolve_host(args.host, 21)
            host = args.host.strip("[]")
            ftp = ScopedFTP()
            ftp.connect(host, 21, timeout=30)
            ftp.login(args.user, args.password)
            for fname, size in ((rsc, rsc_size), (bkp, bkp_size)):
                dst = os.path.join(out_dir, fname.rsplit("/", 1)[-1])
                with open(dst, "wb") as fh:
                    ftp.retrbinary("RETR " + fname, fh.write)
                got = os.path.getsize(dst)
                if got != size:
                    raise SystemExit("Descarga incompleta de %s: %d de %d B. Aborto sin tocar nada."
                                     % (fname, got, size))
                log("  bajado %s (%d B)" % (dst, got))
            ftp.quit()
            downloaded = True
        except (OSError, ftplib.Error) as e:
            raise SystemExit("Falló la bajada por FTP (%s). Aborto sin tocar nada (usa --skip-download "
                             "para confiar en el .backup que quedó en el router)." % e)
        finally:
            if restore is not None:
                ros.set("/ip/service", ftp_svc[".id"], restore)
                log("  (ftp vuelto a deshabilitar)")

    # 4. volcado JSON de los menús que se tocan (+ cambios temporales hechos por el respaldo)
    st = read_state(ros, ctx)
    st["_respaldo"] = {"router_backup": bkp, "router_export": rsc, "downloaded": downloaded,
                       "ftp_temporalmente_habilitado": bool(ftp_restore_note)}
    dump_path = os.path.join(out_dir, name + ".json")
    with open(dump_path, "w", encoding="utf-8") as fh:
        json.dump(st, fh, indent=1, ensure_ascii=False)
    log("  volcado %s" % dump_path)

    # 5. borrar del router solo el .rsc ya bajado (poco espacio); el .backup se queda
    if downloaded:
        for f in ros.print("/file", {"name": rsc}):
            ros.remove("/file", f[".id"])
    n_old = len([f for f in ros.print("/file") if re.match(r"^(flash/)?pre-probe-.*\.backup$", f.get("name", ""))])
    if n_old > 10:
        ctx.warn("Hay %d respaldos pre-probe-*.backup en el router (flash de ~1,6 MB): borra los viejos "
                 "(ya están en %s)." % (n_old, out_dir))
    log("  para volver atrás: /system backup load name=%s" % bkp)
    return name


def router_dir(ros):
    """'flash/' si el equipo tiene esa carpeta (la raíz es RAM y se pierde al reiniciar)."""
    try:
        if ros.print("/file", {"name": ROUTER_DIR.rstrip("/")}):
            return ROUTER_DIR
    except ApiError:
        pass
    return ""


def ftp_allowed(host):
    h = host.split("%")[0].strip("[]")
    try:
        ip = ipaddress.ip_address(h)
    except ValueError:
        return ""
    if ip.version == 6 and ip.is_link_local:
        return "fe80::/10"
    return str(ipaddress.ip_network(h + ("/128" if ip.version == 6 else "/32")))


# ---------------------------------------------------------------------------------------------
# Verificación y diagnóstico
# ---------------------------------------------------------------------------------------------

def pick(r, keys):
    return " ".join("%s=%s" % (k, r.get(k)) for k in keys if r.get(k) not in (None, ""))


def report(ros, ctx, log=print):
    log("== estado")
    for c in ros.print("/ip/dhcp-client"):
        log("  dhcp-client " + pick(c, ("interface", "status", "address", "gateway", "comment")))
    log("  reglas (en orden):")
    for r in ros.print("/routing/rule"):
        log("    " + pick(r, ("comment", "src-address", "dst-address", "action", "table",
                                "disabled", "inactive")))
    log("  rutas por defecto:")
    for r in ros.print("/ip/route", {"dst-address": "0.0.0.0/0"}):
        log("    " + pick(r, ("comment", "routing-table", "gateway", "distance", "check-gateway",
                                "active", "dynamic")))
    for iface in (WAN["A"]["iface"], WAN["B"]["iface"]):
        for a in ros.print("/ip/arp", {"interface": iface}):
            log("  arp " + pick(a, ("interface", "address", "mac-address", "complete", "status")))
    eth = first(ros.print("/interface", {"name": PHONE_IFACE}))
    running = is_true(eth.get("running")) if eth else False
    srv = first(ros.print("/ip/dhcp-server", {"name": PHONE_POOL}))
    log("  celular: %s %s; DHCP %s%s" % (
        PHONE_IFACE, "con enlace" if running else "SIN enlace (¿cable/adaptador del celular?)",
        PHONE_POOL, " inválido" if srv and is_true(srv.get("invalid")) else ""))
    if srv and is_true(srv.get("invalid")) and running:
        ctx.warn("El DHCP %s está inválido con %s con enlace: revisa la dirección %s en %s."
                 % (PHONE_POOL, PHONE_IFACE, PHONE_ADDR, PHONE_IFACE))
    for lease in ros.print("/ip/dhcp-server/lease", {"server": PHONE_POOL}):
        log("  lease " + pick(lease, ("address", "mac-address", "host-name", "status", "last-seen")))
    for s in ros.print("/system/scheduler", {"name": WATCHDOG_NAME}):
        log("  vigilante " + pick(s, ("interval", "run-count", "next-run", "disabled")))
    clk = first(ros.print("/system/clock"))
    ntp = first(ros.print("/system/ntp/client"))
    if clk:
        log("  reloj %s %s %s" % (clk.get("date"), clk.get("time"), clk.get("time-zone-name")))
    if ntp:
        log("  ntp " + pick(ntp, ("enabled", "status", "synced-server", "system-offset")))

    log("== pruebas")
    main_routes = {r.get("comment"): r for r in ros.print("/ip/route", {"dst-address": "0.0.0.0/0"})}
    for slot in ("A", "B"):
        w = WAN[slot]
        c = first(ros.print("/ip/dhcp-client", {"interface": w["iface"]}))
        gw = c.get("gateway") if c and c.get("status") == "bound" else None
        if not gw:
            log("  %s (%s): DHCP no bound; sin ping al gateway" % (slot, w["iface"]))
            continue
        try:
            rr = ros.run("/ping", {"address": gw, "interface": w["iface"], "count": "2"})
            last = rr[-1] if rr else {}
            log("  ping gateway %s (%s %s): recibidos %s/%s" % (slot, gw, w["iface"],
                                                               last.get("received"), last.get("sent")))
            mr = main_routes.get(w["main"])
            if last.get("received") in ("0", None) or (mr is not None and not is_true(mr.get("active"))
                                                        and slot == "A"):
                ctx.warn("El gateway de %s no responde ping o su ruta de main está inactiva: con el "
                         "router encendido, prueba --check-gateway arp." % slot)
        except ApiError as e:
            log("  ping gateway %s: error %s" % (slot, e))
    if not ctx.args.no_egress_test:
        egress_test(ros, ctx, log)
    wr = windows_default_route(ctx.args.host)
    if wr:
        ctx.warn("El adaptador del PC hacia el MikroTik tiene puerta de enlace %s: el PC podría salir "
                 "a Internet por los routers bajo prueba. Corre 'ipconfig /renew' en ese adaptador "
                 "(la red de administración ya no entrega gateway) o sube su métrica." % ", ".join(wr))


def egress_test(ros, ctx, log=print):
    """IP pública de salida por cada tabla. La API de 7.6 no acepta /ping routing-table=, así que
    se crean reglas temporales dst=<IP de prueba>/32 -> to-A/to-B (tráfico del propio MikroTik) y
    se borran al terminar. No afecta al celular: su regla va antes y termina la búsqueda."""
    created = []
    try:
        for slot in ("A", "B"):
            w = WAN[slot]
            ros.add("/routing/rule", {"dst-address": w["egress_ip"] + "/32",
                                      "action": "lookup-only-in-table", "table": w["table"],
                                      "comment": TMP_RULE_COMMENT})
            created.append(slot)
        seen = {}
        for slot in ("A", "B"):
            w = WAN[slot]
            ip = w["egress_ip"]
            try:
                rr = ros.run("/ping", {"address": ip, "count": "3"})
                last = rr[-1] if rr else {}
                ping = "pérdida %s%% promedio %s" % (last.get("packet-loss"), last.get("avg-rtt"))
            except ApiError as e:
                ping = "error %s" % e
            trace = ""
            try:
                rr = ros.run("/tool/fetch", {"url": "https://%s/cdn-cgi/trace" % ip, "output": "user",
                                             "check-certificate": "no"})
                data = "".join(r.get("data", "") for r in rr)
                kv = dict(line.split("=", 1) for line in data.splitlines() if "=" in line)
                seen[slot] = kv.get("ip")
                trace = "salida %s (colo %s)" % (kv.get("ip"), kv.get("colo"))
            except ApiError as e:
                trace = "fetch error %s" % e
            log("  por %s (%s): ping %s; %s" % (w["table"], ip, ping, trace))
        if seen.get("A") and seen.get("A") == seen.get("B"):
            ctx.warn("A y B salen con la misma IP pública (%s): algo no está forzando la ruta." % seen["A"])
    except ApiError as e:
        log("  prueba de salida: error %s" % e)
    finally:
        try:
            for r in ros.print("/routing/rule"):
                if r.get("comment") == TMP_RULE_COMMENT:
                    ros.remove("/routing/rule", r[".id"])
        except (ApiError, ApiFatal, OSError, EOFError) as e:
            ctx.warn("No pude borrar las reglas temporales %s (%s): el próximo --dry-run/--apply "
                     "las borra." % (TMP_RULE_COMMENT, e))


def windows_default_route(host):
    """Puertas de enlace por defecto del adaptador Windows del %ifIndex (solo en Windows)."""
    if os.name != "nt" or "%" not in host:
        return []
    scope = host.split("%", 1)[1].strip("]")
    if not scope.isdigit():
        return []
    try:
        out = subprocess.run(
            ["powershell", "-NoProfile", "-Command",
             "Get-NetRoute -InterfaceIndex %s -DestinationPrefix 0.0.0.0/0 -ErrorAction "
             "SilentlyContinue | ForEach-Object { $_.NextHop }" % scope],
            capture_output=True, text=True, timeout=30).stdout
    except (OSError, subprocess.SubprocessError):
        return []
    return [x for x in out.split() if x and x != "0.0.0.0"]


# ---------------------------------------------------------------------------------------------
# Script .rsc equivalente (para pegar en WinBox o /import)
# ---------------------------------------------------------------------------------------------

def rsc_str(s):
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"').replace("$", "\\$") + '"'


def rsc_val(v):
    v = str(v)
    if re.fullmatch(r"[A-Za-z0-9_.,:/\-]+", v):
        return v
    return rsc_str(v)


def rsc_props(props):
    return " ".join("%s=%s" % (k, rsc_val(v)) for k, v in props.items())


def rsc_ensure(menu, finds, props, create=None, else_extra=""):
    """Bloque {…} que busca con `finds` (en orden) y hace add o set."""
    lines = ["{", ":local x [%s find where %s]" % (menu, finds[0])]
    for f in finds[1:]:
        lines.append(":if ([:len $x] = 0) do={:set x [%s find where %s]}" % (menu, f))
    add = "%s add %s" % (menu, rsc_props(dict(props, **(create or {}))))
    st = "%s set [:pick $x 0] %s" % (menu, rsc_props(props))
    if else_extra:
        st += "; " + else_extra
    lines.append(":if ([:len $x] = 0) do={%s} else={%s}" % (add, st))
    lines.append("}")
    return lines


def emit_rsc(args):
    n = args.rule_watchdog_min
    L = [
        "# probe-ab.rsc - sonda A/B del celular por cable (hAP ac2, RouterOS 7.6)",
        "# GENERADO por: python apply_probe.py --emit-rsc   (no editar a mano; regenerar)",
        "# Contrato: server/docs/CONTRATO-SONDA-AB.md, seccion 3.",
        "#",
        "# Lo recomendado es apply_probe.py --apply (hace respaldo, compara y solo cambia lo que",
        "# difiere). Este script es la alternativa manual e idempotente (se puede correr varias",
        "# veces; reescribe los valores aunque ya esten bien). NO hace respaldo: antes, en la",
        "# terminal:  /export file=flash/pre-probe-manual  y  /system backup save name=flash/pre-probe-manual",
        "# y baja los dos archivos (Files en WinBox).",
        "#",
        "# Uso: cambia CAMBIAR-ESTA-CLAVE (solo se usa si el usuario phone-probe no existe),",
        "# sube el archivo a Files y corre  /import file-name=probe-ab.rsc   (o pegalo en la",
        "# terminal de WinBox). Todo va en un solo bloque { }: si algo falla, se detiene.",
        "#",
        "# Diferencias con apply_probe.py: no compara (siempre hace set); la regla NAT se busca",
        "# sin mirar src-address; no hace las verificaciones finales (usa --verify).",
        "",
        "{",
        ':local phonePass "CAMBIAR-ESTA-CLAVE"',
        ':local gwA "%s"' % WAN["A"]["default_gw"],
        ':local gwB "%s"' % WAN["B"]["default_gw"],
        ":local boundA false",
        ":local boundB false",
        ':if (([:len [/user find where name="%s"]] = 0) && ($phonePass = "CAMBIAR-ESTA-CLAVE")) do={'
        ':error "Cambia phonePass al comienzo del script antes de correrlo"}' % API_USER,
        ':put "sonda A/B: 1/14 MAC fija del bridge"',
        "{",
        ':local b [/interface bridge find where name="%s"]' % args.bridge,
        ':if ([:len $b] = 1) do={:if ([/interface bridge get [:pick $b 0] auto-mac]) do={'
        '/interface bridge set [:pick $b 0] auto-mac=no admin-mac=[/interface bridge get [:pick $b 0] mac-address]}}',
        "}",
        ':put "sonda A/B: 2/14 tablas"',
    ]
    for slot in ("A", "B"):
        t = WAN[slot]["table"]
        L.append(':if ([:len [/routing table find where name="%s"]] = 0) do={/routing table add name=%s fib comment="probe:%s"}'
                 % (t, t, t))
    L.append(':put "sonda A/B: 3/14 listas de interfaces"')
    for lst, iface, comment in (("WAN", WAN["B"]["iface"], "probe:wan-B"),
                                ("WAN", WAN["A"]["iface"], "probe:wan-A"),
                                ("LAN", PHONE_IFACE, "probe:phone")):
        L.append(':if ([:len [/interface list member find where list="%s" and interface="%s"]] = 0) do={'
                 '/interface list member add list=%s interface=%s comment="%s"}' % (lst, iface, lst, iface, comment))
    L.append(':put "sonda A/B: 4/14 sacar %s y %s del bridge"' % (WAN["A"]["iface"], PHONE_IFACE))
    for iface in (WAN["A"]["iface"], PHONE_IFACE):
        L.append('/interface bridge port remove [find where interface="%s"]' % iface)
    L.append(':put "sonda A/B: 5/14 direcciones"')
    L += rsc_ensure("/ip address",
                    ['comment="probe:mgmt"',
                     'interface="%s" and address="%s"' % (args.bridge, MGMT_ADDR),
                     'interface="%s" and address="%s"' % (args.bridge, OLD_MGMT_ADDR)],
                    {"address": MGMT_ADDR, "network": MGMT_NET.split("/")[0], "interface": args.bridge,
                     "comment": "probe:mgmt"})
    L += rsc_ensure("/ip address",
                    ['comment="probe:phone-net"', 'interface="%s" and address="%s"' % (PHONE_IFACE, PHONE_ADDR)],
                    {"address": PHONE_ADDR, "interface": PHONE_IFACE, "comment": "probe:phone-net"})
    L.append(':put "sonda A/B: 6/14 DHCP"')
    L += ["{",
          ':local s [/ip dhcp-server find where interface="%s"]' % args.bridge,
          ':if ([:len $s] > 0) do={:local p [/ip dhcp-server get [:pick $s 0] address-pool]; '
          '/ip pool set [find where name=$p] ranges=%s}' % MGMT_POOL_RANGES,
          "}"]
    L += rsc_ensure("/ip pool", ['name="%s"' % PHONE_POOL],
                    {"name": PHONE_POOL, "ranges": PHONE_POOL_RANGES, "comment": "probe:phone-net"})
    L += rsc_ensure("/ip dhcp-server network",
                    ['comment="probe:mgmt"', 'address="%s"' % MGMT_NET, 'address="%s"' % OLD_MGMT_NET],
                    {"address": MGMT_NET, "gateway": "", "dns-server": "", "comment": "probe:mgmt"})
    L += rsc_ensure("/ip dhcp-server network", ['comment="probe:phone-net"', 'address="%s"' % PHONE_NET],
                    {"address": PHONE_NET, "gateway": PHONE_GW, "dns-server": DNS_SERVERS,
                     "comment": "probe:phone-net"})
    L += rsc_ensure("/ip dhcp-server", ['name="%s"' % PHONE_POOL, 'interface="%s"' % PHONE_IFACE],
                    {"name": PHONE_POOL, "interface": PHONE_IFACE, "address-pool": PHONE_POOL,
                     "lease-time": "1h", "disabled": "no", "comment": "probe:phone-net"})
    for slot in ("B", "A"):
        w = WAN[slot]
        L += rsc_ensure("/ip dhcp-client", ['comment="%s"' % w["comment"], 'interface="%s"' % w["iface"]],
                        {"interface": w["iface"], "add-default-route": "no", "use-peer-dns": "no",
                         "use-peer-ntp": "no", "script": wan_script(slot), "disabled": "no",
                         "comment": w["comment"]})
    L.append(':put "sonda A/B: 7/14 esperando DHCP de los WAN (hasta 15 s)"')
    L += ["{", ":local i 0",
          ':while (($i < 15) && ([:len [/ip dhcp-client find where interface="%s" and status="bound"]] = 0)) do={:delay 1s; :set i ($i + 1)}'
          % WAN["A"]["iface"], "}"]
    for slot, var, bvar in (("A", "gwA", "boundA"), ("B", "gwB", "boundB")):
        L += ["{",
              ':local c [/ip dhcp-client find where interface="%s" and status="bound"]' % WAN[slot]["iface"],
              ':if ([:len $c] > 0) do={:set %s [/ip dhcp-client get [:pick $c 0] gateway]; :set %s true}'
              % (var, bvar),
              "}"]
    L.append(':put "sonda A/B: 8/14 rutas"')
    for comment, table, slot, dist, chk in ((WAN["A"]["route"], WAN["A"]["table"], "A", "1", None),
                                            (WAN["B"]["route"], WAN["B"]["table"], "B", "1", None),
                                            (WAN["A"]["main"], "main", "A", WAN["A"]["main_distance"], args.check_gateway),
                                            (WAN["B"]["main"], "main", "B", WAN["B"]["main_distance"], args.check_gateway)):
        w = WAN[slot]
        var, bvar = ("gwA", "boundA") if slot == "A" else ("gwB", "boundB")
        gwexpr = '($%s . "%%%s")' % (var, w["iface"])
        base = "dst-address=0.0.0.0/0 routing-table=%s distance=%s" % (table, dist)
        if chk:
            base += " check-gateway=%s" % chk
        L += ["{",
              ':local x [/ip route find where comment="%s"]' % comment,
              ':if ([:len $x] = 0) do={/ip route add %s gateway=%s comment="%s"} else={'
              '/ip route set [:pick $x 0] %s; :if ($%s) do={/ip route set [:pick $x 0] gateway=%s}}'
              % (base, gwexpr, comment, base, bvar, gwexpr),
              "}"]
    L.append(':put "sonda A/B: 9/14 reglas de ruteo"')
    L += rsc_ensure("/routing rule", ['comment="%s"' % RULE_LOCAL_COMMENT],
                    {"dst-address": PHONE_NET, "action": "lookup-only-in-table", "table": "main",
                     "disabled": "no", "comment": RULE_LOCAL_COMMENT})
    L += rsc_ensure("/routing rule", ['comment="%s"' % RULE_COMMENT],
                    {"src-address": PHONE_NET, "action": "lookup-only-in-table", "disabled": "no",
                     "comment": RULE_COMMENT}, create={"table": "main"})
    L += ["{", ":local seenProbe false", ":local wrong false",
          ':foreach r in=[/routing rule find] do={:local c [/routing rule get $r comment]; '
          ':if ($c = "%s") do={:set seenProbe true}; :if (($c = "%s") && $seenProbe) do={:set wrong true}}'
          % (RULE_COMMENT, RULE_LOCAL_COMMENT),
          ':if ($wrong) do={/routing rule move [find where comment="%s"] destination=[find where comment="%s"]}'
          % (RULE_LOCAL_COMMENT, RULE_COMMENT),
          "}"]
    L.append(':put "sonda A/B: 10/14 NAT"')
    L.append(':if ([:len [/ip firewall nat find where chain="srcnat" and action="masquerade" and '
             'out-interface-list="WAN"]] = 0) do={/ip firewall nat add chain=srcnat action=masquerade '
             'out-interface-list=WAN comment="probe:masq"}')
    L.append(':put "sonda A/B: 11/14 grupo y usuario del celular"')
    L += rsc_ensure("/user group", ['name="%s"' % API_GROUP],
                    {"name": API_GROUP, "policy": "read,write,api", "comment": "probe:api-group"})
    L += ["{",
          ':local x [/user find where name="%s"]' % API_USER,
          ':if ([:len $x] = 0) do={/user add name=%s group=%s address=%s password=$phonePass comment="probe:phone-user"} '
          'else={/user set [:pick $x 0] group=%s address=%s disabled=no comment="probe:phone-user"}'
          % (API_USER, API_GROUP, PHONE_NET, API_GROUP, PHONE_NET),
          "}"]
    L.append(':put "sonda A/B: 12/14 vigilante de la regla"')
    if n > 0:
        L += rsc_ensure("/system scheduler", ['name="%s"' % WATCHDOG_NAME],
                        {"name": WATCHDOG_NAME, "interval": "1m", "policy": WATCHDOG_POLICY,
                         "on-event": watchdog_script(n), "disabled": "no", "comment": "probe:rule-watchdog"})
    L.append(':put "sonda A/B: 13/14 servicio api (incluye fe80::/10: sin eso el PC queda afuera)"')
    L.append('/ip service set [find where name="api"] disabled=no address=%s' % rsc_val(args.api_address))
    L.append(':put "sonda A/B: 14/14 DNS, NTP, hora, identidad"')
    L.append("/ip dns set servers=%s" % DNS_SERVERS)
    L.append("/system ntp client set enabled=yes servers=%s" % NTP_SERVERS)
    L.append("/system clock set time-zone-name=%s" % TIME_ZONE)
    L.append("/system identity set name=%s" % IDENTITY)
    L.append(':if ([/ip settings get rp-filter] != "no") do={:put "AVISO: /ip settings rp-filter no es no"}')
    L.append(':put "sonda A/B: listo. Verifica con apply_probe.py --verify"')
    L.append("}")
    return "\n".join(L) + "\n"


# ---------------------------------------------------------------------------------------------
# Modos
# ---------------------------------------------------------------------------------------------

def print_plan(p, ctx, log=print):
    total = 0
    for name, acts in p:
        log("== " + name)
        for a in acts:
            log("  " + a.console(ctx.args.verbose))
            total += 1
    return total


def print_warnings(ctx, log=print):
    for w in ctx.warnings:
        log("AVISO: " + w)


def rule_state(st):
    r = [x for x in st.get("/routing/rule", []) if x.get("comment") == RULE_COMMENT]
    return r[0].get("table") if len(r) == 1 else None


def mode_dry(ros, ctx, log=print):
    st = read_state(ros, ctx)
    p = plan(st, ctx)
    n = print_plan(p, ctx, log)
    print_warnings(ctx, log)
    if n == 0:
        log("Sin cambios: el MikroTik ya coincide con el contrato.")
    else:
        log("%d cambio(s). Nada se tocó (dry-run). Para aplicar: python apply_probe.py --password ... --apply"
            % n)
    t = rule_state(st)
    if t not in (None, "main"):
        log("AVISO: la regla %s está en %s (¿el celular está midiendo?); --apply pedirá --force."
            % (RULE_COMMENT, t))
    return 0


def mode_verify(ros, ctx, log=print):
    st = read_state(ros, ctx)
    p = plan(st, ctx)
    n = 0
    if p:
        log("== diferencias con lo esperado")
        n = print_plan(p, ctx, log)
    else:
        log("Configuración: coincide con el contrato.")
    report(ros, ctx, log)
    print_warnings(ctx, log)
    return 1 if n else 0


def mode_apply(ros, ctx, connect, log=print):
    st = read_state(ros, ctx)
    t = rule_state(st)
    if t not in (None, "main") and not ctx.args.force:
        raise SystemExit("La regla %s está en %s: la sonda parece estar midiendo. Detén la sonda en la "
                         "app o usa --force." % (RULE_COMMENT, t))
    pending = plan(st, ctx)
    if not pending:
        log("Sin cambios: el MikroTik ya coincide con el contrato. No se hace respaldo.")
        print_warnings(ctx, log)
        return ros, 0
    do_backup(ros, ctx, log)
    ros = apply_phases(ros, ctx, connect, log)
    log("== comprobación (debe dar cero cambios)")
    ctx2 = Ctx(ctx.args, "verify")
    left = print_plan(plan(read_state(ros, ctx2), ctx2), ctx2, log)
    if left:
        ctx.warn("Quedaron %d diferencia(s) tras aplicar; revisa arriba." % left)
    else:
        log("  cero cambios pendientes")
    report(ros, ctx, log)
    if ctx.phone_password is not None:
        if ctx.args.show_password or not ctx.password_saved:
            log("Contraseña de %s: %s" % (API_USER, ctx.phone_password))
        if ctx.password_saved:
            log("Contraseña de %s guardada en %s; ponla en la app (Ajustes de la sonda > MikroTik)."
                % (API_USER, PASSWORD_FILE))
    print_warnings(ctx, log)
    return ros, (1 if left else 0)


def mode_switch(ros, ctx, target, log=print):
    table = {"A": WAN["A"]["table"], "B": WAN["B"]["table"], "fallback": "main"}[target]
    r = ros.print("/routing/rule", {"comment": RULE_COMMENT})
    if len(r) != 1:
        raise SystemExit("Hay %d reglas %s (se espera una). Corre --apply primero." % (len(r), RULE_COMMENT))
    ros.set("/routing/rule", r[0][".id"], {"table": table, "action": "lookup-only-in-table", "disabled": "no"})
    back = first(ros.print("/routing/rule", {"comment": RULE_COMMENT}))
    log("regla %s: table=%s action=%s inactive=%s" % (RULE_COMMENT, back.get("table"), back.get("action"),
                                                       back.get("inactive", "false")))
    if table != "main":
        for rt in ros.print("/ip/route", {"routing-table": table, "dst-address": "0.0.0.0/0"}):
            log("ruta de %s: gateway=%s active=%s" % (table, rt.get("gateway"), rt.get("active", "false")))
        log("OJO: el vigilante la devuelve a main a los %d min; vuelve con --switch fallback."
            % ctx.args.rule_watchdog_min)
    return 0 if back.get("table") == table else 1


def build_parser():
    p = argparse.ArgumentParser(
        description="Configura el hAP ac2 como sonda A/B (contrato §3). Por defecto: dry-run.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="Volver atrás: /system backup load name=flash/pre-probe-<fecha>.backup (reinicia el equipo).")
    m = p.add_mutually_exclusive_group()
    m.add_argument("--dry-run", action="store_true", help="mostrar el plan sin tocar nada (por defecto)")
    m.add_argument("--apply", action="store_true", help="respaldo obligatorio y aplicar")
    m.add_argument("--verify", action="store_true", help="comparar con lo esperado y probar cada WAN")
    m.add_argument("--emit-rsc", action="store_true", help="imprimir el .rsc equivalente (sin conectarse)")
    m.add_argument("--backup-only", action="store_true", help="solo el respaldo")
    m.add_argument("--switch", choices=("A", "B", "fallback"), help="mover la regla phone-probe (pruebas a mano)")
    p.add_argument("--host", default=DEFAULT_HOST, help="dirección del MikroTik (por defecto %(default)s)")
    p.add_argument("--port", type=int, default=API_PORT)
    p.add_argument("--user", default="admin")
    p.add_argument("--password", help="contraseña de --user (obligatoria salvo --emit-rsc; también "
                   "se toma de la variable de entorno MIKROTIK_PASSWORD, que no queda en el historial)")
    p.add_argument("--bridge", default=BRIDGE)
    p.add_argument("--force", action="store_true", help="aplicar aunque la regla phone-probe no esté en main")
    p.add_argument("--gw-a", help="gateway de A si ether2 no está bound (por defecto %s)" % WAN["A"]["default_gw"])
    p.add_argument("--gw-b", help="gateway de B si ether1 no está bound (por defecto %s)" % WAN["B"]["default_gw"])
    p.add_argument("--check-gateway", choices=("ping", "arp"), default="ping",
                   help="check-gateway de las rutas de respaldo en main")
    p.add_argument("--rule-watchdog-min", type=int, default=10, help="minutos del vigilante (0 = no instalarlo)")
    p.add_argument("--api-address", default=API_ADDRESS_DEFAULT,
                   help="address= del servicio api (debe incluir fe80::/10)")
    p.add_argument("--phone-password", help="contraseña para phone-probe (si no, se genera)")
    p.add_argument("--rotate-phone-password", action="store_true")
    p.add_argument("--show-password", action="store_true",
                   help="imprimir la contraseña nueva de phone-probe (por defecto solo se guarda en el archivo)")
    p.add_argument("--fix-rp-filter", action="store_true")
    p.add_argument("--skip-download", action="store_true", help="no bajar el respaldo por FTP")
    p.add_argument("--no-egress-test", action="store_true",
                   help="en --verify, no crear reglas temporales para ver la IP de salida")
    p.add_argument("--backup-dir", default=os.path.join(BASE_DIR, "backups"))
    p.add_argument("--verbose", action="store_true", help="no recortar valores largos (scripts)")
    return p


def validate_args(args):
    if "fe80::/10" not in split_list(args.api_address):
        raise SystemExit("--api-address debe incluir fe80::/10 (el PC entra por link-local); me niego.")
    for net in split_list(args.api_address):
        ipaddress.ip_network(net, strict=False)
    if args.rule_watchdog_min < 0:
        raise SystemExit("--rule-watchdog-min no puede ser negativo")


def main(argv=None):
    try:
        sys.stdout.reconfigure(errors="replace")
    except (AttributeError, ValueError):
        pass
    args = build_parser().parse_args(argv)
    validate_args(args)
    if args.emit_rsc:
        sys.stdout.write(emit_rsc(args))
        return 0
    if not args.password:
        args.password = os.environ.get("MIKROTIK_PASSWORD") or None
    if not args.password:
        raise SystemExit("Falta --password o MIKROTIK_PASSWORD (la de %s en el MikroTik)." % args.user)

    def connect():
        api = Api.connect(args.host, args.port)
        try:
            api.login(args.user, args.password)
        except Exception:
            api.close()
            raise
        return RouterOS(api)

    try:
        ros = connect()
    except ApiError as e:
        raise SystemExit("Login rechazado: %s" % e)
    except OSError as e:
        raise SystemExit("No pude conectar a %s:%d (%s). ¿Cable en ether5? ¿ifIndex correcto "
                         "(Get-NetAdapter)?" % (args.host, args.port, e))
    ident = first(ros.print("/system/identity"))
    res = first(ros.print("/system/resource"))
    print("Conectado a %s (%s, RouterOS %s, flash libre %s B)"
          % (args.host, ident.get("name") if ident else "?", res.get("version") if res else "?",
             res.get("free-hdd-space") if res else "?"))
    mode = "apply" if args.apply else "verify" if args.verify else "dry"
    ctx = Ctx(args, mode)
    try:
        if args.switch:
            return mode_switch(ros, ctx, args.switch)
        if args.backup_only:
            do_backup(ros, ctx)
            print_warnings(ctx)
            return 0
        if args.apply:
            ros, rc = mode_apply(ros, ctx, connect)
            return rc
        if args.verify:
            return mode_verify(ros, ctx)
        return mode_dry(ros, ctx)
    except ApiError as e:
        print_warnings(ctx)
        raise SystemExit("El MikroTik rechazó un comando: %s. Corre --dry-run para ver qué falta "
                         "(todo es idempotente)." % e)
    finally:
        ros.api.close()


if __name__ == "__main__":
    sys.exit(main())
