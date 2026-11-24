#!/usr/bin/env python3
"""Web UI test of the "Network parameter" section (the TTL setting) of the Diagnostics page in Chromium (Playwright), driven the way a person would use it.

Usage (from the repo root):  NOTION_PASS=... python3 firmware-mods/netparam/test_ui.py [screenshot_dir]
Walks login -> Router -> System Settings -> Diagnostics (the section sits below ping/nslookup/traceroute), applies values, checks validation errors,
disables/enables, checks the English and Chinese strings, and verifies every change in the device's iptables.
The UI is in Spanish (device default), so the expected texts below are the Spanish UI strings.
Leaves the device at enabled=1, value=64, ipv6=1, UI language Spanish.
"""
import os, subprocess, sys
from playwright.sync_api import sync_playwright

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp"
fails = 0


def check(name, ok, detail=""):
    global fails
    fails += not ok
    print(f"{'OK  ' if ok else 'FAIL'} {name}" + (f"  [{detail}]" if detail else ""))


def rules():
    r = subprocess.run([os.path.join(ROOT, "scripts/rsh.sh"),
                        "iptables -t mangle -S POSTROUTING | grep netparam; ip6tables -t mangle -S POSTROUTING | grep netparam"],
                       capture_output=True, text=True, timeout=30)
    return r.stdout


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


def body(pg):
    return pg.evaluate("() => document.body.innerText")


def apply(pg, ttl=None):
    if ttl is not None:
        pg.fill("#txtNetParam", str(ttl))
    pg.click("#lt_btnApply"); pg.wait_for_timeout(2500)
    text = body(pg); dismiss(pg)
    return text


def open_ttl(pg, router, advanced, item):
    pg.get_by_text(router, exact=True).first.click(); pg.wait_for_timeout(2500); dismiss(pg)
    pg.get_by_text(advanced, exact=True).first.click(); pg.wait_for_timeout(1500)
    pg.get_by_text(item, exact=True).first.click(); pg.wait_for_timeout(2500); dismiss(pg)


with sync_playwright() as p:
    b = p.chromium.launch()
    pg = b.new_page(viewport={"width": 1300, "height": 900})
    errs = []
    pg.on("pageerror", lambda e: errs.append(str(e)[:200]))

    pg.goto("http://192.168.1.1/", wait_until="networkidle")
    pg.fill("#tbarouter_username", "admin"); pg.fill("#tbarouter_password", "admin"); pg.click("#btnSignIn")
    pg.wait_for_timeout(10000); dismiss(pg)
    check("login", pg.get_by_text("Router", exact=True).count() > 0)

    open_ttl(pg, "Router", "Configuración del sistema", "Diagnóstico")
    check("page opens from the menu", pg.locator("#lt_NetParam_title").inner_text() == "Parámetro de red")
    check("section is inside Diagnostics (ping form on the same page)", pg.locator("#diagnostics_addr").count() == 1
          and pg.locator("#NetParamSection #txtNetParam").count() == 1)
    check("no separate menu entry", pg.get_by_text("Parámetro de red", exact=True).count() == 1)
    check("no 'TTL'/'Hop' visible on screen (es)", not any(w in body(pg) for w in ("TTL", "Hop Limit")))
    check("loads current value", pg.input_value("#txtNetParam").isdigit(), pg.input_value("#txtNetParam"))
    pg.screenshot(path=f"{OUT}/ui_1_initial.png")

    t = apply(pg, 70)
    check("apply 70: success dialog", "¡Datos modificados con éxito!" in t)
    check("apply 70: status on page", "valor 70" in pg.locator("#NetParamStatus").inner_text())
    r = rules(); check("apply 70: iptables/ip6tables", "--ttl-set 70" in r and "--hl-set 70" in r)

    for bad in ["999", "0", "abc", "", "12a"]:
        t = apply(pg, bad)
        check(f"rejects '{bad}'", "El valor debe ser un número entre 1 y 255." in t)
    check("invalid value does not change the device", "--ttl-set 70" in rules())
    pg.fill("#txtNetParam", "70")   # the field still holds the last invalid value ("12a")

    pg.uncheck("#NetParamIpv6"); apply(pg)
    r = rules(); check("IPv6 unchecked: IPv4 rule only", "--ttl-set 70" in r and "--hl-set" not in r)
    pg.check("#NetParamIpv6"); apply(pg)
    check("IPv6 checked again", "--hl-set 70" in rules())

    pg.uncheck("#NetParamEnable")
    check("disabling locks the field", pg.is_disabled("#txtNetParam"))
    apply(pg)
    check("disabled: status", "Desactivado" in pg.locator("#NetParamStatus").inner_text())
    check("disabled: no rules", rules().strip() == "")
    pg.screenshot(path=f"{OUT}/ui_2_disabled.png")

    pg.check("#NetParamEnable"); apply(pg, 64)
    r = rules(); check("re-enable at 64", "--ttl-set 64" in r and "--hl-set 64" in r)

    # "Factory behavior" (remove) / "Recommended value" (restore) buttons: both ask Yes/No first ("No" must change nothing)
    pg.fill("#txtNetParam", "70"); apply(pg)
    pg.click("#lt_NetParam_btnRemove"); pg.wait_for_timeout(800)
    check("remove: asks for confirmation", "¿Dejar el equipo como de fábrica?" in body(pg))
    pg.locator("#mbd #lt_btnConfirmNo").click(); pg.wait_for_timeout(800)
    r = rules(); check("remove, answer No: nothing changes", "--ttl-set 70" in r)
    pg.click("#lt_NetParam_btnRemove"); pg.wait_for_timeout(800)
    pg.locator("#mbd #lt_btnConfirmYes").click(); pg.wait_for_timeout(2500)
    check("remove, answer Yes: success dialog", "¡Datos modificados con éxito!" in body(pg))
    dismiss(pg)
    check("remove: no rules", "netparam" not in rules())
    check("remove: page shows disabled", "Desactivado" in pg.locator("#NetParamStatus").inner_text()
          and not pg.is_checked("#NetParamEnable"))
    pg.click("#lt_NetParam_btnRestore"); pg.wait_for_timeout(800)
    check("restore: asks for confirmation", "¿Volver al valor recomendado" in body(pg))
    pg.locator("#mbd #lt_btnConfirmYes").click(); pg.wait_for_timeout(2500); dismiss(pg)
    r = rules(); check("restore: 64 on IPv4 and IPv6", "--ttl-set 64" in r and "--hl-set 64" in r)
    check("restore: page shows 64, enabled, IPv6", pg.input_value("#txtNetParam") == "64"
          and pg.is_checked("#NetParamEnable") and pg.is_checked("#NetParamIpv6"))

    pg.reload(wait_until="networkidle"); pg.wait_for_timeout(6000); dismiss(pg)
    open_ttl(pg, "Router", "Configuración del sistema", "Diagnóstico")
    check("after browser reload shows 64", pg.input_value("#txtNetParam") == "64")

    # Switch language with the top selector (this is also saved on the device; set back to "es" at the end).
    for lang, item, title, status in [("en", "Diagnostics", "Network parameter", "Active, value 64"),
                                      ("cn", "网络诊断", "网络参数", "已启用，参数值 64")]:
        pg.select_option("#selLang", value=lang); pg.wait_for_timeout(4000); dismiss(pg)
        got = pg.locator("#lt_NetParam_title").inner_text()
        check(f"language {lang}: title", got == title, got)
        check(f"language {lang}: menu", pg.get_by_text(item, exact=True).count() > 0)
        st = pg.locator("#NetParamStatus").inner_text()
        check(f"language {lang}: translated status", st.startswith(status), st)
        check(f"language {lang}: no 'TTL' visible", not any(w in body(pg) for w in ("TTL", "Hop Limit")))
        pg.screenshot(path=f"{OUT}/ui_3_{lang}.png")
    pg.select_option("#selLang", value="es"); pg.wait_for_timeout(3000); dismiss(pg)
    check("language restored to es", pg.locator("#lt_NetParam_title").inner_text() == "Parámetro de red")
    check("language es: status", pg.locator("#NetParamStatus").inner_text().startswith("Activo, valor 64"))

    check("no JavaScript errors", not errs, "; ".join(errs))
    b.close()

print("ALL OK" if not fails else f"{fails} FAILED")
sys.exit(1 if fails else 0)
