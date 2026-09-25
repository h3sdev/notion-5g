package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"notion5g/server/internal/store"
)

// testAPI arma un servidor completo (store en disco temporal) con el peer del
// túnel fijo, para probar el POST de mediciones de punta a punta.
//
// netEnrich en nil a propósito: el worker de ASN reclasifica DESPUÉS del
// insert y consulta DNS, lo que haría estos tests lentos y no deterministas.
// Lo que se prueba acá es lo que queda guardado en el camino del POST.
func testAPI(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st, "")
	s.netEnrich = nil
	p := &trustedProxy{host: "test", addrs: []netip.Addr{netip.MustParseAddr("172.27.0.3")}, everOK: true}
	p.checked = farFuture()
	s.proxy = p
	return s, st
}

// post manda un POST /api/v1/measurements como lo vería el backend detrás del
// túnel: peer = contenedor de cloudflared, IP real en CF-Connecting-IP.
func post(t *testing.T, s *Server, clientIP, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/measurements", strings.NewReader(body))
	r.RemoteAddr = "172.27.0.3:41234"
	if clientIP != "" {
		r.Header.Set("CF-Connecting-IP", clientIP)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST -> %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func rawOf(t *testing.T, st *store.Store, f store.ListFilter) []map[string]any {
	t.Helper()
	f.Limit = 50
	rows, err := st.ListRaw(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var m map[string]any
		if err := json.Unmarshal(row, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// El caso que fallaba: un ítem que NO participa del esquema (sin test_id, sin
// net_hint, sin net_declared y source != "browser") se insertaba VERBATIM, así
// que el cliente podía escribir él mismo net_route="router" y net.confidence
// ="high". La X-API-Key está en el JS del dashboard: alcanza con tener el link.
func TestPostMeasurementsNoDejaAlClienteFijarLaRuta(t *testing.T) {
	s, st := testAPI(t)
	post(t, s, "190.145.155.7", `{"device_id":"router-A","source":"pc","down_mbps":9999,
		"net_route":"router","net_asn":66666,"net":{"confidence":"high","reason":"inventado"},
		"client_ip":"1.2.3.4"}`)

	if rows := rawOf(t, st, store.ListFilter{NetRoute: "router"}); len(rows) != 0 {
		t.Errorf("el cliente logró marcar %d fila(s) como 'router': %v", len(rows), rows)
	}
	rows := rawOf(t, st, store.ListFilter{})
	if len(rows) != 1 {
		t.Fatalf("filas = %d", len(rows))
	}
	for _, k := range []string{"net_route", "net_asn", "net", "client_ip"} {
		if _, ok := rows[0][k]; ok {
			t.Errorf("%q del cliente quedó guardado en raw: %v", k, rows[0][k])
		}
	}
	if rows[0]["down_mbps"] != float64(9999) {
		t.Errorf("se perdió un campo real de la medición: %v", rows[0])
	}
}

// Mismo agujero por el otro camino: un tipo raro en un campo conocido hacía
// fallar el json.Unmarshal de classifyItem, que devolvía el ítem tal cual.
func TestPostMeasurementsPayloadRaroTampocoFijaLaRuta(t *testing.T) {
	s, st := testAPI(t)
	post(t, s, "190.145.155.7", `{"device_id":"router-A","test_id":123,"down_mbps":8888,"net_route":"router"}`)
	if rows := rawOf(t, st, store.ListFilter{NetRoute: "router"}); len(rows) != 0 {
		t.Errorf("el cliente logró marcar %d fila(s) como 'router'", len(rows))
	}
	rows := rawOf(t, st, store.ListFilter{})
	if len(rows) != 1 {
		t.Fatalf("filas = %d", len(rows))
	}
	if _, ok := rows[0]["net_route"]; ok {
		t.Error("net_route del cliente quedó guardado en raw")
	}
}

// Y en la rama que SÍ participa, lo que manda el cliente tampoco puede ganarle
// a lo que observó el servidor.
func TestPostMeasurementsLaClasificacionEsLaDelServidor(t *testing.T) {
	s, st := testAPI(t)
	ctx := context.Background()
	if err := st.SetDeviceEgress(ctx, "router-A", "190.145.155.7", "v4", "CO"); err != nil {
		t.Fatal(err)
	}
	// El celular sale por una IP distinta a la del equipo, pero dice "router".
	resp := post(t, s, "181.49.1.1", `{"device_id":"router-A","source":"browser","down_mbps":70,
		"net_declared":"router","net_route":"router","net":{"confidence":"high"}}`)
	nets := resp["net"].([]any)
	got := nets[0].(map[string]any)
	if got["net_route"] != "other-network" {
		t.Fatalf("net_route = %v, quería other-network", got["net_route"])
	}
	rows := rawOf(t, st, store.ListFilter{NetRoute: "other-network"})
	if len(rows) != 1 {
		t.Fatalf("filas other-network = %d", len(rows))
	}
	net, _ := rows[0]["net"].(map[string]any)
	if net["reason"] != "ip-differs-from-router" || net["confidence"] != "medium" {
		t.Errorf("evidencia guardada = %v", net)
	}
	if net["declared_matches"] != false {
		t.Errorf("declaró 'router' y salió por otra red: declared_matches = %v", net["declared_matches"])
	}
}

// La velocidad titular del equipo (el número grande del dashboard) no puede
// salir de un "router" de confianza baja: esos son los casos en que la única
// evidencia es que la IP pública coincide, y bajo el CGNAT móvil eso también
// pasa cuando el celular mide por su propia SIM.
func TestVelocidadTitularIgnoraRouterDeConfianzaBaja(t *testing.T) {
	s, st := testAPI(t)
	ctx := context.Background()
	if err := st.SetDeviceEgress(ctx, "router-A", "190.145.155.7", "v4", "CO"); err != nil {
		t.Fatal(err)
	}

	// 1) Misma IP + el navegador confirma que está en un wifi -> confianza media.
	resp := post(t, s, "190.145.155.7", `{"device_id":"router-A","source":"browser","down_mbps":500,
		"net_hint":{"api":"network-information","type":"wifi"}}`)
	first := resp["net"].([]any)[0].(map[string]any)
	if first["net_route"] != "router" || first["confidence"] != "medium" {
		t.Fatalf("primera medición = %v, quería router/medium", first)
	}

	// 2) Misma IP y nada más (iPhone: no hay navigator.connection) -> baja.
	resp = post(t, s, "190.145.155.7", `{"device_id":"router-A","source":"browser","down_mbps":7}`)
	second := resp["net"].([]any)[0].(map[string]any)
	if second["net_route"] != "router" || second["confidence"] != "low" {
		t.Fatalf("segunda medición = %v, quería router/low", second)
	}

	ds, err := st.ListDeviceSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].DownMbps == nil {
		t.Fatalf("resumen = %+v", ds)
	}
	if *ds[0].DownMbps != 500 {
		t.Errorf("velocidad titular = %v, quería 500 (la de confianza baja no asciende)", *ds[0].DownMbps)
	}
}

// El heartbeat del agente es lo que enseña la IP de referencia del equipo. Si
// eso dejara de funcionar, no habría contra qué comparar y TODA medición
// quedaría "sin determinar" para siempre, sin recuperarse sola.
func TestHeartbeatAprendeLaIPDeSalida(t *testing.T) {
	s, st := testAPI(t)
	// Peer del túnel que nunca resolvió: la degradación tiene que seguir
	// aprendiendo la referencia, no apagar la detección.
	s.proxy = &trustedProxy{host: "no-existe"}
	s.proxy.checked = farFuture()

	r := httptest.NewRequest("POST", "/api/v1/heartbeat", strings.NewReader(`{"device_id":"router-A","rat":"LTE"}`))
	r.RemoteAddr = "172.27.0.3:41234"
	r.Header.Set("CF-Connecting-IP", "190.145.155.7")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat -> %d %s", w.Code, w.Body.String())
	}
	ref, ok := st.DeviceEgressOf(context.Background(), "router-A")
	if !ok || ref.IP != "190.145.155.7" {
		t.Fatalf("device_egress = %+v ok=%v", ref, ok)
	}

	// Y la medición que sale de ahí se clasifica, pero con confianza baja.
	resp := post(t, s, "190.145.155.7", `{"device_id":"router-A","source":"browser","down_mbps":50,
		"net_hint":{"api":"network-information","type":"wifi"}}`)
	got := resp["net"].([]any)[0].(map[string]any)
	if got["net_route"] != "router" || got["confidence"] != "low" {
		t.Errorf("con el peer sin verificar: %v, quería router/low", got)
	}
}
