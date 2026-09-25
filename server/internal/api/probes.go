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
		enq, err := s.store.EnqueueDueProbeCycles(ctx, time.Now())
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
	ids, err := s.store.EnqueueProbeCycle(ctx, r.PathValue("probe_id"), body.RequestedBy)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command_ids": ids})
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
	writeJSON(w, http.StatusOK, probes[0])
}
