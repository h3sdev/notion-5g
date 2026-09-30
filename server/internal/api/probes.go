package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"notion5g/server/internal/store"
)

// probeSchedulerEvery: cada cuánto revisa el backend si a alguna sonda le toca
// ciclo automático. El intervalo mínimo configurable es 60 s, así que 30 s de
// resolución alcanzan.
const probeSchedulerEvery = 30 * time.Second

func (s *Server) probeSchedulerLoop() {
	t := time.NewTicker(probeSchedulerEvery)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		now := time.Now()
		// Primero el barrido de órdenes de celular (vencidas / sin cierre), así
		// el dashboard las ve aunque el celular no consulte, y el ciclo
		// automático no cuenta como abiertas las que ya vencieron.
		if exp, intr, err := s.store.SweepPhoneOrders(ctx, "", now); err != nil {
			log.Printf("sondas: barrido de órdenes: %v", err)
		} else if exp+intr > 0 {
			log.Printf("sondas: barrido: %d vencidas, %d sin cierre", exp, intr)
		}
		enq, err := s.store.EnqueueDueProbeCycles(ctx, now)
		cancel()
		if err != nil {
			log.Printf("sondas: %v", err)
		}
		for probeID, ids := range enq {
			log.Printf("sondas: ciclo automático de %s encolado (%d pruebas)", probeID, len(ids))
		}
	}
}

// PUT /api/v1/probes/{probe_id}: crea o reemplaza la configuración de una sonda.
// {"label":"hAP oficina","interval_s":900,"duration_s":10,
//
//	"targets":[{"device_id":"router-R52...","routing_table":"to-notion"},
//	           {"device_id":"router-4g","routing_table":"to-4g","send_heartbeat":true}]}
func (s *Server) handlePutProbe(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 16<<10)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var p store.Probe
	if err := json.Unmarshal(body, &p); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	p.ProbeID = r.PathValue("probe_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.UpsertProbe(ctx, p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeProbe(ctx, w, p.ProbeID)
}

func (s *Server) handleListProbes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	probes, err := s.store.ListProbes(ctx, "")
	if err != nil {
		log.Printf("list probes: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	s.attachPhoneStatus(ctx, probes)
	writeJSON(w, http.StatusOK, probes)
}

func (s *Server) handleGetProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	s.writeProbe(ctx, w, r.PathValue("probe_id"))
}

// GET /api/v1/probes/{probe_id}/config: lo que consulta la propia sonda (qué
// equipos tiene, a cuáles mandarles heartbeat). A diferencia de GET
// /probes/{id}, cuenta como señal de vida.
func (s *Server) handleProbeConfig(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	probeID := r.PathValue("probe_id")
	if err := s.store.TouchProbe(ctx, probeID); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.writeProbe(ctx, w, probeID)
}

// POST /api/v1/probes/{probe_id}/cycle: una prueba por equipo, en orden.
func (s *Server) handleProbeCycle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestedBy string `json:"requested_by"`
	}
	if raw, err := readBody(r, 4<<10); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	if body.RequestedBy == "" {
		body.RequestedBy = "api"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ids, orderIDs, err := s.store.EnqueueProbeCycleOrders(ctx, r.PathValue("probe_id"), body.RequestedBy)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// En una sonda de celular, command_ids son los id numéricos de las órdenes.
	writeJSON(w, http.StatusOK, map[string]any{"command_ids": ids, "order_ids": orderIDs})
}

func (s *Server) writeProbe(ctx context.Context, w http.ResponseWriter, probeID string) {
	probes, err := s.store.ListProbes(ctx, probeID)
	if err != nil {
		log.Printf("get probe: %v", err)
		writeErr(w, http.StatusInternalServerError, "error de consulta")
		return
	}
	if len(probes) == 0 {
		writeErr(w, http.StatusNotFound, "sonda no configurada")
		return
	}
	s.attachPhoneStatus(ctx, probes)
	writeJSON(w, http.StatusOK, probes[0])
}

// attachPhoneStatus agrega phone_status (último estado en vivo del celular) a
// las sondas que tengan uno. Best-effort: si falla, las sondas salen sin él.
func (s *Server) attachPhoneStatus(ctx context.Context, probes []store.Probe) {
	all, err := s.store.ProbeStatuses(ctx, time.Now().UTC())
	if err != nil {
		log.Printf("estado de sondas: %v", err)
		return
	}
	for i := range probes {
		if st, ok := all[probes[i].ProbeID]; ok {
			probes[i].PhoneStatus = &st
		}
	}
}
