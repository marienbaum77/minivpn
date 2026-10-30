// Package tunnel runs sing-box as a child process: the TUN interface that
// sends the PC's traffic to the node.
package tunnel

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// sing-box release the app downloads on first run. Bump all three together.
const (
	Version     = "1.14.4"
	zipURL      = "https://github.com/SagerNet/sing-box/releases/download/v" + Version + "/sing-box-" + Version + "-windows-amd64.zip"
	zipSHA256   = "99185d75d314a07dbba4a130c8ad986cbeb629a07927975636b341ca61f991df"
	startupWait = 3500 * time.Millisecond
)

type SingBox struct {
	// Dir holds sing-box.exe, its config, cache and log.
	Dir string

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	exitErr error
}

func (s *SingBox) exe() string { return filepath.Join(s.Dir, "bin", "sing-box.exe") }

// Ensure downloads and verifies sing-box if it is not there yet. It runs
// before any session, while the PC still has its normal internet.
func (s *SingBox) Ensure(ctx context.Context) error {
	if _, err := os.Stat(s.exe()); err == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zipURL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("download sing-box: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download sing-box: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return fmt.Errorf("download sing-box: %w", err)
	}
	return s.install(data, zipSHA256)
}

func (s *SingBox) install(data []byte, wantSHA string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != wantSHA {
		return fmt.Errorf("sing-box archive checksum %s, want %s", got, wantSHA)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	bin := filepath.Dir(s.exe())
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if !strings.HasSuffix(name, ".exe") && !strings.HasSuffix(name, ".dll") {
			continue
		}
		if err := extract(f, filepath.Join(bin, name)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(s.exe()); err != nil {
		return errors.New("sing-box.exe not found in the archive")
	}
	return nil
}

func extract(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Start writes the config and runs sing-box. It fails if sing-box exits
// within the first seconds, which is how config and TUN errors show up.
func (s *SingBox) Start(config []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		s.stopLocked()
	}
	cfgPath := filepath.Join(s.Dir, "config.json")
	if err := os.WriteFile(cfgPath, config, 0o600); err != nil {
		return err
	}
	logPath := filepath.Join(s.Dir, "sing-box.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(s.exe(), "run", "-c", cfgPath, "-D", s.Dir)
	cmd.Stdout, cmd.Stderr = logf, logf
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	// Without this, a crashed minivpn leaves sing-box routing everything to
	// a node that no longer exists: no internet until a reboot.
	if err := killWithParent(cmd); err != nil {
		cmd.Process.Kill()
		logf.Close()
		return fmt.Errorf("bind sing-box to the app: %w", err)
	}
	s.exitErr = nil
	done := make(chan struct{})
	go func() {
		waitErr := cmd.Wait()
		logf.Close()
		s.mu.Lock()
		if waitErr != nil {
			s.exitErr = fmt.Errorf("%w: %s", waitErr, lastLines(logPath, 5))
		}
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return fmt.Errorf("sing-box exited: %s", lastLines(logPath, 5))
	case <-time.After(startupWait):
	}
	s.cmd, s.done = cmd, done
	return nil
}

func (s *SingBox) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked()
}

func (s *SingBox) stopLocked() error {
	if s.cmd == nil {
		return nil
	}
	err := s.cmd.Process.Kill()
	<-s.done
	s.cmd, s.done = nil, nil
	s.exitErr = nil
	if errors.Is(err, os.ErrProcessDone) {
		err = nil
	}
	return err
}

func (s *SingBox) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Done returns the done channel of the running sing-box process, or nil if not running.
func (s *SingBox) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// ExitErr returns the last exit error recorded for sing-box.
func (s *SingBox) ExitErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

func lastLines(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
