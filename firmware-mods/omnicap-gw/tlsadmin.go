package main

// Secure login (TLS listener with a reloadable certificate), certificate download from the omnicap agent
// (same endpoint RouterOS uses: GET /agent/v1/hotspot-cert/{onmicap-portal.crt,onmicap-portal.key} with
// Authorization: Bearer <agent_token>), login host in dnsmasq, and the local admin endpoint for the web UI.

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CertInfo struct {
	Loaded   bool   `json:"loaded"`
	Subject  string `json:"subject,omitempty"`
	Issuer   string `json:"issuer,omitempty"`
	NotAfter string `json:"not_after,omitempty"`
	Source   string `json:"source,omitempty"` // file | agent
	Error    string `json:"error,omitempty"`
	Fetched  string `json:"fetched,omitempty"`
}

type certStore struct {
	fetch sync.Mutex // one download at a time (startup, 6-hourly and the web UI button may overlap)
	mu    sync.RWMutex
	cert  *tls.Certificate
	info  CertInfo
}

func (cs *certStore) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.cert == nil {
		return nil, errors.New("no certificate")
	}
	return cs.cert, nil
}

func (cs *certStore) ready() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	// an expired certificate is not "ready": the portal falls back to the plain HTTP login instead of
	// sending every phone to a TLS error
	return cs.cert != nil && cs.cert.Leaf != nil && time.Now().Before(cs.cert.Leaf.NotAfter)
}

// parsePair validates a PEM chain + key for host: matching key, hostname covered, not expired.
func parsePair(certPEM, keyPEM []byte, host string) (*tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("certificate/key: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return nil, nil, fmt.Errorf("certificate is not valid for %s", host)
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, nil, fmt.Errorf("certificate expired on %s", leaf.NotAfter.Format("2006-01-02"))
	}
	pair.Leaf = leaf
	return &pair, leaf, nil
}

func (g *Gateway) loadCertFiles(source string) {
	cs := &g.certs
	c, errC := os.ReadFile(g.cfg.CertFile)
	k, errK := os.ReadFile(g.cfg.KeyFile)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if errC != nil || errK != nil {
		cs.cert, cs.info = nil, CertInfo{Error: "no certificate installed"}
		return
	}
	pair, leaf, err := parsePair(c, k, g.cfg.LoginHost)
	if err != nil {
		cs.cert, cs.info = nil, CertInfo{Error: err.Error()}
		return
	}
	prevFetched := cs.info.Fetched
	cs.cert = pair
	cs.info = CertInfo{Loaded: true, Subject: leaf.Subject.CommonName, Issuer: leaf.Issuer.CommonName,
		NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339), Source: source, Fetched: prevFetched}
}

// fetchCert downloads the certificate and key from the omnicap agent and installs them if valid and changed.
func (g *Gateway) fetchCert() error {
	if g.cfg.AgentURL == "" || g.cfg.AgentToken == "" {
		return nil
	}
	g.certs.fetch.Lock()
	defer g.certs.fetch.Unlock()
	c, err := agentGet(g.cfg.AgentURL+"/agent/v1/hotspot-cert/onmicap-portal.crt", g.cfg.AgentToken)
	if err == nil {
		var k []byte
		k, err = agentGet(g.cfg.AgentURL+"/agent/v1/hotspot-cert/onmicap-portal.key", g.cfg.AgentToken)
		if err == nil {
			if _, _, err = parsePair(c, k, g.cfg.LoginHost); err == nil {
				old, _ := os.ReadFile(g.cfg.CertFile)
				if !bytes.Equal(old, c) {
					if err = writeAtomic(g.cfg.KeyFile, k, 0600); err == nil {
						err = writeAtomic(g.cfg.CertFile, c, 0644)
					}
					if err == nil {
						log.Printf("certificate updated from the omnicap agent")
					}
				}
			}
		}
	}
	g.certs.mu.Lock()
	g.certs.info.Fetched = time.Now().UTC().Format(time.RFC3339)
	g.certs.mu.Unlock()
	if err != nil {
		log.Printf("certificate from agent: %v", err)
		g.loadCertFiles("file")
		g.certs.mu.Lock()
		if g.certs.info.Loaded {
			g.certs.info.Error = "last download failed: " + err.Error()
		} else {
			g.certs.info.Error = err.Error()
		}
		g.certs.mu.Unlock()
		return err
	}
	g.loadCertFiles("agent")
	return nil
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// agentGet: GET a PEM file from the omnicap agent.
func agentGet(rawURL, token string) ([]byte, error) {
	body, err := agentRequest("GET", rawURL, token, nil)
	if err != nil {
		return nil, err
	}
	if p, _ := pem.Decode(body); p == nil {
		return nil, errors.New("agent did not return PEM")
	}
	return body, nil
}

// agentRequest: minimal HTTP(S) client (no net/http, it doubles the binary). Verifies TLS with the system
// roots. Returns the body of a 200 answer.
func agentRequest(method, rawURL, token string, payload []byte) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := secureURL(u); err != nil {
		return nil, err
	}
	host := u.Hostname()
	port := u.Port()
	var conn net.Conn
	d := &net.Dialer{Timeout: 15 * time.Second}
	switch u.Scheme {
	case "https":
		if port == "" {
			port = "443"
		}
		conn, err = tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, port), &tls.Config{ServerName: host})
	case "http":
		if port == "" {
			port = "80"
		}
		conn, err = d.Dial("tcp", net.JoinHostPort(host, port))
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	auth := ""
	if token != "" {
		auth = "Authorization: Bearer " + token + "\r\n"
	}
	ctype := ""
	if payload != nil {
		ctype = fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(payload))
	}
	fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\n%s%sUser-Agent: omnicap-gw\r\nConnection: close\r\n\r\n",
		method, u.RequestURI(), u.Host, auth, ctype)
	if payload != nil {
		conn.Write(payload)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	f := strings.Fields(status)
	if len(f) < 2 || f[1] != "200" {
		return nil, fmt.Errorf("agent answered %s", strings.TrimSpace(status))
	}
	chunked, clen := false, -1
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		k, v, _ := strings.Cut(h, ":")
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "transfer-encoding":
			chunked = strings.Contains(strings.ToLower(v), "chunked")
		case "content-length":
			clen, _ = strconv.Atoi(v)
		}
	}
	var body []byte
	switch {
	case chunked:
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return nil, err
			}
			n, err := strconv.ParseInt(strings.TrimSpace(strings.Split(l, ";")[0]), 16, 64)
			if err != nil || n < 0 || n > 1<<20 {
				return nil, errors.New("bad chunk")
			}
			if n == 0 {
				break
			}
			chunk := make([]byte, n+2)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return nil, err
			}
			body = append(body, chunk[:n]...)
		}
	case clen >= 0:
		if clen > 1<<20 {
			return nil, errors.New("body too large")
		}
		body = make([]byte, clen)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, err
		}
	default:
		body, err = io.ReadAll(io.LimitReader(br, 1<<20))
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// secureURL: the agent API carries the fleet RADIUS secret, the agent token and the TLS private key, so it
// must be https. Plain http only for a lab server on a private address.
func secureURL(u *url.URL) error {
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("%s must use https (plain http only for a private lab address)", u.Host)
	}
	return fmt.Errorf("unsupported scheme %q", u.Scheme)
}

// syncLoginHost makes dnsmasq answer LoginHost with the router address (/tmp/hosts is an addn-hosts dir,
// reloaded on SIGHUP; nothing persistent is touched).
func (g *Gateway) syncLoginHost(on bool) {
	const f = "/tmp/hosts/omnicap-gw"
	want := ""
	if on && g.cfg.LoginHost != "" {
		want = g.cfg.RouterIP + " " + g.cfg.LoginHost + "\n"
	}
	cur, _ := os.ReadFile(f)
	if string(cur) == want {
		return
	}
	if st, err := os.Stat("/tmp/hosts"); err != nil || !st.IsDir() {
		log.Printf("dnsmasq: /tmp/hosts is not a directory, login host not published")
		return
	}
	if want == "" {
		os.Remove(f)
	} else if err := os.WriteFile(f, []byte(want), 0644); err != nil {
		log.Printf("dnsmasq hosts: %v", err)
		return
	}
	shq("killall", "-HUP", "dnsmasq")
}

func (g *Gateway) httpsActive() bool { return g.cfg.HTTPS && g.certs.ready() }

func (g *Gateway) loginURL() string {
	if g.httpsActive() {
		if g.cfg.HTTPSPort == 443 {
			return "https://" + g.cfg.LoginHost + "/login"
		}
		return fmt.Sprintf("https://%s:%d/login", g.cfg.LoginHost, g.cfg.HTTPSPort)
	}
	return fmt.Sprintf("http://%s:%d/login", g.cfg.RouterIP, g.cfg.ListenPort)
}

func (g *Gateway) serveTLS() {
	ln, err := tls.Listen("tcp", fmt.Sprintf(":%d", g.cfg.HTTPSPort), &tls.Config{
		GetCertificate: g.certs.get, MinVersion: tls.VersionTLS12})
	if err != nil {
		log.Printf("https listen: %v", err)
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		g.accept(c)
	}
}

// ---------------------------------------------------------------- admin (127.0.0.1 only, for the web UI CGI)

type adminSession struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	User     string `json:"user"`
	Minutes  int    `json:"minutes"`
	Left     int    `json:"left_minutes"`
	In       uint64 `json:"in"`
	Out      uint64 `json:"out"`
	RateUp   int    `json:"up_kbit"`
	RateDown int    `json:"down_kbit"`
}

func (g *Gateway) adminStatus() []byte {
	g.mu.Lock()
	var ss []adminSession
	for _, s := range g.sessions {
		left := 0
		if s.SessionTimeout > 0 {
			left = int((time.Duration(s.SessionTimeout)*time.Second - time.Since(s.Start)).Minutes())
		}
		ss = append(ss, adminSession{s.MAC, s.IP, s.Username, int(time.Since(s.Start).Minutes()), left,
			s.InOctets, s.OutOctets, s.UpKbit, s.DownKbit})
	}
	radiusOK, radiusErr, pull := g.radiusOK, g.radiusErr, g.pull
	g.mu.Unlock()
	g.certs.mu.RLock()
	ci := g.certs.info
	g.certs.mu.RUnlock()
	b, _ := json.Marshal(map[string]any{
		"running": true, "sessions": ss, "cert": ci, "https": g.cfg.HTTPS, "https_active": g.httpsActive(),
		"login_url": g.loginURL(), "radius_ok": radiusOK, "radius_error": radiusErr, "pull": pull,
		"started": g.started.UTC().Format(time.RFC3339),
	})
	return b
}

func (g *Gateway) serveAdmin() {
	ln, err := net.Listen("tcp", g.cfg.AdminAddr)
	if err != nil {
		log.Printf("admin listen: %v", err)
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			r, err := readRequest(c)
			if err != nil {
				return
			}
			var body []byte
			switch r.Path {
			case "/admin/status":
				body = g.adminStatus()
			case "/admin/kick":
				mac := strings.ToLower(r.Form.Get("mac"))
				g.mu.Lock()
				s := g.sessions[mac]
				g.mu.Unlock()
				if s != nil {
					g.endSession(s, 6) // Admin-Reset
				}
				body = []byte(`{"result":"ok"}`)
			case "/admin/fetchcert":
				if err := g.fetchCert(); err != nil {
					body, _ = json.Marshal(map[string]string{"result": "fail", "error": err.Error()})
				} else {
					body = []byte(`{"result":"ok"}`)
				}
			default:
				body = []byte(`{"result":"fail"}`)
			}
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n", len(body))
			c.Write(body)
		}(c)
	}
}
