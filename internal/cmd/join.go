package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/dittofleet/crosstalk/internal/config"
	"github.com/dittofleet/crosstalk/internal/daemon"
	"github.com/dittofleet/crosstalk/internal/secret"
	"github.com/dittofleet/crosstalk/internal/service"
	"github.com/dittofleet/crosstalk/internal/wire"
)

// Join points this machine at a hub. It connects once before saving
// anything, so a wrong address, key or name is caught here and not by a
// daemon failing in the background.
func Join(args []string) error {
	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	name := flags.String("name", "", "what to call this machine (default: its hostname)")
	noService := flags.Bool("no-service", false, "do not start the daemon in the background")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: crosstalk join [--name <name>] [--no-service] <hub url>")
	}
	hub, err := config.ParseHub(flags.Arg(0))
	if err != nil {
		return fmt.Errorf("hub url: %w", err)
	}

	cfg := &config.Config{SchemaVersion: config.SchemaVersion, Hub: hub.Scheme + "://" + hub.Host}
	// Joining again keeps the id, and the name unless a new one is asked
	// for: the hub ties the name to the id.
	oldName := ""
	if old, err := config.Load(); err == nil {
		cfg.ID, cfg.Name, oldName = old.ID, old.Name, old.Name
	}
	if *name != "" {
		cfg.Name = *name
	}
	if cfg.Name == "" {
		cfg.Name = config.DefaultName()
	}
	if !wire.ValidDevice(cfg.Name) {
		return fmt.Errorf("%q cannot be a device name: use up to 32 lowercase letters, digits and dashes (--name)", cfg.Name)
	}
	if cfg.ID == "" {
		if cfg.ID, err = config.NewID(); err != nil {
			return err
		}
	}

	if cfg.Key, err = readKey(); err != nil {
		return err
	}
	keys, err := secret.Derive(cfg.Key)
	if err != nil {
		return err
	}
	devices, err := daemon.Probe(context.Background(), cfg, keys)
	switch {
	case errors.Is(err, daemon.ErrNameTaken):
		// That machine may be this one, before an uninstall: the hub ties a
		// name to the id it first came with, and the id went with the config.
		return fmt.Errorf("another machine is already called %q on this hub: pick another name with --name, or if it was this machine before an uninstall, run `crosstalk remove %s` on another machine first", cfg.Name, cfg.Name)
	case err != nil:
		return fmt.Errorf("could not connect to %s: %w", cfg.Hub, err)
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	var others []string
	for _, device := range devices {
		if device.Name != cfg.Name {
			others = append(others, device.Name)
		}
	}
	fmt.Printf("Joined %s as %s.\n", cfg.Hub, cfg.Name)
	if len(others) > 0 {
		fmt.Printf("Other devices: %s\n", strings.Join(others, ", "))
	}
	if oldName != "" && oldName != cfg.Name {
		fmt.Printf("%s stays on the list as away, until `crosstalk remove %s`.\n", oldName, oldName)
	}
	if *noService {
		fmt.Println("Start the daemon with `crosstalk daemon`.")
		return nil
	}
	// Installing again restarts a daemon that is already running, which is
	// how it comes to use what was just saved.
	binary, err := resolveExecutable()
	if err == nil {
		err = service.Install(binary)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not start the daemon in the background: %v\nRun `crosstalk daemon` to start it yourself.\n", err)
		return nil
	}
	fmt.Println("The daemon is running, and starts again whenever you log in.")
	return nil
}

// readKey takes the key from CROSSTALK_KEY, or asks for it without showing
// it. It is never taken as an argument, which would leave it in shell
// history and the process list.
func readKey() (string, error) {
	if key := os.Getenv("CROSSTALK_KEY"); key != "" {
		return key, nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to ask for the key on, set CROSSTALK_KEY")
	}
	defer tty.Close()
	fmt.Fprint(tty, "Key: ")
	key, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", err
	}
	return string(key), nil
}
