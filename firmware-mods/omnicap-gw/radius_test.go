package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testSecret = "fleet-secret-123"

// fakeRadius answers one Access-Request on 127.0.0.1: it checks the request Message-Authenticator and the
// PAP password like FreeRADIUS does, then replies with reply() (signed with a Message-Authenticator unless
// sign is false). It returns the port and a channel with the decrypted password.
func fakeRadius(t *testing.T, sign bool, extra []attr) (int, chan string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		req := append([]byte{}, buf[:n]...)
		reqAuth := append([]byte{}, req[4:20]...)
		// request Message-Authenticator
		var pw []byte
		for b, off := req[20:], 20; len(b) >= 2; b, off = b[b[1]:], off+int(b[1]) {
			switch b[0] {
			case attrMsgAuth:
				p := append([]byte{}, req...)
				copy(p[off+2:off+18], make([]byte, 16))
				m := hmac.New(md5.New, []byte(testSecret))
				m.Write(p)
				if !hmac.Equal(m.Sum(nil), b[2:18]) {
					got <- "BAD REQUEST MESSAGE-AUTHENTICATOR"
					return
				}
			case attrUserPassword:
				enc := b[2:b[1]]
				last := reqAuth
				for i := 0; i < len(enc); i += 16 {
					h := md5.Sum(append([]byte(testSecret), last...))
					for j := 0; j < 16; j++ {
						pw = append(pw, enc[i+j]^h[j])
					}
					last = enc[i : i+16]
				}
			}
		}
		got <- string(bytes.TrimRight(pw, "\x00"))
		as := append([]attr{}, extra...)
		if sign {
			as = append([]attr{{attrMsgAuth, make([]byte, 16)}}, as...)
		}
		body, _ := encodeAttrs(as)
		resp := make([]byte, 20+len(body))
		resp[0], resp[1] = 2, req[1]
		binary.BigEndian.PutUint16(resp[2:], uint16(len(resp)))
		copy(resp[20:], body)
		copy(resp[4:20], reqAuth)
		if sign { // RFC 3579: HMAC over the reply with the request authenticator, attribute zeroed
			m := hmac.New(md5.New, []byte(testSecret))
			m.Write(resp)
			copy(resp[22:38], m.Sum(nil))
		}
		h := md5.New()
		h.Write(resp[:4])
		h.Write(reqAuth)
		h.Write(resp[20:])
		h.Write([]byte(testSecret))
		copy(resp[4:20], h.Sum(nil))
		pc.WriteTo(resp, addr)
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port, got
}

func vsa(vt byte, v []byte) attr {
	b := make([]byte, 6, 6+len(v))
	binary.BigEndian.PutUint32(b, vendorMikrotik)
	b[4], b[5] = vt, byte(len(v)+2)
	return attr{attrVendor, append(b, v...)}
}

func testGateway(port int) *Gateway {
	c := defaultConfig()
	c.RadiusServer, c.RadiusSecret, c.RadiusAuthPort, c.NASIdentifier = "127.0.0.1", testSecret, port, "NAS1"
	c.RequireReplyMA = true
	return &Gateway{cfg: c, sessions: map[string]*Session{}, slots: map[int]bool{}, fails: map[string][]time.Time{}}
}

func TestRadiusAcceptAndLimits(t *testing.T) {
	big := make([]byte, 4)
	binary.BigEndian.PutUint32(big, 100)
	giga := make([]byte, 4)
	binary.BigEndian.PutUint32(giga, 1)
	port, got := fakeRadius(t, true, []attr{
		aInt(attrSessionTimeout, 3600),
		vsa(mtRateLimit, []byte("512k/1M")),
		vsa(mtTotalLimit, big), vsa(mtTotalLimitGiga, giga),
	})
	g := testGateway(port)
	pass := "a-password-longer-than-16-bytes"
	r, err := g.exchange(1, port, append(g.baseAttrs("aa:bb:cc:dd:ee:ff", "192.168.1.50"),
		aStr(attrUserName, "voucher1"), attr{attrUserPassword, []byte(pass)}))
	if err != nil {
		t.Fatal(err)
	}
	if p := <-got; p != pass {
		t.Fatalf("server decrypted %q, want %q", p, pass)
	}
	if r.code != 2 || r.getInt(attrSessionTimeout) != 3600 {
		t.Fatalf("code %d timeout %d", r.code, r.getInt(attrSessionTimeout))
	}
	if up, down := parseRate(string(r.vendor(vendorMikrotik, mtRateLimit))); up != 512 || down != 1000 {
		t.Fatalf("rate %d/%d, want 512/1000", up, down)
	}
	if n := limit64(r, mtTotalLimit, mtTotalLimitGiga); n != 1<<32+100 {
		t.Fatalf("total limit %d", n)
	}
}

func TestRadiusReplyWithoutMessageAuthenticatorRefused(t *testing.T) {
	port, _ := fakeRadius(t, false, nil)
	g := testGateway(port)
	_, err := g.exchange(1, port, append(g.baseAttrs("aa:bb:cc:dd:ee:ff", "192.168.1.50"),
		aStr(attrUserName, "u"), attr{attrUserPassword, []byte("p")}))
	if err == nil || !strings.Contains(err.Error(), "Message-Authenticator") {
		t.Fatalf("unsigned Access-Accept accepted: %v", err)
	}
}

func TestLongUsernameNeverReachesRadius(t *testing.T) {
	if _, err := encodeAttrs([]attr{aStr(attrUserName, strings.Repeat("a", 254))}); err == nil {
		t.Fatal("254-byte attribute encoded (length byte would wrap)")
	}
	g := testGateway(9) // nothing listens there: the check must answer before any RADIUS traffic
	if msg := g.login("aa:bb:cc:dd:ee:ff", "192.168.1.50", strings.Repeat("a", 260), "x"); msg != "invalid username or password" {
		t.Fatalf("got %q", msg)
	}
}

func TestCounterDelta(t *testing.T) {
	for _, c := range []struct{ raw, last, want uint64 }{
		{1000, 400, 600}, // normal growth
		{400, 400, 0},    // idle
		{50, 500e6, 50},  // chain flushed (restart / fw3 reload): new counter is all new traffic
	} {
		if got := counterDelta(c.raw, c.last); got != c.want {
			t.Errorf("counterDelta(%d,%d)=%d want %d", c.raw, c.last, got, c.want)
		}
	}
}

func TestRequestSizeLimited(t *testing.T) {
	cl, sv := net.Pipe()
	defer cl.Close()
	go func() {
		chunk := bytes.Repeat([]byte("A"), 4096)
		for i := 0; i < 10; i++ { // 40 KB request line with no newline
			if _, err := cl.Write(chunk); err != nil {
				return
			}
		}
	}()
	if _, err := readRequest(sv); err == nil {
		t.Fatal("oversized request line accepted")
	}
}

func TestSecureURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://cautivo.h3s-iot.com/agent/v1/install/abc": true,
		"http://192.168.40.10:8080/agent/v1/install/abc":   true, // lab on a private address
		"http://cautivo.h3s-iot.com/agent/v1/install/abc":  false,
		"http://23.23.2.205/x":                             false,
		"ftp://10.0.0.1/x":                                 false,
	} {
		u, _ := url.Parse(raw)
		if err := secureURL(u); (err == nil) != ok {
			t.Errorf("%s: err=%v", raw, err)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	g := testGateway(9)
	for i := 0; i < 5; i++ {
		if !g.reserveAttempt("m") {
			t.Fatalf("blocked at attempt %d", i+1)
		}
	}
	if g.reserveAttempt("m") {
		t.Fatal("6th attempt in a minute (5 counted as failed) not blocked")
	}
	if !g.reserveAttempt("other") {
		t.Fatal("another device blocked")
	}
	g.releaseAttempt("other") // accepted: does not count
	if _, ok := g.fails["other"]; ok {
		t.Fatal("released attempt still counted")
	}
}

func TestLoginRateLimitGlobal(t *testing.T) {
	g := testGateway(9)
	n := 0
	for i := 0; i < 100; i++ { // random MACs: 4 guesses each
		for j := 0; j < 4; j++ {
			if g.reserveAttempt(fmt.Sprintf("02:00:00:00:%02x:%02x", i, j%2)) {
				n++
			}
		}
	}
	if n != 30 {
		t.Fatalf("%d guesses in a minute across MACs, want 30", n)
	}
}

func TestFreeSlotExhausted(t *testing.T) {
	g := testGateway(9)
	for i := 1; i <= 126; i++ {
		if s := g.freeSlot(); s != i {
			t.Fatalf("slot %d, want %d", s, i)
		}
	}
	if s := g.freeSlot(); s != 0 {
		t.Fatalf("slot %d after 126, want 0 (full)", s)
	}
}
