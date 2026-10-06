// Package config reads and writes what a machine knows about its hub.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dittofleet/go-cli-kit/xdg"

	"github.com/dittofleet/crosstalk/internal/app"
	"github.com/dittofleet/crosstalk/internal/wire"
)

const SchemaVersion = 1

type Config struct {
	SchemaVersion int `json:"schemaVersion"`
	// Hub is where the hub worker lives, e.g. https://crosstalk.example.com.
	Hub string `json:"hub"`
	// Name is what this machine is called on the hub.
	Name string `json:"name"`
	// ID tells the hub this is the machine that first took the name, so
	// another machine with the same name cannot stand in for it. It is not a
	// secret and grants nothing.
	ID string `json:"id"`
	// Key is the one secret, the same on every machine.
	Key string `json:"key"`
}

func Path() string {
	return filepath.Join(xdg.ConfigDir(app.Name), "config.json")
}

// SocketPath is where the daemon listens for programs on this machine.
func SocketPath() string {
	return filepath.Join(xdg.DataDir(app.Name), "sock")
}

// ValuesPath is where the daemon keeps posted and synced values.
func ValuesPath() string {
	return filepath.Join(xdg.DataDir(app.Name), "values.json")
}

// ErrNotJoined is returned by Load on a machine that has not run join.
var ErrNotJoined = errors.New("this machine has not joined a hub, run `crosstalk join <hub url>`")

func Load() (*Config, error) {
	path := Path()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotJoined
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := &Config{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	if cfg.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("invalid %s:\n  - schemaVersion: expected %d, got %d", path, SchemaVersion, cfg.SchemaVersion)
	}
	if _, err := ParseHub(cfg.Hub); err != nil {
		return nil, fmt.Errorf("invalid %s:\n  - hub: %w", path, err)
	}
	if !wire.ValidDevice(cfg.Name) {
		return nil, fmt.Errorf("invalid %s:\n  - name: not a usable device name", path)
	}
	if cfg.ID == "" || cfg.Key == "" {
		return nil, fmt.Errorf("invalid %s:\n  - id and key: missing, run `crosstalk join` again", path)
	}
	return cfg, nil
}

// Save writes the config where only this user can read it: it holds the key.
func (c *Config) Save() error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ConnectURL is the address the daemon opens its connection to.
func (c *Config) ConnectURL() string {
	query := url.Values{"v": {wire.HubProtocol}, "name": {c.Name}, "id": {c.ID}}
	return strings.TrimRight(c.Hub, "/") + "/connect?" + query.Encode()
}

// ParseHub checks a hub address. Plain http is only for a hub running on
// this machine, as `wrangler dev` does: the hub token travels in a header.
func ParseHub(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("must be a URL like https://crosstalk.example.com")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, errors.New("must be an https URL")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		return nil, errors.New("must be the hub's address alone, with no path")
	}
	return u, nil
}

// NewID makes the id a machine keeps for as long as it holds its name.
func NewID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

var notNameChar = regexp.MustCompile(`[^a-z0-9]+`)

// DefaultName turns this machine's hostname into a device name, so
// "Lychee.local" joins as "lychee".
func DefaultName() string {
	host, _ := os.Hostname()
	return nameFrom(host)
}

func nameFrom(host string) string {
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	name := strings.Trim(notNameChar.ReplaceAllString(host, "-"), "-")
	if len(name) > 32 {
		name = strings.Trim(name[:32], "-")
	}
	return name
}
