package cmd

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dittofleet/crosstalk/internal/wire"
)

const sendUsage = `usage: crosstalk send [--timeout <seconds>] <device> <name> [<json> | -]
       crosstalk send --all <name> [<json> | -]`

// Send delivers one message. To one device it prints the reply. With --all
// it goes to every other connected device, waits for nothing, and prints
// the devices it went to.
func Send(args []string) error {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	all := flags.Bool("all", false, "send to every other connected device, without waiting for replies")
	timeout := flags.Float64("timeout", 0, "seconds to wait for the reply (default 10)")
	flags.Usage = func() { fmt.Fprintln(os.Stderr, sendUsage) }
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	req := wire.Request{Op: "send", Timeout: *timeout}
	if *all {
		req.To = "*"
	} else if len(rest) > 0 {
		req.To, rest = rest[0], rest[1:]
	}
	if len(rest) < 1 || len(rest) > 2 {
		return errors.New(sendUsage)
	}
	req.Name = rest[0]
	if len(rest) == 2 {
		body, err := readBody(rest[1])
		if err != nil {
			return err
		}
		req.Body = body
	}

	res, err := ask(req)
	if err != nil {
		return err
	}
	if *all {
		for _, device := range res.Sent {
			fmt.Println(device)
		}
		return nil
	}
	if len(res.Body) == 0 {
		fmt.Println("null")
		return nil
	}
	fmt.Println(string(res.Body))
	return nil
}

// readBody takes a message body from an argument, or from stdin for "-".
func readBody(arg string) (json.RawMessage, error) {
	data := []byte(arg)
	if arg == "-" {
		var err error
		if data, err = io.ReadAll(io.LimitReader(os.Stdin, wire.MaxBody+1)); err != nil {
			return nil, err
		}
	}
	if !json.Valid(data) {
		return nil, errors.New(`the body must be JSON, e.g. '{"do":"refresh"}' or '"some text"'`)
	}
	return data, nil
}
