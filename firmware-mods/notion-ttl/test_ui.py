#!/usr/bin/env python3
"""Prueba de la página "TTL de salida" en Chromium (Playwright), como la usaría una persona.

Uso (desde la raíz del repo):  NOTION_PASS=... python3 firmware-mods/notion-ttl/test_ui.py [carpeta_capturas]
Recorre login -> Router -> Configuraciones avanzadas -> TTL de salida, aplica valores, valida errores,
desactiva/activa, revisa los textos en inglés y chino, y comprueba cada cambio en iptables del equipo.
Deja el equipo en enabled=1, ttl=64, ipv6=1.
"""
import os, subprocess, sys
from playwright.sync_api import sync_playwright

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp"
fails = 0


def check(name, ok, detail=""):
    global fails
    fails += not ok
    print(f"{'OK ' if ok else 'FALLA'} {name}" + (f"  [{detail}]" if detail else ""))


def rules():
    r = subprocess.run([os.path.join(ROOT, "scripts/rsh.sh"),
                        "iptables -t mangle -S POSTROUTING | grep notion-ttl; ip6tables -t mangle -S POSTROUTING | grep notion-ttl"],
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
        pg.fill("#txtNotionTTL", str(ttl))
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

    open_ttl(pg, "Router", "Configuraciones avanzadas", "TTL de salida")
    check("página abre desde el menú", pg.locator("#lt_NotionTTL_title").inner_text() == "TTL de salida (red móvil)")
    check("carga valor actual", pg.input_value("#txtNotionTTL").isdigit(), pg.input_value("#txtNotionTTL"))
    pg.screenshot(path=f"{OUT}/ui_1_inicial.png")

    t = apply(pg, 70)
    check("aplicar 70: diálogo de éxito", "¡Datos modificados con éxito!" in t)
    check("aplicar 70: estado en página", "TTL 70" in pg.locator("#NotionTTLStatus").inner_text())
    r = rules(); check("aplicar 70: iptables/ip6tables", "--ttl-set 70" in r and "--hl-set 70" in r)

    for bad in ["999", "0", "abc", "", "12a"]:
        t = apply(pg, bad)
        check(f"rechaza '{bad}'", "El TTL debe ser un número entre 1 y 255." in t)
    check("valor inválido no cambia el equipo", "--ttl-set 70" in rules())
    pg.fill("#txtNotionTTL", "70")   # el campo quedó con el último inválido ("12a")

    pg.uncheck("#NotionTTLIpv6"); apply(pg)
    r = rules(); check("IPv6 desmarcado: solo regla v4", "--ttl-set 70" in r and "--hl-set" not in r)
    pg.check("#NotionTTLIpv6"); apply(pg)
    check("IPv6 marcado de nuevo", "--hl-set 70" in rules())

    pg.uncheck("#NotionTTLEnable")
    check("al desactivar se bloquea el campo", pg.is_disabled("#txtNotionTTL"))
    apply(pg)
    check("desactivado: estado", "Desactivado" in pg.locator("#NotionTTLStatus").inner_text())
    check("desactivado: sin reglas", rules().strip() == "")
    pg.screenshot(path=f"{OUT}/ui_2_desactivado.png")

    pg.check("#NotionTTLEnable"); apply(pg, 64)
    r = rules(); check("reactivar en 64", "--ttl-set 64" in r and "--hl-set 64" in r)

    pg.reload(wait_until="networkidle"); pg.wait_for_timeout(6000); dismiss(pg)
    open_ttl(pg, "Router", "Configuraciones avanzadas", "TTL de salida")
    check("tras recargar el navegador muestra 64", pg.input_value("#txtNotionTTL") == "64")

    # Cambiar idioma con el selector de arriba (también lo guarda en el equipo; al final se vuelve a "es").
    for lang, item, title, status in [("en", "Outbound TTL", "Outbound TTL (mobile network)", "Active, TTL 64"),
                                      ("cn", "出口TTL", "出口TTL（移动网络）", "已启用，TTL 64")]:
        pg.select_option("#selLang", value=lang); pg.wait_for_timeout(4000); dismiss(pg)
        got = pg.locator("#lt_NotionTTL_title").inner_text()
        check(f"idioma {lang}: título", got == title, got)
        check(f"idioma {lang}: menú", pg.get_by_text(item, exact=True).count() > 0)
        st = pg.locator("#NotionTTLStatus").inner_text()
        check(f"idioma {lang}: estado traducido", st.startswith(status), st)
        pg.screenshot(path=f"{OUT}/ui_3_{lang}.png")
    pg.select_option("#selLang", value="es"); pg.wait_for_timeout(3000); dismiss(pg)
    check("idioma restaurado a es", pg.locator("#lt_NotionTTL_title").inner_text() == "TTL de salida (red móvil)")
    check("idioma es: estado", pg.locator("#NotionTTLStatus").inner_text().startswith("Activo, TTL 64"))

    check("sin errores de JavaScript", not errs, "; ".join(errs))
    b.close()

print("TODO OK" if not fails else f"{fails} FALLAS")
sys.exit(1 if fails else 0)
