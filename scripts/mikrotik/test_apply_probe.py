#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Pruebas sin MikroTik de apply_probe.py:  python -m unittest test_apply_probe -v

- codificador/decodificador de la API clásica con un socket falso;
- plan + aplicación contra un RouterOS falso en memoria sembrado con el estado de fábrica
  real del hAP (leído el 2026-09-29): tras aplicar, el plan debe quedar vacío (idempotencia);
- el .rsc generado: llaves balanceadas y comentarios que busca el celular.
"""

import argparse
import copy
import io
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import apply_probe as ap  # noqa: E402


# ------------------------------------------------------------------------------ socket falso

class FakeSocket:
    def __init__(self, incoming=b""):
        self.inbuf = io.BytesIO(incoming)
        self.sent = b""

    def sendall(self, b):
        self.sent += b

    def recv(self, n):
        return self.inbuf.read(n)

    def close(self):
        pass


def sentence(*words):
    return ap.encode_sentence(list(words))


class ProtocolTests(unittest.TestCase):
    def test_length_roundtrip_at_boundaries(self):
        for n in (0, 1, 0x7F, 0x80, 0x3FFF, 0x4000, 0x1FFFFF, 0x200000, 0xFFFFFFF, 0x10000000, 0xFFFFFFFF):
            enc = ap.encode_length(n)
            buf = io.BytesIO(enc)
            self.assertEqual(ap.decode_length(buf.read), n, hex(n))
            self.assertEqual(buf.read(), b"", "sobran bytes para %x" % n)

    def test_length_sizes(self):
        self.assertEqual(len(ap.encode_length(0x7F)), 1)
        self.assertEqual(len(ap.encode_length(0x80)), 2)
        self.assertEqual(ap.encode_length(0x80), b"\x80\x80")
        self.assertEqual(len(ap.encode_length(0x4000)), 3)
        self.assertEqual(len(ap.encode_length(0x200000)), 4)
        self.assertEqual(ap.encode_length(0x10000000)[:1], b"\xF0")
        self.assertEqual(len(ap.encode_length(0x10000000)), 5)

    def test_sentence_encoding(self):
        self.assertEqual(ap.encode_sentence(["/login", "=name=a"]), b"\x06/login\x07=name=a\x00")

    def test_talk_rows_and_done(self):
        incoming = (sentence("!re", "=.id=*1", "=name=to-A", "=fib=")
                    + sentence("!re", "=.id=*2", "=comment=a=b")
                    + sentence("!done", "=ret=*9"))
        s = FakeSocket(incoming)
        api = ap.Api(s)
        rows = api.talk(["/routing/table/print"])
        self.assertEqual(rows, [{".id": "*1", "name": "to-A", "fib": ""}, {".id": "*2", "comment": "a=b"}])
        self.assertEqual(api.done, {"ret": "*9"})
        self.assertEqual(s.sent, ap.encode_sentence(["/routing/table/print"]))

    def test_trap_then_done_raises_and_session_continues(self):
        incoming = (sentence("!trap", "=message=unknown parameter routing-table") + sentence("!done")
                    + sentence("!re", "=a=1") + sentence("!done"))
        api = ap.Api(FakeSocket(incoming))
        with self.assertRaises(ap.ApiError) as cm:
            api.talk(["/ping", "=routing-table=to-A"])
        self.assertIn("routing-table", str(cm.exception))
        self.assertEqual(api.talk(["/x/print"]), [{"a": "1"}])

    def test_empty_reply(self):
        api = ap.Api(FakeSocket(sentence("!empty") + sentence("!done")))
        self.assertEqual(api.talk(["/routing/rule/print"]), [])

    def test_fatal(self):
        api = ap.Api(FakeSocket(sentence("!fatal", "session terminated")))
        with self.assertRaises(ap.ApiFatal):
            api.talk(["/quit"])

    def test_eof(self):
        api = ap.Api(FakeSocket(b"\x03!re"))
        with self.assertRaises(EOFError):
            api.talk(["/x"])

    def test_long_word(self):
        word = "=on-event=" + "x" * 300
        incoming = sentence("!re", word) + sentence("!done")
        api = ap.Api(FakeSocket(incoming))
        self.assertEqual(api.talk(["/p"]), [{"on-event": "x" * 300}])

    def test_login_ok(self):
        s = FakeSocket(sentence("!done"))
        ap.Api(s).login("admin", "pw")
        self.assertEqual(s.sent, ap.encode_sentence(["/login", "=name=admin", "=password=pw"]))

    def test_resolve_host(self):
        fam, addr = ap.resolve_host("fe80::de2c:6eff:fef7:7fe9%82", 8728)
        self.assertEqual(addr, ("fe80::de2c:6eff:fef7:7fe9", 8728, 0, 82))
        fam, addr = ap.resolve_host("192.168.88.1", 8728)
        self.assertEqual(addr, ("192.168.88.1", 8728))
        with self.assertRaises(ValueError):
            ap.resolve_host("fe80::1", 8728)

    def test_mac_to_link_local(self):
        self.assertEqual(ap.mac_to_link_local("DC:2C:6E:F7:7F:E9"), "fe80::de2c:6eff:fef7:7fe9")


# ------------------------------------------------------------------------------ RouterOS falso

FACTORY = {
    "/interface/bridge": [{".id": "*7", "name": "bridge", "mac-address": "DC:2C:6E:F7:7F:E9",
                           "auto-mac": "true", "protocol-mode": "rstp"}],
    "/interface/bridge/port": [{".id": "*%d" % i, "interface": n, "bridge": "bridge"}
                               for i, n in enumerate(["ether2", "ether3", "ether4", "ether5", "wlan1", "wlan2"])],
    "/interface/list/member": [
        {".id": "*1", "list": "LAN", "interface": "bridge", "dynamic": "false", "disabled": "false", "comment": "defconf"},
        {".id": "*2", "list": "WAN", "interface": "ether1", "dynamic": "false", "disabled": "false", "comment": "defconf"}],
    "/routing/table": [{".id": "*0", "name": "main", "fib": "", "dynamic": "true"}],
    "/ip/address": [
        {".id": "*2", "address": "192.168.1.1/24", "network": "192.168.1.0", "interface": "bridge", "dynamic": "false", "disabled": "false"},
        {".id": "*4", "address": "192.168.2.100/24", "network": "192.168.2.0", "interface": "ether1", "dynamic": "true", "disabled": "false"}],
    "/ip/pool": [{".id": "*1", "name": "default-dhcp", "ranges": "192.168.1.10-192.168.1.254"}],
    "/ip/dhcp-server": [{".id": "*1", "name": "defconf", "interface": "bridge", "lease-time": "10m",
                         "address-pool": "default-dhcp", "disabled": "false"}],
    "/ip/dhcp-server/network": [
        {".id": "*2", "address": "192.168.1.0/24", "gateway": "192.168.1.1", "dns-server": "192.168.1.1"},
        {".id": "*1", "address": "192.168.88.0/24", "gateway": "192.168.88.1", "dns-server": "192.168.88.1", "comment": "defconf"}],
    "/ip/dhcp-client": [{".id": "*1", "interface": "ether1", "add-default-route": "yes", "use-peer-dns": "true",
                         "use-peer-ntp": "true", "status": "bound", "address": "192.168.2.100/24",
                         "gateway": "192.168.2.1", "disabled": "false", "comment": "defconf"}],
    "/ip/route": [{".id": "*80000002", "dst-address": "0.0.0.0/0", "routing-table": "main", "gateway": "192.168.2.1",
                   "distance": "1", "dynamic": "true", "active": "true"}],
    "/routing/rule": [],
    "/ip/firewall/nat": [{".id": "*2", "chain": "srcnat", "action": "masquerade", "src-address": "192.168.1.0/24",
                          "out-interface": "ether1", "dynamic": "false"}],
    "/user/group": [{".id": "*3", "name": "full", "policy": "local,telnet,ssh,ftp,reboot,read,write,policy,test,winbox,password,web,sniff,sensitive,api,romon,rest-api"}],
    "/user": [{".id": "*1", "name": "admin", "group": "full", "address": "", "disabled": "false"}],
    "/system/scheduler": [],
    "/ip/service": [{".id": "*1", "name": "ftp", "port": "21", "address": "", "disabled": "false"},
                    {".id": "*7", "name": "api", "port": "8728", "address": "", "disabled": "false"}],
    "/ip/dns": [{"servers": "", "dynamic-servers": "192.168.2.1", "allow-remote-requests": "true"}],
    "/system/ntp/client": [{"enabled": "false", "mode": "unicast", "servers": "", "status": "stopped"}],
    "/system/clock": [{"time-zone-name": "America/Bogota", "time-zone-autodetect": "true"}],
    "/system/identity": [{"name": "MikroTik"}],
    "/ip/settings": [{"rp-filter": "no", "ip-forward": "true"}],
    "/ipv6/address": [{".id": "*1", "address": "fe80::de2c:6eff:fef7:7fe9/64", "interface": "bridge",
                       "link-local": "true", "dynamic": "true"}],
    # Firewall de fábrica tal como se leyó en el hAP el 2026-09-29 (contadores omitidos).
    "/ip/firewall/filter": [
        {".id": "*D", "chain": "forward", "action": "passthrough", "bytes": "43767534", "dynamic": "true",
         "comment": "special dummy rule to show fasttrack counters"},
        {".id": "*1", "chain": "input", "action": "accept", "connection-state": "established,related,untracked",
         "dynamic": "false", "comment": "defconf: accept established,related,untracked"},
        {".id": "*2", "chain": "input", "action": "drop", "connection-state": "invalid", "dynamic": "false",
         "comment": "defconf: drop invalid"},
        {".id": "*5", "chain": "input", "action": "drop", "in-interface-list": "!LAN", "dynamic": "false",
         "comment": "defconf: drop all not coming from LAN"},
        {".id": "*8", "chain": "forward", "action": "fasttrack-connection", "hw-offload": "true",
         "connection-state": "established,related", "dynamic": "false", "comment": "defconf: fasttrack"},
        {".id": "*9", "chain": "forward", "action": "accept", "connection-state": "established,related,untracked",
         "dynamic": "false", "comment": "defconf: accept established,related, untracked"},
        {".id": "*C", "chain": "forward", "action": "drop", "connection-state": "new",
         "connection-nat-state": "!dstnat", "in-interface-list": "WAN", "dynamic": "false",
         "comment": "defconf: drop all from WAN not DSTNATed"}],
    "/ip/firewall/mangle": [
        {".id": "*3", "chain": "prerouting", "action": "passthrough", "dynamic": "true",
         "comment": "special dummy rule to show fasttrack counters"}],
    "/ip/firewall/raw": [
        {".id": "*1", "chain": "prerouting", "action": "passthrough", "dynamic": "true",
         "comment": "special dummy rule to show fasttrack counters"}],
}


class FakeRouter:
    """Implementa `talk()` como Api, sobre menús en memoria."""

    def __init__(self, state, place_before=True):
        self.m = copy.deepcopy(state)
        self.done = {}
        self.next_id = 100
        self.log = []
        self.place_before = place_before  # False: simula un equipo que rechaza place-before

    def talk(self, words):
        path = words[0]
        params = ap.parse_attrs([w for w in words[1:] if w.startswith("=")])
        queries = dict(w[1:].split("=", 1) for w in words[1:] if w.startswith("?"))
        menu, _, cmd = path.rpartition("/")
        self.done = {}
        self.log.append((cmd, menu, params))
        if cmd == "print":
            rows = [r for r in self.m.get(menu, []) if all(r.get(k) == v for k, v in queries.items())]
            return copy.deepcopy(rows)
        if cmd in ("set", "remove", "move") and menu not in self.m:
            raise ap.ApiError("no such command")
        items = self.m.setdefault(menu, [])
        if cmd == "add":
            if menu == "/routing/table" and params.get("fib") == "":
                params["fib"] = ""
            before = params.pop("place-before", None)
            if before is not None and not self.place_before:
                raise ap.ApiError("unknown parameter place-before")
            row = {".id": "*%X" % self.next_id}
            self.next_id += 1
            row.update(params)
            if menu == "/ip/dhcp-client":
                row.update({"status": "bound", "gateway": "192.168.1.1", "address": "192.168.1.221/24"})
            if before is not None:
                idx = next((i for i, r in enumerate(items) if r[".id"] == before), None)
                if idx is None:
                    raise ap.ApiError("no such item (place-before)")
                items.insert(idx, row)
            else:
                items.append(row)
            self.done = {"ret": row[".id"]}
            return []
        if cmd == "set":
            rid = params.pop(".id", None) or params.pop("numbers", None)
            targets = [r for r in items if rid is None or r.get(".id") == rid]
            if not targets:
                raise ap.ApiError("no such item")
            for r in targets:
                for k, v in params.items():
                    if k == "password":
                        r["_password"] = v
                    else:
                        r[k] = v
            return []
        if cmd == "remove":
            rid = params.get("numbers") or params.get(".id")
            before = len(items)
            items[:] = [r for r in items if r.get(".id") != rid]
            if len(items) == before:
                raise ap.ApiError("no such item")
            return []
        if cmd == "move":
            rid, dest = params["numbers"], params["destination"]
            row = next(r for r in items if r[".id"] == rid)
            items.remove(row)
            idx = next(i for i, r in enumerate(items) if r[".id"] == dest)
            items.insert(idx, row)
            return []
        if path == "/export":
            items = self.m.setdefault("/file", [])
            items.append({".id": "*F%X" % self.next_id, "name": params["file"] + ".rsc", "size": "10051"})
            self.next_id += 1
            return []
        if path == "/system/backup/save":
            items = self.m.setdefault("/file", [])
            items.append({".id": "*F%X" % self.next_id, "name": params["name"] + ".backup", "size": "35699"})
            self.next_id += 1
            return []
        raise ap.ApiError("comando no simulado: " + path)


def args_for(extra=()):
    return ap.build_parser().parse_args(["--password", "x", "--no-egress-test"] + list(extra))


DEPLOYED_WAN_SCRIPT_A = (
    ':if ($bound=1) do={:local gw ($"gateway-address" . "%" . $interface); :foreach c in={"probe:A-default";'
    '"probe:main-A"} do={:foreach r in=[/ip route find comment=$c] do={:if ([:tostr [/ip route get $r gateway]] '
    '!= $gw) do={/ip route set $r gateway=$gw; :log info "probe: $c -> $gw"}}}; :local d ($"gateway-address" . '
    '"/32"); :foreach r in=[/routing rule find comment="probe:mgmt-A"] do={:if ([:tostr [/routing rule get $r '
    'dst-address]] != $d) do={/routing rule set $r dst-address=$d; :log info "probe: mgmt-A -> $d"}}}')


def quiet(*_a, **_k):
    pass


def apply_without_backup(fake, args):
    ctx = ap.Ctx(args, "apply")
    ros = ap.RouterOS(fake)
    ap.apply_phases(ros, ctx, connect=None, log=quiet)
    return ctx


class PlanTests(unittest.TestCase):
    def setUp(self):
        self._pw_file = ap.PASSWORD_FILE
        ap.PASSWORD_FILE = os.devnull  # no escribir la contraseña de prueba en disco
        self._sleep = ap.time.sleep
        ap.time.sleep = lambda s: None

    def tearDown(self):
        ap.PASSWORD_FILE = self._pw_file
        ap.time.sleep = self._sleep

    def plan_for(self, fake, mode="dry", extra=()):
        ctx = ap.Ctx(args_for(extra), mode)
        return ap.plan(ap.read_state(ap.RouterOS(fake), ctx), ctx), ctx

    def test_factory_plan_contains_key_changes(self):
        p, ctx = self.plan_for(FakeRouter(FACTORY))
        lines = "\n".join(a.console(True) for _, acts in p for a in acts)
        for needle in ("auto-mac=no admin-mac=DC:2C:6E:F7:7F:E9", "/routing table add name=to-A",
                       "/interface bridge port remove *0", "address=192.168.88.1/24",
                       "comment=probe:phone-local", "comment=phone-probe table=main",
                       "gateway=192.168.2.1%ether1", "gateway=192.168.1.1%ether2",
                       "check-gateway=ping", "out-interface-list=WAN", "address=192.168.88.0/24,192.168.89.0/24,fe80::/10",
                       "servers=162.159.200.1,162.159.200.123", "password=<oculta>"):
            self.assertIn(needle, lines)
        # ether2 no está bound en fábrica: se avisa del gateway por defecto
        self.assertTrue(any("ether2" in w for w in ctx.warnings))

    def test_apply_then_plan_is_empty(self):
        fake = FakeRouter(FACTORY)
        ctx = apply_without_backup(fake, args_for())
        self.assertEqual(len(ctx.phone_password), 16)
        p, _ = self.plan_for(fake, "verify")
        self.assertEqual(p, [], "\n".join(a.console(True) for _, acts in p for a in acts))
        # segunda pasada: nada que escribir
        n = len(fake.log)
        apply_without_backup(fake, args_for())
        writes = [e for e in fake.log[n:] if e[0] != "print"]
        self.assertEqual(writes, [])

    def test_result_matches_contract(self):
        fake = FakeRouter(FACTORY)
        apply_without_backup(fake, args_for())
        m = fake.m
        rules = [r.get("comment") for r in m["/routing/rule"]]
        self.assertEqual(rules, ap.RULE_ORDER)
        self.assertEqual(rules, ["probe:phone-local", "probe:mgmt-A", "probe:mgmt-B",
                                 "probe:health-A", "probe:health-B", "phone-probe"])
        by = {r["comment"]: r for r in m["/routing/rule"]}
        self.assertEqual((by["probe:mgmt-A"]["dst-address"], by["probe:mgmt-A"]["table"]),
                         ("192.168.1.1/32", "to-A"))
        self.assertEqual((by["probe:mgmt-B"]["dst-address"], by["probe:mgmt-B"]["table"]),
                         ("192.168.2.1/32", "to-B"))
        self.assertEqual((by["probe:health-A"]["dst-address"], by["probe:health-A"]["table"]),
                         ("9.9.9.9/32", "to-A"))
        self.assertEqual((by["probe:health-B"]["dst-address"], by["probe:health-B"]["table"]),
                         ("149.112.112.112/32", "to-B"))
        for c in ap.RULE_ORDER:
            self.assertEqual(by[c]["action"], "lookup-only-in-table")
        probe = by["phone-probe"]
        self.assertEqual((probe["table"], probe["action"], probe["src-address"]),
                         ("main", "lookup-only-in-table", "192.168.89.0/24"))
        routes = {r.get("comment"): r for r in m["/ip/route"]}
        self.assertEqual(routes["probe:A-default"]["gateway"], "192.168.1.1%ether2")
        self.assertEqual(routes["probe:A-default"]["routing-table"], "to-A")
        self.assertNotIn("check-gateway", routes["probe:A-default"])
        self.assertEqual(routes["probe:B-default"]["gateway"], "192.168.2.1%ether1")
        self.assertEqual(routes["probe:main-B"]["distance"], "2")
        self.assertEqual([p["interface"] for p in m["/interface/bridge/port"]], ["ether4", "ether5", "wlan1", "wlan2"])
        self.assertEqual(m["/interface/bridge"][0]["admin-mac"], "DC:2C:6E:F7:7F:E9")
        mg = next(n for n in m["/ip/dhcp-server/network"] if n.get("comment") == "probe:mgmt")
        self.assertEqual((mg["address"], mg["gateway"], mg["dns-server"]), ("192.168.88.0/24", "", ""))
        user = next(u for u in m["/user"] if u["name"] == "phone-probe")
        self.assertEqual((user["group"], user["address"]), ("probe-api", "192.168.89.0/24"))
        self.assertEqual(m["/user/group"][-1]["policy"], "read,write,api,test")
        # la NAT vieja de fábrica se borra; queda solo el masquerade por WAN
        self.assertEqual([(n.get("out-interface-list"), n.get("src-address")) for n in m["/ip/firewall/nat"]],
                         [("WAN", None)])
        # la MAC del bridge se fija ANTES de sacar ether2 (si no, cambia la link-local del PC)
        writes = [(c, menu) for c, menu, _ in fake.log if c != "print"]
        self.assertLess(writes.index(("set", "/interface/bridge")),
                        writes.index(("remove", "/interface/bridge/port")))

    def test_rule_table_is_not_a_difference(self):
        fake = FakeRouter(FACTORY)
        apply_without_backup(fake, args_for())
        next(r for r in fake.m["/routing/rule"] if r["comment"] == "phone-probe")["table"] = "to-A"
        p, _ = self.plan_for(fake, "verify")
        self.assertEqual(p, [])
        with self.assertRaises(SystemExit):
            ap.mode_apply(ap.RouterOS(fake), ap.Ctx(args_for(), "apply"), None, quiet)

    def test_wrong_rule_order_is_fixed(self):
        import itertools
        import random
        fake = FakeRouter(FACTORY)
        apply_without_backup(fake, args_for())
        base = copy.deepcopy(fake.m["/routing/rule"])
        perms = list(itertools.permutations(range(len(base))))
        random.Random(7).shuffle(perms)
        for perm in [tuple(reversed(range(len(base))))] + perms[:60]:
            fake.m["/routing/rule"] = [copy.deepcopy(base[i]) for i in perm]
            # una regla ajena en el medio no debe impedir el orden relativo
            fake.m["/routing/rule"].insert(2, {".id": "*F0", "dst-address": "10.0.0.0/8",
                                               "action": "lookup", "table": "main", "comment": "otra"})
            p, ctx = self.plan_for(fake, "verify")
            ops = [a.op for _, acts in p for a in acts]
            self.assertEqual(set(ops) - {"move"}, set(), perm)
            self.assertTrue(any("ajena" in w for w in ctx.warnings))
            apply_without_backup(fake, args_for())
            got = [r["comment"] for r in fake.m["/routing/rule"] if r["comment"] in ap.RULE_ORDER]
            self.assertEqual(got, ap.RULE_ORDER, perm)
            self.assertEqual(self.plan_for(fake, "verify")[0], [])

    def test_upgrade_from_section3_install_uses_place_before(self):
        """Estado real al 2026-09-29 (solo §3): las reglas nuevas se crean ya en su lugar, antes de
        phone-probe, sin /move; y --apply hace el respaldo antes de la primera escritura."""
        for accepts in (True, False):
            fake = self.section3_router(accepts)
            p, _ = self.plan_for(fake)
            lines = [a.console(True) for _, acts in p for a in acts]
            probe_id = next(r[".id"] for r in fake.m["/routing/rule"] if r["comment"] == "phone-probe")
            adds = [l for l in lines if l.startswith("/routing rule add")]
            self.assertEqual(len(adds), 4, lines)
            for l in adds:
                self.assertIn("place-before=" + probe_id, l)
            self.assertTrue(any(l.startswith("/ip firewall nat remove") for l in lines), lines)
            self.assertTrue(any("policy=read,write,api,test" in l for l in lines), lines)
            self.assertEqual(len([l for l in lines if l.startswith("/ip dhcp-client set")]), 2, lines)
            apply_without_backup(fake, args_for())
            self.assertEqual([r["comment"] for r in fake.m["/routing/rule"]], ap.RULE_ORDER, accepts)
            moves = [e for e in fake.log if e[0] == "move"]
            self.assertEqual(bool(moves), not accepts)  # sin place-before: agrega al final y mueve
            self.assertEqual(self.plan_for(fake, "verify")[0], [])

    def test_apply_backs_up_before_first_write(self):
        fake = self.section3_router(True)
        fake.m["/file"] = [{".id": "*1", "name": "flash", "type": "disk"}]
        fake.m["/system/resource"] = [{"free-hdd-space": "1601536"}]
        tmp = os.path.join(os.path.dirname(os.path.abspath(__file__)), "_test_backups")
        try:
            ctx = ap.Ctx(args_for(["--skip-download", "--backup-dir", tmp]), "apply")
            ap.mode_apply(ap.RouterOS(fake), ctx, None, log=quiet)
        finally:
            for f in os.listdir(tmp) if os.path.isdir(tmp) else []:
                os.remove(os.path.join(tmp, f))
            if os.path.isdir(tmp):
                os.rmdir(tmp)
        writes = [(c, menu) for c, menu, _ in fake.log if c != "print"]
        self.assertEqual(writes[0], ("export", ""))
        self.assertIn(("save", "/system/backup"), writes)
        self.assertLess(writes.index(("save", "/system/backup")), writes.index(("remove", "/ip/firewall/nat")))

    def section3_router(self, accepts_place_before):
        """RouterOS falso con la instalación de §3 ya hecha (como el equipo real antes de §6.1)."""
        fake = FakeRouter(FACTORY, place_before=accepts_place_before)
        apply_without_backup(fake, args_for())
        fake.m["/routing/rule"] = [r for r in fake.m["/routing/rule"]
                                   if r["comment"] in ("probe:phone-local", "phone-probe")]
        next(g for g in fake.m["/user/group"] if g["name"] == "probe-api")["policy"] = \
            "read,write,api,!local,!telnet,!ssh,!ftp,!reboot,!policy,!test"
        fake.m["/ip/firewall/nat"].insert(0, copy.deepcopy(FACTORY["/ip/firewall/nat"][0]))
        for c in fake.m["/ip/dhcp-client"]:
            c["script"] = c["script"].split("; :local d ")[0] + "}"
        fake.log = []
        return fake

    def test_gateway_follows_dhcp_and_is_kept_when_wan_down(self):
        fake = FakeRouter(FACTORY)
        apply_without_backup(fake, args_for())
        c = next(x for x in fake.m["/ip/dhcp-client"] if x["interface"] == "ether2")
        c["gateway"] = "192.168.10.1"
        p, _ = self.plan_for(fake, "verify")
        sets = [a for _, acts in p for a in acts]
        self.assertEqual({a.props.get("gateway") for a in sets if a.menu == "/ip/route"}, {"192.168.10.1%ether2"})
        self.assertEqual(len([a for a in sets if a.menu == "/ip/route"]), 2)  # A-default y main-A
        rule_sets = [a for a in sets if a.menu == "/routing/rule"]
        self.assertEqual([a.props for a in rule_sets], [{"dst-address": "192.168.10.1/32"}])  # mgmt-A
        self.assertEqual(len(sets), 3)
        c["status"] = "searching..."
        del c["gateway"]
        p, _ = self.plan_for(fake, "verify")
        self.assertEqual(p, [])  # WAN caído: se conserva el gateway que había

    def test_apply_aborts_when_a_menu_cannot_be_read(self):
        # Tratar un menú ilegible como vacío crearía duplicados (reglas, rutas, usuario).
        fake = FakeRouter(FACTORY)
        orig = fake.talk

        def talk(words):
            if words[0] == "/routing/rule/print":
                raise ap.ApiError("no such command prefix")
            return orig(words)
        fake.talk = talk
        with self.assertRaises(ap.ApiError):
            apply_without_backup(fake, args_for())
        self.assertEqual([e for e in fake.log if e[0] != "print"], [])
        # en dry-run solo avisa
        ctx = ap.Ctx(args_for(), "dry")
        ap.read_state(ap.RouterOS(fake), ctx)
        self.assertTrue(any("/routing/rule" in w for w in ctx.warnings))

    def test_watchdog_policy_and_reset_conditions(self):
        fake = FakeRouter(FACTORY)
        apply_without_backup(fake, args_for())
        wd = next(x for x in fake.m["/system/scheduler"] if x["name"] == ap.WATCHDOG_NAME)
        # con read,write las :global no sobreviven entre corridas (probado en el hAP)
        self.assertEqual(wd["policy"], "read,write,policy,test")
        ev = wd["on-event"]
        for needle in ('message~"^user phone-probe logged in"', "$t!=[:tostr $probeLastTable]",
                       "$l!=[:tostr $probeLastLogin]", "$probeForcedMin>=10", "table=main"):
            self.assertIn(needle, ev)
        # un vigilante viejo (policy read,write, sin reinicio por actividad) es diferencia
        wd["policy"] = "read,write"
        p, _ = self.plan_for(fake, "verify")
        acts = [a for _, xs in p for a in xs]
        self.assertEqual([(a.op, a.menu) for a in acts], [("set", "/system/scheduler")])
        self.assertEqual(acts[0].props, {"policy": "read,write,policy,test"})

    def test_backup_goes_to_flash_and_skip_download_keeps_files(self):
        state = copy.deepcopy(FACTORY)
        state["/file"] = [{".id": "*1", "name": "flash", "type": "disk"}]
        state["/system/resource"] = [{"free-hdd-space": "1601536", "version": "7.6 (stable)"}]
        fake = FakeRouter(state)
        tmp = os.path.join(os.path.dirname(os.path.abspath(__file__)), "_test_backups")
        try:
            args = args_for(["--skip-download", "--backup-dir", tmp])
            ctx = ap.Ctx(args, "apply")
            name = ap.do_backup(ap.RouterOS(fake), ctx, log=quiet)
            files = {f["name"] for f in fake.m["/file"]}
            self.assertIn("flash/%s.backup" % name, files)
            self.assertIn("flash/%s.rsc" % name, files)  # sin bajar, el .rsc no se borra
            self.assertTrue(os.path.exists(os.path.join(tmp, name + ".json")))
        finally:
            for f in os.listdir(tmp) if os.path.isdir(tmp) else []:
                os.remove(os.path.join(tmp, f))
            if os.path.isdir(tmp):
                os.rmdir(tmp)

    def test_backup_aborts_with_low_flash(self):
        state = copy.deepcopy(FACTORY)
        state["/system/resource"] = [{"free-hdd-space": "1000"}]
        fake = FakeRouter(state)
        with self.assertRaises(SystemExit):
            ap.do_backup(ap.RouterOS(fake), ap.Ctx(args_for(), "apply"), log=quiet)
        self.assertEqual([e for e in fake.log if e[0] != "print"], [])

    def test_new_password_not_printed_unless_asked(self):
        state = copy.deepcopy(FACTORY)
        state["/file"] = [{".id": "*1", "name": "flash", "type": "disk"}]
        state["/system/resource"] = [{"free-hdd-space": "1601536"}]
        tmp = os.path.join(os.path.dirname(os.path.abspath(__file__)), "_test_backups")
        try:
            for extra, printed in (((), False), (("--show-password",), True)):
                fake = FakeRouter(state)
                out = []
                ctx = ap.Ctx(args_for(["--skip-download", "--backup-dir", tmp] + list(extra)), "apply")
                ap.mode_apply(ap.RouterOS(fake), ctx, None, log=out.append)
                self.assertTrue(ctx.password_saved)
                self.assertEqual(any(ctx.phone_password in line for line in out), printed, extra)
                self.assertTrue(any(ap.PASSWORD_FILE in line for line in out))
        finally:
            for f in os.listdir(tmp) if os.path.isdir(tmp) else []:
                os.remove(os.path.join(tmp, f))
            if os.path.isdir(tmp):
                os.rmdir(tmp)

    def test_egress_cleanup_failure_does_not_raise(self):
        fake = FakeRouter(FACTORY)
        orig = fake.talk

        def talk(words):
            if words[0] in ("/ping", "/tool/fetch"):
                raise ap.ApiError("simulado")
            if words[0] == "/routing/rule/print":
                raise EOFError("sesión caída")
            return orig(words)
        fake.talk = talk
        ctx = ap.Ctx(args_for(), "verify")
        ap.egress_test(ap.RouterOS(fake), ctx, log=quiet)
        self.assertTrue(any(ap.TMP_RULE_COMMENT in w for w in ctx.warnings))

    def test_mgmt_dst_without_mask_is_not_a_difference(self):
        fake = FakeRouter(FACTORY)
        apply_without_backup(fake, args_for())
        next(r for r in fake.m["/routing/rule"] if r["comment"] == "probe:mgmt-A")["dst-address"] = "192.168.1.1"
        self.assertEqual(self.plan_for(fake, "verify")[0], [])

    def test_wan_script_updates_mgmt_rule_only_if_different(self):
        for slot in ("A", "B"):
            sc = ap.wan_script(slot)
            w = ap.WAN[slot]
            for needle in ('/routing rule find comment="%s"' % w["mgmt"], ':local d ($"gateway-address" . "/32")',
                           '[:tostr [/routing rule get $r dst-address]] != $d', "dst-address=$d",
                           '"%s";"%s"' % (w["route"], w["main"])):
                self.assertIn(needle, sc)
            other = ap.WAN["B" if slot == "A" else "A"]
            self.assertNotIn(other["mgmt"], sc)
            self.assertEqual(sc.count("{"), sc.count("}"))
        # Texto leído del hAP el 2026-09-29 tras aplicar §6.1: la herramienta no debe reescribirlo.
        self.assertEqual(ap.wan_script("A"), DEPLOYED_WAN_SCRIPT_A)

    def test_identical_gateways_warn(self):
        fake = FakeRouter(FACTORY)
        apply_without_backup(fake, args_for())
        for c in fake.m["/ip/dhcp-client"]:
            c.update({"status": "bound", "gateway": "192.168.1.1"})
        _, ctx = self.plan_for(fake, "verify")
        self.assertTrue(any("mismo gateway" in w for w in ctx.warnings), ctx.warnings)
        _, ctx = self.plan_for(FakeRouter(FACTORY))
        self.assertFalse(any("mismo gateway" in w for w in ctx.warnings), ctx.warnings)

    def test_unreadable_filter_menu_is_not_reported_as_missing_fasttrack(self):
        fake = FakeRouter(FACTORY)
        orig = fake.talk

        def talk(words):
            if words[0] == "/ip/firewall/filter/print":
                raise ap.ApiError("not enough permissions")
            return orig(words)
        fake.talk = talk
        _, ctx = self.plan_for(fake, "verify")
        self.assertFalse(ctx.fasttrack_ok)
        self.assertTrue(any("no se pudo leer" in w for w in ctx.warnings if w.startswith("Fasttrack")),
                        ctx.warnings)

    def test_only_stale_nat_is_removed_and_foreign_rules_warn(self):
        state = copy.deepcopy(FACTORY)
        state["/ip/firewall/nat"].append({".id": "*9", "chain": "dstnat", "action": "dst-nat",
                                          "to-addresses": "192.168.89.10", "dynamic": "false"})
        state["/ip/firewall/mangle"].append({".id": "*7", "chain": "prerouting", "action": "mark-routing",
                                             "new-routing-mark": "to-A", "dynamic": "false"})
        state["/ip/firewall/filter"].append({".id": "*E", "chain": "forward", "action": "drop",
                                             "dst-address": "8.8.8.8", "dynamic": "false"})
        p, ctx = self.plan_for(FakeRouter(state))
        removes = [a for _, acts in p for a in acts if a.op == "remove" and a.menu == "/ip/firewall/nat"]
        self.assertEqual([a.id for a in removes], ["*2"])
        for needle in ("ip firewall nat: .id=*9", "ip firewall mangle: .id=*7", "ip firewall filter: .id=*E"):
            self.assertTrue(any(needle in w for w in ctx.warnings), (needle, ctx.warnings))
        self.assertTrue(ctx.fasttrack_ok)
        # ni las reglas defconf ni las dinámicas (contadores del fasttrack) cuentan como ajenas
        p, ctx = self.plan_for(FakeRouter(FACTORY))
        self.assertFalse(any("ajena" in w for w in ctx.warnings), ctx.warnings)

    def test_fasttrack_checks(self):
        cases = [(None, "no hay regla fasttrack"), ({"disabled": "true"}, "deshabilitada"),
                 ({"hw-offload": "false"}, "hw-offload"), ({}, None)]
        for change, needle in cases:
            state = copy.deepcopy(FACTORY)
            ft = next(r for r in state["/ip/firewall/filter"] if r.get("comment") == "defconf: fasttrack")
            if change is None:
                state["/ip/firewall/filter"].remove(ft)
            else:
                ft.update(change)
            fake = FakeRouter(state)
            apply_without_backup(fake, args_for())
            ctx = ap.Ctx(args_for(), "verify")
            out = []
            rc = ap.mode_verify(ap.RouterOS(fake), ctx, log=out.append)
            self.assertEqual(ctx.fasttrack_ok, needle is None, change)
            self.assertEqual(rc, 0 if needle is None else 1, (change, out))
            if needle:
                self.assertTrue(any(needle in w for w in ctx.warnings), ctx.warnings)
            # el firewall nunca se toca
            self.assertEqual([e for e in fake.log if e[0] != "print" and "firewall/filter" in e[1]], [])

    def test_forbids_api_address_without_link_local(self):
        with self.assertRaises(SystemExit):
            ap.validate_args(args_for(["--api-address", "192.168.88.0/24"]))
        ap.validate_args(args_for())

    def test_same_normalization(self):
        self.assertTrue(ap.same("disabled", None, "no"))
        self.assertTrue(ap.same("use-peer-dns", "false", "no"))
        self.assertTrue(ap.same("address", "fe80::/10,192.168.88.0/24", "192.168.88.0/24,fe80::/10"))
        self.assertTrue(ap.same("policy", "read,write,api,!local,!ftp", "read,write,api"))
        self.assertFalse(ap.same("policy", "read,write,api,ssh", "read,write,api"))
        self.assertTrue(ap.same("on-event", "a  b\n c", "a b c"))


class RscTests(unittest.TestCase):
    def test_rsc_structure(self):
        args = argparse.Namespace(rule_watchdog_min=10, bridge="bridge", check_gateway="ping",
                                  api_address=ap.API_ADDRESS_DEFAULT)
        rsc = ap.emit_rsc(args)
        body = "\n".join(line for line in rsc.splitlines() if not line.startswith("#"))
        # llaves y corchetes balanceados fuera de cadenas
        depth_b = depth_s = 0
        in_str = esc = False
        for ch in body:
            if in_str:
                if esc:
                    esc = False
                elif ch == "\\":
                    esc = True
                elif ch == '"':
                    in_str = False
                continue
            if ch == '"':
                in_str = True
            elif ch == "{":
                depth_b += 1
            elif ch == "}":
                depth_b -= 1
            elif ch == "[":
                depth_s += 1
            elif ch == "]":
                depth_s -= 1
            self.assertGreaterEqual(depth_b, 0)
            self.assertGreaterEqual(depth_s, 0)
        self.assertFalse(in_str)
        self.assertEqual((depth_b, depth_s), (0, 0))
        for needle in ('comment="phone-probe"', 'comment="probe:phone-local"', "table=main",
                       "fe80::/10", "probe-rule-watchdog", '\\$\\"gateway-address\\"', "admin-mac=",
                       'comment="probe:mgmt-A" place-before=[find where comment="phone-probe"]',
                       "dst-address=149.112.112.112/32 action=lookup-only-in-table table=to-B",
                       "policy=read,write,api,test", 'src-address="192.168.1.0/24" and out-interface="ether1"',
                       'fasttrack-connection', '/routing rule find comment=\\"probe:mgmt-B\\"'):
            self.assertIn(needle, rsc)
        self.assertNotIn("\t", rsc)

    def test_repo_rsc_is_current(self):
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "probe-ab.rsc")
        if not os.path.exists(path):
            self.skipTest("probe-ab.rsc aún no generado")
        args = ap.build_parser().parse_args(["--emit-rsc"])
        with open(path, encoding="utf-8", newline="") as fh:
            self.assertEqual(fh.read().replace("\r\n", "\n"), ap.emit_rsc(args),
                             "regenera con: python apply_probe.py --emit-rsc > probe-ab.rsc")


if __name__ == "__main__":
    unittest.main()
