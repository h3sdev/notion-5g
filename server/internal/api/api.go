// Package api expone el backend HTTP que recibe mediciones del módem Notion 5G
// (desde notion5g.py en el PC, y más adelante desde la app Android/Flutter).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"notion5g/server/internal/store"
	"notion5g/server/internal/web"
)

// maxSpeedtestBytes limita cuánto puede pedir/mandar una sola prueba de velocidad
// (evita que un cliente mal comportado pida gigabytes o tumbe el servidor).
const maxSpeedtestBytes = 500 << 20 // 500 MiB

var zeroChunk = make([]byte, 64<<10) // 64 KiB reutilizado, no se reasigna por request

type Server struct {
	store  *store.Store
	apiKey string // vacío = sin autenticación (solo para desarrollo local)
	mux    *http.ServeMux

	// speedInFlight cuenta las pruebas de velocidad que están corriendo
	// contra este servidor en este instante. Dos equipos probando a la vez
	// se reparten el mismo enlace del VPS, así que cada uno mide de menos:
	// el valor se devuelve en X-Speedtest-Concurrent para que el cliente
	// pueda avisarlo y dejarlo anotado en la medición.
	speedInFlight atomic.Int64

	// Identificación automática de la ruta de salida (ver netclass.go).
	proxy     *trustedProxy     // quién tiene permitido mandarnos CF-Connecting-IP
	asn       *asnResolver      // operador de una IP, por DNS (Team Cymru)
	ledger    *obsLedger        // IPs vistas durante la ventana de medición
	netEnrich chan netEnrichJob // cola del worker que resuelve ASN fuera del POST
}

func New(st *store.Store, apiKey string) *Server {
	s := &Server{
		store:  st,
		apiKey: apiKey,
		mux:    http.NewServeMux(),
		proxy:  newTrustedProxy(),
		asn:    newASNResolver(),
		ledger: newObsLedger(),
		// Una sola goroutine: las consultas a Cymru están cacheadas y son de
		// milisegundos, y serializarlas evita mandarle una ráfaga de DNS a un
		// servicio gratuito cuando entra un lote de mediciones.
		netEnrich: make(chan netEnrichJob, netEnrichQueue),
	}
	go s.netEnrichLoop()
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	// Dashboard estático (embebido en el binario): no requiere X-API-Key para
	// cargar, la key se pide dentro de la propia página y se manda en cada
	// fetch() a /api/v1/*. "GET /" matches also every unmatched sub-path
	// (p. ej. /assets/app.js), ya que los patrones más específicos de abajo
	// tienen prioridad en el ServeMux.
	s.mux.Handle("GET /", web.Handler())

	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /api/v1/measurements", s.auth(s.handlePostMeasurements))
	s.mux.HandleFunc("GET /api/v1/measurements", s.auth(s.handleListMeasurements))
	s.mux.HandleFunc("DELETE /api/v1/measurements/{id}", s.auth(s.handleDeleteMeasurement))
	s.mux.HandleFunc("GET /api/v1/measurements/summary", s.auth(s.handleSummary))
	s.mux.HandleFunc("POST /api/v1/heartbeat", s.auth(s.handlePostHeartbeat))
	s.mux.HandleFunc("GET /api/v1/heartbeats", s.auth(s.handleListHeartbeats))
	s.mux.HandleFunc("GET /api/v1/devices", s.auth(s.handleListDevices))
	s.mux.HandleFunc("POST /api/v1/devices/{device_id}/location", s.auth(s.handleSetDeviceLocation))
	s.mux.HandleFunc("GET /api/v1/devices/{device_id}/phone_log", s.auth(s.handleListPhoneLog))

	s.mux.HandleFunc("POST /api/v1/commands", s.auth(s.handleCreateCommand))
	s.mux.HandleFunc("GET /api/v1/commands", s.auth(s.handleListCommands))
	s.mux.HandleFunc("GET /api/v1/commands/next", s.auth(s.handleClaimCommand))
	s.mux.HandleFunc("POST /api/v1/commands/{id}/complete", s.auth(s.handleCompleteCommand))
	s.mux.HandleFunc("POST /api/v1/commands/{id}/end_location", s.auth(s.handleSetCommandEndLocation))

	s.mux.HandleFunc("GET /api/v1/netinfo", s.auth(s.handleNetInfo))

	s.mux.HandleFunc("GET /api/v1/speedtest/download", s.auth(s.handleSpeedDownload))
	s.mux.HandleFunc("POST /api/v1/speedtest/upload", s.auth(s.handleSpeedUpload))
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			next(w, r)
			return
		}
		got := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.apiKey)) != 1 {
			writeErr(w, http.StatusUnauthorized, "X-API-Key inválida o ausente")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
}

// handlePostMeasurements acepta un objeto JSON o un arreglo de objetos.
// Cada objeto es el mismo dict que ya produce notion5g.py (status+speed), más
// device_id (obligatorio) y opcionalmente source/lat/lon/gps_accuracy_m/uptime_s.
func (s *Server) handlePostMeasurements(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 1<<20) // 1 MiB por request
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := splitJSONItems(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// La IP del cliente se lee UNA vez por request (es la misma para todos los
	// ítems del arreglo); la clasificación, en cambio, va POR ÍTEM, porque cada
	// uno puede ser de un device_id distinto.
	obsIP, obsStatus := s.clientIP(r)
	req := netRequestCtx{
		obsIP:     obsIP,
		obsStatus: obsStatus,
		country:   cfCountry(r),
		cfRay:     r.Header.Get("CF-Ray"),
		now:       time.Now().UTC(),
	}

	inserted := 0
	var errs []string
	nets := make([]map[string]any, 0, len(items))
	for _, item := range items {
		payload, nc, job, out := s.classifyItem(ctx, item, req)
		// La clasificación viaja por PARÁMETRO, no dentro del JSON: las
		// columnas net_route/net_asn/net_confidence no se leen nunca del
		// payload del cliente (ver store.NetClassification).
		id, err := s.store.InsertClassified(ctx, payload, nc)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		inserted++
		if out == nil {
			// No participa del esquema (notion5g.py, agente del router): la
			// fila queda con net_route NULL, igual que siempre.
			out = map[string]any{"net_route": nil, "confidence": nil, "reason": nil, "asn": nil, "asn_name": nil}
		} else if job != nil {
			job.measurementID = id
			s.enqueueNetEnrich(*job)
		}
		out["id"] = id
		nets = append(nets, out)
	}
	status := http.StatusOK
	if inserted == 0 && len(errs) > 0 {
		status = http.StatusBadRequest
	} else if len(errs) > 0 {
		status = http.StatusMultiStatus
	}
	// "net" es aditivo: notion5g.py y el agente del router ignoran el cuerpo
	// de la respuesta, así que agregarlo no rompe ningún cliente viejo.
	writeJSON(w, status, map[string]any{"inserted": inserted, "errors": errs, "net": nets})
}

// netRequestCtx es lo que se ve de la conexión, una sola vez por request.
type netRequestCtx struct {
	obsIP     netip.Addr
	obsStatus string
	country   string
	cfRay     string
	now       time.Time
}

// netItemInput son los campos del payload que miramos para clasificar. Los
// tipos laxos (any) son a propósito: si un cliente manda basura en un campo
// nuevo, la medición se tiene que guardar igual, sin clasificar, nunca fallar.
type netItemInput struct {
	DeviceID    string `json:"device_id"`
	Source      string `json:"source"`
	TestID      string `json:"test_id"`
	NetHint     any    `json:"net_hint"`
	NetDeclared string `json:"net_declared"`
	CFIPStart   string `json:"net_cf_ip_start"`
	CFIPEnd     string `json:"net_cf_ip_end"`
	ChangedHint any    `json:"net_changed_hint"`
}

// classifyItem decide la ruta de salida de UN ítem del POST y le inyecta los
// campos del servidor. Devuelve el payload listo para insertar, la
// clasificación para las columnas, el trabajo de enriquecimiento por ASN (o
// nil) y el resumen para la respuesta HTTP (nil si el ítem no participa del
// esquema).
//
// Las claves reservadas se borran del payload SIEMPRE, participe o no el ítem.
// Antes solo se borraban en la rama que participa, y con eso bastaba omitir
// test_id/net_hint/net_declared (y mandar source != "browser") para que
// {"net_route":"router","net":{"confidence":"high"}} quedara guardado tal cual
// y el dashboard lo pintara como "vía router · confianza alta". La X-API-Key
// está en el JS del dashboard, así que alcanzaba con tener el link; y el camino
// accidental era igual de real, porque GET /api/v1/measurements devuelve el raw
// CON esas claves adentro y cualquier reimportación las volvía a estampar.
func (s *Server) classifyItem(ctx context.Context, item json.RawMessage, req netRequestCtx) (json.RawMessage, store.NetClassification, *netEnrichJob, map[string]any) {
	var in netItemInput
	if err := json.Unmarshal(item, &in); err != nil {
		// JSON con un tipo raro en un campo conocido (p. ej. test_id numérico).
		// No se puede clasificar, pero tampoco se puede dejar pasar lo que el
		// cliente haya escrito en las claves reservadas.
		return stripReserved(item), store.NetClassification{}, nil, nil
	}
	// ¿Participa? Si no, no se clasifica: net_route queda NULL y el
	// comportamiento es idéntico al de siempre para notion5g.py y el agente.
	// Las claves reservadas se borran igual.
	if in.TestID == "" && in.NetHint == nil && in.NetDeclared == "" && in.Source != "browser" {
		return stripReserved(item), store.NetClassification{}, nil, nil
	}

	e := netEvidence{
		obs:       req.obsIP,
		obsSource: "post",
		obsStatus: req.obsStatus,
		obsAt:     req.now,
		hint:      normalizeHint(in.NetHint),
	}
	if !req.obsIP.IsValid() {
		e.obsSource = "none"
	}
	// Las observaciones de la ventana de medición ganan sobre la IP del POST:
	// lo que interesa es por dónde salió la prueba, no por dónde se guardó. El
	// ledger guarda también con qué status se vio esa ventana (ver testObs):
	// no se puede asumir "ok", porque entonces una ventana observada con el
	// peer del túnel sin verificar se guardaría como si estuviera confirmada.
	if o, ok := s.ledger.take(in.TestID); ok && o.first.IsValid() {
		e.obs, e.obsSource, e.obsStatus, e.obsAt = o.first, "measure-window", o.status, o.firstAt
		e.changed = o.last.IsValid() && o.first != o.last
	}
	// OJO: net_cf_ip_start/end se comparan SOLO entre sí (las dos las ve el
	// mismo host, speed.cloudflare.com). Compararlas contra la IP que vemos
	// nosotros sería un falso positivo garantizado: son hosts distintos y con
	// Happy Eyeballs el mismo celular puede salir por IPv6 hacia uno y por
	// IPv4 hacia el otro en el mismo segundo.
	cfChanged := in.CFIPStart != "" && in.CFIPEnd != "" && !strings.EqualFold(in.CFIPStart, in.CFIPEnd)
	e.changed = e.changed || truthy(in.ChangedHint) || cfChanged

	if ref, ok := s.store.DeviceEgressOf(ctx, in.DeviceID); ok {
		e.ref, e.refOK, e.refAge = ref, true, req.now.Sub(ref.UpdatedAt)
		if ref.ASN != nil {
			e.refASN = *ref.ASN
		}
	}

	m := netMeta{
		country:     req.country,
		cfRay:       req.cfRay,
		declared:    in.NetDeclared,
		cfIPChanged: cfChanged,
		postedAt:    req.now,
		asnStatus:   "pending",
	}
	if !e.obs.IsValid() {
		m.asnStatus = "no-ip"
	}
	if e.refOK && e.ref.ASN != nil {
		m.refASNName = e.ref.ASNName
	}

	v := classify(e)
	payload, err := withServerFields(item, netDropOnIngest, map[string]any{
		"net_route": v.Route,
		"net":       buildNetMap(e, m, v),
	})
	if err != nil {
		// JSON raro: que falle el insert como siempre, pero sin las claves
		// reservadas del cliente adentro.
		return stripReserved(item), store.NetClassification{}, nil, nil
	}

	out := map[string]any{
		"net_route": v.Route, "confidence": v.Confidence, "reason": v.Reason,
		"asn": nil, "asn_name": nil,
	}
	var job *netEnrichJob
	if e.obs.IsValid() {
		job = &netEnrichJob{deviceID: in.DeviceID, ev: e, meta: m, obsIP: e.obs}
	}
	return payload, store.NetClassification{Route: v.Route, Confidence: v.Confidence}, job, out
}

// stripReserved borra del payload las claves que pone SIEMPRE el servidor, sin
// agregar nada. Es lo que se inserta cuando el ítem no se clasifica: si el
// borrado fallara (el ítem no es un objeto JSON), se devuelve tal cual y el
// insert lo rechaza igual que siempre -- insertMeasurement exige un objeto con
// device_id.
func stripReserved(item json.RawMessage) json.RawMessage {
	clean, err := withServerFields(item, netDropOnIngest, nil)
	if err != nil {
		return item
	}
	return clean
}

// truthy acepta true, "true"/"1"/"yes" y números distintos de cero: el campo
// lo manda el navegador y no vale la pena rechazar una prueba entera porque
// llegó como string.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes", "si", "sí":
			return true
		}
	}
	return false
}

// handlePostHeartbeat guarda el ping liviano del agente del router y, de paso,
// aprende desde qué IP pública sale ese equipo: es la REFERENCIA contra la que
// después se compara la IP del navegador que corre una prueba. Se aprende de
// una petición que el agente YA hace cada minuto, sin tocar nada en el equipo
// de campo (ver §6.1 del plan: el binario ARM se instala a mano por cron).
//
// Nada de esto puede hacer fallar un heartbeat: si falla, se loguea y sigue.
// Un equipo en campo reportando es más importante que saber su IP.
func (s *Server) handlePostHeartbeat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 64<<10)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// usableClientIP y no status == "ok": si el nombre del contenedor del túnel
	// dejara de resolver, exigir "ok" acá haría que NUNCA se aprenda la IP de
	// referencia de ningún equipo, y sin referencia toda medición queda "sin
	// determinar" para siempre, sin recuperarse sola. Se aprende igual; lo que
	// se degrada es la confianza de la clasificación (ver classify).
	if ip, status := s.clientIP(r); usableClientIP(status) && ip.IsValid() {
		var hb struct {
			DeviceID string `json:"device_id"`
		}
		if err := json.Unmarshal(body, &hb); err == nil && hb.DeviceID != "" {
			if err := s.store.SetDeviceEgress(ctx, hb.DeviceID, ip.String(), ipFamily(ip), cfCountry(r)); err != nil {
				log.Printf("heartbeat: guardar ip de salida de %s: %v", hb.DeviceID, err)
			}
			// La IP completa también queda en raw para tener historial de
			// rotación de CGNAT. Es la IP del CPE, no la de una persona.
			if merged, err := withServerFields(body, netReservedKeys, map[string]any{"client_ip": ip.String()}); err == nil {
				body = merged
			}
		}
	}

	if _, err := s.store.InsertHeartbeat(ctx, body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeleteMeasurement: borra una medición puntual. Pensado para que Diego
// pueda sacar del historial pruebas que él mismo marca como inválidas (p. ej.
// una corrida por error con datos móviles en vez de por el módem) sin tener
// que tocar la base de datos a mano.
func (s *Server) handleDeleteMeasurement(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	deleted, err := s.store.DeleteMeasurement(ctx, id)
	if err != nil {
		log.Printf("delete measurement %d: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "error borrando la medición")
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, "no existe esa medición")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListMeasurements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := parseIntSafe(v); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.store.ListRaw(ctx, store.ListFilter{
		DeviceID: q.Get("device_id"),
		Tag:      q.Get("tag"),
		Operator: q.Get("operator"),
		Since:    q.Get("since"),
		NetRoute: q.Get("net_route"),
		Limit:    limit,
	})
	if err != nil {
		log.Printf("list measurements: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("["))
	for i, raw := range rows {
		if i > 0 {
			w.Write([]byte(","))
		}
		w.Write(raw)
	}
	w.Write([]byte("]"))
}

func (s *Server) handleListHeartbeats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := parseIntSafe(v); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.store.ListHeartbeats(ctx, q.Get("device_id"), limit)
	if err != nil {
		log.Printf("list heartbeats: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("["))
	for i, raw := range rows {
		if i > 0 {
			w.Write([]byte(","))
		}
		w.Write(raw)
	}
	w.Write([]byte("]"))
}

// handleListDevices devuelve, por device_id, el último estado conocido
// (operador, RAT, señal, ubicación, última velocidad medida) para el
// dashboard. Ver store.ListDeviceSummaries.
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	devices, err := s.store.ListDeviceSummaries(ctx)
	if err != nil {
		log.Printf("list devices: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

// handleSetDeviceLocation: el celular acompañante manda su ubicación
// ultraprecisa (Fused Location) mientras el equipo va en movimiento; queda
// como "última ubicación conocida" de ese device_id (store.SetDeviceLocation)
// y se usa para completar mediciones/heartbeats del router, que no tiene GPS
// propio. Body: {"lat":..,"lon":..,"gps_accuracy_m":..,"gps_source":"android-fused"}
func (s *Server) handleSetDeviceLocation(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("device_id")
	if deviceID == "" {
		writeErr(w, http.StatusBadRequest, "falta device_id en la ruta")
		return
	}
	body, err := readBody(r, 4<<10)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var loc struct {
		Lat          *float64 `json:"lat"`
		Lon          *float64 `json:"lon"`
		GPSAccuracyM *float64 `json:"gps_accuracy_m"`
		GPSSource    string   `json:"gps_source"`
		// Estado del celular acompañante; opcional (una app vieja no lo manda).
		BatteryPct    *int     `json:"battery_pct"`
		Charging      *bool    `json:"charging"`
		BatteryStatus string   `json:"battery_status"`
		Plugged       string   `json:"plugged"`
		BatteryTempC  *float64 `json:"battery_temp_c"`
		NetType       string   `json:"net_type"`
	}
	if err := json.Unmarshal(body, &loc); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	if loc.Lat == nil || loc.Lon == nil {
		writeErr(w, http.StatusBadRequest, "faltan lat/lon")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.SetDeviceLocation(ctx, deviceID, *loc.Lat, *loc.Lon, loc.GPSAccuracyM, loc.GPSSource); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// El historial es secundario: si falla, la ubicación ya quedó guardada.
	if err := s.store.AppendPhoneLog(ctx, store.PhoneLogEntry{
		DeviceID: deviceID, Lat: loc.Lat, Lon: loc.Lon, GPSAccuracyM: loc.GPSAccuracyM,
		BatteryPct: loc.BatteryPct, Charging: loc.Charging, BatteryStatus: loc.BatteryStatus,
		Plugged: loc.Plugged, BatteryTempC: loc.BatteryTempC, NetType: loc.NetType,
	}); err != nil {
		log.Printf("location: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListPhoneLog(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parseIntSafe(v); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.store.ListPhoneLog(ctx, r.PathValue("device_id"), limit)
	if err != nil {
		log.Printf("phone_log: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "operator"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	summary, err := s.store.Summary(ctx, groupBy, q.Get("since"), q.Get("net_route"))
	if err != nil {
		log.Printf("summary: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// ------------------------------------------------------------------ cola de comandos

// handleCreateCommand: el celular pide que se corra una prueba en un router,
// mandando la ubicación ultraprecisa que ya tiene (Fused Location de Android).
// body: {"device_id":"router-xxx","type":"run_speedtest","duration_s":10,
//
//	"lat":..,"lon":..,"gps_accuracy_m":..,"gps_source":"android-fused","requested_by":"android-abc"}
func (s *Server) handleCreateCommand(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 16<<10)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id, err := s.store.CreateCommand(ctx, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "pending"})
}

// handleClaimCommand: el router hace polling aquí (no puede recibir conexiones
// entrantes, está detrás de NAT celular). Devuelve el comando pendiente más
// viejo para ese device_id, o {} si no hay ninguno.
func (s *Server) handleClaimCommand(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		writeErr(w, http.StatusBadRequest, "falta ?device_id=")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cmd, err := s.store.ClaimNextCommand(ctx, deviceID)
	if err != nil {
		log.Printf("claim command: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	if cmd == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, cmd)
}

// handleCompleteCommand: el router reporta el resultado tras ejecutar la prueba.
// body: {"status":"done","measurement":{...fila tipo notion5g.py...}} o {"status":"failed","error":"..."}
func (s *Server) handleCompleteCommand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	body, err := readBody(r, 1<<20)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	deviceID, _ := s.store.DeviceIDOfCommand(ctx, id)
	measurementID, err := s.store.CompleteCommand(ctx, id, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.noteDeviceMeasurement(ctx, r, deviceID, measurementID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// noteDeviceMeasurement se llama al cerrar un comando: la prueba la corrió el
// PROPIO equipo, por su propio módem, así que la ruta de salida es "router" sin
// ambigüedad (byDevice). De paso se aprende su IP pública, igual que en el
// heartbeat. Best-effort: nada de esto puede romper el cierre de un comando,
// que es el momento en que se guarda una medición real de campo.
func (s *Server) noteDeviceMeasurement(ctx context.Context, r *http.Request, deviceID string, measurementID int64) {
	if deviceID == "" {
		return
	}
	ip, status := s.clientIP(r)
	now := time.Now().UTC()
	e := netEvidence{byDevice: true, obs: ip, obsStatus: status, obsSource: "post", obsAt: now}
	if !ip.IsValid() || !usableClientIP(status) {
		e.obsSource = "none"
	} else {
		if err := s.store.SetDeviceEgress(ctx, deviceID, ip.String(), ipFamily(ip), cfCountry(r)); err != nil {
			log.Printf("complete: guardar ip de salida de %s: %v", deviceID, err)
		}
	}
	if measurementID == 0 {
		return
	}
	if ref, ok := s.store.DeviceEgressOf(ctx, deviceID); ok {
		e.ref, e.refOK, e.refAge = ref, true, now.Sub(ref.UpdatedAt)
		if ref.ASN != nil {
			e.refASN = *ref.ASN
		}
	}
	m := netMeta{country: cfCountry(r), cfRay: r.Header.Get("CF-Ray"), postedAt: now, asnStatus: "pending"}
	if !ip.IsValid() {
		m.asnStatus = "no-ip"
	}
	v := classify(e)
	fields := map[string]any{"net_route": v.Route, "net": buildNetMap(e, m, v)}
	nc := store.NetClassification{Route: v.Route, Confidence: v.Confidence}
	if err := s.store.SetMeasurementNet(ctx, measurementID, nc, fields); err != nil {
		log.Printf("complete: clasificar medición %d: %v", measurementID, err)
		return
	}
	if ip.IsValid() && usableClientIP(status) {
		s.enqueueNetEnrich(netEnrichJob{measurementID: measurementID, deviceID: deviceID, ev: e, meta: m, obsIP: ip})
	}
}

// handleSetCommandEndLocation: el dashboard llama esto cuando el polling
// detecta que la prueba de velocidad terminó (status "done"), con la
// ubicación del navegador tomada en ese momento -- la de arranque ya viajó
// en el propio comando al crearlo (POST /api/v1/commands). Sirve para saber
// si el equipo se movió durante los segundos que duró la prueba. No pisa
// lat/lon (la ubicación de arranque, ya guardada en la medición); se guarda
// aparte, en lat_end/lon_end (ver Store.SetMeasurementEndLocation).
func (s *Server) handleSetCommandEndLocation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	body, err := readBody(r, 4<<10)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var loc struct {
		Lat          *float64 `json:"lat"`
		Lon          *float64 `json:"lon"`
		GPSAccuracyM *float64 `json:"gps_accuracy_m"`
		GPSSource    string   `json:"gps_source"`
	}
	if err := json.Unmarshal(body, &loc); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	if loc.Lat == nil || loc.Lon == nil {
		writeErr(w, http.StatusBadRequest, "faltan lat/lon")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.SetMeasurementEndLocation(ctx, id, *loc.Lat, *loc.Lon, loc.GPSAccuracyM, loc.GPSSource); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListCommands(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := parseIntSafe(v); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cmds, err := s.store.ListCommands(ctx, q.Get("device_id"), q.Get("status"), limit)
	if err != nil {
		log.Printf("list commands: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	writeJSON(w, http.StatusOK, cmds)
}

// ------------------------------------------------------------------ prueba de velocidad propia
//
// Igual que notion5g.py hace hoy contra speed.cloudflare.com, pero contra este
// mismo backend: útil cuando se quiere una latencia/ruta representativa del
// mismo operador (ver docs/02-plan-homologacion.md), y es lo que usa el agente
// del router (que no trae iperf3 para no gastar la poca RAM libre del equipo).

// handleSpeedDownload transmite `bytes` bytes en cero, en trozos pequeños
// reutilizados (no reserva bytes de memoria por request).
func (s *Server) handleSpeedDownload(w http.ResponseWriter, r *http.Request) {
	n := int64(10_000_000) // 10 MB por defecto
	if v := r.URL.Query().Get("bytes"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > maxSpeedtestBytes {
		n = maxSpeedtestBytes
	}
	s.observeTest(r)
	concurrent := s.speedInFlight.Add(1)
	defer s.speedInFlight.Add(-1)
	w.Header().Set("X-Speedtest-Concurrent", strconv.FormatInt(concurrent, 10))
	w.Header().Set("Access-Control-Expose-Headers", "X-Speedtest-Concurrent")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	w.WriteHeader(http.StatusOK)
	remaining := n
	for remaining > 0 {
		chunk := zeroChunk
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}
		written, err := w.Write(chunk)
		remaining -= int64(written)
		if err != nil {
			return // cliente cortó la descarga a mitad de prueba; no es un error del servidor
		}
	}
}

// handleSpeedUpload lee y descarta el body (hasta el límite) para medir cuánto
// tardó el cliente en mandarlo; no lo guarda en memoria ni en disco.
func (s *Server) handleSpeedUpload(w http.ResponseWriter, r *http.Request) {
	s.observeTest(r)
	concurrent := s.speedInFlight.Add(1)
	defer s.speedInFlight.Add(-1)
	w.Header().Set("X-Speedtest-Concurrent", strconv.FormatInt(concurrent, 10))
	r.Body = http.MaxBytesReader(nil, r.Body, maxSpeedtestBytes)
	n, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "error leyendo el cuerpo: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"received_bytes": n, "concurrent": concurrent})
}

// observeTest anota la IP pública desde la que se está corriendo una prueba,
// en el momento en que se corre (no cuando se guarda). El test_id va como
// query param: el cuerpo del upload es el relleno de la prueba, no un JSON.
//
// Sin esto, la única IP disponible sería la del POST final, que puede ser de
// otra red si el celular se cambió justo al terminar de medir.
func (s *Server) observeTest(r *http.Request) {
	testID := r.URL.Query().Get("test_id")
	if testID == "" {
		return
	}
	if ip, status := s.clientIP(r); usableClientIP(status) && ip.IsValid() {
		s.ledger.observe(testID, ip, status, time.Now().UTC())
	}
}

// handleNetInfo responde QUÉ VE EL BACKEND de la conexión de quien pregunta,
// para que el navegador pueda mostrar la red de salida ANTES de medir.
//
// net_route acá es un PREVIEW: se calcula con la IP de ESTA petición, no con
// la de la ventana de medición. La clasificación que queda guardada la decide
// el POST de la medición.
//
// Acá SÍ se resuelve el ASN de forma síncrona (con timeout duro y caché): es
// una lectura disparada por el usuario, no el camino de ingesta. Si el DNS
// falla, la respuesta igual es 200 con asn_status="not-resolved": nunca un 500.
func (s *Server) handleNetInfo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	obsIP, obsStatus := s.clientIP(r)
	now := time.Now().UTC()

	resp := map[string]any{
		"observed_at":      now.Format(time.RFC3339),
		"client_ip_prefix": privacyPrefixStr(obsIP), // NUNCA la IP completa
		"client_ip_family": ipFamily(obsIP),
		"client_ip_status": obsStatus,
		"client_country":   cfCountry(r),
		"cf_ray":           r.Header.Get("CF-Ray"),
		"asn":              nil,
		"asn_name":         nil,
		"asn_status":       "no-ip",
		"device":           nil,
		"net_route":        nil,
		"confidence":       nil,
		"reason":           nil,
	}
	if q.Get("debug") == "1" {
		peer, _ := parseHostAddr(r.RemoteAddr)
		resp["debug_headers"] = map[string]any{
			"cf_connecting_ip": r.Header.Get("CF-Connecting-IP"),
			"cf_ipcountry":     r.Header.Get("CF-IPCountry"),
			"cf_ray":           r.Header.Get("CF-Ray"),
			"x_forwarded_for":  r.Header.Get("X-Forwarded-For"),
			"remote_addr":      r.RemoteAddr,
			"trusted_proxy":    s.proxy.is(peer),
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	e := netEvidence{
		obs:       obsIP,
		obsSource: "post",
		obsStatus: obsStatus,
		obsAt:     now,
		hint:      normalizeHintType(q.Get("hint")),
	}
	if !obsIP.IsValid() {
		e.obsSource = "none"
	} else {
		resp["asn_status"] = "not-resolved"
		if info, ok := s.asn.Lookup(ctx, obsIP); ok {
			resp["asn"], resp["asn_name"], resp["asn_status"] = info.ASN, info.Name, "ok"
			e.obsASN = info.ASN
		}
	}

	deviceID := q.Get("device_id")
	if deviceID == "" {
		writeJSON(w, http.StatusOK, resp) // sin device_id no hay contra qué comparar
		return
	}

	dev := map[string]any{
		"device_id":     deviceID,
		"ref_ip_prefix": nil,
		"ref_ip_family": nil,
		"ref_asn":       nil,
		"ref_asn_name":  nil,
		"ref_age_s":     nil,
		"ref_state":     "absent",
	}
	if ref, ok := s.store.DeviceEgressOf(ctx, deviceID); ok {
		e.ref, e.refOK, e.refAge = ref, true, now.Sub(ref.UpdatedAt)
		if a, err := netip.ParseAddr(ref.IP); err == nil {
			dev["ref_ip_prefix"] = privacyPrefixStr(a)
			if ref.ASN != nil && *ref.ASN != 0 {
				e.refASN = *ref.ASN
				dev["ref_asn"], dev["ref_asn_name"] = *ref.ASN, ref.ASNName
			} else if info, ok := s.asn.Lookup(ctx, a); ok {
				e.refASN = info.ASN
				dev["ref_asn"], dev["ref_asn_name"] = info.ASN, info.Name
				if err := s.store.SetDeviceEgressASN(ctx, deviceID, ref.IP, info.ASN, info.Name); err != nil {
					log.Printf("netinfo: guardar asn de %s: %v", deviceID, err)
				}
			}
		}
		dev["ref_ip_family"] = ref.Family
		dev["ref_age_s"] = int(e.refAge.Seconds())
	}
	dev["ref_state"] = refState(e)
	resp["device"] = dev

	v := classify(e)
	resp["net_route"], resp["confidence"], resp["reason"] = v.Route, v.Confidence, v.Reason
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
