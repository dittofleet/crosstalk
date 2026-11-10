//go:build !windows

package config

import (
	"os"
	"testing"
)

// The config holds the key. Windows has no such permissions to check: there
// the user's own folder keeps others out.
func TestOnlyItsOwnerCanReadASavedConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &Config{SchemaVersion: SchemaVersion, Hub: "https://crosstalk.example.com", Name: "lychee", ID: "abc", Key: "ctk_x"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved config: %v, %v", info, err)
	}
}
