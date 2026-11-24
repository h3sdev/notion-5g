#!/usr/bin/env python3
"""Prueba de punta a punta de /notion_ttl.cgi en el módem (auth, CSRF, validación, persistencia).

Uso (desde la raíz del repo):  NOTION_PASS=... python3 firmware-mods/notion-ttl/test_cgi.py
Deja el equipo en enabled=1, ttl=64, ipv6=1 al terminar.
"""
import os, subprocess, sys, urllib.error, urllib.request

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
sys.path.insert(0, os.path.join(ROOT, "scripts"))
from modem_http_api import ModemHTTP  # noqa: E402

U = "http://192.168.1.1/notion_ttl.cgi"
m = ModemHTTP(); m.login(); SID = m.cgisid
fails = 0


def t(name, expect, data=None, sid=SID, xrw=True, ttl=None):
    global fails
    h = {}
    if sid:
        h["Cookie"] = f"CGISID={sid}"
    if xrw and data is not None:
        h["X-Requested-With"] = "XMLHttpRequest"
    req = urllib.request.Request(U, data=data.encode() if data else None, headers=h)
    try:
        r = urllib.request.urlopen(req, timeout=15); code, body = r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        code, body = e.code, e.read().decode()
    ok = code == expect and (ttl is None or f'"ttl":{ttl},' in body)
    fails += not ok
    print(f"{'OK ' if ok else 'FALLA'} {name}: [{code}] {body.strip()}")


def rsh(c):
    print(subprocess.run([os.path.join(ROOT, "scripts/rsh.sh"), c], capture_output=True, text=True).stdout.rstrip())


t("sin sesión", 401, sid=None)
t("sesión falsa", 401, sid="abc123")
t("GET", 200)
t("POST sin X-Requested-With (CSRF)", 403, "enabled=1&ttl=65&ipv6=1", xrw=False)
t("POST ttl=300", 400, "enabled=1&ttl=300&ipv6=1")
t("POST ttl=0", 400, "enabled=1&ttl=0&ipv6=1")
t("POST inyección ttl=6;reboot", 400, "enabled=1&ttl=6%3Breboot&ipv6=1")
t("POST ttl=65", 200, "enabled=1&ttl=65&ipv6=1", ttl=65)
print("--- fw3 reload (lo mismo que pasa en cada cambio 4G/5G):")
rsh("fw3 -q reload; sleep 1; iptables -t mangle -S POSTROUTING | grep notion; "
    "ip6tables -t mangle -S POSTROUTING | grep notion; cat /etc/config/notion_ttl")
t("POST deshabilitar", 200, "enabled=0&ttl=65&ipv6=1")
rsh('echo "reglas tras deshabilitar: $(iptables -t mangle -S POSTROUTING | grep -c notion)"')
t("POST ttl=010 (no octal)", 200, "enabled=1&ttl=010&ipv6=1", ttl=10)
t("POST ttl=000", 400, "enabled=1&ttl=000&ipv6=1")
t("POST volver a 64 (ttl=064)", 200, "enabled=1&ttl=064&ipv6=1", ttl=64)
rsh("logread | grep notion-ttl | tail -6")
print("TODO OK" if not fails else f"{fails} FALLAS")
sys.exit(1 if fails else 0)
