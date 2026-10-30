// Command hoster-probe runs one node the way minivpn does, against the real
// Serverspace API with the key minivpn saved, and prints every step:
//
//	go run ./test/hoster-probe -list   servers left, which image a node gets (free)
//	go run ./test/hoster-probe         create, install over SSH, check
//	                                   through Reality, time new
//	                                   connections over Hysteria2 and
//	                                   Reality, delete
//	-image Ubuntu-24.04.4-X64          any image instead of minivpn's pick
//
// The server is deleted on every way out, Ctrl+C included: one 10-minute
// billing block, about 0.1 ₽.
//
// It is how server_init_script was found not to run (2026-10-29): a script
// that only powered the server off left it running on Ubuntu 24.04 and 26.04.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/marienbaum77/minivpn/internal/check"
	"github.com/marienbaum77/minivpn/internal/client"
	"github.com/marienbaum77/minivpn/internal/node"
	"github.com/marienbaum77/minivpn/internal/serverspace"
	"github.com/marienbaum77/minivpn/internal/settings"
)

func main() {
	list := flag.Bool("list", false, "show the chosen image and the others, create nothing")
	image := flag.String("image", "", "image id instead of the one minivpn picks")
	flag.Parse()
	if err := run(*list, *image); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(list bool, image string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dir, err := settings.Dir()
	if err != nil {
		return err
	}
	set, err := settings.Load(filepath.Join(dir, "settings.json"))
	if err != nil {
		return err
	}
	key, err := set.Key()
	if err != nil {
		return err
	}
	c := serverspace.New(key)

	spec, err := c.MinimalSpec(ctx, set.Location)
	if err != nil {
		return err
	}
	if image != "" {
		spec.ImageID = image
	}
	fmt.Printf("location %s, image %s, %d cpu, %d MB ram, %d MB disk, %d Mbit/s\n",
		spec.LocationID, spec.ImageID, spec.CPU, spec.RAMMB, spec.DiskMB, spec.BandwidthMbps)
	if bal, err := c.Balance(ctx); err == nil {
		fmt.Printf("balance: %.2f %s\n", bal.Amount, bal.Currency)
	}
	if list {
		servers, err := c.Servers(ctx)
		if err != nil {
			return err
		}
		for _, s := range servers {
			fmt.Printf("  server: %s %s %s\n", s.ID, s.Name, s.PublicIP())
		}
		images, err := c.Images(ctx, spec.LocationID)
		if err != nil {
			return err
		}
		for _, im := range images {
			fmt.Printf("  available: %s (%s %s %s)\n", im.ID, im.Type, im.OSVersion, im.Architecture)
		}
		locs, err := c.Locations(ctx)
		if err != nil {
			return err
		}
		for _, l := range locs {
			ls, err := c.MinimalSpec(ctx, l.ID)
			if err != nil {
				fmt.Printf("  location %s: %v\n", l.ID, err)
				continue
			}
			price, err := c.MonthlyPrice(ctx, ls)
			fmt.Printf("  location %s: %.3f ₽/h (%.0f ₽/mo), %d Mbit/s, creating=%v %v\n", l.ID, price/720, price, ls.BandwidthMbps, l.AllowCreating, err)
		}
		return nil
	}

	spec.Name = "minivpn-probe-" + time.Now().Format("150405")
	spec.Tags = []string{"minivpn"}
	script, err := node.Script(node.Params{
		Name: spec.Name, Keys: set.Keys, Port: 443, SNI: set.SNI,
		APIBase: c.BaseURL, APIKey: key, Watchdog: set.Watchdog(),
	})
	if err != nil {
		return err
	}
	start := time.Now()
	var out sync.Mutex
	say := func(format string, a ...any) {
		out.Lock()
		defer out.Unlock()
		fmt.Printf("%6s  %s\n", time.Since(start).Round(time.Second), fmt.Sprintf(format, a...))
	}

	taskID, err := c.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	task, err := c.WaitTask(ctx, taskID, 3*time.Second)
	if task.ServerID != "" {
		defer func() {
			dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := c.Delete(dctx, task.ServerID); err != nil {
				fmt.Fprintf(os.Stderr, "DELETE FAILED, remove %s by hand: %v\n", task.ServerID, err)
				return
			}
			say("deleted %s", task.ServerID)
		}()
	}
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	say("server %s created", task.ServerID)

	var srv serverspace.Server
	for srv.PublicIP() == "" {
		if srv, err = c.Server(ctx, task.ServerID); err != nil {
			return err
		}
		if srv.PublicIP() == "" {
			time.Sleep(3 * time.Second)
		}
	}
	say("ip %s, state %s, power %v, login %q, password set: %v", srv.PublicIP(), srv.State, srv.PowerOn, srv.Login, srv.Password != "")
	if srv.Password == "" {
		return errors.New("the API gave no password")
	}

	ictx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	// The hoster assigns an address before the machine is up; print how its
	// state and power actually change while SSH waits.
	go func() {
		last := ""
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			if s, serr := c.Server(ictx, task.ServerID); serr == nil {
				cur := fmt.Sprintf("state %s, power %v, ip %s", s.State, s.PowerOn, s.PublicIP())
				if cur != last {
					say("server: %s", cur)
					last = cur
				}
			}
			select {
			case <-ictx.Done():
				return
			case <-t.C:
			}
		}
	}()
	// While the PC tries SSH, check-host.net tries the same port from other
	// countries: open there and dead here means the path from Russia drops
	// this address; dead everywhere means the server never brought its
	// network up.
	go func() {
		for ictx.Err() == nil {
			say("from abroad, %s:22: %s", srv.PublicIP(), fromAbroad(ictx, srv.PublicIP()+":22"))
			select {
			case <-ictx.Done():
			case <-time.After(20 * time.Second):
			}
		}
	}()
	login := node.SSH{Addr: net.JoinHostPort(srv.PublicIP(), "22"), User: srv.Login, Password: srv.Password}
	err = node.Install(ictx, login, script, func(line string) { say("node: %s", line) })
	cancel()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	say("installed")

	ep := node.Endpoint{Address: srv.PublicIP(), Port: 443, SNI: set.SNI, Keys: set.Keys}
	for i := 0; i < 10; i++ {
		res := check.Default().Check(ctx, ep)
		if res.OK {
			say("check OK: handshake %s, %d bytes in %s", res.Handshake.Round(time.Millisecond), res.BulkBytes, res.Bulk.Round(time.Millisecond))
			return perProtocol(ctx, dir, ep, say)
		}
		say("check failed at %s: %s", res.Failed, res.Error)
		time.Sleep(3 * time.Second)
	}
	return errors.New("the node never passed the check")
}

// perProtocol runs the sing-box.exe minivpn downloaded with a local proxy per
// protocol and times fresh connections through each: a new connection is
// what a page load waits for. Hysteria2 failing here while Reality works
// means UDP to the node is cut on this network.
func perProtocol(ctx context.Context, dir string, ep node.Endpoint, say func(string, ...any)) error {
	bin := filepath.Join(dir, "bin", "sing-box.exe")
	ports := map[string]int{client.TagHysteria: 18091, client.TagReality: 18092}
	raw, err := client.PerProtocol(ep, ports)
	if err != nil {
		return err
	}
	cfg := filepath.Join(os.TempDir(), "minivpn-probe.json")
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		return err
	}
	defer os.Remove(cfg)
	cmd := exec.CommandContext(ctx, bin, "run", "-c", cfg)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w (start minivpn once so it downloads sing-box)", bin, err)
	}
	defer cmd.Process.Kill()
	time.Sleep(2 * time.Second)

	for _, tag := range []string{client.TagHysteria, client.TagReality} {
		proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", ports[tag]))
		hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}}
		var times []time.Duration
		var last error
		for i := 0; i < 6; i++ {
			t0 := time.Now()
			resp, err := hc.Get("https://www.gstatic.com/generate_204")
			if err != nil {
				last = err
				continue
			}
			resp.Body.Close()
			if i > 0 { // the first one also opens the tunnel itself
				times = append(times, time.Since(t0))
			}
		}
		if len(times) == 0 {
			say("%s: no connection: %v", tag, last)
			continue
		}
		slices.Sort(times)
		say("%s: new https connection median %s, worst %s (%d/5 ok)", tag,
			times[len(times)/2].Round(time.Millisecond), times[len(times)-1].Round(time.Millisecond), len(times))
	}
	return nil
}

// fromAbroad asks check-host.net to open a TCP connection to addr from a few
// countries and sums up what each saw.
func fromAbroad(ctx context.Context, addr string) string {
	get := func(url string, v any) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(v)
	}
	var started struct {
		RequestID string `json:"request_id"`
	}
	if err := get("https://check-host.net/check-tcp?max_nodes=4&host="+addr, &started); err != nil {
		return err.Error()
	}
	var res map[string][]map[string]any
	for i := 0; i < 10; i++ {
		time.Sleep(2 * time.Second)
		res = nil
		if err := get("https://check-host.net/check-result/"+started.RequestID, &res); err != nil {
			return err.Error()
		}
		done := true
		for _, r := range res {
			done = done && r != nil
		}
		if done {
			break
		}
	}
	var out []string
	for n, r := range res {
		country := strings.SplitN(n, ".", 2)[0]
		switch {
		case len(r) == 0 || r[0] == nil:
			out = append(out, country+" ?")
		case r[0]["error"] != nil:
			out = append(out, fmt.Sprintf("%s %v", country, r[0]["error"]))
		default:
			out = append(out, country+" open")
		}
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}
