// Package proxy is the HTTP gateway: it authenticates apps, enforces budgets, masks requests,
// forwards them to the provider and restores the real values in the answer.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsxd3v/veilox/internal/auth"
	"github.com/jsxd3v/veilox/internal/budget"
	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/detect"
	"github.com/jsxd3v/veilox/internal/mask"
	"github.com/jsxd3v/veilox/internal/pricing"
	"github.com/jsxd3v/veilox/internal/store"
)

// maxResponseBytes caps non-streaming provider responses.
const maxResponseBytes = 32 << 20

// Hint is added to the system prompt when masking.system_hint is on.
const Hint = "Some values in this conversation were replaced with placeholders such as <NAME_1> or <EMAIL_1>. " +
	"When you refer to them, write each placeholder exactly as it appears."

// adapter is what differs between the OpenAI and Anthropic APIs.
type adapter interface {
	name() string
	path() string
	setAuth(h http.Header, key string)
	walk(body map[string]any, f func(string) string) (unsupported []string)
	addHint(body map[string]any, hint string)
	prepareStream(body map[string]any) (injectedUsage bool)
	restore(body map[string]any, m *mask.Mapper)
	usage(body map[string]any) (model string, u pricing.Usage, ok bool)
	errorType(body map[string]any) string
	errorBody(status int, code, msg string) any
	newStream(m *mask.Mapper, injectedUsage bool) streamer
}

// streamer rewrites one SSE response.
type streamer interface {
	event(ev *event) []*event
	end() []*event
	usage() (model string, u pricing.Usage, ok bool)
}

// Runtime is everything that SIGHUP can reload.
type Runtime struct {
	Cfg     *config.Config
	Engine  *detect.Engine
	Keys    *auth.Keyring
	Prices  *pricing.Table
	Budgets map[string]int64 // app -> daily budget in micro-dollars (only apps with a budget)
}

// NewRuntime builds a Runtime from a loaded config.
func NewRuntime(cfg *config.Config) (*Runtime, error) {
	eng, err := cfg.Engine()
	if err != nil {
		return nil, err
	}
	hashes := map[string]string{}
	budgets := map[string]int64{}
	for _, a := range cfg.Apps {
		hashes[a.Name] = a.KeySHA256
		if a.DailyBudgetUSD != nil {
			budgets[a.Name] = int64(math.Round(*a.DailyBudgetUSD * 1e6))
		}
	}
	keys, err := auth.NewKeyring(hashes)
	if err != nil {
		return nil, err
	}
	prices, err := pricing.Load(cfg.Resolve(cfg.Pricing.File))
	if err != nil {
		return nil, fmt.Errorf("pricing file: %w", err)
	}
	return &Runtime{Cfg: cfg, Engine: eng, Keys: keys, Prices: prices, Budgets: budgets}, nil
}

// Server is the gateway.
type Server struct {
	rt      atomic.Pointer[Runtime]
	store   *store.Store
	budgets *budget.Tracker
	log     *slog.Logger
	client  *http.Client
	now     func() time.Time
	getenv  func(string) string
	// bodyTimeout limits how long reading a request body may take (slow uploads). It is set on the
	// connection for the body only and cleared before the answer is relayed.
	bodyTimeout time.Duration
	warned      sync.Map // "day|model" for unpriced-model warnings
}

// New creates a Server. st may be nil (nothing is recorded); bt may be nil (no budgets).
func New(rt *Runtime, st *store.Store, bt *budget.Tracker, log *slog.Logger) *Server {
	s := &Server{
		store:   st,
		budgets: bt,
		log:     log,
		client:  &http.Client{Transport: newTransport()},
		now:     time.Now,
		getenv:  os.Getenv,

		bodyTimeout: 2 * time.Minute,
	}
	s.rt.Store(rt)
	return s
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 0 // models can think for a while; the request timeout covers it
	t.MaxIdleConnsPerHost = 32
	return t
}

// Reload swaps in a new runtime (config, keys, detectors, prices).
func (s *Server) Reload(rt *Runtime) { s.rt.Store(rt) }

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	for _, a := range []adapter{openAI{}, anthropic{}} {
		mux.Handle(a.path(), s.handle(a))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, openAI{}, http.StatusNotFound, "not_found",
			fmt.Sprintf("veilox: %s %s is not supported; use POST /v1/chat/completions (OpenAI) or POST /v1/messages (Anthropic)", r.Method, r.URL.Path))
	})
	return s.recoverPanics(mux)
}

// recoverPanics turns a panic into a 500 without logging any request data.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(v)
				}
				s.log.Error("internal error", "request_id", w.Header().Get("X-Veilox-Request-Id"), "path", r.URL.Path)
				http.Error(w, "veilox: internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) fail(w http.ResponseWriter, a adapter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encodeJSON(a.errorBody(status, code, msg)))
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Headers passed from the app to the provider. Everything else (cookies, the app's key, hop-by-hop
// headers, Accept-Encoding) stays behind.
var forwardHeaders = []string{"Accept", "anthropic-version", "anthropic-beta", "OpenAI-Organization", "OpenAI-Project", "User-Agent"}

// Response headers that veilox sets itself or that no longer apply after rewriting the body.
var dropResponseHeaders = map[string]bool{
	"Content-Length": true, "Content-Encoding": true, "Transfer-Encoding": true, "Connection": true,
	"Keep-Alive": true, "Proxy-Connection": true, "Trailer": true, "Upgrade": true, "Set-Cookie": true,
}

func (s *Server) handle(a adapter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rt := s.rt.Load()
		reqID := newRequestID()
		w.Header().Set("X-Veilox-Request-Id", reqID)

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			s.fail(w, a, http.StatusMethodNotAllowed, "method_not_allowed", "veilox: use POST")
			return
		}
		app, ok := rt.Keys.Lookup(auth.FromRequest(r))
		if !ok {
			s.log.Warn("unknown app key", "request_id", reqID, "provider", a.name())
			s.fail(w, a, http.StatusUnauthorized, "invalid_api_key", "veilox: missing or unknown app key")
			return
		}

		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(s.now().Add(s.bodyTimeout))
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rt.Cfg.Server.MaxBodyBytes))
		_ = rc.SetReadDeadline(time.Time{})
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				s.fail(w, a, http.StatusRequestEntityTooLarge, "request_too_large",
					fmt.Sprintf("veilox: request body is larger than %d bytes (server.max_body_bytes)", tooBig.Limit))
				return
			}
			s.fail(w, a, http.StatusBadRequest, "invalid_request", "veilox: could not read the request body")
			return
		}

		day := s.dayOf(start)
		if limit, has := rt.Budgets[app]; has && s.budgets != nil {
			spent, err := s.budgets.Spent(r.Context(), app)
			if err != nil {
				s.log.Error("budget state unavailable", "request_id", reqID, "app", app, "error", err)
				s.fail(w, a, http.StatusServiceUnavailable, "budget_unavailable", "veilox: budget state unavailable, try again")
				return
			}
			if spent >= limit {
				wait := s.budgets.UntilReset()
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
				s.log.Warn("budget exceeded", "request_id", reqID, "app", app, "spent_usd", usd(spent), "budget_usd", usd(limit))
				s.fail(w, a, http.StatusTooManyRequests, "veilox_budget_exceeded",
					fmt.Sprintf("veilox: daily budget of $%.2f for app %q reached; resets at 00:00 %s", usd(limit), app, rt.Cfg.Server.Timezone))
				return
			}
		}

		body, err := decodeObject(raw)
		if err != nil {
			s.fail(w, a, http.StatusBadRequest, "invalid_json", "veilox: request body is not a valid JSON object; nothing was sent")
			return
		}
		reqModel, _ := asString(body["model"])
		stream, _ := body["stream"].(bool)
		if rt.Cfg.Pricing.UnknownModel == "reject" {
			if _, priced := rt.Prices.Lookup(reqModel); !priced {
				s.fail(w, a, http.StatusBadRequest, "model_not_priced",
					fmt.Sprintf("veilox: model %q has no price in the price table (pricing.unknown_model: reject)", reqModel))
				return
			}
		}

		// Mask: first reserve placeholder-like text already in the request, then replace values.
		m := mask.NewMapper(rt.Engine)
		a.walk(body, m.Reserve)
		unsupported := a.walk(body, m.Mask)
		if len(unsupported) > 0 && rt.Cfg.Masking.UnsupportedContent == "reject" {
			s.fail(w, a, http.StatusBadRequest, "unsupported_content",
				fmt.Sprintf("veilox: the request contains %s content, which can't be masked (masking.unsupported_content: reject)", strings.Join(unique(unsupported), ", ")))
			return
		}
		if rt.Cfg.Masking.SystemHint && m.Issued() {
			a.addHint(body, Hint)
		}
		injected := false
		if stream {
			injected = a.prepareStream(body)
		}
		out := encodeJSON(body)

		rec := store.Record{
			Time: start, Day: day, App: app, Provider: a.name(), Model: reqModel, Stream: stream,
			Masked: m.Counts(), UnmaskedParts: len(unsupported),
		}
		defer func() { s.finish(rt, reqID, &rec, start, reqModel) }()

		prov := rt.Cfg.Providers[a.name()]
		key := s.getenv(prov.APIKeyEnv)
		if key == "" {
			rec.Status, rec.ErrorType = http.StatusBadGateway, "provider_key_missing"
			s.fail(w, a, http.StatusBadGateway, "provider_key_missing",
				fmt.Sprintf("veilox: no %s key configured (set %s in veilox's environment)", a.name(), prov.APIKeyEnv))
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), rt.Cfg.Server.UpstreamTimeout)
		defer cancel()
		up, err := http.NewRequestWithContext(ctx, http.MethodPost, prov.BaseURL+a.path(), bytes.NewReader(out))
		if err != nil {
			rec.Status, rec.ErrorType = http.StatusBadGateway, "bad_upstream_url"
			s.fail(w, a, http.StatusBadGateway, "bad_upstream_url", "veilox: invalid provider base_url")
			return
		}
		for _, h := range forwardHeaders {
			if v := r.Header.Values(h); len(v) > 0 {
				up.Header[http.CanonicalHeaderKey(h)] = v
			}
		}
		up.Header.Set("Content-Type", "application/json")
		a.setAuth(up.Header, key)

		resp, err := s.client.Do(up)
		if err != nil {
			switch {
			case r.Context().Err() != nil:
				rec.Status, rec.ErrorType = 499, "client_closed"
			case errors.Is(err, context.DeadlineExceeded):
				rec.Status, rec.ErrorType = http.StatusGatewayTimeout, "upstream_timeout"
				s.fail(w, a, http.StatusGatewayTimeout, "upstream_timeout", "veilox: the provider did not answer in time (server.upstream_timeout)")
			default:
				rec.Status, rec.ErrorType = http.StatusBadGateway, "upstream_unreachable"
				s.fail(w, a, http.StatusBadGateway, "upstream_unreachable", "veilox: could not reach the provider")
			}
			return
		}
		defer resp.Body.Close()
		rec.TTFBMS = s.now().Sub(start).Milliseconds()
		rec.Status = resp.StatusCode

		for k, v := range resp.Header {
			if !dropResponseHeaders[http.CanonicalHeaderKey(k)] {
				w.Header()[k] = v
			}
		}
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			s.relayStream(w, r, resp, a, m, injected, &rec)
		} else {
			s.relayJSON(w, resp, a, m, &rec)
		}
	})
}

func (s *Server) relayJSON(w http.ResponseWriter, resp *http.Response, a adapter, m *mask.Mapper, rec *store.Record) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		rec.Status, rec.ErrorType = http.StatusBadGateway, "upstream_read_error"
		w.Header().Del("Content-Type")
		s.fail(w, a, http.StatusBadGateway, "upstream_read_error", "veilox: could not read the provider's response")
		return
	}
	body, err := decodeObject(data)
	if err != nil {
		// Not JSON (e.g. an HTML error page from a load balancer): restore as plain text.
		w.WriteHeader(resp.StatusCode)
		_, _ = io.WriteString(w, m.Restore(string(data)))
		if resp.StatusCode >= 400 {
			rec.ErrorType = "upstream_error"
		}
		return
	}
	if model, u, ok := a.usage(body); ok {
		rec.Usage = u
		if model != "" {
			rec.Model = model
		}
	}
	if resp.StatusCode >= 400 {
		rec.ErrorType = a.errorType(body)
		if rec.ErrorType == "" {
			rec.ErrorType = "upstream_error"
		}
	}
	a.restore(body, m)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(encodeJSON(body))
}

func (s *Server) relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, a adapter, m *mask.Mapper, injected bool, rec *store.Record) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // tell nginx not to buffer the stream
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	st := a.newStream(m, injected)
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	write := func(evs []*event) bool {
		for _, ev := range evs {
			if _, err := w.Write(ev.bytes()); err != nil {
				return false
			}
		}
		return rc.Flush() == nil || len(evs) == 0
	}
	clientGone := false
	for {
		ev, err := readEvent(br)
		if ev != nil && !write(st.event(ev)) {
			clientGone = true
			break
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				if r.Context().Err() != nil {
					clientGone = true
				} else {
					rec.ErrorType = "stream_interrupted"
				}
			}
			break
		}
	}
	if !clientGone {
		clientGone = !write(st.end())
	}
	if clientGone || r.Context().Err() != nil {
		rec.Status, rec.ErrorType = 499, "client_closed"
	}
	if model, u, ok := st.usage(); ok {
		rec.Usage = u
		if model != "" {
			rec.Model = model
		}
	}
}

// finish prices the request, updates the budget, stores the record and writes the log line.
func (s *Server) finish(rt *Runtime, reqID string, rec *store.Record, start time.Time, reqModel string) {
	rec.LatencyMS = s.now().Sub(start).Milliseconds()
	u := rec.Usage
	if u.Input+u.Output+u.CacheRead+u.CacheWrite == 0 {
		zero := int64(0)
		rec.CostMicros = &zero
	} else if p, ok := rt.Prices.Lookup(rec.Model); ok {
		c := p.Cost(u)
		rec.CostMicros = &c
	} else if p, ok := rt.Prices.Lookup(reqModel); ok { // e.g. a dated model name the table doesn't list
		c := p.Cost(u)
		rec.CostMicros = &c
	} else if _, seen := s.warned.LoadOrStore(rec.Day+"|"+rec.Model, true); !seen {
		s.log.Warn("model has no price; its cost is not counted toward budgets", "model", rec.Model, "pricing_file", rt.Cfg.Pricing.File)
	}
	if rec.CostMicros != nil && s.budgets != nil {
		s.budgets.Add(rec.App, rec.Day, *rec.CostMicros)
	}
	if s.store != nil {
		s.store.Add(*rec)
	}

	attrs := []any{
		"request_id", reqID, "app", rec.App, "provider", rec.Provider, "model", rec.Model,
		"status", rec.Status, "stream", rec.Stream, "latency_ms", rec.LatencyMS, "ttfb_ms", rec.TTFBMS,
		"input_tokens", u.Input + u.CacheRead + u.CacheWrite, "output_tokens", u.Output,
	}
	if rec.CostMicros != nil {
		attrs = append(attrs, "cost_usd", usd(*rec.CostMicros))
	}
	if len(rec.Masked) > 0 {
		attrs = append(attrs, "masked", rec.Masked)
	}
	if rec.UnmaskedParts > 0 {
		attrs = append(attrs, "unmasked_parts", rec.UnmaskedParts)
	}
	if rec.ErrorType != "" {
		attrs = append(attrs, "error_type", rec.ErrorType)
	}
	s.log.Info("request", attrs...)
}

func (s *Server) dayOf(t time.Time) string {
	if s.budgets != nil {
		return s.budgets.Day()
	}
	return t.In(s.rt.Load().Cfg.Location).Format("2006-01-02")
}

func usd(micros int64) float64 { return float64(micros) / 1e6 }

func unique(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		if v == "" {
			v = "untyped"
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
