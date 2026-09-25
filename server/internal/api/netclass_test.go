package api

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"notion5g/server/internal/store"
)

func farFuture() time.Time { return time.Now().Add(100 * 365 * 24 * time.Hour) }

func ev(obs, ref string, hint string, refAge time.Duration) netEvidence {
	e := netEvidence{obsStatus: "ok", hint: hint, refAge: refAge}
	if obs != "" {
		e.obs = netip.MustParseAddr(obs)
	}
	if ref != "" {
		e.ref = store.DeviceEgress{IP: ref, Family: ipFamily(netip.MustParseAddr(ref))}
		e.refOK = true
	}
	return e
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name                      string
		e                         netEvidence
		route, confidence, reason string
	}{
		// La misma IP pública EXACTA bajo CGNAT móvil es evidencia, no prueba:
		// el celular midiendo por su propia SIM del mismo operador también
		// puede caer en esa IP. Se dice "router", pero con confianza baja, y
		// con eso no alimenta la velocidad titular del equipo.
		{"misma ip que el router: sola no confirma", ev("190.145.155.7", "190.145.155.7", "", time.Minute),
			"router", "low", "ip-matches-router-cgnat-ambiguous"},
		{"misma ip + el navegador dice wifi", ev("190.145.155.7", "190.145.155.7", "wifi", time.Minute),
			"router", "medium", "ip-matches-router-cgnat-ambiguous"},
		{"ip distinta", ev("181.49.1.1", "190.145.155.7", "", time.Minute),
			"other-network", "medium", "ip-differs-from-router"},
		{"ip distinta + hint celular = dos señales", ev("181.49.1.1", "190.145.155.7", "cellular", time.Minute),
			"other-network", "high", "ip-differs-from-router"},
		// "wifi" no identifica CUÁL wifi: no puede contradecir a la IP.
		{"ip distinta aunque esté en un wifi: es OTRO wifi", ev("181.49.1.1", "190.145.155.7", "wifi", time.Minute),
			"other-network", "medium", "ip-differs-from-router"},
		{"mismo /24: cgnat, no prueba nada", ev("190.145.155.9", "190.145.155.7", "", time.Minute),
			"unknown", "low", "same-v4-prefix-cgnat"},
		{"mismo /24 pero el celular dice celular", ev("190.145.155.9", "190.145.155.7", "cellular", time.Minute),
			"other-network", "medium", "hint-cellular"},
		// v6: un /64 es un enlace, dos suscriptores móviles no lo comparten.
		{"mismo /64 en v6 = el mismo enlace", ev("2803:a3e0:1:2::9", "2803:a3e0:1:2::1", "", time.Minute),
			"router", "medium", "same-v6-prefix-64"},
		// Mismo /48 y distinto /64 es el pool del operador: 65.536 /64. El
		// celular por su propia SIM del mismo operador cae ahí también.
		{"mismo /48 en v6: pool del operador, no confirma", ev("2803:a3e0:1:2::9", "2803:a3e0:1:5::1", "", time.Minute),
			"router", "low", "same-v6-prefix-carrier-pool"},
		{"mismo /48 en v6 + wifi", ev("2803:a3e0:1:2::9", "2803:a3e0:1:5::1", "wifi", time.Minute),
			"router", "medium", "same-v6-prefix-carrier-pool"},
		{"464xlat: v6 contra v4 no son comparables", ev("2803:a3e0:1:2::9", "190.145.155.7", "", time.Minute),
			"unknown", "low", "ip-family-mismatch"},
		{"sin referencia", ev("181.49.1.1", "", "", 0),
			"unknown", "low", "no-router-reference"},
		{"referencia vieja: NUNCA router", ev("190.145.155.7", "190.145.155.7", "", 11*time.Minute),
			"unknown", "low", "stale-router-reference"},
		{"hint wifi no dice CUÁL wifi: solo no decide nada", ev("181.49.1.1", "", "wifi", 0),
			"unknown", "low", "no-router-reference"},
	}
	for _, c := range cases {
		v := classify(c.e)
		if v.Route != c.route || v.Confidence != c.confidence || v.Reason != c.reason {
			t.Errorf("%s: %+v, quería {%s %s %s}", c.name, v, c.route, c.confidence, c.reason)
		}
	}
}

// El caso que hacía invisible el problema: un celular midiendo por su PROPIA
// SIM que cae en la misma IP de CGNAT que el router (mismo operador, misma
// región) y sin pista del navegador (iPhone/Safari, Firefox Android). No se
// puede afirmar que salió por el router.
func TestClassifyCGNATNoAscendeALaVelocidadTitular(t *testing.T) {
	v := classify(ev("181.49.1.7", "181.49.1.7", "", time.Minute))
	if v.Confidence != "low" {
		t.Fatalf("%+v: una coincidencia de IP bajo CGNAT no puede pasar de confianza baja", v)
	}
	// store.ListDeviceSummaries solo acepta high/medium para la velocidad
	// titular; este es el veredicto que esa consulta tiene que dejar afuera.
	if v.Confidence == "medium" || v.Confidence == "high" {
		t.Error("alimentaría el número grande del dashboard")
	}
}

func TestClassifySeñalesQueSeContradicen(t *testing.T) {
	// La IP dice "es el router" y el celular dice "estoy en datos móviles".
	e := ev("190.145.155.7", "190.145.155.7", "cellular", time.Minute)
	if v := classify(e); v.Route != "unknown" || v.Reason != "conflicting-signals" {
		t.Fatalf("%+v, quería unknown/conflicting-signals", v)
	}
}

func TestClassifyPrioridades(t *testing.T) {
	base := ev("190.145.155.7", "190.145.155.7", "", time.Minute)

	byDevice := base
	byDevice.byDevice = true
	if v := classify(byDevice); v.Route != "router" || v.Confidence != "high" || v.Reason != "measured-by-device" {
		t.Errorf("byDevice: %+v", v)
	}
	// La corrió el propio equipo: eso no depende de ninguna IP, así que ni
	// siquiera un peer del túnel sin verificar le baja la confianza.
	byDeviceUnverified := byDevice
	byDeviceUnverified.obsStatus = statusProxyUnverified
	if v := classify(byDeviceUnverified); v.Confidence != "high" {
		t.Errorf("byDevice con peer sin verificar: %+v", v)
	}
	changed := base
	changed.changed = true
	if v := classify(changed); v.Route != "unknown" || v.Reason != "network-changed-mid-test" {
		t.Errorf("changed: %+v", v)
	}
	bad := base
	bad.obsStatus = "cgnat"
	if v := classify(bad); v.Route != "unknown" || v.Reason != "client-ip-cgnat" {
		t.Errorf("status: %+v", v)
	}
	forged := base
	forged.obsStatus = "forged-header"
	if v := classify(forged); v.Route != "unknown" || v.Reason != "client-ip-forged-header" {
		t.Errorf("cabecera inventada: %+v", v)
	}
	relay := base
	relay.obsASN = 13335 // Cloudflare WARP / iCloud Private Relay
	if v := classify(relay); v.Route != "unknown" || v.Reason != "relay-or-vpn" {
		t.Errorf("relay: %+v", v)
	}
	asnDiff := ev("181.49.1.1", "190.145.155.7", "", time.Minute)
	asnDiff.obsASN, asnDiff.refASN = 14080, 27831
	if v := classify(asnDiff); v.Route != "other-network" || v.Confidence != "high" {
		t.Errorf("asn: %+v", v)
	}
	// Mismo operador: el ASN igual no prueba nada, así que la coincidencia de
	// IP bajo CGNAT sigue siendo la única señal (y sigue siendo débil).
	asnSame := base
	asnSame.obsASN, asnSame.refASN = 14080, 14080
	if v := classify(asnSame); v.Confidence != "low" {
		t.Errorf("mismo asn no puede subir la confianza: %+v", v)
	}
}

func TestWithServerFieldsBorraLoQueMandaElCliente(t *testing.T) {
	in := []byte(`{"device_id":"router-1","net_route":"router","net_asn":1,"net":{"x":1},"client_ip":"1.2.3.4","net_cf_ip_start":"1.2.3.4","net_cf_ip_end":"5.6.7.8","down_mbps":123}`)
	out, err := withServerFields(in, netDropOnIngest, map[string]any{"net_route": "other-network"})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["net_route"] != "other-network" {
		t.Errorf("net_route = %v, el cliente le ganó al servidor", obj["net_route"])
	}
	for _, k := range []string{"net_asn", "net", "client_ip", "net_cf_ip_start", "net_cf_ip_end"} {
		if _, ok := obj[k]; ok {
			t.Errorf("%s debería haberse borrado del payload del cliente", k)
		}
	}
	if obj["down_mbps"] != float64(123) || obj["device_id"] != "router-1" {
		t.Errorf("se perdió un campo real de la medición: %v", obj)
	}
}

// withServerFields ahora corre sobre TODOS los ítems del POST (a los que no
// participan del esquema solo se les borran las claves reservadas), así que
// vuelve a serializar el JSON de notion5g.py y del agente del router. Los
// números tienen que quedar guardados tal cual: sin UseNumber(), un uptime_s
// grande se guardaría redondeado y nadie lo notaría.
func TestWithServerFieldsPreservaLosNumeros(t *testing.T) {
	in := []byte(`{"device_id":"router-1","uptime_s":1234567890123456789,"down_mbps":123.456789,"rsrp_dbm":-97.5}`)
	out, err := withServerFields(in, netDropOnIngest, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"uptime_s":1234567890123456789`, `"down_mbps":123.456789`, `"rsrp_dbm":-97.5`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("falta %s en %s", want, out)
		}
	}
	// Y sigue siendo estricto con la basura, como json.Unmarshal.
	if _, err := withServerFields([]byte(`{"device_id":"x"} basura`), netDropOnIngest, nil); err == nil {
		t.Error("json con contenido de más debería fallar")
	}
}

func TestLedger(t *testing.T) {
	l := newObsLedger()
	t0 := time.Now()
	id := "abcd1234-ef56"
	l.observe(id, netip.MustParseAddr("190.145.155.7"), "ok", t0)
	l.observe(id, netip.MustParseAddr("181.49.1.1"), "ok", t0.Add(2*time.Second))
	o, ok := l.take(id)
	if !ok || o.first.String() != "190.145.155.7" || o.last.String() != "181.49.1.1" || o.n != 2 {
		t.Fatalf("take = %+v ok=%v", o, ok)
	}
	if _, ok := l.take(id); ok {
		t.Error("take debería borrar la entrada")
	}
	l.observe("corto", netip.MustParseAddr("1.1.1.1"), "ok", t0) // test_id inválido
	if _, ok := l.take("corto"); ok {
		t.Error("un test_id inválido no se debe usar como clave")
	}
	// La ventana hereda el peor status visto: si una de las peticiones llegó
	// con el peer del túnel sin verificar, no se puede guardar como "ok".
	id2 := "abcd1234-ef57"
	l.observe(id2, netip.MustParseAddr("190.145.155.7"), "ok", t0)
	l.observe(id2, netip.MustParseAddr("190.145.155.7"), statusProxyUnverified, t0.Add(time.Second))
	if o, _ := l.take(id2); o.status != statusProxyUnverified {
		t.Errorf("status de la ventana = %q, quería proxy-unverified", o.status)
	}
	// Una cabecera inventada no entra al ledger.
	l.observe("abcd1234-ef58", netip.MustParseAddr("1.1.1.1"), "forged-header", t0)
	if _, ok := l.take("abcd1234-ef58"); ok {
		t.Error("una observación con cabecera inventada no se debe anotar")
	}
}

func TestCymruOriginName(t *testing.T) {
	v4, ok := cymruOriginName(netip.MustParseAddr("181.49.1.1"))
	if !ok || v4 != "1.1.49.181.origin.asn.cymru.com" {
		t.Errorf("v4 = %q ok=%v", v4, ok)
	}
	// Nibbles invertidos, NO bytes invertidos (invertir bytes da NXDOMAIN).
	v6, ok := cymruOriginName(netip.MustParseAddr("2001:4860:4860::8888"))
	want := "8.8.8.8.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.6.8.4.0.6.8.4.1.0.0.2.origin6.asn.cymru.com"
	if !ok || v6 != want {
		t.Errorf("v6 = %q\nquería %q", v6, want)
	}
	if _, ok := cymruOriginName(netip.MustParseAddr("192.168.1.1")); ok {
		t.Error("una IP privada no se consulta")
	}
}

func TestPickMostSpecific(t *testing.T) {
	// El orden de los TXT en DNS no está garantizado: gana el más específico.
	txts := []string{
		"14080 | 181.49.0.0/19 | CO | lacnic | 2011-05-02",
		"14080 | 181.49.0.0/21 | CO | lacnic | 2011-05-02",
	}
	info, ok := pickMostSpecific(txts)
	if !ok || info.Prefix != "181.49.0.0/21" || info.ASN != 14080 || info.CC != "CO" {
		t.Fatalf("%+v ok=%v", info, ok)
	}
	// Mismo resultado al revés.
	info2, _ := pickMostSpecific([]string{txts[1], txts[0]})
	if info2.Prefix != info.Prefix {
		t.Errorf("el resultado depende del orden de los TXT: %q vs %q", info.Prefix, info2.Prefix)
	}
}

func TestParseASNName(t *testing.T) {
	got := parseASNName("14080 | CO | lacnic | 1999-10-15 | AS14080 - Telmex Colombia S.A., CO", 14080)
	if got != "Telmex Colombia S.A." {
		t.Errorf("= %q", got)
	}
	got = parseASNName("27831 | CO | lacnic | 2003-01-01 | COLOMBIA MOVIL, CO", 27831)
	if got != "COLOMBIA MOVIL" {
		t.Errorf("= %q", got)
	}
}

func TestNormalizeHint(t *testing.T) {
	var h any
	_ = json.Unmarshal([]byte(`{"api":"network-information","type":"cellular"}`), &h)
	if normalizeHint(h) != "cellular" {
		t.Error("hint confiable ignorado")
	}
	_ = json.Unmarshal([]byte(`{"api":"present-untrusted","type":"wifi"}`), &h)
	if normalizeHint(h) != "" {
		t.Error("un hint no confiable no se debe usar")
	}
	_ = json.Unmarshal([]byte(`{"api":"unavailable","type":null}`), &h)
	if normalizeHint(h) != "" {
		t.Error("sin API no hay hint")
	}
}

func TestDeclaredMatches(t *testing.T) {
	if declaredMatches("", "router") != nil {
		t.Error("sin declaración no hay comparación")
	}
	if declaredMatches("router", "router") != true {
		t.Error("router/router")
	}
	if declaredMatches("phone-cellular", "router") != false {
		t.Error("declaró celular y salió por el router: no coincide")
	}
	if declaredMatches("other-wifi", "other-network") != true {
		t.Error("other-wifi/other-network")
	}
}
