package api

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// cgnat es 100.64.0.0/10 (RFC 6598). netip.Addr.IsPrivate() NO lo cubre, y es
// justo el rango que usan los operadores móviles colombianos por dentro: si
// aparece como IP "pública" es que algo se interpuso, no sirve para ASN.
var (
	cgnat  = netip.MustParsePrefix("100.64.0.0/10")
	classE = netip.MustParsePrefix("240.0.0.0/4") // incl. pseudo-IPv4 de Cloudflare
	ula    = netip.MustParsePrefix("fc00::/7")    // = IsPrivate() en v6, defensivo
	v6doc  = netip.MustParsePrefix("2001:db8::/32")
)

// trustedProxy resuelve periódicamente el nombre del contenedor de cloudflared
// para saber qué peer tiene permitido mandarnos CF-Connecting-IP.
//
// Por qué hace falta: el contenedor del backend no publica puerto, pero SÍ es
// alcanzable desde el propio host del VPS (verificado: curl a 172.27.0.2:8080
// responde 200) y este host es compartido con ~20 contenedores de otros
// proyectos. O sea que la cabecera solo se puede creer si la conexión vino del
// túnel. No hace falta la lista de rangos de Cloudflare: alcanza con comparar
// contra el peer del túnel, que es el único que puede llegar por esa ruta.
type trustedProxy struct {
	host string

	mu         sync.Mutex
	addrs      []netip.Addr
	checked    time.Time
	everOK     bool
	refreshing bool
	warnOnce   sync.Once
}

const (
	// proxyRecheck: cada cuánto se vuelve a resolver el nombre del contenedor.
	// Docker le puede cambiar la IP al reiniciarse la pila, y cachearla para
	// siempre haría que un día el backend deje de creerle al túnel en silencio.
	proxyRecheck = 60 * time.Second
	// proxyLookupTimeout acota la consulta al DNS interno de docker (127.0.0.11),
	// que es local y responde en microsegundos. El tope existe para que un DNS
	// caído no bloquee el arranque del servidor.
	proxyLookupTimeout = 2 * time.Second
)

func newTrustedProxy() *trustedProxy {
	host := strings.TrimSpace(os.Getenv("TRUSTED_PROXY_HOST"))
	if host == "" {
		host = "notion5g-cloudflared"
	}
	p := &trustedProxy{host: host}
	p.checked = time.Now()
	p.refresh() // una vez, en el arranque, antes de escuchar
	return p
}

func (p *trustedProxy) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), proxyLookupTimeout)
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, p.host)
	cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshing = false
	if err != nil || len(ips) == 0 {
		return // se conserva lo último que sí resolvió
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip.IP); ok {
			addrs = append(addrs, normalizeAddr(a))
		}
	}
	if len(addrs) == 0 {
		return
	}
	p.addrs = addrs
	p.everOK = true
}

// is dice si el peer de esta conexión es el contenedor del túnel.
//
// La revalidación va en segundo plano a propósito: esto corre en el camino de
// CADA petición (incluido el heartbeat de cada equipo en campo), y una consulta
// de DNS ahí adentro le sumaría latencia a la ingesta. Mientras se revalida se
// responde con lo último que resolvió, que es lo correcto el 99.9% del tiempo.
func (p *trustedProxy) is(peer netip.Addr) bool {
	p.mu.Lock()
	if time.Since(p.checked) > proxyRecheck && !p.refreshing {
		p.refreshing = true
		p.checked = time.Now()
		go p.refresh()
	}
	defer p.mu.Unlock()
	for _, a := range p.addrs {
		if a == peer {
			return true
		}
	}
	return false
}

// everResolved dice si el nombre del contenedor resolvió alguna vez. Si nunca
// resolvió (otro despliegue, otro nombre, docker sin DNS interno) NO apagamos
// la detección en silencio: se acepta la cabecera igual, pero marcada como
// "proxy-unverified" para que quede visible en los datos y en el dashboard.
func (p *trustedProxy) everResolved() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.everOK
}

func (p *trustedProxy) warn() {
	p.warnOnce.Do(func() {
		log.Printf("clientip: no se pudo resolver %q (el contenedor del túnel); "+
			"se acepta CF-Connecting-IP sin verificar el peer: se sigue aprendiendo la IP "+
			"pública de cada equipo y clasificando, pero toda clasificación queda con "+
			"client_ip_status=proxy-unverified y confianza baja (no alimenta la velocidad "+
			"titular del equipo). Revisá TRUSTED_PROXY_HOST / el container_name de cloudflared",
			p.host)
	})
}

// statusProxyUnverified: la cabecera llegó pero no se pudo confirmar que venga
// del túnel, porque el nombre del contenedor de cloudflared nunca resolvió.
const statusProxyUnverified = "proxy-unverified"

// usableClientIP dice con qué status vale la pena USAR la IP observada.
//
// "proxy-unverified" entra a propósito: si se descartara, un cambio de nombre
// del contenedor del túnel apagaría la detección entera en silencio y, peor,
// nunca se aprendería la IP de referencia de ningún equipo, así que no se
// recuperaría sola ni cuando el DNS volviera. Se usa, pero classify() le baja
// la confianza a "low" y con eso no alimenta la velocidad titular del equipo.
func usableClientIP(status string) bool {
	return status == "ok" || status == statusProxyUnverified
}

// clientIP devuelve la IP pública del cliente tal como la vio el borde de
// Cloudflare, y un status que SIEMPRE se guarda: un día entero de campo sin
// clasificar y sin saber por qué es peor que no clasificar.
//
// status ∈ "ok" | "absent" | "private" | "cgnat" | "invalid" |
// "proxy-unverified" | "forged-header". Los dos últimos NO son lo mismo:
//   - "forged-header": el nombre del túnel resuelve y este peer no es: la
//     cabecera la inventó alguien. La dirección se descarta.
//   - "proxy-unverified": el nombre del túnel NUNCA resolvió, así que no hay
//     con qué comparar. La dirección se devuelve y se usa (ver usableClientIP),
//     pero la clasificación queda con confianza "low".
//
// Precedencia:
//  1. Si r.RemoteAddr no es el peer confiable y llega la cabecera -> no se lee:
//     cualquiera con acceso al host podría inventarla.
//  2. CF-Connecting-IP presente y peer confiable: parsear, normalizar y validar
//     que sea pública. Si es privada/CGNAT/basura, se devuelve el status
//     correspondiente y NUNCA se cae a RemoteAddr (caer registraría la IP del
//     contenedor de cloudflared, 172.27.0.3, como "la IP del cliente").
//  3. Cabecera ausente y RemoteAddr público (desarrollo local sin túnel) -> ok.
//
// NO se usa X-Forwarded-For: el cliente puede mandarla y cloudflared le hace
// append (no prepend) de la IP real, así que el primer elemento es basura
// controlada por el cliente.
func (s *Server) clientIP(r *http.Request) (netip.Addr, string) {
	peer, peerOK := parseHostAddr(r.RemoteAddr)
	trusted := peerOK && s.proxy.is(peer)
	h := strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))

	if h == "" {
		if trusted {
			return netip.Addr{}, "absent" // el túnel debería haberla puesto siempre
		}
		if peerOK && isPublicAddr(peer) {
			return peer, "ok" // dev local sin túnel: la conexión es directa
		}
		return netip.Addr{}, "absent"
	}

	status := "ok"
	if !trusted {
		if s.proxy.everResolved() {
			// El nombre resuelve, y este peer no es. Cabecera inventada: acá sí
			// se descarta la dirección, no se degrada nada.
			return netip.Addr{}, "forged-header"
		}
		// Nunca resolvió: degradamos a lo que hay hoy, pero marcado. El valor
		// se sigue usando (ver usableClientIP) con confianza tope "low".
		s.proxy.warn()
		status = statusProxyUnverified
	}

	// Defensivo: el borde manda un solo valor; si llegara una lista, el
	// último es el que agregó el proxy más cercano.
	if i := strings.LastIndexByte(h, ','); i >= 0 {
		h = h[i+1:]
	}
	addr, ok := parseHostAddr(h)
	if !ok {
		return netip.Addr{}, "invalid"
	}
	switch {
	case cgnat.Contains(addr):
		return netip.Addr{}, "cgnat"
	case addr.IsPrivate() || ula.Contains(addr) || addr.IsLoopback() || addr.IsLinkLocalUnicast():
		return netip.Addr{}, "private"
	case !isPublicAddr(addr):
		return netip.Addr{}, "invalid"
	}
	return addr, status
}

// parseHostAddr acepta "1.2.3.4", "1.2.3.4:5678", "[2800::1]:443", "2800::1",
// "::ffff:1.2.3.4" y "fe80::1%eth0". Normaliza a v4 nativa y sin zona.
func parseHostAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return normalizeAddr(ap.Addr()), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.Trim(s, "[]")
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return normalizeAddr(addr), true
}

// normalizeAddr deja ::ffff:1.2.3.4 como 1.2.3.4 (si no, Cymru y el dashboard
// ven dos IPs distintas para el mismo cliente) y borra la zona (%eth0).
func normalizeAddr(a netip.Addr) netip.Addr { return a.Unmap().WithZone("") }

// isPublicAddr es "enrutable en Internet y con dueño en un RIR": lo único que
// tiene sentido guardar como client_ip y mandar a Team Cymru.
func isPublicAddr(a netip.Addr) bool {
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsPrivate() {
		return false
	}
	return !cgnat.Contains(a) && !classE.Contains(a) && !ula.Contains(a) && !v6doc.Contains(a)
}

// privacyPrefix reduce la IP a /24 (v4) o /48 (v6): es lo único que se
// PERSISTE de la IP de un celular. La comparación exacta se hace en memoria
// durante el request. Ley 1581/2012: la IP de quien corre la prueba es dato
// personal y acá se guardaría para siempre en raw.
//
// La excepción es device_egress.ip, que sí guarda la dirección exacta: es la
// IP del CPE bajo prueba (no de una persona), es el discriminador, y es un
// último-valor que se sobreescribe cada minuto.
func privacyPrefix(a netip.Addr) netip.Prefix {
	a = normalizeAddr(a)
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// privacyPrefixStr es privacyPrefix lista para guardar/mostrar ("" si no aplica).
func privacyPrefixStr(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	p := privacyPrefix(a)
	if !p.IsValid() {
		return ""
	}
	return p.String()
}

func ipFamily(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	if normalizeAddr(a).Is4() {
		return "v4"
	}
	return "v6"
}

// cfCountry es el país que ya manda el borde (ip_geolocation = on en la zona),
// útil como sanity-check: una medición de campo en Colombia con cf_ipcountry
// distinto de CO es una VPN, no una red móvil colombiana.
func cfCountry(r *http.Request) string {
	c := strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
	if len(c) != 2 || c == "XX" || c == "T1" { // XX = desconocida, T1 = Tor
		return ""
	}
	return c
}
