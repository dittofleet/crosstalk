package cmd

import (
	"fmt"
	"runtime"

	clikit "github.com/dittofleet/go-cli-kit"
	"github.com/dittofleet/go-cli-kit/selfupdate"

	"github.com/dittofleet/crosstalk/internal/service"
)

// Update installs the latest release over this binary and restarts the
// daemon, so the one that keeps running is the new one.
func Update(a clikit.App) error {
	// Releases are built for macOS only, so anywhere else the download
	// would 404.
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	updated, err := selfupdate.Run(a)
	if err != nil || !updated {
		return err
	}
	if service.Installed() {
		if err := service.Restart(); err != nil {
			return fmt.Errorf("updated, but the daemon is still the old version: %w", err)
		}
		fmt.Println("Restarted the daemon.")
	}
	return nil
}
