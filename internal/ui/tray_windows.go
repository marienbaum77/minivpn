package ui

import (
	"context"
	"image/color"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"fyne.io/systray"
	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"github.com/marienbaum77/minivpn/internal/session"
)

// RunTray blocks until the user picks "Выход". openPanel shows the panel
// right away (manual launch, as opposed to autostart at logon).
func RunTray(b Backend, srv *Server, dataDir string, openPanel bool) {
	p := &panel{url: srv.URL(), dataDir: filepath.Join(dataDir, "webview")}
	srv.OnPin = p.setPinned
	systray.Run(func() { onReady(b, p, openPanel) }, func() {})
}

func onReady(b Backend, p *panel, openPanel bool) {
	icons := map[color.RGBA][]byte{}
	icon := func(c color.RGBA) []byte {
		if icons[c] == nil {
			icons[c] = circleICO(c)
		}
		return icons[c]
	}
	systray.SetIcon(icon(colorOff))
	systray.SetTitle("minivpn")
	systray.SetTooltip("minivpn")
	systray.SetOnTapped(p.show)

	mOpen := systray.AddMenuItem("Открыть панель", "")
	mToggle := systray.AddMenuItem("Пуск", "")
	mPause := systray.AddMenuItem("Пауза", "")
	mPause.Hide()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Остановить сессию, удалить ноду и закрыть minivpn")
	if openPanel {
		p.show()
	}

	go func() {
		var lastColor color.RGBA
		var lastTip, lastToggle string
		var lastPauseTitle string
		var lastPauseVisible bool
		var asked bool
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			st := b.Status()
			if c := stateColor(st); c != lastColor {
				systray.SetIcon(icon(c))
				lastColor = c
			}
			if tip := tooltip(st, time.Now()); tip != lastTip {
				systray.SetTooltip(tip)
				lastTip = tip
			}
			toggle := "Пуск"
			if st.Phase != session.Off {
				toggle = "Стоп"
			}
			if toggle != lastToggle {
				mToggle.SetTitle(toggle)
				lastToggle = toggle
			}

			pauseVisible := st.Phase == session.On || st.Phase == session.Paused
			pauseTitle := "Пауза"
			if st.Phase == session.Paused {
				pauseTitle = "Возобновить"
			}
			if pauseVisible != lastPauseVisible {
				if pauseVisible {
					mPause.Show()
				} else {
					mPause.Hide()
				}
				lastPauseVisible = pauseVisible
			}
			if pauseTitle != lastPauseTitle {
				mPause.SetTitle(pauseTitle)
				lastPauseTitle = pauseTitle
			}

			// The extend question must be seen, not hidden behind a hover.
			if st.AskExtend && !asked {
				p.show()
			}
			asked = st.AskExtend

			select {
			case <-mOpen.ClickedCh:
				p.show()
			case <-mToggle.ClickedCh:
				if b.Status().Phase == session.Off {
					go func() {
						if b.Start(context.Background(), false) != nil {
							p.show() // the panel shows why
						}
					}()
				} else {
					go b.Stop(session.ReasonStop)
				}
			case <-mPause.ClickedCh:
				curr := b.Status().Phase
				if curr == session.On {
					go b.Pause()
				} else if curr == session.Paused {
					go b.Resume()
				}
			case <-mQuit.ClickedCh:
				systray.SetTooltip("minivpn: останавливаю и удаляю ноду…")
				systray.SetIcon(icon(colorBusy))
				p.close()
				systray.Quit()
				return
			case <-t.C:
			}
		}
	}()
}

// panel is the WebView2 window. It is created on demand on its own OS
// thread and destroyed when closed; the app keeps running in the tray.
type panel struct {
	url     string
	dataDir string

	mu     sync.Mutex
	w      webview2.WebView
	hwnd   windows.HWND
	pinned bool
}

const panelW, panelH = 380, 580

func (p *panel) show() {
	p.mu.Lock()
	if p.w != nil {
		w, hwnd := p.w, p.hwnd
		p.mu.Unlock()
		w.Dispatch(func() { bringToFront(hwnd) })
		return
	}
	p.mu.Unlock()

	go func() {
		runtime.LockOSThread()
		w := webview2.NewWithOptions(webview2.WebViewOptions{
			DataPath:      p.dataDir,
			AutoFocus:     true,
			WindowOptions: webview2.WindowOptions{Title: "minivpn", Width: panelW, Height: panelH},
		})
		if w == nil {
			// No WebView2 runtime: the same page works in the browser, minus pinning.
			windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(p.url), nil, nil, windows.SW_SHOWNORMAL)
			return
		}
		hwnd := windows.HWND(uintptr(w.Window()))
		p.mu.Lock()
		p.w, p.hwnd = w, hwnd
		pinned := p.pinned
		p.mu.Unlock()

		placeNearTray(hwnd, panelW, panelH)
		setTopmost(hwnd, pinned)
		w.Navigate(p.url)
		bringToFront(hwnd)
		w.Run()
		w.Destroy()

		p.mu.Lock()
		p.w = nil
		p.mu.Unlock()
	}()
}

func (p *panel) setPinned(on bool) {
	p.mu.Lock()
	p.pinned = on
	w, hwnd := p.w, p.hwnd
	p.mu.Unlock()
	if w != nil {
		w.Dispatch(func() { setTopmost(hwnd, on) })
	}
}

func (p *panel) close() {
	p.mu.Lock()
	w := p.w
	p.mu.Unlock()
	if w != nil {
		w.Terminate()
	}
}

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procSystemParamsInfo = user32.NewProc("SystemParametersInfoW")
	procGetDpiForWindow  = user32.NewProc("GetDpiForWindow")
	procSetForegroundWnd = user32.NewProc("SetForegroundWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
	hwndTopmost          = ^uintptr(0) // HWND_TOPMOST (-1)
	hwndNoTopmost        = ^uintptr(1) // HWND_NOTOPMOST (-2)
	swpNoMove, swpNoSize = uintptr(0x0002), uintptr(0x0001)
	spiGetWorkArea       = uintptr(0x0030)
	swRestore            = uintptr(9)
)

// placeNearTray puts the window in the bottom-right corner of the work area,
// above the taskbar, scaled for the monitor's DPI.
func placeNearTray(hwnd windows.HWND, w, h int) {
	var area windows.Rect
	procSystemParamsInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&area)), 0)
	dpi, _, _ := procGetDpiForWindow.Call(uintptr(hwnd))
	if dpi == 0 {
		dpi = 96
	}
	sw, sh := w*int(dpi)/96, h*int(dpi)/96
	margin := 12 * int(dpi) / 96
	x, y := int(area.Right)-sw-margin, int(area.Bottom)-sh-margin
	procSetWindowPos.Call(uintptr(hwnd), 0, uintptr(x), uintptr(y), uintptr(sw), uintptr(sh), 0)
}

func setTopmost(hwnd windows.HWND, on bool) {
	after := hwndNoTopmost
	if on {
		after = hwndTopmost
	}
	procSetWindowPos.Call(uintptr(hwnd), after, 0, 0, 0, 0, swpNoMove|swpNoSize)
}

func bringToFront(hwnd windows.HWND) {
	procShowWindow.Call(uintptr(hwnd), swRestore)
	procSetForegroundWnd.Call(uintptr(hwnd))
}
