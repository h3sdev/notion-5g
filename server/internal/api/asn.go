package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// asnInfo: a qué operador pertenece una IP de salida. Sale de Team Cymru por
// DNS -- gratis, sin API key y sin agregar una sola dependencia al go.mod.
type asnInfo struct {
	ASN    int    // 14080
	Name   string // razón social ya recortada: "Telmex Colombia S.A."
	Prefix string // "190.145.128.0/19"
	CC     string // país DEL REGISTRO DEL PREFIJO, no de la medición
}

const (
	asnLookupTimeout = 2 * time.Second  // medido: 90-180 ms en frío, 0 ms cacheado
	asnCacheTTL      = 6 * time.Hour    //
	asnNegativeTTL   = 10 * time.Minute // NXDOMAIN = la IP no está en BGP; no reintentar en loop
	asnNameTTL       = 24 * time.Hour
	asnCacheMax      = 4096
)

// relayASNs: egresos típicos de iCloud Private Relay y de CDNs usados como
// salida. Es una heurística, no una lista autoritativa. Si la IP del cliente
// pertenece a uno, la medición NO se puede atribuir a ninguna red móvil: un
// iPhone con Private Relay daría "otra red" SIEMPRE, incluso pegado al WiFi
// del router, y eso es peor que no clasificar porque parece un dato.
// OJO: Private Relay preserva el país, así que CF-IPCountry=CO no lo descarta.
var relayASNs = map[int]bool{13335: true, 20940: true, 54113: true}

type asnCacheEntry struct {
	info asnInfo
	ok   bool
	exp  time.Time
}

type asnNameEntry struct {
	name string
	exp  time.Time
}

type asnResolver struct {
	res *net.Resolver

	mu    sync.Mutex
	cache map[netip.Prefix]asnCacheEntry // clave = prefijo de privacidad (/24 o /48)
	names map[int]asnNameEntry
}

func newASNResolver() *asnResolver {
	return &asnResolver{
		res:   &net.Resolver{},
		cache: map[netip.Prefix]asnCacheEntry{},
		names: map[int]asnNameEntry{},
	}
}

// Lookup resuelve el operador de una IP. Nunca es fatal: si el DNS falla,
// devuelve (asnInfo{}, false) y la clasificación sigue con una señal menos.
func (r *asnResolver) Lookup(ctx context.Context, a netip.Addr) (asnInfo, bool) {
	a = normalizeAddr(a)
	if !a.IsValid() || !isPublicAddr(a) {
		return asnInfo{}, false
	}
	key := privacyPrefix(a)
	if !key.IsValid() {
		return asnInfo{}, false
	}
	if e, hit := r.getCached(key); hit {
		return e.info, e.ok
	}

	name, ok := cymruOriginName(a)
	if !ok {
		return asnInfo{}, false
	}
	lctx, cancel := context.WithTimeout(ctx, asnLookupTimeout)
	txts, err := r.res.LookupTXT(lctx, name)
	cancel()
	if err != nil {
		// NXDOMAIN es definitivo (la IP no está anunciada en BGP): se cachea
		// negativo un rato para no repetir la consulta en cada medición. Un
		// timeout o un error de red es transitorio: no se cachea.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			r.putCached(key, asnCacheEntry{ok: false, exp: time.Now().Add(asnNegativeTTL)})
		}
		return asnInfo{}, false
	}

	info, ok := pickMostSpecific(txts)
	if !ok {
		r.putCached(key, asnCacheEntry{ok: false, exp: time.Now().Add(asnNegativeTTL)})
		return asnInfo{}, false
	}
	info.Name = r.asnName(ctx, info.ASN)
	r.putCached(key, asnCacheEntry{info: info, ok: true, exp: time.Now().Add(asnCacheTTL)})
	return info, true
}

func (r *asnResolver) getCached(key netip.Prefix) (asnCacheEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[key]
	if !ok || time.Now().After(e.exp) {
		return asnCacheEntry{}, false
	}
	return e, true
}

func (r *asnResolver) putCached(key netip.Prefix, e asnCacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= asnCacheMax {
		now := time.Now()
		for k, v := range r.cache {
			if now.After(v.exp) {
				delete(r.cache, k)
			}
		}
		// Si después de purgar vencidas sigue llena, se vacía entera: es un
		// caché de conveniencia, perderlo cuesta un round trip de DNS, no un
		// dato. Mucho más barato que mantener un LRU acá.
		if len(r.cache) >= asnCacheMax {
			r.cache = map[netip.Prefix]asnCacheEntry{}
		}
	}
	r.cache[key] = e
}

// asnName resuelve la razón social del operador. Vive en su propio caché
// porque cambia mucho menos seguido que la asignación IP->ASN, y porque un
// mismo ASN aparece en miles de prefijos distintos.
func (r *asnResolver) asnName(ctx context.Context, asn int) string {
	if asn <= 0 {
		return ""
	}
	r.mu.Lock()
	e, ok := r.names[asn]
	r.mu.Unlock()
	if ok && time.Now().Before(e.exp) {
		return e.name
	}
	lctx, cancel := context.WithTimeout(ctx, asnLookupTimeout)
	txts, err := r.res.LookupTXT(lctx, fmt.Sprintf("AS%d.asn.cymru.com", asn))
	cancel()
	name := ""
	if err == nil && len(txts) > 0 {
		name = parseASNName(txts[0], asn)
	}
	if name == "" && err != nil {
		return "" // error transitorio: no cachear un vacío por 24 h
	}
	r.mu.Lock()
	if len(r.names) >= asnCacheMax {
		r.names = map[int]asnNameEntry{}
	}
	r.names[asn] = asnNameEntry{name: name, exp: time.Now().Add(asnNameTTL)}
	r.mu.Unlock()
	return name
}

// cymruOriginName arma el FQDN a consultar para una IP.
//   - v4: octetos invertidos + ".origin.asn.cymru.com"
//     -> "1.1.49.181.origin.asn.cymru.com"
//   - v6: NIBBLES del formato expandido invertidos (32 etiquetas: nibble bajo,
//     después nibble alto, por byte, de atrás para adelante) + ".origin6.asn.cymru.com".
//
// No es invertir bytes: invertir bytes da NXDOMAIN, o sea un falso "no se pudo
// identificar la red" que nadie nota.
func cymruOriginName(a netip.Addr) (string, bool) {
	a = normalizeAddr(a)
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return "", false
	}
	if a.Is4() {
		b := a.As4()
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", b[3], b[2], b[1], b[0]), true
	}
	b := a.As16()
	var sb strings.Builder
	for i := 15; i >= 0; i-- {
		sb.WriteString(strconv.FormatUint(uint64(b[i]&0x0f), 16))
		sb.WriteByte('.')
		sb.WriteString(strconv.FormatUint(uint64(b[i]>>4), 16))
		sb.WriteByte('.')
	}
	sb.WriteString("origin6.asn.cymru.com")
	return sb.String(), true
}

// pickMostSpecific parsea TODOS los TXT y se queda con el prefijo de máscara
// más larga.
//
// Trampa verificada: una misma IP puede devolver varios TXT con prefijos
// superpuestos (181.49.1.1 devuelve /19 y /21) y el orden de los TXT en DNS no
// está garantizado. Un txt[0] a secas hace que el operador identificado salga
// distinto entre dos corridas del mismo dato.
func pickMostSpecific(txts []string) (asnInfo, bool) {
	best := asnInfo{}
	bestBits := -1
	for _, t := range txts {
		info, ok := parseCymruOrigin(t)
		if !ok {
			continue
		}
		bits := prefixBits(info.Prefix)
		if bits > bestBits {
			best, bestBits = info, bits
		}
	}
	return best, bestBits >= 0
}

// parseCymruOrigin: "14080 | 190.145.128.0/19 | CO | lacnic | 2005-02-18".
// El primer campo puede traer varios ASN (prefijo con origen múltiple); en ese
// caso se toma el primero, que es lo que hace también el whois de Cymru.
func parseCymruOrigin(txt string) (asnInfo, bool) {
	parts := strings.Split(txt, "|")
	if len(parts) < 2 {
		return asnInfo{}, false
	}
	asns := strings.Fields(strings.TrimSpace(parts[0]))
	if len(asns) == 0 {
		return asnInfo{}, false
	}
	n, err := strconv.Atoi(asns[0])
	if err != nil || n <= 0 {
		return asnInfo{}, false
	}
	info := asnInfo{ASN: n, Prefix: strings.TrimSpace(parts[1])}
	if len(parts) >= 3 {
		info.CC = strings.ToUpper(strings.TrimSpace(parts[2]))
	}
	return info, true
}

// parseASNName: "14080 | CO | lacnic | 1999-10-15 | AS14080 - Telmex Colombia S.A., CO"
// -> "Telmex Colombia S.A.". Se recorta el prefijo "AS<n> - " y el sufijo ", CC".
func parseASNName(txt string, asn int) string {
	parts := strings.Split(txt, "|")
	if len(parts) < 5 {
		return ""
	}
	name := strings.TrimSpace(parts[4])
	name = strings.TrimPrefix(name, fmt.Sprintf("AS%d - ", asn))
	if i := strings.LastIndex(name, ", "); i > 0 {
		if cc := name[i+2:]; len(cc) == 2 && cc == strings.ToUpper(cc) {
			name = name[:i]
		}
	}
	return strings.TrimSpace(name)
}

func prefixBits(s string) int {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return p.Bits()
}
