package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSingBoxLiveTraffic(t *testing.T) {
	localBin := filepath.Join(os.Getenv("LOCALAPPDATA"), "minivpn", "bin", "sing-box.exe")
	if _, err := os.Stat(localBin); err != nil {
		t.Skip("sing-box.exe not found")
	}

	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}

	p := Params{
		Name:     "test-node",
		Keys:     keys,
		Port:     28443,
		SNI:      "www.apple.com",
		APIBase:  "http://127.0.0.1",
		APIKey:   "key",
		Watchdog: DefaultWatchdog(),
	}

	serverJSON, err := ServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	serverCfgPath := filepath.Join(dir, "server.json")
	if err := os.WriteFile(serverCfgPath, serverJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, localBin, "run", "-c", serverCfgPath, "-D", dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer func() {
		cancel()
		cmd.Wait()
	}()

	time.Sleep(1 * time.Second)

	// Now start a client that acts like Happ (PhoneUUID, vless without flow)
	clientJSON := fmt.Sprintf(`{
		"log": {"level": "info"},
		"inbounds": [
			{"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": 28080}
		],
		"outbounds": [
			{
				"type": "vless",
				"tag": "proxy",
				"server": "127.0.0.1",
				"server_port": 28443,
				"uuid": %q,
				"tls": {
					"enabled": true,
					"server_name": "www.apple.com",
					"utls": {"enabled": true, "fingerprint": "chrome"},
					"reality": {
						"enabled": true,
						"public_key": %q,
						"short_id": %q
					}
				}
			}
		]
	}`, keys.PhoneUUID(), keys.PublicKey, keys.ShortID)

	clientCfgPath := filepath.Join(dir, "client.json")
	if err := os.WriteFile(clientCfgPath, []byte(clientJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	clientCmd := exec.CommandContext(ctx, localBin, "run", "-c", clientCfgPath, "-D", dir)
	clientCmd.Stdout = os.Stdout
	clientCmd.Stderr = os.Stderr
	if err := clientCmd.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer func() {
		clientCmd.Process.Kill()
		clientCmd.Wait()
	}()

	time.Sleep(1 * time.Second)

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL("http://127.0.0.1:28080")),
		},
		Timeout: 5 * time.Second,
	}

	resp, err := httpClient.Get("http://cp.cloudflare.com/generate_204")
	if err != nil {
		t.Fatalf("Happ-style test failed: %v", err)
	}
	resp.Body.Close()
	t.Logf("Happ-style test status: %d", resp.StatusCode)

	u, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		t.Fatalf("direct dial udp 1.1.1.1:53: %v", err)
	}
	defer u.Close()
	dnsMsg := []byte{
		0xaa, 0xbb, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x06, 'g', 'o', 'o', 'g', 'l', 'e', 0x03, 'c', 'o', 'm', 0x00, 0x00, 0x01, 0x00, 0x01,
	}
	u.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := u.Write(dnsMsg); err != nil {
		t.Fatalf("direct write udp: %v", err)
	}
	buf := make([]byte, 512)
	n, err := u.Read(buf)
	if err != nil {
		t.Fatalf("direct read udp 1.1.1.1:53: %v", err)
	}
	t.Logf("Direct UDP to 1.1.1.1:53 works! Got %d bytes", n)
	testUDPThroughSocks5(t, "127.0.0.1:28080", "1.1.1.1:53", dnsMsg)
}

func testUDPThroughSocks5(t *testing.T, socksAddr, destAddr string, payload []byte) {
	uconn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer uconn.Close()
	localPort := uconn.LocalAddr().(*net.UDPAddr).Port

	conn, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatalf("dial socks: %v", err)
	}
	defer conn.Close()

	// SOCKS5 greeting
	conn.Write([]byte{5, 1, 0})
	resp := make([]byte, 2)
	conn.Read(resp)

	// SOCKS5 UDP ASSOCIATE request with local address:
	conn.Write([]byte{5, 3, 0, 1, 127, 0, 0, 1, byte(localPort >> 8), byte(localPort & 0xff)})
	reply := make([]byte, 10)
	io.ReadFull(conn, reply)
	if reply[1] != 0 {
		t.Fatalf("UDP ASSOCIATE failed: %x", reply[1])
	}

	bndPort := int(reply[8])<<8 | int(reply[9])
	bndAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", bndPort))
	if err != nil {
		t.Fatalf("resolve bndAddr: %v", err)
	}
	t.Logf("SOCKS5 UDP relay at %s", bndAddr)

	// SOCKS5 UDP header: RSV(2) + FRAG(1) + ATYP(1) + IPv4(4) + Port(2) + Data
	host, portStr, _ := net.SplitHostPort(destAddr)
	ip := net.ParseIP(host).To4()
	port := 53
	fmt.Sscanf(portStr, "%d", &port)
	pkt := append([]byte{0, 0, 0, 1, ip[0], ip[1], ip[2], ip[3], byte(port >> 8), byte(port & 0xff)}, payload...)

	uconn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := uconn.WriteTo(pkt, bndAddr); err != nil {
		t.Fatalf("write udp: %v", err)
	}

	buf := make([]byte, 2048)
	n, _, err := uconn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read udp dns response: %v", err)
	}
	t.Logf("Got UDP DNS response of %d bytes", n)
}

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
