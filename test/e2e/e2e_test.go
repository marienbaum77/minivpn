//go:build e2e

// End-to-end: the app's SSH installer runs the real setup script in an
// Ubuntu container, which installs the real sing-box and the watchdog. The
// PC-side checker connects through Reality, a sing-box client carries
// traffic through both Hysteria2 and Reality with the PC's outbounds, and
// the watchdog deletes the node through a fake hoster API once traffic
// stops.
//
// Run: go test -tags e2e -v -timeout 30m ./test/e2e/
// Optional: MINIVPN_E2E_SINGBOX_TGZ=/path/sing-box-linux-amd64.tar.gz skips
// the download; MINIVPN_E2E_PYTHON= (set, empty) uses an image without python3.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marienbaum77/minivpn/internal/check"
	"github.com/marienbaum77/minivpn/internal/client"
	"github.com/marienbaum77/minivpn/internal/node"
)

const nodeName = "minivpn-e2e-000000"

func compose(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose", "-f", "compose.yml"}, args...)...)
	cmd.Env = append(os.Environ(), "NODE_NAME="+nodeName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func setupLog(t *testing.T) string {
	cmd := exec.Command("docker", "compose", "-f", "compose.yml", "exec", "-T", "node", "grep", "-v", "not found in the API", "/var/log/minivpn-setup.log")
	cmd.Env = append(os.Environ(), "NODE_NAME="+nodeName)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func TestNode(t *testing.T) {
	keys, err := node.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	p := node.Params{
		Name: nodeName, Keys: keys, Port: 443, SNI: "www.apple.com",
		APIBase: "http://api:8080", APIKey: "e2e-key",
		// Short timings: idle after 20 s of less than 64 KB per 5 s.
		Watchdog: node.Watchdog{Idle: 20 * time.Second, Window: 5 * time.Second, IdleBytes: 64 << 10, MaxAge: 10 * time.Minute},
	}
	script, err := node.Script(p)
	if err != nil {
		t.Fatal(err)
	}
	work := ".work"
	os.RemoveAll(work)
	os.MkdirAll(work, 0o755)
	env := map[string]string{"MINIVPN_NO_SYSTEMD": "1", "MINIVPN_TICK": "2"}
	if tgz := os.Getenv("MINIVPN_E2E_SINGBOX_TGZ"); tgz != "" {
		env["MINIVPN_SINGBOX_TGZ"] = "/work/sing-box.tar.gz"
		data, err := os.ReadFile(tgz)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(work, "sing-box.tar.gz"), data, 0o644)
	}

	compose(t, "down", "-v", "--remove-orphans")
	compose(t, "build")
	compose(t, "up", "-d")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("setup log:\n%s", setupLog(t))
		}
		compose(t, "down", "-v", "--remove-orphans")
	})

	ctx := context.Background()

	// 1. The installer logs in, runs the script and reads its log to the end.
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	login := node.SSH{Addr: "127.0.0.1:18022", User: "root", Password: "e2e-root", Env: env}
	if err := node.Install(ictx, login, script, func(line string) { t.Log("node:", line) }); err != nil {
		t.Fatalf("install: %v", err)
	}

	// 2. The node carries traffic through Reality.
	ep := node.Endpoint{Address: "127.0.0.1", Port: 18443, SNI: p.SNI, Keys: keys}
	checker := check.Default()
	var res check.Result
	for i := 0; i < 10; i++ {
		if res = checker.Check(ctx, ep); res.OK {
			break
		}
		t.Logf("not ready: %s at %s", res.Error, res.Failed)
		time.Sleep(3 * time.Second)
	}
	if !res.OK {
		t.Fatalf("node never passed the check: %s at %s", res.Error, res.Failed)
	}
	t.Logf("check: handshake %s, bulk %d bytes in %s", res.Handshake, res.BulkBytes, res.Bulk)

	// 3. A wrong key is refused: Reality falls back to the real site.
	bad := ep
	bad.Keys.UUID = "00000000-0000-0000-0000-000000000000"
	if r := checker.Check(ctx, bad); r.OK {
		t.Fatal("an unknown user must not get through")
	}

	// 4. sing-box accepts the PC config pointing at this node.
	cfg, err := client.SingBox(ep, client.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "client.json"), cfg, 0o644)
	abs, _ := filepath.Abs(work)
	if out, err := exec.Command("docker", "run", "--rm", "-v", abs+":/w:ro", singBoxImage, "check", "-c", "/w/client.json").CombinedOutput(); err != nil {
		t.Fatalf("sing-box check: %v\n%s", err, out)
	}

	// 5. The PC's outbounds carry traffic through each protocol on its own.
	inner := ep
	inner.Address, inner.Port = "node", 443
	for tag, port := range throughNode(t, work, inner) {
		proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
		hc := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
		var err error
		for i := 0; i < 10; i++ {
			var resp *http.Response
			if resp, err = hc.Get("https://www.gstatic.com/generate_204"); err == nil {
				resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent {
					err = fmt.Errorf("status %s", resp.Status)
				}
			}
			if err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			t.Fatalf("through %s: %v", tag, err)
		}
		t.Logf("through %s: ok", tag)
	}

	// 6. With no traffic the watchdog deletes its own server, and only it.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if d := state(t).Deleted; len(d) > 0 {
			if !slices.Equal(d, []string{"l1s1"}) {
				t.Fatalf("deleted %v, want only the node's own server", d)
			}
			t.Logf("watchdog deleted the node")
			if log := setupLog(t); !strings.Contains(log, "setup done") {
				t.Errorf("setup did not finish:\n%s", log)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("the watchdog did not delete the idle node")
}

const singBoxImage = "ghcr.io/sagernet/sing-box:v1.14.4"

// throughNode starts a sing-box client next to the node with one local HTTP
// proxy port per protocol, each pinned to that outbound.
func throughNode(t *testing.T, work string, ep node.Endpoint) map[string]int {
	ports := map[string]int{client.TagHysteria: 18091, client.TagReality: 18092}
	raw, err := client.PerProtocol(ep, ports)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "through.json"), raw, 0o644)
	abs, _ := filepath.Abs(work)
	const name = "minivpn-e2e-client"
	args := []string{"run", "-d", "--name", name, "--network", "minivpn-e2e_default", "-v", abs + ":/w:ro"}
	for _, port := range ports {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port))
	}
	exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", append(args, singBoxImage, "run", "-c", "/w/through.json")...).CombinedOutput(); err != nil {
		t.Fatalf("sing-box client: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Logf("sing-box client log:\n%s", out)
		}
		exec.Command("docker", "rm", "-f", name).Run()
	})
	time.Sleep(2 * time.Second)
	return ports
}

type apiState struct {
	Deleted []string
}

func state(t *testing.T) apiState {
	var v apiState
	resp, err := http.Get("http://127.0.0.1:18080/_state")
	if err != nil {
		return v
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &v)
	return v
}
