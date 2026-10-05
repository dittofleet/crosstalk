package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig points XDG_CONFIG_HOME at a fresh directory holding body as
// the config file, so the host's own config cannot leak into the test.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "crosstalk"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "crosstalk", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSaveAndLoadGiveBackTheSameConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := Load(); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("Load with no config = %v", err)
	}
	cfg := &Config{SchemaVersion: SchemaVersion, Hub: "https://crosstalk.example.com", Name: "lychee", ID: "abc", Key: "ctk_x"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved config: %v, %v", info, err)
	}
	got, err := Load()
	if err != nil || *got != *cfg {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if want := "https://crosstalk.example.com/connect?id=abc&name=lychee&v=1"; got.ConnectURL() != want {
		t.Errorf("ConnectURL = %s", got.ConnectURL())
	}
}

func TestLoadRefusesAConfigItCannotUse(t *testing.T) {
	for _, c := range []struct{ body, says string }{
		{`{"schemaVersion":2,"hub":"https://a.example","name":"a","id":"x","key":"k"}`, "schemaVersion"},
		{`{"schemaVersion":1,"hub":"http://a.example","name":"a","id":"x","key":"k"}`, "hub"},
		{`{"schemaVersion":1,"hub":"https://a.example","name":"A B","id":"x","key":"k"}`, "name"},
		{`{"schemaVersion":1,"hub":"https://a.example","name":"a","id":"x"}`, "key"},
		{`{"schemaVersion":1,"hub":"https://a.example","name":"a","id":"x","key":"k","extra":1}`, "extra"},
	} {
		writeConfig(t, c.body)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("Load(%s) = %v, want it to mention %s", c.body, err, c.says)
		}
	}
}

func TestParseHubTakesOnlyAnHTTPSAddress(t *testing.T) {
	for _, ok := range []string{"https://crosstalk.example.com", "https://crosstalk.example.com/", "http://localhost:8787", "http://127.0.0.1:8787"} {
		if _, err := ParseHub(ok); err != nil {
			t.Errorf("ParseHub(%s) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "crosstalk.example.com", "http://crosstalk.example.com", "https://crosstalk.example.com/connect", "https://a.example?x=1"} {
		if _, err := ParseHub(bad); err == nil {
			t.Errorf("ParseHub(%s) succeeded", bad)
		}
	}
}

func TestADeviceNameComesFromTheHostname(t *testing.T) {
	for host, want := range map[string]string{
		"Lychee.local":                     "lychee",
		"Sam's MacBook Air":                "sam-s-macbook-air",
		"--odd--":                          "odd",
		strings.Repeat("a", 40) + ".local": strings.Repeat("a", 32),
	} {
		if got := nameFrom(host); got != want {
			t.Errorf("nameFrom(%q) = %q, want %q", host, got, want)
		}
	}
}
