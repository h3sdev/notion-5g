package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Reinicio remoto de un router bajo prueba (docs/CONTRATO-SONDA-AB.md §6).
//
// El backend nunca se conecta al router: guarda una orden reboot_router en la
// cola del celular de la sonda (commands, runner='phone'), y el celular la
// ejecuta por SSH a través del MikroTik, entre pruebas. La orden y su
// resultado quedan en commands como las demás; no generan medición.
//
// La recomendación ("reiniciar: sin conexión hace N min") solo se calcula y
// se muestra: nunca se reinicia solo.

const (
	// RebootOrderType: commands.type de una orden de reinicio.
	RebootOrderType = "reboot_router"

	// rebootWindow: not_after de la orden = creación + esto.
	rebootWindow = 15 * time.Minute
	// RebootCooldown: sin force, no se pide otro reinicio del mismo equipo
	// antes de esto desde el anterior (409).
	RebootCooldown = 15 * time.Minute
	// RebootOfflineAfter: se recomienda reiniciar pasado esto sin conexión.
	RebootOfflineAfter = 10 * time.Minute

	// agentLookback: un equipo "tiene agente" si su agente (heartbeat con
	// source=router) reportó en este lapso. Pasado esto se lo trata como sin
	// agente (desinstalado o equipo retirado) y solo cuenta la salud que ve
	// el celular.
	agentLookback = 7 * 24 * time.Hour
	// agentOfflineAfter: desde cuándo se muestra offline_since por el agente
	// (mismo umbral que "en línea" en GET /devices). Recomendar exige además
	// RebootOfflineAfter.
	agentOfflineAfter = onlineThreshold
	// phoneHealthFresh: el estado del celular cuenta como fresco si llegó
	// hace menos de esto (lo manda cada 15 s; esto tolera un par de fallos
	// sin que la recomendación parpadee).
	phoneHealthFresh = 3 * time.Minute
	// healthCheckStale: la verificación de salud de un router (checked_at)
	// más vieja que esto respecto de sent_at no se usa (el celular la hace
	// cada mikrotik_idle_check_s, nunca durante una prueba de ese router).
	healthCheckStale = 15 * time.Minute
)

// rebootNotSentErrors: errores con que el celular cierra un reinicio fallido
// ANTES de mandar el comando (§6.2 paso 5). Esos no cuentan para el
// enfriamiento: el router no se reinició. timeout-back sí cuenta (el comando
// salió, el router no volvió a tiempo). Si el resultado dice ssh_ok=true, el
// comando salió y cuenta aunque el error sea uno de estos (p. ej.
// mikrotik-unreachable mientras se vigilaba la vuelta).
var rebootNotSentErrors = []string{"ssh-auth", "ssh-connect", "no-ethernet", "mikrotik-unreachable"}

// rebootNotStartedErrors: el celular la cerró interrupted sin empezar (no
// encontró el slot en su sonda, o no conoce el tipo de orden). Tampoco
// cuentan para el enfriamiento, salvo con result.ssh_ok=true.
var rebootNotStartedErrors = []string{"slot-desconocido", "tipo-desconocido"}

// ErrRebootNoProbe: ninguna sonda runner=phone tiene ese equipo (404).
var ErrRebootNoProbe = errors.New("ninguna sonda de celular tiene ese equipo: no se puede reiniciar desde el backend")

// ErrRebootAppOutdated: el último estado del celular de la sonda no trae el
// bloque "routers" (§6.2), así que su app no conoce reboot_router. Una app
// vieja no mira el type y la correría como una prueba de velocidad: no se
// crea la orden (409, app_outdated).
var ErrRebootAppOutdated = errors.New("la app del celular de la sonda no sabe reiniciar routers (su estado no trae la salud por router, §6.2): actualice la app antes de reiniciar")

// RebootConflictError: 409 por un reinicio en curso o por el enfriamiento.
type RebootConflictError struct {
	Msg           string
	InProgress    bool
	Order         *PhoneOrder // la orden en curso, o el último reinicio (enfriamiento)
	LastRebootAt  string
	CooldownUntil string
}

func (e RebootConflictError) Error() string { return e.Msg }

// RebootRequest: cuerpo de POST /devices/{device_id}/reboot.
type RebootRequest struct {
	OrderID     string `json:"order_id"`
	RequestedBy string `json:"requested_by"`
	Reason      string `json:"reason"` // manual | recommended (vacío = manual)
	Force       bool   `json:"force"`
	// ProbeID (opcional): solo si el equipo estuviera en más de una sonda de celular.
	ProbeID string `json:"probe_id"`
}

// rebootOpenCond: una orden de reinicio que todavía puede ejecutarse.
const rebootOpenCond = `(status IN ('pending','delivered') AND not_after >= ?) OR (status = 'running' AND updated_at >= ?)`

// rebootCountsCond: un reinicio que cuenta para el enfriamiento (pudo haber
// llegado al router): no cancelado, no vencido sin ejecutar, y no fallido
// antes de mandar el comando. result_json siempre lo escribe el backend
// (json.Marshal), así que json_extract no falla. Un cancelado o vencido que
// el celular igual ejecutó deja de serlo (SetPhoneOrderState lo reabre).
var rebootCountsCond = `status NOT IN ('cancelled','expired') AND NOT (COALESCE(json_extract(result_json, '$.ssh_ok'), 0) <> 1 AND (
	(status = 'failed' AND COALESCE(error,'') IN ('` + strings.Join(rebootNotSentErrors, "','") + `')) OR
	(status = 'interrupted' AND COALESCE(error,'') IN ('` + strings.Join(rebootNotStartedErrors, "','") + `'))))`

// rebootBaseCol: desde cuándo corre el enfriamiento de un reinicio (hora del
// servidor): cuando el celular se llevó la orden, o su creación si nunca la
// confirmó. Una orden que esperó en cola (celular sin conexión) enfría desde
// que el celular la tomó, no desde que se pidió.
const rebootBaseCol = `COALESCE(delivered_at, created_at)`

// rebootBase: lo mismo que rebootBaseCol, sobre una orden ya leída.
func rebootBase(o *PhoneOrder) string {
	if o.DeliveredAt != nil && *o.DeliveredAt != "" {
		return *o.DeliveredAt
	}
	return o.CreatedAt
}

// CreateRebootOrder crea una orden reboot_router para la sonda de celular que
// tiene a deviceID como equipo. Idempotente por order_id: si ya existe una
// orden con ese order_id (o batch_id) que es un reinicio de este equipo, la
// devuelve con existing=true sin mirar enfriamiento ni el cuerpo nuevo; si es
// otra orden, ErrOrderIDInUse. Con un reinicio en curso: 409 (también con
// force). Con un reinicio en los últimos 15 min: 409 salvo force.
func (s *Store) CreateRebootOrder(ctx context.Context, deviceID string, req RebootRequest, now time.Time) (PhoneOrder, bool, error) {
	now = now.UTC().Truncate(time.Second)
	if deviceID == "" {
		return PhoneOrder{}, false, badReq("falta device_id")
	}
	orderID := strings.TrimSpace(req.OrderID)
	if orderID == "" {
		orderID = NewUUID()
	} else {
		if !ValidClientID(orderID) {
			return PhoneOrder{}, false, badReq("order_id inválido (se espera un UUID: letras, números y '-', 8 a 64)")
		}
		if seqSuffixRe.MatchString(orderID) {
			return PhoneOrder{}, false, badReq("order_id reservado para secuencias")
		}
	}
	reason := req.Reason
	if reason == "" {
		reason = "manual"
	}
	if reason != "manual" && reason != "recommended" {
		return PhoneOrder{}, false, badReq("reason debe ser manual o recommended")
	}
	requestedBy := strings.TrimSpace(req.RequestedBy)
	if requestedBy == "" {
		requestedBy = "dashboard"
	}
	if len(requestedBy) > 64 {
		return PhoneOrder{}, false, badReq("requested_by: máximo 64 caracteres")
	}
	if strings.ContainsFunc(requestedBy, unicode.IsControl) {
		return PhoneOrder{}, false, badReq("requested_by: sin caracteres de control")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PhoneOrder{}, false, err
	}
	defer tx.Rollback()

	if o, found, err := existingRebootOrder(ctx, tx, deviceID, orderID); err != nil || found {
		return o, found, err
	}

	// Sonda de celular que tiene el equipo (activo primero).
	q := `SELECT t.probe_id, COALESCE(t.slot,''), t.routing_table FROM probe_targets t JOIN probes p ON p.probe_id = t.probe_id
WHERE t.device_id = ? AND p.runner = 'phone'`
	args := []any{deviceID}
	if req.ProbeID != "" {
		q += ` AND t.probe_id = ?`
		args = append(args, req.ProbeID)
	}
	q += ` ORDER BY t.enabled DESC, p.enabled DESC, t.probe_id LIMIT 1`
	var probeID, slot, table string
	err = tx.QueryRowContext(ctx, q, args...).Scan(&probeID, &slot, &table)
	if err == sql.ErrNoRows {
		return PhoneOrder{}, false, ErrRebootNoProbe
	}
	if err != nil {
		return PhoneOrder{}, false, err
	}
	if slot == "" {
		return PhoneOrder{}, false, badReq("el equipo %s no tiene slot en la sonda %s", deviceID, probeID)
	}
	// La app del celular tiene que conocer reboot_router: la que lo conoce
	// siempre manda "routers" (aunque sea {}) en su estado en vivo.
	var routersType sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT json_type(raw, '$.routers') FROM probe_status WHERE probe_id = ?`, probeID).Scan(&routersType)
	if err != nil && err != sql.ErrNoRows {
		return PhoneOrder{}, false, err
	}
	if routersType.String != "object" {
		return PhoneOrder{}, false, ErrRebootAppOutdated
	}

	if open, err := lastRebootOrder(ctx, tx, deviceID, rebootOpenCond, FormatTS(now), FormatTS(now.Add(-phoneRunningStale))); err != nil {
		return PhoneOrder{}, false, err
	} else if open != nil {
		return PhoneOrder{}, false, RebootConflictError{
			Msg:        fmt.Sprintf("ya hay un reinicio en curso para %s (orden %s, %s)", deviceID, open.OrderID, open.Status),
			InProgress: true, Order: open, LastRebootAt: open.CreatedAt,
		}
	}
	if !req.Force {
		last, err := lastRebootOrder(ctx, tx, deviceID, rebootCountsCond+` AND `+rebootBaseCol+` >= ?`, FormatTS(now.Add(-RebootCooldown)))
		if err != nil {
			return PhoneOrder{}, false, err
		}
		if last != nil {
			until := ""
			ago := ""
			if t, err := ParseTS(rebootBase(last)); err == nil {
				until = FormatTS(t.Add(RebootCooldown))
				ago = fmt.Sprintf(" hace %d min", int(now.Sub(t).Minutes()))
			}
			return PhoneOrder{}, false, RebootConflictError{
				Msg:   fmt.Sprintf("%s ya se reinició%s (enfriamiento de %d min): use force para reiniciar igual", deviceID, ago, int(RebootCooldown.Minutes())),
				Order: last, LastRebootAt: last.CreatedAt, CooldownUntil: until,
			}
		}
	}

	sp := orderSpec{typ: RebootOrderType, rebootReason: reason, orderID: orderID, batchID: orderID,
		target: slot, slot: slot, deviceID: deviceID, table: table, preferred: deviceID,
		executeAt: now, notAfter: now.Add(rebootWindow), requestedBy: requestedBy}
	if _, err := insertPhoneOrder(ctx, tx, probeID, sp, now); err != nil {
		if isUniqueViolation(err) {
			tx.Rollback()
			// Otro pedido con el mismo order_id ganó la carrera.
			o, found, err := existingRebootOrder(ctx, s.db, deviceID, orderID)
			if err == nil && !found {
				err = ErrOrderIDInUse
			}
			return o, found, err
		}
		return PhoneOrder{}, false, fmt.Errorf("crear orden de reinicio: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PhoneOrder{}, false, err
	}
	orders, err := queryPhoneOrders(ctx, s.db, `order_id = ?`, orderID)
	if err != nil {
		return PhoneOrder{}, false, err
	}
	if len(orders) == 0 {
		return PhoneOrder{}, false, fmt.Errorf("orden de reinicio %s no quedó guardada", orderID)
	}
	return orders[0], false, nil
}

// existingRebootOrder: si orderID ya está usado. Un reinicio de este mismo
// equipo con ese order_id es un reintento (found=true); cualquier otra orden
// que lo use (como order_id o batch_id) es ErrOrderIDInUse.
func existingRebootOrder(ctx context.Context, q queryer, deviceID, orderID string) (PhoneOrder, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, COALESCE(order_id,''), type, device_id, COALESCE(runner,'') FROM commands WHERE order_id = ? OR batch_id = ?`, orderID, orderID)
	if err != nil {
		return PhoneOrder{}, false, err
	}
	n, match := 0, false
	for rows.Next() {
		var id int64
		var oid, typ, dev, runner string
		if err := rows.Scan(&id, &oid, &typ, &dev, &runner); err != nil {
			rows.Close()
			return PhoneOrder{}, false, err
		}
		n++
		match = oid == orderID && typ == RebootOrderType && dev == deviceID && runner == RunnerPhone
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PhoneOrder{}, false, err
	}
	if n == 0 {
		return PhoneOrder{}, false, nil
	}
	if n > 1 || !match {
		return PhoneOrder{}, false, ErrOrderIDInUse
	}
	orders, err := queryPhoneOrders(ctx, q, `order_id = ?`, orderID)
	if err != nil || len(orders) == 0 {
		return PhoneOrder{}, false, err
	}
	return orders[0], true, nil
}

// lastRebootOrder: la orden de reinicio más nueva del equipo que cumpla cond (nil si no hay).
func lastRebootOrder(ctx context.Context, q queryer, deviceID, cond string, args ...any) (*PhoneOrder, error) {
	orders, err := queryPhoneOrders(ctx, q, `type = 'reboot_router' AND device_id = ? AND (`+cond+`) ORDER BY id DESC LIMIT 1`,
		append([]any{deviceID}, args...)...)
	if err != nil || len(orders) == 0 {
		return nil, err
	}
	return &orders[0], nil
}

// ------------------------------------------------------------------ recomendación

// RouterHealth: salud de un router según el celular (estado en vivo,
// "routers": {"A": {...}}, §6.2). Las horas son del reloj del celular.
type RouterHealth struct {
	InternetOK *bool    `json:"internet_ok"`
	GatewayOK  *bool    `json:"gateway_ok"`
	LossPct    *float64 `json:"loss_pct"`
	RTTMs      *float64 `json:"rtt_ms"`
	CheckedAt  *string  `json:"checked_at"`
	DownSince  *string  `json:"down_since"`
	// Fresh: el estado del celular es reciente y esta verificación también.
	// Si es false, la salud se muestra pero no decide nada.
	Fresh bool `json:"fresh"`
}

// RebootInfo: bloque "reboot" de cada equipo en GET /devices y en los
// targets de GET /probes/{id} (§6.3). Solo lo llevan los equipos de una
// sonda de celular (los únicos que se pueden reiniciar desde el backend).
type RebootInfo struct {
	Recommended  bool    `json:"recommended"`
	Reason       string  `json:"reason"`
	OfflineSince *string `json:"offline_since"`
	OfflineMin   *int    `json:"offline_min"`

	LastRebootAt      *string `json:"last_reboot_at"`
	LastRebootStatus  *string `json:"last_reboot_status"`
	LastRebootOrderID *string `json:"last_reboot_order_id"`
	LastRebootStep    *string `json:"last_reboot_step"`
	LastRebootError   *string `json:"last_reboot_error"`
	InProgress        bool    `json:"in_progress"`
	CooldownUntil     *string `json:"cooldown_until"`

	ProbeID       string        `json:"probe_id"`
	Slot          string        `json:"slot"`
	AgentLastSeen *string       `json:"agent_last_seen"`
	PhoneOnline   bool          `json:"phone_online"`
	Health        *RouterHealth `json:"health"`
}

// rebootInputs: todo lo que decide la recomendación, ya leído de la base.
type rebootInputs struct {
	now       time.Time
	probeID   string
	slot      string
	agentLast *time.Time // último heartbeat del agente (nil = sin agente en agentLookback)

	phoneOnline bool          // hay estado del celular fresco (phoneHealthFresh)
	health      *RouterHealth // routers[slot] del estado del celular (nil = no vino)
	downFor     *time.Duration

	last    *PhoneOrder // último reinicio (no cancelado ni vencido sin ejecutar)
	counted *PhoneOrder // último reinicio que cuenta para el enfriamiento, dentro de él
	open    *PhoneOrder // reinicio en curso
}

// evalReboot aplica las reglas de §6.3. "Sin conexión" = heartbeat del
// agente más viejo que 10 min (si tiene agente) O el celular, con estado
// fresco, dice internet_ok=false desde hace más de 10 min. Nunca se
// recomienda con un reinicio en curso ni dentro del enfriamiento.
func evalReboot(in rebootInputs) RebootInfo {
	info := RebootInfo{ProbeID: in.probeID, Slot: in.slot, PhoneOnline: in.phoneOnline, Health: in.health}
	now := in.now
	mins := func(d time.Duration) int { return int(d / time.Minute) }

	var since *time.Time
	earliest := func(t time.Time) {
		if since == nil || t.Before(*since) {
			since = &t
		}
	}
	var parts []string
	offline := false // alguna señal lleva más de RebootOfflineAfter

	agentFresh, agentSilent := false, false
	if in.agentLast != nil {
		v := FormatTS(*in.agentLast)
		info.AgentLastSeen = &v
		age := now.Sub(*in.agentLast)
		if age > agentOfflineAfter {
			earliest(*in.agentLast)
			parts = append(parts, fmt.Sprintf("el agente del router no reporta hace %d min", mins(age)))
			if age > RebootOfflineAfter {
				offline, agentSilent = true, true
			}
		} else {
			agentFresh = true
		}
	}
	healthOK := in.health != nil && in.health.Fresh
	phoneDown := healthOK && in.health.InternetOK != nil && !*in.health.InternetOK
	if phoneDown {
		d := time.Duration(0)
		if in.downFor != nil {
			d = *in.downFor
		}
		earliest(now.Add(-d))
		parts = append(parts, fmt.Sprintf("el celular no ve Internet por este router hace %d min", mins(d)))
		if d > RebootOfflineAfter {
			offline = true
		}
	}
	if since != nil {
		v := FormatTS(*since)
		m := mins(now.Sub(*since))
		info.OfflineSince, info.OfflineMin = &v, &m
	}

	if in.last != nil {
		info.LastRebootAt = &in.last.CreatedAt
		info.LastRebootStatus = &in.last.Status
		info.LastRebootOrderID = &in.last.OrderID
		info.LastRebootStep, info.LastRebootError = in.last.Step, in.last.Error
	}
	if in.counted != nil {
		if t, err := ParseTS(rebootBase(in.counted)); err == nil && t.Add(RebootCooldown).After(now) {
			v := FormatTS(t.Add(RebootCooldown))
			info.CooldownUntil = &v
		}
	}

	var notes []string
	if healthOK && in.health.GatewayOK != nil && !*in.health.GatewayOK {
		notes = append(notes, "el router parece apagado (no responde en la LAN): el SSH probablemente falle")
	}
	if phoneDown && agentFresh {
		notes = append(notes, "el agente sí reporta: puede ser el cable o el MikroTik")
	}
	if agentSilent && healthOK && in.health.InternetOK != nil && *in.health.InternetOK {
		notes = append(notes, "el celular sí ve Internet por este router: puede ser solo el agente")
	}

	switch {
	case in.open != nil:
		info.InProgress = true
		r := "Reinicio en curso (" + in.open.Status
		if in.open.Step != nil && *in.open.Step != "" {
			r += ", " + *in.open.Step
		}
		info.Reason = r + ")"
		return info
	case info.CooldownUntil != nil:
		ago := 0
		if t, err := ParseTS(rebootBase(in.counted)); err == nil {
			ago = mins(now.Sub(t))
		}
		info.Reason = fmt.Sprintf("Reiniciado hace %d min: se espera a que vuelva (enfriamiento de %d min)", ago, mins(RebootCooldown))
		if since != nil {
			info.Reason += "; sigue sin conexión: " + strings.Join(parts, "; ")
		}
		return info
	case offline:
		info.Recommended = true
		info.Reason = fmt.Sprintf("Sin conexión hace %d min: %s", *info.OfflineMin, strings.Join(parts, "; "))
		if !in.phoneOnline {
			notes = append(notes, "el celular de la sonda no está en línea: la orden esperará a que vuelva")
		}
	case since != nil:
		info.Reason = fmt.Sprintf("Sin conexión hace %d min (se recomienda reiniciar pasados %d): %s",
			*info.OfflineMin, mins(RebootOfflineAfter), strings.Join(parts, "; "))
	case in.agentLast == nil && !healthOK:
		info.Reason = "Sin datos de conexión: el equipo no tiene agente y el celular no informa la salud de este router"
	default:
		info.Reason = "En línea"
	}
	if len(notes) > 0 {
		info.Reason += ". " + strings.Join(notes, "; ")
	}
	return info
}

// parseRouterHealth saca routers[slot] del estado en vivo del celular y
// calcula desde hace cuánto está caído sin mezclar relojes: (sent_at −
// down_since), ambas del celular, más la edad del estado en el servidor.
// Tipos raros cuentan como ausentes (el estado es un dato de campo).
func parseRouterHealth(raw json.RawMessage, slot string, statusAge time.Duration, now time.Time) (*RouterHealth, *time.Duration) {
	var st struct {
		SentAt  string                     `json:"sent_at"`
		Routers map[string]json.RawMessage `json:"routers"`
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.Routers == nil {
		return nil, nil
	}
	rawSlot, ok := st.Routers[slot]
	if !ok {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(rawSlot, &m); err != nil || m == nil {
		return nil, nil
	}
	h := &RouterHealth{}
	if b, ok := m["internet_ok"].(bool); ok {
		h.InternetOK = &b
	}
	if b, ok := m["gateway_ok"].(bool); ok {
		h.GatewayOK = &b
	}
	if f, ok := m["loss_pct"].(float64); ok {
		h.LossPct = &f
	}
	if f, ok := m["rtt_ms"].(float64); ok {
		h.RTTMs = &f
	}
	// Las horas se devuelven normalizadas (RFC 3339 UTC); una que no es hora
	// no se reenvía (el estado es texto libre del celular).
	var checked, down, sent *time.Time
	if v, ok := m["checked_at"].(string); ok && v != "" {
		if t, err := ParseTS(v); err == nil {
			checked = &t
			f := FormatTS(t)
			h.CheckedAt = &f
		}
	}
	if v, ok := m["down_since"].(string); ok && v != "" {
		if t, err := ParseTS(v); err == nil {
			down = &t
			f := FormatTS(t)
			h.DownSince = &f
		}
	}
	if t, err := ParseTS(st.SentAt); err == nil {
		sent = &t
	}

	// Fresco: estado reciente y verificación no demasiado vieja respecto
	// del envío (ambas horas del celular). Sin sent_at o checked_at no se
	// puede saber: se confía en la frescura del estado.
	h.Fresh = statusAge <= phoneHealthFresh
	if h.Fresh && sent != nil && checked != nil && sent.Sub(*checked) > healthCheckStale {
		h.Fresh = false
	}

	var downFor *time.Duration
	if down != nil {
		var d time.Duration
		if sent != nil {
			d = sent.Sub(*down) + statusAge
		} else {
			d = now.Sub(*down) // sin sent_at: se compara con la hora del servidor
		}
		if d < 0 {
			d = 0
		}
		downFor = &d
	}
	return h, downFor
}

// agentLastSeen: último heartbeat del agente del router (source=router) en
// agentLookback; nil si no hay.
func (s *Store) agentLastSeen(ctx context.Context, deviceID string, now time.Time) (*time.Time, error) {
	var last sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM heartbeats
WHERE device_id = ? AND received_at >= ? AND json_extract(raw, '$.source') = 'router'`,
		deviceID, FormatTS(now.Add(-agentLookback))).Scan(&last)
	if err != nil {
		return nil, err
	}
	if !last.Valid {
		return nil, nil
	}
	t, err := ParseTS(last.String)
	if err != nil {
		return nil, nil
	}
	return &t, nil
}

// rebootInfoFor arma el bloque reboot de un equipo de una sonda de celular.
func (s *Store) rebootInfoFor(ctx context.Context, deviceID, probeID, slot string, st *ProbeStatus, now time.Time) (*RebootInfo, error) {
	in := rebootInputs{now: now, probeID: probeID, slot: slot}
	var err error
	if in.agentLast, err = s.agentLastSeen(ctx, deviceID, now); err != nil {
		return nil, err
	}
	if st != nil && st.ReceivedAt != nil {
		if t, e := ParseTS(*st.ReceivedAt); e == nil {
			age := max(now.Sub(t), 0)
			in.phoneOnline = age <= phoneHealthFresh
			in.health, in.downFor = parseRouterHealth(st.Status, slot, age, now)
		}
	}
	// Último reinicio: el más nuevo que llegó a ejecutarse o intentarse (un
	// cancelado o vencido sin ejecutar no es un reinicio; su suerte se ve en
	// el historial de órdenes). Incluye los en curso y los fallidos.
	if in.last, err = lastRebootOrder(ctx, s.db, deviceID, `status NOT IN ('cancelled','expired')`); err != nil {
		return nil, err
	}
	if in.counted, err = lastRebootOrder(ctx, s.db, deviceID, rebootCountsCond+` AND `+rebootBaseCol+` >= ?`, FormatTS(now.Add(-RebootCooldown))); err != nil {
		return nil, err
	}
	if in.open, err = lastRebootOrder(ctx, s.db, deviceID, rebootOpenCond, FormatTS(now), FormatTS(now.Add(-phoneRunningStale))); err != nil {
		return nil, err
	}
	info := evalReboot(in)
	return &info, nil
}

// phoneProbeTarget: un equipo de una sonda de celular.
type phoneProbeTarget struct{ deviceID, probeID, slot string }

func (s *Store) phoneProbeTargets(ctx context.Context, probeID string) ([]phoneProbeTarget, error) {
	q := `SELECT t.device_id, t.probe_id, COALESCE(t.slot,'') FROM probe_targets t JOIN probes p ON p.probe_id = t.probe_id
WHERE p.runner = 'phone'`
	var args []any
	if probeID != "" {
		q += ` AND t.probe_id = ?`
		args = append(args, probeID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY t.device_id, t.enabled DESC, p.enabled DESC, t.probe_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []phoneProbeTarget
	for rows.Next() {
		var t phoneProbeTarget
		if err := rows.Scan(&t.deviceID, &t.probeID, &t.slot); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RebootInfos: bloque reboot de cada equipo que esté en una sonda de celular
// (device_id → info). Si un equipo estuviera en varias, manda la sonda en la
// que está activo (la misma que elige POST /devices/{id}/reboot).
func (s *Store) RebootInfos(ctx context.Context, now time.Time) (map[string]*RebootInfo, error) {
	return s.rebootInfos(ctx, "", now)
}

// ProbeRebootInfos: lo mismo, solo para los equipos de una sonda (y con
// esa sonda, aunque el equipo esté también en otra).
func (s *Store) ProbeRebootInfos(ctx context.Context, probeID string, now time.Time) (map[string]*RebootInfo, error) {
	return s.rebootInfos(ctx, probeID, now)
}

func (s *Store) rebootInfos(ctx context.Context, probeID string, now time.Time) (map[string]*RebootInfo, error) {
	now = now.UTC()
	targets, err := s.phoneProbeTargets(ctx, probeID)
	if err != nil {
		return nil, err
	}
	out := map[string]*RebootInfo{}
	if len(targets) == 0 {
		return out, nil
	}
	statuses, err := s.probeStatuses(ctx, probeID, now)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if _, done := out[t.deviceID]; done || t.slot == "" {
			continue
		}
		var st *ProbeStatus
		if v, ok := statuses[t.probeID]; ok {
			st = &v
		}
		info, err := s.rebootInfoFor(ctx, t.deviceID, t.probeID, t.slot, st, now)
		if err != nil {
			return nil, fmt.Errorf("recomendación de reinicio de %s: %w", t.deviceID, err)
		}
		out[t.deviceID] = info
	}
	return out, nil
}
