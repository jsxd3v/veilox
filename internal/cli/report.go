package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/store"
)

var now = time.Now // tests replace it

func runReport(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("report", stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "path to veilox.yaml (for the database path and timezone)")
	dbPath := fs.String("db", "", "database file (default: storage.path from the config)")
	since := fs.String("since", "7d", "first day: Nd (last N days, today included), YYYY-MM-DD, or all")
	until := fs.String("until", "", "last day, YYYY-MM-DD (default: today)")
	app := fs.String("app", "", "only this app")
	by := fs.String("by", "day,app,model", "group by any of day, app, model, provider")
	format := fs.String("format", "table", "table, csv or json")
	if _, err := parse(fs, args); err != nil {
		return err
	}

	loc := time.UTC
	path := *dbPath
	cfg, err := config.Load(*cfgPath)
	switch {
	case err == nil:
		loc = cfg.Location
		if path == "" {
			path = cfg.Resolve(cfg.Storage.Path)
		}
	case path == "" || !errors.Is(err, os.ErrNotExist):
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no database at %s yet (it is created by `veilox serve`)", path)
	}

	today := now().In(loc).Format("2006-01-02")
	from, err := parseSince(*since, now().In(loc))
	if err != nil {
		return err
	}
	to := today
	if *until != "" {
		if !dayRe.MatchString(*until) {
			return fmt.Errorf("--until must be YYYY-MM-DD")
		}
		to = *until
	}

	ctx := context.Background()
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	dims := splitList(*by)
	rows, err := st.Report(ctx, store.Query{From: from, To: to, App: *app, By: dims})
	if err != nil {
		return err
	}
	unpriced, err := st.UnpricedModels(ctx, from, to, *app)
	if err != nil {
		return err
	}

	switch *format {
	case "table":
		printReportTable(stdout, dims, rows, unpriced, from, to)
	case "csv":
		return writeReportCSV(stdout, dims, rows)
	case "json":
		return writeReportJSON(stdout, dims, rows)
	default:
		return fmt.Errorf("--format must be table, csv or json")
	}
	return nil
}

var dayRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func parseSince(s string, today time.Time) (string, error) {
	switch {
	case s == "all":
		return "0000-01-01", nil
	case dayRe.MatchString(s):
		return s, nil
	case strings.HasSuffix(s, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil && n >= 1 {
			return today.AddDate(0, 0, -(n - 1)).Format("2006-01-02"), nil
		}
	}
	return "", fmt.Errorf("--since must be like 7d, 2026-11-01 or all")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dimValue(r store.Row, d string) string {
	switch d {
	case "day":
		return r.Day
	case "app":
		return r.App
	case "model":
		return r.Model
	default:
		return r.Provider
	}
}

func printReportTable(w io.Writer, dims []string, rows []store.Row, unpriced []string, from, to string) {
	if len(rows) == 0 {
		fmt.Fprintf(w, "No requests between %s and %s.\n", from, to)
		return
	}
	t := &table{}
	head := make([]string, 0, len(dims)+5)
	for _, d := range dims {
		head = append(head, strings.ToUpper(d))
	}
	t.header(append(head, "REQUESTS", "ERRORS", "INPUT TOK", "OUTPUT TOK", "COST", "MASKED"), len(dims))
	var total store.Row
	for _, r := range rows {
		cells := make([]string, 0, len(dims)+5)
		for _, d := range dims {
			cells = append(cells, dimValue(r, d))
		}
		t.row(append(cells, num(r.Requests), num(r.Errors), num(r.InputTokens), num(r.OutputTokens), money(r.CostMicros, r.Unpriced), num(r.Masked)))
		total.Requests += r.Requests
		total.InputTokens += r.InputTokens
		total.OutputTokens += r.OutputTokens
		total.CostMicros += r.CostMicros
		total.Unpriced += r.Unpriced
		total.Errors += r.Errors
		total.Masked += r.Masked
	}
	cells := make([]string, len(dims))
	cells[0] = "TOTAL"
	t.row(append(cells, num(total.Requests), num(total.Errors), num(total.InputTokens), num(total.OutputTokens), money(total.CostMicros, total.Unpriced), num(total.Masked)))
	t.write(w)
	if total.Unpriced > 0 {
		fmt.Fprintf(w, "\n* %s requests to models without a price, not included in COST: %s\n  Add them to prices.yaml.\n",
			num(total.Unpriced), strings.Join(unpriced, ", "))
	}
}

func money(micros, unpriced int64) string {
	var s string
	switch {
	case micros == 0:
		s = "$0.00"
	case micros < 10_000:
		s = "<$0.01"
	default:
		s = "$" + commas(fmt.Sprintf("%.2f", float64(micros)/1e6))
	}
	if unpriced > 0 {
		s += "*"
	}
	return s
}

func num(n int64) string { return commas(strconv.FormatInt(n, 10)) }

// commas adds thousands separators to a plain number like 1234567.89.
func commas(s string) string {
	intPart, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if hasFrac {
		b.WriteString("." + frac)
	}
	return b.String()
}

func writeReportCSV(w io.Writer, dims []string, rows []store.Row) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(append(append([]string{}, dims...), "requests", "errors", "input_tokens", "output_tokens", "cost_usd", "unpriced_requests", "masked"))
	for _, r := range rows {
		rec := make([]string, 0, len(dims)+6)
		for _, d := range dims {
			rec = append(rec, dimValue(r, d))
		}
		rec = append(rec, strconv.FormatInt(r.Requests, 10), strconv.FormatInt(r.Errors, 10), strconv.FormatInt(r.InputTokens, 10), strconv.FormatInt(r.OutputTokens, 10),
			strconv.FormatFloat(float64(r.CostMicros)/1e6, 'f', 6, 64), strconv.FormatInt(r.Unpriced, 10), strconv.FormatInt(r.Masked, 10))
		_ = cw.Write(rec)
	}
	cw.Flush()
	return cw.Error()
}

func writeReportJSON(w io.Writer, dims []string, rows []store.Row) error {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := map[string]any{
			"requests": r.Requests, "errors": r.Errors, "input_tokens": r.InputTokens, "output_tokens": r.OutputTokens,
			"cost_usd": float64(r.CostMicros) / 1e6, "unpriced_requests": r.Unpriced, "masked": r.Masked,
		}
		for _, d := range dims {
			m[d] = dimValue(r, d)
		}
		out = append(out, m)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// table prints aligned columns: the first `left` columns left-aligned, the rest right-aligned.
type table struct {
	rows [][]string
	left int
}

func (t *table) header(cells []string, left int) {
	t.left = left
	t.rows = append(t.rows, cells)
}

func (t *table) row(cells []string) { t.rows = append(t.rows, cells) }

func (t *table) write(w io.Writer) {
	widths := make([]int, len(t.rows[0]))
	for _, r := range t.rows {
		for i, c := range r {
			widths[i] = max(widths[i], len([]rune(c)))
		}
	}
	for _, r := range t.rows {
		var b strings.Builder
		for i, c := range r {
			pad := strings.Repeat(" ", widths[i]-len([]rune(c)))
			if i > 0 {
				b.WriteString("  ")
			}
			if i < t.left {
				b.WriteString(c + pad)
			} else {
				b.WriteString(pad + c)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}
