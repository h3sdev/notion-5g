package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sondas: un equipo aparte (hoy un MikroTik hAP) con un puerto por cada router
// bajo prueba y una tabla de ruteo por puerto. Mide por la tabla del equipo
// que pide cada comando, así que la ruta de salida está garantizada por
// construcción. Igual que el agente del router, no recibe conexiones: hace
// polling de la cola de comandos con su probe_id.
//
// Es híbrido en dos sentidos:
//   - la misma cola sirve al agente del router (runner "agent", o NULL en los
//     comandos viejos) y a la sonda (runner "probe"), y ninguno toma los del otro;
//   - los ciclos se piden a mano (POST /probes/{id}/cycle) o los encola solo el
//     backend cada interval_s (EnqueueDueProbeCycles).

const (
	RunnerAgent = "agent"
	RunnerProbe = "probe"
	// RunnerPhone: un celular conectado por cable a la sonda, que elige el
	// router cambiando una regla del MikroTik (ver phoneprobe.go). En
	// probes.runner marca quién mide; en commands.runner, una orden de celular.
	RunnerPhone = "phone"

	// probeOnlineThreshold: la sonda consulta la cola cada ~30-60 s; pasado
	// esto se la da por apagada y el planificador deja de encolarle ciclos
	// (si no, con el hAP apagado se acumularían pruebas viejas).
	probeOnlineThreshold = 3 * time.Minute
)

func (s *Store) migrateProbes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS probes (
	probe_id      TEXT PRIMARY KEY,
	label         TEXT,
	interval_s    INTEGER NOT NULL DEFAULT 0,  -- 0 = sin ciclo automático
	enabled       INTEGER NOT NULL DEFAULT 1,
	duration_s    INTEGER,                     -- duración de cada prueba del ciclo
	last_seen     TEXT,
	last_cycle_at TEXT,
	updated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS probe_targets (
	probe_id       TEXT NOT NULL,
	device_id      TEXT NOT NULL,
	routing_table  TEXT NOT NULL,
	label          TEXT,
	send_heartbeat INTEGER NOT NULL DEFAULT 0, -- solo para equipos sin agente propio
	enabled        INTEGER NOT NULL DEFAULT 1,
	position       INTEGER NOT NULL DEFAULT 0, -- orden dentro del ciclo
	PRIMARY KEY (probe_id, device_id)
);
CREATE INDEX IF NOT EXISTS idx_probe_targets_device ON probe_targets(device_id);
`)
	if err != nil {
		return err
	}
	// commands ya existe en producción: CREATE TABLE no agrega columnas.
	for _, col := range []string{"runner TEXT", "probe_id TEXT", "routing_table TEXT"} {
		if err := s.addColumnIfMissing(ctx, "commands", col); err != nil {
			return err
		}
	}
	if _, err = s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_cmd_probe_status ON commands(probe_id, status, id)`); err != nil {
		return err
	}
	return s.migratePhoneProbe(ctx)
}

type ProbeTarget struct {
	// Slot: letra del equipo dentro de la sonda (A, B, C...). Vacío en el PUT =
	// se conserva la que ya tenía ese device_id, o se asigna una libre.
	Slot          string `json:"slot"`
	DeviceID      string `json:"device_id"`
	RoutingTable  string `json:"routing_table"`
	Label         string `json:"label,omitempty"`
	SendHeartbeat bool   `json:"send_heartbeat"`
	Enabled       *bool  `json:"enabled,omitempty"` // nil = true
	// ExpectedASN: ASN esperado de la salida de ese router. Ausente en el PUT
	// = se conserva el que tenía; null = se borra.
	ExpectedASN OptASN `json:"expected_asn"`
	// Reboot: solo de salida (GET /probes, sondas de celular, §6.3). Lo
	// agrega la capa HTTP; UpsertProbe lo ignora.
	Reboot *RebootInfo `json:"reboot,omitempty"`
}

func (t ProbeTarget) isEnabled() bool { return t.Enabled == nil || *t.Enabled }

// OptASN es un entero opcional de tres estados para el PUT: la clave no vino
// (Set=false: se conserva lo guardado), vino null (Set, !Valid: se borra) o
// vino con valor. Hace falta porque el selector de intervalo del dashboard
// manda la sonda entera sin expected_asn, y eso no puede borrar los ASN.
type OptASN struct {
	Set   bool
	Valid bool
	Value int
}

func (o *OptASN) UnmarshalJSON(b []byte) error {
	o.Set = true
	if strings.TrimSpace(string(b)) == "null" {
		o.Valid, o.Value = false, 0
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("expected_asn debe ser un entero o null")
	}
	v, err := strconv.Atoi(n.String())
	if err != nil {
		return fmt.Errorf("expected_asn debe ser un entero o null")
	}
	o.Valid, o.Value = true, v
	return nil
}

func (o OptASN) MarshalJSON() ([]byte, error) {
	if !o.Valid {
		return []byte("null"), nil
	}
	return []byte(strconv.Itoa(o.Value)), nil
}

// Ptr devuelve el valor para la base (nil = NULL).
func (o OptASN) Ptr() *int {
	if !o.Valid {
		return nil
	}
	v := o.Value
	return &v
}

type Probe struct {
	ProbeID string `json:"probe_id"`
	Label   string `json:"label,omitempty"`
	// Runner: quién mide. "probe" (script RouterOS; también los NULL viejos) o
	// "phone" (celular por cable). Vacío en el PUT = se conserva el que tenía.
	Runner      string        `json:"runner"`
	IntervalS   int           `json:"interval_s"`
	Enabled     *bool         `json:"enabled,omitempty"` // nil = true
	DurationS   *int          `json:"duration_s,omitempty"`
	LastSeen    string        `json:"last_seen,omitempty"`
	LastCycleAt string        `json:"last_cycle_at,omitempty"`
	Online      bool          `json:"online"`
	Targets     []ProbeTarget `json:"targets"`
	// PhoneStatus: último estado en vivo del celular, si la sonda tiene uno.
	// Lo agrega la capa HTTP (no ListProbes), y no va en GET .../orders.
	PhoneStatus *ProbeStatus `json:"phone_status,omitempty"`
}

var slotRe = regexp.MustCompile(`^[A-Z]$`)

// UpsertProbe crea o reemplaza la configuración de una sonda y su lista de
// equipos. last_seen/last_cycle_at no se tocan: son estado, no configuración.
//
// Claves ausentes se conservan (contrato sonda A/B §1.2): runner vacío deja el
// que tenía la sonda (una nueva queda "probe"); slot vacío y expected_asn
// ausente conservan los que tenía ese device_id. Los valores viejos se leen
// DENTRO de la transacción, antes del DELETE de probe_targets.
func (s *Store) UpsertProbe(ctx context.Context, p Probe) error {
	if p.ProbeID == "" {
		return fmt.Errorf("falta probe_id")
	}
	if p.IntervalS < 0 {
		return fmt.Errorf("interval_s no puede ser negativo")
	}
	if p.IntervalS > 0 && p.IntervalS < 60 {
		return fmt.Errorf("interval_s mínimo 60 (o 0 para apagar el ciclo automático)")
	}
	switch p.Runner {
	case "", RunnerProbe, RunnerPhone:
	default:
		return fmt.Errorf("runner debe ser 'probe' o 'phone'")
	}
	seen := map[string]bool{}
	explicitSlots := map[string]bool{}
	for _, t := range p.Targets {
		if t.DeviceID == "" || t.RoutingTable == "" {
			return fmt.Errorf("cada equipo necesita device_id y routing_table")
		}
		if seen[t.DeviceID] {
			return fmt.Errorf("device_id repetido en la sonda: %s", t.DeviceID)
		}
		seen[t.DeviceID] = true
		if t.Slot != "" {
			if !slotRe.MatchString(t.Slot) {
				return fmt.Errorf("slot inválido %q: debe ser una letra mayúscula (A, B, C...)", t.Slot)
			}
			if explicitSlots[t.Slot] {
				return fmt.Errorf("slot repetido en la sonda: %s", t.Slot)
			}
			explicitSlots[t.Slot] = true
		}
		if t.ExpectedASN.Valid && t.ExpectedASN.Value <= 0 {
			return fmt.Errorf("expected_asn debe ser un entero mayor que 0 o null")
		}
	}
	enabled := p.Enabled == nil || *p.Enabled
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var oldRunner sql.NullString
	existed := true
	if err := tx.QueryRowContext(ctx, `SELECT runner FROM probes WHERE probe_id = ?`, p.ProbeID).Scan(&oldRunner); err != nil {
		if err != sql.ErrNoRows {
			return err
		}
		existed = false
	}
	prevRunner := RunnerProbe
	if oldRunner.String == RunnerPhone {
		prevRunner = RunnerPhone
	}
	runner := p.Runner
	if runner == "" {
		runner = prevRunner
		if !existed {
			runner = RunnerProbe
		}
	}

	type oldTarget struct {
		slot string
		asn  sql.NullInt64
	}
	old := map[string]oldTarget{}
	rows, err := tx.QueryContext(ctx, `SELECT device_id, slot, expected_asn FROM probe_targets WHERE probe_id = ?`, p.ProbeID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dev string
		var slot sql.NullString
		var asn sql.NullInt64
		if err := rows.Scan(&dev, &slot, &asn); err != nil {
			rows.Close()
			return err
		}
		old[dev] = oldTarget{slot.String, asn}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Slots: primero los explícitos, después los que se conservan (si no
	// chocan con uno explícito), y al final la letra de la posición o la
	// primera libre.
	targets := append([]ProbeTarget(nil), p.Targets...)
	used := map[string]bool{}
	for k := range explicitSlots {
		used[k] = true
	}
	for i := range targets {
		if targets[i].Slot != "" {
			continue
		}
		if o, ok := old[targets[i].DeviceID]; ok && slotRe.MatchString(o.slot) && !used[o.slot] {
			targets[i].Slot = o.slot
			used[o.slot] = true
		}
	}
	for i := range targets {
		if targets[i].Slot != "" {
			continue
		}
		slot := freeSlot(i, used)
		if slot == "" {
			return fmt.Errorf("demasiados equipos en la sonda (máximo 26)")
		}
		targets[i].Slot = slot
		used[slot] = true
	}
	for i := range targets {
		if !targets[i].ExpectedASN.Set {
			if o, ok := old[targets[i].DeviceID]; ok && o.asn.Valid && o.asn.Int64 > 0 {
				targets[i].ExpectedASN = OptASN{Set: true, Valid: true, Value: int(o.asn.Int64)}
			}
		}
	}
	// En una sonda de celular la atribución va de tabla a equipo: dos equipos
	// con la misma tabla la harían ambigua, y main es la de respaldo.
	if runner == RunnerPhone {
		tables := map[string]bool{}
		for _, t := range targets {
			if t.RoutingTable == "main" {
				return fmt.Errorf("routing_table 'main' es la de respaldo, no la de un router")
			}
			if tables[t.RoutingTable] {
				return fmt.Errorf("routing_table repetida en la sonda: %s", t.RoutingTable)
			}
			tables[t.RoutingTable] = true
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO probes (probe_id, label, runner, interval_s, enabled, duration_s, updated_at) VALUES (?,?,?,?,?,?,?)
ON CONFLICT(probe_id) DO UPDATE SET label=excluded.label, runner=excluded.runner, interval_s=excluded.interval_s,
	enabled=excluded.enabled, duration_s=excluded.duration_s, updated_at=excluded.updated_at`,
		p.ProbeID, nullStr(p.Label), runner, p.IntervalS, boolToInt(&enabled), p.DurationS, now); err != nil {
		return fmt.Errorf("guardar sonda: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM probe_targets WHERE probe_id = ?`, p.ProbeID); err != nil {
		return err
	}
	for i, t := range targets {
		en := t.isEnabled()
		if _, err := tx.ExecContext(ctx, `
INSERT INTO probe_targets (probe_id, device_id, routing_table, label, send_heartbeat, enabled, position, slot, expected_asn)
VALUES (?,?,?,?,?,?,?,?,?)`, p.ProbeID, t.DeviceID, t.RoutingTable, nullStr(t.Label),
			boolToInt(&t.SendHeartbeat), boolToInt(&en), i, t.Slot, t.ExpectedASN.Ptr()); err != nil {
			return fmt.Errorf("guardar equipo de la sonda: %w", err)
		}
	}
	// De celular a script: las órdenes abiertas del celular ya no las ejecuta
	// nadie, se cancelan.
	if prevRunner == RunnerPhone && runner != RunnerPhone {
		if _, err := tx.ExecContext(ctx, `
UPDATE commands SET status='cancelled', closed_by='dashboard', error='cambio-de-runner', completed_at=?, updated_at=?
WHERE runner='phone' AND probe_id=? AND status IN ('pending','delivered','running')`, now, now, p.ProbeID); err != nil {
			return fmt.Errorf("cancelar órdenes del celular: %w", err)
		}
	}
	return tx.Commit()
}

// freeSlot: la letra de la posición (0→A, 1→B...) si está libre; si no, la
// primera libre. "" si no queda ninguna.
func freeSlot(pos int, used map[string]bool) string {
	if pos >= 0 && pos < 26 {
		if l := string(rune('A' + pos)); !used[l] {
			return l
		}
	}
	for c := 'A'; c <= 'Z'; c++ {
		if !used[string(c)] {
			return string(c)
		}
	}
	return ""
}

// ListProbes devuelve todas las sondas (o solo probeID si no es vacío) con sus equipos.
func (s *Store) ListProbes(ctx context.Context, probeID string) ([]Probe, error) {
	q := `SELECT probe_id, label, runner, interval_s, enabled, duration_s, last_seen, last_cycle_at FROM probes`
	var args []any
	if probeID != "" {
		q += ` WHERE probe_id = ?`
		args = append(args, probeID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY probe_id`, args...)
	if err != nil {
		return nil, err
	}
	out := []Probe{}
	for rows.Next() {
		var p Probe
		var label, runner, lastSeen, lastCycle sql.NullString
		var enabled int64
		var dur sql.NullInt64
		if err := rows.Scan(&p.ProbeID, &label, &runner, &p.IntervalS, &enabled, &dur, &lastSeen, &lastCycle); err != nil {
			rows.Close()
			return nil, err
		}
		en := enabled != 0
		p.Enabled = &en
		p.Label = label.String
		p.Runner = RunnerProbe
		if runner.String == RunnerPhone {
			p.Runner = RunnerPhone
		}
		if dur.Valid {
			v := int(dur.Int64)
			p.DurationS = &v
		}
		p.LastSeen, p.LastCycleAt = lastSeen.String, lastCycle.String
		if t, err := time.Parse(time.RFC3339, p.LastSeen); err == nil {
			p.Online = time.Since(t) <= probeOnlineThreshold
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		t, err := s.probeTargets(ctx, out[i].ProbeID, false)
		if err != nil {
			return nil, err
		}
		out[i].Targets = t
	}
	return out, nil
}

func (s *Store) probeTargets(ctx context.Context, probeID string, onlyEnabled bool) ([]ProbeTarget, error) {
	q := `SELECT device_id, routing_table, label, send_heartbeat, enabled, slot, expected_asn FROM probe_targets WHERE probe_id = ?`
	if onlyEnabled {
		q += ` AND enabled = 1`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY position, device_id`, probeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProbeTarget{}
	for rows.Next() {
		var t ProbeTarget
		var label, slot sql.NullString
		var hb, en int64
		var asn sql.NullInt64
		if err := rows.Scan(&t.DeviceID, &t.RoutingTable, &label, &hb, &en, &slot, &asn); err != nil {
			return nil, err
		}
		t.Label = label.String
		t.Slot = slot.String
		if asn.Valid && asn.Int64 > 0 {
			t.ExpectedASN = OptASN{Set: true, Valid: true, Value: int(asn.Int64)}
		}
		t.SendHeartbeat = hb != 0
		e := en != 0
		t.Enabled = &e
		out = append(out, t)
	}
	return out, rows.Err()
}

// TouchProbe anota que la sonda está viva (cada vez que consulta la cola o su config).
func (s *Store) TouchProbe(ctx context.Context, probeID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE probes SET last_seen = ? WHERE probe_id = ?`,
		time.Now().UTC().Format(time.RFC3339), probeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sonda %q no configurada (PUT /api/v1/probes/%s)", probeID, probeID)
	}
	return nil
}

// resolveProbeTarget elige por qué sonda y tabla de ruteo se mide deviceID.
// probeID vacío = la única sonda activa que tenga ese equipo.
func (s *Store) resolveProbeTarget(ctx context.Context, deviceID, probeID string) (string, string, error) {
	q := `SELECT t.probe_id, t.routing_table FROM probe_targets t JOIN probes p ON p.probe_id = t.probe_id
WHERE t.device_id = ? AND t.enabled = 1 AND p.enabled = 1`
	args := []any{deviceID}
	if probeID != "" {
		q += ` AND t.probe_id = ?`
		args = append(args, probeID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	var found [][2]string
	for rows.Next() {
		var pid, table string
		if err := rows.Scan(&pid, &table); err != nil {
			return "", "", err
		}
		found = append(found, [2]string{pid, table})
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("el equipo %s no está conectado a ninguna sonda activa", deviceID)
	case 1:
		return found[0][0], found[0][1], nil
	default:
		return "", "", fmt.Errorf("el equipo %s está en varias sondas: indicá probe_id", deviceID)
	}
}

// ClaimNextProbeCommand es ClaimNextCommand para una sonda: toma el comando
// runner="probe" más viejo de esa sonda (de cualquiera de sus equipos). Como
// los ciclos se encolan en orden, así es como alternan los equipos.
func (s *Store) ClaimNextProbeCommand(ctx context.Context, probeID string) (*Command, error) {
	staleBefore := time.Now().UTC().Add(-staleClaimAfter).Format(time.RFC3339)
	return s.claimWhere(ctx, `runner = 'probe' AND probe_id = ? AND (status = 'pending' OR (status = 'claimed' AND claimed_at < ?))`,
		probeID, staleBefore)
}

// EnqueueProbeCycle encola una prueba por cada equipo activo de la sonda, en
// orden. Se salta los equipos que ya tienen un comando de sonda abierto: pedir
// dos ciclos seguidos no duplica pruebas.
func (s *Store) EnqueueProbeCycle(ctx context.Context, probeID, requestedBy string) ([]int64, error) {
	ids, _, err := s.EnqueueProbeCycleOrders(ctx, probeID, requestedBy)
	return ids, err
}

// EnqueueProbeCycleOrders es EnqueueProbeCycle que además devuelve los
// order_id: en una sonda de celular (runner "phone") el ciclo encola órdenes
// de celular (ver enqueuePhoneCycle) y los ids numéricos son los de esas
// mismas órdenes. En una sonda de script, order_ids va vacío.
func (s *Store) EnqueueProbeCycleOrders(ctx context.Context, probeID, requestedBy string) ([]int64, []string, error) {
	var enabled int64
	var dur sql.NullInt64
	var runner sql.NullString
	var intervalS int
	if err := s.db.QueryRowContext(ctx, `SELECT enabled, duration_s, runner, interval_s FROM probes WHERE probe_id = ?`, probeID).
		Scan(&enabled, &dur, &runner, &intervalS); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, fmt.Errorf("sonda %q no configurada", probeID)
		}
		return nil, nil, err
	}
	if enabled == 0 {
		return nil, nil, fmt.Errorf("la sonda %s está desactivada", probeID)
	}
	if runner.String == RunnerPhone {
		return s.enqueuePhoneCycle(ctx, probeID, requestedBy, dur, intervalS, time.Now())
	}
	ids, err := s.enqueueScriptCycle(ctx, probeID, requestedBy, dur)
	return ids, []string{}, err
}

func (s *Store) enqueueScriptCycle(ctx context.Context, probeID, requestedBy string, dur sql.NullInt64) ([]int64, error) {
	targets, err := s.probeTargets(ctx, probeID, true)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("la sonda %s no tiene equipos activos", probeID)
	}
	staleBefore := time.Now().UTC().Add(-staleClaimAfter).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	var durationS any
	if dur.Valid {
		durationS = dur.Int64
	}
	ids := []int64{}
	for _, t := range targets {
		var open int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commands WHERE runner = 'probe' AND probe_id = ? AND device_id = ?
AND (status = 'pending' OR (status = 'claimed' AND claimed_at >= ?))`, probeID, t.DeviceID, staleBefore).Scan(&open); err != nil {
			return nil, err
		}
		if open > 0 {
			continue
		}
		res, err := s.db.ExecContext(ctx, `
INSERT INTO commands (created_at, device_id, type, duration_s, requested_by, status, runner, probe_id, routing_table)
VALUES (?,?,?,?,?, 'pending', 'probe', ?, ?)`, now, t.DeviceID, "run_speedtest", durationS, nullStr(requestedBy), probeID, t.RoutingTable)
		if err != nil {
			return nil, fmt.Errorf("encolar prueba de %s: %w", t.DeviceID, err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE probes SET last_cycle_at = ? WHERE probe_id = ?`, now, probeID); err != nil {
		return nil, err
	}
	return ids, nil
}

// EnqueueDueProbeCycles lo corre el planificador del backend: encola un ciclo
// en cada sonda activa con interval_s > 0 cuyo último ciclo sea más viejo que
// interval_s, que esté en línea y que no tenga comandos abiertos (si la
// sonda todavía está midiendo el ciclo anterior, se espera).
func (s *Store) EnqueueDueProbeCycles(ctx context.Context, now time.Time) (map[string][]int64, error) {
	probes, err := s.ListProbes(ctx, "")
	if err != nil {
		return nil, err
	}
	staleBefore := now.UTC().Add(-staleClaimAfter).Format(time.RFC3339)
	out := map[string][]int64{}
	for _, p := range probes {
		if p.IntervalS <= 0 || (p.Enabled != nil && !*p.Enabled) {
			continue
		}
		seen, err := time.Parse(time.RFC3339, p.LastSeen)
		if err != nil || now.Sub(seen) > probeOnlineThreshold {
			continue
		}
		if last, err := time.Parse(time.RFC3339, p.LastCycleAt); err == nil && now.Sub(last) < time.Duration(p.IntervalS)*time.Second {
			continue
		}
		var open int
		if p.Runner == RunnerPhone {
			n, err := s.countOpenPhoneOrders(ctx, p.ProbeID, "", now)
			if err != nil {
				return nil, err
			}
			open = n
		} else if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commands WHERE runner = 'probe' AND probe_id = ?
AND (status = 'pending' OR (status = 'claimed' AND claimed_at >= ?))`, p.ProbeID, staleBefore).Scan(&open); err != nil {
			return nil, err
		}
		if open > 0 {
			continue
		}
		ids, err := s.EnqueueProbeCycle(ctx, p.ProbeID, "scheduler")
		if err != nil {
			return out, fmt.Errorf("ciclo automático de %s: %w", p.ProbeID, err)
		}
		out[p.ProbeID] = ids
	}
	return out, nil
}

// CommandRoute: a qué equipo pertenece un comando y quién lo ejecuta.
type CommandRoute struct {
	DeviceID     string
	Runner       string // "agent" (incluye los viejos sin runner) | "probe" | "phone"
	ProbeID      string
	RoutingTable string
}

func (s *Store) CommandRouteOf(ctx context.Context, id int64) (CommandRoute, bool) {
	var r CommandRoute
	var runner, probeID, table sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT device_id, runner, probe_id, routing_table FROM commands WHERE id = ?`, id).
		Scan(&r.DeviceID, &runner, &probeID, &table); err != nil {
		return CommandRoute{}, false
	}
	r.Runner = RunnerAgent
	switch runner.String {
	case RunnerProbe, RunnerPhone:
		r.Runner = runner.String
	}
	r.ProbeID, r.RoutingTable = probeID.String, table.String
	return r, r.DeviceID != ""
}
