// Package journal records sessions and checks. It is both the spending
// ledger behind the monthly limit and the measurement log: what the network
// from Russia did to the node, session after session.
package journal

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"github.com/marienbaum77/minivpn/internal/check"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id            INTEGER PRIMARY KEY,
	started_at    TEXT NOT NULL,
	ended_at      TEXT,
	end_reason    TEXT NOT NULL DEFAULT '',
	server_id     TEXT NOT NULL DEFAULT '',
	node_ip       TEXT NOT NULL DEFAULT '',
	location      TEXT NOT NULL DEFAULT '',
	rate_per_hour REAL NOT NULL,
	estimated_rub REAL NOT NULL DEFAULT 0,
	charged_rub   REAL,
	isp           TEXT NOT NULL DEFAULT '',
	city          TEXT NOT NULL DEFAULT '',
	reputation    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS checks (
	id           INTEGER PRIMARY KEY,
	session_id   INTEGER NOT NULL REFERENCES sessions(id),
	at           TEXT NOT NULL,
	ok           INTEGER NOT NULL,
	failed_stage TEXT NOT NULL DEFAULT '',
	error        TEXT NOT NULL DEFAULT '',
	tcp_ms       INTEGER NOT NULL DEFAULT 0,
	handshake_ms INTEGER NOT NULL DEFAULT 0,
	bulk_ms      INTEGER NOT NULL DEFAULT 0,
	bulk_bytes   INTEGER NOT NULL DEFAULT 0,
	speed_mbps   REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS checks_session ON checks(session_id);
`

type Journal struct {
	db *sql.DB
}

func Open(path string) (*Journal, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("journal schema: %w", err)
	}
	return &Journal{db: db}, nil
}

func (j *Journal) Close() error { return j.db.Close() }

type Session struct {
	ID           int64
	StartedAt    time.Time
	EndedAt      time.Time // zero while running
	EndReason    string
	ServerID     string
	NodeIP       string
	Location     string
	RatePerHour  float64
	EstimatedRUB float64
	// ChargedRUB is the balance difference; Charged is false when unknown.
	ChargedRUB float64
	Charged    bool
	// ISP and City describe the user's network. The user's own IP is never
	// stored, so the journal can be published.
	ISP        string
	City       string
	Reputation string // JSON of check.Reputation
}

// Cost is what the session counts as against the monthly limit: the charged
// amount when known, the estimate otherwise.
func (s Session) Cost() float64 {
	if s.Charged {
		return s.ChargedRUB
	}
	return s.EstimatedRUB
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (j *Journal) Start(s Session) (int64, error) {
	res, err := j.db.Exec(`INSERT INTO sessions (started_at, location, rate_per_hour, isp, city) VALUES (?, ?, ?, ?, ?)`,
		ts(s.StartedAt), s.Location, s.RatePerHour, s.ISP, s.City)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (j *Journal) SetNode(id int64, serverID, ip string) error {
	_, err := j.db.Exec(`UPDATE sessions SET server_id = ?, node_ip = ? WHERE id = ?`, serverID, ip, id)
	return err
}

// SetLocation records the location of a node that a retry moved.
func (j *Journal) SetLocation(id int64, location string) error {
	_, err := j.db.Exec(`UPDATE sessions SET location = ? WHERE id = ?`, location, id)
	return err
}

func (j *Journal) SetReputation(id int64, rep string) error {
	_, err := j.db.Exec(`UPDATE sessions SET reputation = ? WHERE id = ?`, rep, id)
	return err
}

// SetCost updates the running totals; charged < 0 means unknown.
func (j *Journal) SetCost(id int64, estimated, charged float64) error {
	var ch any
	if charged >= 0 {
		ch = charged
	}
	_, err := j.db.Exec(`UPDATE sessions SET estimated_rub = ?, charged_rub = COALESCE(?, charged_rub) WHERE id = ?`, estimated, ch, id)
	return err
}

func (j *Journal) Finish(id int64, at time.Time, reason string) error {
	_, err := j.db.Exec(`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ?`, ts(at), reason, id)
	return err
}

func (j *Journal) AddCheck(sessionID int64, r check.Result) error {
	_, err := j.db.Exec(`INSERT INTO checks (session_id, at, ok, failed_stage, error, tcp_ms, handshake_ms, bulk_ms, bulk_bytes, speed_mbps)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, ts(r.At), r.OK, string(r.Failed), r.Error,
		r.TCP.Milliseconds(), r.Handshake.Milliseconds(), r.Bulk.Milliseconds(), r.BulkBytes, r.SpeedMbps)
	return err
}

const sessionCols = `id, started_at, COALESCE(ended_at, ''), end_reason, server_id, node_ip, location, rate_per_hour, estimated_rub, charged_rub, isp, city, reputation`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var started, ended string
	var charged sql.NullFloat64
	err := row.Scan(&s.ID, &started, &ended, &s.EndReason, &s.ServerID, &s.NodeIP, &s.Location,
		&s.RatePerHour, &s.EstimatedRUB, &charged, &s.ISP, &s.City, &s.Reputation)
	if err != nil {
		return s, err
	}
	s.StartedAt, _ = time.Parse(time.RFC3339, started)
	if ended != "" {
		s.EndedAt, _ = time.Parse(time.RFC3339, ended)
	}
	s.ChargedRUB, s.Charged = charged.Float64, charged.Valid
	return s, nil
}

func (j *Journal) query(q string, args ...any) ([]Session, error) {
	rows, err := j.db.Query(`SELECT `+sessionCols+` FROM sessions `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Unfinished are sessions the app did not close: it crashed or the PC died.
// Their servers may still exist and bill.
func (j *Journal) Unfinished() ([]Session, error) {
	return j.query(`WHERE ended_at IS NULL ORDER BY id`)
}

func (j *Journal) Recent(n int) ([]Session, error) {
	return j.query(`ORDER BY id DESC LIMIT ?`, n)
}

// MonthSpent sums sessions started in now's calendar month (local time).
func (j *Journal) MonthSpent(now time.Time) (float64, error) {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	sessions, err := j.query(`WHERE started_at >= ?`, ts(from))
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, s := range sessions {
		sum += s.Cost()
	}
	return sum, nil
}

// ExportCSV writes one row per finished session with its check summary.
// Node IPs are kept (they are the hoster's, and blocks are per address);
// nothing identifies the user beyond ISP and city.
func (j *Journal) ExportCSV(w io.Writer) error {
	sessions, err := j.query(`WHERE ended_at IS NOT NULL ORDER BY id`)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	cw.Write([]string{"started_at", "minutes", "end_reason", "location", "node_ip", "isp", "city",
		"rub", "rub_charged", "checks", "failed", "failed_stages", "median_handshake_ms", "speed_mbps", "reputation"})
	for _, s := range sessions {
		st, err := j.checkStats(s.ID)
		if err != nil {
			return err
		}
		charged := ""
		if s.Charged {
			charged = money(s.ChargedRUB)
		}
		cw.Write([]string{
			ts(s.StartedAt), strconv.Itoa(int(s.EndedAt.Sub(s.StartedAt).Minutes())), s.EndReason, s.Location, s.NodeIP, s.ISP, s.City,
			money(s.EstimatedRUB), charged, strconv.Itoa(st.total), strconv.Itoa(st.failed), st.stages,
			strconv.FormatInt(st.medianHandshake, 10), strconv.FormatFloat(st.speed, 'f', 1, 64), s.Reputation,
		})
	}
	cw.Flush()
	return cw.Error()
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

type checkStats struct {
	total, failed   int
	stages          string
	medianHandshake int64
	speed           float64
}

func (j *Journal) checkStats(id int64) (checkStats, error) {
	var st checkStats
	rows, err := j.db.Query(`SELECT ok, failed_stage, handshake_ms, speed_mbps FROM checks WHERE session_id = ? ORDER BY handshake_ms`, id)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	var hs []int64
	stages := map[string]int{}
	for rows.Next() {
		var ok bool
		var stage string
		var h int64
		var speed float64
		if err := rows.Scan(&ok, &stage, &h, &speed); err != nil {
			return st, err
		}
		st.total++
		if ok {
			hs = append(hs, h)
		} else {
			st.failed++
			stages[stage]++
		}
		if speed > st.speed {
			st.speed = speed
		}
	}
	if len(hs) > 0 {
		st.medianHandshake = hs[len(hs)/2]
	}
	for _, s := range []string{"tcp", "handshake", "bulk"} {
		if n := stages[s]; n > 0 {
			if st.stages != "" {
				st.stages += " "
			}
			st.stages += s + ":" + strconv.Itoa(n)
		}
	}
	return st, rows.Err()
}
