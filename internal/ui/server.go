// Package ui is the tray icon and the panel. The panel is a local web page
// served on 127.0.0.1 under a random token, shown in a WebView2 window.
package ui

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/marienbaum77/minivpn/internal/app"
	"github.com/marienbaum77/minivpn/internal/journal"
	"github.com/marienbaum77/minivpn/internal/session"
)

//go:embed web
var web embed.FS

// Backend is the part of app.App the panel uses.
type Backend interface {
	Status() session.Status
	Start(ctx context.Context, force bool) error
	Stop(reason string)
	Extend()
	Pause() error
	Resume() error
	Settings() app.SettingsView
	SaveSettings(ctx context.Context, v app.SettingsView) error
	Recent(n int) ([]journal.Session, error)
	Export() (string, error)
}

type Server struct {
	backend Backend
	token   string
	ln      net.Listener
	// Static serves the panel files; the embedded copy unless a dev run
	// points it at the source folder.
	Static fs.FS
	// OnPin is called when the panel's pin button toggles.
	OnPin func(bool)
	// Log feeds the panel's log sheet.
	Log *LogTail
}

func NewServer(b Backend) (*Server, error) {
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	static, _ := fs.Sub(web, "web")
	return &Server{backend: b, token: hex.EncodeToString(tok), ln: ln, Static: static}, nil
}

// URL is the panel's address. The token keeps other local pages and programs
// from driving the app: they cannot guess it.
func (s *Server) URL() string { return "http://" + s.ln.Addr().String() + "/" + s.token + "/" }

func (s *Server) Serve() error {
	mux := http.NewServeMux()
	p := "/" + s.token
	mux.Handle("GET "+p+"/", http.StripPrefix(p, http.FileServerFS(s.Static)))
	mux.HandleFunc("GET "+p+"/api/state", s.state)
	mux.HandleFunc("POST "+p+"/api/start", s.start)
	mux.HandleFunc("POST "+p+"/api/stop", func(w http.ResponseWriter, r *http.Request) {
		go s.backend.Stop(session.ReasonStop)
		reply(w, nil, nil)
	})
	mux.HandleFunc("POST "+p+"/api/extend", func(w http.ResponseWriter, r *http.Request) {
		s.backend.Extend()
		reply(w, nil, nil)
	})
	mux.HandleFunc("POST "+p+"/api/pause", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, s.backend.Pause())
	})
	mux.HandleFunc("POST "+p+"/api/resume", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, s.backend.Resume())
	})
	mux.HandleFunc("GET "+p+"/api/settings", func(w http.ResponseWriter, r *http.Request) {
		reply(w, s.backend.Settings(), nil)
	})
	mux.HandleFunc("POST "+p+"/api/settings", s.saveSettings)
	mux.HandleFunc("POST "+p+"/api/export", func(w http.ResponseWriter, r *http.Request) {
		path, err := s.backend.Export()
		reply(w, map[string]string{"path": path}, err)
	})
	mux.HandleFunc("POST "+p+"/api/pin", func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Pinned bool }
		json.NewDecoder(r.Body).Decode(&v)
		if s.OnPin != nil {
			s.OnPin(v.Pinned)
		}
		reply(w, nil, nil)
	})
	mux.HandleFunc("GET "+p+"/api/qr.png", s.qr)
	mux.HandleFunc("GET "+p+"/api/log", s.log)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return srv.Serve(s.ln)
}

type stateView struct {
	Status session.Status `json:"status"`
	Now    time.Time      `json:"now"`
	Recent []recentView   `json:"recent"`
}

type recentView struct {
	StartedAt time.Time `json:"started_at"`
	Minutes   int       `json:"minutes"`
	RUB       float64   `json:"rub"`
	Reason    string    `json:"reason"`
	ISP       string    `json:"isp"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	v := stateView{Status: s.backend.Status(), Now: time.Now()}
	recent, _ := s.backend.Recent(5)
	for _, x := range recent {
		if x.EndedAt.IsZero() {
			continue
		}
		v.Recent = append(v.Recent, recentView{
			StartedAt: x.StartedAt, Minutes: int(x.EndedAt.Sub(x.StartedAt).Minutes()),
			RUB: x.Cost(), Reason: x.EndReason, ISP: x.ISP,
		})
	}
	reply(w, v, nil)
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	v := struct {
		Lines []string `json:"lines"`
		Next  int      `json:"next"`
	}{Lines: []string{}}
	if s.Log != nil {
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		v.Lines, v.Next = s.Log.Since(after)
	}
	reply(w, v, nil)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var v struct{ Force bool }
	json.NewDecoder(r.Body).Decode(&v)
	// Start blocks while sing-box downloads on first run; the panel polls state.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := s.backend.Start(ctx, v.Force)
	reply(w, map[string]bool{"limit": errors.Is(err, session.ErrLimit)}, err)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var v app.SettingsView
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		reply(w, nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	reply(w, nil, s.backend.SaveSettings(ctx, v))
}

func (s *Server) qr(w http.ResponseWriter, r *http.Request) {
	link := s.backend.Status().Link
	if link == "" {
		http.NotFound(w, r)
		return
	}
	png, err := qrcode.Encode(link, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		body := map[string]any{"error": err.Error()}
		if m, ok := v.(map[string]bool); ok {
			for k, x := range m {
				body[k] = x
			}
		}
		json.NewEncoder(w).Encode(body)
		return
	}
	json.NewEncoder(w).Encode(v)
}
