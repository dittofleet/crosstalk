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
// It also prints the daemon's notices, the lines with a "crosstalk" key,
// such as {"crosstalk":{"hub":"connected"}}.
//
// With --run it runs the command once per message, with the message's body
// on stdin and CROSSTALK_FROM and CROSSTALK_NAME set. What the command
// prints is the reply: as JSON if it is JSON, as a string otherwise.
func Listen(args []string) error {
	if len(args) == 0 || len(args) == 2 || (len(args) > 2 && args[1] != "--run") {
		return errors.New(listenUsage)
	}
	name, command := args[0], args[min(2, len(args)):]

	// Replies come from stdin or from several handlers at once, and go to
	// whichever connection is current. One meant for a message from before
	// the daemon restarted is ignored there: the daemon already told its
	// sender that nobody answered.
	l := &listening{}
	if len(command) == 0 {
		go l.relayReplies()
	}
	return stayConnected(restartPatience, func() error { return l.once(name, command) })
}

type listening struct {
	mu   sync.Mutex
	conn net.Conn
}

// Write sends one whole line to the current connection, or drops it when
// there is none.
func (l *listening) Write(line []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return len(line), nil
	}
	return l.conn.Write(line)
}

func (l *listening) setConn(c net.Conn) {
	l.mu.Lock()
	l.conn = c
	l.mu.Unlock()
}

// once listens until the daemon closes the connection.
func (l *listening) once(name string, command []string) error {
	c, lines, err := connect(wire.Request{Op: "listen", Name: name})
	if err != nil {
		return err
	}
	if _, err := answer(lines); err != nil {
		c.Close()
		return err
	}
	l.setConn(c)
	// Closed first: a reply stuck writing to it holds the lock setConn
	// needs, and only the close lets it go.
	defer func() {
		c.Close()
		l.setConn(nil)
	}()

	for lines.Scan() {
		if len(command) == 0 {
			fmt.Println(lines.Text())
			continue
		}
		var line struct {
			wire.Delivery
			Crosstalk json.RawMessage `json:"crosstalk"`
		}
		// A notice is not a message, so there is nothing to run.
		if json.Unmarshal(lines.Bytes(), &line) != nil || line.Crosstalk != nil {
			continue
		}
		msg := line.Delivery
		go func() {
			r := handle(command, msg)
			if msg.WantsReply {
				wire.WriteLine(l, r)
			}
		}()
	}
	return errDaemonGone
}

// relayReplies passes the lines typed or piped into stdin to the daemon.
// Stdin ending does not end the listening.
func (l *listening) relayReplies() {
	in := wire.NewScanner(os.Stdin)
	for in.Scan() {
		l.Write(append(bytes.Clone(in.Bytes()), '\n'))
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
