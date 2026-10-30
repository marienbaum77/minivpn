package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/marienbaum77/minivpn/internal/node"
)

func endpoint(t *testing.T) node.Endpoint {
	k, err := node.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	return node.Endpoint{Address: "203.0.113.7", Port: 443, SNI: "www.apple.com", Keys: k}
}

// sing-box check validates the schema but not references between sections,
// so a typo in a tag only shows up when the PC loses internet. Check them here.
func TestSingBoxReferences(t *testing.T) {
	r := DefaultRules()
	r.GameDirs = []string{`D:\SteamLibrary\steamapps\common`}
	raw, err := SingBox(endpoint(t), r)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		DNS struct {
			Servers []struct{ Tag, Detour string }
			Rules   []map[string]any
			Final   string
		}
		Outbounds []struct{ Tag string }
		Route     struct {
			Rules                 []map[string]any
			RuleSet               []struct{ Tag string } `json:"rule_set"`
			Final                 string
			DefaultDomainResolver string `json:"default_domain_resolver"`
		}
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	var outbounds, servers, sets []string
	for _, o := range cfg.Outbounds {
		outbounds = append(outbounds, o.Tag)
	}
	for _, s := range cfg.DNS.Servers {
		servers = append(servers, s.Tag)
		if s.Detour != "" && !slices.Contains(outbounds, s.Detour) {
			t.Errorf("dns server %s: unknown detour %s", s.Tag, s.Detour)
		}
	}
	for _, s := range cfg.Route.RuleSet {
		sets = append(sets, s.Tag)
	}
	check := func(where string, rule map[string]any) {
		if o, ok := rule["outbound"].(string); ok && !slices.Contains(outbounds, o) {
			t.Errorf("%s: unknown outbound %s", where, o)
		}
		if s, ok := rule["server"].(string); ok && !slices.Contains(servers, s) {
			t.Errorf("%s: unknown dns server %s", where, s)
		}
		if rs, ok := rule["rule_set"].([]any); ok {
			for _, x := range rs {
				if !slices.Contains(sets, x.(string)) {
					t.Errorf("%s: unknown rule set %s", where, x)
				}
			}
		}
	}
	for _, r := range cfg.DNS.Rules {
		check("dns rule", r)
	}
	for _, r := range cfg.Route.Rules {
		check("route rule", r)
	}
	if !slices.Contains(outbounds, cfg.Route.Final) || !slices.Contains(servers, cfg.DNS.Final) || !slices.Contains(servers, cfg.Route.DefaultDomainResolver) {
		t.Error("final outbound, final dns server or default resolver is unknown")
	}
}

func TestURLTestPrefersRealityWhenLatencyIsClose(t *testing.T) {
	raw, err := SingBox(endpoint(t), DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []struct {
			Type      string
			Tag       string
			Outbounds []string
			Tolerance int
		}
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, out := range cfg.Outbounds {
		if out.Type != "urltest" {
			continue
		}
		if len(out.Outbounds) != 2 || out.Outbounds[0] != TagReality || out.Outbounds[1] != TagHysteria || out.Tolerance != 50 {
			t.Fatalf("urltest should prefer Reality within 50 ms, got %+v", out)
		}
		return
	}
	t.Fatal("urltest outbound not found")
}

// TestSingBoxCheck runs the real binary when MINIVPN_SINGBOX points to it
// (CI and the e2e script set it).
func TestSingBoxCheck(t *testing.T) {
	bin := os.Getenv("MINIVPN_SINGBOX")
	if bin == "" {
		t.Skip("MINIVPN_SINGBOX not set")
	}
	r := DefaultRules()
	r.GameDirs = []string{`D:\SteamLibrary\steamapps\common`}
	raw, err := SingBox(endpoint(t), r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "check", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("sing-box check: %v\n%s", err, out)
	}
}

func TestGameDirRegex(t *testing.T) {
	re := GameDirRegex([]string{`D:/SteamLibrary/steamapps/common/`, `d:\steamlibrary\steamapps\common`, `C:\Program Files (x86)\Steam\steamapps\common`})
	if len(re) != 2 {
		t.Fatalf("duplicates must collapse: %v", re)
	}
	game := regexp.MustCompile(re[1])
	if !game.MatchString(`C:\program files (x86)\Steam\steamapps\common\Dota 2\game\bin\win64\dota2.exe`) {
		t.Error("game inside the library must match")
	}
	if game.MatchString(`C:\Program Files (x86)\Steam\steam.exe`) {
		t.Error("the launcher itself is not inside the library")
	}
}

func TestSteamLibraries(t *testing.T) {
	vdf := `"libraryfolders"
{
	"0"
	{
		"path"		"C:\\Program Files (x86)\\Steam"
		"label"		""
	}
	"1"
	{
		"path"		"D:\\SteamLibrary"
	}
}`
	got := SteamLibraries(vdf)
	want := []string{`C:\Program Files (x86)\Steam`, `D:\SteamLibrary`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEpicInstallDirs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.item"), []byte(`{"DisplayName":"Fortnite","InstallLocation":"E:\\Epic\\Fortnite"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.item"), []byte(`{`), 0o644)
	got := EpicInstallDirs(dir)
	if !slices.Equal(got, []string{`E:\Epic\Fortnite`}) {
		t.Fatalf("got %q", got)
	}
}

func TestLink(t *testing.T) {
	e := endpoint(t)
	l := e.Link("minivpn nj")
	if !regexp.MustCompile(`^vless://[0-9a-f-]{36}@203\.0\.113\.7:443\?.*security=reality.*#minivpn%20nj$`).MatchString(l) {
		t.Fatalf("link: %s", l)
	}
}
