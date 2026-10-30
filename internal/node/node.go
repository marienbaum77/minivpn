package node

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// sing-box release the node installs: the same version as the PC client.
// The checksum is the release asset's digest; bump both together.
const (
	SingBoxVersion = "1.14.4"
	singBoxSHA256  = "699a1b510aa4396b2176d32d1181920399ebf1e267835da6f3b598d0f7c24968"
)

const (
	Fingerprint = "chrome"
	// statsAddr is sing-box's Clash API, where the watchdog reads traffic.
	statsAddr = "127.0.0.1:9090"
)

// Watchdog decides when the node deletes itself.
type Watchdog struct {
	// Idle: the node is idle when every Window-long slice of the last Idle
	// carried less than IdleBytes. System background traffic is a few KB per
	// minute; a stream is tens of MB.
	Idle      time.Duration
	Window    time.Duration
	IdleBytes int64
	// MaxAge deletes the node no matter what. It is the last line of defense
	// against a hung app keeping a node alive with background traffic.
	MaxAge time.Duration
}

func DefaultWatchdog() Watchdog {
	return Watchdog{Idle: 20 * time.Minute, Window: 5 * time.Minute, IdleBytes: 1 << 20, MaxAge: 12 * time.Hour}
}

type Params struct {
	// Name is unique per session; the watchdog finds its own server by it.
	Name string
	Keys Keys
	Port int
	// SNI is a real third-party TLS 1.3 site the node impersonates.
	SNI string
	// APIBase and APIKey let the node delete itself.
	APIBase  string
	APIKey   string
	Watchdog Watchdog
}

func (p Params) validate() error {
	if p.Name == "" || strings.ContainsAny(p.Name, " '\"\n") {
		return fmt.Errorf("bad node name %q", p.Name)
	}
	if p.Port <= 0 || p.SNI == "" || p.APIBase == "" || p.APIKey == "" {
		return fmt.Errorf("port, sni, api base and api key are required")
	}
	if p.Watchdog.Window <= 0 || p.Watchdog.Idle < p.Watchdog.Window || p.Watchdog.MaxAge <= p.Watchdog.Idle {
		return fmt.Errorf("watchdog: need 0 < window <= idle < max age")
	}
	return p.Keys.Validate()
}

// ServerConfig is the node's sing-box config. Both protocols listen on the
// same port number, Hysteria2 on UDP and Reality on TCP; the client prefers
// Hysteria2 and falls back to Reality where UDP is cut.
//
// Reality runs without Vision and with multiplexing: every new site then
// rides an open connection instead of paying for its own TCP and TLS
// handshakes across the ocean. Torrents and private ranges are cut on the
// node too, so a phone with a bare vless:// link also gets that.
func ServerConfig(p Params) ([]byte, error) {
	type obj = map[string]any
	cfg := obj{
		"log": obj{"level": "warn"},
		"inbounds": []any{
			obj{
				"type":        "vless",
				"tag":         "reality-in",
				"listen":      "::",
				"listen_port": p.Port,
				"users": []any{
					obj{"name": "phone", "uuid": p.Keys.PhoneUUID()},
					obj{"name": "pc", "uuid": p.Keys.UUID},
				},
				"tls": obj{
					"enabled":     true,
					"server_name": p.SNI,
					"reality": obj{
						"enabled":     true,
						"handshake":   obj{"server": p.SNI, "server_port": 443},
						"private_key": p.Keys.PrivateKey,
						"short_id":    []string{p.Keys.ShortID},
					},
				},
				"multiplex": obj{"enabled": true},
			},
			obj{
				"type":        "hysteria2",
				"tag":         "hy2-in",
				"listen":      "::",
				"listen_port": p.Port,
				"users":       []any{obj{"password": p.Keys.HY2Password}},
				// Salamander makes every packet look random: no QUIC
				// handshake for DPI to read.
				"obfs": obj{"type": "salamander", "password": p.Keys.HY2Obfs},
				"tls": obj{
					"enabled":     true,
					"alpn":        []string{"h3"},
					"certificate": pemLines(p.Keys.CertPEM),
					"key":         pemLines(p.Keys.CertKeyPEM),
				},
			},
		},
		"outbounds": []any{obj{"type": "direct", "tag": "direct"}},
		"route": obj{
			"rules": []any{
				obj{"action": "sniff"},
				obj{"ip_is_private": true, "action": "reject"},
				obj{"protocol": "bittorrent", "action": "reject"},
			},
		},
		"experimental": obj{"clash_api": obj{"external_controller": statsAddr}},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func pemLines(s string) []string {
	return strings.Split(strings.TrimSpace(s), "\n")
}

func watchdogConfig(p Params) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"name":           p.Name,
		"api_base":       strings.TrimRight(p.APIBase, "/"),
		"api_key":        p.APIKey,
		"stats_url":      "http://" + statsAddr + "/connections",
		"idle_seconds":   int(p.Watchdog.Idle.Seconds()),
		"window_seconds": int(p.Watchdog.Window.Seconds()),
		"idle_bytes":     p.Watchdog.IdleBytes,
		"max_seconds":    int(p.Watchdog.MaxAge.Seconds()),
	}, "", "  ")
}

//go:embed setup.sh.tmpl
var setupTmpl string

//go:embed watchdog.py
var watchdogPy string

var setup = template.Must(template.New("setup").Parse(setupTmpl))

// Script is the setup script Install runs on the node.
func Script(p Params) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	server, err := ServerConfig(p)
	if err != nil {
		return "", err
	}
	wd, err := watchdogConfig(p)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = setup.Execute(&b, map[string]string{
		"SingBoxVersion": SingBoxVersion,
		"SingBoxSHA256":  singBoxSHA256,
		"ServerConfig":   string(server),
		"WatchdogConfig": string(wd),
		"WatchdogPy":     watchdogPy,
	})
	return b.String(), err
}
