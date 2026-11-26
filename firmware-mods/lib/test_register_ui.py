#!/usr/bin/env python3
"""test_register_ui.py - end-to-end provisioning of the captive portal through the two web UIs only:
1) omnicap panel: log in, open the location, create an install key, read the install URL;
2) modem web UI (Router > Advanced Settings > Captive portal, factory state): paste the URL and the identity,
   "Registrar en omnicap" -> the page shows the portal running with the secure login and the certificate.

  python3 firmware-mods/lib/test_register_ui.py register.json
register.json: {"panel": "https://omnicap.h3s-iot.com", "user": "...", "password": "...",
                "location": "<name shown in the panel>", "identity": "NOTION5G_0001",
                "exclude_if": ["eth0.2"]}   # ports left out of the portal (this PC's), set before registering
"""
import json, os, re, sys
from playwright.sync_api import sync_playwright

cfg = json.load(open(sys.argv[1]))
OUT = os.environ.get("OUT", "/tmp")
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


with sync_playwright() as p:
    b = p.chromium.launch()
    # ---------------- 1) omnicap panel
    pg = b.new_page(viewport={"width": 1280, "height": 1200})
    pg.goto(cfg["panel"] + "/login", wait_until="networkidle")
    pg.fill("input[name=username]", cfg["user"]); pg.fill("input[name=password]", cfg["password"])
    with pg.expect_navigation():
        pg.get_by_text("INGRESAR", exact=True).click()
    pg.wait_for_load_state("networkidle")
    check("panel: logged in", "/login" not in pg.url, pg.url)
    pg.goto(cfg["panel"] + "/app/locations", wait_until="networkidle")
    href = pg.evaluate("""(name) => { var rows = document.querySelectorAll('tr');
        for (var i = 0; i < rows.length; i++) { if (rows[i].innerText.indexOf(name) === 0) {
            var a = rows[i].querySelector("a[href^='/app/locations/']"); if (a) return a.getAttribute('href'); } }
        return ''; }""", cfg["location"])
    m = re.search(r"/app/locations/([0-9a-f-]{36})", href or "")
    check("panel: location opened", m is not None, pg.url)
    pg.goto(cfg["panel"] + f"/app/locations/{m.group(1)}/install", wait_until="networkidle")
    with pg.expect_navigation():
        pg.locator("form[action$='/install/keys'] button, form[action$='/install/keys'] input[type=submit]").first.click()
    pg.wait_for_load_state("networkidle")
    body = pg.inner_text("body")
    key = re.search(r"\b([0-9a-f]{64})\b", body)
    base = re.search(r'url="(https?://[^/"]+)/agent/v1/install/', body)
    found = [base.group(1) + "/agent/v1/install/" + key.group(1)] if key and base else []
    check("panel: install key created, URL shown", len(found) > 0)
    pg.screenshot(path=f"{OUT}/reg_panel.png", full_page=True)
    install_url = found[0] if found else ""

    # ---------------- 2) modem web UI
    pg = b.new_page(viewport={"width": 1280, "height": 1600})
    errors = []
    pg.on("pageerror", lambda e: errors.append(str(e)))
    pg.goto("http://192.168.1.1/", wait_until="networkidle")
    pg.fill("#tbarouter_username", os.environ.get("NOTION_HTTP_USER", "admin")); pg.fill("#tbarouter_password", os.environ.get("NOTION_HTTP_PASS", "admin")); pg.click("#btnSignIn")
    pg.wait_for_timeout(10000); dismiss(pg)
    pg.get_by_text("Router", exact=True).first.click(); pg.wait_for_timeout(2500); dismiss(pg)
    pg.get_by_text("Configuraciones avanzadas", exact=True).first.click(); pg.wait_for_timeout(1500)
    pg.get_by_text("Portal cautivo", exact=True).first.click(); pg.wait_for_timeout(4000); dismiss(pg)
    check("modem: portal in factory state", not pg.locator("#ocgwEnable").is_checked() and pg.input_value("#ocgwNas") == "")
    if cfg.get("exclude_if"):  # registering turns the portal on: first leave this PC's port out (portal still off)
        for x in cfg["exclude_if"]:
            pg.locator(f".ocgwPort[value='{x}']").uncheck()
        pg.click("#lt_btnApply"); pg.wait_for_timeout(5000); dismiss(pg)
    pg.fill("#ocgwInstallUrl", install_url); pg.fill("#ocgwIdentity", cfg["identity"])
    pg.click("#lt_OCGW_btnRegister"); pg.wait_for_timeout(12000)
    text = pg.evaluate("() => document.body.innerText")
    check("modem: registered message", "Equipo registrado y portal activado como " + cfg["identity"] in text, text[-300:])
    dismiss(pg)
    state = ""
    for _ in range(12):
        state = pg.locator("#ocgwState").inner_text().strip()
        if state.startswith("Funcionando") and "https://" in state:
            break
        pg.wait_for_timeout(5000)
    check("modem: portal running with secure login", state.startswith("Funcionando") and "https://" in state, state)
    check("modem: NAS identifier filled by omnicap", pg.input_value("#ocgwNas") == cfg["identity"], pg.input_value("#ocgwNas"))
    check("modem: portal URL filled by omnicap", "/portal/" in pg.input_value("#ocgwPortalUrl"))
    check("modem: secret stored, not shown", pg.input_value("#ocgwSecret") == "" and pg.get_attribute("#ocgwSecret", "placeholder") == "guardado")
    cert = pg.locator("#ocgwCert").inner_text().strip()
    check("modem: certificate downloaded from omnicap", "válido hasta" in cert, cert)
    pg.screenshot(path=f"{OUT}/reg_modem.png", full_page=True)
    check("no JavaScript errors", not errors, errors[:3])
    b.close()

print("ALL OK" if ok_all else "SOME CHECKS FAILED")
sys.exit(0 if ok_all else 1)
