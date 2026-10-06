package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	clikit "github.com/dittofleet/go-cli-kit"
	"github.com/dittofleet/go-cli-kit/updatecheck"

	"github.com/dittofleet/crosstalk/internal/app"
	"github.com/dittofleet/crosstalk/internal/cmd"
)

var errUnknownCommand = errors.New("unknown command")

var version = "dev"

const usage = `Usage: crosstalk <command>

Messages, between apps on your machines:
  send <device> <name> [<json> | -]
                     Send a message and print the reply. --timeout <seconds>
                     changes how long to wait (default 10)
  send --all <name> [<json> | -]
                     Send to every other connected device, without waiting
                     for replies
  listen <name>      Print each message sent to <name> on this machine as a
                     line of JSON. Reply with a line on stdin:
                     {"id": <its id>, "body": <any JSON>}
  listen <name> --run <command>...
                     Run the command for each message, with the body on
                     stdin. What it prints is the reply

Values, kept by the daemon so they outlast the app that wrote them:
  post <name> <json | ->
                     Store this machine's value for <name>, for the others to
                     read. --expires <duration> drops it after, e.g., 36h
  unpost <name>      Take it back
  read <device> <name>
                     Print what a machine posted, even while it is away.
                     --fresh asks it to post again first
  read --all <name>  Print what each machine posted, one line per machine
  set <name> <json | ->
                     Store a value all your machines share. The latest write
                     wins
  unset <name>       Remove it everywhere
  get <name>         Print a shared value
  watch <name>       Print the values under <name>, then a line per change

Setup:
  key                Make a new key, and the hub token that goes with it
  key token          Print the hub token for this machine's key
  join [--name <name>] <hub url>
                     Point this machine at your hub (asks for the key), and
                     start the daemon in the background
  start              Start the daemon in the background, or restart it
  daemon             Run the daemon in the foreground, for a machine joined
                     with --no-service
  devices [--json]   List your machines and whether each is connected
  remove <device>    Take a machine that is gone off the list

  version            Print the installed version
  update             Download and install the latest version
  uninstall [--yes]  Stop the daemon and remove binary, config and values
  help               Print this help message
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(0)
	}
	crosstalk := app.New(version)
	if err := dispatch(crosstalk, args); err != nil {
		switch {
		case errors.Is(err, errUnknownCommand):
			fmt.Print(usage)
		case errors.Is(err, flag.ErrHelp):
		default:
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		os.Exit(1)
	}

	updatecheck.MaybeCheck(crosstalk, args[0])
}

func dispatch(crosstalk clikit.App, args []string) error {
	switch args[0] {
	case "send":
		return cmd.Send(args[1:])
	case "listen":
		return cmd.Listen(args[1:])
	case "post":
		return cmd.Post(args[1:])
	case "unpost":
		return cmd.Unpost(args[1:])
	case "read":
		return cmd.Read(args[1:])
	case "set":
		return cmd.Set(args[1:])
	case "unset":
		return cmd.Unset(args[1:])
	case "get":
		return cmd.Get(args[1:])
	case "watch":
		return cmd.Watch(args[1:])
	case "key":
		return cmd.Key(args[1:])
	case "join":
		return cmd.Join(args[1:])
	case "daemon":
		return cmd.Daemon(args[1:])
	case "start":
		return cmd.Start(args[1:])
	case "devices":
		return cmd.Devices(args[1:])
	case "remove":
		return cmd.Remove(args[1:])
	case "update":
		return cmd.Update(crosstalk)
	case "uninstall":
		return cmd.Uninstall(args[1:], crosstalk)
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		return errUnknownCommand
	}
}
