package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Same values as omnicap's rosscript.StartJitterSeconds (checked against it): the Notion spreads like a
// MikroTik with the same token.
func TestTokenJitterMatchesOmnicap(t *testing.T) {
	for tok, want := range map[string]int{"tok-123": 14, "e8a1": 35, "abc": 34} {
		if got := tokenJitter(tok, 3600); got != time.Duration(want)*time.Second {
			t.Errorf("%s: %v, want %ds", tok, got, want)
		}
	}
	if got := tokenJitter("e8a1", 60); got != 15*time.Second {
		t.Errorf("jitter not capped to a quarter of the interval: %v", got)
	}
}

func TestPullBackoff(t *testing.T) {
	j := 7 * time.Second
	for failures, want := range []time.Duration{60 * time.Second, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if got := nextPullDelay(60, failures, j); got != want+j {
			t.Errorf("failures=%d: %v, want %v", failures, got, want+j)
		}
	}
	if got := nextPullDelay(7200, 3, 0); got != 30*time.Minute {
		t.Errorf("long interval after failures: %v, want the 30 min cap", got)
	}
}

func TestPullOnceReportsAndAppliesAnswer(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/pull" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"result":"ok","pull_interval_seconds":300,"jitter_seconds":12,"walled_garden":["203.0.113.7"],"commands":{"applied":1,"unsupported":0}}`)
	}))
	defer srv.Close()

	g := testGateway(9)
	g.cfg.AgentURL, g.cfg.AgentToken, g.uciPkg = srv.URL, "tok-abc", "omnicapgw-test"
	g.wgByHost = map[string][]string{}
	g.sessions["aa"] = &Session{MAC: "aa"}
	interval, jitter, err := g.pullOnce()
	if err != nil {
		t.Fatal(err)
	}
	if interval != 300 || jitter != 12 {
		t.Fatalf("interval %d jitter %d", interval, jitter)
	}
	if auth != "Bearer tok-abc" {
		t.Fatalf("authorization %q", auth)
	}
	if got["identity"] != "NAS1" || got["hotspot_active"] != float64(1) {
		t.Fatalf("report %v", got)
	}
	if up, _ := got["uptime"].(string); up != "" && !regexp.MustCompile(`^\d+d\d{2}:\d{2}:\d{2}$`).MatchString(up) {
		t.Fatalf("uptime %q not in omnicap's format", up)
	}
	if obs, _ := got["observado"].(map[string]any); obs["use_radius"] != "true" || obs["radius_address"] != "127.0.0.1" ||
		!strings.HasPrefix(obs["timezone"].(string), "Etc/GMT") && obs["timezone"] != "UTC" {
		t.Fatalf("observado %v", obs)
	}
	if len(g.cfg.WalledGarden) != 1 || g.cfg.WalledGarden[0] != "203.0.113.7" {
		t.Fatalf("walled garden not applied: %v", g.cfg.WalledGarden)
	}
	if !g.pull.OK || g.pull.Interval != 300 {
		t.Fatalf("status %+v", g.pull)
	}
}

func TestPullOnceServerDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	g := testGateway(9)
	g.cfg.AgentURL, g.cfg.AgentToken = srv.URL, "tok-abc"
	if _, _, err := g.pullOnce(); err == nil {
		t.Fatal("429 taken as a good pull (no backoff)")
	}
	if g.pull.OK || g.pull.Error == "" {
		t.Fatalf("status %+v", g.pull)
	}
}

func TestEtcZone(t *testing.T) {
	for in, want := range map[string]string{"-0500": "Etc/GMT+5", "+0800": "Etc/GMT-8", "+0000": "UTC", "+0530": "UTC", "": "UTC"} {
		if got, _ := etcZone(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
	_, loc := etcZone("-0500")
	exp := time.Date(2027, 2, 11, 16, 16, 18, 0, time.UTC)
	if s := exp.In(loc).Format("Jan/02/2006 15:04:05"); s != "Feb/11/2027 11:16:18" {
		t.Fatalf("expiry in the reported zone: %s", s)
	}
}

func TestFormatUptime(t *testing.T) {
	if s := formatUptime(93784); s != "1d02:03:04" {
		t.Fatal(s)
	}
}
