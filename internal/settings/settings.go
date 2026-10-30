// Package settings keeps the app's configuration in one JSON file under
// %LOCALAPPDATA%\minivpn. The hoster API key is stored encrypted for the
// current Windows user (DPAPI).
package settings

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marienbaum77/minivpn/internal/node"
)

type Settings struct {
	// APIKey is DPAPI-encrypted and base64-encoded; use Key and SetKey.
	APIKey string `json:"api_key"`
	// Location is the Serverspace location id; empty tries New Jersey and
	// then Toronto, which cost the same.
	Location string `json:"location"`
	SNI      string `json:"sni"`

	MonthlyLimit float64 `json:"monthly_limit_rub"`
	WarnAt       float64 `json:"warn_at_rub"`
	SessionHours float64 `json:"session_hours"`
	IdleMinutes  int     `json:"idle_minutes"`
	// IdleMB per 5 minutes: below this the node counts as idle.
	IdleMB       float64 `json:"idle_mb_per_5min"`
	NodeMaxHours float64 `json:"node_max_hours"`

	// GameDirs and DirectExes are added to what is detected automatically.
	GameDirs   []string `json:"game_dirs"`
	DirectExes []string `json:"direct_exes"`
	Autostart  bool     `json:"autostart"`

	Keys node.Keys `json:"keys"`
}

// DefaultSNI is the site the node impersonates. Its certificate record must
// fit in 8 KB or Reality drops every connection after authenticating it:
// www.microsoft.com, the first default, sends 8273 bytes and never worked.
const DefaultSNI = "www.apple.com"

// brokenSNI were defaults once; saved settings still holding them are moved
// to the current default.
var brokenSNI = map[string]bool{"www.microsoft.com": true}

func Default() Settings {
	return Settings{
		SNI:          DefaultSNI,
		MonthlyLimit: 200,
		WarnAt:       150,
		SessionHours: 6,
		IdleMinutes:  20,
		IdleMB:       1,
		NodeMaxHours: 12,
		Autostart:    true,
	}
}

// Dir is where settings, the journal and sing-box live.
func Dir() (string, error) {
	base, err := os.UserCacheDir() // %LOCALAPPDATA% on Windows
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "minivpn"), nil
}

// Load reads the file, filling defaults. Keys are generated on first run and
// saved right away: losing them would break the phone's saved config.
func Load(path string) (Settings, error) {
	s := Default()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return s, err
	default:
		if err := json.Unmarshal(b, &s); err != nil {
			return s, fmt.Errorf("%s: %w", path, err)
		}
	}
	fix := brokenSNI[s.SNI]
	if fix {
		s.SNI = DefaultSNI
	}
	if s.Keys.UUID == "" {
		if s.Keys, err = node.NewKeys(); err != nil {
			return s, err
		}
		fix = true
	}
	// Settings from before Hysteria2 keep their Reality keys, so the phone's
	// saved vless link still works, and get the Hysteria2 ones added.
	if s.Keys.HY2Password == "" {
		if err := s.Keys.AddHysteria(); err != nil {
			return s, err
		}
		fix = true
	}
	if fix {
		if err := s.Save(path); err != nil {
			return s, err
		}
	}
	return s, s.Validate()
}

func (s Settings) Validate() error {
	switch {
	case s.SessionHours <= 0, s.IdleMinutes < 10, s.IdleMB <= 0:
		return errors.New("session_hours > 0, idle_minutes >= 10 and idle_mb_per_5min > 0 are required")
	case s.NodeMaxHours <= s.SessionHours:
		return errors.New("node_max_hours must be longer than session_hours, or the node dies before the app asks to extend")
	case s.SNI == "":
		return errors.New("sni is required")
	}
	return s.Keys.Validate()
}

func (s Settings) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s Settings) Key() (string, error) {
	if s.APIKey == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(s.APIKey)
	if err != nil {
		return "", err
	}
	plain, err := unprotect(raw)
	return string(plain), err
}

func (s *Settings) SetKey(key string) error {
	if key == "" {
		s.APIKey = ""
		return nil
	}
	enc, err := protect([]byte(key))
	if err != nil {
		return err
	}
	s.APIKey = base64.StdEncoding.EncodeToString(enc)
	return nil
}

func (s Settings) Watchdog() node.Watchdog {
	return node.Watchdog{
		Idle:      time.Duration(s.IdleMinutes) * time.Minute,
		Window:    5 * time.Minute,
		IdleBytes: int64(s.IdleMB * (1 << 20)),
		MaxAge:    time.Duration(s.NodeMaxHours * float64(time.Hour)),
	}
}
