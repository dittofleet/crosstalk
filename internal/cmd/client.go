// Package cmd holds the commands the crosstalk binary runs.
package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/dittofleet/crosstalk/internal/config"
	"github.com/dittofleet/crosstalk/internal/wire"
)

// A Failure is an answer from the daemon that says no. Its code is one of
// the wire.Err values, for a program to tell them apart.
type Failure struct {
	Code    string
	Message string
}

func (f *Failure) Error() string {
	if f.Message == "" {
		return f.Code
	}
	return fmt.Sprintf("%s (%s)", f.Message, f.Code)
}

// connect opens the daemon's socket and sends the first line. It returns the
// connection with the scanner that reads its lines.
func connect(req wire.Request) (net.Conn, *bufio.Scanner, error) {
	if _, err := config.Load(); err != nil {
		return nil, nil, err
	}
	c, err := net.Dial("unix", config.SocketPath())
	if err != nil {
		return nil, nil, errors.New("the crosstalk daemon is not running, start it with `crosstalk daemon`")
	}
	if err := wire.WriteLine(c, req); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, wire.NewScanner(c), nil
}

// answer reads the daemon's response to the first line.
func answer(lines *bufio.Scanner) (*wire.Response, error) {
	if !lines.Scan() {
		return nil, errors.New("the crosstalk daemon closed the connection without answering")
	}
	var res wire.Response
	if err := json.Unmarshal(lines.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("unreadable answer from the daemon: %w", err)
	}
	if !res.OK {
		return nil, &Failure{Code: res.Error, Message: res.Message}
	}
	return &res, nil
}

// errDaemonGone ends a connection that stays open, like a watch, when the
// daemon closes it: when it restarts, or when it gives up on a listener
// that stopped taking messages.
var errDaemonGone = errors.New("the crosstalk daemon closed the connection")

// patience is how a listen or watch waits for the daemon to come back: how
// often it looks, and for how long before it gives up.
type patience struct {
	retry, giveUp time.Duration
}

// A restart takes seconds, or ten when launchd holds back a daemon that
// has just exited. A daemon gone for longer than this was stopped on
// purpose, or cannot start, and waiting on would hide that.
var restartPatience = patience{retry: time.Second, giveUp: time.Minute}

// stayConnected runs once, and again each time the daemon goes away, so a
// listen or watch carries on across the daemon restarting for an update.
//
// At the start anything that goes wrong is final: a daemon that is not
// running is for the user to start. Once connected, it tries again through
// anything for as long as p allows, a refusal included, since a listener
// coming back can briefly find its name still held by its old self. A
// machine that has left the hub is final at any point.
func stayConnected(p patience, once func() error) error {
	var lost time.Time
	for first := true; ; first = false {
		err := once()
		switch {
		case first && !errors.Is(err, errDaemonGone):
			return err
		case errors.Is(err, config.ErrNotJoined):
			return err
		case errors.Is(err, errDaemonGone):
			lost = time.Now()
			fmt.Fprintln(os.Stderr, "crosstalk: lost the connection to the daemon, connecting again")
		case time.Since(lost) > p.giveUp:
			return fmt.Errorf("gave up waiting for the crosstalk daemon: %w", err)
		}
		time.Sleep(p.retry)
	}
}

// ask sends one request and returns its one response.
func ask(req wire.Request) (*wire.Response, error) {
	c, lines, err := connect(req)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return answer(lines)
}
