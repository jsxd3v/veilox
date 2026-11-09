// Package config loads and validates veilox.yaml.
package config

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jsxd3v/veilox/internal/detect"
)

// Config is the whole veilox.yaml.
type Config struct {
	Server    Server              `yaml:"server"`
	Providers map[string]Provider `yaml:"providers"`
	Apps      []App               `yaml:"apps"`
	Masking   Masking             `yaml:"masking"`
	Storage   Storage             `yaml:"storage"`
	Pricing   Pricing             `yaml:"pricing"`
	Log       Log                 `yaml:"log"`

	// Dir is the folder of the config file; relative paths are resolved against it.
	Dir string `yaml:"-"`
	// Names holds names from masking.names and masking.names_file together.
	Names []string `yaml:"-"`
	// Location is server.timezone, loaded.
	Location *time.Location `yaml:"-"`
}

// Server settings.
type Server struct {
	Listen            string        `yaml:"listen"`
	MaxBodyBytes      int64         `yaml:"max_body_bytes"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	UpstreamTimeout   time.Duration `yaml:"upstream_timeout"`
	Timezone          string        `yaml:"timezone"`
}

// Provider is an upstream API.
type Provider struct {
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// App is one caller of veilox.
type App struct {
	Name           string   `yaml:"name"`
	KeySHA256      string   `yaml:"key_sha256"`
	DailyBudgetUSD *float64 `yaml:"daily_budget_usd"`
}

// Masking settings.
type Masking struct {
	Detectors          []string `yaml:"detectors"`
	IPv4               IPv4     `yaml:"ipv4"`
	Names              []string `yaml:"names"`
	NamesFile          string   `yaml:"names_file"`
	Rules              []Rule   `yaml:"rules"`
	UnsupportedContent string   `yaml:"unsupported_content"`
	SystemHint         bool     `yaml:"system_hint"`
}

// IPv4 detector settings.
type IPv4 struct {
	SkipPrivate bool `yaml:"skip_private"`
}

// Rule is a custom regex rule.
type Rule struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
	Group   int    `yaml:"group"`
}

// Storage settings.
type Storage struct {
	Path          string `yaml:"path"`
	RetentionDays int    `yaml:"retention_days"`
}

// Pricing settings.
type Pricing struct {
	File         string `yaml:"file"`
	UnknownModel string `yaml:"unknown_model"`
}

// Log settings.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Provider names.
const (
	OpenAI    = "openai"
	Anthropic = "anthropic"
)

// Defaults returns a config with every default filled in and no apps.
func Defaults() *Config {
	return &Config{
		Server: Server{
			Listen:            ":8080",
			MaxBodyBytes:      10 << 20,
			ReadHeaderTimeout: 10 * time.Second,
			UpstreamTimeout:   10 * time.Minute,
			Timezone:          "UTC",
		},
		Providers: map[string]Provider{
			// APIKeyEnv holds the name of an environment variable, not a key.
			OpenAI:    {BaseURL: "https://api.openai.com", APIKeyEnv: "OPENAI_API_KEY"},       // #nosec G101
			Anthropic: {BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"}, // #nosec G101
		},
		Masking: Masking{UnsupportedContent: "forward"},
		Storage: Storage{Path: "./data/veilox.db", RetentionDays: 90},
		Pricing: Pricing{File: "./prices.yaml", UnknownModel: "allow"},
		Log:     Log{Level: "info", Format: "json"},
	}
}

// Load reads, fills defaults, applies environment overrides and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator chooses the config path
	if err != nil {
		return nil, err
	}
	cfg := Defaults()
	providers := cfg.Providers
	cfg.Providers = nil
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) { // io.EOF: empty file
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Merge provider settings over the defaults, field by field.
	for name, p := range cfg.Providers {
		d, ok := providers[name]
		if !ok {
			return nil, fmt.Errorf("%s: providers: unknown provider %q (known: openai, anthropic)", path, name)
		}
		if p.BaseURL != "" {
			// Accept the SDK-style URL too: veilox adds /v1/... itself.
			d.BaseURL = strings.TrimSuffix(strings.TrimRight(p.BaseURL, "/"), "/v1")
		}
		if p.APIKeyEnv != "" {
			d.APIKeyEnv = p.APIKeyEnv
		}
		providers[name] = d
	}
	cfg.Providers = providers

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.Dir = filepath.Dir(abs)
	if v := os.Getenv("VEILOX_LISTEN"); v != "" {
		cfg.Server.Listen = v
	}
	if v := os.Getenv("VEILOX_DB"); v != "" {
		cfg.Storage.Path = v
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Resolve turns a path from the config into an absolute one, relative to the config's folder.
func (c *Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.Server.MaxBodyBytes <= 0 {
		add("server.max_body_bytes must be positive")
	}
	if c.Server.UpstreamTimeout <= 0 || c.Server.ReadHeaderTimeout <= 0 {
		add("server timeouts must be positive")
	}
	loc, err := time.LoadLocation(c.Server.Timezone)
	if err != nil {
		add("server.timezone: %v", err)
	}
	c.Location = loc

	for name, p := range c.Providers {
		if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
			add("providers.%s.base_url must start with http:// or https://", name)
		}
	}

	names := map[string]bool{}
	hashes := map[string]bool{}
	for i, a := range c.Apps {
		where := fmt.Sprintf("apps[%d]", i)
		if a.Name == "" {
			add("%s: name is required", where)
		} else if names[a.Name] {
			add("%s: app %q is defined twice", where, a.Name)
		}
		names[a.Name] = true
		h := strings.ToLower(a.KeySHA256)
		if b, err := hex.DecodeString(h); err != nil || len(b) != 32 {
			add("%s (%s): key_sha256 must be 64 hex characters (run `veilox keygen %s`)", where, a.Name, a.Name)
		} else if hashes[h] {
			add("%s (%s): key_sha256 is used by another app", where, a.Name)
		}
		hashes[h] = true
		c.Apps[i].KeySHA256 = h
		if a.DailyBudgetUSD != nil && *a.DailyBudgetUSD < 0 {
			add("%s (%s): daily_budget_usd can't be negative", where, a.Name)
		}
	}

	switch c.Masking.UnsupportedContent {
	case "forward", "reject":
	default:
		add("masking.unsupported_content must be forward or reject")
	}
	switch c.Pricing.UnknownModel {
	case "allow", "reject":
	default:
		add("pricing.unknown_model must be allow or reject")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be debug, info, warn or error")
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format must be json or text")
	}
	if c.Storage.RetentionDays < 0 {
		add("storage.retention_days can't be negative (0 keeps everything)")
	}

	c.Names = append([]string(nil), c.Masking.Names...)
	if c.Masking.NamesFile != "" {
		extra, err := readNames(c.Resolve(c.Masking.NamesFile))
		if err != nil {
			add("masking.names_file: %v", err)
		}
		c.Names = append(c.Names, extra...)
	}
	if _, err := c.Engine(); err != nil {
		add("masking: %v", err)
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Engine builds the detection engine from the masking settings.
func (c *Config) Engine() (*detect.Engine, error) {
	rules := make([]detect.Rule, len(c.Masking.Rules))
	for i, r := range c.Masking.Rules {
		rules[i] = detect.Rule{Name: r.Name, Pattern: r.Pattern, Group: r.Group}
	}
	return detect.New(detect.Options{
		Builtins:        c.Masking.Detectors,
		SkipPrivateIPv4: c.Masking.IPv4.SkipPrivate,
		Names:           c.Names,
		Rules:           rules,
	})
}

func readNames(path string) ([]string, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from the operator's config
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}
