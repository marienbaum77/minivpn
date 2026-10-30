package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Keys.UUID == "" || s.MonthlyLimit != 200 {
		t.Fatalf("defaults: %+v", s)
	}
	if err := s.SetKey("secret-key"); err != nil {
		t.Fatal(err)
	}
	s.GameDirs = []string{`D:\Games`}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "secret-key") {
		t.Skip("key stored in plain text on this OS (DPAPI is Windows only)")
	}

	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := again.Key(); err != nil || k != "secret-key" {
		t.Fatalf("key round trip: %q %v", k, err)
	}
	if again.Keys != s.Keys || again.GameDirs[0] != `D:\Games` {
		t.Fatal("settings changed on reload")
	}
}

func TestValidate(t *testing.T) {
	s := Default()
	s.Keys.UUID = "x"
	s.NodeMaxHours = s.SessionHours
	if s.Validate() == nil {
		t.Fatal("node max age not longer than the session must be rejected")
	}
}

func TestBrokenSNIMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SNI = "www.microsoft.com"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if again.SNI != DefaultSNI || strings.Contains(string(raw), "microsoft") {
		t.Fatalf("old SNI kept: %q", again.SNI)
	}
}

func TestHysteriaKeysAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	uuid := s.Keys.UUID
	s.Keys.HY2Password, s.Keys.HY2Obfs, s.Keys.CertPEM, s.Keys.CertKeyPEM = "", "", "", ""
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// The Reality keys stay: the phone's saved link must keep working.
	if again.Keys.UUID != uuid || again.Keys.Validate() != nil {
		t.Fatalf("keys after migration: uuid %q, %v", again.Keys.UUID, again.Keys.Validate())
	}
}
