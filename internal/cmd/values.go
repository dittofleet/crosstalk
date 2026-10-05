package cmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dittofleet/crosstalk/internal/wire"
)

const postUsage = `usage: crosstalk post [--expires <duration>] <name> <json | ->`

// Post stores this machine's value for a name, for the other machines to
// read. The daemon keeps it, so it outlives the program that posted it.
func Post(args []string) error {
	flags := flag.NewFlagSet("post", flag.ContinueOnError)
	expires := flags.Duration("expires", 0, "how long the value counts for, e.g. 36h (default: until replaced)")
	flags.Usage = func() { fmt.Fprintln(os.Stderr, postUsage) }
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 || *expires < 0 {
		return errors.New(postUsage)
	}
	body, err := readBody(flags.Arg(1))
	if err != nil {
		return err
	}
	_, err = ask(wire.Request{Op: "post", Name: flags.Arg(0), Body: body, Expires: expires.Seconds()})
	return err
}

// Unpost takes back this machine's value for a name.
func Unpost(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crosstalk unpost <name>")
	}
	_, err := ask(wire.Request{Op: "unpost", Name: args[0]})
	return err
}

const readUsage = `usage: crosstalk read [--fresh] [--timeout <seconds>] <device> <name>
       crosstalk read --all <name>`

// Read prints what a machine posted on a name, or with --all what each
// machine did, one line of JSON per machine.
func Read(args []string) error {
	flags := flag.NewFlagSet("read", flag.ContinueOnError)
	all := flags.Bool("all", false, "read every machine's value")
	fresh := flags.Bool("fresh", false, "ask the machine to post again first")
	timeout := flags.Float64("timeout", 0, "seconds a fresh read waits for the new value (default 10)")
	flags.Usage = func() { fmt.Fprintln(os.Stderr, readUsage) }
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	req := wire.Request{Op: "read", Fresh: *fresh, Timeout: *timeout}
	if *all {
		req.To = "*"
	} else if len(rest) > 0 {
		req.To, rest = rest[0], rest[1:]
	}
	if len(rest) != 1 || (*all && *fresh) {
		return errors.New(readUsage)
	}
	req.Name = rest[0]
	res, err := ask(req)
	if err != nil {
		return err
	}
	for _, e := range res.Entries {
		printLine(e)
	}
	return nil
}

// Set writes a value that all your machines share. The latest write wins.
func Set(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: crosstalk set <name> <json | ->")
	}
	body, err := readBody(args[1])
	if err != nil {
		return err
	}
	_, err = ask(wire.Request{Op: "set", Name: args[0], Body: body})
	return err
}

// Unset removes a shared value on every machine.
func Unset(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crosstalk unset <name>")
	}
	_, err := ask(wire.Request{Op: "unset", Name: args[0]})
	return err
}

// Get prints a shared value, from this machine's own copy.
func Get(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crosstalk get <name>")
	}
	res, err := ask(wire.Request{Op: "get", Name: args[0]})
	if err != nil {
		return err
	}
	fmt.Println(string(res.Entries[0].Value))
	return nil
}

// Watch prints the values under a name as they stand, then a line for each
// change, until it is interrupted. If the daemon goes away it waits for it
// and carries on, printing the values afresh.
func Watch(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crosstalk watch <name>")
	}
	for first := true; ; first = false {
		err := watchOnce(args[0])
		// A refusal will not change by asking again, and a daemon that
		// was never there is for the user to start.
		var failure *Failure
		if errors.As(err, &failure) || (first && err != nil && !errors.Is(err, errWatchEnded)) {
			return err
		}
		time.Sleep(time.Second)
	}
}

var errWatchEnded = errors.New("the crosstalk daemon closed the connection")

func watchOnce(name string) error {
	c, lines, err := connect(wire.Request{Op: "watch", Name: name})
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := answer(lines); err != nil {
		return err
	}
	for lines.Scan() {
		fmt.Println(lines.Text())
	}
	return errWatchEnded
}

func printLine(v any) {
	wire.WriteLine(os.Stdout, v)
}
