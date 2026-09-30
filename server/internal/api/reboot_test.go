package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"notion5g/server/internal/store"
)

// Tests HTTP del reinicio remoto (contrato docs/CONTRATO-SONDA-AB.md §6.3).

func TestRebootEndpointFlow(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	const oid = "reb00000-0000-4000-8000-0000000000a1"
	body := `{"order_id":"` + oid + `","requested_by":"dashboard","reason":"recommended","force":false}`

	// App vieja (estado sin "routers"): 409 app_outdated, sin datos de
	// enfriamiento (el dashboard no ofrece forzar).
	h.do("POST", "/api/v1/probes/hap-oficina/status", `{"measured_by":"phone-3fa9c2d1","seq":1,"service_started_at":"2026-09-29T18:00:02Z","phase":"idle"}`)
	if code, obj, _ := h.do("POST", "/api/v1/devices/router-A/reboot", body); code != 409 || obj["app_outdated"] != true ||
		obj["in_progress"] != false || obj["cooldown_until"] != nil {
		t.Fatalf("app vieja: %d %v", code, obj)
	}
	// La app nueva manda "routers" (y las claves SSH nunca quedan guardadas).
	h.do("POST", "/api/v1/probes/hap-oficina/status", `{"measured_by":"phone-3fa9c2d1","seq":2,"service_started_at":"2026-09-29T18:00:02Z","routers":{},
		"settings":{"ssh_password_A":"clave-de-prueba","SSH_Password_B":"clave-de-prueba","ssh_user_A":"root"}}`)
	if _, st, _ := h.do("GET", "/api/v1/probes/hap-oficina/status", ""); strings.Contains(fmt.Sprint(st), "clave-de-prueba") ||
		!strings.Contains(fmt.Sprint(st), "root") {
		t.Fatalf("claves SSH en el estado: %v", st)
	}

	code, obj, _ := h.do("POST", "/api/v1/devices/router-A/reboot", body)
	if code != 200 || obj["existing"] != false {
		t.Fatalf("reboot: %d %v", code, obj)
	}
	o := obj["order"].(map[string]any)
	if o["type"] != "reboot_router" || o["slot"] != "A" || o["device_id"] != "router-A" || o["routing_table"] != "to-A" ||
		o["probe_id"] != "hap-oficina" || o["status"] != "pending" || o["reboot_reason"] != "recommended" || o["duration_s"] != nil {
		t.Fatalf("orden: %v", o)
	}
	for _, k := range []string{"step", "result", "selection_reason", "measurement_id"} {
		if v, ok := o[k]; !ok || v != nil {
			t.Fatalf("%s debe ir null y presente: %v", k, o)
		}
	}
	ea, _ := time.Parse(time.RFC3339, o["execute_at"].(string))
	na, _ := time.Parse(time.RFC3339, o["not_after"].(string))
	if na.Sub(ea) != 15*time.Minute {
		t.Fatalf("not_after: %v %v", ea, na)
	}
	// Idempotente por order_id.
	if code, obj, _ := h.do("POST", "/api/v1/devices/router-A/reboot", body); code != 200 || obj["existing"] != true ||
		obj["order"].(map[string]any)["id"] != o["id"] {
		t.Fatalf("reintento: %d %v", code, obj)
	}
	// Otro pedido mientras está en curso: 409 aunque traiga force.
	if code, obj, _ := h.do("POST", "/api/v1/devices/router-A/reboot", `{"force":true}`); code != 409 || obj["in_progress"] != true ||
		obj["order"].(map[string]any)["order_id"] != oid {
		t.Fatalf("en curso: %d %v", code, obj)
	}
	if code, _, _ := h.do("POST", "/api/v1/devices/router-X/reboot", `{}`); code != 404 {
		t.Fatalf("equipo sin sonda: %d", code)
	}
	if code, _, _ := h.do("POST", "/api/v1/devices/router-A/reboot", `{"reason":"x"}`); code != 400 {
		t.Fatalf("reason inválido: %d", code)
	}

	// El celular la ve en su cola (con type) y la confirma.
	_, obj, _ = h.do("GET", "/api/v1/probes/hap-oficina/orders", "")
	orders := orderList(t, obj)
	if len(orders) != 1 || orders[0]["type"] != "reboot_router" || orders[0]["order_id"] != oid {
		t.Fatalf("cola del celular: %v", orders)
	}
	h.do("POST", "/api/v1/probes/hap-oficina/orders/ack", `{"order_ids":["`+oid+`"]}`)

	// Ni el agente ni el script la toman.
	if _, obj, _ := h.do("GET", "/api/v1/commands/next?device_id=router-A", ""); len(obj) != 0 {
		t.Fatalf("el agente tomó el reinicio: %v", obj)
	}
	if _, obj, _ := h.do("GET", "/api/v1/commands/next?probe_id=hap-oficina", ""); len(obj) != 0 {
		t.Fatalf("el script tomó el reinicio: %v", obj)
	}

	// Progreso y cierre por /state.
	path := "/api/v1/probes/hap-oficina/orders/" + oid + "/state"
	if code, obj, _ := h.do("POST", path, `{"status":"running","at":"2026-09-29T21:05:02Z","step":"ssh"}`); code != 200 || obj["ignored"] != false {
		t.Fatalf("running: %d %v", code, obj)
	}
	if code, obj, _ := h.do("POST", path, `{"status":"running","step":"waiting_back","result":{"ssh_ok":true,"host_key_fp":"SHA256:abc","ssh_password":"clave-de-prueba","ssh_password_A":"clave-de-prueba"}}`); code != 200 || obj["ignored"] != false {
		t.Fatalf("progreso: %d %v", code, obj)
	}
	if code, _, _ := h.do("POST", path, `{"status":"running","step":"Paso Raro!"}`); code != 400 {
		t.Fatalf("step inválido: %d", code)
	}
	if code, _, _ := h.do("POST", path, `{"status":"done","result":[1,2]}`); code != 400 {
		t.Fatalf("result no objeto: %d", code)
	}
	code, obj, _ = h.do("POST", path, `{"status":"done","step":"done","result":{"ssh_ok":true,"host_key_fp":"SHA256:abc","gateway_back_s":95,"internet_back_s":140}}`)
	if code != 200 || obj["status"] != "done" || obj["ignored"] != false {
		t.Fatalf("done: %d %v", code, obj)
	}
	_, hist, _ := h.do("GET", "/api/v1/probes/hap-oficina/orders?view=history", "")
	o = orderList(t, hist)[0]
	res, _ := o["result"].(map[string]any)
	if o["status"] != "done" || o["closed_by"] != "phone" || o["step"] != "done" || res == nil || res["gateway_back_s"] != float64(95) ||
		res["ssh_password"] != nil || res["ssh_password_A"] != nil || o["measurement_id"] != nil {
		t.Fatalf("cerrada: %v", o)
	}
	// Un resultado de medición con ese order_id se rechaza (no genera medición).
	_, obj = postResults(h, result("res00000-0000-4000-8000-0000000000a1", oid, "to-A", ""))
	if it := resultItems(t, obj)[0]; it["status"] != "error" || it["retryable"] != false {
		t.Fatalf("resultado sobre reinicio: %v", it)
	}
	if _, _, arr := h.do("GET", "/api/v1/measurements?probe_id=hap-oficina", ""); len(arr) != 0 {
		t.Fatalf("mediciones: %v", arr)
	}
	// Queda en commands como las demás.
	_, _, cmds := h.do("GET", "/api/v1/commands?device_id=router-A", "")
	if len(cmds) != 1 {
		t.Fatalf("commands: %v", cmds)
	}
	if c := cmds[0].(map[string]any); c["type"] != "reboot_router" || c["status"] != "done" || c["order_id"] != oid ||
		c["reboot_reason"] != "recommended" || c["step"] != "done" || c["completed_at"] == nil ||
		c["result"].(map[string]any)["internet_back_s"] != float64(140) {
		t.Fatalf("command: %v", c)
	}
	// Un POST /commands no puede fijar result/step/error (solo de salida).
	code, obj, _ = h.do("POST", "/api/v1/commands", `{"device_id":"router-Z","type":"run_speedtest","result":{"x":1},"step":"done","error":"x","reboot_reason":"manual"}`)
	if code != 200 {
		t.Fatalf("POST /commands: %d %v", code, obj)
	}
	if _, _, cmds := h.do("GET", "/api/v1/commands?device_id=router-Z", ""); len(cmds) != 1 || cmds[0].(map[string]any)["result"] != nil ||
		cmds[0].(map[string]any)["step"] != nil || cmds[0].(map[string]any)["error"] != nil {
		t.Fatalf("salida-solo: %v", cmds)
	}
	// Enfriamiento: 409 sin force; con force, 200.
	if code, obj, _ := h.do("POST", "/api/v1/devices/router-A/reboot", `{}`); code != 409 || obj["in_progress"] != false || obj["cooldown_until"] == nil {
		t.Fatalf("enfriamiento: %d %v", code, obj)
	}
	if code, obj, _ := h.do("POST", "/api/v1/devices/router-A/reboot", `{"force":true}`); code != 200 || obj["existing"] != false {
		t.Fatalf("force: %d %v", code, obj)
	}
	// Un order_id de otra orden: 409.
	createOrder(t, h, `{"order_id":"spd00000-0000-4000-8000-0000000000a1","target":"B"}`)
	if code, obj, _ := h.do("POST", "/api/v1/devices/router-B/reboot", `{"order_id":"spd00000-0000-4000-8000-0000000000a1"}`); code != 409 ||
		obj["error"] != store.ErrOrderIDInUse.Error() {
		t.Fatalf("order_id en uso: %d %v", code, obj)
	}
}

func TestRebootRecommendationInDevicesAndProbe(t *testing.T) {
	h, _ := setupPhoneProbe(t)
	// router-A tiene agente al día (aparece en GET /devices); router-B no.
	if code, _, _ := h.do("POST", "/api/v1/heartbeat", `{"device_id":"router-A","source":"router"}`); code != 200 {
		t.Fatalf("heartbeat: %d", code)
	}
	// Salud según el celular: B sin Internet hace 25 min y sin puerta de
	// enlace (reloj del celular 2 h atrasado: solo cuentan sus diferencias).
	sent := time.Now().UTC().Add(-2 * time.Hour)
	fmtTS := func(t time.Time) string { return t.Format(time.RFC3339) }
	status := `{"measured_by":"phone-3fa9c2d1","probe_id":"hap-oficina","seq":1,"service_started_at":"2026-09-29T18:00:02Z","sent_at":"` + fmtTS(sent) + `",
		"routers":{"A":{"internet_ok":true,"gateway_ok":true,"loss_pct":0,"rtt_ms":38.2,"checked_at":"` + fmtTS(sent) + `","down_since":null},
		           "B":{"internet_ok":false,"gateway_ok":false,"loss_pct":100,"rtt_ms":null,"checked_at":"` + fmtTS(sent) + `","down_since":"` + fmtTS(sent.Add(-25*time.Minute)) + `"}}}`
	if code, _, _ := h.do("POST", "/api/v1/probes/hap-oficina/status", status); code != 200 {
		t.Fatalf("status: %d", code)
	}
	// El bloque routers se guarda tal cual.
	_, st, _ := h.do("GET", "/api/v1/probes/hap-oficina/status", "")
	if st["status"].(map[string]any)["routers"].(map[string]any)["B"].(map[string]any)["internet_ok"] != false {
		t.Fatalf("routers no guardado: %v", st)
	}

	_, _, devs := h.do("GET", "/api/v1/devices", "")
	var a map[string]any
	for _, d := range devs {
		if d.(map[string]any)["device_id"] == "router-A" {
			a = d.(map[string]any)
		}
	}
	if a == nil {
		t.Fatalf("router-A no está en /devices: %v", devs)
	}
	rb, ok := a["reboot"].(map[string]any)
	if !ok || rb["recommended"] != false || rb["reason"] != "En línea" || rb["slot"] != "A" || rb["probe_id"] != "hap-oficina" || rb["agent_last_seen"] == nil {
		t.Fatalf("reboot de A: %v", a["reboot"])
	}

	_, p, _ := h.do("GET", "/api/v1/probes/hap-oficina", "")
	var b map[string]any
	for _, tg := range p["targets"].([]any) {
		if tg.(map[string]any)["slot"] == "B" {
			b = tg.(map[string]any)["reboot"].(map[string]any)
		}
	}
	if b == nil || b["recommended"] != true || b["offline_min"] != float64(25) || !strings.Contains(b["reason"].(string), "parece apagado") ||
		b["health"].(map[string]any)["fresh"] != true || b["last_reboot_at"] != nil {
		t.Fatalf("reboot de B: %v", b)
	}
	// Pedido el reinicio recomendado, deja de recomendarse y muestra la orden.
	if code, _, _ := h.do("POST", "/api/v1/devices/router-B/reboot", `{"reason":"recommended"}`); code != 200 {
		t.Fatalf("reboot B: %d", code)
	}
	_, _, list := h.do("GET", "/api/v1/probes", "")
	if len(list) != 1 {
		t.Fatalf("probes: %v", list)
	}
	for _, tg := range list[0].(map[string]any)["targets"].([]any) {
		if tg.(map[string]any)["slot"] == "B" {
			b = tg.(map[string]any)["reboot"].(map[string]any)
		}
	}
	if b["recommended"] != false || b["in_progress"] != true || b["last_reboot_status"] != "pending" {
		t.Fatalf("B en curso: %v", b)
	}
	// Un "routers" con otra forma se descarta sin rechazar el estado.
	if code, _, _ := h.do("POST", "/api/v1/probes/hap-oficina/status", `{"measured_by":"phone-3fa9c2d1","seq":2,"service_started_at":"2026-09-29T18:00:02Z","routers":[1]}`); code != 200 {
		t.Fatalf("routers raro: %d", code)
	}
	_, st, _ = h.do("GET", "/api/v1/probes/hap-oficina/status", "")
	if _, ok := st["status"].(map[string]any)["routers"]; ok {
		t.Fatalf("routers raro guardado: %v", st)
	}
}

func TestApplyPhoneHealthNoAgent(t *testing.T) {
	yes := true
	rsrp, rtt := -95.0, 31.0
	band := 28
	checked := "2099-01-01T00:00:00Z"
	d := store.DeviceSummary{DeviceID: "router-B", LastSeen: "2026-09-30T04:27:53Z", Online: false}
	info := &store.RebootInfo{Health: &store.RouterHealth{InternetOK: &yes, Fresh: true, RTTMs: &rtt, CheckedAt: &checked,
		Signal: &store.RouterSignal{Operator: "Movistar", RAT: "LTE", BandLTE: &band, RSRPDbm: &rsrp}}}
	applyPhoneHealth(&d, info)
	if !d.Online || d.StatusSource != "phone-health" || d.Operator != "Movistar" || d.RSRPDbm == nil || *d.RSRPDbm != -95 || d.LastSeen != checked {
		t.Fatalf("sin agente con salud fresca: %+v", d)
	}
	agent := "2026-09-30T04:30:00Z"
	d2 := store.DeviceSummary{DeviceID: "router-A", Online: false}
	info.AgentLastSeen = &agent
	applyPhoneHealth(&d2, info)
	if d2.Online || d2.StatusSource != "" {
		t.Fatalf("con agente no se toca: %+v", d2)
	}
}

func TestApplyPhoneHealthWrongRouter(t *testing.T) {
	yes, no := true, false
	checked := "2099-01-01T00:00:00Z"
	d := store.DeviceSummary{DeviceID: "router-B", Online: false}
	info := &store.RebootInfo{Health: &store.RouterHealth{InternetOK: &yes, Fresh: true, CheckedAt: &checked, IdentityOK: &no}}
	applyPhoneHealth(&d, info)
	if d.Online || d.StatusSource != "phone-health-wrong-router" {
		t.Fatalf("con otro router en el puerto no se marca en línea: %+v", d)
	}
}
