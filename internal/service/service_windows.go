package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	clikit "github.com/dittofleet/go-cli-kit"
	"github.com/dittofleet/go-cli-kit/xdg"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/dittofleet/crosstalk/internal/app"
	"github.com/dittofleet/crosstalk/internal/daemon"
)

// Windows has nothing like launchd for one user's background programs, so
// here the daemon is an ordinary process that crosstalk starts and stops
// itself. It is started at login from the Run key, which Settings lists
// under Startup apps. Nothing starts it again if it dies.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// logPath is where the daemon's log goes.
func logPath() string {
	return filepath.Join(xdg.DataDir(app.Name), "daemon.log")
}

// start runs the daemon with no window, writing to its log.
func start(binary string) error {
	if err := os.MkdirAll(filepath.Dir(logPath()), 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	child := exec.Command(binary, "daemon")
	// Not the folder this command was run in: Windows will not let a
	// folder be renamed or removed while a process is working in it.
	child.Dir = home()
	child.Stdout, child.Stderr = log, log
	// A console of its own, so it outlives this terminal and is not
	// interrupted along with it.
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := child.Start(); err != nil {
		return err
	}
	return child.Process.Release()
}

// stop stops the running daemon, and waits for its process to go, as
// Windows will not replace or remove a file it still has open. With no
// daemon running it does nothing.
func stop() error {
	pid, err := daemon.Stop()
	if err != nil || pid == 0 {
		return err
	}
	// Not finding it means it has gone already.
	if running, err := os.FindProcess(pid); err == nil {
		running.Wait()
	}
	return nil
}

// Installed reports whether the service has been set up on this machine.
func Installed() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue(app.Name)
	return err == nil
}

// Install sets the service up to run binary, and starts it. Run again, it
// replaces what was there and restarts the daemon, which is how a daemon
// picks up a new config.
func Install(binary string) error {
	// Windows gives a command-line program started outside a terminal, as
	// one is at login, a window of its own. So what it starts at login is
	// the windowless launcher the install script puts beside the binary,
	// which runs `crosstalk start`. A build from source has none.
	launcher := clikit.Windowless(binary)
	if _, err := os.Stat(launcher); err != nil {
		return fmt.Errorf("%s, which starts the daemon at login, is not there", launcher)
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetStringValue(app.Name, fmt.Sprintf(`"%s" start`, launcher)); err != nil {
		return err
	}
	if err := stop(); err != nil {
		return err
	}
	return start(binary)
}

// Restart stops the daemon and starts it again, for a new binary. An
// update leaves the new binary where the running one was started from.
func Restart() error {
	binary, err := clikit.Executable()
	if err != nil {
		return err
	}
	if err := stop(); err != nil {
		return err
	}
	return start(binary)
}

// Remove stops the daemon and takes the service away. With no service set
// up it does nothing.
func Remove() error {
	if !Installed() {
		return nil
	}
	// A daemon still running afterwards would write its files back after
	// they are removed.
	if err := stop(); err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.DeleteValue(app.Name)
}
