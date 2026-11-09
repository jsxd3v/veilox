package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/proxy"
)

var getenv = os.Getenv // tests replace it

func runCheck(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("check", stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "path to veilox.yaml")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	rt, err := proxy.NewRuntime(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s is valid.\n\n", *cfgPath)

	apps := make([]string, 0, len(cfg.Apps))
	for _, a := range cfg.Apps {
		if a.DailyBudgetUSD != nil {
			apps = append(apps, fmt.Sprintf("%s ($%.2f/day)", a.Name, *a.DailyBudgetUSD))
		} else {
			apps = append(apps, a.Name+" (no budget)")
		}
	}
	types := rt.Engine.Types()
	if len(cfg.Names) > 0 {
		types[len(types)-1] = fmt.Sprintf("NAME (%s names)", num(int64(len(cfg.Names))))
	}
	fmt.Fprintf(stdout, "  apps:       %s\n", orNone(apps))
	fmt.Fprintf(stdout, "  detectors:  %s\n", orNone(types))
	fmt.Fprintf(stdout, "  prices:     %d models from %s\n", rt.Prices.Len(), cfg.Pricing.File)
	keep := fmt.Sprintf("keeps %d days", cfg.Storage.RetentionDays)
	if cfg.Storage.RetentionDays == 0 {
		keep = "keeps everything"
	}
	fmt.Fprintf(stdout, "  database:   %s (%s)\n", cfg.Storage.Path, keep)

	var warnings []string
	if len(cfg.Apps) == 0 {
		warnings = append(warnings, "no apps: `veilox serve` won't start. Create one with `veilox keygen <app-name>`")
	}
	if rt.Prices.Len() == 0 {
		warnings = append(warnings, "no prices loaded: costs will show as unknown and budgets can't be enforced")
	}
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	missing := 0
	for _, n := range names {
		if getenv(cfg.Providers[n].APIKeyEnv) == "" {
			missing++
			warnings = append(warnings, fmt.Sprintf("%s is not set: calls to %s will fail", cfg.Providers[n].APIKeyEnv, n))
		}
	}
	if missing == len(names) {
		warnings = append(warnings, "no provider key is set at all")
	}
	if len(warnings) > 0 {
		fmt.Fprintln(stdout)
		for _, w := range warnings {
			fmt.Fprintf(stdout, "  ! %s\n", w)
		}
	}
	return nil
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}
