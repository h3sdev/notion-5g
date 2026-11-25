#!/usr/bin/env python3
"""test_cgi.py - checks /omnicapgw.cgi on a unit and (optionally) saves a configuration through it.

  python3 firmware-mods/omnicap-gw/tools/test_cgi.py                 # security + validation checks only
  python3 firmware-mods/omnicap-gw/tools/test_cgi.py --apply cfg.json # then save cfg.json through the CGI

cfg.json keys = the form fields (portal_url, nas_identifier, radius_server, radius_secret, https, login_host,
https_port, cert_source, agent_url, agent_token, walled_garden, bypass_mac, ...). Secrets go only to the device.
Env: NOTION_HTTP_HOST / NOTION_HTTP_USER / NOTION_HTTP_PASS (see netparam/tools/web_login.py).
"""
import http.client, json, os, sys, urllib.parse
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '../../netparam/tools'))
from web_login import login

HOST = os.environ.get("NOTION_HTTP_HOST", "192.168.1.1")
CGI = "/omnicapgw.cgi"
ok_all = True


def call(method, body=None, sid=None, xrw=True, timeout=90):
    c = http.client.HTTPConnection(HOST, 80, timeout=timeout)
    h = {"Content-Type": "application/x-www-form-urlencoded"}
    if sid:
        h["Cookie"] = "CGISID=" + sid
    if xrw:
        h["X-Requested-With"] = "XMLHttpRequest"
    data = urllib.parse.urlencode(body).encode() if body is not None else None
    c.request(method, CGI, body=data, headers=h)
    r = c.getresponse()
    raw = r.read().decode(errors="replace")
    try:
        return r.status, json.loads(raw)
    except ValueError:
        return r.status, raw


def check(name, cond, extra=""):
    global ok_all
    ok_all &= bool(cond)
    print(("OK   " if cond else "FAIL ") + name + (f"  ({extra})" if extra and not cond else ""))


BASE = {"enabled": "1", "portal_url": "http://192.0.2.10/portal/X.Y", "nas_identifier": "TEST_NAS",
        "radius_server": "192.0.2.10", "radius_auth_port": "1812", "radius_acct_port": "1813",
        "radius_secret": "testsecret123", "https": "1", "login_host": "login.h3s-iot.com", "https_port": "8443",
        "cert_source": "manual", "agent_url": "", "agent_token": "", "walled_garden": "", "bypass_mac": ""}

sid = login()
st, _ = call("GET")
check("GET without session -> 401", st == 401, st)
st, _ = call("POST", {"action": "fetchcert"}, sid, xrw=False)
check("POST without X-Requested-With -> 403", st == 403, st)
st, d = call("GET", sid=sid)
check("GET with session -> ok", st == 200 and d.get("result") == "ok", st)
check("GET never returns the RADIUS secret", "radius_secret" not in d, list(d))
check("GET never returns the agent token", "agent_token" not in d, list(d))
for field, bad in [("portal_url", "javascript:alert(1)"), ("nas_identifier", "a b;rm"), ("radius_server", "x;reboot"),
                   ("radius_auth_port", "70000"), ("https_port", "443"), ("radius_secret", "short"),
                   ("walled_garden", "a.com,$(id)"), ("bypass_mac", "11:22"), ("cert_source", "x"), ("enabled", "2"),
                   ("nas_identifier", "NAS1\nx"),  # a newline must not pass (grep matched any one line)
                   ("agent_url", "http://cautivo.h3s-iot.com"),  # the agent carries secrets: https only
                   ("exclude_if", "eth9;reboot")]:
    body = dict(BASE, **{field: bad})
    st, r = call("POST", body, sid)
    check(f"rejects bad {field}", st == 400 and isinstance(r, dict) and r.get("error") == field, f"{st} {r}")
st, r = call("POST", {"action": "register", "install_url": "http://cautivo.h3s-iot.com/agent/v1/install/" + "a" * 32,
                      "identity": "X1"}, sid)
check("rejects plain-http install URL", st == 400 and r.get("error") == "install_url", f"{st} {r}")
st, r = call("POST", {"action": "kick", "mac": "11;reboot"}, sid)
check("rejects bad kick mac", st == 400, st)
st, r = call("POST", {"action": "cert", "cert_pem": "nope", "key_pem": "nope"}, sid)
check("rejects non-PEM certificate", st == 400 and r.get("error") == "cert", r)

if len(sys.argv) == 3 and sys.argv[1] == "--apply":
    cfg = dict(BASE, **json.load(open(sys.argv[2])))
    st, r = call("POST", cfg, sid)
    check("save configuration", st == 200 and r.get("result") == "ok", f"{st} {r}")
    if isinstance(r, dict):
        s = r.get("status", {})
        print(json.dumps({k: s.get(k) for k in ("running", "https_active", "login_url", "radius_error", "cert")}, indent=1))
print("ALL OK" if ok_all else "SOME CHECKS FAILED")
sys.exit(0 if ok_all else 1)
