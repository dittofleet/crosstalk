package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	clikit "github.com/dittofleet/go-cli-kit"
	"github.com/dittofleet/go-cli-kit/postinstall"
	"golang.org/x/term"

	"github.com/dittofleet/crosstalk/internal/config"
)

// Postinstall is the install script's first-time setup. A machine that has
// joined already gets its daemon started on the new binary, which also
// sets the background service up where there is none. Otherwise it joins
// the hub in CROSSTALK_HUB (which, with CROSSTALK_KEY, makes a fresh
// remote machine a single curl-pipe), or one it asks for.
func Postinstall(a clikit.App) error {
	return postinstall.Run(a, func() error {
		if _, err := os.Stat(config.Path()); err == nil {
			return Start(nil)
		}
		hub := os.Getenv("CROSSTALK_HUB")
		if hub == "" && term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Print("Hub URL (empty to join later): ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			hub = strings.TrimSpace(line)
		}
		if hub == "" {
			fmt.Println("Run `crosstalk join <hub url>` to connect this machine to your hub.")
			return nil
		}
		return Join([]string{hub})
	})
}
