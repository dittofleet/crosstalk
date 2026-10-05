// Package service keeps the daemon running in the background as a launchd
// agent: started at login and restarted if it dies.
package service

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Label names the service to launchd.
const Label = "dev.crosstalk"

// The daemon finds its config and data through these, so a service started
// by launchd, which knows nothing of the shell's environment, has to be told
// them.
var passedOn = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME"}

var errUnsupported = errors.New("no background service on " + runtime.GOOS + ", run `crosstalk daemon` yourself")

func home() string {
	dir, _ := os.UserHomeDir()
	return dir
}

func plistPath() string {
	return filepath.Join(home(), "Library", "LaunchAgents", Label+".plist")
}

// LogPath is where the daemon's log goes.
func LogPath() string {
	return filepath.Join(home(), "Library", "Logs", "crosstalk.log")
}

func domain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// target names the service itself to launchctl.
func target() string {
	return domain() + "/" + Label
}

// Installed reports whether the service has been set up on this machine.
func Installed() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

// Install sets the service up to run binary, and starts it. Run again, it
// replaces what was there and restarts the daemon, which is how a daemon
// picks up a new config.
func Install(binary string) error {
	if runtime.GOOS != "darwin" {
		return errUnsupported
	}
	var env strings.Builder
	for _, name := range passedOn {
		if value := os.Getenv(name); value != "" {
			fmt.Fprintf(&env, "    <key>%s</key><string>%s</string>\n", name, html.EscapeString(value))
		}
	}
	// RunAtLoad starts the daemon at login, KeepAlive restarts it if it
	// dies, and ThrottleInterval keeps a daemon that cannot start from
	// spinning.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
%s  </dict>
</dict>
</plist>
`, Label, html.EscapeString(binary), html.EscapeString(LogPath()), html.EscapeString(LogPath()), env.String())

	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(LogPath()), 0o755); err != nil {
		return err
	}
	// Out with the old one first. There being none is the usual case.
	exec.Command("launchctl", "bootout", target()).Run()
	if err := os.WriteFile(plistPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	// launchd refuses a service whose last run is still shutting down, so
	// right after a bootout this can take a moment to go through.
	var out []byte
	var err error
	for range 10 {
		if out, err = exec.Command("launchctl", "bootstrap", domain(), plistPath()).CombinedOutput(); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(string(out)))
}

// Restart stops the daemon and starts it again, for a new binary.
func Restart() error {
	if runtime.GOOS != "darwin" {
		return errUnsupported
	}
	out, err := exec.Command("launchctl", "kickstart", "-k", target()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Remove stops the daemon and takes the service away. With no service set
// up it does nothing.
func Remove() error {
	if !Installed() {
		return nil
	}
	// Stopping one that is not loaded fails, which is fine. One that is
	// still loaded afterwards is not: its daemon would go on running, and
	// write its files back after they are removed.
	stop := exec.Command("launchctl", "bootout", target()).Run()
	if stop != nil && exec.Command("launchctl", "print", target()).Run() == nil {
		return fmt.Errorf("launchctl bootout %s: %w", target(), stop)
	}
	return os.Remove(plistPath())
}
