package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsxd3v/veilox/internal/auth"
	"github.com/jsxd3v/veilox/internal/config"
)

// examplePrices is written by `veilox setup`. It must stay equal to examples/prices.yaml (tested).
const examplePrices = `# EXAMPLE VALUES ONLY. Check current pricing on your provider's pricing page
# before relying on these numbers. Prices change, and veilox can't know them.
#
# All prices are USD per 1 million tokens.
# A name ending in * matches every model that starts with it (dated versions).
# cached_input / cache_read: price of cached prompt tokens (defaults to input).
# cache_write: price of writing to the prompt cache (defaults to input).
currency: USD
models:
  gpt-6.1-terra:
    input: 1.00
    cached_input: 0.25
    output: 4.00
  gpt-6-luna:
    input: 0.20
    cached_input: 0.05
    output: 0.80
  "claude-sonnet-5-5*":
    input: 1.00
    cache_write: 1.25
    cache_read: 0.10
    output: 5.00
  "claude-haiku-5-5*":
    input: 0.25
    cache_write: 0.31
    cache_read: 0.03
    output: 1.25
`

const envTemplate = `# veilox settings. Keep this file private: never commit it.

# Your provider key: fill in the one you use
OPENAI_API_KEY=
ANTHROPIC_API_KEY=

# The key your apps use to call veilox (created by ` + "`veilox setup`" + `)
`

// runSetup prepares a folder to run veilox from .env alone: it writes .env with a new VEILOX_KEY
// (keeping an existing one) and an example prices.yaml, and prints what to do next.
func runSetup(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("setup", stderr)
	dir := fs.String("dir", ".", "folder to set up")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		fmt.Fprintln(stderr, "usage: veilox setup [app-name] [--dir folder]")
		return errUsage
	}
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		return err
	}

	envPath := filepath.Join(*dir, ".env")
	lines, err := parseDotEnv(envPath)
	if err != nil {
		return err
	}
	have := map[string]string{}
	for _, l := range lines {
		if have[l.key] == "" {
			have[l.key] = l.value
		}
	}
	_, statErr := os.Stat(envPath)
	envExisted := statErr == nil

	var add strings.Builder
	if !envExisted {
		add.WriteString(envTemplate)
	} else if _, ok := have["OPENAI_API_KEY"]; !ok {
		if _, ok := have["ANTHROPIC_API_KEY"]; !ok {
			add.WriteString("\n# Your provider key: fill in the one you use\nOPENAI_API_KEY=\nANTHROPIC_API_KEY=\n")
		}
	}
	key := have["VEILOX_KEY"]
	newKey := key == ""
	if newKey {
		if key, err = auth.NewKey(); err != nil {
			return err
		}
		if envExisted {
			add.WriteString("\n# The key your apps use to call veilox (created by `veilox setup`)\n")
		}
		add.WriteString("VEILOX_KEY=" + key + "\n")
	}
	if len(pos) == 1 && have["VEILOX_APP"] == "" {
		add.WriteString("VEILOX_APP=" + pos[0] + "\n")
	}
	if add.Len() > 0 {
		if err := appendFile(envPath, add.String()); err != nil {
			return err
		}
	}

	pricesPath := filepath.Join(*dir, "prices.yaml")
	pricesCreated := false
	if _, err := os.Stat(pricesPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(pricesPath, []byte(examplePrices), 0o600); err != nil {
			return err
		}
		pricesCreated = true
	}

	switch {
	case !envExisted:
		fmt.Fprintf(stdout, "Created %s\n", envPath)
	case add.Len() > 0:
		fmt.Fprintf(stdout, "Updated %s\n", envPath)
	default:
		fmt.Fprintf(stdout, "%s is already set up\n", envPath)
	}
	if pricesCreated {
		fmt.Fprintf(stdout, "Created %s (example prices: check your provider's pricing page)\n", pricesPath)
	}
	app := have["VEILOX_APP"]
	if app == "" && len(pos) == 1 {
		app = pos[0]
	}
	if app == "" {
		app = config.EnvApp
	}
	label := "Your veilox key"
	if !newKey {
		label = "Your veilox key (already in .env, unchanged)"
	}
	fmt.Fprintf(stdout, `
%s, for the app %q:

  %s

Next:
  1. Open .env and fill in OPENAI_API_KEY (or ANTHROPIC_API_KEY).
  2. Start veilox:  veilox serve
  3. In your app's settings, use:
       API key:   %s
       Base URL:  http://localhost:8080/v1   (OpenAI)
                  http://localhost:8080      (Anthropic)
`, label, app, key, key)
	return nil
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- .env in the chosen folder
	if err != nil {
		return err
	}
	if info, err := f.Stat(); err == nil && info.Size() > 0 && !strings.HasPrefix(text, "\n") {
		text = "\n" + text
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// loadConfig loads .env files, then the config. Without --config, a missing veilox.yaml is fine.
func loadConfig(fs *flag.FlagSet, cfgPath string) (*config.Config, []string, error) {
	warnings, err := loadEnvFiles(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })
	if explicit {
		cfg, err := config.Load(cfgPath)
		return cfg, warnings, err
	}
	cfg, err := config.LoadOrDefault(cfgPath)
	return cfg, warnings, err
}
