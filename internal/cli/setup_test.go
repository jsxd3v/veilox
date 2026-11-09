package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jsxd3v/veilox/internal/fakeprovider"
)

var keyRe = regexp.MustCompile(`vx_[A-Za-z0-9]{40}`)

func TestExamplePricesMatchesFile(t *testing.T) {
	b, err := os.ReadFile("../../examples/prices.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != examplePrices {
		t.Fatal("examplePrices in setup.go differs from examples/prices.yaml: copy the file into the constant")
	}
}

func TestSetupNewFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "veilox")
	code, out, errs := run(t, "", "setup", "--dir", dir)
	if code != 0 {
		t.Fatal(errs)
	}
	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	key := keyRe.FindString(string(env))
	if key == "" || !strings.Contains(out, key) || !strings.Contains(string(env), "OPENAI_API_KEY=\n") {
		t.Fatalf("env:\n%s\nout:\n%s", env, out)
	}
	if !strings.Contains(out, `for the app "default"`) || !strings.Contains(out, "http://localhost:8080/v1") {
		t.Fatal(out)
	}
	if info, _ := os.Stat(filepath.Join(dir, ".env")); info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", info.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "prices.yaml")); string(b) != examplePrices {
		t.Fatal("prices.yaml not written")
	}

	// Running it again keeps the key and changes nothing.
	code, out, _ = run(t, "", "setup", "--dir", dir)
	env2, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if code != 0 || string(env2) != string(env) || !strings.Contains(out, "already set up") || !strings.Contains(out, key) {
		t.Fatalf("second run changed things:\n%s", out)
	}
}

func TestSetupExistingEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "OPENAI_API_KEY=sk-mine")
	writeFile(t, dir, "prices.yaml", "models: {}\n")
	code, out, _ := run(t, "", "setup", "llmvero", "--dir", dir)
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if code != 0 || !strings.HasPrefix(string(env), "OPENAI_API_KEY=sk-mine\n") || strings.Count(string(env), "OPENAI_API_KEY") != 1 {
		t.Fatalf("%s", env)
	}
	if !strings.Contains(string(env), "VEILOX_APP=llmvero") || keyRe.FindString(string(env)) == "" || !strings.Contains(out, `for the app "llmvero"`) {
		t.Fatalf("%s\n%s", env, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "prices.yaml")); string(b) != "models: {}\n" {
		t.Fatal("an existing prices.yaml must not be replaced")
	}
	if code, _, _ := run(t, "", "setup", "a", "b"); code != 2 {
		t.Fatal(code)
	}
}

func TestParseDotEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "# comment\n\nexport A=1\nB = \"two words\"\nC='x'\nD=val # note\nE=\n")
	lines, err := parseDotEnv(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(lines)
	if got != "[{A 1} {B two words} {C x} {D val} {E }]" {
		t.Fatal(got)
	}
	writeFile(t, dir, ".env", "just text\n")
	if _, err := parseDotEnv(filepath.Join(dir, ".env")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatal(err)
	}
	if lines, err := parseDotEnv(filepath.Join(dir, "missing")); err != nil || lines != nil {
		t.Fatal("a missing file is fine")
	}
}

func TestLoadDotEnvRealEnvWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "VEILOX_TEST_A=from-file\nVEILOX_TEST_B=from-file\n")
	if err := os.Chmod(filepath.Join(dir, ".env"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VEILOX_TEST_A", "from-env")
	t.Setenv("VEILOX_TEST_B", "")
	warning, err := loadDotEnv(dir)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("VEILOX_TEST_A") != "from-env" || os.Getenv("VEILOX_TEST_B") != "from-file" {
		t.Fatal(os.Getenv("VEILOX_TEST_A"), os.Getenv("VEILOX_TEST_B"))
	}
	if !strings.Contains(warning, "chmod 600") {
		t.Fatal(warning)
	}
}

// The simple flow: setup, add the provider key to .env, serve, call it with the key. No veilox.yaml.
func TestServeFromDotEnvOnly(t *testing.T) {
	fake := &fakeprovider.Fake{}
	up := httptest.NewServer(fake)
	defer up.Close()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "VEILOX_KEY", "VEILOX_APP", "VEILOX_LISTEN"} {
		t.Setenv(k, "")
	}
	if code, _, errs := run(t, "", "setup", "llmvero"); code != 0 {
		t.Fatal(errs)
	}
	env, _ := os.ReadFile(".env")
	key := keyRe.FindString(string(env))
	writeFile(t, dir, ".env", strings.Replace(string(env), "OPENAI_API_KEY=\n", "OPENAI_API_KEY=sk-test\n", 1)+"VEILOX_LISTEN=127.0.0.1:0\n")

	// check works without veilox.yaml and sees the app from .env.
	getenv = os.Getenv
	code, out, errs := run(t, "", "check")
	if code != 0 || !strings.Contains(out, "No veilox.yaml") || !strings.Contains(out, "apps:       llmvero (no budget)") || strings.Contains(out, "OPENAI_API_KEY is not set") {
		t.Fatalf("%s %s", out, errs)
	}

	// Point the default OpenAI URL at the fake provider for this test only.
	writeFile(t, dir, "veilox.yaml", "providers:\n  openai: { base_url: "+up.URL+" }\n")
	addr := make(chan string, 1)
	sigs := make(chan chan<- os.Signal, 1)
	onListen = func(a string) { addr <- a }
	serveSignals = func(c chan<- os.Signal) { sigs <- c }
	defer func() {
		onListen = func(string) {}
		serveSignals = func(chan<- os.Signal) {}
	}()
	var logs syncWriter
	done := make(chan int, 1)
	go func() { done <- Run([]string{"serve"}, nil, io.Discard, &logs) }()
	base := "http://" + <-addr
	sig := <-sigs

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-6.1-terra","messages":[{"role":"user","content":"mail me at a@example.com"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(fake.Bodies()[0], "<EMAIL_1>") {
		t.Fatalf("%s %s", resp.Status, body)
	}
	if h := fake.Headers()[0].Get("Authorization"); h != "Bearer sk-test" {
		t.Fatalf("provider key from .env not used: %q", h)
	}
	sig <- os.Interrupt
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if !strings.Contains(logs.String(), `"app":"llmvero"`) {
		t.Fatal(logs.String())
	}
}
