// Package cli implements the veilox command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jsxd3v/veilox/internal/auth"
)

// Build information, set by main from -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

const usage = `veilox: keep customer data out of AI providers and know what every AI call costs.

Usage:
  veilox setup   [app-name]                        create .env with a new app key (the quickest start)
  veilox serve   [--config veilox.yaml]            run the gateway
  veilox report  [--since 7d] [--app NAME] [--by day,app,model] [--format table|csv|json]
  veilox scan    <file|->  [--request] [--json]    dry run: show what would be masked
  veilox keygen  <app-name>                         create an app key
  veilox check   [--config veilox.yaml]             validate the config and price table
  veilox version

veilox reads .env from the current folder. veilox.yaml is optional (the path can also be set with
VEILOX_CONFIG). Docs: https://github.com/jsxd3v/veilox
`

// Run runs the CLI and returns the exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "setup":
		err = runSetup(args[1:], stdout, stderr)
	case "serve":
		err = runServe(args[1:], stdout, stderr)
	case "report":
		err = runReport(args[1:], stdout, stderr)
	case "scan":
		err = runScan(args[1:], stdin, stdout, stderr)
	case "keygen":
		err = runKeygen(args[1:], stdout, stderr)
	case "check":
		err = runCheck(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "veilox %s (commit %s, built %s)\n", Version, Commit, Date)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "veilox: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintf(stderr, "veilox: %v\n", err)
		return 1
	}
}

var errUsage = errors.New("usage")

// newFlags returns a flag set that prints errors to stderr and doesn't exit.
func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("veilox "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse parses flags that may come before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errUsage
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func defaultConfigPath() string {
	if v := os.Getenv("VEILOX_CONFIG"); v != "" {
		return v
	}
	return "veilox.yaml"
}

func runKeygen(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("keygen", stderr)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintln(stderr, "usage: veilox keygen <app-name>")
		return errUsage
	}
	key, err := auth.NewKey()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, `App key for %[1]q (shown once; give it to the app, keep it out of Git):

  %[2]s

Add this to the apps list in veilox.yaml (only the hash is stored):

  - name: %[1]s
    key_sha256: %[3]s
`, pos[0], key, auth.Hash(key))
	return nil
}
