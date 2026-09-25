package api

import (
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// testServer arma un Server con un proxy confiable fijo, para no depender del
// DNS de Docker en los tests.
func testServer(trusted string) *Server {
	p := &trustedProxy{host: "test"}
	if trusted != "" {
		a := netip.MustParseAddr(trusted)
		p.addrs = []netip.Addr{a}
		p.everOK = true
	}
	p.checked = farFuture()
	return &Server{proxy: p}
}

func req(cf, remote string) *http.Request {
	r := &http.Request{Header: http.Header{}, RemoteAddr: remote}
	if cf != "" {
		r.Header.Set("CF-Connecting-IP", cf)
	}
	return r
}

func TestClientIPTrustedPeer(t *testing.T) {
	s := testServer("172.27.0.3")
	cases := []struct{ cf, remote, want, wantStatus string }{
		{"190.145.130.7", "172.27.0.3:41234", "190.145.130.7", "ok"},
		{"2803:a3e0:1::5", "172.27.0.3:41234", "2803:a3e0:1::5", "ok"},
		{"::ffff:190.145.130.7", "172.27.0.3:41234", "190.145.130.7", "ok"},
		{"[2803:a3e0:1::5]:443", "172.27.0.3:41234", "2803:a3e0:1::5", "ok"},
		{"1.1.1.1, 190.145.130.7", "172.27.0.3:41234", "190.145.130.7", "ok"},
		{"10.0.0.1", "172.27.0.3:41234", "", "private"},            // privada inyectada
		{"100.100.1.1", "172.27.0.3:41234", "", "cgnat"},           // CGNAT: no es del cliente
		{"240.1.2.3", "172.27.0.3:41234", "", "invalid"},           // pseudo-IPv4 / class E
		{"no-es-ip", "172.27.0.3:41234", "", "invalid"},            // basura -> NO cae a RemoteAddr
		{"", "172.27.0.3:41234", "", "absent"},                     // el túnel no la puso
		{"190.145.130.7", "203.0.113.9:5555", "", "forged-header"}, // peer que no es el túnel
		{"", "201.234.1.9:5555", "201.234.1.9", "ok"},              // curl directo, sin túnel
		{"", "127.0.0.1:8080", "", "absent"},                       // dev local
		{"", "[fe80::1%eth0]:80", "", "absent"},                    // link-local con zona
		{"", "", "", "absent"},
	}
	for _, c := range cases {
		addr, status := s.clientIP(req(c.cf, c.remote))
		got := ""
		if addr.IsValid() {
			got = addr.String()
		}
		if got != c.want || status != c.wantStatus {
			t.Errorf("cf=%q remote=%q -> (%q,%q), quería (%q,%q)", c.cf, c.remote, got, status, c.want, c.wantStatus)
		}
	}
}

// Si el nombre del contenedor del túnel NUNCA resolvió, no apagamos la
// detección en silencio: se acepta la cabecera pero marcada, y ese status
// TIENE que seguir siendo utilizable (si no, la función se apaga sola y no se
// recupera nunca, porque tampoco se aprendería la IP de referencia del equipo).
func TestClientIPProxyNuncaResuelto(t *testing.T) {
	s := testServer("")
	addr, status := s.clientIP(req("190.145.130.7", "172.27.0.3:41234"))
	if addr.String() != "190.145.130.7" || status != statusProxyUnverified {
		t.Fatalf("degradación: (%v,%q), quería (190.145.130.7, proxy-unverified)", addr, status)
	}
	if !usableClientIP(status) {
		t.Error("proxy-unverified se descarta en todos lados: la degradación no degrada, apaga")
	}
	// Pero nunca por encima de confianza baja.
	e := ev("190.145.155.7", "190.145.155.7", "wifi", time.Minute)
	e.obsStatus = status
	if v := classify(e); v.Route != "router" || v.Confidence != "low" {
		t.Errorf("classify con peer sin verificar = %+v, quería router/low", v)
	}
}

// Una cabecera de un peer que NO es el túnel (y el túnel sí resuelve) es una
// cabecera inventada: esa no se usa nunca.
func TestClientIPCabeceraInventada(t *testing.T) {
	if usableClientIP("forged-header") {
		t.Error("una cabecera inventada no se puede usar")
	}
}

func TestPrivacyPrefix(t *testing.T) {
	cases := []struct{ in, want, family string }{
		{"190.145.155.77", "190.145.155.0/24", "v4"},
		{"2803:a3e0:1:2:3:4:5:6", "2803:a3e0:1::/48", "v6"},
		{"::ffff:190.145.155.77", "190.145.155.0/24", "v4"},
	}
	for _, c := range cases {
		a := netip.MustParseAddr(c.in)
		if got := privacyPrefixStr(a); got != c.want {
			t.Errorf("privacyPrefix(%s) = %q, quería %q", c.in, got, c.want)
		}
		if got := ipFamily(a); got != c.family {
			t.Errorf("ipFamily(%s) = %q, quería %q", c.in, got, c.family)
		}
	}
}

func TestCFCountry(t *testing.T) {
	for _, c := range []struct{ in, want string }{{"CO", "CO"}, {"co", "CO"}, {"XX", ""}, {"T1", ""}, {"", ""}, {"COL", ""}} {
		r := &http.Request{Header: http.Header{}}
		if c.in != "" {
			r.Header.Set("CF-IPCountry", c.in)
		}
		if got := cfCountry(r); got != c.want {
			t.Errorf("cfCountry(%q) = %q, quería %q", c.in, got, c.want)
		}
	}
}
