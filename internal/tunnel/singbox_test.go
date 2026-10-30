package tunnel

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func archive(t *testing.T, files map[string]string) ([]byte, string) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func TestInstall(t *testing.T) {
	data, sum := archive(t, map[string]string{
		"sing-box-1.14.4-windows-amd64/sing-box.exe":   "exe",
		"sing-box-1.14.4-windows-amd64/libcronet.dll":  "dll",
		"sing-box-1.14.4-windows-amd64/LICENSE":        "text",
		"sing-box-1.14.4-windows-amd64/../../evil.exe": "nope",
	})
	s := &SingBox{Dir: t.TempDir()}
	if err := s.install(data, "00"+sum[2:]); err == nil {
		t.Fatal("a wrong checksum must be rejected")
	}
	if err := s.install(data, sum); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"sing-box.exe", "libcronet.dll"} {
		if _, err := os.Stat(filepath.Join(s.Dir, "bin", f)); err != nil {
			t.Errorf("%s not extracted", f)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "bin", "LICENSE")); err == nil {
		t.Error("only binaries are extracted")
	}
	// filepath.Base keeps path traversal inside bin/.
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.Dir), "evil.exe")); err == nil {
		t.Error("archive paths must not escape the install dir")
	}
}
