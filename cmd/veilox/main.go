// Command veilox is a small gateway that masks personal data before it reaches OpenAI or Anthropic,
// restores it in the answer, and tracks what every call costs.
package main

import (
	"os"
	_ "time/tzdata" // timezones work in minimal containers too

	"github.com/jsxd3v/veilox/internal/cli"
)

// Set by GoReleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	cli.Version, cli.Commit, cli.Date = version, commit, date
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
