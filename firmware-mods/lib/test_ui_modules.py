#!/usr/bin/env python3
"""test_ui_modules.py - drives the device web UI (Chromium/Playwright) through the two new pages:
Router > Advanced Settings > Captive portal and > Management VPN. Expects both installed and configured.
Screenshots in $OUT (default /tmp). Leaves the UI in Spanish.

  python3 firmware-mods/lib/test_ui_modules.py [--kick-mac 6c:1f:f7:6f:a3:b5]
"""
import os, sys, re, subprocess
from playwright.sync_api import sync_playwright

RSH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "scripts", "rsh.sh")
def rsh(cmd):
    return subprocess.run([RSH, cmd], capture_output=True, text=True, timeout=60).stdout

OUT = os.environ.get("OUT", "/tmp")
ok_all = True
KICK = sys.argv[2].lower() if len(sys.argv) == 3 and sys.argv[1] == "--kick-mac" else None


def check(name, ok, detail=""):
    global ok_all
    ok_all &= bool(ok)
    print(("OK   " if ok else "FAIL ") + name + (f"  ({detail})" if detail and not ok else ""))


def dismiss(pg):
    for _ in range(2):
        for sel in ["text=Aceptar", "text=OK", "text=确定", "text=x", "text=X"]:
            loc = pg.locator(sel)
            for i in range(loc.count()):
                if loc.nth(i).is_visible():
                    try:
                        loc.nth(i).click(timeout=2000); pg.wait_for_timeout(700)
                    except Exception:
                        pass


def open_item(pg, advanced, item, router="Router"):
    pg.get_by_text(router, exact=True).first.click(); pg.wait_for_timeout(2500); dismiss(pg)
    pg.get_by_text(advanced, exact=True).first.click(); pg.wait_for_timeout(1500)
    pg.get_by_text(item, exact=True).first.click(); pg.wait_for_timeout(4000); dismiss(pg)


def text(pg, sel):
    return pg.locator(sel).inner_text().strip()


with sync_playwright() as p:
    b = p.chromium.launch()
    pg = b.new_page(viewport={"width": 1280, "height": 1400})
    errors = []
    pg.on("pageerror", lambda e: errors.append(str(e)))
    pg.goto("http://192.168.1.1/", wait_until="networkidle")
    pg.fill("#tbarouter_username", os.environ.get("NOTION_HTTP_USER", "admin")); pg.fill("#tbarouter_password", os.environ.get("NOTION_HTTP_PASS", "admin")); pg.click("#btnSignIn")
    pg.wait_for_timeout(10000); dismiss(pg)

    # ---------------- captive portal page
    open_item(pg, "Configuraciones avanzadas", "Portal cautivo")
    check("portal: title", text(pg, "#lt_OCGW_title") == "Portal cautivo (omnicap)", text(pg, "#lt_OCGW_title"))
    check("portal: enabled checkbox", pg.locator("#ocgwEnable").is_checked())
    check("portal: NAS-Identifier loaded", re.match(r"^[A-Za-z0-9_.:-]{1,64}$", pg.input_value("#ocgwNas")) is not None, pg.input_value("#ocgwNas"))
    check("portal: secret not shown, marked saved", pg.input_value("#ocgwSecret") == ""
          and pg.get_attribute("#ocgwSecret", "placeholder") == "guardado")
    pg.wait_for_timeout(6000)
    st = text(pg, "#ocgwState")
    check("portal: status running with https login", st.startswith("Funcionando") and "https://login.h3s-iot.com:8443/login" in st, st)
    cert = text(pg, "#ocgwCert")
    check("portal: certificate shown", cert.startswith("login.h3s-iot.com") and "válido hasta" in cert, cert)
    rows = [r for r in pg.evaluate("() => { var t = ''; var rs = document.querySelectorAll('#ocgwClients tr'); for (var i = 0; i < rs.length; i++) { t += rs[i].innerText + '\\n'; } return t; }").split("\n") if r.strip()]
    check("portal: clients table rendered", len(rows) >= 1, rows)
    pg.screenshot(path=f"{OUT}/ui_portal_es.png", full_page=True)

    orig = pg.input_value("#ocgwPortalUrl")
    pg.fill("#ocgwPortalUrl", "javascript:alert(1)"); pg.click("#lt_btnApply"); pg.wait_for_timeout(1500)
    check("portal: invalid URL rejected in the page", "La URL del portal no es válida." in pg.evaluate("() => document.body.innerText"))
    dismiss(pg); pg.fill("#ocgwPortalUrl", orig)

    boxes = lambda: pg.evaluate("() => Array.prototype.map.call(document.querySelectorAll('.ocgwPort'), function(e){ return e.value + (e.checked ? '+' : '-'); }).join(',')")
    check("portal: ports label", text(pg, "#lt_OCGW_portsLbl") == "Aplicar el portal en:")
    b0 = boxes()
    check("portal: ports read from the device, all captured", all(x in b0 for x in ["wlan0+", "wlan1+", "wlan0.1+", "wlan1.1+", "eth0.2+"]), b0)
    ptxt = text(pg, "#ocgwPorts")
    check("portal: guest network listed (off)", "red de invitados" in ptxt and "apagada" in ptxt, ptxt)
    pg.evaluate("() => { var c = document.querySelectorAll('.ocgwPort'); for (var i = 0; i < c.length; i++) { c[i].checked = false; } }")
    pg.click("#lt_btnApply"); pg.wait_for_timeout(1500)
    check("portal: no port rejected in the page", "Marque al menos un medio" in pg.evaluate("() => document.body.innerText"))
    dismiss(pg)
    pg.evaluate("() => { var c = document.querySelectorAll('.ocgwPort'); for (var i = 0; i < c.length; i++) { c[i].checked = c[i].value != 'eth0.2'; } }")
    pg.click("#lt_btnApply"); pg.wait_for_timeout(9000); dismiss(pg)
    eb = rsh("ebtables -t nat -L OCGW_PORTS 2>/dev/null; uci -q get omnicapgw.main.exclude_if")
    check("portal: cable excluded -> ebtables marks eth0.2 only", "-i eth0.2 " in eb and "wlan" not in eb, eb)
    pg.evaluate("() => { var c = document.querySelectorAll('.ocgwPort'); for (var i = 0; i < c.length; i++) { c[i].checked = true; } }")
    pg.click("#lt_btnApply"); pg.wait_for_timeout(9000); dismiss(pg)
    eb = rsh("ebtables -t nat -L PREROUTING; uci -q get omnicapgw.main.exclude_if || echo NONE")
    check("portal: all ports again -> no ebtables rules, nothing excluded", "OCGW" not in eb and "NONE" in eb, eb)

    if KICK:
        q = f"#ocgwClients input[data-mac='{KICK}']"
        n = int(pg.evaluate(f"() => String(document.querySelectorAll(\"{q}\").length)"))
        check("portal: kick button for the client", n == 1, n)
        if n:
            pg.evaluate(f"() => document.querySelector(\"{q}\").click()"); pg.wait_for_timeout(4000)
            n2 = int(pg.evaluate(f"() => String(document.querySelectorAll(\"{q}\").length)"))
            check("portal: client disconnected from the page", n2 == 0, n2)

    # ---------------- management VPN page
    open_item(pg, "Configuraciones avanzadas", "VPN de gestión")
    check("vpn: title", text(pg, "#lt_TVPN_title") == "VPN de gestión (OpenVPN)", text(pg, "#lt_TVPN_title"))
    check("vpn: server loaded", pg.input_value("#tvpnRemote") == "190.145.155.2")
    check("vpn: note does not name H3S", "H3S" not in text(pg, "#lt_TVPN_note"), text(pg, "#lt_TVPN_note"))
    check("vpn: no-cipher option", text(pg, "#lt_TVPN_optNoCipher") == "Ninguno (sin cifrado)")
    check("vpn: no-auth option", text(pg, "#lt_TVPN_optNoAuth") == "Ninguna")
    check("vpn: password not shown, marked saved", pg.input_value("#tvpnPass") == ""
          and pg.get_attribute("#tvpnPass", "placeholder") == "guardada")
    pg.wait_for_timeout(6000)
    st = text(pg, "#tvpnState")
    check("vpn: connected with IP", st.startswith("Conectada, IP 10.77.88."), st)
    check("vpn: CA installed", text(pg, "#tvpnCaState") == "sí")
    check("vpn: log shown", "Initialization Sequence Completed" in text(pg, "#tvpnLog") or len(text(pg, "#tvpnLog")) > 0)
    pg.screenshot(path=f"{OUT}/ui_vpn_es.png", full_page=True)
    pg.click("#lt_TVPN_btnReconnect"); pg.wait_for_timeout(4000); dismiss(pg)
    back = False
    for _ in range(18):
        pg.wait_for_timeout(5000)
        if text(pg, "#tvpnState").startswith("Conectada"):
            back = True; break
    check("vpn: reconnect button -> connected again", back, text(pg, "#tvpnState"))

    # ---------------- languages
    for lang, router, adv, item, title in [("en", "Router", "Advance Settings", "Captive portal", "Captive portal (omnicap)"),
                                           ("cn", "路由器", "高级设置", "认证门户", "认证门户 (omnicap)")]:
        pg.select_option("#selLang", value=lang); pg.wait_for_timeout(4000); dismiss(pg)
        try:
            open_item(pg, adv, item, router)
            got = text(pg, "#lt_OCGW_title")
        except Exception as e:
            got = str(e)[:80]
        check(f"language {lang}: portal page title", got == title, got)
        pg.screenshot(path=f"{OUT}/ui_portal_{lang}.png", full_page=True)
    pg.select_option("#selLang", value="es"); pg.wait_for_timeout(3000)
    check("no JavaScript errors", not errors, errors[:3])
    b.close()

print("ALL OK" if ok_all else "SOME CHECKS FAILED")
sys.exit(0 if ok_all else 1)
