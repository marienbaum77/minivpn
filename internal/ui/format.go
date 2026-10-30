package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/marienbaum77/minivpn/internal/session"
)

var phaseNames = map[session.Phase]string{
	session.Off:        "выключено",
	session.Creating:   "создаю ноду",
	session.Installing: "нода ставит sing-box",
	session.Connecting: "подключаю",
	session.On:         "работает",
	session.Paused:     "пауза",
	session.Stopping:   "останавливаю",
}

// tooltip is what hovering the tray icon shows (Windows cuts it at 127 chars).
func tooltip(st session.Status, now time.Time) string {
	var s string
	switch st.Phase {
	case session.Off:
		s = fmt.Sprintf("minivpn: выключено · месяц %s из %s", rub(st.MonthSpent), rub(st.MonthlyLimit))
		if st.Error != "" {
			s = "minivpn: ошибка, открой панель"
		}
	case session.On:
		s = fmt.Sprintf("minivpn: %s · ~%s", clock(now.Sub(st.StartedAt)), rub(st.EstimatedRUB))
		if st.AskExtend {
			s += " · скоро конец, продлить?"
		}
	case session.Paused:
		s = fmt.Sprintf("minivpn: пауза · ~%s", rub(st.EstimatedRUB))
	default:
		s = "minivpn: " + phaseNames[st.Phase] + "…"
	}
	if r := []rune(s); len(r) > 127 {
		s = string(r[:127])
	}
	return s
}

func clock(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func rub(v float64) string {
	return strings.Replace(fmt.Sprintf("%.2f ₽", v), ".", ",", 1)
}
