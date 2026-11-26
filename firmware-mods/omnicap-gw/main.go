// omnicap-gw: captive portal gateway for the Notion 5G (OpenWrt/ASR1901) that behaves like the RouterOS
// hotspot as omnicap uses it (docs in README.md):
//
//   - unauthenticated clients: HTTP is redirected to this daemon, which serves the omnicap "trampoline"
//     (auto-submitting form to <portal>/login with the same 12 fields RouterOS fills in login.html);
//     DNS goes to the router; everything else is rejected except the walled garden.
//   - the portal makes the client's browser POST username/password to link_login_only (this daemon);
//     the daemon authenticates with RADIUS (PAP, fleet mode: NAS-Identifier + shared secret), opens the
//     firewall for the client's MAC, applies Mikrotik-Rate-Limit with tc and renders the "alogin"
//     trampoline (to <portal>/conectado).
//   - RADIUS accounting Start / Interim-Update / Stop with the Class echoed, octet counters from iptables,
//     Session-Timeout, Idle-Timeout, Mikrotik-Total-Limit / Mikrotik-Xmit-Limit enforced locally.
//
// Runs as a single long-lived process (procd/cron keeps it up). All firewall state lives in its own chains
// and ipsets and is re-applied if fw3 flushes it.
package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------- config

type Config struct {
	LanIf          string   // br-lan
	WanIf          string   // ccinet0 (upload shaping)
	RouterIP       string   // 192.168.1.1
	ListenPort     int      // 8002
	PortalURL      string   // https://cautivo.h3s-iot.com/portal/<ref> (no trailing /login)
	NASIdentifier  string   // device.nas_identifier in omnicap
	RadiusServer   string   // host (auth 1812, acct 1813)
	RadiusAuthPort int      // default 1812
	RadiusAcctPort int      // default 1813
	RadiusSecret   string   // fleet secret
	WalledGarden   []string // hostnames, IPs or CIDRs reachable without login
	BypassMACs     []string // never captured (e.g. the management PC)
	// ExcludeIfaces: LAN bridge ports (exact interface names, "+" = prefix wildcard) whose clients are not
	// captured, e.g. eth0.2 (cable) or wlan0.1 (guest SSID). Empty = capture every port.
	ExcludeIfaces  []string
	HotspotName    string // Called-Station-Id, default "hotspot1"
	DefaultInterim int
	StateFile      string
	// Secure login (like RouterOS "login-by=https" with the omnicap certificate): the portal posts the
	// credential to https://<LoginHost>:<HTTPSPort>/login; dnsmasq resolves LoginHost to the router.
	HTTPS      bool
	LoginHost  string // login.h3s-iot.com
	HTTPSPort  int    // 8443 (443 belongs to the web UI)
	CertFile   string // PEM chain, from the web UI or from the omnicap agent
	KeyFile    string
	AgentURL   string // https://cautivo.h3s-iot.com: fetch the certificate like RouterOS does
	AgentToken string // device agent_token (Authorization: Bearer)
	AdminAddr  string // 127.0.0.1:8009, status/kick for the web UI CGI
	// RequireReplyMA: Access-Accept/Reject must carry a valid Message-Authenticator (Blast-RADIUS,
	// CVE-2024-3596). On by default; uci require_reply_ma '0' only for an old RADIUS server without it.
	RequireReplyMA bool
}

func defaultConfig() *Config {
	return &Config{LanIf: "br-lan", WanIf: "ccinet0", RouterIP: "192.168.1.1", ListenPort: 8002,
		RadiusAuthPort: 1812, RadiusAcctPort: 1813, HotspotName: "hotspot1", DefaultInterim: 300,
		StateFile: "/tmp/omnicap-gw.state", LoginHost: "login.h3s-iot.com", HTTPSPort: 8443,
		CertFile: "/data/omnicap-gw/login.crt", KeyFile: "/data/omnicap-gw/login.key", AdminAddr: "127.0.0.1:8009"}
}

func (c *Config) finish() (*Config, error) {
	if c.PortalURL == "" || c.NASIdentifier == "" || c.RadiusServer == "" || c.RadiusSecret == "" {
		return nil, errors.New("portal_url, nas_identifier, radius_server and radius_secret are required")
	}
	c.PortalURL = strings.TrimSuffix(strings.TrimSuffix(c.PortalURL, "/"), "/login")
	c.AgentURL = strings.TrimSuffix(c.AgentURL, "/")
	return c, nil
}

// loadUCI reads the config written by the web UI (uci package, section "main").
func loadUCI(pkg string) (*Config, error) {
	out, err := sh("uci", "-q", "show", pkg+".main")
	if err != nil {
		return nil, fmt.Errorf("uci %s: %v", pkg, err)
	}
	c := defaultConfig()
	vals := map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k = strings.TrimPrefix(k, pkg+".main.")
		var items []string
		for _, f := range strings.Split(v, "' '") {
			items = append(items, strings.Trim(f, "'"))
		}
		vals[k] = items
	}
	str := func(k string) string {
		if v := vals[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	num := func(k string, d int) int {
		if n, err := strconv.Atoi(str(k)); err == nil && n > 0 {
			return n
		}
		return d
	}
	list := func(k string) []string {
		var out []string
		for _, v := range vals[k] {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	c.PortalURL, c.NASIdentifier, c.RadiusServer, c.RadiusSecret = str("portal_url"), str("nas_identifier"), str("radius_server"), str("radius_secret")
	c.RadiusAuthPort, c.RadiusAcctPort = num("radius_auth_port", 1812), num("radius_acct_port", 1813)
	c.WalledGarden, c.BypassMACs = list("walled_garden"), list("bypass_mac")
	for _, i := range list("exclude_if") {
		if validIface(i) {
			c.ExcludeIfaces = append(c.ExcludeIfaces, i)
		} else {
			log.Printf("exclude_if %q ignored: not an interface name", i)
		}
	}
	c.HTTPS = str("https") == "1"
	if h := str("login_host"); h != "" {
		c.LoginHost = h
	}
	c.HTTPSPort = num("https_port", 8443)
	c.RequireReplyMA = str("require_reply_ma") != "0"
	if str("cert_source") == "agent" {
		c.AgentURL, c.AgentToken = str("agent_url"), str("agent_token")
	}
	// the LAN address may be changed in the vendor UI: redirects, the login host and NAS-IP follow it
	if out, err := sh("uci", "-q", "get", "network.lan.ipaddr"); err == nil {
		if ip := net.ParseIP(strings.TrimSpace(strings.SplitN(out, "/", 2)[0])); ip != nil && ip.To4() != nil {
			c.RouterIP = ip.String()
		}
	}
	return c.finish()
}

// ---------------------------------------------------------------- shell helpers

func sh(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func shq(name string, args ...string) { _, _ = sh(name, args...) }

// ---------------------------------------------------------------- sessions

type Session struct {
	MAC, IP, Username, Class, AcctID string
	Start                            time.Time
	SessionTimeout, IdleTimeout      int    // seconds, 0 = none
	Interim                          int    // seconds
	UpKbit, DownKbit                 int    // 0 = unlimited
	TotalLimit, XmitLimit            uint64 // bytes, 0 = none
	InOctets, OutOctets              uint64 // client upload / download
	LastActivity                     time.Time
	LastInterim                      time.Time
	End                              time.Time `json:",omitempty"` // set when the session ends (Stop retries keep it)
	Slot                             int       // tc class minor / fw mark slot
	// last raw iptables counters seen; not saved, so after a restart (chains flushed, counters back to 0)
	// the next reading counts as new traffic instead of freezing the totals
	rawIn, rawOut uint64
}

type pendingStop struct {
	s     *Session
	cause uint32
}

type Gateway struct {
	cfg      *Config
	mu       sync.Mutex
	sessions map[string]*Session // by MAC
	slots    map[int]bool
	wgByHost map[string][]string // walled garden: last good addresses per host
	nasPort  uint32
	stops    []pendingStop          // Accounting-Stop not answered yet, retried every tick
	fails    map[string][]time.Time // failed logins per MAC (last minute)
	connSem  chan struct{}          // caps concurrent client connections (8002 + HTTPS)
	connMu   sync.Mutex             // guards connIP (not g.mu: setupFirewall holds that for seconds)
	connIP   map[string]int         // open client connections per source IP
	acctBusy atomic.Bool            // an accounting round (Stops, Interims, retries) is talking to RADIUS
	certs    certStore
	started  time.Time
	// last RADIUS outcome, for the web UI status
	radiusOK  bool
	radiusErr string
	pull      pullStatus // last report to omnicap, for the web UI
	uciPkg    string
	wgMu      sync.Mutex // one walled-garden refresh at a time (ticker and pull answers)
}

const (
	chainFwd  = "OCGW_FWD"
	chainAcct = "OCGW_ACCT"
	chainNat  = "OCGW_NAT"
	chainMark = "OCGW_MARK"
	chainIn   = "OCGW_IN"   // hotspot clients -> the router itself: only DNS, DHCP, ping and this portal
	chainDown = "OCGW_DOWN" // ocgw.sh: LAN blocked while the daemon is not up (fail closed); removed here
	setAuth   = "ocgw_auth"
	setWG     = "ocgw_wg"
	setBypass = "ocgw_bypass"
	markMask  = 0x7f000000
	// Port selection: br-lan bridges the Wi-Fi SSIDs (main and guest), the cable LAN port and USB tethering,
	// and this kernel has no br_netfilter/physdev, so iptables cannot see the bridge port. ebtables marks
	// frames that enter on an excluded port (uci list exclude_if, exact names read by the web UI from the
	// device itself); the OCGW chains return early on that mark. Nothing excluded = no ebtables rules, and a
	// port nobody excluded (e.g. a guest SSID enabled later) is always captured (fail closed).
	chainPorts = "OCGW_PORTS"
	// vendor fastpath (ASR1901): 1 = every forwarded packet goes through netfilter (and so through our counters)
	fastpathBypass = "/sys/kernel/fastpath/fp_forward/bypass_fastpath"
	exemptMark     = 0x00800000
)

// validIface accepts a Linux interface name, optionally with the ebtables "+" prefix wildcard.
func validIface(s string) bool {
	n := strings.TrimSuffix(s, "+")
	if n == "" || len(n) > 15 || n[0] == '-' {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

var exemptMatch = []string{"-m", "mark", "--mark", fmt.Sprintf("0x%x/0x%x", exemptMark, exemptMark), "-j", "RETURN"}

// setupPorts (re)creates the ebtables marking for the ports left out of the portal. Idempotent.
func (g *Gateway) setupPorts() error {
	ex := g.cfg.ExcludeIfaces
	if len(ex) == 0 {
		g.teardownPorts()
		return nil
	}
	shq("ebtables", "-t", "nat", "-N", chainPorts, "-P", "RETURN")
	if _, err := sh("ebtables", "-t", "nat", "-F", chainPorts); err != nil {
		return err
	}
	for _, i := range ex {
		if _, err := sh("ebtables", "-t", "nat", "-A", chainPorts, "-i", i, "-j", "mark",
			"--mark-or", fmt.Sprintf("0x%x", exemptMark), "--mark-target", "CONTINUE"); err != nil {
			return err
		}
	}
	if !g.portsJumpPresent() {
		if _, err := sh("ebtables", "-t", "nat", "-I", "PREROUTING", "1", "-j", chainPorts); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gateway) portsJumpPresent() bool {
	out, _ := sh("ebtables", "-t", "nat", "-L", "PREROUTING")
	return strings.Contains(out, "-j "+chainPorts)
}

func (g *Gateway) teardownPorts() {
	for g.portsJumpPresent() {
		if _, err := sh("ebtables", "-t", "nat", "-D", "PREROUTING", "-j", chainPorts); err != nil {
			break
		}
	}
	shq("ebtables", "-t", "nat", "-X", chainPorts)
}

// ---------------------------------------------------------------- firewall

func (g *Gateway) firewallPresent() bool {
	out, _ := sh("iptables", "-S", "FORWARD")
	nat, _ := sh("iptables", "-t", "nat", "-S", "PREROUTING")
	mg, _ := sh("iptables", "-t", "mangle", "-S", "FORWARD")
	in, _ := sh("iptables", "-S", "INPUT")
	in6, _ := sh("ip6tables", "-S", "INPUT")
	fwd6, _ := sh("ip6tables", "-S", "FORWARD")
	return strings.Contains(out, "-j "+chainFwd) && strings.Contains(out, "-j "+chainAcct) &&
		strings.Contains(fwd6, "-j "+chainFwd) &&
		strings.Contains(nat, "-j "+chainNat) && strings.Contains(mg, "-j "+chainMark) &&
		strings.Contains(in, "-j "+chainIn) && strings.Contains(in6, "-j "+chainIn) &&
		(len(g.cfg.ExcludeIfaces) == 0 || g.portsJumpPresent())
}

// setupFirewall (re)creates every chain, set and jump. Idempotent.
func (g *Gateway) setupFirewall() error {
	c := g.cfg
	for _, t := range []struct{ table, chain string }{{"filter", chainFwd}, {"filter", chainAcct}, {"filter", chainIn}, {"nat", chainNat}, {"mangle", chainMark}} {
		shq("iptables", "-t", t.table, "-N", t.chain)
		shq("iptables", "-t", t.table, "-F", t.chain)
	}
	for _, ch := range []string{chainIn, chainFwd} {
		shq("ip6tables", "-N", ch)
		shq("ip6tables", "-F", ch)
	}
	// iptables and tc must see every forwarded packet (accounting, limits): the vendor fastpath would skip them
	if _, err := os.Stat(fastpathBypass); err == nil {
		if err := os.WriteFile(fastpathBypass, []byte("1"), 0644); err != nil {
			log.Printf("fastpath bypass: %v (accounting and limits may miss accelerated flows)", err)
		}
	}
	// Access is bound to IP+MAC: a client that changes its IP is captured again, so the IP-based
	// counters and tc classes always cover what it sends. Older builds used hash:mac under the same name;
	// the chains above no longer reference it, so it can be replaced.
	if out, _ := sh("ipset", "list", "-t", setAuth); out != "" && !strings.Contains(out, "hash:ip,mac") {
		shq("ipset", "destroy", setAuth)
	}
	shq("ipset", "create", setAuth, "hash:ip,mac", "-exist")
	shq("ipset", "create", setBypass, "hash:mac", "-exist")
	shq("ipset", "create", setWG, "hash:net", "-exist")
	shq("ipset", "flush", setBypass) // a MAC removed in the web UI loses its bypass now, not at reboot
	for _, m := range c.BypassMACs {
		shq("ipset", "add", setBypass, strings.ToLower(m), "-exist")
	}
	// filter: captive enforcement for traffic coming from the LAN
	rules := [][]string{
		{"-A", chainFwd, "-o", c.LanIf, "-j", "RETURN"},
		append([]string{"-A", chainFwd}, exemptMatch...),
		{"-A", chainFwd, "-m", "set", "--match-set", setBypass, "src", "-j", "RETURN"},
		{"-A", chainFwd, "-m", "set", "--match-set", setAuth, "src,src", "-j", "RETURN"},
		{"-A", chainFwd, "-m", "set", "--match-set", setWG, "dst", "-j", "RETURN"},
		{"-A", chainFwd, "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset"},
		{"-A", chainFwd, "-j", "REJECT", "--reject-with", "icmp-port-unreachable"},
	}
	for _, r := range rules {
		if _, err := sh("iptables", r...); err != nil {
			return err
		}
	}
	// IPv6: login, accounting and shaping are IPv4 only, so hotspot clients get no IPv6 forwarding at all
	// (apps fall back to IPv4 at once on the reject); excluded ports and bypass MACs keep it.
	r6 := [][]string{
		{"-A", chainFwd, "-o", c.LanIf, "-j", "RETURN"},
		append([]string{"-A", chainFwd}, exemptMatch...),
	}
	for _, m := range c.BypassMACs {
		r6 = append(r6, []string{"-A", chainFwd, "-m", "mac", "--mac-source", strings.ToLower(m), "-j", "RETURN"})
	}
	r6 = append(r6, []string{"-A", chainFwd, "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset"},
		[]string{"-A", chainFwd, "-j", "REJECT", "--reject-with", "icmp6-adm-prohibited"})
	for _, r := range r6 {
		if _, err := sh("ip6tables", r...); err != nil {
			return err
		}
	}
	// nat: HTTP of unauthenticated clients -> this daemon; DNS -> router
	nrules := [][]string{
		append([]string{"-A", chainNat}, exemptMatch...),
		{"-A", chainNat, "-m", "set", "--match-set", setBypass, "src", "-j", "RETURN"},
		{"-A", chainNat, "-m", "set", "--match-set", setAuth, "src,src", "-j", "RETURN"},
		{"-A", chainNat, "-d", c.RouterIP, "-j", "RETURN"},
		{"-A", chainNat, "-m", "set", "--match-set", setWG, "dst", "-j", "RETURN"},
		{"-A", chainNat, "-p", "udp", "--dport", "53", "-j", "REDIRECT", "--to-ports", "53"},
		{"-A", chainNat, "-p", "tcp", "--dport", "53", "-j", "REDIRECT", "--to-ports", "53"},
		{"-A", chainNat, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-ports", strconv.Itoa(c.ListenPort)},
	}
	for _, r := range nrules {
		if _, err := sh("iptables", append([]string{"-t", "nat"}, r...)...); err != nil {
			return err
		}
	}
	if err := g.setupInput(); err != nil {
		return err
	}
	// jumps (first position, ahead of the vendor and fw3 chains)
	jumps := [][]string{
		{"iptables", "filter", "FORWARD", "-i", c.LanIf, "-j", chainFwd},
		{"ip6tables", "filter", "FORWARD", "-i", c.LanIf, "-j", chainFwd},
		{"iptables", "filter", "FORWARD", "-j", chainAcct},
		{"iptables", "filter", "INPUT", "-i", c.LanIf, "-j", chainIn},
		{"ip6tables", "filter", "INPUT", "-i", c.LanIf, "-j", chainIn},
		{"iptables", "nat", "PREROUTING", "-i", c.LanIf, "-j", chainNat},
		{"iptables", "mangle", "FORWARD", "-i", c.LanIf, "-j", chainMark},
	}
	for _, j := range jumps {
		args := append([]string{"-t", j[1], "-C", j[2]}, j[3:]...)
		if _, err := sh(j[0], args...); err != nil {
			args[2] = "-I"
			args = append(args[:4], append([]string{"1"}, args[4:]...)...)
			if _, err := sh(j[0], args...); err != nil {
				return err
			}
		}
	}
	if err := g.setupPorts(); err != nil {
		return err
	}
	// re-add per-session state
	for _, s := range g.sessions {
		g.applySession(s)
	}
	removeDown()
	return nil
}

// removeDown deletes the fail-closed block ocgw.sh puts in place while the daemon is not up.
func removeDown() {
	for _, ipt := range []string{"iptables", "ip6tables"} {
		for {
			if _, err := sh(ipt, "-D", "FORWARD", "-i", "br-lan", "-j", chainDown); err != nil {
				break
			}
		}
		shq(ipt, "-F", chainDown)
		shq(ipt, "-X", chainDown)
	}
}

// ensureFirewall re-applies the rules when fw3 flushed them (fw3 reloads on every 4G/5G change). Called
// every tick and, right away, by the fw3 include (ocgw.sh -> /admin/reapply).
func (g *Gateway) ensureFirewall() {
	if g.firewallPresent() {
		return
	}
	log.Printf("firewall rules missing (fw3 reload?), re-applying")
	g.mu.Lock()
	err := g.setupFirewall()
	g.mu.Unlock()
	if err != nil {
		log.Printf("setup firewall: %v", err)
	}
}

// setupInput: hotspot clients may only reach the router for DNS, DHCP, ping and this portal. The admin
// web UI, SSH and the vendor services (8081, UDP 2345/3456, ...) are closed to them; the admin uses an
// excluded port (e.g. the cable), a bypass MAC or the management VPN (tun0, not br-lan).
func (g *Gateway) setupInput() error {
	c := g.cfg
	open := func(ipt string, rules ...[]string) error {
		for _, r := range rules {
			if _, err := sh(ipt, append([]string{"-A", chainIn}, r...)...); err != nil {
				return err
			}
		}
		return nil
	}
	ports := []string{strconv.Itoa(c.ListenPort), strconv.Itoa(c.HTTPSPort)}
	common := [][]string{
		exemptMatch, // excluded ports (e.g. the admin cable) keep full access to the router
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "--ctdir", "REPLY", "-j", "RETURN"}, // answers to the router's own connections
		{"-p", "udp", "--dport", "53", "-j", "RETURN"},
		{"-p", "tcp", "--dport", "53", "-j", "RETURN"},
		{"-p", "tcp", "-m", "multiport", "--dports", strings.Join(ports, ","), "-j", "RETURN"},
	}
	for _, m := range c.BypassMACs {
		common = append(common, []string{"-m", "mac", "--mac-source", strings.ToLower(m), "-j", "RETURN"})
	}
	v4 := append(common,
		[]string{"-p", "udp", "--dport", "67", "-j", "RETURN"},
		[]string{"-p", "icmp", "-j", "RETURN"},
		[]string{"-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset"},
		[]string{"-j", "REJECT", "--reject-with", "icmp-port-unreachable"},
	)
	v6 := append(append([][]string{}, common...),
		[]string{"-p", "udp", "--dport", "547", "-j", "RETURN"},
		[]string{"-p", "ipv6-icmp", "-j", "RETURN"},
		[]string{"-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset"},
		[]string{"-j", "REJECT", "--reject-with", "icmp6-port-unreachable"},
	)
	if err := open("iptables", v4...); err != nil {
		return err
	}
	return open("ip6tables", v6...)
}

func (g *Gateway) teardownFirewall() {
	c := g.cfg
	for {
		if _, err := sh("iptables", "-D", "FORWARD", "-i", c.LanIf, "-j", chainFwd); err != nil {
			break
		}
	}
	shq("iptables", "-D", "FORWARD", "-j", chainAcct)
	shq("iptables", "-D", "INPUT", "-i", c.LanIf, "-j", chainIn)
	shq("ip6tables", "-D", "INPUT", "-i", c.LanIf, "-j", chainIn)
	for {
		if _, err := sh("ip6tables", "-D", "FORWARD", "-i", c.LanIf, "-j", chainFwd); err != nil {
			break
		}
	}
	for _, ch := range []string{chainIn, chainFwd} {
		shq("ip6tables", "-F", ch)
		shq("ip6tables", "-X", ch)
	}
	removeDown()
	shq("iptables", "-t", "nat", "-D", "PREROUTING", "-i", c.LanIf, "-j", chainNat)
	shq("iptables", "-t", "mangle", "-D", "FORWARD", "-i", c.LanIf, "-j", chainMark)
	for _, t := range []struct{ table, chain string }{{"filter", chainFwd}, {"filter", chainAcct}, {"filter", chainIn}, {"nat", chainNat}, {"mangle", chainMark}} {
		shq("iptables", "-t", t.table, "-F", t.chain)
		shq("iptables", "-t", t.table, "-X", t.chain)
	}
	g.teardownPorts()
	for _, s := range []string{setAuth, setWG, setBypass} {
		shq("ipset", "destroy", s)
	}
	for _, s := range g.sessions {
		g.removeShaping(s)
	}
}

// refreshWalledGarden resolves the configured hosts (dnsmasq here has no ipset support) and replaces the
// whole set, so a host removed in the web UI or an address a CDN dropped stops being reachable. A host
// that does not resolve right now keeps its last good addresses.
func (g *Gateway) refreshWalledGarden() {
	g.wgMu.Lock()
	defer g.wgMu.Unlock()
	g.mu.Lock() // the walled garden can change from omnicap (pull.go)
	hosts := append([]string{}, g.cfg.WalledGarden...)
	g.mu.Unlock()
	if u, err := url.Parse(g.cfg.PortalURL); err == nil {
		hosts = append(hosts, u.Hostname())
	}
	next := map[string][]string{}
	for _, h := range hosts {
		var addrs []string
		if _, n, err := net.ParseCIDR(h); err == nil {
			addrs = []string{n.String()}
		} else if ip := net.ParseIP(h); ip != nil {
			addrs = []string{ip.String()}
		} else if ips, err := net.LookupIP(h); err == nil {
			for _, ip := range ips {
				if ip.To4() != nil {
					addrs = append(addrs, ip.String())
				}
			}
		}
		if len(addrs) == 0 {
			addrs = g.wgByHost[h]
		}
		next[h] = addrs
	}
	tmp := setWG + "_n"
	shq("ipset", "create", tmp, "hash:net", "-exist")
	shq("ipset", "flush", tmp)
	for _, addrs := range next {
		for _, a := range addrs {
			shq("ipset", "add", tmp, a, "-exist")
		}
	}
	if _, err := sh("ipset", "swap", tmp, setWG); err != nil {
		log.Printf("walled garden: %v", err)
	}
	shq("ipset", "destroy", tmp)
	g.wgByHost = next
}

func (g *Gateway) applySession(s *Session) {
	shq("ipset", "add", setAuth, s.IP+","+s.MAC, "-exist")
	s.rawIn, s.rawOut = 0, 0 // fresh counter rules
	shq("iptables", "-A", chainAcct, "-s", s.IP, "-j", "RETURN")
	shq("iptables", "-A", chainAcct, "-d", s.IP, "-j", "RETURN")
	g.applyShaping(s)
}

func (g *Gateway) revokeSession(s *Session) {
	shq("ipset", "del", setAuth, s.IP+","+s.MAC)
	shq("iptables", "-D", chainAcct, "-s", s.IP, "-j", "RETURN")
	shq("iptables", "-D", chainAcct, "-d", s.IP, "-j", "RETURN")
	g.removeShaping(s)
	// drop established flows of this client so access ends now, not when they time out
	shq("conntrack", "-D", "-s", s.IP)
}

// tc: download = class under the LAN egress HTB, upload = fwmark + class under the WAN egress HTB.
// Both roots exist (vendor qos daemon); if not, applyShaping creates one (create=true). Cleanup never
// creates one: htbRoot(dev, false) returns "" when the device has no HTB root.
func htbRoot(dev string, create bool) string {
	out, _ := sh("tc", "qdisc", "show", "dev", dev)
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "qdisc" && f[1] == "htb" && strings.Contains(l, " root") {
			return strings.TrimSuffix(f[2], ":")
		}
	}
	if !create {
		return ""
	}
	shq("tc", "qdisc", "add", "dev", dev, "root", "handle", "77:", "htb", "default", "0")
	return "77"
}

func (g *Gateway) applyShaping(s *Session) {
	if s.DownKbit > 0 {
		r := htbRoot(g.cfg.LanIf, true)
		cls := fmt.Sprintf("%s:%x", r, 0x100+s.Slot)
		shq("tc", "class", "replace", "dev", g.cfg.LanIf, "parent", r+":", "classid", cls, "htb",
			"rate", fmt.Sprintf("%dkbit", s.DownKbit), "ceil", fmt.Sprintf("%dkbit", s.DownKbit))
		shq("tc", "filter", "add", "dev", g.cfg.LanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", fmt.Sprintf("800::%x", 0x100+s.Slot), "u32", "match", "ip", "dst", s.IP+"/32", "flowid", cls)
	}
	if s.UpKbit > 0 {
		mark := fmt.Sprintf("0x%x/0x%x", (0x01000000*(s.Slot+1))&markMask, markMask)
		shq("iptables", "-t", "mangle", "-A", chainMark, "-s", s.IP, "-j", "MARK", "--set-xmark", mark)
		r := htbRoot(g.cfg.WanIf, true)
		cls := fmt.Sprintf("%s:%x", r, 0x100+s.Slot)
		shq("tc", "class", "replace", "dev", g.cfg.WanIf, "parent", r+":", "classid", cls, "htb",
			"rate", fmt.Sprintf("%dkbit", s.UpKbit), "ceil", fmt.Sprintf("%dkbit", s.UpKbit))
		shq("tc", "filter", "add", "dev", g.cfg.WanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", mark, "fw", "flowid", cls)
	}
}

func (g *Gateway) removeShaping(s *Session) {
	if r := htbRoot(g.cfg.LanIf, false); r != "" {
		shq("tc", "filter", "del", "dev", g.cfg.LanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", fmt.Sprintf("800::%x", 0x100+s.Slot), "u32")
		shq("tc", "class", "del", "dev", g.cfg.LanIf, "classid", fmt.Sprintf("%s:%x", r, 0x100+s.Slot))
	}
	mark := fmt.Sprintf("0x%x/0x%x", (0x01000000*(s.Slot+1))&markMask, markMask)
	shq("iptables", "-t", "mangle", "-D", chainMark, "-s", s.IP, "-j", "MARK", "--set-xmark", mark)
	if r := htbRoot(g.cfg.WanIf, false); r != "" {
		shq("tc", "filter", "del", "dev", g.cfg.WanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", mark, "fw")
		shq("tc", "class", "del", "dev", g.cfg.WanIf, "classid", fmt.Sprintf("%s:%x", r, 0x100+s.Slot))
	}
}

// readCounters returns bytes per IP from OCGW_ACCT: src = client upload, dst = client download.
func readCounters() (up, down map[string]uint64, ok bool) {
	up, down = map[string]uint64{}, map[string]uint64{}
	out, err := sh("iptables", "-L", chainAcct, "-n", "-v", "-x")
	if err != nil {
		return up, down, false
	}
	ok = true
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		// pkts bytes target prot opt in out source destination
		if len(f) < 9 || f[2] != "RETURN" {
			continue
		}
		b, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if f[7] != "0.0.0.0/0" {
			up[f[7]] += b
		} else if f[8] != "0.0.0.0/0" {
			down[f[8]] += b
		}
	}
	return
}

// ---------------------------------------------------------------- RADIUS (RFC 2865/2866, PAP, Message-Authenticator)

const (
	attrUserName       = 1
	attrUserPassword   = 2
	attrNASIPAddress   = 4
	attrNASPort        = 5
	attrServiceType    = 6
	attrFramedIP       = 8
	attrReplyMessage   = 18
	attrClass          = 25
	attrVendor         = 26
	attrSessionTimeout = 27
	attrIdleTimeout    = 28
	attrCalledStation  = 30
	attrCallingStation = 31
	attrNASIdentifier  = 32
	attrAcctStatus     = 40
	attrAcctInOctets   = 42
	attrAcctOutOctets  = 43
	attrAcctSessionID  = 44
	attrAcctSessTime   = 46
	attrAcctTermCause  = 49
	attrAcctInGiga     = 52
	attrAcctOutGiga    = 53
	attrEventTimestamp = 55
	attrNASPortType    = 61
	attrInterim        = 85
	attrMsgAuth        = 80
	vendorMikrotik     = 14988
	mtRateLimit        = 8
	mtXmitLimit        = 2
	mtTotalLimit       = 17
	mtXmitLimitGiga    = 14
	mtTotalLimitGiga   = 18
)

type attr struct {
	t byte
	v []byte
}

func aStr(t byte, s string) attr { return attr{t, []byte(s)} }
func aInt(t byte, n uint32) attr {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return attr{t, b}
}
func aIP(t byte, ip string) attr { return attr{t, net.ParseIP(ip).To4()} }

// encodeAttrs refuses values over 253 bytes: the length is one byte, and a longer value would wrap it
// and turn the rest of the value into attributes of a packet signed with the fleet secret.
func encodeAttrs(as []attr) ([]byte, error) {
	var b []byte
	for _, a := range as {
		if len(a.v) > 253 {
			return nil, fmt.Errorf("RADIUS attribute %d too long (%d bytes)", a.t, len(a.v))
		}
		b = append(b, a.t, byte(len(a.v)+2))
		b = append(b, a.v...)
	}
	return b, nil
}

func papEncrypt(pw []byte, secret string, auth []byte) []byte {
	n := (len(pw) + 15) / 16 * 16
	if n == 0 {
		n = 16
	}
	p := make([]byte, n)
	copy(p, pw)
	out := make([]byte, n)
	last := auth
	for i := 0; i < n; i += 16 {
		h := md5.Sum(append([]byte(secret), last...))
		for j := 0; j < 16; j++ {
			out[i+j] = p[i+j] ^ h[j]
		}
		last = out[i : i+16]
	}
	return out
}

type reply struct {
	code  byte
	attrs []attr
}

func (r *reply) get(t byte) []byte {
	for _, a := range r.attrs {
		if a.t == t {
			return a.v
		}
	}
	return nil
}

func (r *reply) getInt(t byte) int {
	v := r.get(t)
	if len(v) != 4 {
		return 0
	}
	return int(binary.BigEndian.Uint32(v))
}

func (r *reply) vendor(vid uint32, vt byte) []byte {
	for _, a := range r.attrs {
		if a.t != attrVendor || len(a.v) < 6 || binary.BigEndian.Uint32(a.v[:4]) != vid {
			continue
		}
		sub := a.v[4:]
		for len(sub) >= 2 && int(sub[1]) <= len(sub) && sub[1] >= 2 {
			if sub[0] == vt {
				return sub[2:sub[1]]
			}
			sub = sub[sub[1]:]
		}
	}
	return nil
}

// exchange sends one packet (code 1 = Access-Request, 4 = Accounting-Request) with retries.
func (g *Gateway) exchange(code byte, port int, as []attr) (*reply, error) {
	secret := []byte(g.cfg.RadiusSecret)
	id := make([]byte, 1)
	rand.Read(id)
	auth := make([]byte, 16)
	var pkt []byte
	build := func(body []byte) []byte {
		p := make([]byte, 20+len(body))
		p[0], p[1] = code, id[0]
		binary.BigEndian.PutUint16(p[2:], uint16(len(p)))
		copy(p[20:], body)
		return p
	}
	if code == 1 {
		rand.Read(auth)
		for i, a := range as {
			if a.t == attrUserPassword {
				as[i].v = papEncrypt(a.v, g.cfg.RadiusSecret, auth)
			}
		}
		as = append(as, attr{attrMsgAuth, make([]byte, 16)})
		body, err := encodeAttrs(as)
		if err != nil {
			return nil, err
		}
		pkt = build(body)
		copy(pkt[4:20], auth)
		mac := hmac.New(md5.New, secret)
		mac.Write(pkt)
		copy(pkt[len(pkt)-16:], mac.Sum(nil))
	} else {
		body, err := encodeAttrs(as)
		if err != nil {
			return nil, err
		}
		pkt = build(body)
		h := md5.New()
		h.Write(pkt[:4])
		h.Write(make([]byte, 16))
		h.Write(pkt[20:])
		h.Write(secret)
		copy(pkt[4:20], h.Sum(nil))
		copy(auth, pkt[4:20])
	}
	addr := net.JoinHostPort(g.cfg.RadiusServer, strconv.Itoa(port))
	var lastErr error
	for try := 0; try < 3; try++ {
		conn, err := net.Dial("udp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		if _, err := conn.Write(pkt); err != nil {
			conn.Close()
			lastErr = err
			continue
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		resp := buf[:n]
		if n < 20 || resp[1] != id[0] || int(binary.BigEndian.Uint16(resp[2:])) > n {
			lastErr = errors.New("malformed RADIUS reply")
			continue
		}
		resp = resp[:binary.BigEndian.Uint16(resp[2:])]
		h := md5.New()
		h.Write(resp[:4])
		h.Write(auth)
		h.Write(resp[20:])
		h.Write(secret)
		if !hmac.Equal(h.Sum(nil), resp[4:20]) {
			return nil, errors.New("RADIUS reply authenticator mismatch (wrong secret?)")
		}
		r := &reply{code: resp[0]}
		b := resp[20:]
		maOff := -1
		for len(b) >= 2 && int(b[1]) <= len(b) && b[1] >= 2 {
			if b[0] == attrMsgAuth && b[1] == 18 {
				maOff = len(resp) - len(b) + 2
			}
			r.attrs = append(r.attrs, attr{b[0], append([]byte{}, b[2:b[1]]...)})
			b = b[b[1]:]
		}
		if err := checkReplyMsgAuth(resp, auth, secret, maOff); err != nil {
			if code == 1 && g.cfg.RequireReplyMA {
				return nil, err
			}
			if maOff >= 0 { // present but wrong: always fatal
				return nil, err
			}
		}
		return r, nil
	}
	return nil, fmt.Errorf("no RADIUS answer from %s: %v", addr, lastErr)
}

// checkReplyMsgAuth verifies the Message-Authenticator of a reply (RFC 3579 3.2: HMAC-MD5 over the reply
// with the request authenticator in place of the response authenticator and the attribute zeroed).
// maOff is the offset of its 16-byte value in resp, -1 when the reply has none.
func checkReplyMsgAuth(resp, reqAuth, secret []byte, maOff int) error {
	if maOff < 0 {
		return errors.New("RADIUS reply without Message-Authenticator")
	}
	p := append([]byte{}, resp...)
	copy(p[4:20], reqAuth)
	got := append([]byte{}, p[maOff:maOff+16]...)
	copy(p[maOff:maOff+16], make([]byte, 16))
	m := hmac.New(md5.New, secret)
	m.Write(p)
	if !hmac.Equal(m.Sum(nil), got) {
		return errors.New("RADIUS reply Message-Authenticator mismatch")
	}
	return nil
}

func (g *Gateway) exchangeTracked(code byte, port int, as []attr) (*reply, error) {
	r, err := g.exchange(code, port, as)
	g.mu.Lock()
	g.radiusOK = err == nil
	g.radiusErr = ""
	if err != nil {
		g.radiusErr = err.Error()
	}
	g.mu.Unlock()
	return r, err
}

func macRadius(mac string) string { return strings.ToUpper(mac) }

func (g *Gateway) baseAttrs(mac, ip string) []attr {
	return []attr{
		aStr(attrNASIdentifier, g.cfg.NASIdentifier),
		aIP(attrNASIPAddress, g.cfg.RouterIP),
		aInt(attrNASPortType, 19), // Wireless-802.11
		aStr(attrCallingStation, macRadius(mac)),
		aStr(attrCalledStation, g.cfg.HotspotName),
		aIP(attrFramedIP, ip),
	}
}

// parseRate parses Mikrotik-Rate-Limit "rx[/tx] ..." (rx = client upload, tx = client download) into kbit/s.
func parseRate(s string) (up, down int) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, 0
	}
	p := strings.SplitN(f[0], "/", 2)
	conv := func(x string) int {
		x = strings.TrimSpace(strings.ToLower(x))
		mul := 1.0 / 1000
		switch {
		case strings.HasSuffix(x, "k"):
			mul, x = 1, strings.TrimSuffix(x, "k")
		case strings.HasSuffix(x, "m"):
			mul, x = 1000, strings.TrimSuffix(x, "m")
		case strings.HasSuffix(x, "g"):
			mul, x = 1000000, strings.TrimSuffix(x, "g")
		}
		v, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0
		}
		return int(v * mul)
	}
	up = conv(p[0])
	down = up
	if len(p) == 2 {
		down = conv(p[1])
	}
	return
}

func limit64(r *reply, lo, giga byte) uint64 {
	v := r.vendor(vendorMikrotik, lo)
	if len(v) != 4 {
		return 0
	}
	n := uint64(binary.BigEndian.Uint32(v))
	if g := r.vendor(vendorMikrotik, giga); len(g) == 4 {
		n += uint64(binary.BigEndian.Uint32(g)) << 32
	}
	return n
}

func sessionEnd(s *Session) time.Time {
	if !s.End.IsZero() {
		return s.End
	}
	return time.Now()
}

func (g *Gateway) accounting(s *Session, status uint32, cause uint32) error {
	as := g.baseAttrs(s.MAC, s.IP)
	as = append(as,
		aInt(attrAcctStatus, status),
		aStr(attrAcctSessionID, s.AcctID),
		aStr(attrUserName, s.Username),
		aInt(attrAcctSessTime, uint32(sessionEnd(s).Sub(s.Start).Seconds())),
		aInt(attrAcctInOctets, uint32(s.InOctets)), aInt(attrAcctInGiga, uint32(s.InOctets>>32)),
		aInt(attrAcctOutOctets, uint32(s.OutOctets)), aInt(attrAcctOutGiga, uint32(s.OutOctets>>32)),
		aInt(attrEventTimestamp, uint32(sessionEnd(s).Unix())),
	)
	if s.Class != "" {
		as = append(as, attr{attrClass, []byte(s.Class)})
	}
	if status == 2 {
		as = append(as, aInt(attrAcctTermCause, cause))
	}
	r, err := g.exchangeTracked(4, g.cfg.RadiusAcctPort, as)
	if err != nil {
		return err
	}
	if r.code != 5 {
		return fmt.Errorf("unexpected accounting reply code %d", r.code)
	}
	return nil
}

// ---------------------------------------------------------------- login / logout

func newSessionID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// freeSlot returns a free tc/mark slot (1..126; the mark field has 7 bits), or 0 when all are taken.
func (g *Gateway) freeSlot() int {
	for i := 1; i <= 126; i++ {
		if !g.slots[i] {
			g.slots[i] = true
			return i
		}
	}
	return 0
}

// reserveAttempt: at most 5 failed logins per device and 30 on the whole hotspot per minute (voucher
// guessing, also with random MACs). The attempt counts as failed from the start, under the lock, so
// parallel requests cannot all pass the check before the first one fails; releaseAttempt takes it back.
func (g *Gateway) reserveAttempt(mac string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now, total := time.Now(), 0
	for m, ts := range g.fails {
		keep := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			delete(g.fails, m)
		} else {
			g.fails[m] = keep
			total += len(keep)
		}
	}
	if len(g.fails[mac]) >= 5 || total >= 30 {
		return false
	}
	g.fails[mac] = append(g.fails[mac], now)
	return true
}

// releaseAttempt: the attempt was not a wrong guess (accepted, or RADIUS did not answer).
func (g *Gateway) releaseAttempt(mac string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ts := g.fails[mac]; len(ts) > 1 {
		g.fails[mac] = ts[:len(ts)-1]
	} else {
		delete(g.fails, mac)
	}
}

// login returns "" on success, otherwise the message to show (Reply-Message or a generic one).
func (g *Gateway) login(mac, ip, user, pass string) string {
	if len(user) > 253 || len(pass) > 128 {
		return "invalid username or password"
	}
	if !g.reserveAttempt(mac) {
		return "too many attempts, wait a minute"
	}
	g.mu.Lock()
	g.nasPort++
	port := g.nasPort
	g.mu.Unlock()
	as := append(g.baseAttrs(mac, ip),
		aStr(attrUserName, user),
		attr{attrUserPassword, []byte(pass)},
		aInt(attrServiceType, 1),
		aInt(attrNASPort, port),
	)
	r, err := g.exchangeTracked(1, g.cfg.RadiusAuthPort, as)
	if err != nil {
		log.Printf("login %s %s: %v", mac, user, err)
		g.releaseAttempt(mac)
		return "RADIUS server is not responding"
	}
	if r.code != 2 {
		msg := string(r.get(attrReplyMessage))
		if msg == "" {
			msg = "invalid username or password"
		}
		log.Printf("login %s %s: rejected: %s", mac, user, msg)
		return msg
	}
	g.releaseAttempt(mac)
	s := &Session{MAC: mac, IP: ip, Username: user, Class: string(r.get(attrClass)), AcctID: newSessionID(),
		Start: time.Now(), SessionTimeout: r.getInt(attrSessionTimeout), IdleTimeout: r.getInt(attrIdleTimeout),
		Interim: r.getInt(attrInterim), LastActivity: time.Now()}
	if s.Interim <= 0 {
		s.Interim = g.cfg.DefaultInterim
	}
	if s.Interim < 60 {
		s.Interim = 60
	}
	s.UpKbit, s.DownKbit = parseRate(string(r.vendor(vendorMikrotik, mtRateLimit)))
	s.TotalLimit = limit64(r, mtTotalLimit, mtTotalLimitGiga)
	s.XmitLimit = limit64(r, mtXmitLimit, mtXmitLimitGiga)

	g.mu.Lock()
	// re-login of the same device: replace the old session under the same lock (two concurrent logins
	// each replace the one before, so every session that got a Start also gets a Stop)
	old := g.sessions[mac]
	if old != nil {
		g.dropLocked(old)
	}
	if s.Slot = g.freeSlot(); s.Slot == 0 {
		g.mu.Unlock()
		if old != nil {
			g.sendStop(old, 1)
		}
		log.Printf("login %s %s: no free slot (126 sessions)", mac, user)
		return "the hotspot is full, try again later"
	}
	g.sessions[mac] = s
	g.applySession(s)
	s.LastInterim = time.Now()
	g.saveState()
	g.mu.Unlock()
	if old != nil {
		g.sendStop(old, 1)
	}
	log.Printf("login %s %s ip=%s ok: timeout=%ds idle=%ds rate=%d/%dkbit total=%d interim=%ds",
		mac, user, ip, s.SessionTimeout, s.IdleTimeout, s.UpKbit, s.DownKbit, s.TotalLimit, s.Interim)
	if err := g.accounting(s, 1, 0); err != nil {
		log.Printf("acct start %s: %v", mac, err)
	}
	return ""
}

// endSession revokes access and sends Accounting-Stop. cause: 1 User-Request, 4 Idle-Timeout,
// 5 Session-Timeout, 10 NAS-Request.
func (g *Gateway) endSession(s *Session, cause uint32) {
	g.mu.Lock()
	if g.sessions[s.MAC] != s { // already ended (logout and a timeout at the same time): one Stop only
		g.mu.Unlock()
		return
	}
	g.dropLocked(s)
	g.mu.Unlock()
	g.sendStop(s, cause)
}

// dropLocked removes a session and its firewall/tc state. Caller holds g.mu.
func (g *Gateway) dropLocked(s *Session) {
	s.End = time.Now()
	delete(g.sessions, s.MAC)
	delete(g.slots, s.Slot)
	g.revokeSession(s)
	g.saveState()
}

// sendStop sends Accounting-Stop; if RADIUS does not answer, it is queued and retried every tick.
func (g *Gateway) sendStop(s *Session, cause uint32) {
	log.Printf("logout %s %s cause=%d in=%d out=%d", s.MAC, s.Username, cause, s.InOctets, s.OutOctets)
	if err := g.accounting(s, 2, cause); err != nil {
		log.Printf("acct stop %s: %v (will retry)", s.MAC, err)
		g.mu.Lock()
		if len(g.stops) < 100 {
			g.stops = append(g.stops, pendingStop{s, cause})
		}
		g.mu.Unlock()
	}
}

// retryStops resends queued Accounting-Stops (one try each per tick).
func (g *Gateway) retryStops() {
	g.mu.Lock()
	q := g.stops
	g.stops = nil
	g.mu.Unlock()
	var left []pendingStop
	for i, p := range q {
		if err := g.accounting(p.s, 2, p.cause); err != nil {
			left = append(left, q[i:]...) // RADIUS still down: keep the rest for the next tick
			break
		}
		log.Printf("acct stop %s: sent on retry", p.s.MAC)
	}
	g.mu.Lock()
	g.stops = append(left, g.stops...)
	g.mu.Unlock()
}

// tick: counters, limits, interim updates, firewall self-heal.
func (g *Gateway) tick() {
	g.ensureFirewall()
	up, down, counted := readCounters()
	var ended []struct {
		s     *Session
		cause uint32
	}
	var interim []*Session
	g.mu.Lock()
	for _, s := range g.sessions {
		if counted {
			dIn, dOut := counterDelta(up[s.IP], s.rawIn), counterDelta(down[s.IP], s.rawOut)
			s.rawIn, s.rawOut = up[s.IP], down[s.IP]
			s.InOctets += dIn
			s.OutOctets += dOut
			if dIn+dOut > 0 {
				s.LastActivity = time.Now()
			}
		}
		switch {
		case s.SessionTimeout > 0 && time.Since(s.Start) >= time.Duration(s.SessionTimeout)*time.Second:
			ended = append(ended, struct {
				s     *Session
				cause uint32
			}{s, 5})
		case s.IdleTimeout > 0 && time.Since(s.LastActivity) >= time.Duration(s.IdleTimeout)*time.Second:
			ended = append(ended, struct {
				s     *Session
				cause uint32
			}{s, 4})
		case (s.TotalLimit > 0 && s.InOctets+s.OutOctets >= s.TotalLimit) || (s.XmitLimit > 0 && s.OutOctets >= s.XmitLimit):
			ended = append(ended, struct {
				s     *Session
				cause uint32
			}{s, 10})
		case time.Since(s.LastInterim) >= time.Duration(s.Interim)*time.Second:
			s.LastInterim = time.Now()
			interim = append(interim, s)
		}
	}
	// access ends now (local); the Accounting-Stop goes with the RADIUS round below
	var stops []pendingStop
	for _, e := range ended {
		if g.sessions[e.s.MAC] == e.s {
			g.dropLocked(e.s)
			stops = append(stops, pendingStop{e.s, e.cause})
		}
	}
	g.saveState()
	g.mu.Unlock()
	// RADIUS can take 12 s per packet when it is down: the round runs apart from the ticker, so firewall
	// self-heal, limits and timeouts keep their 10 s pace. One round at a time; a busy round queues the Stops
	// (retried later) and skips these Interims (the next ones carry the totals).
	if !g.acctBusy.CompareAndSwap(false, true) {
		g.mu.Lock()
		for _, p := range stops {
			if len(g.stops) < 100 {
				g.stops = append(g.stops, p)
			}
		}
		g.mu.Unlock()
		return
	}
	go func() {
		defer g.acctBusy.Store(false)
		for _, p := range stops {
			g.sendStop(p.s, p.cause)
		}
		for _, s := range interim {
			if err := g.accounting(s, 3, 0); err != nil {
				log.Printf("acct interim %s: %v", s.MAC, err)
				break // RADIUS down: the next Interims would wait 12 s each for nothing
			}
		}
		g.retryStops()
	}()
}

// counterDelta: bytes since the last reading. iptables counters only go down when the chain was flushed
// (daemon restart, fw3 reload, rules re-added): then everything counted so far is new.
func counterDelta(raw, last uint64) uint64 {
	if raw >= last {
		return raw - last
	}
	return raw
}

// ---------------------------------------------------------------- state (survives a daemon restart, not a reboot)

func (g *Gateway) saveState() {
	b, _ := json.Marshal(g.sessions)
	tmp := g.cfg.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0600) == nil {
		os.Rename(tmp, g.cfg.StateFile)
	}
}

func (g *Gateway) loadState() {
	b, err := os.ReadFile(g.cfg.StateFile)
	if err != nil {
		return
	}
	var m map[string]*Session
	if json.Unmarshal(b, &m) == nil {
		for k, s := range m {
			g.sessions[k] = s
			g.slots[s.Slot] = true
		}
	}
}

// rebind moves a live session to the client's new IP (DHCP gave it another address): access, counters
// and tc follow it. Without this the client would stay captured until the session ends.
func (g *Gateway) rebind(s *Session, ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions[s.MAC] != s || s.IP == ip {
		return
	}
	log.Printf("rebind %s %s -> %s", s.MAC, s.IP, ip)
	g.revokeSession(s)
	s.IP = ip
	g.applySession(s)
	g.saveState()
}

// ---------------------------------------------------------------- HTTP

func arpMAC(ip string) string {
	b, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[0] == ip && f[3] != "00:00:00:00:00:00" {
			return strings.ToLower(f[3])
		}
	}
	return ""
}

// trampoline renders the same page omnicap generates for RouterOS (internal/rosscript/trampoline.go),
// with the values RouterOS would substitute.
func (g *Gateway) trampoline(w *response, success bool, mac, ip, user, dst, errMsg string) {
	suffix, res := "/login", "notyet"
	if success {
		suffix, res = "/conectado", "success"
	}
	link := g.loginURL()
	fields := [][2]string{
		{"res", res}, {"mac", strings.ToUpper(mac)}, {"ip", ip}, {"username", user},
		{"link_login_only", link}, {"link_orig", dst}, {"nasid", g.cfg.NASIdentifier},
		{"uamip", g.cfg.RouterIP}, {"error", errMsg}, {"chap_id", ""}, {"chap_challenge", ""}, {"ssl_login", "no"},
	}
	if success {
		fields = append(fields, [2]string{"link_redirect", dst})
	}
	w.headers = append(w.headers, [2]string{"Content-Type", "text/html; charset=utf-8"}, [2]string{"Cache-Control", "no-cache, no-store"})
	var b strings.Builder
	b.WriteString("<html><head><title>H3S WiFi</title>\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n</head><body>\n")
	fmt.Fprintf(&b, "<form name=\"redirect\" action=\"%s\" method=\"GET\">\n", html.EscapeString(g.cfg.PortalURL+suffix))
	for _, f := range fields {
		fmt.Fprintf(&b, "<input type=\"hidden\" name=\"%s\" value=\"%s\" />\n", f[0], html.EscapeString(f[1]))
	}
	b.WriteString("</form>\n<script>document.redirect.submit();</script>\n</body></html>\n")
	w.body.WriteString(b.String())
}

// Minimal HTTP/1.1 server (no net/http: it pulls TLS and doubles the binary, and flash is scarce).

type request struct {
	Method, Host, Path, RawQuery string
	Form                         url.Values
	RemoteIP                     string
}

type response struct {
	status  int
	headers [][2]string
	body    strings.Builder
}

func (w *response) redirect(loc string) {
	w.status = 302
	w.headers = append(w.headers, [2]string{"Location", loc})
}

// readRequest reads at most 16 KB per request (request line + headers + body): the clients are untrusted
// and RAM is scarce (~30 MB free).
func readRequest(c net.Conn) (*request, error) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(io.LimitReader(c, 16<<10), 4096)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return nil, errors.New("bad request line")
	}
	r := &request{Method: f[0]}
	r.Path, r.RawQuery, _ = strings.Cut(f[1], "?")
	clen := 0
	for i := 0; i < 64; i++ {
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
		case "host":
			r.Host = v
		case "content-length":
			clen, _ = strconv.Atoi(v)
		}
	}
	r.Form, _ = url.ParseQuery(r.RawQuery)
	if r.Method == "POST" && clen > 0 && clen <= 4096 {
		body := make([]byte, clen)
		if _, err := io.ReadFull(br, body); err == nil {
			if pf, err := url.ParseQuery(string(body)); err == nil {
				for k, vs := range pf {
					r.Form[k] = vs
				}
			}
		}
	}
	r.RemoteIP, _, _ = net.SplitHostPort(c.RemoteAddr().String())
	return r, nil
}

// accept serves one client connection, or drops it when 64 are already open (8002 and HTTPS together)
// or 8 from the same IP.
func (g *Gateway) accept(c net.Conn) {
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	g.connMu.Lock()
	if g.connIP[ip] >= 8 { // one client cannot hold the 64 slots (10 s each) and stall every login
		g.connMu.Unlock()
		c.Close()
		return
	}
	g.connIP[ip]++
	g.connMu.Unlock()
	release := func() {
		g.connMu.Lock()
		if g.connIP[ip]--; g.connIP[ip] <= 0 {
			delete(g.connIP, ip)
		}
		g.connMu.Unlock()
	}
	select {
	case g.connSem <- struct{}{}:
		go func() {
			defer func() { <-g.connSem; release() }()
			g.serveConn(c)
		}()
	default:
		release()
		c.Close()
	}
}

func (g *Gateway) serveConn(c net.Conn) {
	defer c.Close()
	r, err := readRequest(c)
	if err != nil {
		return
	}
	w := &response{status: 200}
	g.handle(w, r)
	text := map[int]string{200: "OK", 302: "Found", 400: "Bad Request"}[w.status]
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: %d\r\n", w.status, text, w.body.Len())
	for _, h := range w.headers {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	b.WriteString("\r\n")
	b.WriteString(w.body.String())
	c.Write([]byte(b.String()))
}

func (g *Gateway) handle(w *response, r *request) {
	ip := r.RemoteIP
	mac := arpMAC(ip)
	self := fmt.Sprintf("http://%s:%d", g.cfg.RouterIP, g.cfg.ListenPort)
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != g.cfg.RouterIP && !(g.cfg.LoginHost != "" && strings.EqualFold(host, g.cfg.LoginHost)) {
		// captured request: send the client to the login page with the URL it wanted
		orig := "http://" + r.Host + r.Path
		if r.RawQuery != "" {
			orig += "?" + r.RawQuery
		}
		w.redirect(self + "/login?dst=" + url.QueryEscape(orig))
		return
	}
	switch r.Path {
	case "/login":
		g.handleLogin(w, r, ip, mac)
	case "/logout":
		if r.Method != "POST" { // a GET would let any web page (<img src=.../logout>) end the session
			w.redirect(self + "/login")
			return
		}
		g.mu.Lock()
		s := g.sessions[mac]
		g.mu.Unlock()
		if s != nil {
			g.endSession(s, 1)
		}
		g.trampoline(w, false, mac, ip, "", "", "")
	case "/status":
		g.mu.Lock()
		s := g.sessions[mac]
		var out []byte
		if s != nil {
			out, _ = json.Marshal(s)
		} else {
			out = []byte(`{"authenticated":false}`)
		}
		g.mu.Unlock()
		w.headers = append(w.headers, [2]string{"Content-Type", "application/json"})
		w.body.Write(out)
	default:
		w.redirect(self + "/login")
	}
}

func (g *Gateway) handleLogin(w *response, r *request, ip, mac string) {
	dst := r.Form.Get("dst")
	if mac == "" {
		w.status = 400
		w.body.WriteString("client not found in ARP table")
		return
	}
	user, pass := r.Form.Get("username"), r.Form.Get("password")
	if user == "" {
		g.mu.Lock()
		s := g.sessions[mac]
		g.mu.Unlock()
		if s != nil { // already logged in (maybe from an IP it no longer has)
			g.rebind(s, ip)
			g.trampoline(w, true, mac, ip, s.Username, dst, "")
			return
		}
		g.trampoline(w, false, mac, ip, "", dst, "")
		return
	}
	if msg := g.login(mac, ip, user, pass); msg != "" {
		g.trampoline(w, false, mac, ip, user, dst, msg)
		return
	}
	g.trampoline(w, true, mac, ip, user, dst, "")
}

// ---------------------------------------------------------------- main

func main() {
	uciPkg := flag.String("uci", "omnicapgw", "uci package written by the web UI")
	cleanup := flag.Bool("cleanup", false, "remove all firewall/tc state and exit")
	installURL := flag.String("install", "", "register in omnicap with this install URL (.../agent/v1/install/<key>), write uci and exit")
	identity := flag.String("identity", "", "device identity (NAS-Identifier) for -install")
	flag.Parse()
	// started from a CGI or cron: make sure uci, iptables, ipset and tc are found
	os.Setenv("PATH", os.Getenv("PATH")+":/usr/sbin:/usr/bin:/sbin:/bin")
	if *installURL != "" {
		os.Exit(runInstall(*installURL, *identity, *uciPkg))
	}
	log.SetFlags(log.LstdFlags)
	cfg, err := loadUCI(*uciPkg)
	radiusOK := err == nil
	if err != nil && *cleanup {
		cfg, err = defaultConfig(), nil // cleanup only needs the interface names and the state file
	}
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	g := &Gateway{cfg: cfg, sessions: map[string]*Session{}, slots: map[int]bool{}, wgByHost: map[string][]string{},
		fails: map[string][]time.Time{}, connSem: make(chan struct{}, 64), connIP: map[string]int{}, started: time.Now(), uciPkg: *uciPkg,
		pull: pullStatus{Interval: defaultPullInterval}}
	g.loadState()
	if *cleanup {
		g.syncLoginHost(false)
		g.teardownFirewall()
		if radiusOK { // portal disabled: close the open sessions in omnicap too (in parallel: RADIUS may be down)
			var wg sync.WaitGroup
			for _, s := range g.sessions {
				s.End = time.Now()
				wg.Add(1)
				go func(s *Session) {
					defer wg.Done()
					if err := g.accounting(s, 2, 10); err != nil {
						log.Printf("acct stop %s: %v", s.MAC, err)
					}
				}(s)
			}
			wg.Wait()
		}
		os.Remove(cfg.StateFile)
		return
	}
	if err := g.setupFirewall(); err != nil {
		log.Fatalf("firewall: %v", err)
	}
	shq("ipset", "flush", setWG) // start from the current walled garden, not what an older run left
	g.refreshWalledGarden()
	g.loadCertFiles("file")
	if cfg.HTTPS {
		go g.fetchCert() // may take ~35 s with the WAN down: the HTTP portal starts meanwhile
		go g.serveTLS()
	}
	g.syncLoginHost(cfg.HTTPS)
	go g.serveAdmin()
	go g.pullLoop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		log.Printf("stopping: sessions stay authorized until the daemon is back (state %s)", cfg.StateFile)
		os.Exit(0)
	}()
	go func() {
		t := time.NewTicker(10 * time.Second)
		n := 0
		for range t.C {
			g.tick()
			if n++; n%30 == 0 {
				g.refreshWalledGarden()
				g.syncLoginHost(cfg.HTTPS)
			}
			if cfg.HTTPS && n%2160 == 0 { // every 6 h, like the RouterOS agent pull picks up renewals
				g.fetchCert()
			}
		}
	}()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ListenPort))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("omnicap-gw up: nas=%s portal=%s radius=%s login=%s sessions=%d",
		cfg.NASIdentifier, cfg.PortalURL, cfg.RadiusServer, g.loginURL(), len(g.sessions))
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		g.accept(c)
	}
}
