package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Sonda A/B con celular por cable ("forma A", docs/CONTRATO-SONDA-AB.md).
//
// Un celular conectado por USB-Ethernet a la sonda (MikroTik) elige por qué
// router sale cambiando una regla de ruteo antes de cada prueba. El backend le
// guarda órdenes (en la misma tabla commands, con runner='phone'), recibe los
// resultados de forma idempotente (result_id) y decide a qué router se
// atribuye cada uno. El celular trae sus órdenes con GET .../orders: igual
// que el agente del router, nunca recibe conexiones.
//
// Horas: todo lo que se escribe acá va en RFC 3339 UTC sin fracciones
// (FormatTS). Por eso las comparaciones en SQL entre columnas de hora escritas
// por este archivo (not_after, updated_at, execute_at) equivalen a comparar
// time.Time: el formato es de ancho fijo y ordena igual que el tiempo. Las
// horas que manda el celular se parsean y normalizan antes de guardarlas.

var (
	// ErrProbeNotFound: la sonda de la ruta no existe (404 "sonda no configurada").
	ErrProbeNotFound = errors.New("sonda no configurada")
	// ErrOrderIDInUse: el order_id lo tiene otra orden (otra sonda u otro lote): 409.
	ErrOrderIDInUse = errors.New("order_id en uso por otra orden")
	// ErrOrderNotFound: la orden no existe en esa sonda (404 "orden desconocida").
	ErrOrderNotFound = errors.New("orden desconocida")
	// ErrCommandClosed: CompleteCommand sobre un comando ya cerrado (se ignora).
	ErrCommandClosed = errors.New("el comando ya estaba cerrado")
)

// BadRequestError: error de validación con el mensaje exacto para el cliente (400).
type BadRequestError struct{ Msg string }

func (e BadRequestError) Error() string { return e.Msg }

func badReq(format string, a ...any) error { return BadRequestError{Msg: fmt.Sprintf(format, a...)} }

// closedStatuses: estados finales de un comando u orden.
var closedStatuses = map[string]bool{"done": true, "failed": true, "expired": true, "interrupted": true, "cancelled": true}

// openPhoneStatuses: una orden de celular que todavía puede ejecutarse.
var openPhoneStatuses = map[string]bool{"pending": true, "delivered": true, "running": true}

const (
	// phoneRunningStale: una orden en running sin cambios (updated_at, hora del
	// servidor) por más de esto se da por interrumpida en el barrido.
	phoneRunningStale = 30 * time.Minute
	// defaultOrderWindow: not_after por defecto = execute_at + esto.
	defaultOrderWindow = 30 * time.Minute
	// phoneStatusOnline: el celular manda estado cada 15 s.
	phoneStatusOnline = 60 * time.Second

	maxOpenOrdersListed   = 200
	maxClosedOrdersListed = 500
)

// ValidClientID es el criterio para order_id/result_id generados por el
// cliente (el mismo de validTestID en internal/api/netclass.go).
var clientIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

func ValidClientID(s string) bool { return clientIDRe.MatchString(s) }

// seqSuffixRe: sufijo reservado para las órdenes de una secuencia (<id>-1…<id>-48).
var seqSuffixRe = regexp.MustCompile(`-[0-9]{1,2}$`)

// ParseTS parsea una hora RFC 3339 (acepta fracciones y desfases) y la deja en
// UTC sin fracciones.
func ParseTS(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC().Truncate(time.Second), nil
}

// FormatTS: RFC 3339 en UTC con Z y sin fracciones.
func FormatTS(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// NewUUID devuelve un UUID v4 en minúsculas (crypto/rand).
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand no falla en Linux
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ------------------------------------------------------------------ esquema

func (s *Store) migratePhoneProbe(ctx context.Context) error {
	cols := map[string][]string{
		"probes":        {"runner TEXT"},
		"probe_targets": {"slot TEXT", "expected_asn INTEGER"},
		"commands": {
			"order_id TEXT", "batch_id TEXT", "target TEXT", "slot TEXT", "allow_fallback INTEGER",
			"preferred_device TEXT", "fallback_device TEXT", "execute_at TEXT", "not_after TEXT",
			"selection_reason TEXT", "delivered_at TEXT", "started_at TEXT", "result_id TEXT",
			"closed_by TEXT", "updated_at TEXT",
			// Reinicio remoto (§6): paso en curso, resultado JSON y motivo.
			"step TEXT", "result_json TEXT", "reboot_reason TEXT",
		},
		"measurements": {
			"result_id TEXT", "order_id TEXT", "probe_id TEXT", "measured_by TEXT", "routing_table TEXT",
			"slot TEXT", "selection_reason TEXT", "test_started_at TEXT", "test_finished_at TEXT",
			"test_status TEXT", "loss_pct REAL", "gps_ts TEXT", "gps_age_s REAL", "location_status TEXT",
			"location_source TEXT", "executed_offline INTEGER", "net_path TEXT", "target_host TEXT", "egress_ip TEXT",
		},
	}
	for _, table := range []string{"probes", "probe_targets", "commands", "measurements"} {
		for _, col := range cols[table] {
			if err := s.addColumnIfMissing(ctx, table, col); err != nil {
				return err
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS idx_cmd_order_id ON commands(order_id);
CREATE INDEX IF NOT EXISTS idx_cmd_batch ON commands(batch_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_meas_result_id ON measurements(result_id);
CREATE INDEX IF NOT EXISTS idx_meas_probe ON measurements(probe_id, id);
CREATE TABLE IF NOT EXISTS probe_status (
	probe_id      TEXT PRIMARY KEY,
	received_at   TEXT NOT NULL,
	measured_by   TEXT,
	phase         TEXT,
	current_table TEXT,
	raw           TEXT NOT NULL
);`); err != nil {
		return fmt.Errorf("migrar sonda de celular: %w", err)
	}
	return s.backfillProbeSlots(ctx)
}

// backfillProbeSlots le da letra a los equipos de sondas creadas antes de que
// existiera slot (misma regla que UpsertProbe: la de su posición o la primera
// libre). Solo toca filas con slot NULL: correrlo de nuevo no cambia nada.
func (s *Store) backfillProbeSlots(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT probe_id, device_id, slot FROM probe_targets ORDER BY probe_id, position, device_id`)
	if err != nil {
		return err
	}
	type row struct{ probe, dev, slot string }
	var all []row
	for rows.Next() {
		var r row
		var slot sql.NullString
		if err := rows.Scan(&r.probe, &r.dev, &slot); err != nil {
			rows.Close()
			return err
		}
		r.slot = slot.String
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	used := map[string]map[string]bool{}
	for _, r := range all {
		if used[r.probe] == nil {
			used[r.probe] = map[string]bool{}
		}
		if r.slot != "" {
			used[r.probe][r.slot] = true
		}
	}
	pos := map[string]int{}
	for _, r := range all {
		i := pos[r.probe]
		pos[r.probe]++
		if r.slot != "" {
			continue
		}
		slot := freeSlot(i, used[r.probe])
		if slot == "" {
			continue
		}
		used[r.probe][slot] = true
		if _, err := s.db.ExecContext(ctx, `UPDATE probe_targets SET slot = ? WHERE probe_id = ? AND device_id = ? AND slot IS NULL`,
			slot, r.probe, r.dev); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ sonda

// ProbeInfo: lo que hace falta de una sonda para validar órdenes.
type ProbeInfo struct {
	ProbeID   string
	Runner    string
	Enabled   bool
	DurationS *int
	IntervalS int
}

// ProbeInfoOf devuelve ErrProbeNotFound si la sonda no existe.
func (s *Store) ProbeInfoOf(ctx context.Context, probeID string) (ProbeInfo, error) {
	return probeInfoOf(ctx, s.db, probeID)
}

func probeInfoOf(ctx context.Context, q queryer, probeID string) (ProbeInfo, error) {
	var p ProbeInfo
	var runner sql.NullString
	var enabled int64
	var dur sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT probe_id, runner, enabled, duration_s, interval_s FROM probes WHERE probe_id = ?`, probeID).
		Scan(&p.ProbeID, &runner, &enabled, &dur, &p.IntervalS)
	if err == sql.ErrNoRows {
		return p, ErrProbeNotFound
	}
	if err != nil {
		return p, err
	}
	p.Runner = RunnerProbe
	if runner.String == RunnerPhone {
		p.Runner = RunnerPhone
	}
	p.Enabled = enabled != 0
	if dur.Valid {
		v := int(dur.Int64)
		p.DurationS = &v
	}
	return p, nil
}

// probeRunner: runner de la sonda y si existe.
func (s *Store) probeRunner(ctx context.Context, probeID string) (string, bool, error) {
	p, err := s.ProbeInfoOf(ctx, probeID)
	if errors.Is(err, ErrProbeNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return p.Runner, true, nil
}

// ProbeTargetsAll: todos los equipos de la sonda, activos o no (la atribución
// de un resultado no depende de que el equipo siga activo).
func (s *Store) ProbeTargetsAll(ctx context.Context, probeID string) ([]ProbeTarget, error) {
	return s.probeTargets(ctx, probeID, false)
}

// activeSlots: equipos activos de la sonda con slot, ordenados por slot.
func activeSlots(ctx context.Context, q queryer, probeID string) ([]ProbeTarget, error) {
	rows, err := q.QueryContext(ctx, `SELECT slot, device_id, routing_table FROM probe_targets
WHERE probe_id = ? AND enabled = 1 AND slot IS NOT NULL AND slot <> '' ORDER BY slot`, probeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProbeTarget
	for rows.Next() {
		var t ProbeTarget
		if err := rows.Scan(&t.Slot, &t.DeviceID, &t.RoutingTable); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// otherSlot: el equipo de respaldo de un slot = el siguiente slot activo (en
// orden, dando la vuelta). Con dos equipos, "el otro".
func otherSlot(slots []ProbeTarget, slot string) *ProbeTarget {
	for i, t := range slots {
		if t.Slot == slot {
			for j := 1; j < len(slots); j++ {
				o := slots[(i+j)%len(slots)]
				if o.Slot != slot {
					return &o
				}
			}
			return nil
		}
	}
	return nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ------------------------------------------------------------------ órdenes

// PhoneOrder es una orden de celular tal como la devuelven los endpoints
// (contrato §1.3). Las claves sin valor van SIEMPRE como null.
type PhoneOrder struct {
	ID              int64   `json:"id"`
	OrderID         string  `json:"order_id"`
	BatchID         *string `json:"batch_id"`
	ProbeID         string  `json:"probe_id"`
	Runner          string  `json:"runner"`
	Type            string  `json:"type"`
	Target          *string `json:"target"`
	Slot            *string `json:"slot"`
	DeviceID        string  `json:"device_id"`
	RoutingTable    *string `json:"routing_table"`
	AllowFallback   bool    `json:"allow_fallback"`
	PreferredDevice *string `json:"preferred_device"`
	FallbackDevice  *string `json:"fallback_device"`
	DurationS       *int    `json:"duration_s"`
	ExecuteAt       *string `json:"execute_at"`
	NotAfter        *string `json:"not_after"`
	SelectionReason *string `json:"selection_reason"`
	RequestedBy     *string `json:"requested_by"`
	Status          string  `json:"status"`
	CreatedAt       string  `json:"created_at"`
	DeliveredAt     *string `json:"delivered_at"`
	StartedAt       *string `json:"started_at"`
	CompletedAt     *string `json:"completed_at"`
	UpdatedAt       *string `json:"updated_at"`
	ClosedBy        *string `json:"closed_by"`
	Error           *string `json:"error"`
	ResultID        *string `json:"result_id"`
	MeasurementID   *int64  `json:"measurement_id"`
	// Solo órdenes reboot_router (§6); null en las pruebas de velocidad.
	// RebootReason: manual | recommended. Step: paso que informó el celular
	// (p. ej. ssh, waiting-back). Result: el JSON con que el celular la cerró.
	RebootReason *string         `json:"reboot_reason"`
	Step         *string         `json:"step"`
	Result       json.RawMessage `json:"result"`
}

const phoneOrderCols = `id, order_id, batch_id, probe_id, type, target, slot, device_id, routing_table, allow_fallback,
	preferred_device, fallback_device, duration_s, execute_at, not_after, selection_reason, requested_by, status,
	created_at, delivered_at, started_at, completed_at, updated_at, closed_by, error, result_id, measurement_id,
	reboot_reason, step, result_json`

type rowScanner interface{ Scan(dest ...any) error }

func strPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func scanPhoneOrder(r rowScanner) (PhoneOrder, error) {
	var o PhoneOrder
	var orderID, batchID, probeID, target, slot, table, preferred, fallback, execAt, notAfter, reason, requestedBy sql.NullString
	var delivered, started, completed, updated, closedBy, errMsg, resultID sql.NullString
	var rebootReason, step, resultJSON sql.NullString
	var allow, dur, mid sql.NullInt64
	if err := r.Scan(&o.ID, &orderID, &batchID, &probeID, &o.Type, &target, &slot, &o.DeviceID, &table, &allow,
		&preferred, &fallback, &dur, &execAt, &notAfter, &reason, &requestedBy, &o.Status,
		&o.CreatedAt, &delivered, &started, &completed, &updated, &closedBy, &errMsg, &resultID, &mid,
		&rebootReason, &step, &resultJSON); err != nil {
		return o, err
	}
	o.RebootReason, o.Step = strPtr(rebootReason), strPtr(step)
	if resultJSON.Valid && resultJSON.String != "" {
		o.Result = json.RawMessage(resultJSON.String)
	}
	o.OrderID, o.ProbeID, o.Runner = orderID.String, probeID.String, RunnerPhone
	o.BatchID, o.Target, o.Slot, o.RoutingTable = strPtr(batchID), strPtr(target), strPtr(slot), strPtr(table)
	o.AllowFallback = allow.Valid && allow.Int64 != 0
	o.PreferredDevice, o.FallbackDevice = strPtr(preferred), strPtr(fallback)
	if dur.Valid {
		v := int(dur.Int64)
		o.DurationS = &v
	}
	o.ExecuteAt, o.NotAfter, o.SelectionReason, o.RequestedBy = strPtr(execAt), strPtr(notAfter), strPtr(reason), strPtr(requestedBy)
	o.DeliveredAt, o.StartedAt, o.CompletedAt, o.UpdatedAt = strPtr(delivered), strPtr(started), strPtr(completed), strPtr(updated)
	o.ClosedBy, o.Error, o.ResultID = strPtr(closedBy), strPtr(errMsg), strPtr(resultID)
	if mid.Valid {
		v := mid.Int64
		o.MeasurementID = &v
	}
	return o, nil
}

func queryPhoneOrders(ctx context.Context, q queryer, where string, args ...any) ([]PhoneOrder, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+phoneOrderCols+` FROM commands WHERE runner = 'phone' AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PhoneOrder{}
	for rows.Next() {
		o, err := scanPhoneOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// orderSpec: una orden lista para insertar. typ vacío = run_speedtest; en
// reboot_router, durationS 0 y reason vacío se guardan NULL.
type orderSpec struct {
	typ, rebootReason                               string
	orderID, batchID, target, slot, deviceID, table string
	allowFallback                                   bool
	preferred, fallback                             string
	durationS                                       int
	executeAt, notAfter                             time.Time
	reason, requestedBy                             string
}

func insertPhoneOrder(ctx context.Context, q queryer, probeID string, sp orderSpec, now time.Time) (int64, error) {
	ts := FormatTS(now)
	allow := 0
	if sp.allowFallback {
		allow = 1
	}
	typ := sp.typ
	if typ == "" {
		typ = "run_speedtest"
	}
	var dur any
	if sp.durationS > 0 {
		dur = sp.durationS
	}
	res, err := q.ExecContext(ctx, `
INSERT INTO commands (created_at, device_id, type, duration_s, requested_by, status, runner, probe_id, routing_table,
	order_id, batch_id, target, slot, allow_fallback, preferred_device, fallback_device, execute_at, not_after,
	selection_reason, updated_at, reboot_reason)
VALUES (?,?,?,?,?, 'pending', 'phone', ?,?, ?,?,?,?,?,?,?,?,?, ?,?,?)`,
		ts, sp.deviceID, typ, dur, nullStr(sp.requestedBy), probeID, nullStr(sp.table),
		sp.orderID, sp.batchID, sp.target, nullStr(sp.slot), allow, nullStr(sp.preferred), nullStr(sp.fallback),
		FormatTS(sp.executeAt), FormatTS(sp.notAfter), nullStr(sp.reason), ts, nullStr(sp.rebootReason))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// OrderRequest: cuerpo de POST /probes/{id}/orders.
type OrderRequest struct {
	OrderID       string  `json:"order_id"`
	Target        string  `json:"target"`
	AllowFallback bool    `json:"allow_fallback"`
	DurationS     *int    `json:"duration_s"`
	ExecuteAt     *string `json:"execute_at"`
	NotAfter      *string `json:"not_after"`
	RequestedBy   string  `json:"requested_by"`
	Count         int     `json:"count"`
	SpacingS      int     `json:"spacing_s"`
	First         string  `json:"first"`
}

// CreatePhoneOrders crea una orden suelta (target = slot o "next") o una
// secuencia alternada (target "sequence"). Idempotente por batch_id = el
// order_id del cuerpo: si ya hay órdenes de ese lote en la sonda, no crea
// nada y las devuelve con existing=true (aunque el cuerpo nuevo sea otro).
// Chequeo e INSERT van en una sola transacción.
func (s *Store) CreatePhoneOrders(ctx context.Context, probeID string, req OrderRequest, now time.Time) ([]PhoneOrder, bool, error) {
	now = now.UTC().Truncate(time.Second)
	if _, err := s.ProbeInfoOf(ctx, probeID); err != nil {
		return nil, false, err
	}
	batchID := req.OrderID
	if batchID == "" {
		batchID = NewUUID()
	} else {
		if !ValidClientID(batchID) {
			return nil, false, badReq("order_id inválido (se espera un UUID: letras, números y '-', 8 a 64)")
		}
		if seqSuffixRe.MatchString(batchID) {
			return nil, false, badReq("order_id reservado para secuencias")
		}
		if req.Target == "sequence" && len(batchID) > 60 {
			return nil, false, badReq("order_id de una secuencia: máximo 60 caracteres")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if existing, found, err := existingBatch(ctx, tx, probeID, batchID); err != nil {
		return nil, false, err
	} else if found {
		return existing, true, nil
	}

	p, err := probeInfoOf(ctx, tx, probeID)
	if err != nil {
		return nil, false, err
	}
	if p.Runner != RunnerPhone {
		return nil, false, badReq("la sonda no la ejecuta un celular")
	}
	if !p.Enabled {
		return nil, false, badReq("la sonda está desactivada")
	}
	slots, err := activeSlots(ctx, tx, probeID)
	if err != nil {
		return nil, false, err
	}
	if len(slots) == 0 {
		return nil, false, badReq("la sonda no tiene equipos activos")
	}
	specs, err := buildOrderSpecs(req, batchID, probeID, p, slots, now)
	if err != nil {
		return nil, false, err
	}
	for _, sp := range specs {
		if _, err := insertPhoneOrder(ctx, tx, probeID, sp, now); err != nil {
			if isUniqueViolation(err) {
				tx.Rollback()
				return s.resolveOrderIDCollision(ctx, probeID, batchID, specs)
			}
			return nil, false, fmt.Errorf("crear orden: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	orders, err := queryPhoneOrders(ctx, s.db, `batch_id = ? ORDER BY id`, batchID)
	return orders, false, err
}

// existingBatch: si alguna orden tiene batch_id u order_id = batchID. Si todas
// son de este lote y de esta sonda, las devuelve (found=true); si alguna es de
// otra sonda u otro lote, ErrOrderIDInUse.
func existingBatch(ctx context.Context, q queryer, probeID, batchID string) ([]PhoneOrder, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT probe_id, batch_id FROM commands WHERE batch_id = ? OR order_id = ?`, batchID, batchID)
	if err != nil {
		return nil, false, err
	}
	n := 0
	foreign := false
	for rows.Next() {
		var pid, bid sql.NullString
		if err := rows.Scan(&pid, &bid); err != nil {
			rows.Close()
			return nil, false, err
		}
		n++
		if pid.String != probeID || bid.String != batchID {
			foreign = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if n == 0 {
		return nil, false, nil
	}
	if foreign {
		return nil, false, ErrOrderIDInUse
	}
	orders, err := queryPhoneOrders(ctx, q, `batch_id = ? ORDER BY id`, batchID)
	return orders, true, err
}

// resolveOrderIDCollision: saltó el índice único de order_id pese al chequeo.
// Si la fila que choca es de este lote, es un reintento (existing); si no, 409.
func (s *Store) resolveOrderIDCollision(ctx context.Context, probeID, batchID string, specs []orderSpec) ([]PhoneOrder, bool, error) {
	for _, sp := range specs {
		var pid, bid sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT probe_id, batch_id FROM commands WHERE order_id = ?`, sp.orderID).Scan(&pid, &bid)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if pid.String != probeID || bid.String != batchID {
			return nil, false, ErrOrderIDInUse
		}
	}
	orders, err := queryPhoneOrders(ctx, s.db, `batch_id = ? ORDER BY id`, batchID)
	if err != nil {
		return nil, false, err
	}
	if len(orders) == 0 {
		return nil, false, ErrOrderIDInUse
	}
	return orders, true, nil
}

func buildOrderSpecs(req OrderRequest, batchID, probeID string, p ProbeInfo, slots []ProbeTarget, now time.Time) ([]orderSpec, error) {
	bySlot := map[string]ProbeTarget{}
	names := make([]string, 0, len(slots))
	for _, t := range slots {
		bySlot[t.Slot] = t
		names = append(names, t.Slot)
	}
	execAt := now
	if req.ExecuteAt != nil && strings.TrimSpace(*req.ExecuteAt) != "" {
		t, err := ParseTS(*req.ExecuteAt)
		if err != nil {
			return nil, badReq("execute_at inválido (RFC 3339)")
		}
		execAt = t
	}
	var notAfter *time.Time
	if req.NotAfter != nil && strings.TrimSpace(*req.NotAfter) != "" {
		t, err := ParseTS(*req.NotAfter)
		if err != nil {
			return nil, badReq("not_after inválido (RFC 3339)")
		}
		notAfter = &t
	}
	dur, err := orderDuration(req.DurationS, p.DurationS)
	if err != nil {
		return nil, err
	}
	requestedBy := req.RequestedBy
	if requestedBy == "" {
		requestedBy = "dashboard"
	}
	mk := func(orderID, slot string, ea, na time.Time, reason string) orderSpec {
		sp := orderSpec{orderID: orderID, batchID: batchID, target: slot, durationS: dur,
			executeAt: ea, notAfter: na, reason: reason, requestedBy: requestedBy, allowFallback: req.AllowFallback}
		if slot == "next" {
			sp.deviceID = probeID
			return sp
		}
		t := bySlot[slot]
		sp.slot, sp.deviceID, sp.table, sp.preferred = slot, t.DeviceID, t.RoutingTable, t.DeviceID
		if req.AllowFallback {
			if o := otherSlot(slots, slot); o != nil {
				sp.fallback = o.DeviceID
			}
		}
		return sp
	}

	switch req.Target {
	case "":
		return nil, badReq("falta target")
	case "sequence":
		if req.Count < 2 || req.Count > 48 {
			return nil, badReq("count debe estar entre 2 y 48")
		}
		if req.SpacingS < 60 || req.SpacingS > 86400 {
			return nil, badReq("spacing_s debe estar entre 60 y 86400")
		}
		first := req.First
		if first == "" {
			first = "A"
			if _, ok := bySlot[first]; !ok {
				first = names[0]
			}
		}
		start := -1
		for i, n := range names {
			if n == first {
				start = i
			}
		}
		if start < 0 {
			return nil, badReq("first inválido: %s (slots activos: %s)", first, strings.Join(names, ", "))
		}
		spacing := time.Duration(req.SpacingS) * time.Second
		specs := make([]orderSpec, 0, req.Count)
		for i := 0; i < req.Count; i++ {
			slot := names[(start+i)%len(names)]
			ea := execAt.Add(time.Duration(i) * spacing)
			specs = append(specs, mk(fmt.Sprintf("%s-%d", batchID, i+1), slot, ea, ea.Add(spacing), "sequence"))
		}
		return specs, nil
	default:
		reason := "requested"
		if req.Target == "next" {
			reason = "next"
		} else if _, ok := bySlot[req.Target]; !ok {
			return nil, badReq("target inválido: %s (se espera un slot activo —%s—, next o sequence)", req.Target, strings.Join(names, ", "))
		}
		na := execAt.Add(defaultOrderWindow)
		if notAfter != nil {
			na = *notAfter
		}
		if !na.After(execAt) {
			return nil, badReq("not_after debe ser posterior a execute_at")
		}
		return []orderSpec{mk(batchID, req.Target, execAt, na, reason)}, nil
	}
}

// orderDuration: la pedida (3–60 s), o la de la sonda (o 10) acotada a ese rango.
func orderDuration(req *int, probeDur *int) (int, error) {
	if req != nil {
		if *req < 3 || *req > 60 {
			return 0, badReq("duration_s debe estar entre 3 y 60")
		}
		return *req, nil
	}
	d := 10
	if probeDur != nil && *probeDur > 0 {
		d = *probeDur
	}
	if d < 3 {
		d = 3
	}
	if d > 60 {
		d = 60
	}
	return d, nil
}

// createPhoneOrderForDevice: POST /commands con runner "probe" sobre un equipo
// de una sonda de celular. Crea una orden (UUID del servidor, target = slot
// del equipo, requested, not_after +30 min). La ubicación del navegador que
// traía el comando no se guarda: el celular toma su propio fix.
func (s *Store) createPhoneOrderForDevice(ctx context.Context, probeID, deviceID string, durationS *int, requestedBy string, now time.Time) (int64, string, error) {
	now = now.UTC().Truncate(time.Second)
	var slot, table string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(slot,''), routing_table FROM probe_targets WHERE probe_id = ? AND device_id = ?`,
		probeID, deviceID).Scan(&slot, &table); err != nil {
		return 0, "", fmt.Errorf("equipo %s de la sonda %s: %w", deviceID, probeID, err)
	}
	if slot == "" {
		return 0, "", fmt.Errorf("el equipo %s no tiene slot en la sonda %s", deviceID, probeID)
	}
	p, err := s.ProbeInfoOf(ctx, probeID)
	if err != nil {
		return 0, "", err
	}
	dur, err := orderDuration(nil, p.DurationS)
	if err != nil {
		return 0, "", err
	}
	if durationS != nil {
		dur = min(max(*durationS, 3), 60)
	}
	id := NewUUID()
	sp := orderSpec{orderID: id, batchID: id, target: slot, slot: slot, deviceID: deviceID, table: table,
		preferred: deviceID, durationS: dur, executeAt: now, notAfter: now.Add(defaultOrderWindow),
		reason: "requested", requestedBy: requestedBy}
	cid, err := insertPhoneOrder(ctx, s.db, probeID, sp, now)
	if err != nil {
		return 0, "", fmt.Errorf("crear orden de celular: %w", err)
	}
	return cid, id, nil
}

// countOpenPhoneOrders: pruebas abiertas de la sonda (o de un slot): pending,
// delivered, o running con updated_at de hace menos de 30 min. Las órdenes de
// reinicio (§6) no cuentan: no son pruebas y no frenan el ciclo.
func (s *Store) countOpenPhoneOrders(ctx context.Context, probeID, slot string, now time.Time) (int, error) {
	q := `SELECT COUNT(*) FROM commands WHERE runner = 'phone' AND probe_id = ? AND type <> 'reboot_router'
AND (status IN ('pending','delivered') OR (status = 'running' AND updated_at >= ?))`
	args := []any{probeID, FormatTS(now.Add(-phoneRunningStale))}
	if slot != "" {
		q += ` AND slot = ?`
		args = append(args, slot)
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// enqueuePhoneCycle: ciclo de una sonda de celular ("Ciclo ahora" y
// planificador). Una orden por equipo activo, en orden de slot, salvo los que
// ya tengan una abierta.
func (s *Store) enqueuePhoneCycle(ctx context.Context, probeID, requestedBy string, dur sql.NullInt64, intervalS int, now time.Time) ([]int64, []string, error) {
	now = now.UTC().Truncate(time.Second)
	slots, err := activeSlots(ctx, s.db, probeID)
	if err != nil {
		return nil, nil, err
	}
	if len(slots) == 0 {
		return nil, nil, fmt.Errorf("la sonda %s no tiene equipos activos", probeID)
	}
	var probeDur *int
	if dur.Valid {
		v := int(dur.Int64)
		probeDur = &v
	}
	d, _ := orderDuration(nil, probeDur)
	window := time.Duration(max(intervalS, 1800)) * time.Second
	ids, orderIDs := []int64{}, []string{}
	for _, t := range slots {
		open, err := s.countOpenPhoneOrders(ctx, probeID, t.Slot, now)
		if err != nil {
			return nil, nil, err
		}
		if open > 0 {
			continue
		}
		oid := NewUUID()
		sp := orderSpec{orderID: oid, batchID: oid, target: t.Slot, slot: t.Slot, deviceID: t.DeviceID,
			table: t.RoutingTable, preferred: t.DeviceID, durationS: d, executeAt: now, notAfter: now.Add(window),
			reason: "alternation", requestedBy: requestedBy}
		id, err := insertPhoneOrder(ctx, s.db, probeID, sp, now)
		if err != nil {
			return nil, nil, fmt.Errorf("encolar orden de %s: %w", t.Slot, err)
		}
		ids = append(ids, id)
		orderIDs = append(orderIDs, oid)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE probes SET last_cycle_at = ? WHERE probe_id = ?`, FormatTS(now), probeID); err != nil {
		return nil, nil, err
	}
	return ids, orderIDs, nil
}

// SweepPhoneOrders cierra (con la hora del servidor) las órdenes que el
// celular no ejecutó a tiempo: pending/delivered pasado not_after → expired;
// running sin cambios hace más de 30 min → interrupted. closed_by = server
// (final blando: un resultado que llegue después la cierra igual).
// probeID vacío = todas las sondas.
func (s *Store) SweepPhoneOrders(ctx context.Context, probeID string, now time.Time) (int64, int64, error) {
	ts := FormatTS(now)
	probeCond, args := "", []any{}
	if probeID != "" {
		probeCond, args = " AND probe_id = ?", []any{probeID}
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE commands SET status='expired', error='vencida-sin-ejecutar', completed_at=?, updated_at=?, closed_by='server'
WHERE runner='phone' AND status IN ('pending','delivered') AND not_after IS NOT NULL AND not_after < ?`+probeCond,
		append([]any{ts, ts, ts}, args...)...)
	if err != nil {
		return 0, 0, fmt.Errorf("vencer órdenes: %w", err)
	}
	expired, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `
UPDATE commands SET status='interrupted', error='sin-cierre', completed_at=?, updated_at=?, closed_by='server'
WHERE runner='phone' AND status='running' AND (updated_at IS NULL OR updated_at < ?)`+probeCond,
		append([]any{ts, ts, FormatTS(now.Add(-phoneRunningStale))}, args...)...)
	if err != nil {
		return expired, 0, fmt.Errorf("interrumpir órdenes: %w", err)
	}
	interrupted, _ := res.RowsAffected()
	return expired, interrupted, nil
}

// ClosedOrder: una orden que cerró el servidor o el dashboard (lista closed de GET .../orders).
type ClosedOrder struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

// ListPhoneOrders: lo que trae el celular. Órdenes abiertas con execute_at <=
// now+horizon (máx. 200, truncated si había más) y las cerradas por el
// servidor o el dashboard en las últimas 24 h o que todavía podrían
// ejecutarse (not_after futuro), máx. 500. No barre: el llamador barre antes.
func (s *Store) ListPhoneOrders(ctx context.Context, probeID string, horizon time.Duration, now time.Time) ([]PhoneOrder, bool, []ClosedOrder, error) {
	orders, err := queryPhoneOrders(ctx, s.db, `probe_id = ? AND status IN ('pending','delivered','running') AND execute_at <= ?
ORDER BY execute_at, id LIMIT ?`, probeID, FormatTS(now.Add(horizon)), maxOpenOrdersListed+1)
	if err != nil {
		return nil, false, nil, err
	}
	truncated := len(orders) > maxOpenOrdersListed
	if truncated {
		orders = orders[:maxOpenOrdersListed]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT order_id, status FROM commands
WHERE runner='phone' AND probe_id = ? AND order_id IS NOT NULL AND closed_by IN ('server','dashboard')
  AND (updated_at >= ? OR not_after > ?)
ORDER BY id DESC LIMIT ?`, probeID, FormatTS(now.Add(-24*time.Hour)), FormatTS(now), maxClosedOrdersListed)
	if err != nil {
		return nil, false, nil, err
	}
	defer rows.Close()
	closed := []ClosedOrder{}
	for rows.Next() {
		var c ClosedOrder
		if err := rows.Scan(&c.OrderID, &c.Status); err != nil {
			return nil, false, nil, err
		}
		closed = append(closed, c)
	}
	return orders, truncated, closed, rows.Err()
}

// ListPhoneOrderHistory: todas las órdenes de celular de la sonda, más nuevas
// primero (dashboard). Sin barrer.
func (s *Store) ListPhoneOrderHistory(ctx context.Context, probeID string, limit int) ([]PhoneOrder, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	orders, err := queryPhoneOrders(ctx, s.db, `probe_id = ? ORDER BY id DESC LIMIT ?`, probeID, limit+1)
	if err != nil {
		return nil, false, err
	}
	truncated := len(orders) > limit
	if truncated {
		orders = orders[:limit]
	}
	return orders, truncated, nil
}

// AckPhoneOrders: el celular confirma que tiene las órdenes. Solo pending → delivered.
func (s *Store) AckPhoneOrders(ctx context.Context, probeID string, ids []string, now time.Time) (acked, unknown, unchanged []string, err error) {
	acked, unknown, unchanged = []string{}, []string{}, []string{}
	ts := FormatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM commands WHERE runner='phone' AND probe_id = ? AND order_id = ?`, probeID, id).Scan(&status)
		if err == sql.ErrNoRows {
			unknown = append(unknown, id)
			continue
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if status != "pending" {
			unchanged = append(unchanged, id)
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET status='delivered', delivered_at=?, updated_at=?
WHERE runner='phone' AND probe_id = ? AND order_id = ? AND status='pending'`, ts, ts, probeID, id); err != nil {
			return nil, nil, nil, err
		}
		acked = append(acked, id)
	}
	return acked, unknown, unchanged, tx.Commit()
}

// CancelRequest: cuerpo de POST .../orders/cancel (una de las tres formas).
type CancelRequest struct {
	OrderIDs []string `json:"order_ids"`
	BatchID  string   `json:"batch_id"`
	AllOpen  bool     `json:"all_open"`
}

// CancelPhoneOrders: pending/delivered → cancelled (closed_by = dashboard). Una
// orden en running no se cancela: el celular ya está midiendo.
func (s *Store) CancelPhoneOrders(ctx context.Context, probeID string, req CancelRequest, now time.Time) (cancelled, unknown, unchanged []string, err error) {
	cancelled, unknown, unchanged = []string{}, []string{}, []string{}
	ts := FormatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback()

	type cand struct{ id, status string }
	var cands []cand
	load := func(where string, args ...any) error {
		rows, err := tx.QueryContext(ctx, `SELECT order_id, status FROM commands WHERE runner='phone' AND probe_id = ? AND order_id IS NOT NULL AND `+where+` ORDER BY id`,
			append([]any{probeID}, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.status); err != nil {
				return err
			}
			cands = append(cands, c)
		}
		return rows.Err()
	}
	switch {
	case req.AllOpen:
		err = load(`status IN ('pending','delivered','running')`)
	case req.BatchID != "":
		err = load(`batch_id = ?`, req.BatchID)
		if err == nil && len(cands) == 0 {
			unknown = append(unknown, req.BatchID)
		}
	default:
		for _, id := range req.OrderIDs {
			var status string
			e := tx.QueryRowContext(ctx, `SELECT status FROM commands WHERE runner='phone' AND probe_id = ? AND order_id = ?`, probeID, id).Scan(&status)
			if e == sql.ErrNoRows {
				unknown = append(unknown, id)
				continue
			}
			if e != nil {
				return nil, nil, nil, e
			}
			cands = append(cands, cand{id, status})
		}
	}
	if err != nil {
		return nil, nil, nil, err
	}
	for _, c := range cands {
		if c.status != "pending" && c.status != "delivered" {
			unchanged = append(unchanged, c.id)
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET status='cancelled', closed_by='dashboard', error='cancelada', completed_at=?, updated_at=?
WHERE runner='phone' AND probe_id = ? AND order_id = ? AND status IN ('pending','delivered')`, ts, ts, probeID, c.id); err != nil {
			return nil, nil, nil, err
		}
		cancelled = append(cancelled, c.id)
	}
	return cancelled, unknown, unchanged, tx.Commit()
}

// phoneTransitions: cambios de estado que el celular puede pedir por /state.
var phoneTransitions = map[string]map[string]bool{
	"pending":   {"delivered": true, "running": true, "interrupted": true, "expired": true},
	"delivered": {"running": true, "interrupted": true, "expired": true},
	"running":   {"interrupted": true},
}

// PhoneStateUpdate: cuerpo de POST .../orders/{order_id}/state ya validado.
// At = hora del celular (started_at en running). Step y Result solo se
// guardan en órdenes reboot_router (§6); en las pruebas se ignoran.
type PhoneStateUpdate struct {
	Status   string
	At       *time.Time
	ResultID string
	Error    string
	Step     string
	Result   json.RawMessage
}

// SetPhoneOrderState aplica un cambio de estado sin resultado de medición.
// Devuelve el estado actual y ignored=true si la transición no era válida
// (repetida, hacia atrás, o sobre una orden final, dura o blanda). Todo lo
// que no es At usa la hora del servidor.
//
// Órdenes reboot_router (§6): no generan medición, así que se cierran por acá
// con done/failed (y el result JSON del celular), desde abiertas o desde un
// final blando (el barrido las cerró porque el celular no avisó a tiempo). Un
// running repetido con otro step (o con result) actualiza el progreso.
//
// En un reinicio manda lo que dice el celular: si la orden quedó cancelled
// (dashboard o servidor) o en un final blando pero el celular ya la había
// tomado (se entera de la cancelación en su próximo GET), su running,
// interrupted, done o failed se aplica igual, para que el historial y el
// enfriamiento sepan que el comando pudo salir. Y cuando un reinicio pasa a
// running se cancelan (closed_by server) los otros reinicios pendientes del
// mismo equipo: nunca dos reboot seguidos.
func (s *Store) SetPhoneOrderState(ctx context.Context, probeID, orderID string, u PhoneStateUpdate, now time.Time) (string, bool, error) {
	ts := FormatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var cur, typ, deviceID string
	var closedBy, curStep sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT status, type, closed_by, step, device_id FROM commands WHERE runner='phone' AND probe_id = ? AND order_id = ?`,
		probeID, orderID).Scan(&cur, &typ, &closedBy, &curStep, &deviceID)
	if err == sql.ErrNoRows {
		return "", false, ErrOrderNotFound
	}
	if err != nil {
		return "", false, err
	}
	reboot := typ == RebootOrderType
	status := u.Status
	final := status == "done" || status == "failed"
	if final && !reboot {
		return "", false, badReq("done/failed se cierran con /results")
	}

	softFinal := (cur == "expired" || cur == "interrupted") && closedBy.String == "server"
	// Reinicio cerrado por el servidor o el dashboard: el reporte del celular
	// manda (ver arriba). Un final del propio celular no se reabre.
	rebootReopen := reboot && (softFinal || (cur == "cancelled" && closedBy.String != "phone"))
	set := `status=?, updated_at=?`
	args := []any{status, ts}
	switch {
	case final || (rebootReopen && status == "interrupted"):
		if !openPhoneStatuses[cur] && !softFinal && !rebootReopen {
			return cur, true, nil
		}
		set += `, completed_at=?, closed_by='phone', error=?`
		args = append(args, ts, nullStr(u.Error))
	case rebootReopen && status == "running":
		started := now
		if u.At != nil {
			started = *u.At
		}
		set += `, started_at=?, result_id=?, completed_at=NULL, closed_by=NULL, error=NULL`
		args = append(args, FormatTS(started), nullStr(u.ResultID))
	case reboot && status == cur && cur == "running":
		// Progreso de un reinicio: solo si trae algo nuevo.
		if (u.Step == "" || u.Step == curStep.String) && u.Result == nil {
			return cur, true, nil
		}
	case !phoneTransitions[cur][status]:
		return cur, true, nil
	default:
		switch status {
		case "delivered":
			set += `, delivered_at=?`
			args = append(args, ts)
		case "running":
			started := now
			if u.At != nil {
				started = *u.At
			}
			set += `, started_at=?, result_id=?`
			args = append(args, FormatTS(started), nullStr(u.ResultID))
		case "interrupted", "expired":
			set += `, completed_at=?, closed_by='phone', error=?`
			args = append(args, ts, nullStr(u.Error))
		}
	}
	if reboot {
		if u.Step != "" {
			set += `, step=?`
			args = append(args, u.Step)
		}
		if u.Result != nil {
			set += `, result_json=?`
			args = append(args, string(u.Result))
		}
	}
	args = append(args, probeID, orderID, cur)
	res, err := tx.ExecContext(ctx, `UPDATE commands SET `+set+` WHERE runner='phone' AND probe_id = ? AND order_id = ? AND status = ?`, args...)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return cur, true, nil
	}
	if reboot && status == "running" && cur != "running" {
		// Otro reinicio del mismo equipo que se coló (p. ej. pedido tras
		// cancelar éste justo cuando el celular ya lo corría): se cancela.
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET status='cancelled', closed_by='server', error='reinicio-duplicado',
	completed_at=?, updated_at=?
WHERE runner='phone' AND type=? AND device_id = ? AND order_id <> ? AND status IN ('pending','delivered')`,
			ts, ts, RebootOrderType, deviceID, orderID); err != nil {
			return "", false, err
		}
	}
	return status, false, tx.Commit()
}

// ------------------------------------------------------------------ resultados

// ProbeResultCols: columnas nuevas de measurements que llena un resultado del
// celular (las viejas salen del JSON, como en insertMeasurement). Las arma la
// capa HTTP ya validadas y con la atribución decidida por el servidor.
type ProbeResultCols struct {
	ResultID        string
	OrderID         string // el que mandó el celular ("" = sin orden)
	ProbeID         string
	MeasuredBy      string
	RoutingTable    string
	Slot            string // resuelto por el servidor ("" = sin atribuir)
	DeviceID        string // resuelto por el servidor
	Attributed      bool   // salió por un equipo de la sonda (Ethernet confirmado)
	SelectionReason string
	TestStartedAt   time.Time
	TestFinishedAt  string
	TestStatus      string // done | failed
	Error           string
	ClockSkewS      *float64
	LossPct         *float64
	GPSTS           string
	GPSAgeS         *float64
	LocationStatus  string
	LocationSource  string
	ExecutedOffline *bool
	NetPath         string
	TargetHost      string
	EgressIP        string
}

// ProbeResultOutcome: qué pasó con un resultado.
type ProbeResultOutcome struct {
	Duplicate     bool
	MeasurementID int64
	OrderLinked   bool
	OrderStatus   string // estado de la orden tras procesar ("" si no hay orden de esta sonda)
	OrderKnown    bool   // la orden existe y es de esta sonda
}

// ErrBadResult: el resultado no se puede guardar tal como vino (no reintentar).
type ErrBadResult struct{ Msg string }

func (e ErrBadResult) Error() string { return e.Msg }

// InsertProbeResult guarda un resultado del celular y, si trae una orden de
// esta sonda, la cierra, todo en UNA transacción. Idempotente por result_id
// (INSERT ... ON CONFLICT DO NOTHING; duplicado = RowsAffected 0, y entonces
// no se reevalúa la orden). No usa cachedLocation: sin fix, sin posición.
func (s *Store) InsertProbeResult(ctx context.Context, raw json.RawMessage, c ProbeResultCols, nc NetClassification, now time.Time) (ProbeResultOutcome, error) {
	var out ProbeResultOutcome
	nc = nc.clean()
	ts := FormatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	// ¿La orden es de esta sonda? Si no, la medición se guarda sin order_id en
	// la columna y el que mandó el celular queda en raw.order_id_client.
	var ord struct {
		id       int64
		status   string
		closedBy sql.NullString
		notAfter sql.NullString
	}
	orderCol := ""
	if c.OrderID != "" {
		var typ string
		err := tx.QueryRowContext(ctx, `SELECT id, status, closed_by, not_after, type FROM commands WHERE runner='phone' AND probe_id = ? AND order_id = ?`,
			c.ProbeID, c.OrderID).Scan(&ord.id, &ord.status, &ord.closedBy, &ord.notAfter, &typ)
		switch {
		case err == nil && typ == RebootOrderType:
			// Un reinicio nunca genera medición (§6.3): se cierra con /state.
			return out, ErrBadResult{"la orden " + c.OrderID + " es un reinicio (reboot_router): se cierra con /state y no genera medición"}
		case err == sql.ErrNoRows:
			edited, e := editRaw(raw, []string{"order_id"}, map[string]any{"order_id_client": c.OrderID})
			if e != nil {
				return out, ErrBadResult{"resultado inválido: " + e.Error()}
			}
			raw = edited
		case err != nil:
			return out, err
		default:
			orderCol = c.OrderID
			out.OrderKnown = true
			out.OrderStatus = ord.status
		}
	}

	var m Measurement
	if err := json.Unmarshal(raw, &m); err != nil {
		return out, ErrBadResult{"resultado inválido: " + err.Error()}
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO measurements (received_at, device_id, source, tag, ts, operator, rat, band_lte, nr_band, pci,
	rsrp_dbm, rsrq_db, sinr_db, rssi_dbm, eps_reg, nr_reg, lat, lon, gps_accuracy_m, gps_source, uptime_s,
	down_mbps, up_mbps, ping_ms, jitter_ms, via_modem, note, lat_end, lon_end, gps_accuracy_m_end, gps_source_end,
	net_route, net_asn, net_confidence, raw,
	result_id, order_id, probe_id, measured_by, routing_table, slot, selection_reason, test_started_at, test_finished_at,
	test_status, loss_pct, gps_ts, gps_age_s, location_status, location_source, executed_offline, net_path, target_host, egress_ip)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,
	?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(result_id) DO NOTHING`,
		ts, c.DeviceID, nullStr(m.Source), nullStr(m.Tag), nullStr(m.TS),
		nullStr(m.Operator), nullStr(m.RAT), m.BandLTE, m.NRBand, m.PCI, m.RSRPDbm, m.RSRQDb, m.SINRDb, m.RSSIDbm,
		m.EPSReg, m.NRReg, m.Lat, m.Lon, m.GPSAccuracyM, nullStr(m.GPSSource), m.UptimeS, m.DownMbps, m.UpMbps,
		m.PingMs, m.JitterMs, boolToInt(m.ViaModem), nullStr(m.Note),
		m.LatEnd, m.LonEnd, m.GPSAccuracyMEnd, nullStr(m.GPSSourceEnd),
		nullStr(nc.Route), nc.ASN, nullStr(nc.Confidence), string(raw),
		c.ResultID, nullStr(orderCol), c.ProbeID, nullStr(c.MeasuredBy), nullStr(c.RoutingTable), nullStr(c.Slot),
		nullStr(c.SelectionReason), FormatTS(c.TestStartedAt), nullStr(c.TestFinishedAt), c.TestStatus, c.LossPct,
		nullStr(c.GPSTS), c.GPSAgeS, nullStr(c.LocationStatus), nullStr(c.LocationSource), boolToInt(c.ExecutedOffline),
		c.NetPath, nullStr(c.TargetHost), nullStr(c.EgressIP))
	if err != nil {
		return out, fmt.Errorf("insertar resultado: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		out.Duplicate = true
		if err := tx.QueryRowContext(ctx, `SELECT id FROM measurements WHERE result_id = ?`, c.ResultID).Scan(&out.MeasurementID); err != nil {
			return out, fmt.Errorf("leer resultado duplicado: %w", err)
		}
		return out, nil
	}
	mid, err := res.LastInsertId()
	if err != nil {
		return out, err
	}
	out.MeasurementID = mid

	if out.OrderKnown && orderAcceptsResult(ord.status, ord.closedBy.String, ord.notAfter.String, c) {
		set := `status=?, completed_at=?, updated_at=?, closed_by='phone', measurement_id=?, result_id=?, error=?`
		args := []any{c.TestStatus, ts, ts, mid, c.ResultID, nullStr(c.Error)}
		if c.Attributed {
			set += `, slot=?, device_id=?, routing_table=?`
			args = append(args, c.Slot, c.DeviceID, c.RoutingTable)
		}
		args = append(args, ord.id)
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET `+set+` WHERE id = ?`, args...); err != nil {
			return out, fmt.Errorf("cerrar orden: %w", err)
		}
		out.OrderLinked = true
		out.OrderStatus = c.TestStatus
	}
	return out, tx.Commit()
}

// orderAcceptsResult: una orden abierta, o en final blando (la cerró el
// barrido del servidor) si la prueba empezó antes de not_after (corrigiendo
// por el desfase del reloj del celular si vino).
func orderAcceptsResult(status, closedBy, notAfter string, c ProbeResultCols) bool {
	if openPhoneStatuses[status] {
		return true
	}
	if (status != "expired" && status != "interrupted") || closedBy != "server" {
		return false
	}
	na, err := ParseTS(notAfter)
	if err != nil {
		return false
	}
	started := c.TestStartedAt
	if c.ClockSkewS != nil {
		started = started.Add(-time.Duration(*c.ClockSkewS * float64(time.Second)))
	}
	return !started.After(na)
}

// editRaw borra y escribe claves de un JSON crudo conservando los números tal
// cual (UseNumber). Una clave con valor nil se escribe como null.
func editRaw(raw json.RawMessage, drop []string, set map[string]any) (json.RawMessage, error) {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		obj = map[string]any{}
	}
	for _, k := range drop {
		delete(obj, k)
	}
	for k, v := range set {
		obj[k] = v
	}
	return json.Marshal(obj)
}

// ------------------------------------------------------------------ estado en vivo

// ProbeStatus: último estado en vivo del celular (GET .../status y phone_status).
type ProbeStatus struct {
	ProbeID    string          `json:"probe_id"`
	ReceivedAt *string         `json:"received_at"`
	AgeS       *int64          `json:"age_s"`
	Online     bool            `json:"online"`
	Status     json.RawMessage `json:"status"`
}

// ProbeStatusUpdate: lo que el servidor saca del cuerpo para indexar y ordenar.
type ProbeStatusUpdate struct {
	MeasuredBy       string
	Phase            string
	CurrentTable     string
	Seq              *int64
	ServiceStartedAt string
}

// UpsertProbeStatus guarda el último estado. Descarta (discarded=true) un
// cuerpo que llegó tarde: seq menor que el guardado con el mismo
// service_started_at y el mismo measured_by. prevMeasuredBy = el celular
// anterior si cambió.
func (s *Store) UpsertProbeStatus(ctx context.Context, probeID string, raw json.RawMessage, u ProbeStatusUpdate, now time.Time) (discarded bool, prevMeasuredBy string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	var oldBy sql.NullString
	var oldRaw string
	err = tx.QueryRowContext(ctx, `SELECT measured_by, raw FROM probe_status WHERE probe_id = ?`, probeID).Scan(&oldBy, &oldRaw)
	if err != nil && err != sql.ErrNoRows {
		return false, "", err
	}
	if err == nil {
		var old struct {
			Seq              *json.Number `json:"seq"`
			ServiceStartedAt string       `json:"service_started_at"`
		}
		_ = json.Unmarshal([]byte(oldRaw), &old)
		if u.Seq != nil && old.Seq != nil && oldBy.String == u.MeasuredBy && old.ServiceStartedAt == u.ServiceStartedAt && u.ServiceStartedAt != "" {
			if oldSeq, e := old.Seq.Int64(); e == nil && *u.Seq < oldSeq {
				return true, "", nil
			}
		}
		if oldBy.String != "" && u.MeasuredBy != "" && oldBy.String != u.MeasuredBy {
			prevMeasuredBy = oldBy.String
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO probe_status (probe_id, received_at, measured_by, phase, current_table, raw) VALUES (?,?,?,?,?,?)
ON CONFLICT(probe_id) DO UPDATE SET received_at=excluded.received_at, measured_by=excluded.measured_by,
	phase=excluded.phase, current_table=excluded.current_table, raw=excluded.raw`,
		probeID, FormatTS(now), nullStr(u.MeasuredBy), nullStr(u.Phase), nullStr(u.CurrentTable), string(raw)); err != nil {
		return false, "", fmt.Errorf("guardar estado de la sonda: %w", err)
	}
	return false, prevMeasuredBy, tx.Commit()
}

// ProbeStatusOf: último estado de la sonda; ok=false si todavía no mandó ninguno.
func (s *Store) ProbeStatusOf(ctx context.Context, probeID string, now time.Time) (ProbeStatus, bool, error) {
	all, err := s.probeStatuses(ctx, probeID, now)
	if err != nil {
		return ProbeStatus{ProbeID: probeID}, false, err
	}
	if st, ok := all[probeID]; ok {
		return st, true, nil
	}
	return ProbeStatus{ProbeID: probeID}, false, nil
}

// ProbeStatuses: el último estado de cada sonda que tenga uno.
func (s *Store) ProbeStatuses(ctx context.Context, now time.Time) (map[string]ProbeStatus, error) {
	return s.probeStatuses(ctx, "", now)
}

func (s *Store) probeStatuses(ctx context.Context, probeID string, now time.Time) (map[string]ProbeStatus, error) {
	q := `SELECT probe_id, received_at, raw FROM probe_status`
	var args []any
	if probeID != "" {
		q += ` WHERE probe_id = ?`
		args = append(args, probeID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProbeStatus{}
	for rows.Next() {
		var st ProbeStatus
		var received, raw string
		if err := rows.Scan(&st.ProbeID, &received, &raw); err != nil {
			return nil, err
		}
		st.ReceivedAt = &received
		st.Status = json.RawMessage(raw)
		if t, err := ParseTS(received); err == nil {
			age := int64(now.Sub(t).Seconds())
			if age < 0 {
				age = 0
			}
			st.AgeS = &age
			st.Online = now.Sub(t) <= phoneStatusOnline
		}
		out[st.ProbeID] = st
	}
	return out, rows.Err()
}
