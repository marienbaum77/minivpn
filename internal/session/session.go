// Package session runs one session at a time: create the node, wait until
// it really carries traffic, switch the PC's tunnel on, keep the books, and
// tear everything down for whichever reason comes first.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/marienbaum77/minivpn/internal/check"
	"github.com/marienbaum77/minivpn/internal/client"
	"github.com/marienbaum77/minivpn/internal/journal"
	"github.com/marienbaum77/minivpn/internal/node"
	"github.com/marienbaum77/minivpn/internal/serverspace"
)

// NamePrefix marks the hoster's servers that belong to minivpn. Anything with
// it that is not the current session's node is a leftover and gets deleted.
const NamePrefix = "minivpn-"

type Hoster interface {
	MonthlyPrice(ctx context.Context, s serverspace.Spec) (float64, error)
	Create(ctx context.Context, s serverspace.Spec) (string, error)
	WaitTask(ctx context.Context, id string, every time.Duration) (serverspace.Task, error)
	Server(ctx context.Context, id string) (serverspace.Server, error)
	Servers(ctx context.Context) ([]serverspace.Server, error)
	Delete(ctx context.Context, id string) error
	PowerOn(ctx context.Context, id string) error
	Balance(ctx context.Context) (serverspace.Balance, error)
}

// Tunnel is the PC side: sing-box with a TUN interface.
type Tunnel interface {
	Start(config []byte) error
	Stop() error
	Running() bool
	Done() <-chan struct{}
}

type Checker interface {
	Check(ctx context.Context, e node.Endpoint, speed bool) check.Result
	Reputation(ctx context.Context, e node.Endpoint) (check.Reputation, error)
}

// End reasons, as written to the journal.
const (
	ReasonStop     = "stop"      // the user pressed Stop
	ReasonExit     = "exit"      // the app was closed
	ReasonLimit    = "limit"     // session length reached
	ReasonNodeGone = "node-gone" // the node deleted itself (idle, max age) or was deleted by hand
	ReasonFailed   = "failed"    // the node never became usable
	ReasonTunnel   = "tunnel"    // sing-box died and would not restart
	ReasonRecover  = "recovered" // found unfinished after a crash
)

type Config struct {
	// Spec is the server to create, minus Name.
	Spec     serverspace.Spec
	Keys     node.Keys
	Port     int
	SNI      string
	APIBase  string
	APIKey   string
	Watchdog node.Watchdog
	Rules    client.Rules

	MonthlyLimit float64
	WarnAt       float64

	Length    time.Duration // ask, then stop after this
	ExtendBy  time.Duration
	AskBefore time.Duration

	ReadyTimeout time.Duration
	// InstallTimeout covers the boot, the SSH login and the setup script.
	// BlockedAfter: setup is done but checks from the PC keep failing for
	// this long, so the path is blocked.
	InstallTimeout time.Duration
	BlockedAfter   time.Duration
	CheckEvery     time.Duration
	BalanceEvery   time.Duration
	Tick           time.Duration // node existence, cost, deadline
	Poll           time.Duration // task and readiness polling

	// Network names the user's ISP and city for the journal. It runs before
	// the tunnel is up, so it sees the home connection. Optional.
	Network func(context.Context) (isp, city string)
	// Install runs the setup script on the node: node.Install.
	Install func(ctx context.Context, s node.SSH, script string, progress func(line string)) error
	// NextSpec returns the spec for a second node when the first never
	// answered SSH or could not be reached from the PC. The failed node is
	// still alive when this is called, so the hoster cannot hand its address
	// back. Nil means one node per session.
	NextSpec func(ctx context.Context, failed serverspace.Spec) (serverspace.Spec, error)
}

func DefaultTimings(c *Config) {
	c.Length, c.ExtendBy, c.AskBefore = 6*time.Hour, time.Hour, 10*time.Minute
	c.ReadyTimeout = 6 * time.Minute
	c.InstallTimeout, c.BlockedAfter = 4*time.Minute, 45*time.Second
	c.CheckEvery, c.BalanceEvery = 15*time.Minute, 2*time.Minute
	c.Tick, c.Poll = 30*time.Second, 3*time.Second
}

type Phase string

const (
	Off        Phase = "off"
	Creating   Phase = "creating"   // the hoster is building the server
	Installing Phase = "installing" // installing sing-box over SSH, then checking it
	Connecting Phase = "connecting" // starting sing-box
	On         Phase = "on"
	Paused     Phase = "paused" // PC tunnel stopped, node alive
	Stopping   Phase = "stopping"
)

// Status is everything the tray and the panel show.
type Status struct {
	Phase     Phase     `json:"phase"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Deadline  time.Time `json:"deadline"`
	AskExtend bool      `json:"ask_extend"`

	RatePerHour  float64 `json:"rate_per_hour"`
	EstimatedRUB float64 `json:"estimated_rub"`
	ChargedRUB   float64 `json:"charged_rub"`
	Charged      bool    `json:"charged"`
	MonthSpent   float64 `json:"month_spent"` // finished sessions this month
	MonthlyLimit float64 `json:"monthly_limit"`
	WarnAt       float64 `json:"warn_at"`
	Balance      float64 `json:"balance"`

	NodeIP     string            `json:"node_ip,omitempty"`
	NodeStage  string            `json:"node_stage,omitempty"` // while installing: boot, setup, ready
	NodeStep   string            `json:"node_step,omitempty"`  // the setup script's current step
	Link       string            `json:"link,omitempty"`
	LastCheck  *check.Result     `json:"last_check,omitempty"`
	Reputation *check.Reputation `json:"reputation,omitempty"`
	LastEnd    string            `json:"last_end,omitempty"`
}

var (
	ErrRunning = errors.New("сессия уже идёт")
	ErrLimit   = errors.New("месячный лимит исчерпан")
)

type Controller struct {
	cfg     Config
	hoster  Hoster
	tunnel  Tunnel
	checker Checker
	journal *journal.Journal
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	st      Status
	cancel  context.CancelCauseFunc
	done    chan struct{}
	extendC chan struct{}
	pauseC  chan struct{}
	resumeC chan struct{}
}

func New(cfg Config, h Hoster, t Tunnel, c Checker, j *journal.Journal, log *slog.Logger) *Controller {
	ctl := &Controller{cfg: cfg, hoster: h, tunnel: t, checker: c, journal: j, log: log, now: time.Now}
	ctl.st = Status{Phase: Off, MonthlyLimit: cfg.MonthlyLimit, WarnAt: cfg.WarnAt}
	return ctl
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	if st.Phase != Off && !st.StartedAt.IsZero() {
		st.EstimatedRUB = c.now().Sub(st.StartedAt).Hours() * st.RatePerHour
	}
	return st
}

func (c *Controller) update(f func(*Status)) {
	c.mu.Lock()
	f(&c.st)
	c.mu.Unlock()
}

// Refresh reloads the month total and the price for the panel while off.
func (c *Controller) Refresh(ctx context.Context) error {
	spent, err := c.journal.MonthSpent(c.now())
	if err != nil {
		return err
	}
	rate, err := c.rate(ctx)
	c.update(func(s *Status) {
		s.MonthSpent = spent
		if err == nil {
			s.RatePerHour = rate
		}
	})
	if b, berr := c.hoster.Balance(ctx); berr == nil {
		c.update(func(s *Status) { s.Balance = b.Amount })
	}
	return err
}

func (c *Controller) rate(ctx context.Context) (float64, error) {
	monthly, err := c.hoster.MonthlyPrice(ctx, c.cfg.Spec)
	if err != nil {
		return 0, fmt.Errorf("цена сервера: %w", err)
	}
	return monthly / 720, nil
}

// Start begins a session in the background. force skips the monthly limit.
func (c *Controller) Start(ctx context.Context, force bool) error {
	c.mu.Lock()
	if c.st.Phase != Off {
		c.mu.Unlock()
		return ErrRunning
	}
	c.mu.Unlock()

	spent, err := c.journal.MonthSpent(c.now())
	if err != nil {
		return err
	}
	if !force && c.cfg.MonthlyLimit > 0 && spent >= c.cfg.MonthlyLimit {
		return fmt.Errorf("%w: %.0f из %.0f ₽", ErrLimit, spent, c.cfg.MonthlyLimit)
	}
	rate, err := c.rate(ctx)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancelCause(context.Background())
	c.mu.Lock()
	if c.st.Phase != Off {
		c.mu.Unlock()
		cancel(nil)
		return ErrRunning
	}
	now := c.now()
	c.st = Status{
		Phase: Creating, StartedAt: now, Deadline: now.Add(c.cfg.Length),
		RatePerHour: rate, MonthSpent: spent, MonthlyLimit: c.cfg.MonthlyLimit, WarnAt: c.cfg.WarnAt,
		Balance: c.st.Balance,
	}
	c.cancel = cancel
	c.done = make(chan struct{})
	c.extendC = make(chan struct{}, 1)
	c.pauseC = make(chan struct{}, 1)
	c.resumeC = make(chan struct{}, 1)
	done, extendC, pauseC, resumeC := c.done, c.extendC, c.pauseC, c.resumeC
	c.mu.Unlock()

	go func() {
		defer close(done)
		c.run(runCtx, extendC, pauseC, resumeC)
	}()
	return nil
}

// Stop ends the running session and waits until the node is deleted.
func (c *Controller) Stop(reason string) {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel(errors.New(reason))
	<-done
}

// Pause pauses the PC tunnel while keeping the server node alive.
func (c *Controller) Pause() error {
	c.mu.Lock()
	phase := c.st.Phase
	ch := c.pauseC
	c.mu.Unlock()
	if phase != On {
		return errors.New("пауза возможна только при активном VPN")
	}
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

// Resume restarts the PC tunnel to resume routing traffic through the node.
func (c *Controller) Resume() error {
	c.mu.Lock()
	phase := c.st.Phase
	ch := c.resumeC
	c.mu.Unlock()
	if phase != Paused {
		return errors.New("VPN не на паузе")
	}
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

// Extend pushes the deadline by ExtendBy; the panel calls it on "Продлить".
func (c *Controller) Extend() {
	c.mu.Lock()
	ch := c.extendC
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type run struct {
	id       int64
	serverID string
	// extra are replaced nodes whose deletion failed; tearDown retries them.
	extra    []string
	endpoint node.Endpoint
	balance0 float64
	haveBal  bool
}

func (c *Controller) run(ctx context.Context, extendC, pauseC, resumeC <-chan struct{}) {
	r := &run{}
	reason, err := c.bringUp(ctx, r)
	if err == nil {
		reason = c.watch(ctx, r, extendC, pauseC, resumeC)
	} else if reason == "" {
		reason = ReasonFailed
	}
	if reason == ReasonStop || reason == ReasonExit {
		err = nil // stopped on purpose while starting: not an error to show
	}
	c.tearDown(r, reason, err)
}

// maxNodes is how many servers one session may create. A second is worth it:
// Serverspace hands a freed address straight to the next server, so a node
// that never answered is likely to be followed by another on the same
// filtered address. A different location breaks that loop.
const maxNodes = 2

func (c *Controller) bringUp(ctx context.Context, r *run) (string, error) {
	st := c.Status()
	var isp, city string
	if c.cfg.Network != nil {
		isp, city = c.cfg.Network(ctx)
	}
	id, err := c.journal.Start(journal.Session{StartedAt: st.StartedAt, Location: c.cfg.Spec.LocationID, RatePerHour: st.RatePerHour, ISP: isp, City: city})
	if err != nil {
		return "", err
	}
	r.id = id
	if b, err := c.hoster.Balance(ctx); err == nil {
		r.balance0, r.haveBal = b.Amount, true
	}
	defer c.deleteExtra(r)

	spec := c.cfg.Spec
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			next, err := c.cfg.NextSpec(ctx, spec)
			if err != nil {
				return "", fmt.Errorf("вторая нода: %w", err)
			}
			spec = next
			c.log.Warn("trying another node", "attempt", attempt, "location", spec.LocationID)
			c.update(func(s *Status) {
				s.NodeStage, s.NodeStep = StageBoot, "SSH не отвечает, пробуем другую ноду"
				s.NodeIP, s.Link = "", ""
			})
		}
		spec.Name = newName(c.now())
		spec.Tags = []string{"minivpn"}
		script, err := node.Script(node.Params{
			Name: spec.Name, Keys: c.cfg.Keys, Port: c.cfg.Port, SNI: c.cfg.SNI,
			APIBase: c.cfg.APIBase, APIKey: c.cfg.APIKey, Watchdog: c.cfg.Watchdog,
		})
		if err != nil {
			return "", err
		}

		c.log.Info("creating server", "name", spec.Name, "location", spec.LocationID, "attempt", attempt)
		waitCtx, cancel := context.WithTimeout(ctx, c.cfg.ReadyTimeout)
		taskID, err := c.hoster.Create(ctx, spec)
		if err != nil {
			cancel()
			return "", fmt.Errorf("создание сервера: %w", err)
		}
		task, err := c.hoster.WaitTask(waitCtx, taskID, c.cfg.Poll)
		prev := r.serverID
		if task.ServerID != "" {
			r.serverID = task.ServerID
		}
		if err != nil {
			cancel()
			return stopReason(ctx), fmt.Errorf("создание сервера: %w", err)
		}
		if r.serverID == "" {
			cancel()
			return "", errors.New("создание сервера: задача завершилась без id сервера")
		}
		if prev != "" && prev != r.serverID {
			// The replacement already exists, so the hoster cannot hand the
			// failed node's address to it anymore.
			c.dropServer(r, prev)
		}

		srv, ip, err := c.waitServer(waitCtx, r.serverID)
		if err != nil {
			cancel()
			return stopReason(ctx), err
		}
		c.log.Info("server up", "id", r.serverID, "ip", ip, "attempt", attempt)
		r.endpoint = node.Endpoint{Address: ip, Port: c.cfg.Port, SNI: c.cfg.SNI, Keys: c.cfg.Keys}
		c.journal.SetNode(r.id, r.serverID, ip)
		if spec.LocationID != c.cfg.Spec.LocationID {
			c.journal.SetLocation(r.id, spec.LocationID)
		}
		c.update(func(s *Status) { s.Phase, s.NodeIP, s.Link = Installing, ip, r.endpoint.Link("minivpn") })

		if srv.Login == "" || srv.Password == "" {
			cancel()
			return "", errors.New("API хостера не отдал логин и пароль сервера: установить ноду нечем")
		}
		login := node.SSH{Addr: net.JoinHostPort(ip, "22"), User: srv.Login, Password: srv.Password}
		installErr := c.waitReady(ctx, waitCtx, r, login, script)
		cancel()
		if installErr != nil {
			var retry retryableError
			if attempt < maxNodes && c.cfg.NextSpec != nil && errors.As(installErr, &retry) && ctx.Err() == nil {
				c.log.Warn("node is unusable, trying another", "server", r.serverID, "err", installErr)
				continue
			}
			return stopReason(ctx), installErr
		}

		c.update(func(s *Status) { s.Phase = Connecting })
		cfg, err := client.SingBox(r.endpoint, c.cfg.Rules)
		if err != nil {
			return "", err
		}
		var tunnelErr error
		for attempt := 0; attempt < 3; attempt++ {
			tunnelErr = c.tunnel.Start(cfg)
			if tunnelErr == nil {
				time.Sleep(1 * time.Second)
				if c.tunnel.Running() {
					break
				}
				tunnelErr = errors.New("sing-box closed immediately after start")
			}
			c.log.Warn("sing-box start attempt failed, retrying", "attempt", attempt+1, "err", tunnelErr)
			time.Sleep(1 * time.Second)
		}
		if !c.tunnel.Running() {
			return ReasonTunnel, fmt.Errorf("sing-box: %w", tunnelErr)
		}
		c.log.Info("tunnel up")
		c.update(func(s *Status) { s.Phase = On })

		go func() {
			res := c.checker.Check(ctx, r.endpoint, true)
			c.journal.AddCheck(r.id, res)
			c.update(func(s *Status) { s.LastCheck = &res })
			if rep, err := c.checker.Reputation(ctx, r.endpoint); err == nil {
				b, _ := json.Marshal(rep)
				c.journal.SetReputation(r.id, string(b))
				c.update(func(s *Status) { s.Reputation = &rep })
			}
		}()
		return "", nil
	}
}

// Node setup stages, shown while installing.
const (
	StageBoot  = "boot"  // the server is booting: no SSH login yet
	StageSetup = "setup" // the setup script is installing sing-box
	StageReady = "ready" // sing-box runs; checking it from the PC
)

// waitReady installs sing-box over SSH, then checks the node from the PC until
// real traffic gets through. The script's log lines come back as they are
// written, so the panel shows the step and a failure names it.
func (c *Controller) waitReady(ctx, waitCtx context.Context, r *run, login node.SSH, script string) error {
	start := c.now()
	after := func() time.Duration { return c.now().Sub(start).Round(time.Second) }
	stage := func(st, step string) {
		c.update(func(s *Status) { s.NodeStage, s.NodeStep = st, step })
	}

	stage(StageBoot, "")
	ictx, cancel := context.WithTimeout(waitCtx, c.cfg.InstallTimeout)
	err := c.cfg.Install(ictx, login, script, func(line string) {
		c.log.Info("node", "line", line, "after", after())
		if step, ok := strings.CutPrefix(line, "step "); ok {
			stage(StageSetup, step)
		} else if strings.HasPrefix(line, "ssh:") {
			stage(StageBoot, line)
		}
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		unreachable := errors.Is(err, node.ErrUnreachable)
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("установка не уложилась в %s", c.cfg.InstallTimeout)
		}
		if unreachable {
			err = retryableError{err}
		}
		return err
	}

	stage(StageReady, "")
	readyAt := c.now()
	for {
		last := c.checker.Check(waitCtx, r.endpoint, false)
		if last.OK {
			c.log.Info("node ready", "after", after())
			return nil
		}
		c.log.Info("node not reachable yet", "failed", last.Failed, "err", last.Error, "after", after())
		if c.now().Sub(readyAt) > c.cfg.BlockedAfter {
			c.journal.AddCheck(r.id, last)
			return retryableError{blockedError(last)}
		}
		if err := sleep(waitCtx, c.cfg.Poll); err != nil {
			c.journal.AddCheck(r.id, last)
			if ctx.Err() != nil {
				return err
			}
			return retryableError{fmt.Errorf("нода не заработала за %s (стадия %s): %s", c.cfg.ReadyTimeout, last.Failed, last.Error)}
		}
	}
}

// retryableError marks a node failure another node may not share: SSH never
// got through, or the address is filtered from here. A setup script error is
// not wrapped: it would fail the same way on the next node.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

// blockedError explains a node that runs but cannot be reached from the PC.
func blockedError(r check.Result) error {
	switch r.Failed {
	case check.StageTCP:
		return errors.New("нода работает, но TCP до неё из твоей сети не доходит: похоже, IP заблокирован. Попробуй другую локацию в настройках")
	case check.StageHandshake:
		return errors.New("нода работает, но Reality не проходит: фильтр протокола, или сайт из SNI не годится для Reality. Смени SNI или локацию в настройках")
	default:
		return errors.New("нода работает, но загрузка замирает: похоже на ТСПУ. Попробуй другую локацию в настройках")
	}
}

// waitServer waits until the hoster has given the server an address and the
// machine is really on. A server can be Active with its power off, and SSH
// never answers then.
func (c *Controller) waitServer(ctx context.Context, id string) (serverspace.Server, string, error) {
	var srv serverspace.Server
	powerAsked := false
	for {
		var err error
		if srv, err = c.hoster.Server(ctx, id); err == nil {
			ip := srv.PublicIP()
			switch {
			case srv.State == "Blocked":
				return srv, "", fmt.Errorf("хостер заблокировал ноду %s", id)
			case ip != "" && srv.State == "Active" && srv.PowerOn:
				return srv, ip, nil
			case ip != "" && srv.State == "Active" && !srv.PowerOn && !powerAsked:
				powerAsked = true
				c.log.Warn("server is active but powered off, turning it on", "id", id)
				if err := c.hoster.PowerOn(ctx, id); err != nil {
					c.log.Warn("power on failed", "id", id, "err", err)
				}
			}
		} else if errors.Is(err, serverspace.ErrNotFound) {
			return srv, "", errors.New("нода исчезла до установки")
		}
		if err := sleep(ctx, c.cfg.Poll); err != nil {
			return srv, "", fmt.Errorf("нода не запустилась за %s (состояние %q, питание %v)", c.cfg.ReadyTimeout, srv.State, srv.PowerOn)
		}
	}
}

// deleteServer deletes one node with the same retries tearDown uses, keeping
// the last error.
func (c *Controller) deleteServer(ctx context.Context, id string) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = c.hoster.Delete(ctx, id); err == nil {
			return nil
		}
		if attempt < 3 {
			sleep(ctx, time.Duration(attempt+1)*c.cfg.Poll)
		}
	}
	return err
}

// dropServer deletes the failed node once its replacement exists. If the
// delete fails, tearDown retries it from r.extra.
func (c *Controller) dropServer(r *run, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.deleteServer(ctx, id); err != nil {
		c.log.Warn("delete replaced node", "id", id, "err", err)
		r.extra = append(r.extra, id)
		return
	}
	c.log.Info("deleted replaced node", "id", id)
}

// deleteExtra retries the replaced nodes that dropServer could not delete.
func (c *Controller) deleteExtra(r *run) {
	var left []string
	for _, id := range r.extra {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := c.deleteServer(ctx, id)
		cancel()
		if err != nil {
			c.log.Warn("delete replaced node", "id", id, "err", err)
			left = append(left, id)
			continue
		}
		c.log.Info("deleted replaced node", "id", id)
	}
	r.extra = left
}

func (c *Controller) watch(ctx context.Context, r *run, extendC, pauseC, resumeC <-chan struct{}) string {
	tick := time.NewTicker(c.cfg.Tick)
	defer tick.Stop()
	lastCheck, lastBalance, lastKeepalive := c.now(), c.now(), c.now()
	for {
		var tunnelDone <-chan struct{}
		if c.tunnel != nil {
			tunnelDone = c.tunnel.Done()
		}

		select {
		case <-ctx.Done():
			return stopReason(ctx)
		case <-extendC:
			c.update(func(s *Status) { s.Deadline, s.AskExtend = s.Deadline.Add(c.cfg.ExtendBy), false })
			continue
		case <-pauseC:
			if c.Status().Phase == On {
				c.log.Info("pausing tunnel")
				c.update(func(s *Status) { s.Phase = Paused })
				if err := c.tunnel.Stop(); err != nil {
					c.log.Warn("pause tunnel stop error", "err", err)
				}
				lastKeepalive = c.now()
				go c.checker.Check(ctx, r.endpoint, true)
			}
			continue
		case <-resumeC:
			if c.Status().Phase == Paused {
				c.log.Info("resuming tunnel")
				c.update(func(s *Status) { s.Phase = Connecting; s.Error = "" })
				cfg, err := client.SingBox(r.endpoint, c.cfg.Rules)
				if err == nil {
					err = c.tunnel.Start(cfg)
				}
				if err != nil {
					c.log.Warn("resume tunnel failed", "err", err)
					c.update(func(s *Status) { s.Phase = Paused; s.Error = "sing-box: " + err.Error() })
				} else {
					c.update(func(s *Status) { s.Phase = On; s.Error = "" })
				}
			}
			continue
		case <-tunnelDone:
			if c.Status().Phase == Paused || c.Status().Phase == Stopping {
				continue
			}
			c.log.Warn("sing-box exited, restarting")
			cfg, _ := client.SingBox(r.endpoint, c.cfg.Rules)
			if err := c.tunnel.Start(cfg); err != nil {
				c.update(func(s *Status) { s.Error = "sing-box: " + err.Error() })
				return ReasonTunnel
			}
			continue
		case <-tick.C:
		}
		now := c.now()
		st := c.Status()
		if !now.Before(st.Deadline) {
			return ReasonLimit
		}
		if !st.AskExtend && !now.Before(st.Deadline.Add(-c.cfg.AskBefore)) {
			c.update(func(s *Status) { s.AskExtend = true })
		}
		if _, err := c.hoster.Server(ctx, r.serverID); errors.Is(err, serverspace.ErrNotFound) {
			return ReasonNodeGone
		}
		if st.Phase != Paused && !c.tunnel.Running() {
			c.log.Warn("sing-box exited, restarting")
			cfg, _ := client.SingBox(r.endpoint, c.cfg.Rules)
			if err := c.tunnel.Start(cfg); err != nil {
				c.update(func(s *Status) { s.Error = "sing-box: " + err.Error() })
				return ReasonTunnel
			}
		}
		if st.Phase == Paused {
			if now.Sub(lastKeepalive) >= 3*time.Minute {
				lastKeepalive = now
				go c.checker.Check(ctx, r.endpoint, true)
			}
		}
		if now.Sub(lastCheck) >= c.cfg.CheckEvery {
			lastCheck = now
			res := c.checker.Check(ctx, r.endpoint, false)
			c.journal.AddCheck(r.id, res)
			c.update(func(s *Status) { s.LastCheck = &res })
		}
		if now.Sub(lastBalance) >= c.cfg.BalanceEvery {
			lastBalance = now
			c.readBalance(ctx, r)
		}
		c.journal.SetCost(r.id, c.Status().EstimatedRUB, -1)
	}
}

func (c *Controller) readBalance(ctx context.Context, r *run) {
	b, err := c.hoster.Balance(ctx)
	if err != nil {
		return
	}
	c.update(func(s *Status) {
		s.Balance = b.Amount
		if r.haveBal {
			s.ChargedRUB, s.Charged = r.balance0-b.Amount, true
		}
	})
}

func (c *Controller) tearDown(r *run, reason string, cause error) {
	c.update(func(s *Status) { s.Phase, s.AskExtend = Stopping, false })
	if err := c.tunnel.Stop(); err != nil {
		c.log.Warn("sing-box stop", "err", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var delErr error
	if r.serverID != "" {
		delErr = c.deleteServer(ctx, r.serverID)
	}
	for _, id := range r.extra {
		if err := c.deleteServer(ctx, id); err != nil {
			delErr = errors.Join(delErr, fmt.Errorf("нода %s: %w", id, err))
		}
	}
	if r.serverID == "" && r.id != 0 {
		// Create may have succeeded on the hoster's side even if we never got
		// the task back; the name is the only handle.
		c.deleteLeftovers(ctx, "")
	}

	end := c.now()
	if r.id != 0 {
		c.readBalance(ctx, r)
		st := c.Status()
		st.EstimatedRUB = end.Sub(st.StartedAt).Hours() * st.RatePerHour
		charged := -1.0
		if st.Charged {
			charged = st.ChargedRUB
		}
		c.journal.SetCost(r.id, st.EstimatedRUB, charged)
		c.journal.Finish(r.id, end, reason)

		// Follow up after 5s as hosters take a moment to settle balance after server deletion.
		go func() {
			time.Sleep(5 * time.Second)
			c.readBalance(context.Background(), r)
		}()
	}
	spent, _ := c.journal.MonthSpent(end)

	msg := ""
	switch {
	case delErr != nil:
		msg = fmt.Sprintf("Не удалось удалить ноду (%v). Удали её в панели Serverspace, иначе она продолжит списывать деньги.", delErr)
	case cause != nil:
		msg = cause.Error()
	}
	level := slog.LevelInfo
	if msg != "" {
		level = slog.LevelWarn
	}
	c.log.Log(context.Background(), level, "session ended", "reason", reason, "server", r.serverID, "err", msg)
	c.mu.Lock()
	c.st = Status{
		Phase: Off, Error: msg, LastEnd: reason, MonthSpent: spent,
		MonthlyLimit: c.cfg.MonthlyLimit, WarnAt: c.cfg.WarnAt,
		RatePerHour: c.st.RatePerHour, Balance: c.st.Balance,
		LastCheck: c.st.LastCheck, Reputation: c.st.Reputation,
	}
	c.cancel, c.extendC, c.pauseC, c.resumeC = nil, nil, nil, nil
	c.mu.Unlock()
}

// Recover runs at app start: it closes sessions a crash left open and deletes
// every minivpn server that is still around. Nothing is running yet, so all
// of them are leftovers that bill for nothing.
func (c *Controller) Recover(ctx context.Context) error {
	open, err := c.journal.Unfinished()
	if err != nil {
		return err
	}
	if err := c.deleteLeftovers(ctx, ""); err != nil {
		return err
	}
	for _, s := range open {
		c.journal.Finish(s.ID, c.now(), ReasonRecover)
	}
	return nil
}

func (c *Controller) deleteLeftovers(ctx context.Context, keep string) error {
	servers, err := c.hoster.Servers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range servers {
		if strings.HasPrefix(s.Name, NamePrefix) && s.ID != keep {
			c.log.Info("deleting leftover node", "id", s.ID, "name", s.Name)
			errs = append(errs, c.hoster.Delete(ctx, s.ID))
		}
	}
	return errors.Join(errs...)
}

func stopReason(ctx context.Context) string {
	if cause := context.Cause(ctx); cause != nil && ctx.Err() != nil {
		return cause.Error()
	}
	return ""
}

func newName(now time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return NamePrefix + now.UTC().Format("20060102-1504") + "-" + hex.EncodeToString(b)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
