// Package store persiste mediciones y heartbeats del módem Notion 5G en SQLite.
//
// Diseño: cada medición llega como un JSON "libre" (el mismo dict que ya produce
// scripts/notion5g.py, o lo que mande la futura app Android/Flutter). Guardamos el
// JSON completo tal cual (columna raw) y además extraemos a columnas indexadas los
// campos que usamos para filtrar/agregar. Así el esquema no se rompe si el cliente
// manda campos nuevos.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // sqlite + WAL: un writer a la vez evita "database is locked"
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS measurements (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at   TEXT NOT NULL,
	device_id     TEXT NOT NULL,
	source        TEXT,
	tag           TEXT,
	ts            TEXT,
	operator      TEXT,
	rat           TEXT,
	band_lte      INTEGER,
	nr_band       INTEGER,
	pci           INTEGER,
	rsrp_dbm      REAL,
	rsrq_db       REAL,
	sinr_db       REAL,
	rssi_dbm      REAL,
	eps_reg       INTEGER,
	nr_reg        INTEGER,
	lat           REAL,
	lon           REAL,
	gps_accuracy_m REAL,
	gps_source    TEXT,
	uptime_s      REAL,
	down_mbps     REAL,
	up_mbps       REAL,
	ping_ms       REAL,
	jitter_ms     REAL,
	via_modem     INTEGER,
	note          TEXT,
	lat_end        REAL,
	lon_end        REAL,
	gps_accuracy_m_end REAL,
	gps_source_end TEXT,
	net_route     TEXT,
	net_asn       INTEGER,
	net_confidence TEXT,
	raw           TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_meas_device_ts ON measurements(device_id, ts);
CREATE INDEX IF NOT EXISTS idx_meas_operator ON measurements(operator);
CREATE INDEX IF NOT EXISTS idx_meas_tag ON measurements(tag);
CREATE INDEX IF NOT EXISTS idx_meas_received ON measurements(received_at);

CREATE TABLE IF NOT EXISTS commands (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at     TEXT NOT NULL,
	device_id      TEXT NOT NULL,
	type           TEXT NOT NULL,
	duration_s     INTEGER,
	lat            REAL,
	lon            REAL,
	gps_accuracy_m REAL,
	gps_source     TEXT,
	requested_by   TEXT,
	status         TEXT NOT NULL DEFAULT 'pending', -- pending | claimed | done | failed
	claimed_at     TEXT,
	completed_at   TEXT,
	error          TEXT,
	measurement_id INTEGER
);
CREATE INDEX IF NOT EXISTS idx_cmd_device_status ON commands(device_id, status, id);

CREATE TABLE IF NOT EXISTS heartbeats (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at    TEXT NOT NULL,
	device_id      TEXT NOT NULL,
	ts             TEXT,
	uptime_s       REAL,
	operator       TEXT,
	rat            TEXT,
	rsrp_dbm       REAL,
	ping_ms        REAL,
	loss_pct       REAL,
	lat            REAL,
	lon            REAL,
	gps_accuracy_m REAL,
	gps_source     TEXT,
	raw            TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hb_device_ts ON heartbeats(device_id, ts);

-- Última ubicación conocida de cada device_id, reportada por el celular
-- acompañante (el módem no tiene GPS propio). Se usa para rellenar lat/lon en
-- mediciones/heartbeats del router que lleguen sin ubicación -- perfil de
-- ping+ubicación mientras el equipo va en movimiento con el celular al lado.
CREATE TABLE IF NOT EXISTS device_locations (
	device_id      TEXT PRIMARY KEY,
	lat            REAL NOT NULL,
	lon            REAL NOT NULL,
	gps_accuracy_m REAL,
	gps_source     TEXT,
	updated_at     TEXT NOT NULL
);

-- Última IP pública desde la que se vio salir a cada equipo, observada por el
-- backend en las peticiones que el propio agente ya hace por el túnel
-- (heartbeat y cierre de comando). Es la REFERENCIA contra la que se compara
-- la IP del navegador que corre una prueba: si coinciden, esa prueba salió por
-- el router; si no, salió por otra red (la SIM del celular, otro WiFi).
-- No requiere ningún cambio en el agente del router.
--
-- Tabla NUEVA, así que acá "CREATE TABLE IF NOT EXISTS" sí alcanza: el gotcha
-- de más abajo (columnas que no se agregan solas) aplica a tablas que ya
-- existían en producción, no a una que se crea entera por primera vez.
CREATE TABLE IF NOT EXISTS device_egress (
	device_id   TEXT PRIMARY KEY,
	ip          TEXT NOT NULL,
	ip_family   TEXT NOT NULL,   -- "v4" | "v6"
	asn         INTEGER,
	asn_name    TEXT,
	country     TEXT,
	updated_at  TEXT NOT NULL
);

-- Historial de lo que manda el celular acompañante junto con cada ubicación
-- (batería, carga, red que usa). device_locations guarda solo el último valor;
-- esto es append-only para poder ver el consumo de batería en el tiempo.
CREATE TABLE IF NOT EXISTS phone_log (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id      TEXT NOT NULL,
	received_at    TEXT NOT NULL,
	lat            REAL,
	lon            REAL,
	gps_accuracy_m REAL,
	battery_pct    INTEGER,
	charging       INTEGER,
	-- estado real que reporta Android, independiente de si hay algo enchufado:
	-- con un adaptador USB-Ethernet el teléfono puede estar "usb" y descargándose
	battery_status TEXT,     -- "charging" | "discharging" | "full" | "not_charging" | "unknown"
	plugged        TEXT,     -- "ac" | "usb" | "wireless" | "dock" | "none"
	battery_temp_c REAL,
	net_type       TEXT      -- "ethernet" | "wifi" | "cellular" | "none" | ...
);
CREATE INDEX IF NOT EXISTS idx_phone_log_device ON phone_log(device_id, id);
`)
	if err != nil {
		return err
	}
	// "CREATE TABLE IF NOT EXISTS" no le agrega columnas a una tabla que ya
	// existía de un despliegue anterior (pasó de verdad: producción tenía la
	// tabla heartbeats vieja sin ping_ms/lat/lon y el primer heartbeat después
	// de este cambio tronó con "no such column"). Sin un framework de
	// migraciones, el idiom estándar en SQLite es intentar el ALTER TABLE y
	// tragarse el error si la columna ya existe.
	for _, col := range []string{
		"ping_ms REAL", "loss_pct REAL", "lat REAL", "lon REAL",
		"gps_accuracy_m REAL", "gps_source TEXT",
	} {
		if err := s.addColumnIfMissing(ctx, "heartbeats", col); err != nil {
			return err
		}
	}
	// Ubicación de FIN de la prueba de velocidad (la de arranque ya vivía en
	// lat/lon/gps_accuracy_m/gps_source, sin tocar esas columnas ni los datos
	// ya guardados -- ver SetMeasurementEndLocation). Mismo idiom que arriba:
	// la tabla measurements ya existía en producción antes de este cambio.
	//
	// net_route/net_asn/net_confidence (ruta de salida por la que se midió y
	// qué tan seguro está el servidor de eso) entran por el mismo camino y por
	// la misma razón: la tabla measurements ya existe en producción, así que el
	// CREATE TABLE de arriba NO las agrega.
	for _, col := range []string{
		"lat_end REAL", "lon_end REAL", "gps_accuracy_m_end REAL", "gps_source_end TEXT",
		"net_route TEXT", "net_asn INTEGER", "net_confidence TEXT",
	} {
		if err := s.addColumnIfMissing(ctx, "measurements", col); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing corre "ALTER TABLE table ADD COLUMN colDef" e ignora el
// error si la columna ya existe (SQLite no tiene "ADD COLUMN IF NOT EXISTS").
func (s *Store) addColumnIfMissing(ctx context.Context, table, colDef string) error {
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, colDef))
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrar %s.%s: %w", table, colDef, err)
	}
	return nil
}

// Measurement es la vista tipada de los campos que indexamos. Todo lo demás
// (cualquier clave adicional del JSON de entrada) se preserva en Raw y se
// devuelve intacto en las consultas.
type Measurement struct {
	DeviceID     string   `json:"device_id"`
	Source       string   `json:"source,omitempty"` // "pc" | "android" | "router" | "browser"
	Tag          string   `json:"tag,omitempty"`
	TS           string   `json:"ts,omitempty"`
	Operator     string   `json:"operator,omitempty"`
	RAT          string   `json:"rat,omitempty"`
	BandLTE      *int     `json:"band_lte,omitempty"`
	NRBand       *int     `json:"nr_band,omitempty"`
	PCI          *int     `json:"pci,omitempty"`
	RSRPDbm      *float64 `json:"rsrp_dbm,omitempty"`
	RSRQDb       *float64 `json:"rsrq_db,omitempty"`
	SINRDb       *float64 `json:"sinr_db,omitempty"`
	RSSIDbm      *float64 `json:"rssi_dbm,omitempty"`
	EPSReg       *int     `json:"eps_reg,omitempty"`
	NRReg        *int     `json:"nr_reg,omitempty"`
	Lat          *float64 `json:"lat,omitempty"`
	Lon          *float64 `json:"lon,omitempty"`
	GPSAccuracyM *float64 `json:"gps_accuracy_m,omitempty"`
	GPSSource    string   `json:"gps_source,omitempty"`
	UptimeS      *float64 `json:"uptime_s,omitempty"`
	DownMbps     *float64 `json:"down_mbps,omitempty"`
	UpMbps       *float64 `json:"up_mbps,omitempty"`
	PingMs       *float64 `json:"ping_ms,omitempty"`
	JitterMs     *float64 `json:"jitter_ms,omitempty"`
	ViaModem     *bool    `json:"via_modem,omitempty"`
	Note         string   `json:"note,omitempty"`
	// Ubicación al TERMINAR la prueba de velocidad, además de Lat/Lon (que es
	// la de ARRANQUE, sin cambiar de significado). Solo aplica a mediciones
	// que vinieron de un comando run_speedtest disparado desde el dashboard;
	// se rellena después, con SetMeasurementEndLocation -- no llega en el
	// insert inicial. Ver §"prueba de velocidad" en app.js.
	LatEnd          *float64 `json:"lat_end,omitempty"`
	LonEnd          *float64 `json:"lon_end,omitempty"`
	GPSAccuracyMEnd *float64 `json:"gps_accuracy_m_end,omitempty"`
	GPSSourceEnd    string   `json:"gps_source_end,omitempty"`
	// OJO: net_route / net_asn / net_confidence NO están en este struct A
	// PROPÓSITO. Este struct se desserializa del JSON que manda el cliente, y
	// la ruta de salida la decide SIEMPRE el servidor: tenerla acá hacía que
	// bastara con mandar {"net_route":"router"} en el POST para que la columna
	// quedara escrita con lo que dijo el cliente (y la X-API-Key está en el JS
	// del dashboard, así que alcanza con tener el link). Esas tres columnas
	// entran por NetClassification, que es un argumento explícito de
	// insertMeasurement, no un campo del payload. Ver InsertClassified.
}

// NetClassification es lo que el SERVIDOR decidió sobre la ruta de salida de
// una medición. Va como argumento explícito justamente para que no pueda venir
// del JSON del cliente: el único camino a estas columnas es el clasificador
// (internal/api/netclass.go) o SetMeasurementNet.
type NetClassification struct {
	Route      string // "" (sin clasificar) | "router" | "other-network" | "unknown"
	Confidence string // "" | "high" | "medium" | "low"
	ASN        *int
}

// netRoutes / netConfidences: valores aceptados en las columnas. Segunda
// barrera, además de borrarle las claves reservadas al payload: si algún día
// otro camino llega hasta acá con basura (o con un "router" inventado en un
// backfill), la columna queda NULL en vez de mentir. Una fila sin clasificar
// es un dato honesto; una fila que dice "router" sin que nadie lo haya
// verificado, no.
var netRoutes = map[string]bool{"router": true, "other-network": true, "unknown": true}
var netConfidences = map[string]bool{"high": true, "medium": true, "low": true}

// clean deja la clasificación en valores conocidos. Una ruta inválida anula
// también la confianza: "alta" sin ruta no significa nada.
func (nc NetClassification) clean() NetClassification {
	if !netRoutes[nc.Route] {
		return NetClassification{}
	}
	if !netConfidences[nc.Confidence] {
		nc.Confidence = ""
	}
	if nc.ASN != nil && *nc.ASN <= 0 {
		nc.ASN = nil
	}
	return nc
}

// maxLocationAge: qué tan vieja puede ser la última ubicación reportada por el
// celular para seguirla usando en un reporte del router que llegue sin GPS
// propio. Pasado esto, mejor guardar sin ubicación que pegarle una vieja y
// engañosa (el equipo pudo haberse movido varios km en ese tiempo).
const maxLocationAge = 10 * time.Minute

// SetDeviceLocation guarda/actualiza la última ubicación conocida de un
// device_id (la manda el celular acompañante, ver POST /api/v1/devices/{id}/location).
func (s *Store) SetDeviceLocation(ctx context.Context, deviceID string, lat, lon float64, accuracyM *float64, source string) error {
	if deviceID == "" {
		return fmt.Errorf("falta device_id")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO device_locations (device_id, lat, lon, gps_accuracy_m, gps_source, updated_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(device_id) DO UPDATE SET
	lat=excluded.lat, lon=excluded.lon, gps_accuracy_m=excluded.gps_accuracy_m,
	gps_source=excluded.gps_source, updated_at=excluded.updated_at`,
		deviceID, lat, lon, accuracyM, nullStr(source), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("guardar ubicación: %w", err)
	}
	return nil
}

// PhoneLogEntry es una fila de phone_log: el estado del celular acompañante
// en el momento en que mandó una ubicación. Todo es opcional salvo device_id:
// una app vieja manda solo la ubicación.
type PhoneLogEntry struct {
	ID            int64    `json:"id"`
	DeviceID      string   `json:"device_id"`
	ReceivedAt    string   `json:"received_at"`
	Lat           *float64 `json:"lat"`
	Lon           *float64 `json:"lon"`
	GPSAccuracyM  *float64 `json:"gps_accuracy_m"`
	BatteryPct    *int     `json:"battery_pct"`
	Charging      *bool    `json:"charging"`
	BatteryStatus string   `json:"battery_status,omitempty"`
	Plugged       string   `json:"plugged,omitempty"`
	BatteryTempC  *float64 `json:"battery_temp_c"`
	NetType       string   `json:"net_type,omitempty"`
}

func (s *Store) AppendPhoneLog(ctx context.Context, e PhoneLogEntry) error {
	if e.DeviceID == "" {
		return fmt.Errorf("falta device_id")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO phone_log (device_id, received_at, lat, lon, gps_accuracy_m, battery_pct, charging, battery_status, plugged, battery_temp_c, net_type)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.DeviceID, time.Now().UTC().Format(time.RFC3339), e.Lat, e.Lon, e.GPSAccuracyM,
		e.BatteryPct, boolToInt(e.Charging), nullStr(e.BatteryStatus), nullStr(e.Plugged), e.BatteryTempC, nullStr(e.NetType))
	if err != nil {
		return fmt.Errorf("guardar log del celular: %w", err)
	}
	return nil
}

// ListPhoneLog devuelve las últimas filas del celular de deviceID, más nuevas primero.
func (s *Store) ListPhoneLog(ctx context.Context, deviceID string, limit int) ([]PhoneLogEntry, error) {
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, device_id, received_at, lat, lon, gps_accuracy_m, battery_pct, charging, battery_status, plugged, battery_temp_c, net_type
FROM phone_log WHERE device_id = ? ORDER BY id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PhoneLogEntry{}
	for rows.Next() {
		var e PhoneLogEntry
		var lat, lon, acc, temp sql.NullFloat64
		var batt, charging sql.NullInt64
		var status, plugged, netType sql.NullString
		if err := rows.Scan(&e.ID, &e.DeviceID, &e.ReceivedAt, &lat, &lon, &acc, &batt, &charging, &status, &plugged, &temp, &netType); err != nil {
			return nil, err
		}
		if lat.Valid {
			e.Lat = &lat.Float64
		}
		if lon.Valid {
			e.Lon = &lon.Float64
		}
		if acc.Valid {
			e.GPSAccuracyM = &acc.Float64
		}
		if batt.Valid {
			v := int(batt.Int64)
			e.BatteryPct = &v
		}
		if charging.Valid {
			v := charging.Int64 != 0
			e.Charging = &v
		}
		if temp.Valid {
			e.BatteryTempC = &temp.Float64
		}
		e.BatteryStatus = status.String
		e.Plugged = plugged.String
		e.NetType = netType.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// cachedLocation devuelve la última ubicación conocida de deviceID si no está
// más vieja que maxLocationAge, o ok=false si no hay ninguna (útil) todavía.
func (s *Store) cachedLocation(ctx context.Context, deviceID string) (lat, lon float64, accuracyM *float64, source string, ok bool) {
	row := s.db.QueryRowContext(ctx,
		`SELECT lat, lon, gps_accuracy_m, gps_source, updated_at FROM device_locations WHERE device_id = ?`, deviceID)
	var acc sql.NullFloat64
	var src sql.NullString
	var updatedAt string
	if err := row.Scan(&lat, &lon, &acc, &src, &updatedAt); err != nil {
		return 0, 0, nil, "", false
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil || time.Since(t) > maxLocationAge {
		return 0, 0, nil, "", false
	}
	if acc.Valid {
		accuracyM = &acc.Float64
	}
	return lat, lon, accuracyM, src.String, true
}

// ------------------------------------------------------------------ ruta de salida
//
// Para saber si una prueba corrida desde el navegador salió por el router o por
// otra red (la SIM del celular, otro WiFi) hace falta una REFERENCIA: desde qué
// IP pública se ve salir al equipo. Se aprende sola, de las peticiones que el
// agente del router ya hace por el túnel (heartbeat y cierre de comando), sin
// tocar nada en el equipo de campo.

// maxEgressAge: qué tan vieja puede ser la última IP pública vista de un equipo
// para seguir usándola como referencia. Mismo criterio que maxLocationAge: el
// agente reporta cada 60s, así que 10 min ya significa que el equipo dejó de
// reportar y la IP pudo haber rotado (CGNAT móvil). Comparar contra una IP
// vieja daría "otra red" para una prueba que sí salió por el router.
const maxEgressAge = 10 * time.Minute

// MaxEgressAge expone maxEgressAge al paquete api, que necesita exactamente la
// misma frontera para decidir si la referencia sirve o ya está vieja (y para
// poder reportar la edad, por eso DeviceEgressOf no filtra por frescura).
const MaxEgressAge = maxEgressAge

// DeviceEgress es la última IP pública desde la que se vio salir a un equipo.
// A diferencia de lo que se guarda de un celular (solo el prefijo), acá va la
// dirección exacta a propósito: es la IP del CPE bajo prueba, no la de una
// persona, es el discriminador de toda la clasificación, y es un último-valor
// que se sobreescribe cada minuto, no un archivo histórico.
type DeviceEgress struct {
	DeviceID  string
	IP        string // dirección exacta ya normalizada (Unmap, sin zona)
	Family    string // "v4" | "v6"
	ASN       *int
	ASNName   string
	Country   string
	UpdatedAt time.Time
}

// SetDeviceEgress guarda la última IP pública desde la que se vio salir al
// equipo. Si la IP cambió respecto de la guardada, invalida asn/asn_name: el
// ASN viejo puede no corresponder a la IP nueva (rotación de CGNAT), y un ASN
// equivocado en la referencia es peor que no tenerlo -- haría que una prueba
// hecha por el router se marque como "otra red".
func (s *Store) SetDeviceEgress(ctx context.Context, deviceID, ip, family, country string) error {
	if deviceID == "" {
		return fmt.Errorf("falta device_id")
	}
	if ip == "" || family == "" {
		return fmt.Errorf("falta ip/familia")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO device_egress (device_id, ip, ip_family, asn, asn_name, country, updated_at)
VALUES (?,?,?,NULL,NULL,?,?)
ON CONFLICT(device_id) DO UPDATE SET
	ip=excluded.ip, ip_family=excluded.ip_family, country=excluded.country,
	updated_at=excluded.updated_at,
	asn      = CASE WHEN excluded.ip <> device_egress.ip THEN NULL ELSE device_egress.asn END,
	asn_name = CASE WHEN excluded.ip <> device_egress.ip THEN NULL ELSE device_egress.asn_name END`,
		deviceID, ip, family, nullStr(country), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("guardar ip de salida: %w", err)
	}
	return nil
}

// DeviceEgressOf devuelve la referencia guardada; ok=false si no hay ninguna.
// La frescura NO se decide acá a propósito: el llamador necesita la edad para
// poder reportarla ("el equipo no reporta hace más de 10 minutos" es un
// diagnóstico útil, "no hay referencia" no lo es).
func (s *Store) DeviceEgressOf(ctx context.Context, deviceID string) (DeviceEgress, bool) {
	if deviceID == "" {
		return DeviceEgress{}, false
	}
	row := s.db.QueryRowContext(ctx, `
SELECT ip, ip_family, asn, asn_name, country, updated_at FROM device_egress WHERE device_id = ?`, deviceID)
	var e DeviceEgress
	var asn sql.NullInt64
	var asnName, country sql.NullString
	var updatedAt string
	if err := row.Scan(&e.IP, &e.Family, &asn, &asnName, &country, &updatedAt); err != nil {
		return DeviceEgress{}, false
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return DeviceEgress{}, false
	}
	e.DeviceID = deviceID
	if asn.Valid && asn.Int64 > 0 {
		v := int(asn.Int64)
		e.ASN = &v
	}
	e.ASNName = asnName.String
	e.Country = country.String
	e.UpdatedAt = t
	return e, true
}

// SetDeviceEgressASN completa el ASN de la referencia, solo si la IP guardada
// sigue siendo la misma que se resolvió. Si rotó mientras corría la consulta
// DNS, el resultado ya no aplica y escribirlo sería peor que no escribir nada.
func (s *Store) SetDeviceEgressASN(ctx context.Context, deviceID, ip string, asn int, name string) error {
	if deviceID == "" || ip == "" || asn <= 0 {
		return fmt.Errorf("faltan datos para guardar el asn de salida")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE device_egress SET asn=?, asn_name=? WHERE device_id=? AND ip=?`,
		asn, nullStr(name), deviceID, ip)
	if err != nil {
		return fmt.Errorf("guardar asn de salida: %w", err)
	}
	return nil
}

// SetMeasurementNet reescribe la clasificación de una medición ya insertada.
// La llama SOLO el worker asíncrono de ASN (y el cierre de comando), nunca el
// camino del POST: resolver un ASN es un round trip de DNS y la ingesta no
// puede depender de eso.
//
// Usa overwriteRawFields, no mergeIntoRaw: estos campos los pone SIEMPRE el
// servidor (al cliente se le borran del payload antes de insertar), así que
// pisar lo que haya es lo correcto -- lo que hay es la clasificación previa,
// hecha con una señal menos.
func (s *Store) SetMeasurementNet(ctx context.Context, id int64, nc NetClassification, rawFields map[string]any) error {
	nc = nc.clean()
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT raw FROM measurements WHERE id = ?`, id).Scan(&raw); err != nil {
		return fmt.Errorf("leer medición %d: %w", id, err)
	}
	merged, err := overwriteRawFields(json.RawMessage(raw), rawFields)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE measurements SET net_route=?, net_asn=?, net_confidence=?, raw=? WHERE id=?`,
		nullStr(nc.Route), nc.ASN, nullStr(nc.Confidence), string(merged), id); err != nil {
		return fmt.Errorf("guardar clasificación de red: %w", err)
	}
	return nil
}

// mergeIntoRaw agrega/completa campos en un JSON crudo sin pisar los que ya
// traiga (mismo criterio en todos lados: lo que reportó el dispositivo manda).
// Se usa para que las columnas indexadas y el JSON guardado en `raw` nunca se
// desincronicen cuando el servidor rellena algo (ubicación desde caché, etc.).
func mergeIntoRaw(raw json.RawMessage, fields map[string]any) (json.RawMessage, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("payload inválido: %w", err)
	}
	for k, v := range fields {
		if v == nil {
			continue
		}
		if _, exists := obj[k]; !exists {
			obj[k] = v
		}
	}
	return json.Marshal(obj)
}

// overwriteRawFields agrega/PISA campos en un JSON crudo (a diferencia de
// mergeIntoRaw, que solo rellena lo que falta). Se usa nada más para
// lat_end/lon_end/etc: esos campos nunca los manda el dispositivo, siempre
// los pone el servidor después, así que no hay riesgo de pisar algo real; si
// este endpoint se llama dos veces (reintento del navegador), gana la
// ubicación más nueva en vez de quedarse pegado con la primera.
func overwriteRawFields(raw json.RawMessage, fields map[string]any) (json.RawMessage, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("payload inválido: %w", err)
	}
	for k, v := range fields {
		if v == nil {
			continue
		}
		obj[k] = v
	}
	return json.Marshal(obj)
}

// SetMeasurementEndLocation guarda la ubicación del navegador al TERMINAR una
// prueba de velocidad -- la de ARRANQUE ya viaja en el propio comando
// (CreateCommand/CompleteCommand, columnas lat/lon de siempre, sin tocar). Es
// útil porque el equipo se puede haber movido varios metros/cuadras durante
// los segundos que dura la prueba. Se guarda contra measurement_id (la
// medición que generó el comando, ya seteada por CompleteCommand cuando el
// router reportó el resultado), no contra el comando en sí -- si todavía no
// hay medición asociada (el router no terminó/no reportó), devuelve error y
// el caller decide si reintentar; no hay nada que escribir todavía.
// No pisa ningún valor ya registrado: lat_end/lon_end/etc son columnas
// nuevas y separadas de lat/lon, que quedan intactas para toda medición
// vieja y nueva.
func (s *Store) SetMeasurementEndLocation(ctx context.Context, commandID int64, lat, lon float64, accuracyM *float64, source string) error {
	row := s.db.QueryRowContext(ctx, `SELECT measurement_id FROM commands WHERE id = ?`, commandID)
	var measurementID sql.NullInt64
	if err := row.Scan(&measurementID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("comando %d no existe", commandID)
		}
		return fmt.Errorf("leer comando: %w", err)
	}
	if !measurementID.Valid {
		return fmt.Errorf("el comando %d todavía no tiene una medición asociada", commandID)
	}

	rawRow := s.db.QueryRowContext(ctx, `SELECT raw FROM measurements WHERE id = ?`, measurementID.Int64)
	var raw string
	if err := rawRow.Scan(&raw); err != nil {
		return fmt.Errorf("leer medición %d: %w", measurementID.Int64, err)
	}
	merged, err := overwriteRawFields(json.RawMessage(raw), map[string]any{
		"lat_end": lat, "lon_end": lon, "gps_accuracy_m_end": accuracyM, "gps_source_end": nullStr(source),
	})
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
UPDATE measurements SET lat_end=?, lon_end=?, gps_accuracy_m_end=?, gps_source_end=?, raw=? WHERE id=?`,
		lat, lon, accuracyM, nullStr(source), string(merged), measurementID.Int64)
	if err != nil {
		return fmt.Errorf("guardar ubicación de fin: %w", err)
	}
	return nil
}

// InsertRaw guarda un payload JSON arbitrario: extrae los campos conocidos a
// columnas indexadas y conserva el objeto completo en raw. La fila queda SIN
// clasificar (net_route NULL), que es lo correcto para todo lo que no pasó por
// el clasificador: notion5g.py, el agente del router y las mediciones que
// entran al cerrar un comando (esas las clasifica después noteDeviceMeasurement).
func (s *Store) InsertRaw(ctx context.Context, raw json.RawMessage) (int64, error) {
	return s.insertMeasurement(ctx, raw, nil, NetClassification{})
}

// InsertClassified es InsertRaw con la ruta de salida que calculó el servidor.
// Es el ÚNICO camino por el que se escriben net_route/net_asn/net_confidence en
// un insert, y el valor llega por parámetro, nunca desde el JSON del cliente.
func (s *Store) InsertClassified(ctx context.Context, raw json.RawMessage, nc NetClassification) (int64, error) {
	return s.insertMeasurement(ctx, raw, nil, nc)
}

// insertMeasurement es InsertRaw con un merge opcional de campos (usado por
// CompleteCommand para inyectar la ubicación que mandó el celular en una
// medición que reportó el router, que no tiene GPS propio) y con la
// clasificación de red que decidió el servidor.
func (s *Store) insertMeasurement(ctx context.Context, raw json.RawMessage, mergeFields map[string]any, nc NetClassification) (int64, error) {
	nc = nc.clean()
	if len(mergeFields) > 0 {
		merged, err := mergeIntoRaw(raw, mergeFields)
		if err != nil {
			return 0, err
		}
		raw = merged
	}
	var m Measurement
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("payload inválido: %w", err)
	}
	if m.DeviceID == "" {
		return 0, fmt.Errorf("falta device_id")
	}
	if m.Lat == nil || m.Lon == nil {
		if lat, lon, acc, src, ok := s.cachedLocation(ctx, m.DeviceID); ok {
			m.Lat, m.Lon, m.GPSAccuracyM, m.GPSSource = &lat, &lon, acc, src
			// el JSON crudo también debe reflejar la ubicación rellenada, o
			// ListRaw (que devuelve raw, no las columnas indexadas) mentiría
			merged, err := mergeIntoRaw(raw, map[string]any{
				"lat": lat, "lon": lon, "gps_accuracy_m": acc, "gps_source": src,
			})
			if err != nil {
				return 0, err
			}
			raw = merged
		}
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO measurements (received_at, device_id, source, tag, ts, operator, rat, band_lte, nr_band, pci,
	rsrp_dbm, rsrq_db, sinr_db, rssi_dbm, eps_reg, nr_reg, lat, lon, gps_accuracy_m, gps_source, uptime_s,
	down_mbps, up_mbps, ping_ms, jitter_ms, via_modem, note, lat_end, lon_end, gps_accuracy_m_end, gps_source_end,
	net_route, net_asn, net_confidence, raw)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), m.DeviceID, nullStr(m.Source), nullStr(m.Tag), nullStr(m.TS),
		nullStr(m.Operator), nullStr(m.RAT), m.BandLTE, m.NRBand, m.PCI, m.RSRPDbm, m.RSRQDb, m.SINRDb, m.RSSIDbm,
		m.EPSReg, m.NRReg, m.Lat, m.Lon, m.GPSAccuracyM, nullStr(m.GPSSource), m.UptimeS, m.DownMbps, m.UpMbps,
		m.PingMs, m.JitterMs, boolToInt(m.ViaModem), nullStr(m.Note),
		m.LatEnd, m.LonEnd, m.GPSAccuracyMEnd, nullStr(m.GPSSourceEnd),
		nullStr(nc.Route), nc.ASN, nullStr(nc.Confidence), string(raw))
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	return res.LastInsertId()
}

// Heartbeat es el ping liviano y frecuente del agente del router: señal +
// uptime + (si el equipo va en movimiento) latencia y ubicación. Es la unidad
// básica del "perfil de ping con ubicación" -- mucho más barato que una
// medición completa, pensado para correr cada 1 minuto sin gastar datos.
type Heartbeat struct {
	DeviceID     string   `json:"device_id"`
	TS           string   `json:"ts"`
	UptimeS      *float64 `json:"uptime_s"`
	Operator     string   `json:"operator"`
	RAT          string   `json:"rat"`
	RSRPDbm      *float64 `json:"rsrp_dbm"`
	PingMs       *float64 `json:"ping_ms,omitempty"`
	LossPct      *float64 `json:"loss_pct,omitempty"`
	Lat          *float64 `json:"lat,omitempty"`
	Lon          *float64 `json:"lon,omitempty"`
	GPSAccuracyM *float64 `json:"gps_accuracy_m,omitempty"`
	GPSSource    string   `json:"gps_source,omitempty"`
}

func (s *Store) InsertHeartbeat(ctx context.Context, raw json.RawMessage) (int64, error) {
	var h Heartbeat
	if err := json.Unmarshal(raw, &h); err != nil {
		return 0, fmt.Errorf("payload inválido: %w", err)
	}
	if h.DeviceID == "" {
		return 0, fmt.Errorf("falta device_id")
	}
	if h.Lat == nil || h.Lon == nil {
		if lat, lon, acc, src, ok := s.cachedLocation(ctx, h.DeviceID); ok {
			h.Lat, h.Lon, h.GPSAccuracyM, h.GPSSource = &lat, &lon, acc, src
			merged, err := mergeIntoRaw(raw, map[string]any{
				"lat": lat, "lon": lon, "gps_accuracy_m": acc, "gps_source": src,
			})
			if err != nil {
				return 0, err
			}
			raw = merged
		}
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO heartbeats (received_at, device_id, ts, uptime_s, operator, rat, rsrp_dbm, ping_ms, loss_pct,
	lat, lon, gps_accuracy_m, gps_source, raw)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), h.DeviceID, nullStr(h.TS), h.UptimeS, nullStr(h.Operator),
		nullStr(h.RAT), h.RSRPDbm, h.PingMs, h.LossPct, h.Lat, h.Lon, h.GPSAccuracyM, nullStr(h.GPSSource), string(raw))
	if err != nil {
		return 0, fmt.Errorf("insert heartbeat: %w", err)
	}
	return res.LastInsertId()
}

type ListFilter struct {
	DeviceID string
	Tag      string
	Operator string
	Since    string // RFC3339; vacío = sin filtro
	// NetRoute filtra por ruta de salida: "router" | "other-network" |
	// "unknown", o "none" para ver SOLO las que no tienen clasificación
	// (todo lo anterior a esta función, útil para auditar el histórico).
	// Vacío = sin filtro.
	NetRoute string
	Limit    int
}

// ListRaw devuelve las filas como el JSON crudo que se guardó (más id/received_at),
// más reciente primero.
func (s *Store) ListRaw(ctx context.Context, f ListFilter) ([]json.RawMessage, error) {
	if f.Limit <= 0 || f.Limit > 5000 {
		f.Limit = 100
	}
	q := "SELECT id, received_at, raw FROM measurements WHERE 1=1"
	var args []any
	if f.DeviceID != "" {
		q += " AND device_id = ?"
		args = append(args, f.DeviceID)
	}
	if f.Tag != "" {
		q += " AND tag = ?"
		args = append(args, f.Tag)
	}
	if f.Operator != "" {
		q += " AND operator = ?"
		args = append(args, f.Operator)
	}
	if f.Since != "" {
		q += " AND ts >= ?"
		args = append(args, f.Since)
	}
	// "none" = solo las viejas/sin clasificar; útil para auditar el histórico.
	if f.NetRoute == "none" {
		q += " AND net_route IS NULL"
	} else if f.NetRoute != "" {
		q += " AND net_route = ?"
		args = append(args, f.NetRoute)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []json.RawMessage
	for rows.Next() {
		var id int64
		var receivedAt, raw string
		if err := rows.Scan(&id, &receivedAt, &raw); err != nil {
			return nil, err
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			continue
		}
		obj["_id"] = id
		obj["_received_at"] = receivedAt
		merged, _ := json.Marshal(obj)
		out = append(out, merged)
	}
	return out, rows.Err()
}

// DeleteMeasurement borra una medición por id. Devuelve deleted=false (sin
// error) si no existía, para que el handler HTTP pueda devolver 404 en vez de
// un 200 engañoso.
func (s *Store) DeleteMeasurement(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM measurements WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListHeartbeats devuelve los últimos heartbeats de un device_id (o de todos si
// va vacío), más reciente primero. Sirve para verificar que un equipo sigue
// vivo (uptime del módem) sin necesidad de una medición completa.
func (s *Store) ListHeartbeats(ctx context.Context, deviceID string, limit int) ([]json.RawMessage, error) {
	if limit <= 0 || limit > 5000 {
		limit = 100
	}
	q := "SELECT id, received_at, raw FROM heartbeats"
	var args []any
	if deviceID != "" {
		q += " WHERE device_id = ?"
		args = append(args, deviceID)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []json.RawMessage
	for rows.Next() {
		var id int64
		var receivedAt, raw string
		if err := rows.Scan(&id, &receivedAt, &raw); err != nil {
			return nil, err
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			continue
		}
		obj["_id"] = id
		obj["_received_at"] = receivedAt
		merged, _ := json.Marshal(obj)
		out = append(out, merged)
	}
	return out, rows.Err()
}

type GroupSummary struct {
	Group       string  `json:"group"`
	Count       int     `json:"count"`
	DownMbpsAvg float64 `json:"down_mbps_avg"`
	DownMbpsP50 float64 `json:"down_mbps_p50"`
	UpMbpsAvg   float64 `json:"up_mbps_avg"`
	UpMbpsP50   float64 `json:"up_mbps_p50"`
	RSRPDbmAvg  float64 `json:"rsrp_dbm_avg"`
	PingMsAvg   float64 `json:"ping_ms_avg"`
}

// Summary agrupa por operator|tag|device_id y calcula promedio + mediana de
// down/up_mbps (sqlite no trae MEDIAN, así que se calcula en Go).
//
// netRoute vacío = sin filtro, o sea exactamente el comportamiento de siempre.
// Existe porque group_by=device_id y group_by=tag hoy mezclan mediciones hechas
// por otra red con las del equipo; group_by=operator no, porque ya filtra
// "operator IS NOT NULL" y la prueba del navegador no manda operator.
func (s *Store) Summary(ctx context.Context, groupBy string, since string, netRoute string) ([]GroupSummary, error) {
	col := map[string]string{"operator": "operator", "tag": "tag", "device_id": "device_id"}[groupBy]
	if col == "" {
		col = "operator"
	}
	q := fmt.Sprintf("SELECT %s, down_mbps, up_mbps, rsrp_dbm, ping_ms FROM measurements WHERE %s IS NOT NULL", col, col)
	var args []any
	if since != "" {
		q += " AND ts >= ?"
		args = append(args, since)
	}
	if netRoute == "none" {
		q += " AND net_route IS NULL"
	} else if netRoute != "" {
		q += " AND net_route = ?"
		args = append(args, netRoute)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type acc struct {
		down, up []float64
		rsrpSum  float64
		rsrpN    int
		pingSum  float64
		pingN    int
		n        int
	}
	groups := map[string]*acc{}
	for rows.Next() {
		var g string
		var down, up, rsrp, ping sql.NullFloat64
		if err := rows.Scan(&g, &down, &up, &rsrp, &ping); err != nil {
			return nil, err
		}
		a := groups[g]
		if a == nil {
			a = &acc{}
			groups[g] = a
		}
		a.n++
		if down.Valid {
			a.down = append(a.down, down.Float64)
		}
		if up.Valid {
			a.up = append(a.up, up.Float64)
		}
		if rsrp.Valid {
			a.rsrpSum += rsrp.Float64
			a.rsrpN++
		}
		if ping.Valid {
			a.pingSum += ping.Float64
			a.pingN++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []GroupSummary
	for g, a := range groups {
		gs := GroupSummary{Group: g, Count: a.n}
		gs.DownMbpsAvg, gs.DownMbpsP50 = avgMedian(a.down)
		gs.UpMbpsAvg, gs.UpMbpsP50 = avgMedian(a.up)
		if a.rsrpN > 0 {
			gs.RSRPDbmAvg = a.rsrpSum / float64(a.rsrpN)
		}
		if a.pingN > 0 {
			gs.PingMsAvg = a.pingSum / float64(a.pingN)
		}
		out = append(out, gs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out, nil
}

func avgMedian(v []float64) (avg, median float64) {
	if len(v) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	avg = sum / float64(len(v))
	cp := append([]float64(nil), v...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 0 {
		median = (cp[mid-1] + cp[mid]) / 2
	} else {
		median = cp[mid]
	}
	return math.Round(avg*100) / 100, math.Round(median*100) / 100
}

// ------------------------------------------------------------------ resumen por dispositivo

// DeviceSummary es el último estado conocido de un device_id, combinando
// measurements y heartbeats. Ver ListDeviceSummaries.
type DeviceSummary struct {
	DeviceID           string   `json:"device_id"`
	LastSeen           string   `json:"last_seen"`
	LastSeenSecondsAgo *float64 `json:"last_seen_seconds_ago,omitempty"`
	Online             bool     `json:"online"`
	Operator           string   `json:"operator,omitempty"`
	RAT                string   `json:"rat,omitempty"`
	BandLTE            *int     `json:"band_lte,omitempty"`
	RSRPDbm            *float64 `json:"rsrp_dbm,omitempty"`
	Lat                *float64 `json:"lat,omitempty"`
	Lon                *float64 `json:"lon,omitempty"`
	GPSAccuracyM       *float64 `json:"gps_accuracy_m,omitempty"`
	DownMbps           *float64 `json:"down_mbps,omitempty"`
	UpMbps             *float64 `json:"up_mbps,omitempty"`
	PingMs             *float64 `json:"ping_ms,omitempty"`
	LossPct            *float64 `json:"loss_pct,omitempty"`
	UptimeS            *float64 `json:"uptime_s,omitempty"`
}

// onlineThreshold: a partir de cuánto tiempo sin reportar se considera que un
// equipo está desconectado. El agente del router manda un heartbeat cada ~1
// minuto (ver cmd/routeragent); 2.5x ese intervalo da margen a que un ciclo se
// demore o se pierda sin marcar el equipo como offline al toque.
const onlineThreshold = 150 * time.Second

// ListDeviceSummaries devuelve, por cada device_id visto, el último estado
// conocido combinando measurements y heartbeats (el que tenga el received_at
// más reciente de los dos). down_mbps/up_mbps se toman aparte, de la última
// medición que sí trajo prueba de velocidad: puede ser una fila más vieja que
// la de last_seen si el dato más reciente fue solo un heartbeat sin prueba.
func (s *Store) ListDeviceSummaries(ctx context.Context) ([]DeviceSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT device_id, MAX(received_at) AS last_seen FROM (
	SELECT device_id, received_at FROM measurements
	UNION ALL
	SELECT device_id, received_at FROM heartbeats
) GROUP BY device_id ORDER BY device_id`)
	if err != nil {
		return nil, fmt.Errorf("listar devices: %w", err)
	}
	type deviceLastSeen struct{ deviceID, lastSeen string }
	var seen []deviceLastSeen
	for rows.Next() {
		var d deviceLastSeen
		if err := rows.Scan(&d.deviceID, &d.lastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		seen = append(seen, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]DeviceSummary, 0, len(seen))
	for _, d := range seen {
		ds := DeviceSummary{DeviceID: d.deviceID, LastSeen: d.lastSeen}

		if t, err := time.Parse(time.RFC3339, d.lastSeen); err == nil {
			secs := time.Since(t).Seconds()
			ds.LastSeenSecondsAgo = &secs
			ds.Online = time.Since(t) <= onlineThreshold
		}

		var operator, rat sql.NullString
		var bandLTE sql.NullInt64
		var rsrp, lat, lon, acc, uptime, pingMs, lossPct sql.NullFloat64

		mrow := s.db.QueryRowContext(ctx, `
SELECT operator, rat, band_lte, rsrp_dbm, lat, lon, gps_accuracy_m, uptime_s
FROM measurements WHERE device_id = ? AND received_at = ? ORDER BY id DESC LIMIT 1`, d.deviceID, d.lastSeen)
		err := mrow.Scan(&operator, &rat, &bandLTE, &rsrp, &lat, &lon, &acc, &uptime)
		if err == sql.ErrNoRows {
			hrow := s.db.QueryRowContext(ctx, `
SELECT operator, rat, rsrp_dbm, uptime_s, ping_ms, loss_pct, lat, lon, gps_accuracy_m
FROM heartbeats WHERE device_id = ? AND received_at = ? ORDER BY id DESC LIMIT 1`, d.deviceID, d.lastSeen)
			if err := hrow.Scan(&operator, &rat, &rsrp, &uptime, &pingMs, &lossPct, &lat, &lon, &acc); err != nil && err != sql.ErrNoRows {
				return nil, fmt.Errorf("leer heartbeat de %s: %w", d.deviceID, err)
			}
		} else if err != nil {
			return nil, fmt.Errorf("leer medición de %s: %w", d.deviceID, err)
		}
		ds.Operator = operator.String
		ds.RAT = rat.String
		if bandLTE.Valid {
			v := int(bandLTE.Int64)
			ds.BandLTE = &v
		}
		if rsrp.Valid {
			ds.RSRPDbm = &rsrp.Float64
		}
		if lat.Valid {
			ds.Lat = &lat.Float64
		}
		if lon.Valid {
			ds.Lon = &lon.Float64
		}
		if acc.Valid {
			ds.GPSAccuracyM = &acc.Float64
		}
		if uptime.Valid {
			ds.UptimeS = &uptime.Float64
		}
		if pingMs.Valid {
			ds.PingMs = &pingMs.Float64
		}
		if lossPct.Valid {
			ds.LossPct = &lossPct.Float64
		}

		// La velocidad titular del equipo NO puede salir de una prueba que se
		// midió por otra red (la SIM del celular, otro WiFi): sería el número
		// grande del dashboard mintiendo. Las filas viejas (net_route NULL) se
		// siguen aceptando para no esconder todo el histórico de un saque.
		//
		// Tampoco puede salir de un "router" de confianza BAJA: esos son los
		// casos en que la única evidencia es que la IP pública coincide con la
		// del equipo, y bajo el CGNAT móvil eso también pasa cuando el celular
		// mide por su propia SIM y le toca la misma IP del pool del operador
		// (ver ipSignal en internal/api/netclass.go). La medición se guarda y se
		// muestra etiquetada, pero no asciende al número grande.
		var down, up sql.NullFloat64
		srow := s.db.QueryRowContext(ctx, `
SELECT down_mbps, up_mbps FROM measurements
WHERE device_id = ? AND down_mbps IS NOT NULL
  AND (net_route IS NULL
       OR (net_route = 'router' AND net_confidence IN ('high','medium')))
ORDER BY id DESC LIMIT 1`, d.deviceID)
		if err := srow.Scan(&down, &up); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("leer velocidad de %s: %w", d.deviceID, err)
		}
		if down.Valid {
			ds.DownMbps = &down.Float64
		}
		if up.Valid {
			ds.UpMbps = &up.Float64
		}

		// ping_ms puede venir de un heartbeat más reciente que el "last_seen"
		// combinado (p. ej. si la última fila fue una medición sin ping propio);
		// mismo criterio que down/up_mbps arriba: el dato más reciente que sí lo trae.
		if ds.PingMs == nil {
			var pm sql.NullFloat64
			prow := s.db.QueryRowContext(ctx, `
SELECT ping_ms FROM heartbeats WHERE device_id = ? AND ping_ms IS NOT NULL
ORDER BY id DESC LIMIT 1`, d.deviceID)
			if err := prow.Scan(&pm); err != nil && err != sql.ErrNoRows {
				return nil, fmt.Errorf("leer ping de %s: %w", d.deviceID, err)
			}
			if pm.Valid {
				ds.PingMs = &pm.Float64
			}
		}

		// Igual con la banda: los heartbeats no la traen (solo ubus/get_zcainfo,
		// que se lee en una medición completa), así que si el último reporte fue
		// un heartbeat, se toma la banda de la última medición que sí la tenga.
		if ds.BandLTE == nil {
			var b sql.NullInt64
			brow := s.db.QueryRowContext(ctx, `
SELECT band_lte FROM measurements WHERE device_id = ? AND band_lte IS NOT NULL
ORDER BY id DESC LIMIT 1`, d.deviceID)
			if err := brow.Scan(&b); err != nil && err != sql.ErrNoRows {
				return nil, fmt.Errorf("leer banda de %s: %w", d.deviceID, err)
			}
			if b.Valid {
				v := int(b.Int64)
				ds.BandLTE = &v
			}
		}

		out = append(out, ds)
	}
	return out, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

// ------------------------------------------------------------------ cola de comandos
//
// Flujo: el celular crea un comando (p. ej. "run_speedtest" con la ubicación
// ultra-precisa que le dio Google) dirigido a un device_id de router. El router
// (que no puede recibir conexiones entrantes por estar detrás de su NAT
// celular) hace polling con ClaimNextCommand, ejecuta la prueba localmente y
// llama CompleteCommand con el resultado; la ubicación del comando se inyecta
// en esa medición porque el módem no tiene GPS propio.

type Command struct {
	ID           int64    `json:"id"`
	CreatedAt    string   `json:"created_at"`
	DeviceID     string   `json:"device_id"`
	Type         string   `json:"type"`
	DurationS    *int     `json:"duration_s,omitempty"`
	Lat          *float64 `json:"lat,omitempty"`
	Lon          *float64 `json:"lon,omitempty"`
	GPSAccuracyM *float64 `json:"gps_accuracy_m,omitempty"`
	GPSSource    string   `json:"gps_source,omitempty"`
	RequestedBy  string   `json:"requested_by,omitempty"`
	Status       string   `json:"status"`
}

// CreateCommand: `raw` es el body que manda el celular, p.ej.
// {"device_id":"router-<serial>","type":"run_speedtest","duration_s":10,
//
//	"lat":4.65,"lon":-74.05,"gps_accuracy_m":5,"gps_source":"android-fused","requested_by":"android-abc"}
func (s *Store) CreateCommand(ctx context.Context, raw json.RawMessage) (int64, error) {
	var c Command
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, fmt.Errorf("payload inválido: %w", err)
	}
	if c.DeviceID == "" {
		return 0, fmt.Errorf("falta device_id")
	}
	if c.Type == "" {
		return 0, fmt.Errorf("falta type")
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO commands (created_at, device_id, type, duration_s, lat, lon, gps_accuracy_m, gps_source,
	requested_by, status)
VALUES (?,?,?,?,?,?,?,?,?, 'pending')`,
		time.Now().UTC().Format(time.RFC3339), c.DeviceID, c.Type, c.DurationS, c.Lat, c.Lon,
		c.GPSAccuracyM, nullStr(c.GPSSource), nullStr(c.RequestedBy))
	if err != nil {
		return 0, fmt.Errorf("insert command: %w", err)
	}
	return res.LastInsertId()
}

// staleClaimAfter: si un comando lleva más de esto en 'claimed' sin cerrarse
// (el router lo tomó pero nunca llamó CompleteCommand — se cayó a mitad de la
// prueba, falló el reporte, etc.), se vuelve a ofrecer como si fuera nuevo en
// vez de quedar bloqueado para siempre. Una prueba de velocidad dura segundos,
// así que unos minutos sin cerrar ya es señal de que se abandonó.
const staleClaimAfter = 3 * time.Minute

// ClaimNextCommand toma el comando más antiguo para ese device_id que esté
// 'pending', o 'claimed' hace más de staleClaimAfter (abandonado), y lo marca
// 'claimed' de nuevo. Devuelve (nil, nil) si no hay ninguno disponible.
// SetMaxOpenConns(1) en Open() serializa esto: no hay carrera real entre
// selección y marcado aunque no use una transacción explícita.
func (s *Store) ClaimNextCommand(ctx context.Context, deviceID string) (*Command, error) {
	staleBefore := time.Now().UTC().Add(-staleClaimAfter).Format(time.RFC3339)
	row := s.db.QueryRowContext(ctx, `
SELECT id, created_at, device_id, type, duration_s, lat, lon, gps_accuracy_m, gps_source, requested_by
FROM commands
WHERE device_id = ? AND (status = 'pending' OR (status = 'claimed' AND claimed_at < ?))
ORDER BY id ASC LIMIT 1`, deviceID, staleBefore)

	var c Command
	var durationS sql.NullInt64
	var lat, lon, acc sql.NullFloat64
	var gpsSource, requestedBy sql.NullString
	err := row.Scan(&c.ID, &c.CreatedAt, &c.DeviceID, &c.Type, &durationS, &lat, &lon, &acc, &gpsSource, &requestedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim command: %w", err)
	}
	if durationS.Valid {
		v := int(durationS.Int64)
		c.DurationS = &v
	}
	if lat.Valid {
		c.Lat = &lat.Float64
	}
	if lon.Valid {
		c.Lon = &lon.Float64
	}
	if acc.Valid {
		c.GPSAccuracyM = &acc.Float64
	}
	c.GPSSource = gpsSource.String
	c.RequestedBy = requestedBy.String
	c.Status = "claimed"

	if _, err := s.db.ExecContext(ctx, `UPDATE commands SET status='claimed', claimed_at=? WHERE id=? AND status IN ('pending','claimed')`,
		time.Now().UTC().Format(time.RFC3339), c.ID); err != nil {
		return nil, fmt.Errorf("marcar claimed: %w", err)
	}
	return &c, nil
}

// CompleteCommand cierra un comando: guarda éxito/error y, si viene una
// medición, la inserta fusionándole la ubicación que traía el comando
// original (el router no la conoce; se la puso el celular al crearlo).
// Devuelve el id de la medición insertada (0 si el comando no trajo ninguna),
// para que la capa HTTP pueda clasificarle la ruta de salida: una medición que
// llega por acá la corrió el propio equipo, o sea que salió por su módem.
func (s *Store) CompleteCommand(ctx context.Context, id int64, raw json.RawMessage) (int64, error) {
	var body struct {
		Status      string          `json:"status"` // "done" | "failed"
		Error       string          `json:"error,omitempty"`
		Measurement json.RawMessage `json:"measurement,omitempty"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, fmt.Errorf("payload inválido: %w", err)
	}
	if body.Status != "done" && body.Status != "failed" {
		return 0, fmt.Errorf("status debe ser 'done' o 'failed'")
	}

	row := s.db.QueryRowContext(ctx, `SELECT lat, lon, gps_accuracy_m, gps_source FROM commands WHERE id = ?`, id)
	var lat, lon, acc sql.NullFloat64
	var gpsSource sql.NullString
	if err := row.Scan(&lat, &lon, &acc, &gpsSource); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("comando %d no existe", id)
		}
		return 0, fmt.Errorf("leer comando: %w", err)
	}

	var insertedID int64
	var measurementID any
	if len(body.Measurement) > 0 {
		merge := map[string]any{"gps_source": gpsSource.String}
		if lat.Valid {
			merge["lat"] = lat.Float64
		}
		if lon.Valid {
			merge["lon"] = lon.Float64
		}
		if acc.Valid {
			merge["gps_accuracy_m"] = acc.Float64
		}
		// Sin clasificar: la medición la corrió el propio equipo, pero eso lo
		// decide el servidor en noteDeviceMeasurement (que además aprende la
		// IP pública del equipo), no el JSON que mandó el agente.
		mid, err := s.insertMeasurement(ctx, body.Measurement, merge, NetClassification{})
		if err != nil {
			return 0, fmt.Errorf("insertar medición del comando: %w", err)
		}
		measurementID = mid
		insertedID = mid
	}

	_, err := s.db.ExecContext(ctx, `
UPDATE commands SET status=?, completed_at=?, error=?, measurement_id=? WHERE id=?`,
		body.Status, time.Now().UTC().Format(time.RFC3339), nullStr(body.Error), measurementID, id)
	if err != nil {
		return 0, fmt.Errorf("cerrar comando: %w", err)
	}
	return insertedID, nil
}

// DeviceIDOfCommand dice a qué equipo pertenece un comando. Se usa al cerrarlo
// para anotar desde qué IP pública se vio salir a ese equipo (el cuerpo del
// POST /complete no trae device_id: el agente solo manda el resultado).
func (s *Store) DeviceIDOfCommand(ctx context.Context, id int64) (string, bool) {
	var deviceID string
	if err := s.db.QueryRowContext(ctx, `SELECT device_id FROM commands WHERE id = ?`, id).Scan(&deviceID); err != nil {
		return "", false
	}
	return deviceID, deviceID != ""
}

// ListCommands: para depurar/verificar desde fuera qué comandos hay (todos, o
// filtrados por device_id/status), más reciente primero.
func (s *Store) ListCommands(ctx context.Context, deviceID, status string, limit int) ([]Command, error) {
	if limit <= 0 || limit > 2000 {
		limit = 100
	}
	q := "SELECT id, created_at, device_id, type, duration_s, lat, lon, gps_accuracy_m, gps_source, requested_by, status FROM commands WHERE 1=1"
	var args []any
	if deviceID != "" {
		q += " AND device_id = ?"
		args = append(args, deviceID)
	}
	if status != "" {
		q += " AND status = ?"
		args = append(args, status)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Command
	for rows.Next() {
		var c Command
		var durationS sql.NullInt64
		var lat, lon, acc sql.NullFloat64
		var gpsSource, requestedBy sql.NullString
		if err := rows.Scan(&c.ID, &c.CreatedAt, &c.DeviceID, &c.Type, &durationS, &lat, &lon, &acc,
			&gpsSource, &requestedBy, &c.Status); err != nil {
			return nil, err
		}
		if durationS.Valid {
			v := int(durationS.Int64)
			c.DurationS = &v
		}
		if lat.Valid {
			c.Lat = &lat.Float64
		}
		if lon.Valid {
			c.Lon = &lon.Float64
		}
		if acc.Valid {
			c.GPSAccuracyM = &acc.Float64
		}
		c.GPSSource = gpsSource.String
		c.RequestedBy = requestedBy.String
		out = append(out, c)
	}
	return out, rows.Err()
}
