package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dittofleet/crosstalk/internal/config"
	"github.com/dittofleet/crosstalk/internal/daemon"
	"github.com/dittofleet/crosstalk/internal/service"
)

// Start sets the background service up if it is not, and starts the daemon
// or restarts one that is running.
func Start(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: crosstalk start")
	}
	if _, err := config.Load(); err != nil {
		return err
	}
	binary, err := resolveExecutable()
	if err != nil {
		return err
	}
	if err := service.Install(binary); err != nil {
		return err
	}
	fmt.Println("The daemon is running, and starts again whenever you log in.")
	return nil
}

// Daemon runs the daemon in the foreground until it is interrupted.
func Daemon(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: crosstalk daemon")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	d, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx)
}
