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
	"strconv"
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
}

func New(st *store.Store, apiKey string) *Server {
	s := &Server{store: st, apiKey: apiKey, mux: http.NewServeMux()}
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

	s.mux.HandleFunc("POST /api/v1/commands", s.auth(s.handleCreateCommand))
	s.mux.HandleFunc("GET /api/v1/commands", s.auth(s.handleListCommands))
	s.mux.HandleFunc("GET /api/v1/commands/next", s.auth(s.handleClaimCommand))
	s.mux.HandleFunc("POST /api/v1/commands/{id}/complete", s.auth(s.handleCompleteCommand))
	s.mux.HandleFunc("POST /api/v1/commands/{id}/end_location", s.auth(s.handleSetCommandEndLocation))

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

	inserted := 0
	var errs []string
	for _, item := range items {
		if _, err := s.store.InsertRaw(ctx, item); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		inserted++
	}
	status := http.StatusOK
	if inserted == 0 && len(errs) > 0 {
		status = http.StatusBadRequest
	} else if len(errs) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{"inserted": inserted, "errors": errs})
}

func (s *Server) handlePostHeartbeat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 64<<10)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "operator"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	summary, err := s.store.Summary(ctx, groupBy, q.Get("since"))
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
	if err := s.store.CompleteCommand(ctx, id, body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	r.Body = http.MaxBytesReader(nil, r.Body, maxSpeedtestBytes)
	n, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "error leyendo el cuerpo: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"received_bytes": n})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
