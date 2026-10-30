// minivpn: a personal VPN on a node that exists only while you use it.
//
// The app lives in the tray. Start creates a server at Serverspace, waits
// until the node answers through Reality, and routes the PC through it with
// sing-box; Stop (or idle, or the time limit) deletes the server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/marienbaum77/minivpn/internal/app"
	"github.com/marienbaum77/minivpn/internal/settings"
	"github.com/marienbaum77/minivpn/internal/ui"
)

var (
	kernel32        = windows.NewLazySystemDLL("kernel32.dll")
	procFreeConsole = kernel32.NewProc("FreeConsole")
)

func main() {
	procFreeConsole.Call()

	autostart := flag.Bool("autostart", false, "started at logon: stay in the tray")
	flag.Parse()

	mutexName := windows.StringToUTF16Ptr(`Local\minivpn`)
	if mutex, err := windows.OpenMutex(windows.SYNCHRONIZE, false, mutexName); err == nil {
		windows.CloseHandle(mutex)
		windows.MessageBox(0,
			windows.StringToUTF16Ptr("minivpn уже запущен и находится в системном трее. Закрой его через меню значка в трее, чтобы перезапустить приложение."),
			windows.StringToUTF16Ptr("minivpn"),
			windows.MB_ICONINFORMATION)
		return
	}

	// TUN needs administrator rights. Ask once at launch (UAC) rather than
	// installing a service.
	if !windows.GetCurrentProcessToken().IsElevated() {
		if err := relaunchElevated(); err != nil {
			fatal("Нужны права администратора: без них sing-box не создаст TUN.\n\n" + err.Error())
		}
		return
	}
	mutex, err := windows.CreateMutex(nil, false, mutexName)
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(mutex)
		windows.MessageBox(0,
			windows.StringToUTF16Ptr("minivpn уже запущен и находится в системном трее. Закрой его через меню значка в трее, чтобы перезапустить приложение."),
			windows.StringToUTF16Ptr("minivpn"),
			windows.MB_ICONINFORMATION)
		return // already in the tray
	}
	defer windows.CloseHandle(mutex)

	dir, err := settings.Dir()
	if err != nil {
		fatal(err.Error())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatal(err.Error())
	}
	tail := ui.NewLogTail(300)
	log := slog.New(slog.NewMultiHandler(openLog(filepath.Join(dir, "minivpn.log")), tail.Handler()))

	a, err := app.Open(dir, log, platform{})
	if err != nil {
		fatal("Не удалось открыть данные minivpn:\n\n" + err.Error())
	}
	if a.Settings().Autostart {
		if err := (platform{}).SetAutostart(true); err != nil {
			log.Warn("autostart", "err", err)
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		a.Init(ctx)
	}()

	srv, err := ui.NewServer(a)
	if err != nil {
		fatal(err.Error())
	}
	srv.Log = tail
	go func() {
		if err := srv.Serve(); err != nil {
			log.Error("panel server", "err", err)
		}
	}()

	ui.RunTray(a, srv, dir, !*autostart)
	a.Close() // deletes the node if a session is running
	log.Info("exit")
}

func relaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	args := strings.Join(os.Args[1:], " ")
	return windows.ShellExecute(0, windows.StringToUTF16Ptr("runas"), windows.StringToUTF16Ptr(exe),
		windows.StringToUTF16Ptr(args), windows.StringToUTF16Ptr(cwd), windows.SW_HIDE)
}

func openLog(path string) slog.Handler {
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.DiscardHandler
	}
	return slog.NewTextHandler(f, nil)
}

func fatal(msg string) {
	windows.MessageBox(0, windows.StringToUTF16Ptr(msg), windows.StringToUTF16Ptr("minivpn"), windows.MB_ICONERROR)
	os.Exit(1)
}

type platform struct{}

// SetAutostart uses Task Scheduler, not the Run key: a program that needs
// administrator rights is silently skipped from Run at logon, while a task
// with "highest privileges" starts without a UAC prompt.
func (platform) SetAutostart(on bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"/Delete", "/F", "/TN", "minivpn"}
	if on {
		args = []string{"/Create", "/F", "/TN", "minivpn", "/SC", "ONLOGON", "/RL", "HIGHEST", "/TR", fmt.Sprintf(`"%s" --autostart`, exe)}
	}
	cmd := exec.Command("schtasks", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err != nil && on {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil // deleting a task that does not exist is fine
}

func (platform) SteamPath() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Valve\Steam`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	p, _, err := k.GetStringValue("SteamPath")
	if err != nil {
		return ""
	}
	return filepath.FromSlash(p)
}
