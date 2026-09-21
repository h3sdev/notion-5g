#!/usr/bin/env python3
"""
modem_http_api.py — cliente del API HTTP nativo del router Notion 5G (H3S P52HL),
sin pasar por SSH. Sirve de referencia para portar la misma lógica a la futura
app Android/Flutter (que no puede usar dropbear con ssh-rsa/DH group1 fácilmente,
pero sí puede hacer HTTP normal).

Ingeniería inversa a partir de /www/js/base/utils.js y /www/js/base/ajax_calls.js
del propio equipo (JS servido públicamente por su interfaz web), más una captura
de red real del navegador. Resumen del protocolo:

  - Los endpoints "REST" (`json_status_info.cgi`, `json_wan_info.cgi`, etc.) están
    MUERTOS: siempre devuelven {"result":"fail"} sin importar la autenticación.
    El API real y vivo es XML sobre POST /xml_action.cgi?method=set.

  - Autenticación: HTTP Digest "casero", con una particularidad importante:
    el server siempre anuncia el mismo nonce ("1000") y realm ("Highwmg") —
    no rota por sesión — así que una vez se conoce la contraseña, se puede
    calcular una respuesta Digest válida para cualquier request sin más
    negociación.
      HA1 = MD5(usuario:realm:password)
      HA2 = MD5(<METODO_HTTP>:/cgi/xml_action.cgi)   (URI SIEMPRE fija, aunque
            la URL real tenga querystring; y el HA2 de login.cgi usa "GET" fijo
            mientras que el de xml_action.cgi usa "POST")
      response = MD5(HA1:nonce:nc:cnonce:qop:HA2)

  - login.cgi: hay que loguearse una vez para obtener la cookie de sesión
    CGISID (el server valida el `response` del login por separado del que
    luego se manda en cada llamada a xml_action.cgi). El éxito NO se ve en el
    cuerpo (que viene vacío) sino en el header `WWW-Authenticate` de la
    respuesta: "status=0,..." = éxito; "status=5,left_time=N" = credenciales
    inválidas Y bloqueo temporal tras varios intentos fallidos (cuidado con
    reintentar credenciales al aire, el equipo banea por tiempo).

  - xml_action.cgi: cada llamada manda un Authorization Digest fresco (nc y
    cnonce nuevos) + la cookie CGISID + un body XML:
      <RGW><param><method>call</method><session>000</session>
      <obj_path>...</obj_path><obj_method>...</obj_method></param></RGW>
    obj_path/obj_method son los MISMOS nombres que ubus usa localmente en el
    equipo (p. ej. cm/get_zcainfo, version/get_cesq, sim/get_double_sim_status):
    la web es un envoltorio HTTP sobre los mismos objetos ubus que ya leemos
    por SSH en notion5g.py.

Uso:
    from modem_http_api import ModemHTTP
    m = ModemHTTP("192.168.1.1", "admin", "admin")
    m.login()
    print(m.call("cm", "get_zcainfo"))
"""
import hashlib
import http.client
import secrets
import xml.etree.ElementTree as ET

URI_FOR_DIGEST = "/cgi/xml_action.cgi"  # fijo: el firmware lo usa así en TODAS las llamadas


def _md5(s: str) -> str:
    return hashlib.md5(s.encode()).hexdigest()


class ModemHTTPError(RuntimeError):
    pass


class ModemHTTP:
    def __init__(self, host="192.168.1.1", username="admin", password="admin", timeout=10):
        self.host = host
        self.username = username
        self.password = password
        self.timeout = timeout
        self.realm = self.nonce = self.qop = None
        self.cgisid = None
        self._nc = 0

    def _conn(self):
        return http.client.HTTPConnection(self.host, 80, timeout=self.timeout)

    def _digest(self, method: str, nc: str, cnonce: str) -> str:
        ha1 = _md5(f"{self.username}:{self.realm}:{self.password}")
        ha2 = _md5(f"{method}:{URI_FOR_DIGEST}")
        return _md5(f"{ha1}:{self.nonce}:{nc}:{cnonce}:{self.qop}:{ha2}")

    def _auth_header(self, method: str) -> str:
        self._nc += 1
        nc = f"{self._nc:08d}"
        cnonce = secrets.token_hex(8)
        resp = self._digest(method, nc, cnonce)
        return (f'Digest username="{self.username}", realm="{self.realm}", nonce="{self.nonce}", '
                f'uri="{URI_FOR_DIGEST}", response="{resp}", qop={self.qop}, nc={nc}, cnonce="{cnonce}"')

    def login(self):
        """Obtiene realm/nonce/qop, hace login y guarda la cookie de sesión (CGISID)."""
        conn = self._conn()
        conn.request("POST", "/login.cgi", body=b"")
        r = conn.getresponse()
        www_auth = r.getheader("WWW-Authenticate", "")
        r.read()
        if not www_auth.startswith("Digest"):
            raise ModemHTTPError(f"respuesta inesperada de login.cgi (sin challenge Digest): {www_auth!r}")
        parts = dict(kv.strip().split("=", 1) for kv in www_auth.split(" ", 1)[1].split(","))
        self.realm = parts["realm"].strip('"')
        self.nonce = parts["nonce"].strip('"')
        self.qop = parts["qop"].strip('"')

        nc, cnonce = "00000001", secrets.token_hex(8)
        resp = self._digest("GET", nc, cnonce)  # doLogin() usa HA2 fijo con "GET", aunque el POST real sea distinto
        body = (f"Action=Digest&username={self.username}&realm={self.realm}&nonce={self.nonce}"
                f"&response={resp}&qop={self.qop}&cnonce={cnonce}&nc={nc}&temp=marvell")
        conn2 = self._conn()
        conn2.request("POST", "/login.cgi", body=body,
                       headers={"Content-Type": "application/x-www-form-urlencoded"})
        r2 = conn2.getresponse()
        login_result = r2.getheader("WWW-Authenticate", "")
        cgisid = None
        for raw in r2.msg.get_all("Set-Cookie", []):
            if raw.startswith("CGISID="):
                cgisid = raw.split(";", 1)[0].split("=", 1)[1]
        r2.read()

        status = login_result.split(",")[0].split("=")[-1] if login_result else None
        if status != "0":
            if status == "5":
                left = login_result.split(",")[1].split("=")[-1] if "," in login_result else "?"
                raise ModemHTTPError(f"login rechazado (credenciales inválidas); intentos restantes: {left}. "
                                      "OJO: varios intentos fallidos seguidos pueden bloquear el login por un rato.")
            raise ModemHTTPError(f"login rechazado, status={status!r} ({login_result!r})")
        if not cgisid:
            raise ModemHTTPError("login reportó éxito pero no llegó cookie CGISID")
        self.cgisid = cgisid
        self._nc = 0  # las llamadas a xml_action.cgi llevan su propio contador
        return True

    def call(self, obj_path: str, obj_method: str, extra_xml: str = "") -> dict:
        """Llama xml_action.cgi obj_path/obj_method (equivalentes a `ubus call obj_path obj_method`)
        y devuelve el XML de respuesta ya parseado a dict anidado (listas cuando hay <Item> repetidos)."""
        if not self.cgisid:
            raise ModemHTTPError("hay que llamar login() primero")
        xml_body = ('<?xml version="1.0" encoding="US-ASCII"?>'
                    f'<RGW><param><method>call</method><session>000</session>'
                    f'<obj_path>{obj_path}</obj_path><obj_method>{obj_method}</obj_method>{extra_xml}</param></RGW>')
        conn = self._conn()
        conn.request("POST", "/xml_action.cgi?method=set", body=xml_body, headers={
            "Authorization": self._auth_header("POST"),
            "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
            "X-Requested-With": "XMLHttpRequest",
            "csrftoken": "hfiehifejfklihefiuehflejhfueihfeuihfeui",  # constante fija del propio JS del equipo
            "Cookie": f"CGISID={self.cgisid}; Path=/",
        })
        r = conn.getresponse()
        raw = r.read().decode(errors="replace")
        try:
            root = ET.fromstring(raw)
        except ET.ParseError as e:
            raise ModemHTTPError(f"XML inválido de {obj_path}/{obj_method}: {e}: {raw[:200]!r}")
        err = root.findtext("error_cause")
        if err == "5":
            raise ModemHTTPError("sesión/credenciales rechazadas (error_cause=5); vuelve a llamar login()")
        if err and err != "0":
            raise ModemHTTPError(f"{obj_path}/{obj_method} -> error_cause={err}")
        return _xml_to_dict(root)


def _xml_to_dict(el: ET.Element):
    children = list(el)
    if not children:
        return el.text
    out: dict = {}
    for child in children:
        val = _xml_to_dict(child)
        if child.tag in out:
            if not isinstance(out[child.tag], list):
                out[child.tag] = [out[child.tag]]
            out[child.tag].append(val)
        else:
            out[child.tag] = val
    return out


if __name__ == "__main__":
    import argparse
    import json
    import os

    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("obj_path")
    ap.add_argument("obj_method")
    ap.add_argument("--host", default=os.environ.get("NOTION_HTTP_HOST", "192.168.1.1"))
    ap.add_argument("--user", default=os.environ.get("NOTION_HTTP_USER", "admin"))
    ap.add_argument("--password", default=os.environ.get("NOTION_HTTP_PASS", "admin"))
    a = ap.parse_args()

    m = ModemHTTP(a.host, a.user, a.password)
    m.login()
    print(json.dumps(m.call(a.obj_path, a.obj_method), indent=2, ensure_ascii=False))
