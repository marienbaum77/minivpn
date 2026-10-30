// Package app wires settings, the hoster, the journal, sing-box and the
// session controller together. The tray and the panel talk only to App.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marienbaum77/minivpn/internal/check"
	"github.com/marienbaum77/minivpn/internal/client"
	"github.com/marienbaum77/minivpn/internal/journal"
	"github.com/marienbaum77/minivpn/internal/node"
	"github.com/marienbaum77/minivpn/internal/serverspace"
	"github.com/marienbaum77/minivpn/internal/session"
	"github.com/marienbaum77/minivpn/internal/settings"
	"github.com/marienbaum77/minivpn/internal/tunnel"
)

// Platform is what differs on Windows: autostart and where Steam lives.
type Platform interface {
	SetAutostart(on bool) error
	SteamPath() string
}

type App struct {
	dir      string
	log      *slog.Logger
	journal  *journal.Journal
	tunnel   *tunnel.SingBox
	platform Platform

	mu          sync.Mutex
	set         settings.Settings
	ctl         *session.Controller
	err         string // why there is no controller
	startErr    string // why the last Start failed before the session began
	busy        bool   // Start is preparing (download, spec), before the controller takes over
	lastRefresh time.Time
	refreshOnce sync.Once
}

func Open(dir string, log *slog.Logger, p Platform) (*App, error) {
	set, err := settings.Load(filepath.Join(dir, "settings.json"))
	if err != nil {
		return nil, err
	}
	j, err := journal.Open(filepath.Join(dir, "journal.db"))
	if err != nil {
		return nil, err
	}
	return &App{dir: dir, log: log, journal: j, tunnel: &tunnel.SingBox{Dir: dir}, platform: p, set: set}, nil
}

// Init connects to the hoster, cleans up after a crash and loads the price.
// Errors end up in Status; the app stays usable for fixing settings.
func (a *App) Init(ctx context.Context) {
	ctl, err := a.controller(ctx)
	if err != nil {
		return
	}
	if err := ctl.Recover(ctx); err != nil {
		a.log.Warn("recover", "err", err)
	}
	if err := ctl.Refresh(ctx); err != nil {
		a.log.Warn("refresh", "err", err)
	} else {
		a.mu.Lock()
		a.lastRefresh = time.Now()
		a.mu.Unlock()
	}
	a.startPeriodicRefresh()
}

func (a *App) startPeriodicRefresh() {
	a.refreshOnce.Do(func() {
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for range t.C {
				a.mu.Lock()
				ctl := a.ctl
				a.mu.Unlock()
				if ctl != nil && ctl.Status().Phase == session.Off {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					if err := ctl.Refresh(ctx); err == nil {
						a.mu.Lock()
						a.lastRefresh = time.Now()
						a.mu.Unlock()
					}
					cancel()
				}
			}
		}()
	})
}

// controller builds the session controller on demand: it needs the API key
// and the server spec, which comes from the API.
func (a *App) controller(ctx context.Context) (*session.Controller, error) {
	a.mu.Lock()
	if a.ctl != nil {
		defer a.mu.Unlock()
		return a.ctl, nil
	}
	set := a.set
	a.mu.Unlock()

	ctl, err := a.build(ctx, set)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.err = err.Error()
		return nil, err
	}
	a.ctl, a.err = ctl, ""
	return ctl, nil
}

func (a *App) build(ctx context.Context, set settings.Settings) (*session.Controller, error) {
	key, err := set.Key()
	if err != nil {
		return nil, fmt.Errorf("API-ключ не расшифровывается, введи его заново: %w", err)
	}
	if key == "" {
		return nil, errors.New("Укажи API-ключ Serverspace в настройках")
	}
	hoster := serverspace.New(key)
	spec, err := hoster.MinimalSpec(ctx, set.Location)
	if err != nil {
		return nil, fmt.Errorf("Serverspace: %w", err)
	}
	cfg := session.Config{
		Spec:         spec,
		Keys:         set.Keys,
		Port:         443,
		SNI:          set.SNI,
		APIBase:      hoster.BaseURL,
		APIKey:       key,
		Watchdog:     set.Watchdog(),
		Rules:        a.rules(set),
		MonthlyLimit: set.MonthlyLimit,
		WarnAt:       set.WarnAt,
		Network:      lookupNetwork,
		Install:      installAlone,
		NextSpec: func(ctx context.Context, failed serverspace.Spec) (serverspace.Spec, error) {
			if set.Location == "" {
				// Another address pool: the failed address is likely to be
				// handed back by the same location.
				if next, err := hoster.FallbackSpec(ctx, failed.LocationID); err == nil {
					return next, nil
				} else {
					a.log.Warn("no fallback location", "err", err)
				}
			}
			return hoster.MinimalSpec(ctx, set.Location)
		},
	}
	session.DefaultTimings(&cfg)
	cfg.Length = time.Duration(set.SessionHours * float64(time.Hour))
	return session.New(cfg, hoster, a.tunnel, check.Node{Checker: check.Default()}, a.journal, a.log), nil
}

func (a *App) rules(set settings.Settings) client.Rules {
	r := client.DefaultRules()
	r.GameDirs = append(client.DetectGameDirs(a.platform.SteamPath()), set.GameDirs...)
	r.DirectProcesses = append(append([]string{}, client.DefaultDirectProcesses...), set.DirectExes...)
	return r
}

func (a *App) Status() session.Status {
	a.mu.Lock()
	ctl, msg, busy, limit, warn, startErr, lastRef := a.ctl, a.err, a.busy, a.set.MonthlyLimit, a.set.WarnAt, a.startErr, a.lastRefresh
	a.mu.Unlock()
	var st session.Status
	if ctl == nil {
		st = session.Status{Phase: session.Off, Error: msg, MonthlyLimit: limit, WarnAt: warn}
	} else {
		st = ctl.Status()
		if st.Phase == session.Off && time.Since(lastRef) > 2*time.Minute {
			a.mu.Lock()
			a.lastRefresh = time.Now()
			a.mu.Unlock()
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				ctl.Refresh(ctx)
			}()
		}
	}
	if st.Phase == session.Off && startErr != "" {
		st.Error = startErr
	}
	if busy && st.Phase == session.Off {
		st.Phase = session.Creating
	}
	return st
}

// Start prepares sing-box and starts a session. It returns once the session
// is underway; progress shows in Status.
func (a *App) Start(ctx context.Context, force bool) error {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return session.ErrRunning
	}
	a.busy, a.startErr = true, ""
	a.mu.Unlock()

	err := a.start(ctx, force)
	a.mu.Lock()
	a.busy = false
	if err != nil {
		a.startErr = err.Error() // the tray starts too; the panel must see why it failed
	}
	a.mu.Unlock()
	return err
}

// installAlone is node.Install that gives up once another VPN comes up
// midway: SSH would go through that VPN's server, which often drops port 22,
// and the install would hang until its timeout.
func installAlone(ctx context.Context, s node.SSH, script string, progress func(string)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			if other := tunnel.OtherVPNs(); len(other) > 0 {
				cancel(fmt.Errorf("включился другой VPN (%s), SSH к ноде пошёл через него: выключи его и нажми пуск", strings.Join(other, ", ")))
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	err := node.Install(ctx, s, script, progress)
	if cause := context.Cause(ctx); err != nil && cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return err
}

func (a *App) start(ctx context.Context, force bool) error {
	// Checked before a node exists, so a clash costs nothing.
	if other := tunnel.OtherVPNs(); len(other) > 0 {
		return fmt.Errorf("выключи другой VPN (%s): два VPN дерутся за маршруты, а проверки ноды пойдут через чужой сервер", strings.Join(other, ", "))
	}
	ctl, err := a.controller(ctx)
	if err != nil {
		return err
	}
	if err := a.tunnel.Ensure(ctx); err != nil {
		return fmt.Errorf("sing-box не скачался: %w", err)
	}
	return ctl.Start(ctx, force)
}

func (a *App) Stop(reason string) {
	a.mu.Lock()
	ctl := a.ctl
	a.mu.Unlock()
	if ctl != nil {
		ctl.Stop(reason)
	}
}

func (a *App) Extend() {
	a.mu.Lock()
	ctl := a.ctl
	a.mu.Unlock()
	if ctl != nil {
		ctl.Extend()
	}
}

func (a *App) Pause() error {
	a.mu.Lock()
	ctl := a.ctl
	a.mu.Unlock()
	if ctl == nil {
		return errors.New("сессия не запущена")
	}
	return ctl.Pause()
}

func (a *App) Resume() error {
	a.mu.Lock()
	ctl := a.ctl
	a.mu.Unlock()
	if ctl == nil {
		return errors.New("сессия не запущена")
	}
	return ctl.Resume()
}

// SettingsView is what the panel edits. The key itself never leaves the app.
type SettingsView struct {
	HasKey       bool     `json:"has_key"`
	APIKey       string   `json:"api_key,omitempty"` // write-only: set to change
	Location     string   `json:"location"`
	SNI          string   `json:"sni"`
	MonthlyLimit float64  `json:"monthly_limit"`
	WarnAt       float64  `json:"warn_at"`
	SessionHours float64  `json:"session_hours"`
	IdleMinutes  int      `json:"idle_minutes"`
	IdleMB       float64  `json:"idle_mb"`
	NodeMaxHours float64  `json:"node_max_hours"`
	GameDirs     []string `json:"game_dirs"`
	DirectExes   []string `json:"direct_exes"`
	Autostart    bool     `json:"autostart"`
	// Detected are the game folders found automatically, for display.
	Detected []string `json:"detected"`
}

func (a *App) Settings() SettingsView {
	a.mu.Lock()
	s := a.set
	a.mu.Unlock()
	return SettingsView{
		HasKey: s.APIKey != "", Location: s.Location, SNI: s.SNI,
		MonthlyLimit: s.MonthlyLimit, WarnAt: s.WarnAt, SessionHours: s.SessionHours,
		IdleMinutes: s.IdleMinutes, IdleMB: s.IdleMB, NodeMaxHours: s.NodeMaxHours,
		GameDirs: s.GameDirs, DirectExes: s.DirectExes, Autostart: s.Autostart,
		Detected: client.DetectGameDirs(a.platform.SteamPath()),
	}
}

var ErrBusy = errors.New("Останови сессию, чтобы поменять настройки")

func (a *App) SaveSettings(ctx context.Context, v SettingsView) error {
	if a.Status().Phase != session.Off {
		return ErrBusy
	}
	a.mu.Lock()
	s := a.set
	a.mu.Unlock()

	s.Location, s.SNI = v.Location, v.SNI
	s.MonthlyLimit, s.WarnAt, s.SessionHours = v.MonthlyLimit, v.WarnAt, v.SessionHours
	s.IdleMinutes, s.IdleMB, s.NodeMaxHours = v.IdleMinutes, v.IdleMB, v.NodeMaxHours
	s.GameDirs, s.DirectExes = clean(v.GameDirs), clean(v.DirectExes)
	if v.APIKey != "" {
		if err := s.SetKey(v.APIKey); err != nil {
			return err
		}
	}
	if s.APIKey == "" {
		return errors.New("Укажи API-ключ Serverspace")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if v.Autostart != s.Autostart {
		if err := a.platform.SetAutostart(v.Autostart); err != nil {
			return fmt.Errorf("автозапуск: %w", err)
		}
		s.Autostart = v.Autostart
	}
	if err := s.Save(filepath.Join(a.dir, "settings.json")); err != nil {
		return err
	}
	a.mu.Lock()
	a.set, a.ctl, a.err = s, nil, ""
	a.mu.Unlock()
	a.Init(ctx)
	return nil
}

func clean(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (a *App) Recent(n int) ([]journal.Session, error) { return a.journal.Recent(n) }

// Export writes the journal as CSV into Documents and returns the path.
func (a *App) Export() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Documents")
	if _, err := os.Stat(dir); err != nil {
		dir = home
	}
	path := filepath.Join(dir, "minivpn-journal-"+time.Now().Format("2006-01-02")+".csv")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if err := a.journal.ExportCSV(f); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

// Close ends a running session (the node is deleted) and closes the journal.
func (a *App) Close() {
	a.Stop(session.ReasonExit)
	a.journal.Close()
}

// lookupNetwork asks ip-api which ISP and city the home connection is in.
// The answer is all that is kept: the user's IP is not stored.
func lookupNetwork(ctx context.Context) (isp, city string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://ip-api.com/json/?fields=status,isp,city", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var v struct{ Status, ISP, City string }
	if json.NewDecoder(resp.Body).Decode(&v) != nil || v.Status != "success" {
		return "", ""
	}
	return v.ISP, v.City
}
