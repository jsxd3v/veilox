// Package store keeps one row per proxied request in SQLite: tokens, cost, latency and how many
// values were masked per type. It never stores prompt or response text or any detected value.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no CGO

	"github.com/jsxd3v/veilox/internal/pricing"
)

// Record is one proxied request.
type Record struct {
	Time          time.Time
	Day           string // YYYY-MM-DD in the configured timezone
	App           string
	Provider      string
	Model         string
	Stream        bool
	Status        int // HTTP status; 499 when the app closed the connection
	Usage         pricing.Usage
	CostMicros    *int64 // nil when the model has no price
	LatencyMS     int64
	TTFBMS        int64
	Masked        map[string]int
	UnmaskedParts int
	ErrorType     string
}

// Store is the SQLite database.
type Store struct {
	db    *sql.DB
	queue chan Record
	wg    sync.WaitGroup
	errFn func(error)

	pending sync.WaitGroup
	mu      sync.RWMutex // guards closed and sends on queue
	closed  bool
}

// ErrClosed is reported (through the writer's error callback) for records added after Close.
var ErrClosed = errors.New("store: closed; request record dropped")

const schemaVersion = 1

// Open opens (and creates or upgrades) the database at path.
func Open(path string) (*Store, error) {
	ctx := context.Background()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return &Store{db: db, errFn: func(error) {}}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion {
		return fmt.Errorf("schema version %d is newer than this veilox (%d); upgrade veilox", v, schemaVersion)
	}
	if v < 1 {
		_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS requests (
  id                 INTEGER PRIMARY KEY,
  ts                 TEXT    NOT NULL,
  day                TEXT    NOT NULL,
  app                TEXT    NOT NULL,
  provider           TEXT    NOT NULL,
  model              TEXT    NOT NULL,
  stream             INTEGER NOT NULL,
  status             INTEGER NOT NULL,
  input_tokens       INTEGER NOT NULL,
  output_tokens      INTEGER NOT NULL,
  cache_read_tokens  INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  cost_micros        INTEGER,
  latency_ms         INTEGER NOT NULL,
  ttfb_ms            INTEGER NOT NULL,
  masked             TEXT    NOT NULL,
  masked_total       INTEGER NOT NULL,
  unmasked_parts     INTEGER NOT NULL,
  error_type         TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS requests_day_app ON requests(day, app);
CREATE INDEX IF NOT EXISTS requests_ts ON requests(ts);
PRAGMA user_version = 1;`)
		if err != nil {
			return err
		}
	}
	return nil
}

// StartWriter starts the background writer. Records added with Add are written in order by one
// goroutine, so a slow disk never delays a response. onError is called for failed writes.
func (s *Store) StartWriter(onError func(error)) {
	s.queue = make(chan Record, 1024)
	if onError != nil {
		s.errFn = onError
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for r := range s.queue {
			if err := s.Insert(context.Background(), r); err != nil {
				s.errFn(err)
			}
			s.pending.Done()
		}
	}()
}

// Add queues a record for the writer (or writes it directly if no writer runs).
func (s *Store) Add(r Record) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.errFn(ErrClosed) // e.g. a stream that outlived the shutdown grace period
		return
	}
	if s.queue == nil {
		if err := s.Insert(context.Background(), r); err != nil {
			s.errFn(err)
		}
		return
	}
	s.pending.Add(1)
	s.queue <- r
}

// Wait blocks until every queued record is written.
func (s *Store) Wait() { s.pending.Wait() }

// Close writes what is queued and closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.queue != nil {
		close(s.queue)
	}
	s.mu.Unlock()
	s.wg.Wait()
	return s.db.Close()
}

// Insert writes one record now.
func (s *Store) Insert(ctx context.Context, r Record) error {
	masked := r.Masked
	if masked == nil {
		masked = map[string]int{}
	}
	mj, err := json.Marshal(masked) // map keys are sorted, so the JSON is stable
	if err != nil {
		return err
	}
	total := 0
	for _, n := range masked {
		total += n
	}
	var cost any
	if r.CostMicros != nil {
		cost = *r.CostMicros
	}
	stream := 0
	if r.Stream {
		stream = 1
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO requests (ts, day, app, provider, model, stream, status, input_tokens, output_tokens,
  cache_read_tokens, cache_write_tokens, cost_micros, latency_ms, ttfb_ms, masked, masked_total, unmasked_parts, error_type)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Time.UTC().Format(time.RFC3339Nano), r.Day, r.App, r.Provider, r.Model, stream, r.Status,
		r.Usage.Input, r.Usage.Output, r.Usage.CacheRead, r.Usage.CacheWrite, cost, r.LatencyMS, r.TTFBMS,
		string(mj), total, r.UnmaskedParts, r.ErrorType)
	return err
}

// SpendByApp returns the priced spend per app on one day, in micro-dollars.
func (s *Store) SpendByApp(ctx context.Context, day string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app, COALESCE(SUM(cost_micros), 0) FROM requests WHERE day = ? GROUP BY app`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var app string
		var micros int64
		if err := rows.Scan(&app, &micros); err != nil {
			return nil, err
		}
		out[app] = micros
	}
	return out, rows.Err()
}

// Prune deletes rows older than before and returns how many were deleted.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM requests WHERE ts < ?`, before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Query selects rows for a report.
type Query struct {
	From, To string   // days, inclusive (YYYY-MM-DD)
	App      string   // optional filter
	By       []string // any of app, model, day, provider
}

// Row is one line of a report.
type Row struct {
	Day, App, Model, Provider string
	Requests                  int64
	InputTokens               int64 // input + cache read + cache write
	OutputTokens              int64
	CostMicros                int64 // priced requests only
	Unpriced                  int64 // requests without a price
	Errors                    int64 // requests that ended with status 400 or higher (incl. 499: app closed)
	Masked                    int64
}

// ValidDims are the columns a report can group by.
var ValidDims = []string{"day", "app", "model", "provider"}

// Report groups requests by the chosen columns.
func (s *Store) Report(ctx context.Context, q Query) ([]Row, error) {
	if len(q.By) == 0 {
		return nil, fmt.Errorf("report: choose at least one of %s", strings.Join(ValidDims, ", "))
	}
	seen := map[string]bool{}
	for _, d := range q.By {
		if !slices.Contains(ValidDims, d) {
			return nil, fmt.Errorf("report: can't group by %q (use %s)", d, strings.Join(ValidDims, ", "))
		}
		if seen[d] {
			return nil, fmt.Errorf("report: %q listed twice", d)
		}
		seen[d] = true
	}
	// Column names come from the fixed list above, never from user text.
	cols := strings.Join(q.By, ", ")
	where := "day >= ? AND day <= ?"
	args := []any{q.From, q.To}
	if q.App != "" {
		where += " AND app = ?"
		args = append(args, q.App)
	}
	order := "cost DESC, requests DESC"
	if seen["day"] {
		order = "day DESC, " + order
	}
	for _, d := range q.By {
		if d != "day" {
			order += ", " + d
		}
	}
	// #nosec G202 -- cols and order are built from ValidDims only
	sqlText := `SELECT ` + cols + `, COUNT(*) AS requests,
  SUM(input_tokens + cache_read_tokens + cache_write_tokens), SUM(output_tokens),
  COALESCE(SUM(cost_micros), 0) AS cost, SUM(cost_micros IS NULL), SUM(status >= 400), SUM(masked_total)
  FROM requests WHERE ` + where + ` GROUP BY ` + cols + ` ORDER BY ` + order
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		dest := make([]any, 0, len(q.By)+7)
		for _, d := range q.By {
			switch d {
			case "day":
				dest = append(dest, &r.Day)
			case "app":
				dest = append(dest, &r.App)
			case "model":
				dest = append(dest, &r.Model)
			case "provider":
				dest = append(dest, &r.Provider)
			}
		}
		dest = append(dest, &r.Requests, &r.InputTokens, &r.OutputTokens, &r.CostMicros, &r.Unpriced, &r.Errors, &r.Masked)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnpricedModels lists models with requests but no price in the range.
func (s *Store) UnpricedModels(ctx context.Context, from, to, app string) ([]string, error) {
	where := "day >= ? AND day <= ? AND cost_micros IS NULL AND model != ''"
	args := []any{from, to}
	if app != "" {
		where += " AND app = ?"
		args = append(args, app)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT model FROM requests WHERE `+where, args...) // #nosec G202 -- fixed text
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out, rows.Err()
}
