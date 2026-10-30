package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH is how the app reaches a fresh node. Serverspace's API accepts
// server_init_script but never runs it (checked 2026-10-29 on Ubuntu 24.04
// and 26.04: a script that only powers the server off left it running), so
// the app logs in with the root password the API returns instead.
type SSH struct {
	Addr     string // host:port
	User     string
	Password string
	// Env is set for the script; tests use it to run without systemd.
	Env map[string]string
}

const (
	setupPath = "/root/minivpn-setup.sh"
	setupLog  = "/var/log/minivpn-setup.log"
)

// ErrUnreachable says SSH never answered. The session treats it as a reason
// to replace the node: Serverspace hands recently freed addresses back, and a
// filtered address stays unreachable for a while even though the server runs.
var ErrUnreachable = errors.New("нода не отвечает по SSH")

// attemptsGap is how long between periodic "still waiting" progress lines
// during the SSH retry loop.
const attemptsGap = 15 * time.Second

// Install puts the setup script on the node and runs it detached from the
// session, so a dropped connection does not kill it halfway. The script's
// log streams to progress line by line until it reports "setup done" or
// "failed at <step>"; after that the script turns SSH off.
func Install(ctx context.Context, s SSH, script string, progress func(string)) error {
	client, err := dial(ctx, s, progress)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := run(client, "umask 077 && cat > "+setupPath, strings.NewReader(script)); err != nil {
		return fmt.Errorf("скрипт не загрузился на ноду: %w", err)
	}
	// setsid -f forks the script into its own session and returns at once;
	// a shell "&" would leave a waiting subshell holding the SSH channel open.
	start := ": > " + setupLog + " && setsid -f env " + envArgs(s.Env) + "sh " + setupPath + " </dev/null >/dev/null 2>&1"
	if err := run(client, start, nil); err != nil {
		return fmt.Errorf("скрипт не запустился на ноде: %w", err)
	}
	return follow(ctx, client, progress)
}

// dial retries until sshd is up and takes the password: right after
// creation the server is still booting.
func dial(ctx context.Context, s SSH, progress func(string)) (*ssh.Client, error) {
	answer := func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range answers {
			answers[i] = s.Password
		}
		return answers, nil
	}
	cfg := &ssh.ClientConfig{
		User: s.User,
		Auth: []ssh.AuthMethod{ssh.Password(s.Password), ssh.KeyboardInteractive(answer)},
		// Nothing knows a fresh server's host key: the API does not report
		// it. The first key is trusted, which is all a node that lives for
		// one session allows.
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			progress("host key " + ssh.FingerprintSHA256(key))
			return nil
		},
	}
	start := time.Now()
	var attempts int
	var lastReason string
	var lastProgress time.Time
	for {
		attempts++
		client, err := dialOnce(ctx, s.Addr, cfg)
		if err == nil {
			return client, nil
		}
		elapsed := time.Since(start)
		reason := sshWaitReason(err)
		now := time.Now()
		if shouldReportSSHWait(reason, lastReason, lastProgress, now) {
			progress(fmt.Sprintf("ssh: %s (%ds, attempt %d)", reason, int(elapsed.Seconds()), attempts))
			lastReason, lastProgress = reason, now
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: за %d попыток: %v", ErrUnreachable, attempts, err)
		case <-time.After(3 * time.Second):
		}
	}
}

func sshWaitReason(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "порт SSH (TCP/22) пока не принимает подключения"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "тайм-аут при подключении к SSH (TCP/22)"
	case errors.Is(err, io.EOF):
		return "порт SSH доступен, но сервер ещё не завершил рукопожатие"
	default:
		return "SSH пока не готов: " + err.Error()
	}
}

func shouldReportSSHWait(reason, lastReason string, lastProgress, now time.Time) bool {
	return reason != lastReason || lastProgress.IsZero() || now.Sub(lastProgress) >= attemptsGap
}

func dialOnce(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second)) // a stuck handshake
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func run(c *ssh.Client, cmd string, stdin io.Reader) error {
	sess, err := c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = stdin
	if out, err := sess.CombinedOutput(cmd); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// follow tails the setup log until the script says how it ended.
func follow(ctx context.Context, c *ssh.Client, progress func(string)) error {
	sess, err := c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	out, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	if err := sess.Start("tail -n +1 -F " + setupLog + " 2>/dev/null"); err != nil {
		return err
	}
	lines := make(chan string)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-done:
				return
			}
		}
	}()
	var recent []string // the lines before a failure say why
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return errors.New("SSH оборвался до конца установки: " + strings.Join(recent, " | "))
			}
			progress(line)
			switch {
			case strings.HasPrefix(line, "setup done"):
				return nil
			case strings.HasPrefix(line, "failed at "):
				return fmt.Errorf("установка упала на шаге %s: %s", strings.TrimPrefix(line, "failed at "), strings.Join(recent, " | "))
			}
			if recent = append(recent, line); len(recent) > 3 {
				recent = recent[1:]
			}
		}
	}
}

func envArgs(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s='%s' ", k, strings.ReplaceAll(env[k], "'", ""))
	}
	return b.String()
}
