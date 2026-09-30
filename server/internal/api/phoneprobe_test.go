package api

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"notion5g/server/internal/store"
)

// Tests de la sonda A/B con celular (contrato docs/CONTRATO-SONDA-AB.md §1.6).

const phoneProbePUT = `{"label":"hAP ac2 oficina","runner":"phone","interval_s":0,"duration_s":10,
 "targets":[{"slot":"A","device_id":"router-A","routing_table":"to-A","label":"Notion 5G"},
            {"slot":"B","device_id":"router-B","routing_table":"to-B","label":"Notion 4G","expected_asn":3816}]}`

func setupPhoneProbe(t *testing.T) (probeHarness, *store.Store) {
	t.Helper()
	s, st := testAPI(t)
	h := probeHarness{t, s}
	if code, obj, _ := h.do("PUT", "/api/v1/probes/hap-oficina", phoneProbePUT); code != 200 {
		t.Fatalf("PUT sonda: %d %v", code, obj)
	}
	return h, st
}

// cacheASN precarga el caché del resolvedor (sin red).
func cacheASN(s *Server, ip string, asn int, name string) {
	a := netip.MustParseAddr(ip)
	s.asn.putCached(privacyPrefix(a), asnCacheEntry{info: asnInfo{ASN: asn, Name: name}, ok: true, exp: time.Now().Add(time.Hour)})
}

func orderList(t *testing.T, obj map[string]any) []map[string]any {
	t.Helper()
	raw, ok := obj["orders"].([]any)
	if !ok {
		t.Fatalf("sin orders: %v", obj)
	}
	out := make([]map[string]any, len(raw))
	for i, o := range raw {
		out[i] = o.(map[string]any)
	}
	return out
}

func resultItems(t *testing.T, obj map[string]any) []map[string]any {
	t.Helper()
	raw, ok := obj["results"].([]any)
	if !ok {
		t.Fatalf("sin results: %v", obj)
	}
	out := make([]map[string]any, len(raw))
	for i, o := range raw {
		out[i] = o.(map[string]any)
	}
	return out
}

// result arma un resultado Ethernet confirmado por la tabla dada; extra pisa claves.
func result(id, orderID, table string, extra string) string {
	oid := "null"
	if orderID != "" {
		oid = `"` + orderID + `"`
	}
	body := fmt.Sprintf(`{"result_id":"%s","order_id":%s,"probe_id":"hap-oficina","device_id":"router-A","measured_by":"phone-3fa9c2d1",
"source":"phone-probe","tag":"probe-ab","test_started_at":"2026-09-29T21:05:20.123Z","test_finished_at":"2026-09-29T16:05:47-05:00",
"test_status":"done","routing_table":"%s","mikrotik_confirmed":true,"mikrotik_rule":{"table":"%s"},"mikrotik_rule_end":{"table":"%s"},
"net_path":"ethernet","selection_reason":"requested","down_mbps":85.3,"up_mbps":20.1,"ping_ms":38.2,"loss_pct":0,"clock_skew_s":0.4`,
		id, oid, table, table, table)
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

func postResults(h probeHarness, items ...string) (int, map[string]any) {
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/results", `{"results":[`+strings.Join(items, ",")+`]}`)
	return code, obj
}

func createOrder(t *testing.T, h probeHarness, body string) map[string]any {
	t.Helper()
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/orders", body)
	if code != 200 {
		t.Fatalf("crear orden: %d %v", code, obj)
	}
	return obj
}

// 1 y 2: idempotencia de la creación, y secuencias.
func TestPhoneOrderCreateIdempotent(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	body := `{"order_id":"3f7d2a10-8f2e-4c1b-9a55-0c8d6e1b2f44","target":"A","allow_fallback":true,"requested_by":"dashboard"}`
	first := createOrder(t, h, body)
	if first["existing"] != false {
		t.Fatalf("primera: %v", first)
	}
	o := orderList(t, first)[0]
	if o["status"] != "pending" || o["slot"] != "A" || o["routing_table"] != "to-A" || o["selection_reason"] != "requested" ||
		o["preferred_device"] != "router-A" || o["fallback_device"] != "router-B" || o["batch_id"] != o["order_id"] ||
		o["runner"] != "phone" || o["duration_s"] != float64(10) {
		t.Fatalf("orden mal armada: %v", o)
	}
	for _, k := range []string{"delivered_at", "started_at", "completed_at", "closed_by", "error", "result_id", "measurement_id"} {
		if v, ok := o[k]; !ok || v != nil {
			t.Fatalf("%s debe ir null y presente: %v", k, o)
		}
	}
	ea, _ := time.Parse(time.RFC3339, o["execute_at"].(string))
	na, _ := time.Parse(time.RFC3339, o["not_after"].(string))
	if na.Sub(ea) != 30*time.Minute {
		t.Fatalf("not_after por defecto: %v %v", ea, na)
	}
	// Mismo order_id con otro cuerpo: no crea nada.
	again := createOrder(t, h, `{"order_id":"3f7d2a10-8f2e-4c1b-9a55-0c8d6e1b2f44","target":"B"}`)
	if again["existing"] != true || len(orderList(t, again)) != 1 || orderList(t, again)[0]["id"] != o["id"] {
		t.Fatalf("repetida: %v", again)
	}

	seq := `{"order_id":"b1c9aaaa-0000-4000-8000-000000000001","target":"sequence","count":4,"spacing_s":300,"first":"A",
		"execute_at":"2026-09-29T22:00:00Z","duration_s":10,"requested_by":"dashboard"}`
	s1 := orderList(t, createOrder(t, h, seq))
	if len(s1) != 4 {
		t.Fatalf("secuencia: %v", s1)
	}
	for i, o := range s1 {
		wantSlot := []string{"A", "B", "A", "B"}[i]
		wantEA := time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC).Add(time.Duration(i) * 5 * time.Minute)
		if o["order_id"] != fmt.Sprintf("b1c9aaaa-0000-4000-8000-000000000001-%d", i+1) || o["slot"] != wantSlot ||
			o["batch_id"] != "b1c9aaaa-0000-4000-8000-000000000001" || o["execute_at"] != wantEA.Format(time.RFC3339) ||
			o["not_after"] != wantEA.Add(5*time.Minute).Format(time.RFC3339) || o["selection_reason"] != "sequence" {
			t.Fatalf("orden %d de la secuencia: %v", i, o)
		}
	}
	s2 := createOrder(t, h, seq)
	if s2["existing"] != true || len(orderList(t, s2)) != 4 {
		t.Fatalf("secuencia repetida: %v", s2)
	}
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history&limit=50", "")
	if n := len(orderList(t, hist)); n != 5 {
		t.Fatalf("deberían existir 5 órdenes, hay %d", n)
	}
}

// 15: order_id reservado y order_id de otra sonda.
func TestPhoneOrderIDReservedAndConflict(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/orders", `{"order_id":"abcdefgh-1","target":"A"}`)
	if code != 400 || obj["error"] != "order_id reservado para secuencias" {
		t.Fatalf("X-1: %d %v", code, obj)
	}
	if code, obj, _ := h.do("PUT", "/api/v1/probes/otra", strings.Replace(phoneProbePUT, "hAP ac2 oficina", "otra", 1)); code != 200 {
		t.Fatalf("PUT otra: %d %v", code, obj)
	}
	createOrder(t, h, `{"order_id":"11111111-2222-4333-8444-555555555555","target":"A"}`)
	code, obj, _ = h.do("POST", "/api/v1/probes/otra/orders", `{"order_id":"11111111-2222-4333-8444-555555555555","target":"A"}`)
	if code != 409 || obj["error"] != "order_id en uso por otra orden" {
		t.Fatalf("otra sonda: %d %v", code, obj)
	}
	// Sonda de script: no se le crean órdenes de celular.
	if code, obj, _ := h.do("PUT", "/api/v1/probes/script", `{"targets":[{"device_id":"x","routing_table":"t"}]}`); code != 200 {
		t.Fatalf("PUT script: %d %v", code, obj)
	}
	code, obj, _ = h.do("POST", "/api/v1/probes/script/orders", `{"target":"A"}`)
	if code != 400 || obj["error"] != "la sonda no la ejecuta un celular" {
		t.Fatalf("sonda script: %d %v", code, obj)
	}
	code, obj, _ = h.do("POST", "/api/v1/probes/nada/orders", `{"target":"A"}`)
	if code != 404 || obj["error"] != "sonda no configurada" {
		t.Fatalf("sonda inexistente: %d %v", code, obj)
	}
}

// 3 y 16: horizonte, barrido, closed y truncated.
func TestPhoneOrdersHorizonSweepClosed(t *testing.T) {
	h, st := setupPhoneProbe(t)
	now := time.Now().UTC()
	later := now.Add(2 * time.Hour).Format(time.RFC3339)
	createOrder(t, h, `{"order_id":"later000-0000-4000-8000-000000000001","target":"A","execute_at":"`+later+`"}`)
	past := now.Add(-2 * time.Hour).Format(time.RFC3339)
	pastNA := now.Add(-1 * time.Hour).Format(time.RFC3339)
	createOrder(t, h, `{"order_id":"past0000-0000-4000-8000-000000000001","target":"B","execute_at":"`+past+`","not_after":"`+pastNA+`"}`)
	createOrder(t, h, `{"order_id":"now00000-0000-4000-8000-000000000001","target":"next"}`)

	code, obj, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?horizon=1h", "")
	if code != 200 {
		t.Fatalf("GET orders: %d %v", code, obj)
	}
	orders := orderList(t, obj)
	if len(orders) != 1 || orders[0]["order_id"] != "now00000-0000-4000-8000-000000000001" {
		t.Fatalf("solo la de ahora: %v", orders)
	}
	if n := orders[0]; n["slot"] != nil || n["routing_table"] != nil || n["device_id"] != "hap-oficina" || n["selection_reason"] != "next" {
		t.Fatalf("orden next: %v", n)
	}
	closed := obj["closed"].([]any)
	if len(closed) != 1 || closed[0].(map[string]any)["order_id"] != "past0000-0000-4000-8000-000000000001" ||
		closed[0].(map[string]any)["status"] != "expired" {
		t.Fatalf("closed: %v", closed)
	}
	if obj["truncated"] != false || obj["probe"].(map[string]any)["runner"] != "phone" || obj["server_time"] == nil {
		t.Fatalf("respuesta: %v", obj)
	}
	if _, ok := obj["probe"].(map[string]any)["phone_status"]; ok {
		t.Fatalf("GET orders no lleva phone_status")
	}
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	for _, o := range orderList(t, hist) {
		if o["order_id"] == "past0000-0000-4000-8000-000000000001" && (o["closed_by"] != "server" || o["error"] != "vencida-sin-ejecutar") {
			t.Fatalf("vencida: %v", o)
		}
	}
	// GET orders cuenta como señal de vida.
	_, p, _ := h.do("GET", "/api/v1/probes/hap-oficina", "")
	if p["online"] != true {
		t.Fatalf("GET orders debe tocar last_seen: %v", p)
	}

	// Una secuencia larga cancelada hace más de 24 h sigue en closed mientras
	// su not_after esté en el futuro.
	farEA := now.Add(72 * time.Hour).Format(time.RFC3339)
	createOrder(t, h, `{"order_id":"farseq00-0000-4000-8000-000000000001","target":"sequence","count":2,"spacing_s":3600,"execute_at":"`+farEA+`"}`)
	if code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/orders/cancel", `{"batch_id":"farseq00-0000-4000-8000-000000000001"}`); code != 200 ||
		len(obj["cancelled"].([]any)) != 2 {
		t.Fatalf("cancelar secuencia: %d %v", code, obj)
	}
	_, _, cl, err := st.ListPhoneOrders(context.Background(), "hap-oficina", 6*time.Hour, now.Add(30*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, c := range cl {
		if strings.HasPrefix(c.OrderID, "farseq00") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("la secuencia cancelada debe seguir en closed: %v", cl)
	}

	// Más de 200 abiertas: truncated.
	for i := 0; i < 5; i++ {
		createOrder(t, h, fmt.Sprintf(`{"order_id":"bulk%04d-0000-4000-8000-000000000000","target":"sequence","count":48,"spacing_s":60}`, i))
	}
	_, obj, _ = h.do("GET", "/api/v1/probes/hap-oficina/orders?horizon=24h", "")
	if obj["truncated"] != true || len(orderList(t, obj)) != 200 {
		t.Fatalf("truncated: %v %d", obj["truncated"], len(orderList(t, obj)))
	}
}

// 4: ack y state.
func TestPhoneOrderAckAndState(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	createOrder(t, h, `{"order_id":"ack00000-0000-4000-8000-000000000001","target":"A"}`)
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/orders/ack", `{"order_ids":["ack00000-0000-4000-8000-000000000001","nope0000-0000-4000-8000-000000000000"]}`)
	if code != 200 || len(obj["acked"].([]any)) != 1 || len(obj["unknown"].([]any)) != 1 {
		t.Fatalf("ack: %d %v", code, obj)
	}
	_, obj, _ = h.do("POST", "/api/v1/probes/hap-oficina/orders/ack", `{"order_ids":["ack00000-0000-4000-8000-000000000001"]}`)
	if len(obj["unchanged"].([]any)) != 1 {
		t.Fatalf("ack repetido: %v", obj)
	}
	path := "/api/v1/probes/hap-oficina/orders/ack00000-0000-4000-8000-000000000001/state"
	code, obj, _ = h.do("POST", path, `{"status":"running","at":"2026-09-29T21:05:02.5-05:00","result_id":"6b0e1c2a-4d3f-4e8a-9b1c-2a3d4e5f6a7b","error":null}`)
	if code != 200 || obj["status"] != "running" || obj["ignored"] != false {
		t.Fatalf("running: %d %v", code, obj)
	}
	_, obj, _ = h.do("POST", path, `{"status":"running","at":"2026-09-29T21:05:02Z"}`)
	if obj["ignored"] != true || obj["status"] != "running" {
		t.Fatalf("running repetido: %v", obj)
	}
	_, obj, _ = h.do("POST", path, `{"status":"delivered"}`)
	if obj["ignored"] != true {
		t.Fatalf("retroceder: %v", obj)
	}
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	o := orderList(t, hist)[0]
	if o["started_at"] != "2026-09-30T02:05:02Z" || o["result_id"] != "6b0e1c2a-4d3f-4e8a-9b1c-2a3d4e5f6a7b" || o["delivered_at"] == nil {
		t.Fatalf("running guardado: %v", o)
	}
	_, obj, _ = h.do("POST", path, `{"status":"interrupted","error":"app-reiniciada"}`)
	if obj["ignored"] != false || obj["status"] != "interrupted" {
		t.Fatalf("interrupted: %v", obj)
	}
	_, obj, _ = h.do("POST", path, `{"status":"expired"}`)
	if obj["ignored"] != true || obj["status"] != "interrupted" {
		t.Fatalf("sobre final: %v", obj)
	}
	if code, obj, _ := h.do("POST", path, `{"status":"done"}`); code != 400 || obj["error"] != "done/failed se cierran con /results" {
		t.Fatalf("done por state: %d %v", code, obj)
	}
	if code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/orders/nope0000-0000-4000-8000-000000000000/state", `{"status":"running"}`); code != 404 ||
		obj["error"] != "orden desconocida" {
		t.Fatalf("desconocida: %d %v", code, obj)
	}
}

// 13: cancel no toca running.
func TestPhoneOrderCancel(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	createOrder(t, h, `{"order_id":"can00000-0000-4000-8000-000000000001","target":"A"}`)
	createOrder(t, h, `{"order_id":"can00000-0000-4000-8000-000000000002","target":"B"}`)
	createOrder(t, h, `{"order_id":"can00000-0000-4000-8000-000000000003","target":"A"}`)
	h.do("POST", "/api/v1/probes/hap-oficina/orders/ack", `{"order_ids":["can00000-0000-4000-8000-000000000002"]}`)
	h.do("POST", "/api/v1/probes/hap-oficina/orders/can00000-0000-4000-8000-000000000003/state", `{"status":"running","at":"2026-09-29T21:05:02Z"}`)
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/orders/cancel", `{"all_open":true}`)
	if code != 200 || len(obj["cancelled"].([]any)) != 2 || len(obj["unchanged"].([]any)) != 1 ||
		obj["unchanged"].([]any)[0] != "can00000-0000-4000-8000-000000000003" {
		t.Fatalf("cancel: %d %v", code, obj)
	}
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	for _, o := range orderList(t, hist) {
		if o["order_id"] == "can00000-0000-4000-8000-000000000003" {
			if o["status"] != "running" {
				t.Fatalf("running cancelada: %v", o)
			}
			continue
		}
		if o["status"] != "cancelled" || o["closed_by"] != "dashboard" || o["error"] != "cancelada" {
			t.Fatalf("cancelada mal: %v", o)
		}
	}
	_, obj, _ = h.do("POST", "/api/v1/probes/hap-oficina/orders/cancel", `{"order_ids":["zzzzzzzz-0000-4000-8000-000000000000"]}`)
	if len(obj["unknown"].([]any)) != 1 {
		t.Fatalf("cancel desconocida: %v", obj)
	}
}

// 5, 8 y 18: subida idempotente, sin ubicación de caché, device_egress intacto.
func TestPhoneResultsIdempotent(t *testing.T) {
	h, st := setupPhoneProbe(t)
	ctx := context.Background()
	if err := st.SetDeviceLocation(ctx, "router-A", 4.6, -74.0, nil, "android-fused"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeviceEgress(ctx, "router-A", "200.1.2.3", "v4", "CO"); err != nil {
		t.Fatal(err)
	}
	before, _ := st.DeviceEgressOf(ctx, "router-A")
	createOrder(t, h, `{"order_id":"res00000-0000-4000-8000-000000000001","target":"A"}`)
	r1 := result("6b0e1c2a-4d3f-4e8a-9b1c-2a3d4e5f6a7b", "res00000-0000-4000-8000-000000000001", "to-A", "")
	code, obj := postResults(h, r1)
	if code != 200 {
		t.Fatalf("results: %d %v", code, obj)
	}
	it := resultItems(t, obj)[0]
	if it["status"] != "inserted" || it["order_linked"] != true || it["order_status"] != "done" || it["device_id"] != "router-A" ||
		it["slot"] != "A" || it["net_route"] != "router" || it["confidence"] != "medium" || it["reason"] != "probe-table-confirmed" {
		t.Fatalf("insertado: %v", it)
	}
	mid := it["measurement_id"]
	// Mismo lote otra vez: duplicado, mismo measurement_id.
	_, obj = postResults(h, r1)
	it2 := resultItems(t, obj)[0]
	if it2["status"] != "duplicate" || it2["measurement_id"] != mid {
		t.Fatalf("duplicado: %v", it2)
	}
	_, _, rows := h.do("GET", "/api/v1/measurements?probe_id=hap-oficina", "")
	if len(rows) != 1 {
		t.Fatalf("una sola medición: %v", rows)
	}
	m := rows[0].(map[string]any)
	if m["lat"] != nil || m["lon"] != nil {
		t.Fatalf("no debe tomar la ubicación de device_locations: %v", m)
	}
	if m["ts"] != "2026-09-29T21:05:20Z" || m["test_started_at"] != "2026-09-29T21:05:20Z" || m["test_finished_at"] != "2026-09-29T21:05:47Z" ||
		m["device_id"] != "router-A" || m["slot"] != "A" || m["net_route"] != "router" {
		t.Fatalf("raw normalizado: %v", m)
	}
	if net := m["net"].(map[string]any); net["method"] != "phone-probe" || net["asn_status"] != "no-ip" {
		t.Fatalf("net: %v", net)
	}
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	o := orderList(t, hist)[0]
	if o["status"] != "done" || o["closed_by"] != "phone" || o["measurement_id"] != mid || o["result_id"] != "6b0e1c2a-4d3f-4e8a-9b1c-2a3d4e5f6a7b" {
		t.Fatalf("orden cerrada: %v", o)
	}
	after, _ := st.DeviceEgressOf(ctx, "router-A")
	if after.IP != before.IP || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("device_egress cambió: %v -> %v", before, after)
	}
	if _, ok := st.DeviceEgressOf(ctx, "phone-3fa9c2d1"); ok {
		t.Fatalf("no debe crear device_egress")
	}
	// Errores por ítem y lote demasiado grande.
	_, obj = postResults(h, `{"result_id":"zz"}`, `{"result_id":"abcdefgh-0000","probe_id":"otra","test_status":"done","net_path":"wifi","test_started_at":"2026-09-29T21:05:20Z","measured_by":"p"}`)
	items := resultItems(t, obj)
	if items[0]["status"] != "error" || items[0]["error"] != "result_id inválido" || items[0]["retryable"] != false ||
		items[1]["error"] != "probe_id no coincide con la sonda de la ruta" {
		t.Fatalf("errores: %v", items)
	}
	many := make([]string, 51)
	for i := range many {
		many[i] = `{}`
	}
	if code, _ := postResults(h, many...); code != 400 {
		t.Fatalf("más de 50: %d", code)
	}
}

// 6: finales blandos y duros.
func TestPhoneResultSoftAndHardFinal(t *testing.T) {
	h, st := setupPhoneProbe(t)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-2 * time.Hour).Format(time.RFC3339)
	pastNA := now.Add(-90 * time.Minute).Format(time.RFC3339)
	inWindow := `"test_started_at":"` + now.Add(-100*time.Minute).Format(time.RFC3339) + `"`
	afterWindow := `"test_started_at":"` + now.Add(-60*time.Minute).Format(time.RFC3339) + `"`
	createOrder(t, h, `{"order_id":"soft0000-0000-4000-8000-000000000001","target":"A","execute_at":"`+past+`","not_after":"`+pastNA+`"}`)
	createOrder(t, h, `{"order_id":"hard0000-0000-4000-8000-000000000001","target":"A","execute_at":"`+past+`","not_after":"`+pastNA+`"}`)
	createOrder(t, h, `{"order_id":"hard0000-0000-4000-8000-000000000002","target":"A"}`)
	if _, _, err := st.SweepPhoneOrders(ctx, "hap-oficina", time.Now()); err != nil {
		t.Fatal(err)
	}
	// hard-1: la cerró el celular (expired por el celular).
	h.do("POST", "/api/v1/probes/hap-oficina/orders/cancel", `{"order_ids":["hard0000-0000-4000-8000-000000000002"]}`)

	_, obj := postResults(h,
		result("r0000000-0000-4000-8000-000000000001", "soft0000-0000-4000-8000-000000000001", "to-A", inWindow),
		result("r0000000-0000-4000-8000-000000000002", "hard0000-0000-4000-8000-000000000002", "to-A", ""),
		result("r0000000-0000-4000-8000-000000000003", "nope0000-0000-4000-8000-000000000001", "to-A", ""),
	)
	items := resultItems(t, obj)
	if items[0]["order_linked"] != true || items[0]["order_status"] != "done" {
		t.Fatalf("final blando debe cerrarse: %v", items[0])
	}
	if items[1]["status"] != "inserted" || items[1]["order_linked"] != false || items[1]["order_status"] != "cancelled" {
		t.Fatalf("cancelada no se toca: %v", items[1])
	}
	if items[2]["status"] != "inserted" || items[2]["order_linked"] != false || items[2]["order_status"] != nil {
		t.Fatalf("orden desconocida: %v", items[2])
	}
	_, _, rows := h.do("GET", "/api/v1/measurements?probe_id=hap-oficina", "")
	for _, r := range rows {
		m := r.(map[string]any)
		if m["result_id"] == "r0000000-0000-4000-8000-000000000003" && (m["order_id"] != nil || m["order_id_client"] != "nope0000-0000-4000-8000-000000000001") {
			t.Fatalf("orden desconocida en raw: %v", m)
		}
	}

	// expired puesta por el celular: final duro.
	createOrder(t, h, `{"order_id":"hard0000-0000-4000-8000-000000000003","target":"A"}`)
	path := "/api/v1/probes/hap-oficina/orders/hard0000-0000-4000-8000-000000000003/state"
	if _, obj, _ := h.do("POST", path, `{"status":"expired","error":"sin-ethernet"}`); obj["ignored"] != false {
		t.Fatalf("expired por el celular: %v", obj)
	}
	_, obj = postResults(h, result("r0000000-0000-4000-8000-000000000004", "hard0000-0000-4000-8000-000000000003", "to-A", ""))
	if it := resultItems(t, obj)[0]; it["order_linked"] != false || it["order_status"] != "expired" {
		t.Fatalf("expired del celular no se toca: %v", it)
	}
	// Blando pero la prueba empezó después de not_after: no se liga.
	_, obj = postResults(h, result("r0000000-0000-4000-8000-000000000005", "hard0000-0000-4000-8000-000000000001", "to-A", afterWindow))
	if it := resultItems(t, obj)[0]; it["order_linked"] != false || it["order_status"] != "expired" {
		t.Fatalf("blando fuera de ventana: %v", it)
	}
}

// 7 y 17: clasificación.
func TestPhoneResultClassification(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	cacheASN(h.s, "181.49.10.20", 27831, "Colombia Movil")
	cacheASN(h.s, "186.102.123.189", 3816, "Colombia Telecomunicaciones")
	cacheASN(h.s, "190.60.1.1", 14080, "Telmex")
	cacheASN(h.s, "186.102.72.153", 3816, "Colombia Telecomunicaciones")
	cases := []struct {
		name, body               string
		route, conf, reason, dev string
		slot                     any
	}{
		{"wifi", result("c0000000-0000-4000-8000-000000000001", "", "to-A",
			`"net_path":"wifi","routing_table":null,"mikrotik_confirmed":false,"mikrotik_rule":null,"mikrotik_rule_end":null,"egress_ip":"181.49.10.20"`),
			"other-network", "high", "phone-wifi", "phone-3fa9c2d1", nil},
		{"sin confirmar", result("c0000000-0000-4000-8000-000000000002", "", "to-A", `"mikrotik_confirmed":false`),
			"unknown", "low", "probe-route-unconfirmed", "phone-3fa9c2d1", nil},
		{"regla cambió", result("c0000000-0000-4000-8000-000000000003", "", "to-A", `"mikrotik_rule_end":{"table":"main"}`),
			"unknown", "low", "probe-route-changed", "phone-3fa9c2d1", nil},
		{"sin ASN", result("c0000000-0000-4000-8000-000000000004", "", "to-A", ""),
			"router", "medium", "probe-table-confirmed", "router-A", "A"},
		{"ASN esperado", result("c0000000-0000-4000-8000-000000000005", "", "to-B", `"egress_ip":"186.102.123.189"`),
			"router", "high", "probe-asn-match", "router-B", "B"},
		{"ASN del otro", result("c0000000-0000-4000-8000-000000000006", "", "to-A", `"egress_ip":"186.102.123.189"`),
			"other-network", "high", "probe-asn-other-router", "router-A", "A"},
		{"ASN distinto", result("c0000000-0000-4000-8000-000000000007", "", "to-B", `"egress_ip":"190.60.1.1"`),
			"unknown", "low", "probe-asn-mismatch", "router-B", "B"},
		{"IP cambió de red", result("c0000000-0000-4000-8000-000000000008", "", "to-B", `"egress_ip":"186.102.123.189","egress_ip_end":"190.60.1.1"`),
			"unknown", "low", "network-changed-mid-test", "router-B", "B"},
		{"CGNAT mismo ASN", result("c0000000-0000-4000-8000-000000000019", "", "to-B", `"egress_ip":"186.102.123.189","egress_ip_end":"186.102.72.153"`),
			"router", "high", "probe-asn-match", "router-B", "B"},
	}
	for _, c := range cases {
		_, obj := postResults(h, c.body)
		it := resultItems(t, obj)[0]
		if it["net_route"] != c.route || it["confidence"] != c.conf || it["reason"] != c.reason || it["device_id"] != c.dev || it["slot"] != c.slot {
			t.Errorf("%s: %v", c.name, it)
		}
	}
	_, _, rows := h.do("GET", "/api/v1/measurements?probe_id=hap-oficina", "")
	for _, r := range rows {
		m := r.(map[string]any)
		switch m["result_id"] {
		case "c0000000-0000-4000-8000-000000000001":
			if m["egress_ip"] != "181.49.10.0/24" {
				t.Errorf("wifi: egress_ip debe ir recortada: %v", m["egress_ip"])
			}
		case "c0000000-0000-4000-8000-000000000003":
			if m["device_id_client"] != "router-A" || m["slot"] != nil {
				t.Errorf("regla cambió: %v", m)
			}
		case "c0000000-0000-4000-8000-000000000005":
			if m["egress_ip"] != "186.102.123.189" || m["net"].(map[string]any)["egress_asn"] != float64(3816) {
				t.Errorf("ethernet: %v", m)
			}
		}
	}

	// Mismo ASN esperado en A y B: ambiguo.
	amb := strings.Replace(phoneProbePUT, `"label":"Notion 5G"`, `"label":"Notion 5G","expected_asn":3816`, 1)
	if code, obj, _ := h.do("PUT", "/api/v1/probes/hap-oficina", amb); code != 200 {
		t.Fatalf("PUT: %d %v", code, obj)
	}
	_, obj := postResults(h, result("c0000000-0000-4000-8000-000000000009", "", "to-A", `"egress_ip":"186.102.123.189"`))
	if it := resultItems(t, obj)[0]; it["net_route"] != "router" || it["confidence"] != "medium" || it["reason"] != "probe-asn-ambiguous" {
		t.Fatalf("ambiguo: %v", it)
	}
}

func TestClassifyPhoneResultTable(t *testing.T) {
	base := phoneClassInput{netPath: "ethernet", confirmed: true, knownTable: true, otherExpected: map[int]bool{}}
	in := base
	in.asn, in.expected = 100, 100
	if v := classifyPhoneResult(in); v.reason != "probe-asn-match" {
		t.Fatal(v)
	}
	in.otherExpected = map[int]bool{100: true}
	if v := classifyPhoneResult(in); v.reason != "probe-asn-ambiguous" {
		t.Fatal(v)
	}
	in = base
	in.asn = 200 // sin esperado propio, tampoco de otro: sin decidir por ASN
	if v := classifyPhoneResult(in); v.reason != "probe-table-confirmed" {
		t.Fatal(v)
	}
	in = base
	in.knownTable = false
	if v := classifyPhoneResult(in); v.route != "unknown" {
		t.Fatal(v)
	}
}

// 9, 10, 14 y 19: comandos existentes frente a órdenes del celular.
func TestPhoneOrdersAndLegacyCommands(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	// 14: "Prueba vía sonda" sobre un equipo de la sonda de celular.
	code, obj, _ := h.do("POST", "/api/v1/commands", `{"device_id":"router-B","type":"run_speedtest","runner":"probe","duration_s":10,"lat":4.6,"lon":-74.0,"requested_by":"dash"}`)
	if code != 200 || obj["order_id"] == nil || obj["status"] != "pending" {
		t.Fatalf("POST /commands: %d %v", code, obj)
	}
	orderID := obj["order_id"].(string)
	cmdID := int(obj["id"].(float64))
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	o := orderList(t, hist)[0]
	if o["order_id"] != orderID || o["slot"] != "B" || o["batch_id"] != orderID || o["selection_reason"] != "requested" || o["requested_by"] != "dash" {
		t.Fatalf("orden desde /commands: %v", o)
	}
	// Un POST /commands no puede fijar order_id ni estados.
	_, obj, _ = h.do("POST", "/api/v1/commands", `{"device_id":"router-A","type":"run_speedtest","order_id":"forged00-0000-4000-8000-000000000000","status":"done","closed_by":"phone"}`)
	_, _, cmds := h.do("GET", "/api/v1/commands?device_id=router-A", "")
	c0 := cmds[0].(map[string]any)
	if c0["order_id"] != nil || c0["status"] != "pending" || c0["closed_by"] != nil {
		t.Fatalf("campos de solo salida fijados por el cliente: %v", c0)
	}
	// GET /commands muestra los campos de la orden.
	_, _, cmds = h.do("GET", "/api/v1/commands?device_id=router-B", "")
	if c := cmds[0].(map[string]any); c["order_id"] != orderID || c["runner"] != "phone" || c["execute_at"] == nil {
		t.Fatalf("GET /commands: %v", c)
	}

	// 10: ni el agente ni el script toman órdenes de celular.
	_, got, _ := h.do("GET", "/api/v1/commands/next?device_id=router-B", "")
	if len(got) != 0 {
		t.Fatalf("el agente tomó una orden de celular: %v", got)
	}
	_, got, _ = h.do("GET", "/api/v1/commands/next?probe_id=hap-oficina", "")
	if len(got) != 0 {
		t.Fatalf("el script tomó una orden de celular: %v", got)
	}

	// 9: complete sobre una orden de celular → 400; sobre un comando, idempotente.
	code, obj, _ = h.do("POST", "/api/v1/commands/"+itoa(cmdID)+"/complete", `{"status":"done","measurement":{"device_id":"router-B","down_mbps":1}}`)
	if code != 400 || !strings.Contains(obj["error"].(string), "/results") {
		t.Fatalf("complete de orden: %d %v", code, obj)
	}
	_, agent, _ := h.do("POST", "/api/v1/commands", `{"device_id":"router-A","type":"run_speedtest"}`)
	aid := itoa(int(agent["id"].(float64)))
	body := `{"status":"done","measurement":{"device_id":"router-A","down_mbps":42}}`
	if code, obj, _ := h.do("POST", "/api/v1/commands/"+aid+"/complete", body); code != 200 || obj["ignored"] != nil {
		t.Fatalf("complete 1: %d %v", code, obj)
	}
	if code, obj, _ := h.do("POST", "/api/v1/commands/"+aid+"/complete", body); code != 200 || obj["ignored"] != true {
		t.Fatalf("complete 2: %d %v", code, obj)
	}
	_, _, rows := h.do("GET", "/api/v1/measurements?device_id=router-A", "")
	if len(rows) != 1 {
		t.Fatalf("complete dos veces debe dejar una sola medición: %d", len(rows))
	}

	// 19: end_location sobre una orden de celular: ignorado, la medición sin lat_end.
	_, obj = postResults(h, result("e0000000-0000-4000-8000-000000000001", orderID, "to-B", ""))
	if it := resultItems(t, obj)[0]; it["order_linked"] != true {
		t.Fatalf("resultado de la orden: %v", it)
	}
	if code, obj, _ := h.do("POST", "/api/v1/commands/"+itoa(cmdID)+"/end_location", `{"lat":1,"lon":2}`); code != 200 || obj["ignored"] != true {
		t.Fatalf("end_location: %d %v", code, obj)
	}
	_, _, rows = h.do("GET", "/api/v1/measurements?probe_id=hap-oficina", "")
	if m := rows[0].(map[string]any); m["lat_end"] != nil {
		t.Fatalf("lat_end escrito: %v", m)
	}
}

// Ciclo de una sonda de celular: órdenes de alternancia, sin duplicar abiertas.
func TestPhoneProbeCycle(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/cycle", `{}`)
	if code != 200 || len(obj["order_ids"].([]any)) != 2 || len(obj["command_ids"].([]any)) != 2 {
		t.Fatalf("cycle: %d %v", code, obj)
	}
	_, obj, _ = h.do("POST", "/api/v1/probes/hap-oficina/cycle", `{}`)
	if len(obj["order_ids"].([]any)) != 0 {
		t.Fatalf("no debe duplicar: %v", obj)
	}
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	for _, o := range orderList(t, hist) {
		ea, _ := time.Parse(time.RFC3339, o["execute_at"].(string))
		na, _ := time.Parse(time.RFC3339, o["not_after"].(string))
		if o["selection_reason"] != "alternation" || na.Sub(ea) != 1800*time.Second {
			t.Fatalf("orden de ciclo: %v", o)
		}
	}
}

// 12 y 20: PUT conserva runner/slot/expected_asn y valida tablas.
func TestPhoneProbePutPreserves(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	// Lo que manda hoy el selector de intervalo: sin runner, slot ni expected_asn.
	code, obj, _ := h.do("PUT", "/api/v1/probes/hap-oficina", `{"label":"hAP ac2 oficina","interval_s":900,"duration_s":10,
		"targets":[{"device_id":"router-A","routing_table":"to-A"},{"device_id":"router-B","routing_table":"to-B"}]}`)
	if code != 200 {
		t.Fatalf("PUT: %d %v", code, obj)
	}
	ts := obj["targets"].([]any)
	if obj["runner"] != "phone" || ts[1].(map[string]any)["expected_asn"] != float64(3816) || ts[1].(map[string]any)["slot"] != "B" ||
		ts[0].(map[string]any)["expected_asn"] != nil || ts[0].(map[string]any)["slot"] != "A" {
		t.Fatalf("no conservó: %v", obj)
	}
	// null explícito borra el ASN.
	_, obj, _ = h.do("PUT", "/api/v1/probes/hap-oficina", `{"interval_s":0,"targets":[{"device_id":"router-A","routing_table":"to-A"},{"device_id":"router-B","routing_table":"to-B","expected_asn":null}]}`)
	if obj["targets"].([]any)[1].(map[string]any)["expected_asn"] != nil {
		t.Fatalf("null no borró: %v", obj)
	}
	for _, bad := range []string{
		`{"targets":[{"device_id":"a","routing_table":"to-A"},{"device_id":"b","routing_table":"to-A"}]}`,
		`{"targets":[{"device_id":"a","routing_table":"main"}]}`,
		`{"targets":[{"slot":"a","device_id":"a","routing_table":"x"}]}`,
		`{"targets":[{"slot":"A","device_id":"a","routing_table":"x"},{"slot":"A","device_id":"b","routing_table":"y"}]}`,
		`{"targets":[{"device_id":"a","routing_table":"x","expected_asn":0}]}`,
		`{"runner":"robot","targets":[]}`,
	} {
		if code, obj, _ := h.do("PUT", "/api/v1/probes/hap-oficina", bad); code != 400 {
			t.Fatalf("debía rechazar %s: %d %v", bad, code, obj)
		}
	}
	// Una sonda de script sí puede repetir tablas (compatibilidad).
	if code, obj, _ := h.do("PUT", "/api/v1/probes/viejo", `{"targets":[{"device_id":"a","routing_table":"main"},{"device_id":"b","routing_table":"main"}]}`); code != 200 ||
		obj["runner"] != "probe" {
		t.Fatalf("sonda de script: %d %v", code, obj)
	}
	// De phone a probe con órdenes abiertas: se cancelan.
	createOrder(t, h, `{"order_id":"run00000-0000-4000-8000-000000000001","target":"A"}`)
	h.do("PUT", "/api/v1/probes/hap-oficina", `{"runner":"probe","targets":[{"device_id":"router-A","routing_table":"to-A"}]}`)
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	if o := orderList(t, hist)[0]; o["status"] != "cancelled" || o["error"] != "cambio-de-runner" {
		t.Fatalf("cambio de runner: %v", o)
	}
}

// Estado en vivo: upsert, orden por seq, phone_status en la sonda, secretos.
func TestPhoneProbeStatus(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	_, obj, _ := h.do("GET", "/api/v1/probes/hap-oficina/status", "")
	if obj["status"] != nil || obj["online"] != false {
		t.Fatalf("sin estado: %v", obj)
	}
	st := func(seq int, phase string) string {
		return fmt.Sprintf(`{"measured_by":"phone-3fa9c2d1","probe_id":"hap-oficina","seq":%d,"service_started_at":"2026-09-29T18:00:02Z",
			"phase":"%s","mikrotik":{"current_table":"to-A","api_key":"x"},"api_key":"secreto","alerts":[]}`, seq, phase)
	}
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/status", st(10, "download"))
	if code != 200 || obj["status"] != "ok" || obj["server_time"] == nil {
		t.Fatalf("POST status: %d %v", code, obj)
	}
	h.do("POST", "/api/v1/probes/hap-oficina/status", st(9, "idle")) // llegó tarde: se descarta
	_, obj, _ = h.do("GET", "/api/v1/probes/hap-oficina/status", "")
	status := obj["status"].(map[string]any)
	if obj["online"] != true || status["phase"] != "download" || status["api_key"] != nil || status["mikrotik"].(map[string]any)["api_key"] != nil {
		t.Fatalf("GET status: %v", obj)
	}
	// Otro servicio (reinicio): seq vuelve a empezar y se acepta.
	h.do("POST", "/api/v1/probes/hap-oficina/status", strings.Replace(st(1, "idle"), "18:00:02", "19:00:00", 1))
	_, p, _ := h.do("GET", "/api/v1/probes/hap-oficina", "")
	ps := p["phone_status"].(map[string]any)
	if ps["status"].(map[string]any)["phase"] != "idle" || ps["online"] != true || p["online"] != true {
		t.Fatalf("phone_status: %v", p)
	}
	if code, _, _ := h.do("POST", "/api/v1/probes/nada/status", st(1, "idle")); code != 404 {
		t.Fatalf("sonda inexistente: %d", code)
	}
}

// Revisión: una orden "next" se cierra con el equipo medido (Ethernet
// confirmado) y conserva slot/tabla NULL si el resultado no se atribuyó (WiFi).
func TestPhoneNextOrderClosesWithMeasuredSlot(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	createOrder(t, h, `{"order_id":"next0000-0000-4000-8000-000000000001","target":"next"}`)
	createOrder(t, h, `{"order_id":"next0000-0000-4000-8000-000000000002","target":"next"}`)
	_, obj := postResults(h,
		result("rn000000-0000-4000-8000-000000000001", "next0000-0000-4000-8000-000000000001", "to-B", ""),
		result("rn000000-0000-4000-8000-000000000002", "next0000-0000-4000-8000-000000000002", "to-B", `"net_path":"wifi","mikrotik_confirmed":false`))
	items := resultItems(t, obj)
	if items[0]["order_linked"] != true || items[0]["slot"] != "B" || items[1]["order_linked"] != true || items[1]["slot"] != nil {
		t.Fatalf("resultados: %v", items)
	}
	_, obj, _ = h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	for _, o := range orderList(t, obj) {
		switch o["order_id"] {
		case "next0000-0000-4000-8000-000000000001":
			if o["status"] != "done" || o["slot"] != "B" || o["device_id"] != "router-B" || o["routing_table"] != "to-B" || o["closed_by"] != "phone" {
				t.Errorf("next atribuida: %v", o)
			}
		case "next0000-0000-4000-8000-000000000002":
			if o["status"] != "done" || o["slot"] != nil || o["device_id"] != "hap-oficina" || o["routing_table"] != nil {
				t.Errorf("next por WiFi: %v", o)
			}
		}
	}
}

// Revisión: creación de órdenes y subida de resultados concurrentes con la
// misma clave -> un solo lote, una sola medición, la orden cerrada una vez.
func TestPhoneConcurrentIdempotency(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	const n = 8
	var wg sync.WaitGroup
	existing := make([]any, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, obj, _ := h.do("POST", "/api/v1/probes/hap-oficina/orders", `{"order_id":"conc0000-0000-4000-8000-000000000001","target":"sequence","count":4,"spacing_s":60}`)
			existing[i] = obj["existing"]
		}(i)
	}
	wg.Wait()
	created := 0
	for _, e := range existing {
		if e == false {
			created++
		} else if e != true {
			t.Fatalf("respuesta sin existing: %v", existing)
		}
	}
	if created != 1 {
		t.Fatalf("lotes creados: %d (%v)", created, existing)
	}
	statuses := make([]any, n)
	mids := make([]any, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, obj := postResults(h, result("rc000000-0000-4000-8000-000000000001", "conc0000-0000-4000-8000-000000000001-1", "to-A", ""))
			it := resultItems(t, obj)[0]
			statuses[i], mids[i] = it["status"], it["measurement_id"]
		}(i)
	}
	wg.Wait()
	inserted := 0
	for i := range statuses {
		if statuses[i] == "inserted" {
			inserted++
		}
		if mids[i] != mids[0] {
			t.Fatalf("measurement_id distinto: %v", mids)
		}
	}
	if inserted != 1 {
		t.Fatalf("insertados: %d (%v)", inserted, statuses)
	}
	_, _, rows := h.do("GET", "/api/v1/measurements?probe_id=hap-oficina", "")
	if len(rows) != 1 {
		t.Fatalf("mediciones: %d", len(rows))
	}
}

// Revisión: los secretos nunca quedan guardados en un resultado (§0), aunque
// vengan anidados.
func TestPhoneResultStripsSecrets(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	_, obj := postResults(h, result("rs000000-0000-4000-8000-000000000001", "", "to-A",
		`"api_key":"k1","prod_api_key":"k2","mikrotik_password":"k3","mikrotik":{"password":"k4","user":"phone-probe"}`))
	if it := resultItems(t, obj)[0]; it["status"] != "inserted" {
		t.Fatalf("resultado: %v", it)
	}
	_, _, rows := h.do("GET", "/api/v1/measurements?probe_id=hap-oficina", "")
	m := rows[0].(map[string]any)
	for _, k := range []string{"api_key", "prod_api_key", "mikrotik_password"} {
		if m[k] != nil {
			t.Errorf("%s guardado: %v", k, m[k])
		}
	}
	if mk, _ := m["mikrotik"].(map[string]any); mk == nil || mk["password"] != nil || mk["user"] != "phone-probe" {
		t.Errorf("mikrotik: %v", m["mikrotik"])
	}
	if m["down_mbps"] != 85.3 || m["device_id"] != "router-A" {
		t.Errorf("resto del resultado: %v", m)
	}
}

func TestEgressChangedCGNAT(t *testing.T) {
	start, end, other := netip.MustParseAddr("186.102.14.153"), netip.MustParseAddr("186.102.72.153"), netip.MustParseAddr("179.19.72.14")
	asns := map[netip.Addr]asnInfo{start: {ASN: 3816}, end: {ASN: 3816}, other: {ASN: 271773}}
	it := phoneResultItem{egressIP: start.String(), egressIPEnd: end.String(), egressAddr: start, egressEndAddr: end}
	if egressChanged(it, asns) {
		t.Fatal("dos IP de Movistar (mismo ASN) no son un cambio de red")
	}
	it.egressIPEnd, it.egressEndAddr = other.String(), other
	if !egressChanged(it, asns) {
		t.Fatal("pasar de AS3816 a AS271773 es un cambio de red")
	}
	delete(asns, other)
	if !egressChanged(it, asns) {
		t.Fatal("sin ASN del final, IP distinta cuenta como cambio")
	}
	it.egressIPEnd, it.egressEndAddr = start.String(), start
	if egressChanged(it, asns) {
		t.Fatal("misma IP no es cambio")
	}
}
