package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/detect"
	"github.com/jsxd3v/veilox/internal/mask"
	"github.com/jsxd3v/veilox/internal/proxy"
)

func runScan(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlags("scan", stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "path to veilox.yaml (for names and custom rules)")
	request := fs.Bool("request", false, "the file is an OpenAI or Anthropic request body: mask the fields the gateway masks")
	api := fs.String("api", "auto", "with --request: openai, anthropic or auto")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: veilox scan <file|-> [--request] [--json]")
		return errUsage
	}
	explicitConfig := false
	fs.Visit(func(f *flag.Flag) { explicitConfig = explicitConfig || f.Name == "config" })

	engine, err := scanEngine(*cfgPath, explicitConfig, stderr)
	if err != nil {
		return err
	}
	var data []byte
	if pos[0] == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(pos[0])
	}
	if err != nil {
		return err
	}

	m := mask.NewMapper(engine)
	if !*request {
		text := string(data)
		m.Reserve(text)
		masked, findings := m.MaskText(text)
		if *asJSON {
			return writeScanJSON(stdout, masked, findings, text, m)
		}
		fmt.Fprint(stdout, masked)
		if !strings.HasSuffix(masked, "\n") {
			fmt.Fprintln(stdout)
		}
		printFindings(stdout, findings, text, nil)
		return nil
	}

	body, err := proxy.DecodeRequest(data)
	if err != nil {
		return fmt.Errorf("%s: not a JSON request body: %w", pos[0], err)
	}
	which := *api
	if which == "auto" {
		which = proxy.GuessAPI(body)
	}
	if _, err := proxy.WalkRequest(which, body, m.Reserve); err != nil {
		return err
	}
	var findings []mask.Finding
	unsupported, _ := proxy.WalkRequest(which, body, func(s string) string {
		out, f := m.MaskText(s)
		findings = append(findings, f...)
		return out
	})
	if *asJSON {
		return writeScanJSON(stdout, body, findings, "", m)
	}
	fmt.Fprintf(stdout, "%s request, as the provider would receive it:\n\n", which)
	var pretty strings.Builder
	enc := json.NewEncoder(&pretty)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
	fmt.Fprint(stdout, pretty.String())
	printFindings(stdout, findings, "", unsupported)
	return nil
}

// scanEngine uses the config's detectors, names and rules when there is a config file.
func scanEngine(path string, explicit bool, stderr io.Writer) (*detect.Engine, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg.Engine()
	}
	if explicit || !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fmt.Fprintln(stderr, "(no veilox.yaml here: using the built-in detectors only, without your names and rules)")
	return detect.New(detect.Options{})
}

type position struct{ line, col int }

// positions converts byte offsets to 1-based line and column (in characters).
func positions(text string, offsets []int) []position {
	out := make([]position, len(offsets))
	for i, off := range offsets {
		before := text[:off]
		line := strings.Count(before, "\n") + 1
		col := utf8.RuneCountInString(before[strings.LastIndexByte(before, '\n')+1:]) + 1
		out[i] = position{line, col}
	}
	return out
}

func printFindings(w io.Writer, findings []mask.Finding, text string, unsupported []string) {
	fmt.Fprintln(w)
	if len(findings) == 0 {
		fmt.Fprintln(w, "Nothing to mask. (Dry run: nothing was sent anywhere.)")
	} else {
		fmt.Fprintf(w, "%d values would be masked (dry run: nothing was sent anywhere):\n\n", len(findings))
		t := &table{}
		var pos []position
		if text != "" {
			offs := make([]int, len(findings))
			for i, f := range findings {
				offs[i] = f.Start
			}
			pos = positions(text, offs)
			t.header([]string{"LINE:COL", "TYPE", "PLACEHOLDER", "VALUE"}, 4)
		} else {
			t.header([]string{"TYPE", "PLACEHOLDER", "VALUE"}, 3)
		}
		for i, f := range findings {
			row := []string{f.Type, f.Placeholder, f.Value}
			if pos != nil {
				row = append([]string{fmt.Sprintf("%d:%d", pos[i].line, pos[i].col)}, row...)
			}
			t.row(row)
		}
		t.write(w)
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		fmt.Fprintf(w, "\nNot masked (veilox can't read these parts): %s\n", strings.Join(unsupported, ", "))
	}
}

func writeScanJSON(w io.Writer, masked any, findings []mask.Finding, text string, m *mask.Mapper) error {
	type item struct {
		Line        int    `json:"line,omitempty"`
		Column      int    `json:"column,omitempty"`
		Type        string `json:"type"`
		Placeholder string `json:"placeholder"`
		Value       string `json:"value"`
	}
	items := make([]item, len(findings))
	var pos []position
	if text != "" {
		offs := make([]int, len(findings))
		for i, f := range findings {
			offs[i] = f.Start
		}
		pos = positions(text, offs)
	}
	for i, f := range findings {
		items[i] = item{Type: f.Type, Placeholder: f.Placeholder, Value: f.Value}
		if pos != nil {
			items[i].Line, items[i].Column = pos[i].line, pos[i].col
		}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"masked": masked, "findings": items, "counts": m.Counts()})
}
