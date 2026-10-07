package cmd

import (
	"errors"
	"os"

	clikit "github.com/dittofleet/go-cli-kit"
	"github.com/dittofleet/go-cli-kit/uninstall"
	"github.com/dittofleet/go-cli-kit/xdg"

	"github.com/dittofleet/crosstalk/internal/service"
)

const uninstallUsage = "usage: crosstalk uninstall [--yes]"

// Uninstall stops the daemon and removes the service, the values, the
// config and the binary.
func Uninstall(args []string, a clikit.App) error {
	yes := len(args) == 1 && args[0] == "--yes"
	if len(args) > 0 && !yes {
		return errors.New(uninstallUsage)
	}
	return uninstall.Run(a, yes, uninstall.Plan{
		Items: []uninstall.Item{
			{Label: "Daemon", Note: "stopped, and no longer started at login", Remove: func(string) error { return service.Remove() }},
			{Label: "Values", Path: xdg.DataDir(a.Name), Remove: os.RemoveAll},
			{Label: "Config", Path: xdg.ConfigDir(a.Name), Note: "hub address and key", Remove: os.RemoveAll},
		},
		Notice: "Note: the hub and your other machines are NOT touched. This machine stays on\n" +
			"their list until one of them runs `crosstalk remove`.",
	})
}
