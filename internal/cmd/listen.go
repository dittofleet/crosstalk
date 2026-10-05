package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"

	"github.com/dittofleet/crosstalk/internal/wire"
)

const listenUsage = `usage: crosstalk listen <name>
       crosstalk listen <name> --run <command> [<arg>...]`

// Listen receives the messages sent to a name on this machine until it is
// interrupted.
//
// Plain, it prints each message as a line of JSON and takes replies as
// lines of JSON on stdin: {"id": <the message's id>, "body": <any JSON>}.
//
// With --run it runs the command once per message, with the message's body
// on stdin and CROSSTALK_FROM and CROSSTALK_NAME set. What the command
// prints is the reply: as JSON if it is JSON, as a string otherwise.
func Listen(args []string) error {
	if len(args) == 0 || len(args) == 2 || (len(args) > 2 && args[1] != "--run") {
		return errors.New(listenUsage)
	}
	name, command := args[0], args[min(2, len(args)):]

	c, lines, err := connect(wire.Request{Op: "listen", Name: name})
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := answer(lines); err != nil {
		return err
	}

	// Replies come from stdin or from several handlers at once.
	var wmu sync.Mutex
	reply := func(r wire.Reply) {
		wmu.Lock()
		defer wmu.Unlock()
		wire.WriteLine(c, r)
	}

	if len(command) == 0 {
		go relayReplies(c, &wmu)
	}
	for lines.Scan() {
		if len(command) == 0 {
			fmt.Println(lines.Text())
			continue
		}
		var msg wire.Delivery
		if json.Unmarshal(lines.Bytes(), &msg) != nil {
			continue
		}
		go func() {
			r := handle(command, msg)
			if msg.WantsReply {
				reply(r)
			}
		}()
	}
	return errors.New("the crosstalk daemon closed the connection")
}

// relayReplies passes the lines typed or piped into stdin to the daemon.
// Stdin ending does not end the listening.
func relayReplies(c net.Conn, wmu *sync.Mutex) {
	in := wire.NewScanner(os.Stdin)
	for in.Scan() {
		wmu.Lock()
		c.Write(append(bytes.Clone(in.Bytes()), '\n'))
		wmu.Unlock()
	}
}

// handle runs the command for one message and turns its output into a reply.
func handle(command []string, msg wire.Delivery) wire.Reply {
	run := exec.Command(command[0], command[1:]...)
	run.Stdin = bytes.NewReader(msg.Body)
	run.Stderr = os.Stderr
	run.Env = append(os.Environ(), "CROSSTALK_FROM="+msg.From, "CROSSTALK_NAME="+msg.Name)
	out, err := run.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "crosstalk: %s: %v\n", command[0], err)
		return wire.Reply{ID: msg.ID, Error: "handler-failed", Message: fmt.Sprintf("%s: %v", command[0], err)}
	}
	out = bytes.TrimSpace(out)
	switch {
	case len(out) == 0:
		return wire.Reply{ID: msg.ID}
	case json.Valid(out):
		return wire.Reply{ID: msg.ID, Body: out}
	}
	text, _ := json.Marshal(string(out))
	return wire.Reply{ID: msg.ID, Body: text}
}
