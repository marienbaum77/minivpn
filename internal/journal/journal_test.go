package journal

import (
	"bytes"
	"encoding/csv"
	"path/filepath"
	"testing"
	"time"

	"github.com/marienbaum77/minivpn/internal/check"
)

func TestJournal(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	oct := time.Date(2026, 10, 28, 20, 0, 0, 0, time.UTC)
	old, _ := j.Start(Session{StartedAt: oct.AddDate(0, -1, 0), RatePerHour: 0.6})
	j.SetCost(old, 5, -1)
	j.Finish(old, oct.AddDate(0, -1, 0).Add(time.Hour), "stop")

	id, err := j.Start(Session{StartedAt: oct, Location: "nj3", RatePerHour: 0.609, ISP: "Rostelecom", City: "Novosibirsk"})
	if err != nil {
		t.Fatal(err)
	}
	j.SetNode(id, "l9s42", "203.0.113.7")
	j.AddCheck(id, check.Result{At: oct, OK: true, Handshake: 300 * time.Millisecond, SpeedMbps: 41.5})
	j.AddCheck(id, check.Result{At: oct.Add(15 * time.Minute), Failed: check.StageBulk, Error: "timeout"})
	j.SetCost(id, 0.61, -1)

	unfinished, _ := j.Unfinished()
	if len(unfinished) != 1 || unfinished[0].ServerID != "l9s42" {
		t.Fatalf("unfinished: %+v", unfinished)
	}

	// The charged amount wins over the estimate once known.
	j.SetCost(id, 0.61, 0.8)
	j.SetCost(id, 0.7, -1) // a later estimate must not erase it
	spent, _ := j.MonthSpent(oct)
	if spent != 0.8 {
		t.Fatalf("month spent = %v, want 0.8 (last month's session excluded)", spent)
	}

	j.Finish(id, oct.Add(62*time.Minute), "idle")
	var buf bytes.Buffer
	if err := j.ExportCSV(&buf); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want header + 2 sessions, got %d rows", len(rows))
	}
	last := rows[2]
	col := map[string]string{}
	for i, h := range rows[0] {
		col[h] = last[i]
	}
	if col["minutes"] != "62" || col["failed_stages"] != "bulk:1" || col["median_handshake_ms"] != "300" || col["rub_charged"] != "0.80" || col["speed_mbps"] != "41.5" {
		t.Fatalf("row: %v", col)
	}
}
