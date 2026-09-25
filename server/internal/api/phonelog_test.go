package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhoneLogRoundTrip(t *testing.T) {
	s, _ := testAPI(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	// App vieja: solo ubicación. Debe seguir funcionando y dejar la fila sin batería.
	if rec := do("POST", "/api/v1/devices/router-x/location", `{"lat":4.7,"lon":-74.2}`); rec.Code != http.StatusOK {
		t.Fatalf("location sin batería: %d %s", rec.Code, rec.Body)
	}
	body := `{"lat":4.71,"lon":-74.22,"gps_accuracy_m":12,"battery_pct":83,"charging":false,
		"battery_status":"discharging","plugged":"usb","battery_temp_c":31.5,"net_type":"ethernet"}`
	if rec := do("POST", "/api/v1/devices/router-x/location", body); rec.Code != http.StatusOK {
		t.Fatalf("location con batería: %d %s", rec.Code, rec.Body)
	}

	rec := do("GET", "/api/v1/devices/router-x/phone_log?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("phone_log: %d %s", rec.Code, rec.Body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("esperaba 2 filas, hay %d", len(rows))
	}
	last := rows[0]
	if last["battery_pct"] != float64(83) || last["charging"] != false || last["battery_status"] != "discharging" ||
		last["plugged"] != "usb" || last["battery_temp_c"] != 31.5 || last["net_type"] != "ethernet" {
		t.Fatalf("fila más nueva mal guardada: %v", last)
	}
	if rows[1]["battery_pct"] != nil || rows[1]["charging"] != nil {
		t.Fatalf("la fila de la app vieja no debería tener batería: %v", rows[1])
	}

	if rec := do("GET", "/api/v1/devices/otro/phone_log", ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("equipo sin historial debería dar []: %d %s", rec.Code, rec.Body)
	}
}
