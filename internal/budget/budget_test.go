package budget

import (
	"context"
	"errors"
	"testing"
	"time"
)

var ctx = context.Background()

func TestTrackerRollsOverInTimezone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 03:30 UTC on Nov 10 is still Nov 9 in New York.
	now := time.Date(2026, 11, 10, 3, 30, 0, 0, time.UTC)
	loads := []string{}
	tr, err := New(ctx, ny, func() time.Time { return now }, func(_ context.Context, day string) (map[string]int64, error) {
		loads = append(loads, day)
		if day == "2026-11-09" {
			return map[string]int64{"bot": 700}, nil
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Day() != "2026-11-09" {
		t.Fatal(tr.Day())
	}
	if s, _ := tr.Spent(ctx, "bot"); s != 700 {
		t.Fatal(s)
	}
	tr.Add("bot", "2026-11-09", 300)
	tr.Add("bot", "2026-11-08", 999) // old day: ignored
	if s, _ := tr.Spent(ctx, "bot"); s != 1000 {
		t.Fatal(s)
	}
	if got := tr.UntilReset(); got != 90*time.Minute {
		t.Fatal(got)
	}

	now = now.Add(2 * time.Hour) // 00:30 Nov 10 in New York
	if s, _ := tr.Spent(ctx, "bot"); s != 0 {
		t.Fatal("new day should start at zero", s)
	}
	if len(loads) != 2 || loads[1] != "2026-11-10" {
		t.Fatal(loads)
	}
}

func TestLoadError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := New(ctx, time.UTC, nil, func(context.Context, string) (map[string]int64, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	day := 9
	calls := 0
	tr, err := New(ctx, time.UTC, func() time.Time { return time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC) }, func(context.Context, string) (map[string]int64, error) {
		calls++
		if calls > 1 {
			return nil, boom
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	day = 10
	if _, err := tr.Spent(ctx, "x"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
