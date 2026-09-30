package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"notion5g/server/internal/store"
)

// Reinicio remoto de un router bajo prueba (docs/CONTRATO-SONDA-AB.md §6.3).
// El backend no se conecta al router: deja una orden reboot_router en la cola
// del celular de la sonda, que la ejecuta por SSH a través del MikroTik entre
// pruebas y la cierra con POST .../orders/{order_id}/state (done/failed +
// result). La recomendación solo se muestra: nunca se reinicia solo.

// POST /api/v1/devices/{device_id}/reboot
// {"order_id":"<uuid>","requested_by":"dashboard","reason":"manual|recommended","force":false}
func (s *Server) handleRebootDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("device_id")
	body, err := readBody(r, phoneOrdersBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req store.RebootRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	// Barrido primero: una orden vencida que el planificador todavía no cerró
	// no debe contar como "reinicio en curso".
	if _, _, err := s.store.SweepPhoneOrders(ctx, "", now); err != nil {
		log.Printf("reinicio de %s: barrido de órdenes: %v", deviceID, err)
	}
	order, existing, err := s.store.CreateRebootOrder(ctx, deviceID, req, now)
	var conflict store.RebootConflictError
	switch {
	case errors.As(err, &conflict):
		resp := map[string]any{"error": conflict.Msg, "in_progress": conflict.InProgress,
			"last_reboot_at": nilIfEmpty(conflict.LastRebootAt), "cooldown_until": nilIfEmpty(conflict.CooldownUntil)}
		if conflict.Order != nil {
			resp["order"] = conflict.Order
		}
		writeJSON(w, http.StatusConflict, resp)
		return
	case errors.Is(err, store.ErrRebootAppOutdated):
		// 409 sin cooldown_until/last_reboot_at: el dashboard lo muestra como
		// error y no ofrece forzar (force no cambia nada).
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "in_progress": false, "app_outdated": true})
		return
	case errors.Is(err, store.ErrRebootNoProbe):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeStoreErr(w, err, "reinicio de "+deviceID)
		return
	}
	if !existing {
		log.Printf("reinicio de %s pedido por %s (%s, orden %s, sonda %s, slot %s)", deviceID,
			deref(order.RequestedBy), deref(order.RebootReason), order.OrderID, order.ProbeID, deref(order.Slot))
	}
	writeJSON(w, http.StatusOK, map[string]any{"existing": existing, "order": order})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// attachDeviceReboot agrega el bloque reboot a los equipos de una sonda de
// celular. Best-effort: si falla, los equipos salen sin él.
func (s *Server) attachDeviceReboot(ctx context.Context, devices []store.DeviceSummary) {
	infos, err := s.store.RebootInfos(ctx, time.Now().UTC())
	if err != nil {
		log.Printf("recomendación de reinicio: %v", err)
		return
	}
	for i := range devices {
		if info, ok := infos[devices[i].DeviceID]; ok {
			devices[i].Reboot = info
			applyPhoneHealth(&devices[i], info)
		}
	}
}

// applyPhoneHealth: un router sin agente (el Notion 4G) solo reporta cuando el
// celular lo mide, así que por last_seen quedaría "sin señal" entre pruebas.
// Si el celular lo ve con Internet ahora (salud fresca), está en línea, y su
// señal es la que el celular leyó de su página web.
func applyPhoneHealth(d *store.DeviceSummary, info *store.RebootInfo) {
	if info.AgentLastSeen != nil || info.Health == nil || !info.Health.Fresh || info.Health.InternetOK == nil {
		return
	}
	d.Online = *info.Health.InternetOK
	d.StatusSource = "phone-health"
	if info.Health.CheckedAt != nil {
		if t, err := store.ParseTS(*info.Health.CheckedAt); err == nil && t.After(mustTS(d.LastSeen)) {
			d.LastSeen = *info.Health.CheckedAt
			ago := max(time.Since(t).Seconds(), 0)
			d.LastSeenSecondsAgo = &ago
		}
	}
	if info.Health.RTTMs != nil {
		d.PingMs = info.Health.RTTMs
	}
	if info.Health.LossPct != nil {
		d.LossPct = info.Health.LossPct
	}
	if sig := info.Health.Signal; sig != nil {
		if sig.Operator != "" {
			d.Operator = sig.Operator
		}
		if sig.RAT != "" {
			d.RAT = sig.RAT
		}
		if sig.BandLTE != nil {
			d.BandLTE = sig.BandLTE
		}
		if sig.RSRPDbm != nil {
			d.RSRPDbm = sig.RSRPDbm
		}
		d.RSRQDb, d.SINRDb = sig.RSRQDb, sig.SINRDb
		if sig.UptimeS != nil {
			d.UptimeS = sig.UptimeS
		}
	}
}

func mustTS(v string) time.Time {
	t, _ := store.ParseTS(v)
	return t
}

// attachProbeReboot agrega el bloque reboot a cada equipo de las sondas de
// celular. Best-effort, como attachPhoneStatus.
func (s *Server) attachProbeReboot(ctx context.Context, probes []store.Probe) {
	now := time.Now().UTC()
	for i := range probes {
		if probes[i].Runner != store.RunnerPhone {
			continue
		}
		infos, err := s.store.ProbeRebootInfos(ctx, probes[i].ProbeID, now)
		if err != nil {
			log.Printf("sonda %s: recomendación de reinicio: %v", probes[i].ProbeID, err)
			continue
		}
		for j := range probes[i].Targets {
			if info, ok := infos[probes[i].Targets[j].DeviceID]; ok {
				probes[i].Targets[j].Reboot = info
			}
		}
	}
}
