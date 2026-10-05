package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/dittofleet/crosstalk/internal/service"
	"github.com/dittofleet/crosstalk/internal/xdg"
)

const uninstallUsage = "usage: crosstalk uninstall [--yes]"

// Uninstall stops the daemon and removes the service, the values, the
// config and the binary. The binary goes last, so a failure leaves a tool
// to retry with.
func Uninstall(args []string, version string) error {
	yes := len(args) == 1 && args[0] == "--yes"
	if len(args) > 0 && !yes {
		return errors.New(uninstallUsage)
	}
	if version == "dev" {
		return errors.New("cannot uninstall a dev build")
	}

	binaryPath, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("cannot determine binary path: %w", err)
	}
	configDir := xdg.ConfigDir("crosstalk")
	dataDir := xdg.DataDir("crosstalk")

	fmt.Println("This will stop the daemon and remove:")
	fmt.Printf("  - Binary:  %s\n", binaryPath)
	fmt.Printf("  - Config:  %s  (hub address and key)\n", configDir)
	fmt.Printf("  - Values:  %s\n", dataDir)
	fmt.Println()
	fmt.Println("Note: the hub and your other machines are NOT touched. This machine stays on")
	fmt.Println("their list until one of them runs `crosstalk remove`.")
	fmt.Println()

	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("refusing to uninstall non-interactively without --yes")
		}
		fmt.Print("Proceed? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	if err := service.Remove(); err != nil {
		return fmt.Errorf("failed to remove the background service: %w", err)
	}
	steps := []struct {
		label string
		path  string
		fn    func(string) error
	}{
		{"values", dataDir, os.RemoveAll},
		{"config directory", configDir, os.RemoveAll},
		{"binary", binaryPath, os.Remove},
	}
	var removed []string
	for _, s := range steps {
		err := s.fn(s.path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			if len(removed) > 0 {
				fmt.Fprintf(os.Stderr, "Removed before failure: %s\n", strings.Join(removed, ", "))
			}
			return fmt.Errorf("failed to remove %s (%s): %w", s.label, s.path, err)
		}
		removed = append(removed, s.label)
	}

	fmt.Println("Uninstalled crosstalk.")
	return nil
}
