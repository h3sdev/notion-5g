// Command routeragent corre EN el propio router Notion 5G (ASR1901/OpenWrt),
// no en el PC. A diferencia de notion5g.py (que llega por SSH), este binario
// llama `ubus` localmente (nunca `serial_atcmd`: el puerto AT es un canal único
// compartido con el firmware, y llamarlo desde un cron externo puede chocar con
// lo que el equipo ya está haciendo) y le reporta al backend (server/) directamente
// por HTTP, sin depender de que haya un PC o celular conectado.
//
// Pensado para invocarse por cron cada N minutos (una corrida corta, sin
// demonio persistente): el router tiene ~96 MB de RAM con solo ~30 MB libres,
// así que aquí NO hay goroutines de fondo ni loops infinitos — se ejecuta,
// hace UNA cosa (atender un comando pendiente o mandar un heartbeat) y termina.
//
// Cross-compilar sin CGO (no depende de la libc del sistema del router):
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
//	    -o routeragent-arm ./cmd/routeragent
//
// Config en /data/notion5g-agent.json (partición escribible; /etc es solo
// lectura en este equipo):
//
//	{"backend_url":"https://xxx.trycloudflare.com","api_key":"...","device_id":"router-R524260829000001"}
//
// device_id es opcional: si falta, se deriva del número de serie del equipo
// (cm/get_link_context) la primera vez y se guarda de vuelta en el archivo.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultConfigPath = "/data/notion5g-agent.json"

// pingTargetHost: mismo destino por defecto que ya usa notion5g.py desde el
// PC, para que los perfiles de ping del router y del PC sean comparables.
const pingTargetHost = "8.8.8.8"

// configPath: NOTION_AGENT_CONFIG permite apuntar a otra ruta (pruebas en un
// PC de desarrollo); en el router real se deja sin definir y usa /data/....
func configPath() string {
	if p := os.Getenv("NOTION_AGENT_CONFIG"); p != "" {
		return p
	}
	return defaultConfigPath
}

type Config struct {
	BackendURL string `json:"backend_url"`
	APIKey     string `json:"api_key"`
	DeviceID   string `json:"device_id,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "notion5g-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	path := configPath()
	cfg, err := loadConfig(path)
	if err != nil {
		return fmt.Errorf("config (%s): %w", path, err)
	}
	if cfg.BackendURL == "" {
		return fmt.Errorf("config sin backend_url")
	}
	client := &http.Client{Timeout: 20 * time.Second}

	if cfg.DeviceID == "" {
		serial, err := readSerial()
		if err != nil {
			return fmt.Errorf("no se pudo derivar device_id (falta serial y no está en config): %w", err)
		}
		cfg.DeviceID = "router-" + serial
		if err := saveConfig(path, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "aviso: no se pudo guardar device_id derivado:", err)
		}
	}

	cmd, err := claimCommand(client, cfg)
	if err != nil {
		return fmt.Errorf("consultando comandos: %w", err)
	}
	if cmd == nil {
		// sin comando pendiente: heartbeat liviano (solo uptime+señal, sin prueba de velocidad)
		return sendHeartbeat(client, cfg)
	}

	switch cmd.Type {
	case "run_speedtest":
		return handleSpeedtest(client, cfg, cmd)
	default:
		return completeCommand(client, cfg, cmd.ID, map[string]any{
			"status": "failed",
			"error":  "tipo de comando desconocido: " + cmd.Type,
		})
	}
}

// ------------------------------------------------------------------ config

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("JSON inválido: %w", err)
	}
	return c, nil
}

func saveConfig(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ------------------------------------------------------------------ comandos

type Command struct {
	ID           int64    `json:"id"`
	DeviceID     string   `json:"device_id"`
	Type         string   `json:"type"`
	DurationS    *int     `json:"duration_s"`
	Lat          *float64 `json:"lat"`
	Lon          *float64 `json:"lon"`
	GPSAccuracyM *float64 `json:"gps_accuracy_m"`
}

func claimCommand(client *http.Client, cfg Config) (*Command, error) {
	req, err := http.NewRequest("GET", cfg.BackendURL+"/api/v1/commands/next?device_id="+cfg.DeviceID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var cmd Command
	if err := json.Unmarshal(body, &cmd); err != nil {
		return nil, fmt.Errorf("respuesta inválida: %w", err)
	}
	if cmd.ID == 0 {
		return nil, nil // {} = no había nada pendiente
	}
	return &cmd, nil
}

func completeCommand(client *http.Client, cfg Config, id int64, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/v1/commands/%d/complete", cfg.BackendURL, id)
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d completando comando: %s", resp.StatusCode, string(body))
	}
	return nil
}

// pingHost hace un ping corto (busybox ping, ya viene en el firmware) y
// devuelve el RTT promedio y el % de pérdida. Best-effort: si el binario
// falla o el parseo no encuentra nada, devuelve (nil, nil, err) y quien
// llama simplemente omite esos campos en vez de fallar todo el heartbeat.
func pingHost(host string, count int) (avgMs, lossPct *float64, err error) {
	out, runErr := exec.Command("ping", "-c", strconv.Itoa(count), "-W", "2", host).CombinedOutput()
	text := string(out)
	if m := regexp.MustCompile(`(\d+(?:\.\d+)?)% packet loss`).FindStringSubmatch(text); len(m) == 2 {
		if v, e := strconv.ParseFloat(m[1], 64); e == nil {
			lossPct = &v
		}
	}
	// "round-trip min/avg/max = 31.847/32.172/32.561 ms" (busybox) o variantes con "/mdev"
	if m := regexp.MustCompile(`(?:round-trip|rtt) min[^=]*=\s*([\d.]+)/([\d.]+)/([\d.]+)`).FindStringSubmatch(text); len(m) == 4 {
		if v, e := strconv.ParseFloat(m[2], 64); e == nil {
			avgMs = &v
		}
	}
	if avgMs == nil && lossPct == nil {
		if runErr != nil {
			return nil, nil, fmt.Errorf("ping %s: %w", host, runErr)
		}
		return nil, nil, fmt.Errorf("ping %s: no se pudo interpretar la salida", host)
	}
	return avgMs, lossPct, nil
}

func sendHeartbeat(client *http.Client, cfg Config) error {
	st, statusErr := readLocalStatus()
	hb := map[string]any{"device_id": cfg.DeviceID, "source": "router"}
	if statusErr != nil {
		hb["ts"] = time.Now().UTC().Format(time.RFC3339)
		hb["error"] = statusErr.Error()
	} else {
		for _, k := range []string{"ts", "uptime_s", "operator", "rat", "rsrp_dbm"} {
			if v, ok := st[k]; ok {
				hb[k] = v
			}
		}
	}
	// El ping es lo que le da sentido al "perfil de ping con ubicación" cuando
	// el equipo va en movimiento (powerbank + celular al lado, sin PC). Se
	// intenta siempre, incluso si leer el estado local falló arriba.
	if avgMs, lossPct, err := pingHost(pingTargetHost, 3); err == nil {
		if avgMs != nil {
			hb["ping_ms"] = *avgMs
		}
		if lossPct != nil {
			hb["loss_pct"] = *lossPct
		}
	}
	b, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", cfg.BackendURL+"/api/v1/heartbeat", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d en heartbeat", resp.StatusCode)
	}
	return nil
}

// ------------------------------------------------------------------ prueba de velocidad

func handleSpeedtest(client *http.Client, cfg Config, cmd *Command) error {
	duration := 8 * time.Second
	if cmd.DurationS != nil && *cmd.DurationS > 0 {
		duration = time.Duration(*cmd.DurationS) * time.Second
	}

	st, statusErr := readLocalStatus()
	if statusErr != nil {
		return completeCommand(client, cfg, cmd.ID, map[string]any{
			"status": "failed", "error": "leyendo estado local: " + statusErr.Error(),
		})
	}

	downBytes, downErr := timedDownload(cfg, duration)
	upBytes, upErr := timedUpload(cfg, duration)

	measurement := map[string]any{"device_id": cfg.DeviceID, "source": "router"}
	for k, v := range st {
		measurement[k] = v
	}
	measurement["down_mbps"] = round2(float64(downBytes) * 8 / duration.Seconds() / 1e6)
	measurement["up_mbps"] = round2(float64(upBytes) * 8 / duration.Seconds() / 1e6)
	if avgMs, lossPct, err := pingHost(pingTargetHost, 3); err == nil {
		if avgMs != nil {
			measurement["ping_ms"] = *avgMs
		}
		if lossPct != nil {
			measurement["loss_pct"] = *lossPct
		}
	}
	if downErr != nil {
		measurement["down_error"] = downErr.Error()
	}
	if upErr != nil {
		measurement["up_error"] = upErr.Error()
	}

	return completeCommand(client, cfg, cmd.ID, map[string]any{
		"status":      "done",
		"measurement": measurement,
	})
}

// timedDownload lee del endpoint de descarga del backend durante `d`, contando
// bytes sin guardarlos (io.Discard): memoria plana sin importar cuántos bytes
// pasen, clave con solo ~30 MB libres en el equipo.
func timedDownload(cfg Config, d time.Duration) (int64, error) {
	url := fmt.Sprintf("%s/api/v1/speedtest/download?bytes=%d", cfg.BackendURL, 200_000_000)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)
	c := &http.Client{Timeout: d + 15*time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, 2<<30) // techo defensivo, el tiempo es lo que realmente corta
	done := make(chan struct{})
	var n int64
	go func() {
		n, _ = io.Copy(io.Discard, limited)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		resp.Body.Close() // corta la conexión; el goroutine de arriba termina al ver EOF/error
		<-done
	}
	return n, nil
}

// timedUpload manda bytes en cero al endpoint de subida durante `d`.
func timedUpload(cfg Config, d time.Duration) (int64, error) {
	pr, pw := io.Pipe()
	counter := &countingReader{r: pr}
	url := cfg.BackendURL + "/api/v1/speedtest/upload"
	req, err := http.NewRequest("POST", url, counter)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)
	req.Header.Set("Content-Type", "application/octet-stream")

	stop := time.NewTimer(d)
	defer stop.Stop()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			select {
			case <-stop.C:
				pw.Close()
				return
			default:
			}
			if _, err := pw.Write(buf); err != nil {
				return
			}
		}
	}()

	c := &http.Client{Timeout: d + 15*time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return counter.n, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return counter.n, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return counter.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// ------------------------------------------------------------------ estado local (sin SSH)
//
// Mismos objetos ubus / comandos AT que STATUS_REMOTE en notion5g.py, pero
// ejecutados en el propio equipo con os/exec en vez de por SSH. Los nombres de
// campo de salida coinciden con los que ya usa notion5g.py, para que el
// backend no tenga que distinguir el origen del reporte.

func readLocalStatus() (map[string]any, error) {
	st := map[string]any{"ts": time.Now().Format("2006-01-02T15:04:05")}

	if zca, err := ubusCall("cm", "get_zcainfo"); err == nil {
		lte, _ := zca["lte"].(map[string]any)
		st["pci"] = lte["p_pci"]
		st["band_lte"] = lte["p_band"]
		st["earfcn"] = lte["p_dlEuArfcn"]
		st["bw_mhz"] = lte["p_dlBandwitdh"]
		if v, ok := toFloat(lte["p_rsrp"]); ok {
			st["rsrp_dbm"] = cesqRSRPDbm(v)
		}
		if v, ok := toFloat(lte["p_rsrq"]); ok {
			st["rsrq_db"] = cesqRSRQDb(v)
		}
		st["sinr_db"] = lte["p_sinr"]
		if v, ok := toFloat(lte["p_rssi"]); ok {
			st["rssi_dbm"] = cesqRSSIDbm(v)
		}
		if v, ok := toFloat(lte["s_status"]); ok {
			st["ca_secondary"] = v != 0
		}

		// Bloque NR. Hasta 2026-09-24 el agente leía SOLO el sub-objeto "lte",
		// así que nr_band llegaba siempre nulo al backend (0 de 74 mediciones
		// en producción lo tenían) y el dashboard no podía mostrar la
		// combinación NSA -- que es el dato que importa en campo, porque en
		// Colombia el 5G es siempre ENDC: ancla LTE + portadora NR a la vez.
		//
		// No hay documentación del firmware para este equipo y no se pudo
		// inspeccionar un get_zcainfo con NR activo (el router venía midiendo
		// en LTE), así que en vez de adivinar un nombre de campo se prueban los
		// candidatos plausibles y, si no aparece ninguno, se adjunta el bloque
		// crudo para descubrir los nombres reales con la primera medición que
		// enganche 5G. Cuando se sepan, esto se reemplaza por los nombres
		// exactos y se borra el volcado.
		nr := firstMap(zca, "nr", "nr5g", "NR", "endc", "sa", "nsa")
		if nr != nil {
			if v, ok := firstVal(nr, "n_band", "band", "nr_band", "p_band"); ok {
				st["nr_band"] = v
			}
			if v, ok := firstVal(nr, "n_pci", "pci", "p_pci"); ok {
				st["nr_pci"] = v
			}
			if v, ok := firstVal(nr, "n_arfcn", "nrarfcn", "arfcn", "p_dlEuArfcn"); ok {
				st["nr_arfcn"] = v
			}
			if v, ok := toFloat(firstAny(nr, "n_rsrp", "rsrp", "p_rsrp")); ok {
				st["nr_rsrp_dbm"] = v
			}
			if v, ok := toFloat(firstAny(nr, "n_sinr", "sinr", "p_sinr")); ok {
				st["nr_sinr_db"] = v
			}
		}
		// Volcado de diagnóstico: solo cuando hay indicio de NR y no se pudo
		// sacar la banda. Es chico (el propio objeto ubus) y va a parar a la
		// columna raw del backend, que ya guarda el JSON completo. No se manda
		// siempre para no inflar cada heartbeat del equipo.
		if _, tieneBanda := st["nr_band"]; !tieneBanda && nr != nil {
			st["zcainfo_nr_raw"] = nr
		}
	}
	if mode, err := ubusCall("util_wan", "get_network_mode"); err == nil {
		if nm, ok := mode["net_mode"].(map[string]any); ok {
			st["nw_mode"] = nm["nw_mode"]
			st["prefer_mode"] = nm["prefer_mode"]
			st["nr_mode"] = nm["nr_mode"]
		}
	}
	if sim, err := ubusCall("sim", "get_double_sim_status"); err == nil {
		if s, ok := sim["sim"].(map[string]any); ok {
			st["sim_slot"] = s["current_card"]
		}
	}

	// Operador y estado de registro: por el mismo objeto ubus que envuelve la web
	// (cm/get_link_context), NO por serial_atcmd. El puerto AT del módem es un canal
	// único compartido con el firmware; llamarlo directo desde un cron externo puede
	// chocar con lo que el propio equipo ya está haciendo y dejarlo bloqueado.
	if ctx, err := ubusCall("cm", "get_link_context"); err == nil {
		info, _ := ctx["celluar_basic_info"].(map[string]any)
		if op, ok := info["network_name"].(string); ok && op != "" {
			st["operator"] = op
		}
		if v, ok := toFloat(info["RegStatus"]); ok {
			st["eps_reg"] = int(v)
		}
		// sys_mode: mismo campo que usa el panel web del propio equipo
		// (/www/js/panel/internet/engineeringInfo.js) para decidir qué mostrar
		// como "tipo de red". Reemplaza al heurístico viejo (rat="LTE" si hay
		// banda LTE), que nunca podía detectar 5G: en Colombia el 5G siempre es
		// NSA/ENDC (confirmado con Diego 2026-09-18), es decir CON ancla LTE
		// activa, así que ese heurístico marcaba "LTE" aunque el equipo ya
		// estuviera agregando portadora NR.
		//   0 sin servicio · 1 2G/3G · 2 LTE · 3 LTE-CA (4G+) · 4 5G SA · 5 5G NSA (ENDC)
		if v, ok := toFloat(info["sys_mode"]); ok {
			switch int(v) {
			case 1:
				st["rat"] = "2G/3G"
			case 2:
				st["rat"] = "LTE"
			case 3:
				st["rat"] = "LTE-CA"
			case 4:
				st["rat"] = "5G-SA"
			case 5:
				st["rat"] = "5G-NSA"
			}
		}
	}
	if _, ok := st["rat"]; !ok {
		if _, ok := st["band_lte"]; ok {
			st["rat"] = "LTE" // fallback si get_link_context falló: al menos hay portadora LTE activa
		}
	}

	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				st["uptime_s"] = int64(v)
			}
		}
	}
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			st["temp_c"] = v / 1000
		}
	}
	return st, nil
}

func readSerial() (string, error) {
	ctx, err := ubusCall("cm", "get_link_context")
	if err != nil {
		return "", err
	}
	info, _ := ctx["celluar_basic_info"].(map[string]any)
	sn, _ := info["sn"].(string)
	if sn == "" {
		return "", fmt.Errorf("get_link_context no trajo número de serie")
	}
	return sn, nil
}

func ubusCall(obj, method string) (map[string]any, error) {
	out, err := exec.Command("ubus", "-t", "6", "call", obj, method).Output()
	if err != nil {
		return nil, fmt.Errorf("ubus %s %s: %w", obj, method, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("ubus %s %s: JSON inválido: %w", obj, method, err)
	}
	return m, nil
}

// firstMap devuelve el primer sub-objeto que exista entre los nombres dados.
// Los firmwares de estos equipos no comparten nomenclatura entre versiones, y
// el agente no puede fallar por eso: si no está ninguno, devuelve nil.
func firstMap(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if sub, ok := m[k].(map[string]any); ok && len(sub) > 0 {
			return sub
		}
	}
	return nil
}

// firstAny devuelve el valor del primer campo presente y no vacío.
func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil && v != "" {
			return v
		}
	}
	return nil
}

// firstVal es firstAny pero descartando además los valores que el firmware usa
// como "sin dato" (0 y -1 en los campos de banda/PCI/ARFCN): mandar un 0 sería
// peor que no mandar nada, porque el dashboard lo mostraría como banda 0.
func firstVal(m map[string]any, keys ...string) (any, bool) {
	v := firstAny(m, keys...)
	if v == nil {
		return nil, false
	}
	if f, ok := toFloat(v); ok && (f == 0 || f == -1) {
		return nil, false
	}
	return v, true
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func cesqRSRPDbm(idx float64) *float64 {
	if idx == 255 {
		return nil
	}
	v := -141 + idx
	return &v
}

func cesqRSRQDb(idx float64) *float64 {
	if idx == 255 {
		return nil
	}
	v := round2(-19.5 + idx*0.5)
	return &v
}

func cesqRSSIDbm(idx float64) *float64 {
	if idx == 99 {
		return nil
	}
	v := -111 + idx
	return &v
}
