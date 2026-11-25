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
	"syscall"
	"time"
)

// ---------------------------------------------------------------- config

type Config struct {
	LanIf          string   `json:"lan_if"`           // br-lan
	WanIf          string   `json:"wan_if"`           // ccinet0 (upload shaping)
	RouterIP       string   `json:"router_ip"`        // 192.168.1.1
	ListenPort     int      `json:"listen_port"`      // 8002
	PortalURL      string   `json:"portal_url"`       // https://cautivo.h3s-iot.com/portal/<ref> (no trailing /login)
	NASIdentifier  string   `json:"nas_identifier"`   // device.nas_identifier in omnicap
	RadiusServer   string   `json:"radius_server"`    // host (auth 1812, acct 1813)
	RadiusAuthPort int      `json:"radius_auth_port"` // default 1812
	RadiusAcctPort int      `json:"radius_acct_port"` // default 1813
	RadiusSecret   string   `json:"radius_secret"`    // fleet secret
	WalledGarden   []string `json:"walled_garden"`    // hostnames, IPs or CIDRs reachable without login
	BypassMACs     []string `json:"bypass_macs"`      // never captured (e.g. the management PC)
	HotspotName    string   `json:"hotspot_name"`     // Called-Station-Id, default "hotspot1"
	DefaultInterim int      `json:"default_interim_s"`
	StateFile      string   `json:"state_file"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{LanIf: "br-lan", WanIf: "ccinet0", RouterIP: "192.168.1.1", ListenPort: 8002,
		RadiusAuthPort: 1812, RadiusAcctPort: 1813, HotspotName: "hotspot1", DefaultInterim: 300,
		StateFile: "/tmp/omnicap-gw.state"}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.PortalURL == "" || c.NASIdentifier == "" || c.RadiusServer == "" || c.RadiusSecret == "" {
		return nil, errors.New("portal_url, nas_identifier, radius_server and radius_secret are required")
	}
	c.PortalURL = strings.TrimSuffix(strings.TrimSuffix(c.PortalURL, "/"), "/login")
	return c, nil
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
	Slot                             int // tc class minor / fw mark slot
}

type Gateway struct {
	cfg      *Config
	mu       sync.Mutex
	sessions map[string]*Session // by MAC
	slots    map[int]bool
	wgIPs    map[string]bool
	nasPort  uint32
}

const (
	chainFwd  = "OCGW_FWD"
	chainAcct = "OCGW_ACCT"
	chainNat  = "OCGW_NAT"
	chainMark = "OCGW_MARK"
	setAuth   = "ocgw_auth"
	setWG     = "ocgw_wg"
	setBypass = "ocgw_bypass"
	markMask  = 0x7f000000
)

// ---------------------------------------------------------------- firewall

func (g *Gateway) firewallPresent() bool {
	out, _ := sh("iptables", "-S", "FORWARD")
	nat, _ := sh("iptables", "-t", "nat", "-S", "PREROUTING")
	mg, _ := sh("iptables", "-t", "mangle", "-S", "FORWARD")
	return strings.Contains(out, "-j "+chainFwd) && strings.Contains(out, "-j "+chainAcct) &&
		strings.Contains(nat, "-j "+chainNat) && strings.Contains(mg, "-j "+chainMark)
}

// setupFirewall (re)creates every chain, set and jump. Idempotent.
func (g *Gateway) setupFirewall() error {
	c := g.cfg
	shq("ipset", "create", setAuth, "hash:mac", "-exist")
	shq("ipset", "create", setBypass, "hash:mac", "-exist")
	shq("ipset", "create", setWG, "hash:net", "-exist")
	for _, m := range c.BypassMACs {
		shq("ipset", "add", setBypass, strings.ToLower(m), "-exist")
	}
	for _, t := range []struct{ table, chain string }{{"filter", chainFwd}, {"filter", chainAcct}, {"nat", chainNat}, {"mangle", chainMark}} {
		shq("iptables", "-t", t.table, "-N", t.chain)
		shq("iptables", "-t", t.table, "-F", t.chain)
	}
	// filter: captive enforcement for traffic coming from the LAN
	rules := [][]string{
		{"-A", chainFwd, "-o", c.LanIf, "-j", "RETURN"},
		{"-A", chainFwd, "-m", "set", "--match-set", setBypass, "src", "-j", "RETURN"},
		{"-A", chainFwd, "-m", "set", "--match-set", setAuth, "src", "-j", "RETURN"},
		{"-A", chainFwd, "-m", "set", "--match-set", setWG, "dst", "-j", "RETURN"},
		{"-A", chainFwd, "-p", "udp", "--dport", "53", "-j", "RETURN"},
		{"-A", chainFwd, "-p", "tcp", "--dport", "53", "-j", "RETURN"},
		{"-A", chainFwd, "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset"},
		{"-A", chainFwd, "-j", "REJECT", "--reject-with", "icmp-port-unreachable"},
	}
	for _, r := range rules {
		if _, err := sh("iptables", r...); err != nil {
			return err
		}
	}
	// nat: HTTP of unauthenticated clients -> this daemon; DNS -> router
	nrules := [][]string{
		{"-A", chainNat, "-m", "set", "--match-set", setBypass, "src", "-j", "RETURN"},
		{"-A", chainNat, "-m", "set", "--match-set", setAuth, "src", "-j", "RETURN"},
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
	// jumps (first position, ahead of the vendor and fw3 chains)
	jumps := [][]string{
		{"filter", "FORWARD", "-i", c.LanIf, "-j", chainFwd},
		{"filter", "FORWARD", "-j", chainAcct},
		{"nat", "PREROUTING", "-i", c.LanIf, "-j", chainNat},
		{"mangle", "FORWARD", "-i", c.LanIf, "-j", chainMark},
	}
	for _, j := range jumps {
		args := append([]string{"-t", j[0], "-C", j[1]}, j[2:]...)
		if _, err := sh("iptables", args...); err != nil {
			args[2] = "-I"
			args = append(args[:4], append([]string{"1"}, args[4:]...)...)
			if _, err := sh("iptables", args...); err != nil {
				return err
			}
		}
	}
	// re-add per-session state
	for _, s := range g.sessions {
		g.applySession(s)
	}
	return nil
}

func (g *Gateway) teardownFirewall() {
	c := g.cfg
	for {
		if _, err := sh("iptables", "-D", "FORWARD", "-i", c.LanIf, "-j", chainFwd); err != nil {
			break
		}
	}
	shq("iptables", "-D", "FORWARD", "-j", chainAcct)
	shq("iptables", "-t", "nat", "-D", "PREROUTING", "-i", c.LanIf, "-j", chainNat)
	shq("iptables", "-t", "mangle", "-D", "FORWARD", "-i", c.LanIf, "-j", chainMark)
	for _, t := range []struct{ table, chain string }{{"filter", chainFwd}, {"filter", chainAcct}, {"nat", chainNat}, {"mangle", chainMark}} {
		shq("iptables", "-t", t.table, "-F", t.chain)
		shq("iptables", "-t", t.table, "-X", t.chain)
	}
	for _, s := range []string{setAuth, setWG, setBypass} {
		shq("ipset", "destroy", s)
	}
	for _, s := range g.sessions {
		g.removeShaping(s)
	}
}

// refreshWalledGarden resolves the configured hosts (dnsmasq here has no ipset support).
func (g *Gateway) refreshWalledGarden() {
	hosts := append([]string{}, g.cfg.WalledGarden...)
	if u, err := url.Parse(g.cfg.PortalURL); err == nil {
		hosts = append(hosts, u.Hostname())
	}
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
		for _, a := range addrs {
			if !g.wgIPs[a] {
				shq("ipset", "add", setWG, a, "-exist")
				g.wgIPs[a] = true
			}
		}
	}
}

func (g *Gateway) applySession(s *Session) {
	shq("ipset", "add", setAuth, s.MAC, "-exist")
	shq("iptables", "-A", chainAcct, "-s", s.IP, "-j", "RETURN")
	shq("iptables", "-A", chainAcct, "-d", s.IP, "-j", "RETURN")
	g.applyShaping(s)
}

func (g *Gateway) revokeSession(s *Session) {
	shq("ipset", "del", setAuth, s.MAC)
	shq("iptables", "-D", chainAcct, "-s", s.IP, "-j", "RETURN")
	shq("iptables", "-D", chainAcct, "-d", s.IP, "-j", "RETURN")
	g.removeShaping(s)
	// drop established flows of this client so access ends now, not when they time out
	shq("conntrack", "-D", "-s", s.IP)
}

// tc: download = class under the LAN egress HTB, upload = fwmark + class under the WAN egress HTB.
// Both roots exist (vendor qos daemon); if not, a root is created.
func htbRoot(dev string) string {
	out, _ := sh("tc", "qdisc", "show", "dev", dev)
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "qdisc" && f[1] == "htb" && strings.Contains(l, " root") {
			return strings.TrimSuffix(f[2], ":")
		}
	}
	shq("tc", "qdisc", "add", "dev", dev, "root", "handle", "77:", "htb", "default", "0")
	return "77"
}

func (g *Gateway) applyShaping(s *Session) {
	if s.DownKbit > 0 {
		r := htbRoot(g.cfg.LanIf)
		cls := fmt.Sprintf("%s:%x", r, 0x100+s.Slot)
		shq("tc", "class", "replace", "dev", g.cfg.LanIf, "parent", r+":", "classid", cls, "htb",
			"rate", fmt.Sprintf("%dkbit", s.DownKbit), "ceil", fmt.Sprintf("%dkbit", s.DownKbit))
		shq("tc", "filter", "add", "dev", g.cfg.LanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", fmt.Sprintf("800::%x", 0x100+s.Slot), "u32", "match", "ip", "dst", s.IP+"/32", "flowid", cls)
	}
	if s.UpKbit > 0 {
		mark := fmt.Sprintf("0x%x/0x%x", (0x01000000*(s.Slot+1))&markMask, markMask)
		shq("iptables", "-t", "mangle", "-A", chainMark, "-s", s.IP, "-j", "MARK", "--set-xmark", mark)
		r := htbRoot(g.cfg.WanIf)
		cls := fmt.Sprintf("%s:%x", r, 0x100+s.Slot)
		shq("tc", "class", "replace", "dev", g.cfg.WanIf, "parent", r+":", "classid", cls, "htb",
			"rate", fmt.Sprintf("%dkbit", s.UpKbit), "ceil", fmt.Sprintf("%dkbit", s.UpKbit))
		shq("tc", "filter", "add", "dev", g.cfg.WanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", mark, "fw", "flowid", cls)
	}
}

func (g *Gateway) removeShaping(s *Session) {
	if r := htbRoot(g.cfg.LanIf); r != "" {
		shq("tc", "filter", "del", "dev", g.cfg.LanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", fmt.Sprintf("800::%x", 0x100+s.Slot), "u32")
		shq("tc", "class", "del", "dev", g.cfg.LanIf, "classid", fmt.Sprintf("%s:%x", r, 0x100+s.Slot))
	}
	mark := fmt.Sprintf("0x%x/0x%x", (0x01000000*(s.Slot+1))&markMask, markMask)
	shq("iptables", "-t", "mangle", "-D", chainMark, "-s", s.IP, "-j", "MARK", "--set-xmark", mark)
	if r := htbRoot(g.cfg.WanIf); r != "" {
		shq("tc", "filter", "del", "dev", g.cfg.WanIf, "parent", r+":", "protocol", "ip", "prio", "3",
			"handle", mark, "fw")
		shq("tc", "class", "del", "dev", g.cfg.WanIf, "classid", fmt.Sprintf("%s:%x", r, 0x100+s.Slot))
	}
}

// readCounters returns bytes per IP from OCGW_ACCT: src = client upload, dst = client download.
func readCounters() (up, down map[string]uint64) {
	up, down = map[string]uint64{}, map[string]uint64{}
	out, err := sh("iptables", "-L", chainAcct, "-n", "-v", "-x")
	if err != nil {
		return
	}
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

func encodeAttrs(as []attr) []byte {
	var b []byte
	for _, a := range as {
		b = append(b, a.t, byte(len(a.v)+2))
		b = append(b, a.v...)
	}
	return b
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
		pkt = build(encodeAttrs(as))
		copy(pkt[4:20], auth)
		mac := hmac.New(md5.New, secret)
		mac.Write(pkt)
		copy(pkt[len(pkt)-16:], mac.Sum(nil))
	} else {
		pkt = build(encodeAttrs(as))
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
		for len(b) >= 2 && int(b[1]) <= len(b) && b[1] >= 2 {
			r.attrs = append(r.attrs, attr{b[0], append([]byte{}, b[2:b[1]]...)})
			b = b[b[1]:]
		}
		return r, nil
	}
	return nil, fmt.Errorf("no RADIUS answer from %s: %v", addr, lastErr)
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

func (g *Gateway) accounting(s *Session, status uint32, cause uint32) error {
	as := g.baseAttrs(s.MAC, s.IP)
	as = append(as,
		aInt(attrAcctStatus, status),
		aStr(attrAcctSessionID, s.AcctID),
		aStr(attrUserName, s.Username),
		aInt(attrAcctSessTime, uint32(time.Since(s.Start).Seconds())),
		aInt(attrAcctInOctets, uint32(s.InOctets)), aInt(attrAcctInGiga, uint32(s.InOctets>>32)),
		aInt(attrAcctOutOctets, uint32(s.OutOctets)), aInt(attrAcctOutGiga, uint32(s.OutOctets>>32)),
		aInt(attrEventTimestamp, uint32(time.Now().Unix())),
	)
	if s.Class != "" {
		as = append(as, attr{attrClass, []byte(s.Class)})
	}
	if status == 2 {
		as = append(as, aInt(attrAcctTermCause, cause))
	}
	r, err := g.exchange(4, g.cfg.RadiusAcctPort, as)
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

func (g *Gateway) freeSlot() int {
	for i := 1; i < 126; i++ {
		if !g.slots[i] {
			g.slots[i] = true
			return i
		}
	}
	return 126
}

// login returns "" on success, otherwise the message to show (Reply-Message or a generic one).
func (g *Gateway) login(mac, ip, user, pass string) string {
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
	r, err := g.exchange(1, g.cfg.RadiusAuthPort, as)
	if err != nil {
		log.Printf("login %s %s: %v", mac, user, err)
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
	if old, ok := g.sessions[mac]; ok { // re-login of the same device: close the old one first
		g.mu.Unlock()
		g.endSession(old, 1)
		g.mu.Lock()
	}
	s.Slot = g.freeSlot()
	g.sessions[mac] = s
	g.applySession(s)
	s.LastInterim = time.Now()
	g.saveState()
	g.mu.Unlock()
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
	if g.sessions[s.MAC] == s {
		delete(g.sessions, s.MAC)
		delete(g.slots, s.Slot)
		g.revokeSession(s)
		g.saveState()
	}
	g.mu.Unlock()
	if err := g.accounting(s, 2, cause); err != nil {
		log.Printf("acct stop %s: %v", s.MAC, err)
	}
	log.Printf("logout %s %s cause=%d in=%d out=%d", s.MAC, s.Username, cause, s.InOctets, s.OutOctets)
}

// tick: counters, limits, interim updates, firewall self-heal.
func (g *Gateway) tick() {
	if !g.firewallPresent() {
		log.Printf("firewall rules missing (fw3 reload?), re-applying")
		g.mu.Lock()
		if err := g.setupFirewall(); err != nil {
			log.Printf("setup firewall: %v", err)
		}
		g.wgIPs = map[string]bool{}
		g.mu.Unlock()
	}
	up, down := readCounters()
	var ended []struct {
		s     *Session
		cause uint32
	}
	var interim []*Session
	g.mu.Lock()
	for _, s := range g.sessions {
		in, out := up[s.IP], down[s.IP]
		if in > s.InOctets || out > s.OutOctets {
			s.LastActivity = time.Now()
		}
		if in >= s.InOctets {
			s.InOctets = in
		}
		if out >= s.OutOctets {
			s.OutOctets = out
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
	g.saveState()
	g.mu.Unlock()
	for _, e := range ended {
		g.endSession(e.s, e.cause)
	}
	for _, s := range interim {
		if err := g.accounting(s, 3, 0); err != nil {
			log.Printf("acct interim %s: %v", s.MAC, err)
		}
	}
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
	link := fmt.Sprintf("http://%s:%d/login", g.cfg.RouterIP, g.cfg.ListenPort)
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

func readRequest(c net.Conn) (*request, error) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(c, 4096)
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
	if r.Method == "POST" && clen > 0 && clen < 65536 {
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
	if !strings.HasPrefix(r.Host, g.cfg.RouterIP) {
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
		if s != nil { // already logged in
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
	cfgPath := flag.String("config", "/data/omnicap-gw/config.json", "config file")
	cleanup := flag.Bool("cleanup", false, "remove all firewall/tc state and exit")
	flag.Parse()
	log.SetFlags(log.LstdFlags)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	g := &Gateway{cfg: cfg, sessions: map[string]*Session{}, slots: map[int]bool{}, wgIPs: map[string]bool{}}
	g.loadState()
	if *cleanup {
		g.teardownFirewall()
		os.Remove(cfg.StateFile)
		return
	}
	if err := g.setupFirewall(); err != nil {
		log.Fatalf("firewall: %v", err)
	}
	g.refreshWalledGarden()

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
			}
		}
	}()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ListenPort))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("omnicap-gw up: nas=%s portal=%s radius=%s listen=:%d sessions=%d",
		cfg.NASIdentifier, cfg.PortalURL, cfg.RadiusServer, cfg.ListenPort, len(g.sessions))
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go g.serveConn(c)
	}
}
