package ui

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/marienbaum77/minivpn/internal/session"
)

func TestCircleICO(t *testing.T) {
	ico := circleICO(colorOn)
	var hdr [3]uint16
	binary.Read(bytes.NewReader(ico), binary.LittleEndian, &hdr)
	if hdr != [3]uint16{0, 1, 1} {
		t.Fatalf("ico header %v", hdr)
	}
	size := binary.LittleEndian.Uint32(ico[6+8:])
	off := binary.LittleEndian.Uint32(ico[6+12:])
	img, err := png.Decode(bytes.NewReader(ico[off : off+size]))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Error("corners must be transparent")
	}
	if _, g, _, a := img.At(16, 16).RGBA(); a != 0xffff || g < 0x8000 {
		t.Error("center must be solid green")
	}
}

func TestTooltip(t *testing.T) {
	now := time.Date(2026, 10, 28, 21, 0, 0, 0, time.UTC)
	on := session.Status{Phase: session.On, StartedAt: now.Add(-83*time.Minute - 5*time.Second), EstimatedRUB: 0.842}
	if got := tooltip(on, now); got != "minivpn: 1:23:05 · ~0,84 ₽" {
		t.Errorf("on: %q", got)
	}
	off := session.Status{Phase: session.Off, MonthSpent: 37.1, MonthlyLimit: 200}
	if got := tooltip(off, now); got != "minivpn: выключено · месяц 37,10 ₽ из 200,00 ₽" {
		t.Errorf("off: %q", got)
	}
	long := session.Status{Phase: session.On, StartedAt: now, AskExtend: true}
	if !strings.Contains(tooltip(long, now), "продлить") {
		t.Error("the tooltip must say the session is about to end")
	}
}
