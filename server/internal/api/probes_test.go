package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type probeHarness struct {
	t *testing.T
	s *Server
}

func (h probeHarness) do(method, path, body string) (int, map[string]any, []any) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "172.27.0.3:40000"
	req.Header.Set("CF-Connecting-IP", "190.145.155.7")
	rec := httptest.NewRecorder()
	h.s.ServeHTTP(rec, req)
	var obj map[string]any
	var arr []any
	if strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		_ = json.Unmarshal(rec.Body.Bytes(), &arr)
	} else {
		_ = json.Unmarshal(rec.Body.Bytes(), &obj)
	}
	return rec.Code, obj, arr
}

func setupProbe(t *testing.T, interval int) probeHarness {
	t.Helper()
	s, _ := testAPI(t)
	h := probeHarness{t, s}
	code, obj, _ := h.do("PUT", "/api/v1/probes/hap-1", `{"label":"hAP","interval_s":`+itoa(interval)+`,"duration_s":10,
		"targets":[{"device_id":"notion","routing_table":"to-notion"},{"device_id":"lte4g","routing_table":"to-4g","send_heartbeat":true}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT probe: %d %v", code, obj)
	}
	return h
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestProbeAndAgentDoNotStealCommands(t *testing.T) {
	h := setupProbe(t, 0)
	// Un comando para el agente y otro para la sonda, del MISMO equipo.
	if code, obj, _ := h.do("POST", "/api/v1/commands", `{"device_id":"notion","type":"run_speedtest"}`); code != 200 {
		t.Fatalf("crear comando agente: %d %v", code, obj)
	}
	code, obj, _ := h.do("POST", "/api/v1/commands", `{"device_id":"notion","type":"run_speedtest","runner":"probe"}`)
	if code != 200 {
		t.Fatalf("crear comando sonda: %d %v", code, obj)
	}
	probeCmdID := obj["id"].(float64)

	_, agentCmd, _ := h.do("GET", "/api/v1/commands/next?device_id=notion", "")
	if agentCmd["runner"] != "agent" || agentCmd["routing_table"] != nil {
		t.Fatalf("el agente recibió algo que no es suyo: %v", agentCmd)
	}
	_, again, _ := h.do("GET", "/api/v1/commands/next?device_id=notion", "")
	if len(again) != 0 {
		t.Fatalf("el agente no debería ver el comando de la sonda: %v", again)
	}
	_, probeCmd, _ := h.do("GET", "/api/v1/commands/next?probe_id=hap-1", "")
	if probeCmd["id"] != probeCmdID || probeCmd["routing_table"] != "to-notion" || probeCmd["runner"] != "probe" {
		t.Fatalf("comando de sonda mal: %v", probeCmd)
	}

	// Cerrarlo con una medición: queda como ruta confirmada por la sonda.
	body := `{"status":"done","measurement":{"device_id":"notion","down_mbps":42.5,"up_mbps":8.1,"ping_ms":21}}`
	if code, obj, _ := h.do("POST", "/api/v1/commands/"+itoa(int(probeCmdID))+"/complete", body); code != 200 {
		t.Fatalf("complete: %d %v", code, obj)
	}
	_, _, rows := h.do("GET", "/api/v1/measurements?device_id=notion", "")
	if len(rows) != 1 {
		t.Fatalf("esperaba 1 medición, hay %d", len(rows))
	}
	m := rows[0].(map[string]any)
	net, _ := m["net"].(map[string]any)
	if m["net_route"] != "router" || net["reason"] != "measured-by-probe" || m["source"] != "mikrotik-probe" ||
		m["probe_id"] != "hap-1" || m["routing_table"] != "to-notion" {
		t.Fatalf("medición de sonda mal etiquetada: %v", m)
	}
}

func TestProbeCommandRequiresTarget(t *testing.T) {
	h := setupProbe(t, 0)
	if code, obj, _ := h.do("POST", "/api/v1/commands", `{"device_id":"otro","type":"run_speedtest","runner":"probe"}`); code != 400 {
		t.Fatalf("un equipo sin sonda debería dar 400: %d %v", code, obj)
	}
	if code, _, _ := h.do("POST", "/api/v1/commands", `{"device_id":"notion","type":"run_speedtest","runner":"robot"}`); code != 400 {
		t.Fatalf("runner inválido debería dar 400: %d", code)
	}
	if code, _, _ := h.do("GET", "/api/v1/commands/next?probe_id=no-existe", ""); code != 404 {
		t.Fatalf("sonda no configurada debería dar 404: %d", code)
	}
}

func TestProbeCycleAlternatesAndDoesNotDuplicate(t *testing.T) {
	h := setupProbe(t, 0)
	code, obj, _ := h.do("POST", "/api/v1/probes/hap-1/cycle", "")
	if code != 200 || len(obj["command_ids"].([]any)) != 2 {
		t.Fatalf("ciclo: %d %v", code, obj)
	}
	// Pedirlo otra vez con el anterior sin terminar no duplica nada.
	_, obj, _ = h.do("POST", "/api/v1/probes/hap-1/cycle", "")
	if len(obj["command_ids"].([]any)) != 0 {
		t.Fatalf("ciclo repetido duplicó pruebas: %v", obj)
	}
	var order []string
	for i := 0; i < 2; i++ {
		_, c, _ := h.do("GET", "/api/v1/commands/next?probe_id=hap-1", "")
		order = append(order, c["routing_table"].(string))
		if c["duration_s"] != float64(10) {
			t.Fatalf("el ciclo debería usar duration_s de la sonda: %v", c)
		}
	}
	if order[0] != "to-notion" || order[1] != "to-4g" {
		t.Fatalf("orden del ciclo: %v", order)
	}
}

func TestProbeScheduler(t *testing.T) {
	h := setupProbe(t, 60)
	ctx := context.Background()
	st := h.s.store

	// Nunca consultó la cola: se la considera apagada y no se le encola nada.
	if enq, err := st.EnqueueDueProbeCycles(ctx, time.Now()); err != nil || len(enq) != 0 {
		t.Fatalf("sonda apagada no debería recibir ciclos: %v %v", enq, err)
	}
	h.do("GET", "/api/v1/probes/hap-1/config", "") // señal de vida
	enq, err := st.EnqueueDueProbeCycles(ctx, time.Now())
	if err != nil || len(enq["hap-1"]) != 2 {
		t.Fatalf("primer ciclo automático: %v %v", enq, err)
	}
	// Con el ciclo anterior sin ejecutar, aunque pase el intervalo, se espera.
	if enq, _ := st.EnqueueDueProbeCycles(ctx, time.Now().Add(61*time.Second)); len(enq) != 0 {
		t.Fatalf("no debería encolar con comandos abiertos: %v", enq)
	}
	for i := 0; i < 2; i++ {
		_, c, _ := h.do("GET", "/api/v1/commands/next?probe_id=hap-1", "")
		h.do("POST", "/api/v1/commands/"+itoa(int(c["id"].(float64)))+"/complete", `{"status":"failed","error":"test"}`)
	}
	if enq, _ := st.EnqueueDueProbeCycles(ctx, time.Now().Add(30*time.Second)); len(enq) != 0 {
		t.Fatalf("antes del intervalo no toca: %v", enq)
	}
	if enq, _ := st.EnqueueDueProbeCycles(ctx, time.Now().Add(61*time.Second)); len(enq["hap-1"]) != 2 {
		t.Fatalf("pasado el intervalo sí toca: %v", enq)
	}

	_, p, _ := h.do("GET", "/api/v1/probes/hap-1", "")
	if p["online"] != true || p["interval_s"] != float64(60) || len(p["targets"].([]any)) != 2 {
		t.Fatalf("GET probe: %v", p)
	}
	if code, _, _ := h.do("PUT", "/api/v1/probes/hap-1", `{"interval_s":30,"targets":[]}`); code != 400 {
		t.Fatalf("interval_s < 60 debería rechazarse: %d", code)
	}
}
