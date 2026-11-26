#!/usr/bin/env python3
"""test_provision_ui.py - provisions a unit from factory state using ONLY the web UI (Chromium/Playwright),
the way a technician does it: Management VPN (CA, server, user, password) and Captive portal (all fields).

  python3 firmware-mods/lib/test_provision_ui.py provision.json

provision.json (keep it out of git, it has secrets):
{"vpn": {"ca_file": "...", "remote": "...", "port": "1194", "proto": "tcp", "username": "...", "password": "...",
         "server_cert": "1"},   # "0" only for a server certificate without the server key usage
 "portal": {"portal_url": "...", "nas_identifier": "...", "radius_server": "...", "radius_secret": "...",
            "login_host": "login.h3s-iot.com", "https_port": "8443", "cert_source": "agent",
            "agent_url": "...", "agent_token": "...", "walled_garden": "", "bypass_mac": ""}}
Screenshots in $OUT (default /tmp).
"""
import json, os, sys
from playwright.sync_api import sync_playwright

OUT = os.environ.get("OUT", "/tmp")
cfg = json.load(open(sys.argv[1]))
ok_all = True


def check(name, ok, detail=""):
    global ok_all
    ok_all &= bool(ok)
    print(("OK   " if ok else "FAIL ") + name + (f"  ({detail})" if detail and not ok else ""))


def dismiss(pg):
    for _ in range(2):
        for sel in ["text=Aceptar", "text=OK", "text=x", "text=X"]:
            loc = pg.locator(sel)
            for i in range(loc.count()):
                if loc.nth(i).is_visible():
                    try:
                        loc.nth(i).click(timeout=2000); pg.wait_for_timeout(700)
                    except Exception:
                        pass


def msgbox(pg):
    return pg.evaluate("() => document.body.innerText")


def open_item(pg, item):
    pg.get_by_text("Router", exact=True).first.click(); pg.wait_for_timeout(2500); dismiss(pg)
    pg.get_by_text("Configuraciones avanzadas", exact=True).first.click(); pg.wait_for_timeout(1500)
    pg.get_by_text(item, exact=True).first.click(); pg.wait_for_timeout(4000); dismiss(pg)


def wait_text(pg, sel, prefix, seconds):
    for _ in range(seconds // 5):
        t = pg.locator(sel).inner_text().strip()
        if t.startswith(prefix):
            return t
        pg.wait_for_timeout(5000)
    return pg.locator(sel).inner_text().strip()


with sync_playwright() as p:
    b = p.chromium.launch()
    pg = b.new_page(viewport={"width": 1280, "height": 1500})
    errors = []
    pg.on("pageerror", lambda e: errors.append(str(e)))
    pg.goto("http://192.168.1.1/", wait_until="networkidle")
    pg.fill("#tbarouter_username", os.environ.get("NOTION_HTTP_USER", "admin")); pg.fill("#tbarouter_password", os.environ.get("NOTION_HTTP_PASS", "admin")); pg.click("#btnSignIn")
    pg.wait_for_timeout(10000); dismiss(pg)

    # ---------------- Management VPN, from factory state
    v = cfg["vpn"]
    open_item(pg, "VPN de gestión")
    check("vpn: factory state = disabled, no server, no CA", not pg.locator("#tvpnEnable").is_checked()
          and pg.input_value("#tvpnRemote") == "" and pg.locator("#tvpnCaState").inner_text().startswith("no"))
    pg.check("#tvpnEnable")
    pg.fill("#tvpnRemote", v["remote"]); pg.fill("#tvpnPort", v["port"]); pg.select_option("#tvpnProto", v["proto"])
    pg.fill("#tvpnUser", v["username"]); pg.fill("#tvpnPass", v["password"])
    pg.click("#lt_btnApply"); pg.wait_for_timeout(3000)
    check("vpn: enabling without CA is refused with a clear message",
          "Primero instale el certificado del servidor (CA)" in msgbox(pg))
    dismiss(pg)
    pg.fill("#tvpnCaPem", open(v["ca_file"]).read())
    pg.click("#lt_TVPN_btnCa"); pg.wait_for_timeout(4000); dismiss(pg)
    check("vpn: CA installed from the page", pg.locator("#tvpnCaState").inner_text().strip() == "sí")
    pg.check("#tvpnEnable")
    pg.fill("#tvpnRemote", v["remote"]); pg.fill("#tvpnPort", v["port"]); pg.select_option("#tvpnProto", v["proto"])
    pg.fill("#tvpnUser", v["username"]); pg.fill("#tvpnPass", v["password"])
    if v.get("server_cert") == "0":  # only for a server whose certificate lacks the server key usage
        pg.uncheck("#tvpnServerCert")
    pg.click("#lt_btnApply"); pg.wait_for_timeout(4000); dismiss(pg)
    st = wait_text(pg, "#tvpnState", "Conectada", 120)
    check("vpn: connected after provisioning from the page", st.startswith("Conectada, IP "), st)
    pg.screenshot(path=f"{OUT}/prov_vpn.png", full_page=True)

    # ---------------- Captive portal, from factory state
    c = cfg["portal"]
    open_item(pg, "Portal cautivo")
    check("portal: factory state = disabled and empty", not pg.locator("#ocgwEnable").is_checked()
          and pg.input_value("#ocgwPortalUrl") == "" and pg.input_value("#ocgwNas") == ""
          and pg.input_value("#ocgwRadiusServer") == "" and pg.input_value("#ocgwLoginHost") == "")
    pg.check("#ocgwEnable")
    pg.fill("#ocgwPortalUrl", c["portal_url"]); pg.fill("#ocgwNas", c["nas_identifier"])
    pg.fill("#ocgwRadiusServer", c["radius_server"]); pg.fill("#ocgwSecret", c["radius_secret"])
    pg.check("#ocgwHttps")
    pg.fill("#ocgwLoginHost", c["login_host"]); pg.fill("#ocgwHttpsPort", c["https_port"])
    pg.select_option("#ocgwCertSource", c["cert_source"])
    if c["cert_source"] == "agent":
        pg.fill("#ocgwAgentUrl", c["agent_url"]); pg.fill("#ocgwAgentToken", c["agent_token"])
    pg.fill("#ocgwWalled", c.get("walled_garden", "")); pg.fill("#ocgwBypass", c.get("bypass_mac", ""))
    pg.click("#lt_btnApply"); pg.wait_for_timeout(5000)
    check("portal: saved", "correctamente" in msgbox(pg).lower() or "éxito" in msgbox(pg).lower() or "exitosa" in msgbox(pg).lower(),
          msgbox(pg)[-200:])
    dismiss(pg)
    st = wait_text(pg, "#ocgwState", "Funcionando", 60)
    check("portal: running with the secure login", st.startswith("Funcionando") and f"https://{c['login_host']}" in st, st)
    cert = pg.locator("#ocgwCert").inner_text().strip()
    check("portal: certificate installed", cert.startswith(c["login_host"]) and "válido hasta" in cert, cert)
    check("portal: secrets not shown after saving", pg.input_value("#ocgwSecret") == "" and pg.input_value("#ocgwAgentToken") == "")
    pg.screenshot(path=f"{OUT}/prov_portal.png", full_page=True)
    check("no JavaScript errors", not errors, errors[:3])
    b.close()

print("ALL OK" if ok_all else "SOME CHECKS FAILED")
sys.exit(0 if ok_all else 1)
