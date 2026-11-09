package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jsxd3v/veilox/internal/pricing"
)

func ptr(v int64) *int64 { return &v }

var ctx = context.Background()

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "veilox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	ts := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	recs := []Record{
		{Time: ts, Day: "2026-11-09", App: "bot", Provider: "anthropic", Model: "claude-x", Status: 200, Usage: pricing.Usage{Input: 100, CacheRead: 50, CacheWrite: 10, Output: 20}, CostMicros: ptr(1000), Masked: map[string]int{"EMAIL": 2, "NAME": 1}},
		{Time: ts, Day: "2026-11-09", App: "bot", Provider: "anthropic", Model: "claude-x", Status: 200, Stream: true, Usage: pricing.Usage{Input: 40, Output: 10}, CostMicros: ptr(500), Masked: map[string]int{"PHONE": 1}},
		{Time: ts, Day: "2026-11-09", App: "crm", Provider: "openai", Model: "gpt-x", Status: 200, Usage: pricing.Usage{Input: 10, Output: 5}, CostMicros: ptr(3000)},
		{Time: ts, Day: "2026-11-09", App: "crm", Provider: "openai", Model: "local-x", Status: 200, Usage: pricing.Usage{Input: 7, Output: 3}},
		{Time: ts.Add(-24 * time.Hour), Day: "2026-11-08", App: "bot", Provider: "anthropic", Model: "claude-x", Status: 500, CostMicros: ptr(0), ErrorType: "api_error"},
		{Time: ts.Add(-100 * 24 * time.Hour), Day: "2026-08-01", App: "bot", Provider: "anthropic", Model: "claude-x", Status: 200, CostMicros: ptr(99)},
	}
	for _, r := range recs {
		if err := s.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpendAndReport(t *testing.T) {
	s := open(t)
	seed(t, s)
	spend, err := s.SpendByApp(ctx, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spend, map[string]int64{"bot": 1500, "crm": 3000}) {
		t.Fatal(spend)
	}

	rows, err := s.Report(ctx, Query{From: "2026-11-01", To: "2026-11-09", By: []string{"day", "app"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Day: "2026-11-09", App: "crm", Requests: 2, InputTokens: 17, OutputTokens: 8, CostMicros: 3000, Unpriced: 1},
		{Day: "2026-11-09", App: "bot", Requests: 2, InputTokens: 200, OutputTokens: 30, CostMicros: 1500, Masked: 4},
		{Day: "2026-11-08", App: "bot", Requests: 1, Errors: 1},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("got  %+v\nwant %+v", rows, want)
	}

	rows, err = s.Report(ctx, Query{From: "2026-01-01", To: "2026-12-31", App: "bot", By: []string{"model", "provider"}})
	if err != nil || len(rows) != 1 || rows[0].Requests != 4 || rows[0].Model != "claude-x" || rows[0].Provider != "anthropic" || rows[0].CostMicros != 1599 {
		t.Fatalf("%+v %v", rows, err)
	}

	models, err := s.UnpricedModels(ctx, "2026-11-01", "2026-11-09", "")
	if err != nil || !reflect.DeepEqual(models, []string{"local-x"}) {
		t.Fatal(models, err)
	}
	if models, _ := s.UnpricedModels(ctx, "2026-11-01", "2026-11-09", "bot"); len(models) != 0 {
		t.Fatal(models)
	}
}

func TestReportErrors(t *testing.T) {
	s := open(t)
	for _, by := range [][]string{nil, {"bogus"}, {"app", "app"}, {"app; DROP TABLE requests"}} {
		if _, err := s.Report(ctx, Query{From: "a", To: "b", By: by}); err == nil {
			t.Errorf("%v: expected error", by)
		}
	}
}

func TestPrune(t *testing.T) {
	s := open(t)
	seed(t, s)
	n, err := s.Prune(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestWriterAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	s.StartWriter(func(err error) { errs = append(errs, err) })
	for i := range 50 {
		s.Add(Record{Time: time.Now(), Day: "2026-11-09", App: "a", Provider: "openai", Model: "m", Status: 200, CostMicros: ptr(int64(i))})
	}
	s.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	s, err = Open(path) // migrations are idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	spend, _ := s.SpendByApp(ctx, "2026-11-09")
	if spend["a"] != 49*50/2 {
		t.Fatal(spend)
	}
}

func TestAddAfterClose(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	var got []error
	s.StartWriter(func(err error) { got = append(got, err) })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.Add(Record{App: "late"}) // must not panic
	if err := s.Close(); err != nil {
		t.Fatal("second Close should be a no-op")
	}
	if len(got) != 1 || !errors.Is(got[0], ErrClosed) {
		t.Fatal(got)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected error for a newer schema")
	}
}

func TestInsertErrorReported(t *testing.T) {
	s := open(t)
	var got error
	s.errFn = func(err error) { got = err }
	_ = s.db.Close()
	s.Add(Record{})
	if got == nil || errors.Is(got, nil) {
		t.Fatal("expected an error from a closed database")
	}
}

func TestURIPath(t *testing.T) {
	if got := uriPath(filepath.Join("data", "my db#1.db")); got != "data/my%20db%231.db" {
		t.Fatal(got)
	}
	abs, _ := filepath.Abs("x.db")
	if got := uriPath(abs); !strings.HasPrefix(got, "/") || strings.Contains(got, `\`) {
		t.Fatal(got)
	}
}
