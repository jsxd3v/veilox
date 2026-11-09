// Package budget tracks each app's spend for the current day in memory.
package budget

import (
	"context"
	"sync"
	"time"
)

// Tracker holds today's spend per app, in micro-dollars.
type Tracker struct {
	mu    sync.Mutex
	loc   *time.Location
	now   func() time.Time
	load  func(ctx context.Context, day string) (map[string]int64, error)
	day   string
	spent map[string]int64
}

// New creates a tracker. load returns the spend already recorded for a day (from the database); it
// is called when the tracker starts and whenever the day changes.
func New(ctx context.Context, loc *time.Location, now func() time.Time, load func(ctx context.Context, day string) (map[string]int64, error)) (*Tracker, error) {
	if now == nil {
		now = time.Now
	}
	t := &Tracker{loc: loc, now: now, load: load}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t, t.rollLocked(ctx)
}

// Day returns today's date (YYYY-MM-DD) in the tracker's timezone.
func (t *Tracker) Day() string { return t.now().In(t.loc).Format("2006-01-02") }

func (t *Tracker) rollLocked(ctx context.Context) error {
	day := t.Day()
	if day == t.day {
		return nil
	}
	spent := map[string]int64{}
	if t.load != nil {
		m, err := t.load(ctx, day)
		if err != nil {
			return err
		}
		for k, v := range m {
			spent[k] = v
		}
	}
	t.day, t.spent = day, spent
	return nil
}

// Spent returns the app's spend today.
func (t *Tracker) Spent(ctx context.Context, app string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.rollLocked(ctx); err != nil {
		return 0, err
	}
	return t.spent[app], nil
}

// Add records spend for an app on a day. Spend for a day that already ended is ignored.
func (t *Tracker) Add(app, day string, micros int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if day == t.day {
		t.spent[app] += micros
	}
}

// UntilReset is the time left until the next day starts.
func (t *Tracker) UntilReset() time.Duration {
	now := t.now().In(t.loc)
	y, m, d := now.Date()
	next := time.Date(y, m, d+1, 0, 0, 0, 0, t.loc)
	return next.Sub(now)
}
