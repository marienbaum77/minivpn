package node

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestSSHWaitReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "connection refused",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			want: "порт SSH (TCP/22) пока не принимает подключения",
		},
		{
			name: "timeout",
			err:  timeoutError{},
			want: "тайм-аут при подключении к SSH (TCP/22)",
		},
		{
			name: "server closed connection",
			err:  io.EOF,
			want: "порт SSH доступен, но сервер ещё не завершил рукопожатие",
		},
		{
			name: "other SSH error",
			err:  errors.New("ssh: unable to authenticate"),
			want: "SSH пока не готов: ssh: unable to authenticate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshWaitReason(tt.err); got != tt.want {
				t.Fatalf("sshWaitReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldReportSSHWait(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		reason     string
		lastReason string
		last       time.Time
		now        time.Time
		want       bool
	}{
		{name: "first report", reason: "timeout", now: start, want: true},
		{name: "same error is throttled", reason: "timeout", lastReason: "timeout", last: start, now: start.Add(5 * time.Second), want: false},
		{name: "same error reported after interval", reason: "timeout", lastReason: "timeout", last: start, now: start.Add(attemptsGap), want: true},
		{name: "changed error reported immediately", reason: "refused", lastReason: "timeout", last: start, now: start.Add(time.Second), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReportSSHWait(tt.reason, tt.lastReason, tt.last, tt.now); got != tt.want {
				t.Fatalf("shouldReportSSHWait() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstallUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var progress []string
	err = Install(ctx, SSH{Addr: addr, User: "root", Password: "test"}, "", func(line string) {
		progress = append(progress, line)
	})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Install() error = %v, want ErrUnreachable", err)
	}
	if len(progress) == 0 {
		t.Fatal("Install() did not report the SSH connection failure")
	}
}
