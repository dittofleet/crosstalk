// Package cmd holds the commands the crosstalk binary runs.
package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"

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

// ask sends one request and returns its one response.
func ask(req wire.Request) (*wire.Response, error) {
	c, lines, err := connect(req)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return answer(lines)
}
