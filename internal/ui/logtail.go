package ui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// LogTail keeps the last lines the app logged, short and readable, for the
// panel's log sheet. The full log still goes to minivpn.log.
type LogTail struct {
	mu    sync.Mutex
	lines []string
	next  int // number of the next line; the panel asks for lines after it
	max   int
}

func NewLogTail(max int) *LogTail { return &LogTail{max: max} }

// Since returns the lines numbered from seq on, and the number to ask with
// next time. Lines that fell out of the buffer are skipped.
func (t *LogTail) Since(seq int) ([]string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	first := t.next - len(t.lines)
	if seq < first {
		seq = first
	}
	if seq > t.next {
		seq = t.next
	}
	return append([]string{}, t.lines[seq-first:]...), t.next
}

func (t *LogTail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
	t.next++
}

// Handler is the slog side of the tail.
func (t *LogTail) Handler() slog.Handler { return tailHandler{tail: t} }

type tailHandler struct {
	tail  *LogTail
	attrs []slog.Attr
}

func (h tailHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h tailHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return tailHandler{tail: h.tail, attrs: append(append([]slog.Attr(nil), h.attrs...), as...)}
}

func (h tailHandler) WithGroup(string) slog.Handler { return h }

// Handle renders "15:04:05 msg k=v". A node line is the setup script's own
// output, so it goes without the "node line=" wrapping.
func (h tailHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05 "))
	if r.Level >= slog.LevelWarn {
		b.WriteString("! ")
	}
	var rest []string
	add := func(a slog.Attr) bool {
		if r.Message == "node" && a.Key == "line" {
			b.WriteString(a.Value.String())
			return true
		}
		if s := a.Value.String(); s != "" {
			rest = append(rest, fmt.Sprintf("%s=%s", a.Key, s))
		}
		return true
	}
	if r.Message != "node" {
		b.WriteString(r.Message)
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	if len(rest) > 0 {
		b.WriteString(" · " + strings.Join(rest, " "))
	}
	h.tail.add(b.String())
	return nil
}
