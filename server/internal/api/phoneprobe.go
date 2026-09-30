package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"notion5g/server/internal/store"
)

// Sonda A/B con celular por cable ("forma A"). Contrato completo en
// docs/CONTRATO-SONDA-AB.md §1; acá solo la capa HTTP. El celular trae sus
// órdenes (GET .../orders), confirma que las tiene (ack), avisa cambios de
// estado (state), sube resultados de forma idempotente (results) y manda su
// estado en vivo (status). El dashboard crea y cancela órdenes.

const (
	phoneOrdersBody   = 16 << 10
	phoneResultsBody  = 1 << 20
	phoneResultsBatch = 50
	phoneAckMax       = 200
	defaultHorizon    = 6 * time.Hour
	maxHorizon        = 24 * time.Hour

	// Resolución de ASN de las IP de salida de un lote: 3 s por IP, 8 s en
	// total. Lo que no alcance queda "not-resolved" (no se reintenta después).
	phoneASNPerIP = 3 * time.Second
	phoneASNBatch = 8 * time.Second
)

// writeStoreErr traduce los errores del store al código HTTP del contrato.
func writeStoreErr(w http.ResponseWriter, err error, what string) {
	var br store.BadRequestError
	switch {
	case errors.Is(err, store.ErrProbeNotFound):
		writeErr(w, http.StatusNotFound, "sonda no configurada")
	case errors.Is(err, store.ErrOrderNotFound):
		writeErr(w, http.StatusNotFound, "orden desconocida")
	case errors.Is(err, store.ErrOrderIDInUse):
		writeErr(w, http.StatusConflict, store.ErrOrderIDInUse.Error())
	case errors.As(err, &br):
		writeErr(w, http.StatusBadRequest, br.Msg)
	default:
		log.Printf("%s: %v", what, err)
		writeErr(w, http.StatusInternalServerError, "error de base de datos")
	}
}

// requireProbe responde 404 si la sonda no existe.
func (s *Server) requireProbe(ctx context.Context, w http.ResponseWriter, probeID string) (store.ProbeInfo, bool) {
	p, err := s.store.ProbeInfoOf(ctx, probeID)
	if err != nil {
		writeStoreErr(w, err, "sonda")
		return p, false
	}
	return p, true
}

// POST /api/v1/probes/{probe_id}/orders — el dashboard crea órdenes.
func (s *Server) handleCreateOrders(w http.ResponseWriter, r *http.Request) {
	probeID := r.PathValue("probe_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		return
	}
	body, err := readBody(r, phoneOrdersBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req store.OrderRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	orders, existing, err := s.store.CreatePhoneOrders(ctx, probeID, req, time.Now())
	if err != nil {
		writeStoreErr(w, err, "crear órdenes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"existing": existing, "orders": orders})
}

// parseHorizon: duración de Go ("30m", "6h") o segundos enteros; por defecto
// 6 h, máximo 24 h.
func parseHorizon(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultHorizon, true
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, e := strconv.Atoi(v)
		if e != nil {
			return 0, false
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 {
		return 0, false
	}
	return min(d, maxHorizon), true
}

// GET /api/v1/probes/{probe_id}/orders?horizon=6h — el celular trae sus órdenes.
// Con ?view=history&limit=50 (dashboard): todas, más nuevas primero, sin
// barrer ni tocar last_seen.
func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	probeID := r.PathValue("probe_id")
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		return
	}
	now := time.Now().UTC()
	if q.Get("view") == "history" {
		limit := 50
		if n, err := parseIntSafe(q.Get("limit")); err == nil {
			limit = n
		}
		orders, truncated, err := s.store.ListPhoneOrderHistory(ctx, probeID, limit)
		if err != nil {
			writeStoreErr(w, err, "historial de órdenes")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"server_time": store.FormatTS(now), "orders": orders, "truncated": truncated})
		return
	}
	horizon, ok := parseHorizon(q.Get("horizon"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "horizon inválido (duración como 6h o segundos)")
		return
	}
	if _, _, err := s.store.SweepPhoneOrders(ctx, probeID, now); err != nil {
		log.Printf("sonda %s: barrido de órdenes: %v", probeID, err)
	}
	if err := s.store.TouchProbe(ctx, probeID); err != nil {
		log.Printf("sonda %s: touch: %v", probeID, err)
	}
	orders, truncated, closed, err := s.store.ListPhoneOrders(ctx, probeID, horizon, now)
	if err != nil {
		writeStoreErr(w, err, "órdenes")
		return
	}
	probes, err := s.store.ListProbes(ctx, probeID)
	if err != nil || len(probes) == 0 {
		writeStoreErr(w, err, "config de la sonda")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server_time": store.FormatTS(now),
		"probe":       probes[0],
		"orders":      orders,
		"truncated":   truncated,
		"closed":      closed,
	})
}

// POST /api/v1/probes/{probe_id}/orders/ack — {"order_ids":[...]}.
func (s *Server) handleAckOrders(w http.ResponseWriter, r *http.Request) {
	probeID := r.PathValue("probe_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		return
	}
	body, err := readBody(r, phoneOrdersBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		OrderIDs []string `json:"order_ids"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	if len(req.OrderIDs) > phoneAckMax {
		writeErr(w, http.StatusBadRequest, "máximo 200 order_ids por llamada")
		return
	}
	acked, unknown, unchanged, err := s.store.AckPhoneOrders(ctx, probeID, req.OrderIDs, time.Now())
	if err != nil {
		writeStoreErr(w, err, "ack de órdenes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acked": acked, "unknown": unknown, "unchanged": unchanged})
}

// POST /api/v1/probes/{probe_id}/orders/cancel — {"order_ids":[...]} |
// {"batch_id":"..."} | {"all_open":true}.
func (s *Server) handleCancelOrders(w http.ResponseWriter, r *http.Request) {
	probeID := r.PathValue("probe_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		return
	}
	body, err := readBody(r, phoneOrdersBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req store.CancelRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	if !req.AllOpen && req.BatchID == "" && len(req.OrderIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "indique order_ids, batch_id o all_open")
		return
	}
	if len(req.OrderIDs) > phoneAckMax {
		writeErr(w, http.StatusBadRequest, "máximo 200 order_ids por llamada")
		return
	}
	cancelled, unknown, unchanged, err := s.store.CancelPhoneOrders(ctx, probeID, req, time.Now())
	if err != nil {
		writeStoreErr(w, err, "cancelar órdenes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled, "unknown": unknown, "unchanged": unchanged})
}

// POST /api/v1/probes/{probe_id}/orders/{order_id}/state — cambios de estado
// sin resultado: {"status":"running","at":"...","result_id":"...","error":null}.
// Una orden reboot_router (§6) además acepta "step" (progreso) y se cierra por
// acá: {"status":"done","result":{"ssh_ok":true,...}} o
// {"status":"failed","error":"ssh-auth","result":{...}}.
func (s *Server) handleOrderState(w http.ResponseWriter, r *http.Request) {
	probeID, orderID := r.PathValue("probe_id"), r.PathValue("order_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		return
	}
	body, err := readBody(r, phoneOrdersBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		Status   string          `json:"status"`
		At       *string         `json:"at"`
		ResultID *string         `json:"result_id"`
		Error    *string         `json:"error"`
		Step     *string         `json:"step"`
		Result   json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	switch req.Status {
	case "delivered", "running", "interrupted", "expired":
	case "done", "failed":
		// Solo órdenes reboot_router: el store responde 400 "done/failed se
		// cierran con /results" si es una prueba.
	default:
		writeErr(w, http.StatusBadRequest, "status debe ser delivered, running, interrupted o expired (done/failed solo en reboot_router)")
		return
	}
	step := ""
	if req.Step != nil {
		step = strings.TrimSpace(*req.Step)
		if step != "" && !stepRe.MatchString(step) {
			writeErr(w, http.StatusBadRequest, "step inválido (minúsculas, números, '_' y '-', hasta 32)")
			return
		}
	}
	var result json.RawMessage
	if t := bytes.TrimSpace(req.Result); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(t))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil || obj == nil {
			writeErr(w, http.StatusBadRequest, "result debe ser un objeto JSON")
			return
		}
		stripSecrets(obj) // la clave SSH nunca va al backend (§6.2)
		if result, err = json.Marshal(obj); err != nil {
			writeErr(w, http.StatusBadRequest, "result inválido")
			return
		}
	}
	var at *time.Time
	if req.At != nil && strings.TrimSpace(*req.At) != "" {
		t, err := store.ParseTS(*req.At)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "at inválido (RFC 3339)")
			return
		}
		at = &t
	}
	resultID, errMsg := "", ""
	if req.ResultID != nil && *req.ResultID != "" {
		if !store.ValidClientID(*req.ResultID) {
			writeErr(w, http.StatusBadRequest, "result_id inválido")
			return
		}
		resultID = *req.ResultID
	}
	if req.Error != nil {
		errMsg = *req.Error
	}
	cur, ignored, err := s.store.SetPhoneOrderState(ctx, probeID, orderID, store.PhoneStateUpdate{
		Status: req.Status, At: at, ResultID: resultID, Error: errMsg, Step: step, Result: result,
	}, time.Now())
	if err != nil {
		writeStoreErr(w, err, "estado de orden")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": orderID, "status": cur, "ignored": ignored})
}

// ------------------------------------------------------------------ resultados

// phoneResultItem: los campos del resultado que mira el servidor, leídos con
// tipos laxos (un campo opcional con un tipo raro cuenta como ausente: el
// resultado es un dato de campo y no se rechaza por eso).
type phoneResultItem struct {
	obj map[string]any
	raw json.RawMessage

	resultID, probeID, deviceIDClient, measuredBy string
	orderID                                       string
	testStatus, netPath                           string
	started                                       time.Time
	routingTable                                  string
	confirmed                                     bool
	ruleEndTable                                  *string // nil = mikrotik_rule_end null o sin table
	egressIP, egressIPEnd                         string
	egressAddr                                    netip.Addr
	egressEndAddr                                 netip.Addr
}

func objStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func objNum(m map[string]any, k string) *float64 {
	if n, ok := m[k].(json.Number); ok {
		if f, err := n.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

func objBool(m map[string]any, k string) *bool {
	if b, ok := m[k].(bool); ok {
		return &b
	}
	return nil
}

// splitResults acepta {"results":[...]}, un arreglo solo, o un único resultado.
func splitResults(body []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var items []json.RawMessage
		err := json.Unmarshal(trimmed, &items)
		return items, err
	}
	var wrap map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &wrap); err != nil {
		return nil, err
	}
	if res, ok := wrap["results"]; ok {
		var items []json.RawMessage
		if err := json.Unmarshal(res, &items); err != nil {
			return nil, errors.New("results debe ser un arreglo")
		}
		return items, nil
	}
	return []json.RawMessage{json.RawMessage(trimmed)}, nil
}

// parseResultItem valida lo obligatorio (contrato §1.4 regla 1). Devuelve el
// mensaje de error para el ítem ("" si es válido).
func parseResultItem(raw json.RawMessage, probeID string) (phoneResultItem, string) {
	it := phoneResultItem{raw: raw}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&it.obj); err != nil || it.obj == nil {
		return it, "el resultado debe ser un objeto JSON"
	}
	m := it.obj
	it.resultID = objStr(m, "result_id")
	if !store.ValidClientID(it.resultID) {
		return it, "result_id inválido"
	}
	it.probeID = objStr(m, "probe_id")
	if it.probeID != "" && it.probeID != probeID {
		return it, "probe_id no coincide con la sonda de la ruta"
	}
	it.probeID = probeID
	it.testStatus = objStr(m, "test_status")
	if it.testStatus != "done" && it.testStatus != "failed" {
		return it, "test_status debe ser done o failed"
	}
	it.netPath = objStr(m, "net_path")
	if it.netPath != "ethernet" && it.netPath != "wifi" {
		return it, "net_path debe ser ethernet o wifi"
	}
	t, err := store.ParseTS(objStr(m, "test_started_at"))
	if err != nil {
		return it, "test_started_at inválido (RFC 3339)"
	}
	it.started = t
	it.measuredBy = strings.TrimSpace(objStr(m, "measured_by"))
	if it.measuredBy == "" || len(it.measuredBy) > 64 {
		return it, "falta measured_by"
	}
	it.deviceIDClient = objStr(m, "device_id")
	it.orderID = objStr(m, "order_id")
	it.routingTable = objStr(m, "routing_table")
	if b := objBool(m, "mikrotik_confirmed"); b != nil {
		it.confirmed = *b
	}
	if end, ok := m["mikrotik_rule_end"].(map[string]any); ok {
		if tbl, ok := end["table"].(string); ok {
			it.ruleEndTable = &tbl
		}
	}
	it.egressIP, it.egressIPEnd = objStr(m, "egress_ip"), objStr(m, "egress_ip_end")
	if a, err := netip.ParseAddr(strings.TrimSpace(it.egressIP)); err == nil {
		it.egressAddr = normalizeAddr(a)
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(it.egressIPEnd)); err == nil {
		it.egressEndAddr = normalizeAddr(a)
	}
	return it, ""
}

// phoneVerdict: clasificación de red de un resultado del celular (§1.4 regla 4).
type phoneVerdict struct {
	route, confidence, reason string
}

// phoneClassInput: lo que decide la clasificación, ya resuelto.
type phoneClassInput struct {
	netPath       string
	confirmed     bool // Ethernet con la regla confirmada en el MikroTik
	ruleChanged   bool // mikrotik_rule_end.table ≠ routing_table
	knownTable    bool // la tabla es de un equipo de la sonda
	ipChanged     bool // la salida cambió de red a mitad de la prueba (ver egressChanged)
	asn           int  // ASN resuelto (0 = no)
	expected      int  // ASN esperado del equipo atribuido (0 = no se sabe)
	otherExpected map[int]bool
}

// classifyPhoneResult aplica la primera regla que corresponda (tabla del contrato).
func classifyPhoneResult(in phoneClassInput) phoneVerdict {
	switch {
	case in.netPath == "wifi":
		return phoneVerdict{"other-network", "high", "phone-wifi"}
	case !in.confirmed:
		return phoneVerdict{"unknown", "low", "probe-route-unconfirmed"}
	case in.ruleChanged:
		return phoneVerdict{"unknown", "low", "probe-route-changed"}
	case !in.knownTable:
		// Regla confirmada, pero en una tabla que no es de ningún equipo de la
		// sonda: no hay router al que atribuirla (ver Desviaciones del contrato).
		return phoneVerdict{"unknown", "low", "probe-route-unconfirmed"}
	case in.ipChanged:
		return phoneVerdict{"unknown", "low", "network-changed-mid-test"}
	case in.asn > 0 && in.expected > 0 && in.asn == in.expected && in.otherExpected[in.asn]:
		return phoneVerdict{"router", "medium", "probe-asn-ambiguous"}
	case in.asn > 0 && in.expected > 0 && in.asn == in.expected:
		return phoneVerdict{"router", "high", "probe-asn-match"}
	case in.asn > 0 && in.otherExpected[in.asn]:
		return phoneVerdict{"other-network", "high", "probe-asn-other-router"}
	case in.asn > 0 && in.expected > 0:
		return phoneVerdict{"unknown", "low", "probe-asn-mismatch"}
	default:
		return phoneVerdict{"router", "medium", "probe-table-confirmed"}
	}
}

// egressChanged: la IP de salida del inicio y la del final son de redes
// distintas. Con CGNAT (Movistar, por ejemplo) cada conexión puede salir por
// otra IP pública del mismo operador, así que dos IP distintas con el mismo ASN
// no son un cambio de red. Si falta alguno de los dos ASN, se compara la IP.
func egressChanged(it phoneResultItem, asns map[netip.Addr]asnInfo) bool {
	if it.egressIP == "" || it.egressIPEnd == "" || strings.EqualFold(it.egressIP, it.egressIPEnd) {
		return false
	}
	a, okA := asns[it.egressAddr]
	b, okB := asns[it.egressEndAddr]
	if okA && okB && a.ASN > 0 {
		return a.ASN != b.ASN
	}
	return true
}

// resolveEgressASNs resuelve el ASN de cada IP única del lote: 3 s por IP y 8 s
// en total. Lo que no alcance queda sin resolver.
func (s *Server) resolveEgressASNs(ctx context.Context, items []phoneResultItem) map[netip.Addr]asnInfo {
	out := map[netip.Addr]asnInfo{}
	seen := map[netip.Addr]bool{}
	bctx, cancel := context.WithTimeout(ctx, phoneASNBatch)
	defer cancel()
	var addrs []netip.Addr
	for _, it := range items {
		addrs = append(addrs, it.egressAddr, it.egressEndAddr)
	}
	for _, a := range addrs {
		if !a.IsValid() || seen[a] {
			continue
		}
		seen[a] = true
		if bctx.Err() != nil {
			break
		}
		ictx, icancel := context.WithTimeout(bctx, phoneASNPerIP)
		info, ok := s.asn.Lookup(ictx, a)
		icancel()
		if ok {
			out[a] = info
		}
	}
	return out
}

// POST /api/v1/probes/{probe_id}/results — subida idempotente de resultados.
// La atribución al router y la clasificación de red las decide el servidor.
// La IP de la petición HTTP NO se usa (llega por el WiFi de la oficina): este
// camino nunca llama a noteDeviceMeasurement, SetDeviceEgress, enqueueNetEnrich
// ni al ledger de test_id, y no rellena la ubicación desde device_locations.
func (s *Server) handlePostResults(w http.ResponseWriter, r *http.Request) {
	probeID := r.PathValue("probe_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		cancel()
		return
	}
	body, err := readBody(r, phoneResultsBody)
	if err != nil {
		cancel()
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	raws, err := splitResults(body)
	if err != nil {
		cancel()
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	if len(raws) > phoneResultsBatch {
		cancel()
		writeErr(w, http.StatusBadRequest, "máximo 50 resultados por lote")
		return
	}
	targets, err := s.store.ProbeTargetsAll(ctx, probeID)
	if err != nil {
		cancel()
		writeStoreErr(w, err, "equipos de la sonda")
		return
	}
	// ASN esperado de cada equipo: el configurado, o el de device_egress.
	expected := map[string]int{} // device_id -> ASN
	for _, t := range targets {
		if t.ExpectedASN.Valid {
			expected[t.DeviceID] = t.ExpectedASN.Value
		} else if ref, ok := s.store.DeviceEgressOf(ctx, t.DeviceID); ok && ref.ASN != nil {
			expected[t.DeviceID] = *ref.ASN
		}
	}
	cancel()

	items := make([]phoneResultItem, len(raws))
	errs := make([]string, len(raws))
	for i, raw := range raws {
		items[i], errs[i] = parseResultItem(raw, probeID)
	}
	var valid []phoneResultItem
	for i := range items {
		if errs[i] == "" {
			valid = append(valid, items[i])
		}
	}
	asns := s.resolveEgressASNs(r.Context(), valid)

	dbctx, dbcancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer dbcancel()
	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(raws))
	for i, it := range items {
		if errs[i] != "" {
			var rid any
			if it.resultID != "" {
				rid = it.resultID
			} else if it.obj != nil {
				rid = it.obj["result_id"]
			}
			out = append(out, map[string]any{"result_id": rid, "status": "error", "error": errs[i], "retryable": false})
			continue
		}
		out = append(out, s.storeResult(dbctx, it, targets, expected, asns, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// resultDropKeys: claves que pone siempre el servidor (se borran del payload).
var resultDropKeys = append(append([]string{}, netReservedKeys...), "egress_asn", "slot", "device_id_client")

// storeResult atribuye, clasifica e inserta un resultado ya validado.
func (s *Server) storeResult(ctx context.Context, it phoneResultItem, targets []store.ProbeTarget,
	expected map[string]int, asns map[netip.Addr]asnInfo, now time.Time) map[string]any {
	m := it.obj

	// Atribución (regla 3): Ethernet con la regla confirmada y sin cambios.
	ruleChanged := it.ruleEndTable != nil && *it.ruleEndTable != it.routingTable
	var target *store.ProbeTarget
	if it.netPath == "ethernet" && it.confirmed && !ruleChanged && it.routingTable != "" {
		for i := range targets {
			if targets[i].RoutingTable == it.routingTable {
				target = &targets[i]
				break
			}
		}
	}
	deviceID, slot := it.measuredBy, ""
	if target != nil {
		deviceID, slot = target.DeviceID, target.Slot
	}

	// Clasificación (regla 4).
	in := phoneClassInput{
		netPath:       it.netPath,
		confirmed:     it.netPath == "ethernet" && it.confirmed,
		ruleChanged:   ruleChanged,
		knownTable:    target != nil,
		ipChanged:     egressChanged(it, asns),
		otherExpected: map[int]bool{},
	}
	info, resolved := asns[it.egressAddr]
	asnStatus := "no-ip"
	if it.egressIP != "" {
		asnStatus = "not-resolved"
		if resolved {
			asnStatus = "ok"
			in.asn = info.ASN
		}
	}
	var expectedASN any
	if target != nil {
		if e := expected[target.DeviceID]; e > 0 {
			in.expected, expectedASN = e, e
		}
	}
	for _, t := range targets {
		if target != nil && t.DeviceID == target.DeviceID {
			continue
		}
		if e := expected[t.DeviceID]; e > 0 {
			in.otherExpected[e] = true
		}
	}
	v := classifyPhoneResult(in)

	var egressASN, egressASNName any
	var ncASN *int
	if resolved {
		a := info.ASN
		egressASN, egressASNName, ncASN = a, info.Name, &a
	}
	netMap := map[string]any{
		"method": "phone-probe", "route": v.route, "confidence": v.confidence, "reason": v.reason,
		"egress_asn": egressASN, "egress_asn_name": egressASNName, "expected_asn": expectedASN, "asn_status": asnStatus,
	}

	// Campos del servidor en raw (el dashboard lee raw).
	var slotVal any
	if slot != "" {
		slotVal = slot
	}
	set := map[string]any{
		"probe_id": it.probeID, "device_id": deviceID, "slot": slotVal,
		"ts": store.FormatTS(it.started), "test_started_at": store.FormatTS(it.started),
		"net_route": v.route, "net": netMap,
	}
	if it.deviceIDClient != "" && it.deviceIDClient != deviceID {
		set["device_id_client"] = it.deviceIDClient
	}
	finished := ""
	if t, err := store.ParseTS(objStr(m, "test_finished_at")); err == nil {
		finished = store.FormatTS(t)
		set["test_finished_at"] = finished
	}
	gpsTS := ""
	if t, err := store.ParseTS(objStr(m, "gps_ts")); err == nil {
		gpsTS = store.FormatTS(t)
		set["gps_ts"] = gpsTS
	}
	egressCol := it.egressIP
	if it.netPath == "wifi" {
		// Por WiFi la IP de salida es la de la oficina, no la de un router:
		// se guarda recortada, como las IP de celulares en el resto del backend.
		egressCol = truncIP(it.egressIP)
		if _, ok := m["egress_ip"]; ok {
			set["egress_ip"] = nilIfEmpty(egressCol)
		}
		if _, ok := m["egress_ip_end"]; ok {
			set["egress_ip_end"] = nilIfEmpty(truncIP(it.egressIPEnd))
		}
	}
	// Secretos (§0: nunca van en resultados; el celular no debería mandarlos,
	// esto es una segunda barrera, a cualquier nivel del objeto).
	stripSecrets(m)
	clean, err := json.Marshal(m)
	if err != nil {
		return map[string]any{"result_id": it.resultID, "status": "error", "error": "resultado inválido: " + err.Error(), "retryable": false}
	}
	payload, err := withServerFields(clean, resultDropKeys, set)
	if err != nil {
		return map[string]any{"result_id": it.resultID, "status": "error", "error": "resultado inválido: " + err.Error(), "retryable": false}
	}

	cols := store.ProbeResultCols{
		ResultID: it.resultID, OrderID: it.orderID, ProbeID: it.probeID, MeasuredBy: it.measuredBy,
		RoutingTable: it.routingTable, Slot: slot, DeviceID: deviceID, Attributed: target != nil,
		SelectionReason: objStr(m, "selection_reason"), TestStartedAt: it.started, TestFinishedAt: finished,
		TestStatus: it.testStatus, Error: objStr(m, "error"), ClockSkewS: objNum(m, "clock_skew_s"),
		LossPct: objNum(m, "loss_pct"), GPSTS: gpsTS, GPSAgeS: objNum(m, "gps_age_s"),
		LocationStatus: objStr(m, "location_status"), LocationSource: objStr(m, "location_source"),
		ExecutedOffline: objBool(m, "executed_offline"), NetPath: it.netPath,
		TargetHost: objStr(m, "target_host"), EgressIP: egressCol,
	}
	nc := store.NetClassification{Route: v.route, Confidence: v.confidence, ASN: ncASN}
	res, err := s.store.InsertProbeResult(ctx, payload, cols, nc, now)
	if err != nil {
		var bad store.ErrBadResult
		if errors.As(err, &bad) {
			return map[string]any{"result_id": it.resultID, "status": "error", "error": bad.Msg, "retryable": false}
		}
		log.Printf("sonda %s: guardar resultado %s: %v", it.probeID, it.resultID, err)
		return map[string]any{"result_id": it.resultID, "status": "error", "error": "error de base de datos", "retryable": true}
	}
	if res.Duplicate {
		return map[string]any{"result_id": it.resultID, "status": "duplicate", "measurement_id": res.MeasurementID}
	}
	var orderID, orderStatus any
	if it.orderID != "" {
		orderID = it.orderID
	}
	if res.OrderStatus != "" {
		orderStatus = res.OrderStatus
	}
	return map[string]any{
		"result_id": it.resultID, "status": "inserted", "measurement_id": res.MeasurementID,
		"order_id": orderID, "order_linked": res.OrderLinked, "order_status": orderStatus,
		"device_id": deviceID, "slot": slotVal,
		"net_route": v.route, "confidence": v.confidence, "reason": v.reason,
		"egress_asn": egressASN, "egress_asn_name": egressASNName,
	}
}

func truncIP(s string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return privacyPrefixStr(a)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ------------------------------------------------------------------ estado en vivo

// secretKeys: nunca se guardan en el estado en vivo ni en resultados (el
// celular no debería mandarlas; esto es una segunda barrera). "password" cubre
// un {"mikrotik":{"password":...}} anidado. Además isSecretKey borra
// cualquier clave que contenga "password"/"passwd" (p. ej. ssh_password_A y
// ssh_password_B, como las guarda la app, §6.2), sin importar mayúsculas.
var secretKeys = map[string]bool{"api_key": true, "prod_api_key": true, "mikrotik_password": true, "password": true,
	"ssh_password": true, "pass": true, "pwd": true, "secret": true}

func isSecretKey(k string) bool {
	l := strings.ToLower(k)
	return secretKeys[l] || strings.Contains(l, "password") || strings.Contains(l, "passwd")
}

// stepRe: paso de una orden reboot_router (ssh, waiting_back…).
var stepRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func stripSecrets(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if isSecretKey(k) {
				delete(t, k)
				continue
			}
			stripSecrets(x)
		}
	case []any:
		for _, x := range t {
			stripSecrets(x)
		}
	}
}

// POST /api/v1/probes/{probe_id}/status — el celular manda su estado en vivo.
func (s *Server) handlePostProbeStatus(w http.ResponseWriter, r *http.Request) {
	probeID := r.PathValue("probe_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		return
	}
	body, err := readBody(r, phoneOrdersBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil || obj == nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: se espera un objeto")
		return
	}
	stripSecrets(obj)
	// Salud por router (§6.2): "routers" debe ser un objeto {"A":{...},...};
	// si viene con otra forma se descarta ese bloque (no el estado entero).
	if v, ok := obj["routers"]; ok && v != nil {
		if _, isObj := v.(map[string]any); !isObj {
			delete(obj, "routers")
		}
	}
	u := store.ProbeStatusUpdate{
		MeasuredBy:       objStr(obj, "measured_by"),
		Phase:            objStr(obj, "phase"),
		ServiceStartedAt: objStr(obj, "service_started_at"),
	}
	if n, ok := obj["seq"].(json.Number); ok {
		if v, err := n.Int64(); err == nil {
			u.Seq = &v
		}
	}
	if mk, ok := obj["mikrotik"].(map[string]any); ok {
		u.CurrentTable = objStr(mk, "current_table")
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido")
		return
	}
	now := time.Now().UTC()
	_, prev, err := s.store.UpsertProbeStatus(ctx, probeID, raw, u, now)
	if err != nil {
		writeStoreErr(w, err, "estado de la sonda")
		return
	}
	if prev != "" {
		log.Printf("sonda %s: cambió el celular de %s a %s", probeID, prev, u.MeasuredBy)
	}
	if err := s.store.TouchProbe(ctx, probeID); err != nil {
		log.Printf("sonda %s: touch: %v", probeID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "server_time": store.FormatTS(now)})
}

// GET /api/v1/probes/{probe_id}/status — lo que lee el dashboard.
func (s *Server) handleGetProbeStatus(w http.ResponseWriter, r *http.Request) {
	probeID := r.PathValue("probe_id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, ok := s.requireProbe(ctx, w, probeID); !ok {
		return
	}
	st, _, err := s.store.ProbeStatusOf(ctx, probeID, time.Now().UTC())
	if err != nil {
		writeStoreErr(w, err, "estado de la sonda")
		return
	}
	writeJSON(w, http.StatusOK, st)
}
