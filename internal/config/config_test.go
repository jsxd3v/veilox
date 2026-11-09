package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const hashA = "3b1f0d6a9c2e4f5a6b7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c"
const hashB = "9ac40d6a9c2e4f5a6b7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c"

func load(t *testing.T, yaml string, files map[string]string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "veilox.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestDefaults(t *testing.T) {
	cfg, err := load(t, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.MaxBodyBytes != 10<<20 || cfg.Server.UpstreamTimeout != 10*time.Minute {
		t.Fatalf("%+v", cfg.Server)
	}
	if cfg.Providers[OpenAI].BaseURL != "https://api.openai.com" || cfg.Providers[Anthropic].APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("%+v", cfg.Providers)
	}
	if cfg.Location != time.UTC || cfg.Masking.UnsupportedContent != "forward" || cfg.Pricing.UnknownModel != "allow" {
		t.Fatal("defaults")
	}
	if cfg.Resolve("prices.yaml") != filepath.Join(cfg.Dir, "prices.yaml") || cfg.Resolve(cfg.Dir) != cfg.Dir || cfg.Resolve("") != "" {
		t.Fatal("resolve")
	}
}

func TestFullConfig(t *testing.T) {
	cfg, err := load(t, `
server:
  listen: "127.0.0.1:9000"
  upstream_timeout: 30s
  timezone: Europe/Lisbon
providers:
  openai: { base_url: "https://openrouter.ai/api/v1/" }
  anthropic: { api_key_env: MY_KEY }
apps:
  - name: support-bot
    key_sha256: `+strings.ToUpper(hashA)+`
    daily_budget_usd: 5
  - name: crm
    key_sha256: `+hashB+`
masking:
  detectors: [email, iban]
  ipv4: { skip_private: true }
  names: ["Maria Santos"]
  names_file: names.txt
  rules:
    - { name: CUSTOMER_ID, pattern: 'CUS-[0-9]{6}' }
  unsupported_content: reject
  system_hint: true
storage: { path: data/v.db, retention_days: 30 }
pricing: { file: p.yaml, unknown_model: reject }
log: { level: debug, format: text }
`, map[string]string{"names.txt": "# CRM export\nJohn O'Neill\n\n  Ana Lima  \n"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[OpenAI].BaseURL != "https://openrouter.ai/api" {
		t.Fatal(cfg.Providers[OpenAI].BaseURL)
	}
	if cfg.Providers[Anthropic].BaseURL != "https://api.anthropic.com" || cfg.Providers[Anthropic].APIKeyEnv != "MY_KEY" {
		t.Fatal(cfg.Providers[Anthropic])
	}
	if cfg.Apps[0].KeySHA256 != hashA || *cfg.Apps[0].DailyBudgetUSD != 5 || cfg.Apps[1].DailyBudgetUSD != nil {
		t.Fatalf("%+v", cfg.Apps)
	}
	if strings.Join(cfg.Names, "|") != "Maria Santos|John O'Neill|Ana Lima" {
		t.Fatal(cfg.Names)
	}
	if cfg.Location.String() != "Europe/Lisbon" || cfg.Server.UpstreamTimeout != 30*time.Second {
		t.Fatal(cfg.Location)
	}
	e, err := cfg.Engine()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.Types(), ","); got != "IBAN,EMAIL,CUSTOMER_ID,NAME" {
		t.Fatal(got)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("VEILOX_LISTEN", ":7000")
	t.Setenv("VEILOX_DB", "/tmp/x.db")
	cfg, err := load(t, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":7000" || cfg.Storage.Path != "/tmp/x.db" {
		t.Fatal(cfg.Server.Listen, cfg.Storage.Path)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		"server:\n  listne: x\n":                                                                      "listne",
		"providers:\n  gemini: { base_url: https://x }\n":                                             "unknown provider",
		"providers:\n  openai: { base_url: ftp://x }\n":                                               "base_url",
		"apps:\n  - name: a\n    key_sha256: abc\n":                                                   "64 hex",
		"apps:\n  - key_sha256: " + hashA + "\n":                                                      "name is required",
		"apps:\n  - {name: a, key_sha256: " + hashA + "}\n  - {name: a, key_sha256: " + hashB + "}\n": "defined twice",
		"apps:\n  - {name: a, key_sha256: " + hashA + "}\n  - {name: b, key_sha256: " + hashA + "}\n": "used by another app",
		"apps:\n  - {name: a, key_sha256: " + hashA + ", daily_budget_usd: -1}\n":                     "negative",
		"masking:\n  unsupported_content: drop\n":                                                     "forward or reject",
		"masking:\n  detectors: [ssn]\n":                                                              "unknown detector",
		"masking:\n  rules: [{name: X, pattern: '('}]\n":                                              "invalid pattern",
		"masking:\n  names_file: nope.txt\n":                                                          "names_file",
		"pricing:\n  unknown_model: maybe\n":                                                          "allow or reject",
		"log:\n  level: loud\n":                                                                       "log.level",
		"log:\n  format: xml\n":                                                                       "log.format",
		"server:\n  timezone: Mars/Base\n":                                                            "timezone",
		"server:\n  max_body_bytes: 0\n":                                                              "max_body_bytes",
		"server:\n  upstream_timeout: 0s\n":                                                           "timeouts",
		"storage:\n  retention_days: -1\n":                                                            "retention_days",
		"server: [":                                                                                   "yaml",
	}
	for yaml, want := range cases {
		_, err := load(t, yaml, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want error containing %q", yaml, err, want)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file should fail")
	}
}
