#!/usr/bin/env python3
"""web_login.py - log in to the device web UI and return the session id (CGISID cookie).

Used by test_cgi.py: /netparam.cgi accepts the same session as the rest of the web UI.
It reproduces doLogin() from /www/js/base/utils.js: HTTP Digest with realm/nonce/qop taken from the
challenge of /login.cgi, HA2 always computed over "GET:/cgi/xml_action.cgi". Success is reported in the
WWW-Authenticate header of the reply ("status=0,..."), not in the body.

Careful: several failed logins in a row lock the login for a while (status=5).
Env: NOTION_HTTP_HOST (default 192.168.1.1), NOTION_HTTP_USER / NOTION_HTTP_PASS (default admin / admin).
"""
import hashlib
import http.client
import os
import secrets

URI_FOR_DIGEST = "/cgi/xml_action.cgi"


def _md5(s):
    return hashlib.md5(s.encode()).hexdigest()


def login(host=None, user=None, password=None, timeout=10):
    host = host or os.environ.get("NOTION_HTTP_HOST", "192.168.1.1")
    user = user or os.environ.get("NOTION_HTTP_USER", "admin")
    password = password or os.environ.get("NOTION_HTTP_PASS", "admin")

    c = http.client.HTTPConnection(host, 80, timeout=timeout)
    c.request("POST", "/login.cgi", body=b"")
    r = c.getresponse(); challenge = r.getheader("WWW-Authenticate", ""); r.read()
    if not challenge.startswith("Digest"):
        raise RuntimeError(f"unexpected login.cgi reply (no Digest challenge): {challenge!r}")
    p = dict(kv.strip().split("=", 1) for kv in challenge.split(" ", 1)[1].split(","))
    realm, nonce, qop = (p[k].strip('"') for k in ("realm", "nonce", "qop"))

    nc, cnonce = "00000001", secrets.token_hex(8)
    ha1 = _md5(f"{user}:{realm}:{password}")
    ha2 = _md5(f"GET:{URI_FOR_DIGEST}")
    resp = _md5(f"{ha1}:{nonce}:{nc}:{cnonce}:{qop}:{ha2}")
    body = (f"Action=Digest&username={user}&realm={realm}&nonce={nonce}&response={resp}"
            f"&qop={qop}&cnonce={cnonce}&nc={nc}&temp=marvell")
    c = http.client.HTTPConnection(host, 80, timeout=timeout)
    c.request("POST", "/login.cgi", body=body, headers={"Content-Type": "application/x-www-form-urlencoded"})
    r = c.getresponse(); result = r.getheader("WWW-Authenticate", "")
    sid = next((v.split(";", 1)[0].split("=", 1)[1] for v in r.msg.get_all("Set-Cookie", [])
                if v.startswith("CGISID=")), None)
    r.read()
    status = result.split(",")[0].split("=")[-1] if result else None
    if status != "0" or not sid:
        raise RuntimeError(f"login rejected ({result!r}); repeated failures lock the login for a while")
    return sid


if __name__ == "__main__":
    print(login())
