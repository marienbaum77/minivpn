// Package check tests a node the way real traffic uses it: an in-process
// Xray client with the node's exact Reality settings fetches URLs through
// the tunnel. A TCP ping or an HTTPS GET to port 443 would succeed even when
// DPI kills the actual VPN traffic.
package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"

	"github.com/marienbaum77/minivpn/internal/node"
)

// Stage is where a check failed; it hints at the kind of blocking.
type Stage string

const (
	StageTCP       Stage = "tcp"       // IP or subnet blocked, or the node is down
	StageHandshake Stage = "handshake" // protocol or SNI filtering, or Xray is not up
	StageBulk      Stage = "bulk"      // the 16-20 KB "freeze" typical for TSPU
)

type Result struct {
	At        time.Time
	OK        bool
	Failed    Stage
	Error     string
	TCP       time.Duration
	Handshake time.Duration
	Bulk      time.Duration
	BulkBytes int64
	// SpeedMbps is measured only when the Checker has a SpeedURL.
	SpeedMbps float64
}

type Checker struct {
	// SmallURL should return a tiny response; it measures the handshake.
	SmallURL string
	// BulkURL should return at least BulkMin bytes, well above the 16-20 KB
	// where DPI freezes usually hit.
	BulkURL string
	BulkMin int64
	// SpeedURL is a larger download for throughput; empty skips it.
	SpeedURL    string
	TCPTimeout  time.Duration
	StepTimeout time.Duration
}

func Default() Checker {
	return Checker{
		SmallURL:    "https://www.gstatic.com/generate_204",
		BulkURL:     "https://speed.cloudflare.com/__down?bytes=262144",
		BulkMin:     262144,
		TCPTimeout:  5 * time.Second,
		StepTimeout: 20 * time.Second,
	}
}

// WithSpeed adds a 10 MB download, used once per session.
func (c Checker) WithSpeed() Checker {
	c.SpeedURL = "https://speed.cloudflare.com/__down?bytes=10000000"
	return c
}

// Check runs all stages against one node and stops at the first failure.
func (c Checker) Check(ctx context.Context, e node.Endpoint) Result {
	res := Result{At: time.Now()}
	start := time.Now()
	d := net.Dialer{Timeout: c.TCPTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(e.Address, strconv.Itoa(e.Port)))
	if err != nil {
		return fail(res, StageTCP, err)
	}
	conn.Close()
	res.TCP = time.Since(start)

	t, err := Open(e)
	if err != nil {
		return fail(res, StageHandshake, err)
	}
	defer t.Close()
	client := t.HTTPClient(c.StepTimeout)

	start = time.Now()
	if _, err := fetch(ctx, client, c.SmallURL); err != nil {
		return fail(res, StageHandshake, err)
	}
	res.Handshake = time.Since(start)

	start = time.Now()
	n, err := fetch(ctx, client, c.BulkURL)
	res.BulkBytes = n
	if err == nil && n < c.BulkMin {
		err = fmt.Errorf("got %d of %d bytes", n, c.BulkMin)
	}
	if err != nil {
		return fail(res, StageBulk, err)
	}
	res.Bulk = time.Since(start)
	res.OK = true

	if c.SpeedURL != "" {
		start = time.Now()
		long := t.HTTPClient(2 * time.Minute)
		if n, err := fetch(ctx, long, c.SpeedURL); err == nil {
			res.SpeedMbps = float64(n*8) / time.Since(start).Seconds() / 1e6
		}
	}
	return res
}

func fail(res Result, stage Stage, err error) Result {
	res.Failed = stage
	res.Error = shortError(err)
	return res
}

func shortError(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case strings.Contains(err.Error(), "connection refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "connection reset"):
		return "connection reset"
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func fetch(ctx context.Context, c *http.Client, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return n, err
	}
	if resp.StatusCode >= 400 {
		return n, fmt.Errorf("status %s", resp.Status)
	}
	return n, nil
}

// Tunnel is an in-process Xray client connected to one node.
type Tunnel struct {
	inst *core.Instance
}

func Open(e node.Endpoint) (*Tunnel, error) {
	raw, err := json.Marshal(map[string]any{
		"log": map[string]any{"loglevel": "none"},
		"outbounds": []any{map[string]any{
			"protocol": "vless",
			"settings": map[string]any{
				"address":    e.Address,
				"port":       e.Port,
				"id":         e.Keys.UUID,
				"encryption": "none",
			},
			"streamSettings": map[string]any{
				"network":  "raw",
				"security": "reality",
				"realitySettings": map[string]any{
					"serverName":  e.SNI,
					"fingerprint": node.Fingerprint,
					"publicKey":   e.Keys.PublicKey,
					"shortId":     e.Keys.ShortID,
				},
			},
		}},
	})
	if err != nil {
		return nil, err
	}
	jc, err := serial.DecodeJSONConfig(strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	pb, err := jc.Build()
	if err != nil {
		return nil, err
	}
	inst, err := core.New(pb)
	if err != nil {
		return nil, err
	}
	if err := inst.Start(); err != nil {
		inst.Close()
		return nil, fmt.Errorf("xray client: %w", err)
	}
	return &Tunnel{inst: inst}, nil
}

func (t *Tunnel) Close() error { return t.inst.Close() }

// HTTPClient sends every request on a fresh tunnel connection.
func (t *Tunnel) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dest, err := xnet.ParseDestination("tcp:" + addr)
				if err != nil {
					return nil, err
				}
				return core.Dial(ctx, t.inst, dest)
			},
		},
	}
}

// Node adapts a Checker to what a session needs.
type Node struct {
	Checker Checker
}

func (n Node) Check(ctx context.Context, e node.Endpoint, speed bool) Result {
	c := n.Checker
	if speed {
		c = c.WithSpeed()
	}
	return c.Check(ctx, e)
}

func (n Node) Reputation(ctx context.Context, e node.Endpoint) (Reputation, error) {
	t, err := Open(e)
	if err != nil {
		return Reputation{}, err
	}
	defer t.Close()
	return CheckReputation(ctx, t.HTTPClient(30*time.Second)), nil
}
