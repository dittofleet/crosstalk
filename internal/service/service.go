// Package service keeps the daemon running in the background: as a launchd
// agent on macOS and a systemd user service on Linux, started on its own
// and restarted if it dies.
package service

import "os"

// The daemon finds its config and data through these, so a service started
// by launchd or systemd, which know nothing of the shell's environment, has
// to be told them.
var passedOn = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME"}

func home() string {
	dir, _ := os.UserHomeDir()
	return dir
}
