package main

// Periodic report to omnicap (POST /agent/v1/pull, the same endpoint the RouterOS agent uses). omnicap
// answers a device registered as "openwrt" with JSON: the pull interval set in the panel ("Segundos entre
// reportes"), this device's jitter and the current settings. The report keeps the device online in the
// panel and carries the certificate state; the answer brings walled-garden changes without reinstalling.
//
// Load on the server: the first pull waits a fixed per-device jitter (derived from the token, like the
// RouterOS agent), then one pull per interval. When omnicap does not answer (or answers 429), the wait
// doubles each time up to 30 min, plus the same jitter, so a fleet that lost the server does not come
// back all in the same second.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPullInterval = 60 // s, omnicap's default for the platform until the first answer
	minPullInterval     = 10
	maxPullInterval     = 86399
	maxPullBackoff      = 30 * time.Minute
)

type pullStatus struct {
	Last     string `json:"last,omitempty"` // RFC 3339 of the last attempt
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Interval int    `json:"interval_s"`
}

// tokenJitter is the RouterOS agent's StartJitterSeconds (sha256 of the token, 0..45 s), capped to a quarter
// of the interval. Fixed per device, so the fleet spreads without any coordination.
func tokenJitter(token string, interval int) time.Duration {
	sum := sha256.Sum256([]byte(token))
	j := (int(sum[0])<<8 | int(sum[1])) % 46
	if j > interval/4 {
		j = interval / 4
	}
	return time.Duration(j) * time.Second
}

// nextPullDelay: the interval after a good pull; after n consecutive failures interval*2^n (max 30 min);
// always plus the device jitter.
func nextPullDelay(interval int, failures int, jitter time.Duration) time.Duration {
	d := time.Duration(interval) * time.Second
	for i := 0; i < failures && d < maxPullBackoff; i++ {
		d *= 2
	}
	if d > maxPullBackoff {
		d = maxPullBackoff
	}
	return d + jitter
}

func clampInterval(n int) int {
	switch {
	case n < minPullInterval:
		return minPullInterval
	case n > maxPullInterval:
		return maxPullInterval
	}
	return n
}

func (g *Gateway) pullLoop() {
	if g.cfg.AgentURL == "" || g.cfg.AgentToken == "" {
		return // not registered in omnicap: nothing to report to
	}
	interval, failures := defaultPullInterval, 0
	jitter := tokenJitter(g.cfg.AgentToken, interval)
	time.Sleep(jitter)
	for {
		if n, j, err := g.pullOnce(); err != nil {
			failures++
			log.Printf("pull: %v (retry in %s)", err, nextPullDelay(interval, failures, jitter).Round(time.Second))
		} else {
			if failures > 0 {
				log.Printf("pull: omnicap answers again")
			}
			failures = 0
			if n > 0 {
				interval = clampInterval(n)
			}
			if j >= 0 {
				jitter = time.Duration(j) * time.Second
			}
		}
		time.Sleep(nextPullDelay(interval, failures, jitter))
	}
}

type pullAnswer struct {
	Result       string   `json:"result"`
	Interval     int      `json:"pull_interval_seconds"`
	Jitter       *int     `json:"jitter_seconds"`
	WalledGarden []string `json:"walled_garden"`
	PortalURL    string   `json:"portal_url"`
	LoginHost    string   `json:"login_host"`
}

// pullOnce reports and applies the answer. Returns the interval and jitter omnicap asked for (0 / -1 when
// the answer had none, e.g. an omnicap version that still answers every pull with a RouterOS script).
func (g *Gateway) pullOnce() (interval, jitter int, err error) {
	body, _ := json.Marshal(g.pullReport())
	resp, err := agentRequest("POST", g.cfg.AgentURL+"/agent/v1/pull", g.cfg.AgentToken, body)
	g.mu.Lock()
	g.pull.Last = time.Now().UTC().Format(time.RFC3339)
	g.pull.OK, g.pull.Error = err == nil, ""
	if err != nil {
		g.pull.Error = err.Error()
	}
	g.mu.Unlock()
	if err != nil {
		return 0, -1, err
	}
	var a pullAnswer
	if json.Unmarshal(resp, &a) != nil || a.Result != "ok" {
		return 0, -1, nil // heartbeat registered; no settings in this answer
	}
	jitter = -1
	if a.Jitter != nil {
		jitter = *a.Jitter
	}
	g.applyPullAnswer(a)
	g.mu.Lock()
	g.pull.Interval = clampInterval(a.Interval)
	g.mu.Unlock()
	return a.Interval, jitter, nil
}

// applyPullAnswer applies what the panel can change remotely: the walled garden (saved to uci so a restart
// keeps it). A different portal URL or login host means the device was moved in omnicap: that needs the
// registration again (new certificate, new secret), so it is only logged.
func (g *Gateway) applyPullAnswer(a pullAnswer) {
	wg := a.WalledGarden
	if wg == nil {
		wg = []string{}
	}
	g.mu.Lock()
	cur := g.cfg.WalledGarden
	if cur == nil {
		cur = []string{}
	}
	changed := !reflect.DeepEqual(cur, wg)
	if changed {
		g.cfg.WalledGarden = append([]string{}, wg...)
	}
	g.mu.Unlock()
	if changed {
		log.Printf("pull: walled garden from omnicap: %s", strings.Join(wg, ", "))
		shq("uci", "-q", "delete", g.uciPkg+".main.walled_garden")
		for _, w := range wg {
			shq("uci", "add_list", g.uciPkg+".main.walled_garden="+w)
		}
		shq("uci", "commit", g.uciPkg)
		g.refreshWalledGarden()
	}
	if a.PortalURL != "" && strings.TrimSuffix(a.PortalURL, "/") != g.cfg.PortalURL {
		log.Printf("pull: omnicap has another portal URL for this device (%s): register it again from the web UI", a.PortalURL)
	}
}

type pullObserved struct {
	DNSName          string `json:"dns_name"`
	SSLCertificate   string `json:"ssl_certificate"`
	Timezone         string `json:"timezone"`
	SSID             string `json:"ssid"`
	UseRADIUS        string `json:"use_radius"`
	RADIUSAddress    string `json:"radius_address"`
	SSLFingerprint   string `json:"ssl_certificate_fingerprint,omitempty"`
	SSLExpires       string `json:"ssl_certificate_expires,omitempty"`
	CertUpdateStatus string `json:"cert_update_status,omitempty"`
}

type pullReport struct {
	MAC           string       `json:"mac"`
	Identity      string       `json:"identity"`
	Uptime        string       `json:"uptime"`
	CPULoad       *float64     `json:"cpu_load,omitempty"`
	FreeMemory    *int64       `json:"free_memory,omitempty"`
	Version       string       `json:"version"`
	Board         string       `json:"board"`
	HotspotActive int          `json:"hotspot_active"`
	Observado     pullObserved `json:"observado"`
}

// pullReport fills the fields omnicap reads from a RouterOS agent, in the formats it parses: uptime
// "[Nd]HH:MM:SS", certificate fingerprint = sha256 hex of the leaf, expiry "Jan/02/2006 15:04:05" in the
// reported timezone (UTC here), update status "ok:<...>" or "error:fetch".
func (g *Gateway) pullReport() pullReport {
	r := pullReport{Identity: g.cfg.NASIdentifier, Version: "OpenWrt omnicap-gw",
		Observado: pullObserved{UseRADIUS: "true", RADIUSAddress: g.cfg.RadiusServer, SSID: mainSSID()}}
	// the clock's real offset (the vendor UI sets a POSIX TZ and leaves zonename stale), as an Etc/GMT zone
	// omnicap can load: it reads the certificate expiry below in this zone
	zone, offset := localZone()
	r.Observado.Timezone = zone
	if b, err := os.ReadFile("/sys/class/net/" + g.cfg.LanIf + "/address"); err == nil {
		r.MAC = strings.ToUpper(strings.TrimSpace(string(b)))
	}
	if b, err := os.ReadFile("/etc/openwrt_version"); err == nil {
		r.Version = "OpenWrt " + strings.TrimSpace(string(b)) + " omnicap-gw"
	}
	if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		r.Board = strings.Trim(string(b), "\x00 \n")
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if s, err := strconv.ParseFloat(f[0], 64); err == nil {
				r.Uptime = formatUptime(int64(s))
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if l, err := strconv.ParseFloat(f[0], 64); err == nil {
				pct := l / float64(runtime.NumCPU()) * 100
				r.CPULoad = &pct
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemAvailable:" {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					n := kb * 1024
					r.FreeMemory = &n
				}
			}
		}
	}
	g.mu.Lock()
	r.HotspotActive = len(g.sessions)
	g.mu.Unlock()
	if g.cfg.HTTPS {
		r.Observado.DNSName = g.cfg.LoginHost
	}
	g.certs.mu.RLock()
	if c := g.certs.cert; c != nil && c.Leaf != nil {
		sum := sha256.Sum256(c.Leaf.Raw)
		r.Observado.SSLFingerprint = hex.EncodeToString(sum[:])
		r.Observado.SSLExpires = c.Leaf.NotAfter.In(offset).Format("Jan/02/2006 15:04:05")
		r.Observado.CertUpdateStatus = "ok:" + r.Observado.SSLFingerprint[:12]
	}
	if e := g.certs.info.Error; e != "" && g.cfg.HTTPS {
		r.Observado.CertUpdateStatus = "error:fetch"
	}
	g.certs.mu.RUnlock()
	return r
}

// localZone returns the router clock's UTC offset as an "Etc/GMT±N" name (POSIX sign: UTC-5 is Etc/GMT+5)
// and as a fixed zone. Go cannot read the POSIX TZ string, so it asks date(1).
func localZone() (string, *time.Location) {
	out, _ := sh("date", "+%z")
	return etcZone(strings.TrimSpace(out))
}

func etcZone(z string) (string, *time.Location) {
	if len(z) != 5 || (z[0] != '+' && z[0] != '-') {
		return "UTC", time.UTC
	}
	h, err1 := strconv.Atoi(z[1:3])
	m, err2 := strconv.Atoi(z[3:5])
	if err1 != nil || err2 != nil || (h == 0 && m == 0) {
		return "UTC", time.UTC
	}
	secs := (h*3600 + m*60)
	if z[0] == '-' {
		secs = -secs
	}
	loc := time.FixedZone(z, secs)
	if m != 0 { // no Etc/GMT name for half hours: report UTC, the expiry is then given in UTC too
		return "UTC", time.UTC
	}
	if z[0] == '-' {
		return fmt.Sprintf("Etc/GMT+%d", h), loc
	}
	return fmt.Sprintf("Etc/GMT-%d", h), loc
}

// mainSSID: the first SSID of the first radio on the LAN (what the panel calls the device's SSID).
func mainSSID() string {
	out, _ := sh("uci", "-q", "show", "wireless")
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && strings.HasSuffix(k, ".ssid") {
			sec := strings.TrimSuffix(k, ".ssid")
			if n, _ := sh("uci", "-q", "get", sec+".network"); strings.TrimSpace(n) != "lan" {
				continue
			}
			if d, _ := sh("uci", "-q", "get", sec+".disabled"); strings.TrimSpace(d) == "1" {
				continue
			}
			return strings.Trim(v, "'")
		}
	}
	return ""
}

func formatUptime(s int64) string {
	return fmt.Sprintf("%dd%02d:%02d:%02d", s/86400, s%86400/3600, s%3600/60, s%60)
}
