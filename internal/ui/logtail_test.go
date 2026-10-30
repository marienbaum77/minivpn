package ui

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestLogTail(t *testing.T) {
	tail := NewLogTail(3)
	log := slog.New(tail.Handler())
	log.Info("node", "line", "step download", "after", "25s")
	log.Warn("sing-box exited, restarting")

	got, next := tail.Since(0)
	if next != 2 || len(got) != 2 {
		t.Fatalf("got %q, next %d", got, next)
	}
	if !strings.HasSuffix(got[0], " step download · after=25s") || strings.Contains(got[0], "line=") {
		t.Errorf("node line: %q", got[0])
	}
	if !strings.HasSuffix(got[1], " ! sing-box exited, restarting") {
		t.Errorf("warning: %q", got[1])
	}

	for _, m := range []string{"a", "b", "c"} {
		log.Info(m)
	}
	got, next = tail.Since(2)
	if next != 5 || len(got) != 3 {
		t.Fatalf("after overflow: %q, next %d", got, next)
	}
	// Asked from a line that fell out: get what is left.
	got, _ = tail.Since(0)
	if len(got) != 3 || !strings.HasSuffix(got[0], " a") {
		t.Fatalf("from the start: %q", got)
	}
	// Nothing new is an empty list, not null: the panel reads its length.
	if got, _ := tail.Since(5); got == nil || !slices.Equal(got, []string{}) {
		t.Fatalf("nothing new: %#v", got)
	}
}
