package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marienbaum77/minivpn/internal/check"
	"github.com/marienbaum77/minivpn/internal/client"
	"github.com/marienbaum77/minivpn/internal/journal"
	"github.com/marienbaum77/minivpn/internal/node"
	"github.com/marienbaum77/minivpn/internal/serverspace"
)

type fakeHoster struct {
	mu       sync.Mutex
	servers  map[string]serverspace.Server
	next     int
	balance  float64
	deleted  []string
	password string // what the API reports for new servers
	powerOff bool   // create servers powered off: the app must turn them on
	specs    []serverspace.Spec
	powered  []string
}

func newHoster() *fakeHoster {
	return &fakeHoster{servers: map[string]serverspace.Server{}, balance: 500, password: "pw"}
}

func (h *fakeHoster) MonthlyPrice(context.Context, serverspace.Spec) (float64, error) {
	return 438.21, nil
}

func (h *fakeHoster) Create(_ context.Context, s serverspace.Spec) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := fmt.Sprintf("l1s%d", h.next)
	// The next free address is handed out only if the previous server is
	// still alive: the same pool reuses a freed address immediately.
	ip := "203.0.113.7"
	if len(h.servers) > 0 {
		ip = "203.0.113.8"
	}
	h.specs = append(h.specs, s)
	h.servers[id] = serverspace.Server{
		ID: id, Name: s.Name, State: "Active", PowerOn: !h.powerOff,
		NICs: []serverspace.NIC{{NetworkType: "PublicShared", IP: ip}}, Login: "root", Password: h.password,
	}
	return id, nil
}

func (h *fakeHoster) WaitTask(_ context.Context, id string, _ time.Duration) (serverspace.Task, error) {
	return serverspace.Task{ID: id, State: "Completed", ServerID: id}, nil
}

func (h *fakeHoster) Server(_ context.Context, id string) (serverspace.Server, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.servers[id]
	if !ok {
		return s, serverspace.ErrNotFound
	}
	return s, nil
}

func (h *fakeHoster) Servers(context.Context) ([]serverspace.Server, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []serverspace.Server
	for _, s := range h.servers {
		out = append(out, s)
	}
	return out, nil
}

func (h *fakeHoster) Delete(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.servers, id)
	h.deleted = append(h.deleted, id)
	return nil
}

func (h *fakeHoster) PowerOn(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.servers[id]
	if !ok {
		return serverspace.ErrNotFound
	}
	s.PowerOn = true
	h.servers[id] = s
	h.powered = append(h.powered, id)
	return nil
}

func (h *fakeHoster) Balance(context.Context) (serverspace.Balance, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.balance -= 0.1 // every read sees ten more minutes billed
	return serverspace.Balance{Amount: h.balance, Currency: "RUB"}, nil
}

func (h *fakeHoster) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.servers)
}

type fakeTunnel struct {
	mu      sync.Mutex
	running bool
	starts  int
	fail    bool
	done    chan struct{}
}

func (t *fakeTunnel) Start([]byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fail {
		return errors.New("wintun missing")
	}
	t.running = true
	t.starts++
	t.done = make(chan struct{})
	return nil
}

func (t *fakeTunnel) Stop() error {
	t.mu.Lock()
	t.running = false
	if t.done != nil {
		select {
		case <-t.done:
		default:
			close(t.done)
		}
	}
	t.mu.Unlock()
	return nil
}

func (t *fakeTunnel) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *fakeTunnel) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// fakeChecker fails the first failures calls, like a node still installing Xray.
type fakeChecker struct {
	mu        sync.Mutex
	failures  int
	calls     int
	blockedIP string
}

func (c *fakeChecker) Check(_ context.Context, e node.Endpoint, _ bool) check.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.blockedIP != "" && e.Address == c.blockedIP {
		return check.Result{At: time.Now(), Failed: check.StageTCP, Error: "i/o timeout"}
	}
	if c.calls <= c.failures {
		return check.Result{At: time.Now(), Failed: check.StageTCP, Error: "connection refused"}
	}
	return check.Result{At: time.Now(), OK: true, Handshake: 200 * time.Millisecond}
}

func (c *fakeChecker) Reputation(context.Context, node.Endpoint) (check.Reputation, error) {
	return check.Reputation{Country: "US", RedditOK: true, OpenAIOK: true}, nil
}

type env struct {
	ctl     *Controller
	hoster  *fakeHoster
	tunnel  *fakeTunnel
	checker *fakeChecker
	install *fakeInstall
	journal *journal.Journal
}

// fakeInstall stands in for node.Install: it reports one step, then fails
// with err, or hangs until the timeout when hang is set. errs fails one call
// at a time; noStep omits the step line, like SSH that never got through.
type fakeInstall struct {
	mu      sync.Mutex
	err     error
	errs    []error
	hang    bool
	noStep  bool
	logins  []node.SSH
	scripts []string
}

func (f *fakeInstall) Install(ctx context.Context, s node.SSH, script string, progress func(string)) error {
	f.mu.Lock()
	f.logins, f.scripts = append(f.logins, s), append(f.scripts, script)
	err, hang, noStep := f.err, f.hang, f.noStep
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	f.mu.Unlock()
	if !noStep {
		progress("step xray")
	}
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func setup(t *testing.T, mod func(*Config)) env {
	t.Helper()
	keys, err := node.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	cfg := Config{
		Spec: serverspace.Spec{LocationID: "nj3", ImageID: "ubuntu", CPU: 1, RAMMB: 1024, DiskMB: 10240, BandwidthMbps: 50},
		Keys: keys, Port: 443, SNI: "www.apple.com",
		APIBase: "https://api.serverspace.ru", APIKey: "k",
		Watchdog: node.DefaultWatchdog(), Rules: client.DefaultRules(),
		MonthlyLimit: 200, WarnAt: 150,
		Length: time.Hour, ExtendBy: time.Hour, AskBefore: 10 * time.Minute,
		ReadyTimeout: 2 * time.Second, InstallTimeout: time.Hour, BlockedAfter: time.Hour,
		CheckEvery: time.Hour, BalanceEvery: time.Hour,
		Tick: 20 * time.Millisecond, Poll: time.Millisecond,
	}
	inst := &fakeInstall{}
	cfg.Install = inst.Install
	if mod != nil {
		mod(&cfg)
	}
	e := env{hoster: newHoster(), tunnel: &fakeTunnel{}, checker: &fakeChecker{failures: 3}, install: inst, journal: j}
	e.ctl = New(cfg, e.hoster, e.tunnel, e.checker, j, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e
}

func waitPhase(t *testing.T, c *Controller, want Phase) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); st.Phase == want {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("phase stuck at %s, want %s (error: %s)", c.Status().Phase, want, c.Status().Error)
	return Status{}
}

func lastSession(t *testing.T, j *journal.Journal) journal.Session {
	t.Helper()
	s, err := j.Recent(1)
	if err != nil || len(s) == 0 {
		t.Fatalf("no session in journal: %v", err)
	}
	return s[0]
}

func TestStartStop(t *testing.T) {
	e := setup(t, nil)
	if err := e.ctl.Start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := e.ctl.Start(context.Background(), false); !errors.Is(err, ErrRunning) {
		t.Fatalf("second start: %v", err)
	}
	st := waitPhase(t, e.ctl, On)
	if st.NodeIP != "203.0.113.7" || !strings.HasPrefix(st.Link, "vless://") || !e.tunnel.Running() {
		t.Fatalf("on: %+v", st)
	}
	if want := 438.21 / 720; st.RatePerHour != want {
		t.Fatalf("rate %v, want %v", st.RatePerHour, want)
	}
	if !strings.Contains(e.install.scripts[0], `"name": "minivpn-`) {
		t.Fatal("the node must know its own name to delete itself")
	}
	if l := e.install.logins[0]; l.Addr != "203.0.113.7:22" || l.User != "root" || l.Password != "pw" {
		t.Fatalf("ssh login: %+v", l)
	}

	e.ctl.Stop(ReasonStop)
	st = e.ctl.Status()
	if st.Phase != Off || st.Error != "" || st.LastEnd != ReasonStop {
		t.Fatalf("after stop: %+v", st)
	}
	if e.hoster.count() != 0 || e.tunnel.Running() {
		t.Fatal("stop must delete the node and stop the tunnel")
	}
	s := lastSession(t, e.journal)
	if s.EndReason != ReasonStop || s.ServerID != "l1s1" || !s.Charged {
		t.Fatalf("journal: %+v", s)
	}
}

func TestMonthlyLimit(t *testing.T) {
	e := setup(t, func(c *Config) { c.MonthlyLimit = 1 })
	id, _ := e.journal.Start(journal.Session{StartedAt: time.Now(), RatePerHour: 0.6})
	e.journal.SetCost(id, 1.5, -1)
	e.journal.Finish(id, time.Now(), ReasonStop)

	if err := e.ctl.Start(context.Background(), false); !errors.Is(err, ErrLimit) {
		t.Fatalf("want ErrLimit, got %v", err)
	}
	if e.hoster.count() != 0 {
		t.Fatal("nothing may be created over the limit")
	}
	if err := e.ctl.Start(context.Background(), true); err != nil {
		t.Fatalf("forced start: %v", err)
	}
	waitPhase(t, e.ctl, On)
	e.ctl.Stop(ReasonStop)
}

func TestNodeGone(t *testing.T) {
	e := setup(t, nil)
	e.ctl.Start(context.Background(), false)
	waitPhase(t, e.ctl, On)
	e.hoster.Delete(context.Background(), "l1s1") // the watchdog deleted it
	st := waitPhase(t, e.ctl, Off)
	if st.LastEnd != ReasonNodeGone || e.tunnel.Running() {
		t.Fatalf("after node gone: %+v", st)
	}
}

func TestNotReady(t *testing.T) {
	e := setup(t, func(c *Config) { c.ReadyTimeout = 50 * time.Millisecond })
	e.checker.failures = 1 << 30
	e.ctl.Start(context.Background(), false)
	st := waitPhase(t, e.ctl, Off)
	if st.LastEnd != ReasonFailed || !strings.Contains(st.Error, "не заработала") {
		t.Fatalf("status: %+v", st)
	}
	if e.hoster.count() != 0 || e.tunnel.starts != 0 {
		t.Fatal("a node that never came up must be deleted and the tunnel never started")
	}
}

func TestTunnelFails(t *testing.T) {
	e := setup(t, nil)
	e.tunnel.fail = true
	e.ctl.Start(context.Background(), false)
	st := waitPhase(t, e.ctl, Off)
	if st.LastEnd != ReasonTunnel || e.hoster.count() != 0 {
		t.Fatalf("status: %+v, servers left: %d", st, e.hoster.count())
	}
}

func TestDeadlineAndExtend(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.Length, c.AskBefore, c.ExtendBy = 2*time.Second, 1500*time.Millisecond, 2*time.Second
	})
	e.ctl.Start(context.Background(), false)
	st := waitPhase(t, e.ctl, On)
	first := st.Deadline
	for !e.ctl.Status().AskExtend {
		time.Sleep(2 * time.Millisecond)
	}
	e.ctl.Extend()
	for e.ctl.Status().Deadline.Equal(first) {
		time.Sleep(2 * time.Millisecond)
	}
	if e.ctl.Status().AskExtend {
		t.Fatal("extending must clear the question")
	}
	st = waitPhase(t, e.ctl, Off)
	if st.LastEnd != ReasonLimit {
		t.Fatalf("end reason %q, want limit", st.LastEnd)
	}
}

func TestRecover(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	e.hoster.Create(ctx, serverspace.Spec{Name: "minivpn-20261028-2000-abcdef"})
	e.hoster.Create(ctx, serverspace.Spec{Name: "my-blog"})
	id, _ := e.journal.Start(journal.Session{StartedAt: time.Now(), RatePerHour: 0.6})
	e.journal.SetNode(id, "l1s1", "203.0.113.7")

	if err := e.ctl.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if e.hoster.count() != 1 {
		t.Fatal("only the minivpn leftover may be deleted")
	}
	if open, _ := e.journal.Unfinished(); len(open) != 0 {
		t.Fatal("the crashed session must be closed")
	}
}

func TestBlockedPath(t *testing.T) {
	e := setup(t, func(c *Config) { c.BlockedAfter = 30 * time.Millisecond })
	e.checker.failures = 1 << 30 // installed, but the PC never gets through
	e.ctl.Start(context.Background(), false)
	st := waitPhase(t, e.ctl, Off)
	if !strings.Contains(st.Error, "заблокирован") {
		t.Fatalf("want a blocked-IP explanation, got %q", st.Error)
	}
	if e.hoster.count() != 0 {
		t.Fatal("the node must be deleted")
	}
}

func TestSetupFailed(t *testing.T) {
	e := setup(t, nil)
	e.install.err = errors.New("установка упала на шаге download: curl: (6) Could not resolve host")
	e.ctl.Start(context.Background(), false)
	st := waitPhase(t, e.ctl, Off)
	if !strings.Contains(st.Error, "download") || e.hoster.count() != 0 {
		t.Fatalf("want the failed step in the error and the node deleted, got %q", st.Error)
	}
}

func TestSSHUnreachableTriesAnotherNode(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.NextSpec = func(context.Context, serverspace.Spec) (serverspace.Spec, error) {
			return serverspace.Spec{LocationID: "ca", ImageID: "ubuntu", CPU: 1, RAMMB: 1024, DiskMB: 10240, BandwidthMbps: 50}, nil
		}
	})
	e.install.noStep = true
	e.install.errs = []error{fmt.Errorf("%w: dial tcp 203.0.113.7:22: i/o timeout", node.ErrUnreachable)}
	if err := e.ctl.Start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	st := waitPhase(t, e.ctl, On)
	if st.NodeIP != "203.0.113.8" {
		t.Fatalf("node ip %q, want the replacement", st.NodeIP)
	}
	if len(e.hoster.specs) != 2 || e.hoster.specs[1].LocationID != "ca" {
		t.Fatalf("specs: %+v", e.hoster.specs)
	}
	if len(e.hoster.deleted) != 1 || e.hoster.deleted[0] != "l1s1" {
		t.Fatalf("the failed node must be deleted after the replacement exists: %v", e.hoster.deleted)
	}
	if e.hoster.count() != 1 {
		t.Fatalf("servers left: %d", e.hoster.count())
	}
	s := lastSession(t, e.journal)
	if s.ServerID != "l1s2" || s.Location != "ca" {
		t.Fatalf("journal: %+v", s)
	}
	e.ctl.Stop(ReasonStop)
}

func TestBlockedNodeTriesAnother(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.BlockedAfter = 20 * time.Millisecond
		c.NextSpec = func(context.Context, serverspace.Spec) (serverspace.Spec, error) {
			return serverspace.Spec{LocationID: "ca", ImageID: "ubuntu", CPU: 1, RAMMB: 1024, DiskMB: 10240, BandwidthMbps: 50}, nil
		}
	})
	e.checker.blockedIP = "203.0.113.7"
	e.checker.failures = 0
	e.ctl.Start(context.Background(), false)
	st := waitPhase(t, e.ctl, On)
	if st.NodeIP != "203.0.113.8" {
		t.Fatalf("node ip %q, want the replacement", st.NodeIP)
	}
	if len(e.hoster.deleted) != 1 || e.hoster.deleted[0] != "l1s1" {
		t.Fatalf("the blocked node must be deleted: %v", e.hoster.deleted)
	}
	e.ctl.Stop(ReasonStop)
}

func TestPowerOn(t *testing.T) {
	e := setup(t, nil)
	e.hoster.powerOff = true
	e.ctl.Start(context.Background(), false)
	st := waitPhase(t, e.ctl, On)
	if st.NodeIP != "203.0.113.7" {
		t.Fatalf("status: %+v", st)
	}
	if len(e.hoster.powered) != 1 || e.hoster.powered[0] != "l1s1" {
		t.Fatalf("power on calls: %v", e.hoster.powered)
	}
	e.ctl.Stop(ReasonStop)
}

func TestInstallTimeout(t *testing.T) {
	e := setup(t, func(c *Config) { c.InstallTimeout = 30 * time.Millisecond })
	e.install.hang = true
	e.ctl.Start(context.Background(), false)
	if st := waitPhase(t, e.ctl, Off); !strings.Contains(st.Error, "не уложилась") {
		t.Fatalf("got %q", st.Error)
	}
}

func TestNoPassword(t *testing.T) {
	e := setup(t, nil)
	e.hoster.password = ""
	e.ctl.Start(context.Background(), false)
	if st := waitPhase(t, e.ctl, Off); !strings.Contains(st.Error, "пароль") || len(e.install.logins) != 0 {
		t.Fatalf("got %q", st.Error)
	}
}

func TestPauseResume(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	if err := e.ctl.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e.ctl, On)

	// Pause
	if err := e.ctl.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	st := waitPhase(t, e.ctl, Paused)
	if st.Phase != Paused {
		t.Fatalf("phase %s, want paused", st.Phase)
	}
	if e.tunnel.Running() {
		t.Fatal("tunnel should not be running while paused")
	}
	if e.hoster.count() != 1 {
		t.Fatalf("node count %d, want 1 (node must stay alive during pause)", e.hoster.count())
	}

	// Pausing again must error
	if err := e.ctl.Pause(); err == nil {
		t.Fatal("second pause must return error")
	}

	// Resume
	if err := e.ctl.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st = waitPhase(t, e.ctl, On)
	if st.Phase != On {
		t.Fatalf("phase %s, want on", st.Phase)
	}
	if !e.tunnel.Running() {
		t.Fatal("tunnel should be running after resume")
	}

	// Resuming while on must error
	if err := e.ctl.Resume(); err == nil {
		t.Fatal("resume while on must return error")
	}

	// Stop cleanly
	e.ctl.Stop(ReasonStop)
	waitPhase(t, e.ctl, Off)
	if e.hoster.count() != 0 {
		t.Fatal("node must be deleted after stop")
	}
}
