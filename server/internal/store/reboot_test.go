package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests del reinicio remoto (contrato docs/CONTRATO-SONDA-AB.md §6.3).

func boolp(b bool) *bool { return &b }

func durp(d time.Duration) *time.Duration { return &d }

func rebootStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	var p Probe
	if err := json.Unmarshal([]byte(`{"label":"hAP","runner":"phone","interval_s":0,"duration_s":10,
 "targets":[{"slot":"A","device_id":"router-A","routing_table":"to-A"},{"slot":"B","device_id":"router-B","routing_table":"to-B"}]}`), &p); err != nil {
		t.Fatal(err)
	}
	p.ProbeID = "hap-oficina"
	if err := st.UpsertProbe(ctx, p); err != nil {
		t.Fatal(err)
	}
	var q Probe
	_ = json.Unmarshal([]byte(`{"label":"script","interval_s":0,"targets":[{"device_id":"router-S","routing_table":"to-s"}]}`), &q)
	q.ProbeID = "hap-script"
	if err := st.UpsertProbe(ctx, q); err != nil {
		t.Fatal(err)
	}
	// Celular con la app que conoce reboot_router (manda "routers").
	if _, _, err := st.UpsertProbeStatus(ctx, "hap-oficina", json.RawMessage(`{"measured_by":"phone-1","routers":{}}`),
		ProbeStatusUpdate{MeasuredBy: "phone-1"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestEvalRebootRules(t *testing.T) {
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	fresh := func(internet, gateway bool) *RouterHealth {
		return &RouterHealth{InternetOK: boolp(internet), GatewayOK: boolp(gateway), Fresh: true}
	}
	order := func(status string, created time.Duration, step string) *PhoneOrder {
		o := &PhoneOrder{OrderID: "r-1", Status: status, CreatedAt: FormatTS(now.Add(-created))}
		if step != "" {
			o.Step = &step
		}
		return o
	}
	cases := []struct {
		name        string
		in          rebootInputs
		recommended bool
		offline     bool // offline_since presente
		reason      string
	}{
		{"agente al día", rebootInputs{agentLast: ago(time.Minute)}, false, false, "En línea"},
		{"agente 4 min: sin conexión, todavía no se recomienda", rebootInputs{agentLast: ago(4 * time.Minute), phoneOnline: true}, false, true, "se recomienda reiniciar pasados 10"},
		{"agente 11 min", rebootInputs{agentLast: ago(11 * time.Minute), phoneOnline: true}, true, true, "el agente del router no reporta hace 11 min"},
		{"justo 10 min no alcanza (> 10)", rebootInputs{agentLast: ago(10 * time.Minute), phoneOnline: true}, false, true, ""},
		{"celular: sin Internet hace 11 min", rebootInputs{phoneOnline: true, health: fresh(false, true), downFor: durp(11 * time.Minute)}, true, true, "el celular no ve Internet por este router hace 11 min"},
		{"celular: sin Internet hace 9 min", rebootInputs{phoneOnline: true, health: fresh(false, true), downFor: durp(9 * time.Minute)}, false, true, ""},
		{"celular: estado viejo no decide", rebootInputs{health: &RouterHealth{InternetOK: boolp(false), Fresh: false}, downFor: durp(time.Hour)}, false, false, "Sin datos"},
		{"router apagado", rebootInputs{phoneOnline: true, health: fresh(false, false), downFor: durp(20 * time.Minute)}, true, true, "parece apagado"},
		{"celular caído pero agente al día (O)", rebootInputs{agentLast: ago(time.Minute), phoneOnline: true, health: fresh(false, true), downFor: durp(12 * time.Minute)}, true, true, "el agente sí reporta"},
		{"agente caído, celular fuera de línea", rebootInputs{agentLast: ago(30 * time.Minute)}, true, true, "el celular de la sonda no está en línea"},
		{"sin agente ni salud", rebootInputs{}, false, false, "Sin datos"},
		{"celular ve Internet", rebootInputs{phoneOnline: true, health: fresh(true, true)}, false, false, "En línea"},
		{"reinicio en curso", rebootInputs{agentLast: ago(30 * time.Minute), open: order("running", 2*time.Minute, "waiting-back")}, false, true, "Reinicio en curso (running, waiting-back)"},
		{"enfriamiento", rebootInputs{agentLast: ago(30 * time.Minute), counted: order("done", 5*time.Minute, "")}, false, true, "Reiniciado hace 5 min"},
		{"enfriamiento vencido", rebootInputs{agentLast: ago(30 * time.Minute), phoneOnline: true, counted: order("done", 16*time.Minute, "")}, true, true, "Sin conexión hace 30 min"},
	}
	for _, c := range cases {
		c.in.now = now
		c.in.probeID, c.in.slot = "hap-oficina", "A"
		info := evalReboot(c.in)
		if info.Recommended != c.recommended || (info.OfflineSince != nil) != c.offline || !strings.Contains(info.Reason, c.reason) {
			t.Errorf("%s: recommended=%v offline_since=%v reason=%q", c.name, info.Recommended, info.OfflineSince, info.Reason)
		}
		if info.ProbeID != "hap-oficina" || info.Slot != "A" {
			t.Errorf("%s: probe/slot %v", c.name, info)
		}
	}
	// offline_since = el más viejo de las dos señales.
	info := evalReboot(rebootInputs{now: now, agentLast: ago(12 * time.Minute), phoneOnline: true, health: fresh(false, true), downFor: durp(20 * time.Minute)})
	if *info.OfflineSince != FormatTS(now.Add(-20*time.Minute)) || *info.OfflineMin != 20 {
		t.Fatalf("offline_since: %v %v", *info.OfflineSince, *info.OfflineMin)
	}
	// Datos del último reinicio y fin del enfriamiento.
	last := order("done", 5*time.Minute, "done")
	info = evalReboot(rebootInputs{now: now, last: last, counted: last})
	if info.LastRebootStatus == nil || *info.LastRebootStatus != "done" || *info.LastRebootAt != last.CreatedAt ||
		*info.CooldownUntil != FormatTS(now.Add(10*time.Minute)) || *info.LastRebootOrderID != "r-1" {
		t.Fatalf("último reinicio: %+v", info)
	}
}

func TestParseRouterHealth(t *testing.T) {
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	// El reloj del celular va 1 h adelantado: solo cuentan sus diferencias.
	raw := json.RawMessage(`{"sent_at":"2026-09-29T22:00:00Z","routers":{
		"A":{"internet_ok":false,"gateway_ok":true,"loss_pct":100,"rtt_ms":null,"checked_at":"2026-09-29T21:59:30Z","down_since":"2026-09-29T21:48:00Z"},
		"B":{"internet_ok":true,"gateway_ok":true,"loss_pct":0,"rtt_ms":38.2,"checked_at":"2026-09-29T21:30:00Z","down_since":null},
		"C":"basura"}}`)
	h, down := parseRouterHealth(raw, "A", 30*time.Second, now)
	if h == nil || !h.Fresh || *h.InternetOK || !*h.GatewayOK || *h.LossPct != 100 || h.RTTMs != nil || down == nil ||
		*down != 12*time.Minute+30*time.Second {
		t.Fatalf("A: %+v %v", h, down)
	}
	if h, down := parseRouterHealth(raw, "B", 30*time.Second, now); h == nil || h.Fresh || down != nil || *h.RTTMs != 38.2 {
		t.Fatalf("B (verificación de hace 30 min: no fresca): %+v %v", h, down)
	}
	if h, _ := parseRouterHealth(raw, "A", 4*time.Minute, now); h.Fresh {
		t.Fatal("estado de hace 4 min no es fresco")
	}
	if h, _ := parseRouterHealth(raw, "C", 0, now); h != nil {
		t.Fatalf("C inválido: %+v", h)
	}
	if h, _ := parseRouterHealth(json.RawMessage(`{"phase":"idle"}`), "A", 0, now); h != nil {
		t.Fatal("sin routers")
	}
	// Sin sent_at: down_since contra la hora del servidor.
	_, down = parseRouterHealth(json.RawMessage(`{"routers":{"A":{"internet_ok":false,"down_since":"2026-09-29T20:45:00Z"}}}`), "A", 0, now)
	if down == nil || *down != 15*time.Minute {
		t.Fatalf("sin sent_at: %v", down)
	}
}

func TestCreateRebootOrderIdempotentAndCooldown(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	req := RebootRequest{OrderID: "reb00000-0000-4000-8000-000000000001", RequestedBy: "dashboard", Reason: "recommended"}

	o, existing, err := st.CreateRebootOrder(ctx, "router-A", req, now)
	if err != nil || existing {
		t.Fatalf("crear: %v %v", existing, err)
	}
	if o.Type != RebootOrderType || o.Status != "pending" || *o.Slot != "A" || *o.RoutingTable != "to-A" || o.ProbeID != "hap-oficina" ||
		*o.NotAfter != FormatTS(now.Add(15*time.Minute)) || *o.RebootReason != "recommended" || o.DurationS != nil ||
		o.SelectionReason != nil || o.Result != nil || *o.BatchID != o.OrderID {
		t.Fatalf("orden mal armada: %+v", o)
	}
	// Mismo order_id (aunque cambie el cuerpo): la misma orden, sin fila nueva.
	again, existing, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{OrderID: req.OrderID, Force: true, Reason: "manual"}, now.Add(time.Minute))
	if err != nil || !existing || again.ID != o.ID {
		t.Fatalf("reintento: %v %v %+v", existing, err, again)
	}
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM commands WHERE type='reboot_router'`).Scan(&n)
	if n != 1 {
		t.Fatalf("filas: %d", n)
	}
	// El mismo order_id para otro equipo: en uso.
	if _, _, err := st.CreateRebootOrder(ctx, "router-B", req, now); !errors.Is(err, ErrOrderIDInUse) {
		t.Fatalf("otro equipo: %v", err)
	}
	// En curso: 409 aunque venga force.
	var conflict RebootConflictError
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{Force: true}, now.Add(time.Minute)); !errors.As(err, &conflict) || !conflict.InProgress || conflict.Order.OrderID != req.OrderID {
		t.Fatalf("en curso: %v", err)
	}
	// El otro router sí se puede reiniciar a la vez.
	if _, _, err := st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, now); err != nil {
		t.Fatalf("router-B: %v", err)
	}

	// El celular lo ejecuta y lo cierra.
	if _, ign, err := st.SetPhoneOrderState(ctx, "hap-oficina", req.OrderID, PhoneStateUpdate{Status: "running", Step: "ssh"}, now.Add(time.Minute)); err != nil || ign {
		t.Fatalf("running: %v %v", ign, err)
	}
	if _, ign, err := st.SetPhoneOrderState(ctx, "hap-oficina", req.OrderID, PhoneStateUpdate{Status: "done", Result: json.RawMessage(`{"ssh_ok":true,"gateway_back_s":95}`)}, now.Add(3*time.Minute)); err != nil || ign {
		t.Fatalf("done: %v %v", ign, err)
	}
	// Enfriamiento: 409 sin force, con los datos del último reinicio.
	conflict = RebootConflictError{}
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now.Add(10*time.Minute)); !errors.As(err, &conflict) || conflict.InProgress ||
		conflict.CooldownUntil != FormatTS(now.Add(15*time.Minute)) || conflict.LastRebootAt != FormatTS(now) {
		t.Fatalf("enfriamiento: %v %+v", err, conflict)
	}
	// Con force sí.
	forced, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{Force: true}, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("force: %v", err)
	}
	// Se cancela desde el dashboard: un cancelado no cuenta para el enfriamiento...
	if c, _, _, err := st.CancelPhoneOrders(ctx, "hap-oficina", CancelRequest{OrderIDs: []string{forced.OrderID}}, now.Add(10*time.Minute)); err != nil || len(c) != 1 {
		t.Fatalf("cancelar: %v %v", c, err)
	}
	// ...pero el done de hace 11 min sí.
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now.Add(11*time.Minute)); !errors.As(err, &conflict) {
		t.Fatalf("enfriamiento tras cancelar: %v", err)
	}
	// Pasados 15 min, sin force.
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now.Add(16*time.Minute)); err != nil {
		t.Fatalf("tras enfriamiento: %v", err)
	}
}

func TestRebootFailedBeforeSSHDoesNotCool(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	for i, errCode := range []string{"ssh-connect", "timeout-back", "mikrotik-unreachable"} {
		t0 := now.Add(time.Duration(i) * time.Hour)
		o, _, err := st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, t0)
		if err != nil {
			t.Fatal(err)
		}
		st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, PhoneStateUpdate{Status: "running"}, t0)
		// mikrotik-unreachable acá es mientras se vigilaba la vuelta: el comando salió.
		sshOK := errCode == "mikrotik-unreachable"
		st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, PhoneStateUpdate{Status: "failed", Error: errCode,
			Result: json.RawMessage(fmt.Sprintf(`{"ssh_ok":%v}`, sshOK))}, t0.Add(time.Minute))
		_, _, err = st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, t0.Add(2*time.Minute))
		var conflict RebootConflictError
		switch errCode {
		case "ssh-connect": // el comando nunca salió: se puede reintentar ya
			if err != nil {
				t.Fatalf("tras ssh-connect: %v", err)
			}
			st.CancelPhoneOrders(ctx, "hap-oficina", CancelRequest{AllOpen: true}, t0.Add(2*time.Minute))
		case "timeout-back", "mikrotik-unreachable": // el comando salió: enfriamiento
			if !errors.As(err, &conflict) {
				t.Fatalf("tras timeout-back: %v", err)
			}
		}
	}
	// Vencida sin ejecutar (barrido del servidor): no cuenta.
	t1 := now.Add(5 * time.Hour)
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, t1); err != nil {
		t.Fatal(err)
	}
	if exp, _, _ := st.SweepPhoneOrders(ctx, "", t1.Add(16*time.Minute)); exp != 1 {
		t.Fatalf("barrido: %d", exp)
	}
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, t1.Add(16*time.Minute)); err != nil {
		t.Fatalf("tras vencida: %v", err)
	}
}

func TestCreateRebootOrderValidation(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Now()
	if _, _, err := st.CreateRebootOrder(ctx, "router-X", RebootRequest{}, now); !errors.Is(err, ErrRebootNoProbe) {
		t.Fatalf("sin sonda: %v", err)
	}
	// Un equipo de una sonda de script (runner probe) no se reinicia por acá.
	if _, _, err := st.CreateRebootOrder(ctx, "router-S", RebootRequest{}, now); !errors.Is(err, ErrRebootNoProbe) {
		t.Fatalf("sonda de script: %v", err)
	}
	var br BadRequestError
	for _, req := range []RebootRequest{{Reason: "porque sí"}, {OrderID: "x"}, {OrderID: "reb00000-0000-4000-8000-00000000-1"}} {
		if _, _, err := st.CreateRebootOrder(ctx, "router-A", req, now); !errors.As(err, &br) {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	// order_id de una prueba de velocidad: en uso.
	if _, _, err := st.CreatePhoneOrders(ctx, "hap-oficina", OrderRequest{OrderID: "spd00000-0000-4000-8000-000000000001", Target: "A"}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{OrderID: "spd00000-0000-4000-8000-000000000001"}, now); !errors.Is(err, ErrOrderIDInUse) {
		t.Fatalf("order_id de prueba: %v", err)
	}
}

// Una orden de reinicio nunca la toma el agente del router ni el script de la
// sonda, nunca produce una medición y no frena el ciclo de pruebas.
func TestRebootOrderIsolation(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	o, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := st.ClaimNextCommand(ctx, "router-A"); err != nil || c != nil {
		t.Fatalf("el agente tomó el reinicio: %+v %v", c, err)
	}
	if c, err := st.ClaimNextProbeCommand(ctx, "hap-oficina"); err != nil || c != nil {
		t.Fatalf("el script tomó el reinicio: %+v %v", c, err)
	}
	// CompleteCommand (agente) sobre la orden: 400, sin medición.
	if _, err := st.CompleteCommand(ctx, o.ID, json.RawMessage(`{"status":"done","measurement":{"device_id":"router-A","down_mbps":1}}`)); err == nil {
		t.Fatal("CompleteCommand cerró un reinicio")
	}
	// El ciclo encola pruebas para A y B aunque A tenga un reinicio pendiente.
	ids, _, err := st.EnqueueProbeCycleOrders(ctx, "hap-oficina", "test")
	if err != nil || len(ids) != 2 {
		t.Fatalf("ciclo: %v %v", ids, err)
	}
	// Un resultado con el order_id del reinicio: rechazado, sin fila.
	_, err = st.InsertProbeResult(ctx, json.RawMessage(`{"device_id":"router-A","down_mbps":50}`), ProbeResultCols{
		ResultID: "res00000-0000-4000-8000-000000000001", OrderID: o.OrderID, ProbeID: "hap-oficina", MeasuredBy: "phone-1",
		DeviceID: "router-A", TestStartedAt: now, TestStatus: "done", NetPath: "ethernet",
	}, NetClassification{}, now)
	var bad ErrBadResult
	if !errors.As(err, &bad) {
		t.Fatalf("resultado sobre reinicio: %v", err)
	}
	// Cerrarla con done tampoco genera medición.
	st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, PhoneStateUpdate{Status: "done", Result: json.RawMessage(`{"ssh_ok":true}`)}, now)
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM measurements`).Scan(&n)
	if n != 0 {
		t.Fatalf("mediciones: %d", n)
	}
	var mid any
	st.db.QueryRow(`SELECT measurement_id FROM commands WHERE id = ?`, o.ID).Scan(&mid)
	if mid != nil {
		t.Fatalf("measurement_id: %v", mid)
	}
}

func TestRebootOrderState(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	o, _, _ := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now)
	set := func(u PhoneStateUpdate, at time.Time) (string, bool) {
		t.Helper()
		cur, ign, err := st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, u, at)
		if err != nil {
			t.Fatalf("%+v: %v", u, err)
		}
		return cur, ign
	}
	if cur, ign := set(PhoneStateUpdate{Status: "running", Step: "ssh"}, now); cur != "running" || ign {
		t.Fatal("running")
	}
	if _, ign := set(PhoneStateUpdate{Status: "running", Step: "ssh"}, now); !ign {
		t.Fatal("running con el mismo step debe ignorarse")
	}
	if _, ign := set(PhoneStateUpdate{Status: "running", Step: "waiting-back", Result: json.RawMessage(`{"ssh_ok":true,"host_key_fp":"SHA256:x"}`)}, now); ign {
		t.Fatal("progreso ignorado")
	}
	got, _ := queryPhoneOrders(ctx, st.db, `order_id = ?`, o.OrderID)
	if *got[0].Step != "waiting-back" || !strings.Contains(string(got[0].Result), "host_key_fp") {
		t.Fatalf("progreso: %+v", got[0])
	}
	// Barrido: sin noticias 30 min → interrupted (final blando).
	st.SweepPhoneOrders(ctx, "", now.Add(31*time.Minute))
	// Un done tardío la cierra igual.
	if cur, ign := set(PhoneStateUpdate{Status: "done", Step: "done", Result: json.RawMessage(`{"ssh_ok":true,"gateway_back_s":95,"internet_back_s":140}`)}, now.Add(32*time.Minute)); cur != "done" || ign {
		t.Fatalf("done tardío: %v %v", cur, ign)
	}
	got, _ = queryPhoneOrders(ctx, st.db, `order_id = ?`, o.OrderID)
	if *got[0].ClosedBy != "phone" || !strings.Contains(string(got[0].Result), "internet_back_s") || got[0].CompletedAt == nil {
		t.Fatalf("cerrada: %+v", got[0])
	}
	// Final duro: nada lo cambia.
	if cur, ign := set(PhoneStateUpdate{Status: "failed", Error: "timeout-back"}, now.Add(33*time.Minute)); cur != "done" || !ign {
		t.Fatal("failed sobre done")
	}
	// done/failed por /state en una prueba de velocidad: 400.
	sp, _, _ := st.CreatePhoneOrders(ctx, "hap-oficina", OrderRequest{Target: "B"}, now)
	var br BadRequestError
	if _, _, err := st.SetPhoneOrderState(ctx, "hap-oficina", sp[0].OrderID, PhoneStateUpdate{Status: "done"}, now); !errors.As(err, &br) {
		t.Fatalf("done en prueba: %v", err)
	}
}

func TestRebootInfosFromDB(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	// Agente de router-A: último heartbeat hace 20 min.
	st.db.Exec(`INSERT INTO heartbeats (received_at, device_id, raw) VALUES (?, 'router-A', '{"device_id":"router-A","source":"router"}')`, FormatTS(now.Add(-20*time.Minute)))
	// Un heartbeat que no es del agente no cuenta.
	st.db.Exec(`INSERT INTO heartbeats (received_at, device_id, raw) VALUES (?, 'router-B', '{"device_id":"router-B","source":"mikrotik-probe"}')`, FormatTS(now.Add(-20*time.Minute)))
	// Celular: B sin Internet hace 12 min, router apagado.
	status := `{"measured_by":"phone-1","sent_at":"` + FormatTS(now) + `","routers":{"A":{"internet_ok":true,"gateway_ok":true,"checked_at":"` + FormatTS(now) + `"},
		"B":{"internet_ok":false,"gateway_ok":false,"checked_at":"` + FormatTS(now) + `","down_since":"` + FormatTS(now.Add(-12*time.Minute)) + `"}}}`
	if _, _, err := st.UpsertProbeStatus(ctx, "hap-oficina", json.RawMessage(status), ProbeStatusUpdate{MeasuredBy: "phone-1"}, now); err != nil {
		t.Fatal(err)
	}
	infos, err := st.RebootInfos(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	a, b := infos["router-A"], infos["router-B"]
	if a == nil || b == nil || infos["router-S"] != nil {
		t.Fatalf("infos: %v", infos)
	}
	if !a.Recommended || *a.AgentLastSeen != FormatTS(now.Add(-20*time.Minute)) || !a.Health.Fresh || !*a.Health.InternetOK || !a.PhoneOnline {
		t.Fatalf("A (agente caído, O): %+v", a)
	}
	if !b.Recommended || b.AgentLastSeen != nil || *b.OfflineMin != 12 || !strings.Contains(b.Reason, "parece apagado") {
		t.Fatalf("B: %+v", b)
	}
	// Con un reinicio pedido deja de recomendarse y lo muestra.
	o, _, _ := st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, now)
	infos, _ = st.ProbeRebootInfos(ctx, "hap-oficina", now)
	if b := infos["router-B"]; b.Recommended || !b.InProgress || *b.LastRebootOrderID != o.OrderID || *b.LastRebootStatus != "pending" {
		t.Fatalf("B en curso: %+v", b)
	}
	// Cancelado: deja de estar en curso y no cuenta como último reinicio.
	st.CancelPhoneOrders(ctx, "hap-oficina", CancelRequest{OrderIDs: []string{o.OrderID}}, now)
	infos, _ = st.ProbeRebootInfos(ctx, "hap-oficina", now)
	if b := infos["router-B"]; !b.Recommended || b.InProgress || b.LastRebootAt != nil || b.CooldownUntil != nil {
		t.Fatalf("B cancelado: %+v", b)
	}
}

// Una app vieja (estado sin "routers") correría el reinicio como una prueba:
// no se crea la orden.
func TestRebootRequiresAppWithRouterHealth(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, raw := range []string{`{"measured_by":"phone-1","phase":"idle"}`, `{"measured_by":"phone-1","routers":null}`} {
		st.UpsertProbeStatus(ctx, "hap-oficina", json.RawMessage(raw), ProbeStatusUpdate{MeasuredBy: "phone-1"}, now)
		if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now); !errors.Is(err, ErrRebootAppOutdated) {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	// Sin ningún estado del celular tampoco.
	st.db.Exec(`DELETE FROM probe_status`)
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now); !errors.Is(err, ErrRebootAppOutdated) {
		t.Fatalf("sin estado: %v", err)
	}
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM commands`).Scan(&n)
	if n != 0 {
		t.Fatalf("filas: %d", n)
	}
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{RequestedBy: "x\nFALSO"}, now); err == nil {
		t.Fatal("requested_by con salto de línea")
	}
}

// Carrera: el dashboard cancela (o el barrido vence) un reinicio que el
// celular ya había tomado. Lo que informa el celular manda, el enfriamiento
// cuenta, y el reinicio que se pidió en el medio se cancela.
func TestRebootCancelledButRunByPhone(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	first, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if c, _, _, _ := st.CancelPhoneOrders(ctx, "hap-oficina", CancelRequest{OrderIDs: []string{first.OrderID}}, now.Add(10*time.Second)); len(c) != 1 {
		t.Fatal("cancelar")
	}
	// Cancelado: no está en curso ni enfría, así que se puede pedir otro.
	second, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now.Add(20*time.Second))
	if err != nil {
		t.Fatalf("segundo: %v", err)
	}
	// El celular ya corría el primero.
	if cur, ign, err := st.SetPhoneOrderState(ctx, "hap-oficina", first.OrderID, PhoneStateUpdate{Status: "running", Step: "reading_gateway"}, now.Add(30*time.Second)); err != nil || ign || cur != "running" {
		t.Fatalf("running sobre cancelado: %v %v %v", cur, ign, err)
	}
	got, _ := queryPhoneOrders(ctx, st.db, `order_id IN (?,?) ORDER BY id`, first.OrderID, second.OrderID)
	if got[0].Status != "running" || got[0].ClosedBy != nil || got[0].CompletedAt != nil || got[0].Error != nil {
		t.Fatalf("primero reabierto: %+v", got[0])
	}
	if got[1].Status != "cancelled" || *got[1].ClosedBy != "server" || *got[1].Error != "reinicio-duplicado" {
		t.Fatalf("segundo no cancelado: %+v", got[1])
	}
	// En curso: 409 aunque venga force.
	var conflict RebootConflictError
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{Force: true}, now.Add(40*time.Second)); !errors.As(err, &conflict) || !conflict.InProgress {
		t.Fatalf("en curso: %v", err)
	}
	st.SetPhoneOrderState(ctx, "hap-oficina", first.OrderID, PhoneStateUpdate{Status: "done", Result: json.RawMessage(`{"ssh_ok":true}`)}, now.Add(4*time.Minute))
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now.Add(5*time.Minute)); !errors.As(err, &conflict) || conflict.InProgress {
		t.Fatalf("enfriamiento tras el reinicio que sí corrió: %v", err)
	}

	// Un done directo sobre un cancelado (el running se perdió) también cuenta.
	t1 := now.Add(time.Hour)
	o, _, err := st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, t1)
	if err != nil {
		t.Fatal(err)
	}
	st.CancelPhoneOrders(ctx, "hap-oficina", CancelRequest{OrderIDs: []string{o.OrderID}}, t1)
	if cur, ign, _ := st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, PhoneStateUpdate{Status: "failed", Error: "timeout-back",
		Result: json.RawMessage(`{"ssh_ok":true}`)}, t1.Add(6*time.Minute)); cur != "failed" || ign {
		t.Fatalf("failed sobre cancelado: %v %v", cur, ign)
	}
	if _, _, err := st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, t1.Add(7*time.Minute)); !errors.As(err, &conflict) {
		t.Fatalf("enfriamiento: %v", err)
	}
	// Un final del propio celular no se reabre.
	if _, ign, _ := st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, PhoneStateUpdate{Status: "running"}, t1.Add(8*time.Minute)); !ign {
		t.Fatal("running sobre failed del celular")
	}
	// Una prueba de velocidad cancelada sigue sin reabrirse.
	sp, _, _ := st.CreatePhoneOrders(ctx, "hap-oficina", OrderRequest{Target: "A"}, t1)
	st.CancelPhoneOrders(ctx, "hap-oficina", CancelRequest{OrderIDs: []string{sp[0].OrderID}}, t1)
	if _, ign, _ := st.SetPhoneOrderState(ctx, "hap-oficina", sp[0].OrderID, PhoneStateUpdate{Status: "running"}, t1); !ign {
		t.Fatal("prueba cancelada reabierta")
	}
}

// El enfriamiento corre desde que el celular se llevó la orden, no desde que
// se pidió (una orden que esperó en cola 14 min no deja reiniciar de nuevo 1
// min después de ejecutarse); y interrupted sin empezar no enfría.
func TestRebootCooldownBase(t *testing.T) {
	st := rebootStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	o, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.AckPhoneOrders(ctx, "hap-oficina", []string{o.OrderID}, now.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, PhoneStateUpdate{Status: "running"}, now.Add(14*time.Minute))
	st.SetPhoneOrderState(ctx, "hap-oficina", o.OrderID, PhoneStateUpdate{Status: "done", Result: json.RawMessage(`{"ssh_ok":true}`)}, now.Add(18*time.Minute))
	var conflict RebootConflictError
	if _, _, err := st.CreateRebootOrder(ctx, "router-A", RebootRequest{}, now.Add(20*time.Minute)); !errors.As(err, &conflict) ||
		conflict.CooldownUntil != FormatTS(now.Add(29*time.Minute)) {
		t.Fatalf("enfriamiento desde la entrega: %v %+v", err, conflict)
	}
	infos, _ := st.ProbeRebootInfos(ctx, "hap-oficina", now.Add(20*time.Minute))
	if a := infos["router-A"]; a.CooldownUntil == nil || *a.CooldownUntil != FormatTS(now.Add(29*time.Minute)) {
		t.Fatalf("info: %+v", a)
	}

	t1 := now.Add(time.Hour)
	b, _, _ := st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, t1)
	st.SetPhoneOrderState(ctx, "hap-oficina", b.OrderID, PhoneStateUpdate{Status: "interrupted", Error: "slot-desconocido"}, t1)
	if _, _, err := st.CreateRebootOrder(ctx, "router-B", RebootRequest{}, t1.Add(time.Minute)); err != nil {
		t.Fatalf("tras slot-desconocido: %v", err)
	}
}

func TestParseRouterHealthNormalizesTimes(t *testing.T) {
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	h, _ := parseRouterHealth(json.RawMessage(`{"routers":{"A":{"internet_ok":false,"checked_at":"<img src=x onerror=alert(1)>","down_since":"2026-09-29T15:50:00-05:00"}}}`), "A", 0, now)
	if h == nil || h.CheckedAt != nil || h.DownSince == nil || *h.DownSince != "2026-09-29T20:50:00Z" {
		t.Fatalf("%+v", h)
	}
}
