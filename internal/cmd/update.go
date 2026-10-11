package cmd

import (
	"fmt"

	"github.com/dittofleet/crosstalk/internal/service"
)

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
