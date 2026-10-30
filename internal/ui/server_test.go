package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marienbaum77/minivpn/internal/app"
	"github.com/marienbaum77/minivpn/internal/check"
	"github.com/marienbaum77/minivpn/internal/journal"
	"github.com/marienbaum77/minivpn/internal/session"
)

// fakeBackend walks through the phases on Start, like a real session.
type fakeBackend struct {
	mu      sync.Mutex
	st      session.Status
	limited bool
}

func newFake() *fakeBackend {
	return &fakeBackend{st: session.Status{Phase: session.Off, MonthSpent: 37.1, MonthlyLimit: 200, WarnAt: 150, RatePerHour: 0.609, Balance: 412.5}}
}

func (f *fakeBackend) Status() session.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	if st.Phase != session.Off {
		st.EstimatedRUB = time.Since(st.StartedAt).Hours() * st.RatePerHour
	}
	return st
}

func (f *fakeBackend) Start(_ context.Context, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limited && !force {
		return fmt.Errorf("%w: 201 из 200 ₽", session.ErrLimit)
	}
	f.st.Phase, f.st.StartedAt, f.st.Error = session.Creating, time.Now().Add(-83*time.Minute), ""
	f.st.Deadline = time.Now().Add(8 * time.Minute)
	go func() {
		steps := []struct {
			phase       session.Phase
			stage, step string
		}{
			{session.Installing, session.StageBoot, ""}, {session.Installing, session.StageSetup, "download"},
			{session.Installing, session.StageSetup, "sing-box"}, {session.Installing, session.StageReady, ""},
			{session.Connecting, "", ""}, {session.On, "", ""},
		}
		for _, s := range steps {
			time.Sleep(1500 * time.Millisecond)
			f.mu.Lock()
			p := s.phase
			f.st.Phase, f.st.NodeStage, f.st.NodeStep = p, s.stage, s.step
			if p == session.Installing {
				f.st.NodeIP, f.st.Link = "203.0.113.7", "vless://00000000-0000-0000-0000-000000000000@203.0.113.7:443?security=reality#minivpn"
			}
			if p == session.On {
				f.st.AskExtend = true
				f.st.LastCheck = &check.Result{OK: true, Handshake: 310 * time.Millisecond, SpeedMbps: 38.4}
				f.st.Reputation = &check.Reputation{Country: "US", OpenAIOK: true}
			}
			f.mu.Unlock()
		}
	}()
	return nil
}

func (f *fakeBackend) Stop(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st = session.Status{Phase: session.Off, MonthSpent: 38, MonthlyLimit: 200, WarnAt: 150, RatePerHour: 0.609, LastEnd: session.ReasonStop}
}

func (f *fakeBackend) Extend() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.AskExtend, f.st.Deadline = false, f.st.Deadline.Add(time.Hour)
}

func (f *fakeBackend) Pause() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Phase != session.On {
		return errors.New("not on")
	}
	f.st.Phase = session.Paused
	return nil
}

func (f *fakeBackend) Resume() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Phase != session.Paused {
		return errors.New("not paused")
	}
	f.st.Phase = session.On
	return nil
}

func (f *fakeBackend) Settings() app.SettingsView {
	return app.SettingsView{MonthlyLimit: 200, WarnAt: 150, SessionHours: 6, IdleMinutes: 20, IdleMB: 1, NodeMaxHours: 12, SNI: "www.apple.com", Autostart: true,
		Detected: []string{`C:\Program Files (x86)\Steam\steamapps\common`}}
}

func (f *fakeBackend) SaveSettings(_ context.Context, v app.SettingsView) error {
	if v.MonthlyLimit < 0 {
		return errors.New("лимит не может быть отрицательным")
	}
	return nil
}

func (f *fakeBackend) Recent(int) ([]journal.Session, error) {
	t := time.Date(2026, 10, 27, 20, 0, 0, 0, time.Local)
	return []journal.Session{{StartedAt: t, EndedAt: t.Add(95 * time.Minute), EstimatedRUB: 0.96, EndReason: session.ReasonNodeGone}}, nil
}

func (f *fakeBackend) Export() (string, error) {
	return `C:\Users\me\Documents\minivpn-journal.csv`, nil
}

func TestServer(t *testing.T) {
	b := newFake()
	srv, err := NewServer(b)
	if err != nil {
		t.Fatal(err)
	}
	srv.Log = NewLogTail(10)
	slog.New(srv.Log.Handler()).Info("node", "line", "step download")
	go srv.Serve()
	base := srv.URL()

	get := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	post := func(path, body string) (int, map[string]any) {
		resp, err := http.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]any
		json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v
	}

	if code, body := get(""); code != 200 || !strings.Contains(body, "<title>minivpn</title>") {
		t.Fatalf("index: %d", code)
	}
	if code, _ := get("app.js"); code != 200 {
		t.Fatal("app.js not served")
	}
	resp, _ := http.Get(strings.TrimSuffix(base, "/")[:strings.LastIndex(strings.TrimSuffix(base, "/"), "/")] + "/api/state")
	if resp.StatusCode != 404 {
		t.Fatal("the API must not answer without the token")
	}

	b.limited = true
	code, v := post("api/start", `{}`)
	if code != 400 || v["limit"] != true {
		t.Fatalf("limit: %d %v", code, v)
	}
	if code, _ := post("api/start", `{"force":true}`); code != 200 {
		t.Fatal("forced start failed")
	}
	if code, _ := get("api/qr.png"); code != 404 {
		t.Fatal("no QR before the node has an address")
	}
	time.Sleep(2 * time.Second)
	if code, body := get("api/qr.png"); code != 200 || !strings.HasPrefix(body, "\x89PNG") {
		t.Fatal("QR not served")
	}
	if code, v := post("api/settings", `{"monthly_limit":-1}`); code != 400 || v["error"] == nil {
		t.Fatal("settings errors must reach the panel")
	}
	if code, body := get("api/log?after=0"); code != 200 || !strings.Contains(body, "step download") || !strings.Contains(body, `"next":1`) {
		t.Fatalf("log: %d %s", code, body)
	}

	b.mu.Lock()
	b.st.Phase = session.On
	b.mu.Unlock()
	if code, _ := post("api/pause", `{}`); code != 200 {
		t.Fatal("pause failed")
	}
	if b.Status().Phase != session.Paused {
		t.Fatalf("expected paused phase, got %s", b.Status().Phase)
	}
	if code, _ := post("api/resume", `{}`); code != 200 {
		t.Fatal("resume failed")
	}
	if b.Status().Phase != session.On {
		t.Fatalf("expected on phase, got %s", b.Status().Phase)
	}
}

// TestServeDev keeps the panel up with the fake backend for manual review:
// MINIVPN_UI_DEV=1 go test -run TestServeDev -v ./internal/ui/
func TestServeDev(t *testing.T) {
	if os.Getenv("MINIVPN_UI_DEV") == "" {
		t.Skip("MINIVPN_UI_DEV not set")
	}
	srv, err := NewServer(newFake())
	if err != nil {
		t.Fatal(err)
	}
	srv.Static = os.DirFS("web") // edits show up on reload
	srv.Log = NewLogTail(300)
	go func() {
		log := slog.New(srv.Log.Handler())
		for i := 0; ; i++ {
			log.Info("node", "line", fmt.Sprintf("step %d", i), "after", fmt.Sprintf("%ds", i))
			if i%5 == 4 {
				log.Warn("session ended", "reason", "failed", "err", "установка упала на шаге download")
			}
			time.Sleep(2 * time.Second)
		}
	}()
	fmt.Println("PANEL", srv.URL())
	go srv.Serve()
	time.Sleep(15 * time.Minute)
}
