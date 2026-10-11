package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// unit names the service to systemd.
const unit = "crosstalk.service"

func unitPath() string {
	return filepath.Join(home(), ".config", "systemd", "user", unit)
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		err = errors.New(msg)
	}
	return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
}

// quote makes s one word to systemd, with nothing in it read as a
// specifier.
func quote(s string) string {
	return strconv.Quote(strings.ReplaceAll(s, "%", "%%"))
}

// Installed reports whether the service has been set up on this machine.
func Installed() bool {
	_, err := os.Stat(unitPath())
	return err == nil
}

// Install sets the service up to run binary, and starts it. Run again, it
// replaces what was there and restarts the daemon, which is how a daemon
// picks up a new config.
func Install(binary string) error {
	var env strings.Builder
	for _, name := range passedOn {
		if value := os.Getenv(name); value != "" {
			fmt.Fprintf(&env, "Environment=%s\n", quote(name+"="+value))
		}
	}
	// RestartSec keeps a daemon that cannot start from spinning. The log
	// goes to the journal: `journalctl --user-unit crosstalk`.
	content := fmt.Sprintf(`[Unit]
Description=crosstalk daemon

[Service]
ExecStart=%s daemon
%sRestart=always
RestartSec=10

[Install]
WantedBy=default.target
`, strings.ReplaceAll(quote(binary), "$", "$$"), env.String())

	// A user's services stop when their last session ends, unless they
	// linger. A Raspberry Pi reached over SSH has no session most of the
	// time, and lingering also starts the daemon at boot. It goes first, as
	// it also starts the user's service manager where none is running yet.
	linger, _ := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value").Output()
	if strings.TrimSpace(string(linger)) != "yes" && exec.Command("loginctl", "enable-linger").Run() != nil {
		fmt.Fprintln(os.Stderr, "The daemon stops when you log out. To keep it running, run `sudo loginctl enable-linger $USER`.")
	}

	if err := os.MkdirAll(filepath.Dir(unitPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath(), []byte(content), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}} {
		if err := systemctl(args...); err != nil {
			// A unit left behind would count as installed, and every
			// update would then fail to restart it.
			os.Remove(unitPath())
			return err
		}
	}
	return nil
}

// Restart stops the daemon and starts it again, for a new binary.
func Restart() error {
	return systemctl("restart", unit)
}

// Remove stops the daemon and takes the service away. With no service set
// up it does nothing.
func Remove() error {
	if !Installed() {
		return nil
	}
	// A daemon still running afterwards would write its files back after
	// they are removed.
	if err := systemctl("disable", "--now", unit); err != nil {
		return err
	}
	if err := os.Remove(unitPath()); err != nil {
		return err
	}
	return systemctl("daemon-reload")
}
