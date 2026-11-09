package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type envLine struct{ key, value string }

// parseDotEnv reads KEY=value lines. Blank lines and # comments are skipped; "export " and matching
// quotes around the value are allowed. A missing file gives no lines and no error.
func parseDotEnv(path string) ([]envLine, error) {
	f, err := os.Open(path) // #nosec G304 -- a .env file in the operator's folder
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []envLine
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("%s line %d: expected KEY=value", path, n)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i]) // trailing comment
		}
		out = append(out, envLine{key, value})
	}
	return out, sc.Err()
}

// loadDotEnv sets variables from dir/.env that are empty in the environment (the real environment
// wins). It returns a warning when other users can read the file.
func loadDotEnv(dir string) (warning string, err error) {
	path := filepath.Join(dir, ".env")
	lines, err := parseDotEnv(path)
	if err != nil || lines == nil {
		return "", err
	}
	if info, err := os.Stat(path); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		warning = fmt.Sprintf("%s can be read by other users; run: chmod 600 %s", path, path)
	}
	for _, l := range lines {
		if l.value == "" || os.Getenv(l.key) != "" {
			continue
		}
		if err := os.Setenv(l.key, l.value); err != nil {
			return warning, err
		}
	}
	return warning, nil
}

// loadEnvFiles loads .env from the current folder and from the config file's folder.
func loadEnvFiles(cfgPath string) ([]string, error) {
	dirs := []string{"."}
	if d := filepath.Dir(cfgPath); d != "." {
		dirs = append(dirs, d)
	}
	var warnings []string
	for _, d := range dirs {
		w, err := loadDotEnv(d)
		if err != nil {
			return warnings, err
		}
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings, nil
}
