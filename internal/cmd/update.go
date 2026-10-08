package cmd

import (
	"fmt"
	"runtime"

	clikit "github.com/dittofleet/go-cli-kit"
	"github.com/dittofleet/go-cli-kit/selfupdate"

	"github.com/dittofleet/crosstalk/internal/service"
)

// Update installs the latest release over this binary.
func Update(a clikit.App) error {
	// Releases are built for macOS only, so anywhere else the download
	// would 404.
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	_, err := selfupdate.Run(a)
	return err
}

// RestartDaemon runs after every update, by `update` or automatic, so the
// daemon that keeps running is the new one.
func RestartDaemon() error {
	if !service.Installed() {
		return nil
	}
	if err := service.Restart(); err != nil {
		return fmt.Errorf("the daemon is still the old version: %w", err)
	}
	fmt.Println("Restarted the daemon.")
	return nil
}
