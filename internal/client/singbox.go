// Package client renders the sing-box config the PC runs during a session.
//
// Everything goes through the node except the exceptions: Russian domains
// and addresses, games, chosen programs and torrents. Ads are rejected at
// DNS and connection level.
package client

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/marienbaum77/minivpn/internal/node"
)

// RuleSet is a sing-box binary rule-set downloaded through the node and
// cached between sessions.
type RuleSet struct {
	Name string
	URL  string
	// IP rule-sets cannot be used in DNS rules, so they are kept apart.
	IP bool
}

type Rules struct {
	DirectDomainSuffixes []string
	DirectRuleSets       []RuleSet
	BlockRuleSets        []RuleSet
	// GameDirs: anything started from these folders goes direct.
	GameDirs []string
	// DirectProcesses are exe names, e.g. "steam.exe".
	DirectProcesses []string
	DirectDNS       string
	RemoteDNS       string
}

func DefaultRules() Rules {
	const geosite = "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/"
	const geoip = "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/"
	return Rules{
		DirectDomainSuffixes: []string{"ru", "su", "xn--p1ai", "xn--d1acj3b", "xn--80asehdb"}, // .ru .su .рф .дети .онлайн
		DirectRuleSets: []RuleSet{
			{Name: "geosite-category-ru", URL: geosite + "geosite-category-ru.srs"},
			{Name: "geoip-ru", URL: geoip + "geoip-ru.srs", IP: true},
		},
		BlockRuleSets: []RuleSet{
			{Name: "geosite-category-ads-all", URL: geosite + "geosite-category-ads-all.srs"},
		},
		DirectProcesses: DefaultDirectProcesses,
		DirectDNS:       "77.88.8.8",
		RemoteDNS:       "1.1.1.1",
	}
}

// DefaultDirectProcesses are launchers and torrent clients: their downloads
// are large, work from Russia and would only eat the node's 50 Mbit/s.
// minivpn.exe itself talks to the hoster and checks the node directly.
var DefaultDirectProcesses = []string{
	"minivpn.exe",
	"steam.exe", "steamwebhelper.exe", "steamservice.exe",
	"EpicGamesLauncher.exe", "EpicWebHelper.exe",
	"RiotClientServices.exe", "Battle.net.exe", "GalaxyClient.exe",
	"qbittorrent.exe", "uTorrent.exe", "BitTorrent.exe",
}

const (
	TagHysteria = "hy2"
	TagReality  = "reality"

	tagProxy  = "proxy"
	tagDirect = "direct"
	dnsRemote = "dns-remote"
	dnsDirect = "dns-direct"
)

type obj = map[string]any

// SingBox renders a sing-box 1.14 config with a TUN inbound for Windows.
func SingBox(e node.Endpoint, r Rules) ([]byte, error) {
	if e.Address == "" {
		return nil, fmt.Errorf("node has no address")
	}
	var domainSets, ipSets, blockSets []string
	for _, rs := range r.DirectRuleSets {
		if rs.IP {
			ipSets = append(ipSets, rs.Name)
		} else {
			domainSets = append(domainSets, rs.Name)
		}
	}
	for _, rs := range r.BlockRuleSets {
		blockSets = append(blockSets, rs.Name)
	}
	gameRegex := GameDirRegex(r.GameDirs)

	// The same matchers decide DNS and routing: a game's lookups must not go
	// through the node either, or CDNs hand it foreign servers.
	var direct []obj
	if len(r.DirectProcesses) > 0 {
		direct = append(direct, obj{"process_name": r.DirectProcesses})
	}
	if len(gameRegex) > 0 {
		direct = append(direct, obj{"process_path_regex": gameRegex})
	}
	if len(r.DirectDomainSuffixes) > 0 {
		direct = append(direct, obj{"domain_suffix": r.DirectDomainSuffixes})
	}
	if len(domainSets) > 0 {
		direct = append(direct, obj{"rule_set": domainSets})
	}

	var dnsRules []any
	if len(blockSets) > 0 {
		dnsRules = append(dnsRules, obj{"rule_set": blockSets, "action": "reject"})
	}
	for _, m := range direct {
		dnsRules = append(dnsRules, with(m, "server", dnsDirect))
	}

	// First match wins.
	routeRules := []any{
		obj{"action": "sniff"},
		obj{"protocol": "dns", "action": "hijack-dns"},
		obj{"ip_is_private": true, "outbound": tagDirect},
	}
	if len(blockSets) > 0 {
		routeRules = append(routeRules, obj{"rule_set": blockSets, "action": "reject"})
	}
	// Torrents through a VPS bring abuse letters and get servers banned.
	routeRules = append(routeRules, obj{"protocol": "bittorrent", "outbound": tagDirect})
	for _, m := range direct {
		routeRules = append(routeRules, with(m, "outbound", tagDirect))
	}
	if len(ipSets) > 0 {
		// Resolve after the domain rules so known Russian names skip a lookup.
		// This catches Russian services on foreign domains and bare IPs.
		routeRules = append(routeRules,
			obj{"action": "resolve", "strategy": "ipv4_only"},
			obj{"rule_set": ipSets, "outbound": tagDirect})
	}

	var ruleSets []any
	for _, rs := range append(append([]RuleSet{}, r.DirectRuleSets...), r.BlockRuleSets...) {
		ruleSets = append(ruleSets, obj{
			"type":            "remote",
			"tag":             rs.Name,
			"format":          "binary",
			"url":             rs.URL,
			"download_detour": tagProxy, // raw.githubusercontent.com is unreliable from Russia
			"update_interval": "72h",
		})
	}

	cfg := obj{
		"log": obj{"level": "warn", "timestamp": true},
		"dns": obj{
			"servers": []any{
				obj{"type": "https", "tag": dnsRemote, "server": r.RemoteDNS, "detour": tagProxy},
				obj{"type": "udp", "tag": dnsDirect, "server": r.DirectDNS},
			},
			"rules":    dnsRules,
			"final":    dnsRemote,
			"strategy": "ipv4_only",
		},
		"inbounds": []any{obj{
			"type":           "tun",
			"tag":            "tun-in",
			"interface_name": "minivpn", // tunnel.InterfaceName: lets the app tell its own adapter from other VPNs
			"address":        []string{"172.19.0.1/30"},
			"auto_route":     true,
			"strict_route":   true,
			"stack":          "mixed",
		}},
		"outbounds": append(Outbounds(e), obj{"type": "direct", "tag": tagDirect}),
		"route": obj{
			"rules":                   routeRules,
			"rule_set":                ruleSets,
			"final":                   tagProxy,
			"auto_detect_interface":   true,
			"default_domain_resolver": dnsDirect,
		},
		// The cache keeps rule-sets between sessions, so a new node starts
		// with them already in place.
		"experimental": obj{"cache_file": obj{"enabled": true}},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// Outbounds are the ways to the node, grouped under tagProxy. URLTest measures
// response latency, not download speed, so prefer Reality when results are
// close; its multiplexed TCP connections also avoid repeated handshakes.
// Hysteria2 wins when its QUIC path is clearly faster or Reality is blocked.
func Outbounds(e node.Endpoint) []any {
	return []any{
		obj{
			"type":      "urltest",
			"tag":       tagProxy,
			"outbounds": []string{TagReality, TagHysteria},
			"interval":  "1m",
			"tolerance": 50,
		},
		obj{
			"type":        "hysteria2",
			"tag":         TagHysteria,
			"server":      e.Address,
			"server_port": e.Port,
			"password":    e.Keys.HY2Password,
			"obfs":        obj{"type": "salamander", "password": e.Keys.HY2Obfs},
			"tls": obj{
				"enabled":     true,
				"server_name": node.HY2ServerName,
				"alpn":        []string{"h3"},
				"certificate": strings.Split(strings.TrimSpace(e.Keys.CertPEM), "\n"),
			},
		},
		obj{
			"type":        "vless",
			"tag":         TagReality,
			"server":      e.Address,
			"server_port": e.Port,
			"uuid":        e.Keys.UUID,
			"tls": obj{
				"enabled":     true,
				"server_name": e.SNI,
				"utls":        obj{"enabled": true, "fingerprint": node.Fingerprint},
				"reality":     obj{"enabled": true, "public_key": e.Keys.PublicKey, "short_id": e.Keys.ShortID},
			},
			"multiplex": obj{"enabled": true, "protocol": "h2mux", "max_connections": 4},
		},
	}
}

// PerProtocol renders a sing-box config with a local HTTP/SOCKS proxy per
// protocol, each pinned to that outbound: tests and probes measure the
// protocols apart. ports maps an outbound tag to its local port.
func PerProtocol(e node.Endpoint, ports map[string]int) ([]byte, error) {
	var inbounds, rules []any
	for tag, port := range ports {
		inbounds = append(inbounds, obj{"type": "mixed", "tag": "in-" + tag, "listen": "0.0.0.0", "listen_port": port})
		rules = append(rules, obj{"inbound": "in-" + tag, "outbound": tag})
	}
	return json.MarshalIndent(obj{
		"log":       obj{"level": "warn"},
		"inbounds":  inbounds,
		"outbounds": Outbounds(e),
		"route":     obj{"rules": rules},
	}, "", "  ")
}

func with(m obj, k string, v any) obj {
	out := obj{k: v}
	for mk, mv := range m {
		out[mk] = mv
	}
	return out
}

// GameDirRegex turns folders into case-insensitive prefix patterns for
// sing-box's process_path_regex.
func GameDirRegex(dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		d = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(d), "/", `\`), `\`)
		if d == "" || seen[strings.ToLower(d)] {
			continue
		}
		seen[strings.ToLower(d)] = true
		out = append(out, `(?i)^`+regexp.QuoteMeta(d+`\`))
	}
	return out
}
