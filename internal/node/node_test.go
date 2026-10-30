package node

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testParams(t *testing.T) Params {
	t.Helper()
	k, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	return Params{
		Name:     "minivpn-20261028-ab12",
		Keys:     k,
		Port:     443,
		SNI:      "www.apple.com",
		APIBase:  "https://api.serverspace.ru",
		APIKey:   "secret",
		Watchdog: DefaultWatchdog(),
	}
}

func TestKeys(t *testing.T) {
	k, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	other, _ := NewKeys()
	k.PublicKey = other.PublicKey
	if k.Validate() == nil {
		t.Fatal("mismatched public key must not validate")
	}
	k, _ = NewKeys()
	k.CertKeyPEM = other.CertKeyPEM
	if k.Validate() == nil {
		t.Fatal("a certificate with someone else's key must not validate")
	}
}

func TestScript(t *testing.T) {
	p := testParams(t)
	s, err := Script(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "\r") {
		t.Fatal("script has CR characters; /bin/sh on the node will choke on them")
	}
	if !strings.HasPrefix(s, "#!/bin/sh\n") {
		t.Fatal("missing shebang")
	}
	for _, want := range []string{p.Keys.PrivateKey, p.Keys.UUID, `"api_key": "secret"`, `"idle_bytes": 1048576`, "def is_idle", SingBoxVersion, singBoxSHA256, p.Keys.HY2Password} {
		if !strings.Contains(s, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
	// Every heredoc must be closed, or the rest of the script ends up inside a file.
	if opened, closed := strings.Count(s, "<<'MINIVPN_EOF'"), strings.Count(s, "\nMINIVPN_EOF\n"); opened != closed {
		t.Fatalf("%d heredocs opened, %d closed", opened, closed)
	}
}

func TestServerConfig(t *testing.T) {
	p := testParams(t)
	raw, err := ServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Inbounds []struct {
			Type       string
			ListenPort int `json:"listen_port"`
			Users      []struct{ UUID, Flow, Password string }
			Multiplex  struct{ Enabled bool }
			Obfs       struct{ Type, Password string }
			TLS        struct {
				Certificate []string
				Reality     struct {
					Handshake struct{ Server string }
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	vl, hy := cfg.Inbounds[0], cfg.Inbounds[1]
	// Both phone and PC users run without Vision so they can pass HTTP and UDP (DNS) without breakages.
	if vl.Type != "vless" || vl.ListenPort != 443 || len(vl.Users) != 2 ||
		vl.Users[0].UUID != p.Keys.PhoneUUID() || vl.Users[0].Flow != "" ||
		vl.Users[1].UUID != p.Keys.UUID || vl.Users[1].Flow != "" || !vl.Multiplex.Enabled {
		t.Fatalf("reality inbound: %+v", vl)
	}
	if vl.TLS.Reality.Handshake.Server != "www.apple.com" {
		t.Fatalf("reality target: %+v", vl.TLS.Reality)
	}
	if hy.Type != "hysteria2" || hy.ListenPort != 443 || hy.Users[0].Password != p.Keys.HY2Password ||
		hy.Obfs.Type != "salamander" || hy.Obfs.Password != p.Keys.HY2Obfs || len(hy.TLS.Certificate) < 3 {
		t.Fatalf("hysteria2 inbound: %+v", hy)
	}

	var fullCfg struct {
		Route struct {
			Rules []struct {
				Action   string
				Protocol string
			}
		}
	}
	if err := json.Unmarshal(raw, &fullCfg); err != nil {
		t.Fatal(err)
	}
	if len(fullCfg.Route.Rules) != 3 || fullCfg.Route.Rules[0].Action != "sniff" {
		t.Fatalf("bad route config: %+v", fullCfg.Route)
	}

	link := (Endpoint{Address: "1.2.3.4", Port: 443, SNI: p.SNI, Keys: p.Keys}).Link("test")
	if strings.Contains(link, "flow=") {
		t.Fatalf("link must not have flow: %s", link)
	}
}

func TestSingBoxCheck(t *testing.T) {
	bin := os.Getenv("MINIVPN_SINGBOX")
	if bin == "" {
		localBin := filepath.Join(os.Getenv("LOCALAPPDATA"), "minivpn", "bin", "sing-box.exe")
		if _, err := os.Stat(localBin); err == nil {
			bin = localBin
		}
	}
	if bin == "" {
		t.Skip("sing-box binary not found")
	}
	p := testParams(t)
	raw, err := ServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "check", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("sing-box check: %v\n%s", err, out)
	}
}

func TestValidate(t *testing.T) {
	for name, mod := range map[string]func(*Params){
		"no key":        func(p *Params) { p.APIKey = "" },
		"quote in name": func(p *Params) { p.Name = "a'b" },
		"idle > max":    func(p *Params) { p.Watchdog.MaxAge = p.Watchdog.Idle },
	} {
		p := testParams(t)
		mod(&p)
		if _, err := Script(p); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
