package api

// Identificación automática de la ruta de salida de una prueba.
//
// El problema: el dashboard puede correr una prueba desde el navegador del
// celular. Si ese celular está en el WiFi del router, la prueba mide el router.
// Si está por datos móviles, mide la SIM del celular -- pero la medición se
// guardaba igual contra el device_id del router, contaminada y sin forma de
// saberlo después.
//
// La solución NO es una señal autoritativa (no existe): son tres señales
// independientes, ninguna concluyente sola.
//
//	IP   - la IP pública del cliente vs. la del router (la ve el backend).
//	       Prueba que salió (o no) por la misma IP; no prueba nada bajo CGNAT
//	       compartido ni si las familias difieren (v4 vs v6).
//	hint - navigator.connection.type (solo Android/Chrome). Si dice "cellular",
//	       NO salió por el WiFi del router. Si dice "wifi" no dice CUÁL wifi:
//	       como positivo es inútil.
//	ASN  - el operador de la IP de salida (Team Cymru, asíncrono). Si difiere
//	       del ASN del router, es otra red; si los dos son Movistar, nada.
//
// Regla: dos señales concordantes -> alta; una sola -> media; cero o
// contradicción -> "unknown". Nunca "router" por defecto y nunca el campo
// vacío: el valor honesto es "unknown".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"

	"notion5g/server/internal/store"
)

// ------------------------------------------------------------------ ledger de observaciones

const (
	// obsTTL: una prueba de navegador dura decenas de segundos; 10 minutos es
	// margen de sobra para el POST que la cierra. Pasado eso, la entrada es
	// basura (el celular se quedó sin datos a mitad y nunca posteó).
	obsTTL = 10 * time.Minute
	// obsMaxEntries: tope duro para que un cliente que mande test_id nuevos en
	// loop no haga crecer el mapa sin límite. Más allá se ignora en silencio:
	// perder la correlación degrada a "obs = la IP del POST", que es lo que
	// había antes de esto, no un error.
	obsMaxEntries = 512
)

// testObs son las IPs vistas DURANTE la ventana de medición de una prueba.
type testObs struct {
	first, last     netip.Addr
	firstAt, lastAt time.Time
	n               int
	// status es el peor status con que se vio la ventana: si alguna de las
	// peticiones llegó con el peer del túnel sin verificar, la ventana entera
	// hereda esa duda. Sin esto, tomar la observación de la ventana borraría
	// la degradación que hace classify() con "proxy-unverified".
	status string
}

// obsLedger ata las tres peticiones de una misma prueba local (download,
// upload y el POST), que hoy no tienen nada en común: el `ts` lo pone el reloj
// del celular y puede estar mal, así que no sirve para correlacionar.
//
// Vive SOLO en memoria: guarda direcciones exactas, y a disco solo va el
// prefijo. Se pierde al reiniciar el proceso, y está bien: una prueba a mitad
// de camino cuando el backend se reinicia se clasifica con la IP del POST.
type obsLedger struct {
	mu sync.Mutex
	m  map[string]*testObs
}

func newObsLedger() *obsLedger { return &obsLedger{m: map[string]*testObs{}} }

func (l *obsLedger) observe(testID string, ip netip.Addr, status string, at time.Time) {
	if !validTestID(testID) || !ip.IsValid() || !usableClientIP(status) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeLocked(at)
	o := l.m[testID]
	if o == nil {
		if len(l.m) >= obsMaxEntries {
			return
		}
		o = &testObs{first: ip, firstAt: at, status: status}
		l.m[testID] = o
	}
	if status == statusProxyUnverified {
		o.status = status
	}
	o.last = ip
	o.lastAt = at
	o.n++
}

// take devuelve las observaciones de una prueba y borra la entrada: el POST
// llega una sola vez y el ledger no es un historial.
func (l *obsLedger) take(testID string) (testObs, bool) {
	if !validTestID(testID) {
		return testObs{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.m[testID]
	if !ok {
		return testObs{}, false
	}
	delete(l.m, testID)
	return *o, true
}

// purgeLocked limpia entradas vencidas. Oportunista (en cada observe) en vez
// de por timer: no hace falta una goroutine viva para un mapa de 512 entradas.
func (l *obsLedger) purgeLocked(now time.Time) {
	for k, o := range l.m {
		if now.Sub(o.lastAt) > obsTTL {
			delete(l.m, k)
		}
	}
}

// validTestID valida antes de usar el valor como clave de un mapa: lo elige el
// cliente. ^[a-zA-Z0-9-]{8,64}$
func validTestID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// ------------------------------------------------------------------ evidencia y veredicto

type netEvidence struct {
	obs       netip.Addr
	obsSource string // "measure-window" | "post" | "none"
	obsStatus string // el de clientIP()
	obsAt     time.Time
	changed   bool // cambió la ruta durante la prueba
	ref       store.DeviceEgress
	refOK     bool
	refAge    time.Duration
	hint      string // "cellular" | "wifi" | ""  (normalizado de net_hint)
	obsASN    int    // 0 = todavía desconocido (lo llena el worker)
	refASN    int
	byDevice  bool // la prueba la corrió el propio equipo
}

// netMeta es lo que NO entra en la decisión pero sí se guarda como evidencia
// en raw.net, para poder auditar después por qué salió lo que salió.
type netMeta struct {
	country     string
	cfRay       string
	declared    string // lo que el usuario eligió en el dashboard
	cfIPChanged bool
	postedAt    time.Time
	obsASNName  string
	refASNName  string
	asnStatus   string // "pending" | "ok" | "not-resolved" | "no-ip"
}

type netVerdict struct{ Route, Confidence, Reason string }

// netSignal es una señal ya interpretada: a qué ruta apunta, por qué, y si
// alcanza por sí sola para afirmarla.
//
// La distinción fuerte/débil es lo que arregla la incoherencia que tenía la
// primera versión: descartaba el "mismo /24" por ser el pool de CGNAT del
// operador y al mismo tiempo aceptaba la IP EXACTAMENTE igual como prueba de
// "router". Bajo el CGNAT móvil que usan todos estos equipos, que dos
// suscriptores del mismo operador caigan en la misma IP pública es el diseño
// de la red, no una coincidencia imposible: es evidencia, pero no es prueba.
type netSignal struct {
	route  string // "router" | "other-network" ("" = no hay señal)
	reason string
	weak   bool // ambigua: sola no confirma, solo suma
}

func classify(e netEvidence) netVerdict {
	// La corrió el propio equipo (cierre de comando): no depende de ninguna IP,
	// así que no la afecta nada de lo de abajo.
	if e.byDevice {
		return netVerdict{"router", "high", "measured-by-device"}
	}
	v := classifySignals(e)
	// Peer del túnel sin verificar (ver clientIP): la IP del cliente salió de
	// una cabecera que no se pudo confirmar que venga de cloudflared. Se
	// clasifica igual -- apagar la detección entera en silencio es peor, y sin
	// aprender nada nunca se recuperaría sola -- pero nunca por encima de
	// "low", que además impide que la medición alimente la velocidad titular
	// del equipo (ver ListDeviceSummaries).
	if e.obsStatus == statusProxyUnverified && v.Route != "unknown" {
		v.Confidence = "low"
	}
	return v
}

// classifySignals junta las señales disponibles y decide.
//
//	0 señales               -> unknown (la razón la da el intento de IP)
//	señales que se oponen   -> unknown
//	1 señal fuerte          -> medium     (2 fuertes, o 1 fuerte + corroboración -> high)
//	1 señal débil           -> low        (débil + corroboración -> medium)
//
// "Corroboración" es una pista que sola no vale nada pero que sube lo que ya
// dijo otra señal: hoy, el navegador diciendo "mi interfaz activa es un wifi".
func classifySignals(e netEvidence) netVerdict {
	if e.changed {
		return netVerdict{"unknown", "low", "network-changed-mid-test"}
	}
	if !usableClientIP(e.obsStatus) {
		return netVerdict{"unknown", "low", "client-ip-" + e.obsStatus}
	}
	if relayASNs[e.obsASN] {
		return netVerdict{"unknown", "low", "relay-or-vpn"}
	}

	ipS, ipReason := ipSignal(e)
	// El orden importa: la razón del veredicto es la de la primera señal.
	var sigs []netSignal
	for _, sg := range []netSignal{ipS, hintSignal(e), asnSignal(e)} {
		if sg.route != "" {
			sigs = append(sigs, sg)
		}
	}
	if len(sigs) == 0 {
		// Sin ninguna señal utilizable: la razón la da el intento de IP, que es
		// el que sabe POR QUÉ no pudo decidir (no hay referencia, está vieja,
		// las familias no son comparables, es el CGNAT del operador).
		return netVerdict{"unknown", "low", ipReason}
	}
	for _, sg := range sigs[1:] {
		if sg.route != sigs[0].route {
			return netVerdict{"unknown", "low", "conflicting-signals"}
		}
	}
	route := sigs[0].route
	corroborations := len(sigs) - 1
	if hintCorroborates(e, route) {
		corroborations++
	}
	strong := false
	for _, sg := range sigs {
		if !sg.weak {
			strong = true
		}
	}
	switch {
	case strong && corroborations > 0:
		return netVerdict{route, "high", sigs[0].reason}
	case strong:
		return netVerdict{route, "medium", sigs[0].reason}
	case corroborations > 0:
		return netVerdict{route, "medium", sigs[0].reason}
	default:
		// Una sola señal ambigua. Se dice cuál parece ser la ruta, pero se
		// dice también que es una suposición: confianza baja, y con eso no
		// alimenta la velocidad titular del equipo.
		return netVerdict{route, "low", sigs[0].reason}
	}
}

// ipSignal compara la IP pública del cliente contra la última que se vio salir
// del equipo. Devuelve además la razón de por qué NO hay señal, que es lo que
// se reporta cuando no hay ninguna otra.
//
// Todo equipo de este proyecto es un CPE móvil (módems 5G/LTE en campo), así
// que su IPv4 pública es, por defecto, una IP compartida de CGNAT y su IPv6
// sale de un pool del operador. Eso es lo que hace ambigua la coincidencia.
func ipSignal(e netEvidence) (netSignal, string) {
	if !e.refOK {
		return netSignal{}, "no-router-reference"
	}
	if e.refAge > store.MaxEgressAge {
		return netSignal{}, "stale-router-reference"
	}
	ref, err := netip.ParseAddr(e.ref.IP)
	if err != nil {
		return netSignal{}, "no-router-reference"
	}
	ref = normalizeAddr(ref)
	obs := normalizeAddr(e.obs)
	if !obs.IsValid() || !ref.IsValid() {
		return netSignal{}, "no-router-reference"
	}
	// Las redes móviles colombianas entregan seguido bearer IPv6-only con
	// NAT64/464XLAT: el celular sale por IPv6 y el router por IPv4 bajo CGNAT.
	// NUNCA van a coincidir, así que una regla ingenua diría "otra red" para
	// siempre, incluso con el celular pegado al WiFi del router. No son
	// comparables: mejor decirlo que inventar un veredicto.
	if ipFamily(obs) != ipFamily(ref) {
		return netSignal{}, "ip-family-mismatch"
	}
	if obs.Is4() {
		if obs == ref {
			// Misma IP pública exacta. Es evidencia real, pero NO es prueba:
			// el equipo sale por el CGNAT del operador, donde miles de
			// suscriptores comparten dirección con reparto de puertos, así que
			// el celular midiendo por su PROPIA SIM del mismo operador también
			// puede caer acá. Señal débil: sola deja el veredicto en confianza
			// baja; con el navegador diciendo "estoy en un wifi" ya alcanza.
			return netSignal{"router", "ip-matches-router-cgnat-ambiguous", true}, ""
		}
		if privacyPrefix(obs) == privacyPrefix(ref) {
			// Mismo /24 no prueba nada: es el pool de CGNAT del operador, donde
			// caen también los celulares de media ciudad.
			return netSignal{}, "same-v4-prefix-cgnat"
		}
		return netSignal{"other-network", "ip-differs-from-router", false}, ""
	}
	// v6: un /64 es un enlace. Dos suscriptores móviles distintos no comparten
	// /64 (cada contexto PDP recibe el suyo), así que compartirlo sí es prueba:
	// o es el mismo equipo, o es un cliente al que el CPE le extendió su propio
	// /64 por WiFi.
	if samePrefix(obs, ref, 64) {
		return netSignal{"router", "same-v6-prefix-64", false}, ""
	}
	// Mismo /48 y distinto /64: son 65.536 /64 contiguos del pool del operador.
	// El celular en el WiFi del CPE puede caer acá (prefijo delegado), pero el
	// celular midiendo por su propia SIM del mismo operador TAMBIÉN. Débil.
	if samePrefix(obs, ref, 48) {
		return netSignal{"router", "same-v6-prefix-carrier-pool", true}, ""
	}
	return netSignal{"other-network", "ip-differs-from-router", false}, ""
}

// samePrefix: ¿las dos direcciones caen en el mismo bloque de n bits?
func samePrefix(a, b netip.Addr, bits int) bool {
	p, err := a.Prefix(bits)
	if err != nil {
		return false
	}
	return p.Contains(b)
}

// hintSignal: el equipo bajo prueba es un AP WiFi. Si la interfaz activa del
// celular es la radio celular, la prueba no salió por él.
func hintSignal(e netEvidence) netSignal {
	if e.hint == "cellular" {
		return netSignal{"other-network", "hint-cellular", false}
	}
	return netSignal{}
}

// hintCorroborates: "wifi" NO identifica CUÁL wifi (el del router, el de la
// oficina y el hotspot de otro celular dan los tres lo mismo), así que como
// señal sola no vale nada -- por eso no está en hintSignal. Pero SÍ corrobora
// un "router" que ya salió de la IP: para que eso fuera un falso positivo
// tendría que pasar que el celular esté en otro wifi Y que ese otro wifi salga
// por la misma IP pública que el equipo.
//
// Nunca contradice: si la IP dice "otra red" y el celular dice "wifi", es otro
// wifi, y el veredicto correcto sigue siendo "otra red".
func hintCorroborates(e netEvidence, route string) bool {
	return route == "router" && e.hint == "wifi"
}

// asnSignal: dos Movistar dan el mismo ASN, así que la igualdad no prueba
// nada; la diferencia sí.
func asnSignal(e netEvidence) netSignal {
	if e.obsASN != 0 && e.refASN != 0 && e.obsASN != e.refASN {
		return netSignal{"other-network", "asn-differs", false}
	}
	return netSignal{}
}

func refState(e netEvidence) string {
	if !e.refOK {
		return "absent"
	}
	if e.refAge > store.MaxEgressAge {
		return "stale"
	}
	return "fresh"
}

// normalizeHint acepta el type SOLO cuando el navegador dijo que la lectura es
// confiable (api == "network-information"). Chrome de escritorio expone el
// objeto pero .type solo funciona en ChromeOS, por eso no alcanza con que el
// campo exista.
func normalizeHint(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if api, _ := m["api"].(string); api != "network-information" {
		return ""
	}
	t, _ := m["type"].(string)
	return normalizeHintType(t)
}

func normalizeHintType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "cellular":
		return "cellular"
	case "wifi":
		return "wifi"
	}
	return ""
}

// ------------------------------------------------------------------ campos que pone el servidor

// netReservedKeys: claves que SIEMPRE las pone el servidor. Se borran del
// payload entrante antes de insertar: la X-API-Key está escrita en el JS del
// dashboard, así que cualquiera con el link podría mandar su propio net_route
// y ganarle al observado. mergeIntoRaw() no sirve acá porque nunca pisa lo que
// ya viene.
var netReservedKeys = []string{"net_route", "net_asn", "net", "client_ip"}

// netDropOnIngest: además de las reservadas, las IPs que el navegador leyó de
// speed.cloudflare.com. Se usan para detectar que la red cambió a mitad de
// prueba y después se borran: son IPs completas de un celular; a disco solo va
// net.cf_ip_changed (bool) y el prefijo.
var netDropOnIngest = append(append([]string{}, netReservedKeys...), "net_cf_ip_start", "net_cf_ip_end")

// withServerFields borra las claves reservadas y escribe las del servidor.
//
// UseNumber() no es un detalle: esto ahora corre sobre TODOS los ítems del
// POST (también los de notion5g.py y el agente del router, a los que solo se
// les borran las claves reservadas), así que el JSON se vuelve a serializar
// siempre. Sin json.Number, un entero grande o un decimal largo volverían a
// salir en notación científica y la medición guardada no sería byte a byte la
// que mandó el equipo.
func withServerFields(raw json.RawMessage, drop []string, fields map[string]any) (json.RawMessage, error) {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	// Un Decoder acepta contenido de más después del objeto; json.Unmarshal no.
	// Se mantiene el comportamiento estricto de antes: un ítem así se rechaza.
	if dec.More() {
		return nil, errors.New("json con contenido de más después del objeto")
	}
	if obj == nil {
		obj = map[string]any{}
	}
	for _, k := range drop {
		delete(obj, k)
	}
	for k, v := range fields {
		obj[k] = v
	}
	return json.Marshal(obj)
}

// buildNetMap arma la evidencia completa que se guarda en raw bajo "net".
// Se guarda TODO lo que se miró, no solo el resultado: sin esto, un
// "sin determinar" en el dashboard es indistinguible de un bug.
func buildNetMap(e netEvidence, m netMeta, v netVerdict) map[string]any {
	ipS, _ := ipSignal(e)
	out := map[string]any{
		"confidence":       v.Confidence,
		"reason":           v.Reason,
		"changed":          e.changed,
		"ip_source":        e.obsSource,
		"client_ip_prefix": privacyPrefixStr(e.obs),
		"client_ip_family": ipFamily(e.obs),
		"client_ip_status": e.obsStatus,
		"client_country":   m.country,
		"cf_ray":           m.cfRay,
		"asn":              nil,
		"asn_name":         nil,
		"asn_status":       m.asnStatus,
		"ref_ip_prefix":    nil,
		"ref_ip_family":    nil,
		"ref_asn":          nil,
		"ref_asn_name":     nil,
		"ref_age_s":        nil,
		"ref_state":        refState(e),
		"ip_verdict":       ipS.route,
		// "weak" = la señal de IP es ambigua (coincidencia bajo CGNAT móvil o
		// dentro del pool IPv6 del operador): sola deja el veredicto en
		// confianza baja. Queda anotado para poder auditar después por qué una
		// prueba salió "router" con confianza baja y otra con media.
		"ip_signal":        signalStrength(ipS),
		"hint_verdict":     hintVerdictEvidence(e, v.Route),
		"asn_verdict":      asnSignal(e).route,
		"observed_at":      nil,
		"posted_at":        m.postedAt.UTC().Format(time.RFC3339),
		"declared_matches": declaredMatches(m.declared, v.Route),
		"cf_ip_changed":    m.cfIPChanged,
	}
	if e.obsASN != 0 {
		out["asn"] = e.obsASN
		if m.obsASNName != "" {
			out["asn_name"] = m.obsASNName
		}
	}
	if !e.obsAt.IsZero() {
		out["observed_at"] = e.obsAt.UTC().Format(time.RFC3339)
	}
	if e.refOK {
		if a, err := netip.ParseAddr(e.ref.IP); err == nil {
			out["ref_ip_prefix"] = privacyPrefixStr(a)
		}
		out["ref_ip_family"] = e.ref.Family
		out["ref_age_s"] = int(e.refAge.Seconds())
		if e.refASN != 0 {
			out["ref_asn"] = e.refASN
			name := m.refASNName
			if name == "" {
				name = e.ref.ASNName
			}
			if name != "" {
				out["ref_asn_name"] = name
			}
		}
	}
	return out
}

// signalStrength describe la señal de IP para la evidencia guardada.
func signalStrength(sg netSignal) any {
	if sg.route == "" {
		return nil
	}
	if sg.weak {
		return "weak"
	}
	return "strong"
}

// hintVerdictEvidence deja anotado qué papel jugó la pista del navegador:
// "other-network" cuando decidió, "corroborates-router" cuando solo sumó.
func hintVerdictEvidence(e netEvidence, route string) any {
	if sg := hintSignal(e); sg.route != "" {
		return sg.route
	}
	if hintCorroborates(e, route) {
		return "corroborates-router"
	}
	return ""
}

// declaredMatches compara lo que el usuario dijo que iba a medir con lo que se
// detectó. No bloquea nada: solo queda anotado para poder revisar después.
// nil = no declaró nada.
func declaredMatches(declared, route string) any {
	switch declared {
	case "router":
		return route == "router"
	case "phone-cellular", "other-wifi":
		return route == "other-network"
	}
	return nil
}

// ------------------------------------------------------------------ worker asíncrono de ASN

// netEnrichJob: resolver el ASN mete un round trip de DNS, y el POST de una
// medición NO puede depender de eso (SetMaxOpenConns(1) serializa las
// escrituras contra los heartbeats de cada router, y un timeout de Cymru
// degradaría la ingesta entera). Se resuelve después del insert, fuera del
// camino de escritura; si la cola está llena se descarta el trabajo y la fila
// se queda con la clasificación del ingest, que ya es correcta, solo con una
// señal menos.
type netEnrichJob struct {
	measurementID int64
	deviceID      string
	ev            netEvidence
	meta          netMeta
	obsIP         netip.Addr
}

const netEnrichQueue = 64

func (s *Server) enqueueNetEnrich(job netEnrichJob) {
	if job.measurementID == 0 || s.netEnrich == nil {
		return
	}
	select {
	case s.netEnrich <- job:
	default:
		log.Printf("netclass: cola de ASN llena, la medición %d se queda sin señal de operador", job.measurementID)
	}
}

func (s *Server) netEnrichLoop() {
	for job := range s.netEnrich {
		s.enrichNet(job)
	}
}

func (s *Server) enrichNet(job netEnrichJob) {
	// context.Background() con timeout propio: el contexto del request ya se
	// canceló al responder, y este trabajo es justamente el que NO corre
	// dentro del request.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	e, m := job.ev, job.meta
	m.asnStatus = "no-ip"
	if job.obsIP.IsValid() {
		m.asnStatus = "not-resolved"
		if info, ok := s.asn.Lookup(ctx, job.obsIP); ok {
			e.obsASN, m.obsASNName, m.asnStatus = info.ASN, info.Name, "ok"
		}
	}

	// El ASN de la referencia: si ya está guardado se reusa; si no, se resuelve
	// una vez y se guarda (solo si la IP guardada sigue siendo la misma, ver
	// SetDeviceEgressASN: bajo CGNAT móvil la IP del router rota).
	if e.refOK {
		if e.ref.ASN != nil && *e.ref.ASN != 0 {
			e.refASN, m.refASNName = *e.ref.ASN, e.ref.ASNName
		} else if refAddr, err := netip.ParseAddr(e.ref.IP); err == nil {
			if info, ok := s.asn.Lookup(ctx, refAddr); ok {
				e.refASN, m.refASNName = info.ASN, info.Name
				if err := s.store.SetDeviceEgressASN(ctx, job.deviceID, e.ref.IP, info.ASN, info.Name); err != nil {
					log.Printf("netclass: guardar asn de %s: %v", job.deviceID, err)
				}
			}
		}
	}

	v := classify(e)
	var asnPtr *int
	if e.obsASN != 0 {
		asn := e.obsASN
		asnPtr = &asn
	}
	fields := map[string]any{"net_route": v.Route, "net": buildNetMap(e, m, v)}
	if asnPtr != nil {
		fields["net_asn"] = *asnPtr
	}
	nc := store.NetClassification{Route: v.Route, Confidence: v.Confidence, ASN: asnPtr}
	if err := s.store.SetMeasurementNet(ctx, job.measurementID, nc, fields); err != nil {
		log.Printf("netclass: reclasificar medición %d: %v", job.measurementID, err)
	}
}
