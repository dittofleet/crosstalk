package cmd

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/dittofleet/crosstalk/internal/wire"
)

// Devices lists the machines on the hub and whether each is connected.
func Devices(args []string) error {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !asJSON {
		return errors.New("usage: crosstalk devices [--json]")
	}
	res, err := ask(wire.Request{Op: "devices"})
	if err != nil {
		return err
	}
	if asJSON {
		printLine(struct {
			Hub     string        `json:"hub"`
			Devices []wire.Device `json:"devices"`
		}{res.Hub, res.Devices})
		return nil
	}

	if res.Hub != wire.HubConnected {
		fmt.Fprintln(os.Stderr, hubTrouble(res.Hub))
	}
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, device := range res.Devices {
		state := "away, last seen " + time.UnixMilli(device.LastSeen).Format("Jan 2 15:04")
		switch {
		case device.Self:
			state = "this machine"
		case device.Online:
			state = "connected"
		}
		fmt.Fprintf(table, "%s\t%s\n", device.Name, state)
	}
	return table.Flush()
}

func hubTrouble(state string) string {
	switch state {
	case wire.HubNameTaken:
		return "Not connected: another machine uses this machine's name on the hub. Join again with --name."
	case wire.HubUnauthorized:
		return "Not connected: the hub rejected the key. Join again with the current key."
	}
	return "Not connected to the hub, still trying. What follows is from the last time it was."
}

// Remove takes a machine off the hub's list. It only tidies the list: a
// machine that still has the key is put back the next time it connects.
func Remove(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crosstalk remove <device>")
	}
	_, err := ask(wire.Request{Op: "remove", Name: args[0]})
	return err
}
