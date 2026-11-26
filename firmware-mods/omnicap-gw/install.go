package main

// Self-registration in omnicap with an install key, the same flow the buses use
// (POST <agent>/agent/v1/install/<key>/<identity>), reporting platform "openwrt": omnicap creates the device in
// fleet mode (NAS-Identifier = identity) and answers JSON with everything needed. The values are written to
// uci omnicapgw (the web UI then shows them) and the portal is enabled.
//
//   omnicap-gw -install https://cautivo.h3s-iot.com/agent/v1/install/<key> -identity NOTION5G_0001

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var identityRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type installAnswer struct {
	Result        string   `json:"result"`
	Error         string   `json:"error"`
	NASIdentifier string   `json:"nas_identifier"`
	AgentURL      string   `json:"agent_url"`
	AgentToken    string   `json:"agent_token"`
	PortalURL     string   `json:"portal_url"`
	RadiusServer  string   `json:"radius_server"`
	RadiusSecret  string   `json:"radius_secret"`
	LoginHost     string   `json:"login_host"`
	WalledGarden  []string `json:"walled_garden"`
	RadiusEnabled bool     `json:"radius_enabled"`
	Reinstalled   bool     `json:"reinstalled"`
}

// runInstall prints one JSON line ({"result":"ok",...} or {"result":"fail","error":...}) and returns the exit code.
func runInstall(installURL, identity, pkg string) int {
	fail := func(msg string) int {
		b, _ := json.Marshal(map[string]string{"result": "fail", "error": msg})
		fmt.Println(string(b))
		return 1
	}
	installURL = strings.TrimRight(strings.TrimSpace(installURL), "/")
	u, err := url.Parse(installURL)
	if err != nil || !strings.Contains(installURL, "/agent/v1/install/") {
		return fail("the install URL must look like https://<omnicap>/agent/v1/install/<key>")
	}
	if err := secureURL(u); err != nil {
		return fail(err.Error())
	}
	if !identityRE.MatchString(identity) {
		return fail("the identity only allows letters, digits, _ . - (max 64)")
	}
	board, _ := os.ReadFile("/proc/device-tree/model")
	ver, _ := os.ReadFile("/etc/openwrt_version")
	report, _ := json.Marshal(map[string]string{"platform": "openwrt", "identity": identity,
		"version": "OpenWrt " + strings.TrimSpace(string(ver)), "board": strings.Trim(string(board), "\x00 \n")})
	body, err := agentRequest("POST", installURL+"/"+identity, "", report)
	if err != nil {
		return fail(err.Error())
	}
	var a installAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return fail("omnicap did not answer JSON: is this omnicap version able to install OpenWrt devices?")
	}
	if a.Result != "ok" {
		return fail(a.Error)
	}
	if a.NASIdentifier == "" || a.RadiusServer == "" || a.RadiusSecret == "" || a.PortalURL == "" || a.AgentToken == "" {
		return fail("omnicap answered an incomplete configuration")
	}
	set := func(k, v string) error {
		_, err := sh("uci", "set", pkg+".main."+k+"="+v)
		return err
	}
	if _, err := sh("uci", "-q", "get", pkg+".main"); err != nil {
		sh("uci", "set", pkg+".main="+pkg)
	}
	for k, v := range map[string]string{
		"enabled": "1", "portal_url": a.PortalURL, "nas_identifier": a.NASIdentifier,
		"radius_server": a.RadiusServer, "radius_secret": a.RadiusSecret, "agent_url": a.AgentURL,
		"agent_token": a.AgentToken, "cert_source": "agent",
	} {
		if err := set(k, v); err != nil {
			shq("uci", "revert", pkg) // drop the values staged so far: the next commit (web UI) must not save half a registration
			return fail(err.Error())
		}
	}
	if a.LoginHost != "" {
		set("https", "1")
		set("login_host", a.LoginHost)
	}
	sh("uci", "-q", "delete", pkg+".main.walled_garden")
	for _, w := range a.WalledGarden {
		if validWG(w) {
			sh("uci", "add_list", pkg+".main.walled_garden="+w)
		}
	}
	if _, err := sh("uci", "commit", pkg); err != nil {
		return fail(err.Error())
	}
	b, _ := json.Marshal(struct {
		Result        string `json:"result"`
		NASIdentifier string `json:"nas_identifier"`
		PortalURL     string `json:"portal_url"`
		RadiusServer  string `json:"radius_server"`
		LoginHost     string `json:"login_host"`
		RadiusEnabled bool   `json:"radius_enabled"`
		Reinstalled   bool   `json:"reinstalled"`
	}{"ok", a.NASIdentifier, a.PortalURL, a.RadiusServer, a.LoginHost, a.RadiusEnabled, a.Reinstalled})
	fmt.Println(string(b))
	return 0
}
