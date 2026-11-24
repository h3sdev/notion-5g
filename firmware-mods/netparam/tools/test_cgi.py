#!/usr/bin/env python3
"""End-to-end test of /netparam.cgi on the device (auth, CSRF, validation, persistence).

Usage:  NOTION_PASS=<root ssh password> python3 tools/test_cgi.py
Leaves the device at enabled=1, value=64, ipv6=1.
"""
import os, subprocess, sys, urllib.error, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from web_login import login  # noqa: E402

U = "http://192.168.1.1/netparam.cgi"
SID = login()
fails = 0


def t(name, expect, data=None, sid=SID, xrw=True, ttl=None, has=()):
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
    ok = code == expect and (ttl is None or f'"value":{ttl},' in body) and all(x in body for x in has)
    fails += not ok
    print(f"{'OK  ' if ok else 'FAIL'} {name}: [{code}] {body.strip()}")


def rsh(c):
    print(subprocess.run([os.path.join(HERE, "rsh.sh"), c], capture_output=True, text=True).stdout.rstrip())


# What a browser can see (served files and the JSON) must not name the feature
for path in ("/js/panel/router/netparam.js", "/html/router/netparam.html"):
    src = urllib.request.urlopen("http://192.168.1.1" + path, timeout=15).read().decode().lower()
    ok = "ttl" not in src and "hop limit" not in src
    fails += not ok
    print(f"{'OK  ' if ok else 'FAIL'} served {path} does not mention ttl/hop limit")

t("no session", 401, sid=None)
t("forged session", 401, sid="abc123")
t("GET: JSON keys do not mention ttl", 200, has=('"value":',))
t("POST without X-Requested-With (CSRF)", 403, "enabled=1&value=65&ipv6=1", xrw=False)
t("POST ttl=300", 400, "enabled=1&value=300&ipv6=1")
t("POST ttl=0", 400, "enabled=1&value=0&ipv6=1")
t("POST injection ttl=6;reboot", 400, "enabled=1&value=6%3Breboot&ipv6=1")
t("POST ttl=65", 200, "enabled=1&value=65&ipv6=1", ttl=65)
print("--- fw3 reload (same as every 4G/5G change):")
rsh("fw3 -q reload; sleep 1; iptables -t mangle -S POSTROUTING | grep netparam; "
    "ip6tables -t mangle -S POSTROUTING | grep netparam; cat /etc/config/netparam")
t("POST disable", 200, "enabled=0&value=65&ipv6=1")
rsh('echo "rules after disable: $(iptables -t mangle -S POSTROUTING | grep -c netparam)"')
t("POST ttl=010 (not octal)", 200, "enabled=1&value=010&ipv6=1", ttl=10)
t("POST ttl=000", 400, "enabled=1&value=000&ipv6=1")
t("POST back to 64 (ttl=064)", 200, "enabled=1&value=064&ipv6=1", ttl=64)
t("POST inbound=2", 400, "enabled=1&value=64&ipv6=1&inbound=2")
t("POST inbound=0: outgoing only", 200, "enabled=1&value=64&ipv6=1&inbound=0", has=('"inbound":0', '"active_in":false'))
rsh('echo "INPUT/FORWARD rules with inbound=0: $(iptables -t mangle -S | grep netparam | grep -c -E "^-A (INPUT|FORWARD)")"')
t("POST inbound=1: incoming too", 200, "enabled=1&value=64&ipv6=1&inbound=1", has=('"inbound":1', '"active_in":true'))
rsh('echo "INPUT/FORWARD rules with inbound=1: $(iptables -t mangle -S | grep netparam | grep -c -E "^-A (INPUT|FORWARD)") v4, '
    '$(ip6tables -t mangle -S | grep netparam | grep -c -E "^-A (INPUT|FORWARD)") v6; '
    'modem ping shows $(ping -c1 8.8.8.8 | grep -o "ttl=[0-9]*")"')
t("POST action=bogus", 400, "action=bogus")
t("POST action=remove without X-Requested-With (CSRF)", 403, "action=remove", xrw=False)
t("POST ttl=70 before remove/restore", 200, "enabled=1&value=70&ipv6=0", ttl=70)
t("POST action=remove: disabled, no rules", 200, "action=remove",
  has=('"enabled":0', '"active_v4":false', '"active_v6":false', '"active_in":false'))
rsh('echo "rules after remove: $(iptables -t mangle -S | grep -c netparam) v4, '
    '$(ip6tables -t mangle -S | grep -c netparam) v6"')
t("POST action=restore: enabled, 64, IPv6", 200, "action=restore", ttl=64,
  has=('"enabled":1', '"ipv6":1', '"inbound":1', '"active_v4":true', '"active_v6":true', '"active_in":true'))
rsh("logread | grep netparam | tail -8")
print("ALL OK" if not fails else f"{fails} FAILED")
sys.exit(1 if fails else 0)
