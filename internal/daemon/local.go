package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dittofleet/crosstalk/internal/wire"
)

// ErrRunning is returned by Run when a daemon already serves this user.
var ErrRunning = errors.New("the crosstalk daemon is already running")

// hubBack tells a listener that messages can reach it through the hub,
// after a time when they could not.
var hubBack = wire.Notice{Crosstalk: wire.Event{Hub: wire.HubConnected}}

// A listener that cannot take a line for this long is treated as gone, so
// one stuck program cannot hold up every message from the hub.
const deliverTimeout = 5 * time.Second

// listenLocal opens the socket programs on this machine connect to. Only
// this user can reach it: that is the whole of the local access check.
func listenLocal(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, ErrRunning
	}
	// Left behind by a daemon that did not get to clean up.
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (d *Daemon) serveLocal(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// Out of file descriptors, say. It passes, and the socket
			// must outlast it.
			d.log.Printf("local socket: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go d.handle(ctx, c)
	}
}

func (d *Daemon) handle(ctx context.Context, c net.Conn) {
	d.mu.Lock()
	d.locals[c] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.locals, c)
		d.mu.Unlock()
		c.Close()
	}()

	lines := wire.NewScanner(c)
	if !lines.Scan() {
		return
	}
	var req wire.Request
	if json.Unmarshal(lines.Bytes(), &req) != nil {
		wire.WriteLine(c, refusal(wire.ErrBadRequest, "expected a line of JSON with an op"))
		return
	}
	switch req.Op {
	case "send":
		wire.WriteLine(c, d.send(ctx, req))
	case "listen":
		d.listen(c, lines, req)
	case "post":
		wire.WriteLine(c, d.opPost(req))
	case "unpost":
		wire.WriteLine(c, d.opUnpost(req))
	case "read":
		wire.WriteLine(c, d.opRead(ctx, req))
	case "set":
		wire.WriteLine(c, d.opSet(req, false))
	case "unset":
		wire.WriteLine(c, d.opSet(req, true))
	case "get":
		wire.WriteLine(c, d.opGet(req))
	case "watch":
		d.opWatch(c, lines, req)
	case "devices":
		d.mu.Lock()
		res := wire.Response{OK: true, Hub: d.hub, Devices: append([]wire.Device{}, d.devices...)}
		d.mu.Unlock()
		wire.WriteLine(c, res)
	case "remove":
		if err := d.toHub(wire.ToHub{T: "remove", Name: req.Name}); err != nil {
			wire.WriteLine(c, refusal(wire.ErrOffline, "this machine is not connected to the hub"))
			return
		}
		wire.WriteLine(c, wire.Response{OK: true})
	default:
		wire.WriteLine(c, refusal(wire.ErrBadRequest, fmt.Sprintf("unknown op %q", req.Op)))
	}
}

func refusal(code, message string) wire.Response {
	return wire.Response{Error: code, Message: message}
}

func (d *Daemon) send(ctx context.Context, req wire.Request) wire.Response {
	if bad := checkBody(req, false, "body"); bad != nil {
		return *bad
	}

	d.mu.Lock()
	connected := d.conn != nil
	devices := append([]wire.Device{}, d.devices...)
	others := d.others()
	d.mu.Unlock()
	if !connected {
		return refusal(wire.ErrOffline, "this machine is not connected to the hub")
	}

	if req.To == "*" {
		sent := []string{}
		for _, name := range others {
			if d.post(name, wire.Sealed{K: "req", Name: req.Name, Body: req.Body}) == nil {
				sent = append(sent, name)
			}
		}
		return wire.Response{OK: true, Sent: sent}
	}

	var names []string
	known := false
	for _, device := range devices {
		names = append(names, device.Name)
		known = known || device.Name == req.To
	}
	if !known {
		return refusal(wire.ErrUnreachable, fmt.Sprintf("no device named %q (have: %s)", req.To, strings.Join(names, ", ")))
	}

	timeout := DefaultTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(min(req.Timeout, maxTimeout.Seconds()) * float64(time.Second))
	}
	id := rand.Text()
	reply := make(chan wire.Sealed, 1)
	d.mu.Lock()
	d.pending[id] = reply
	d.mu.Unlock()
	defer d.takePending(id)

	if err := d.post(req.To, wire.Sealed{K: "req", ID: id, Name: req.Name, Body: req.Body, Reply: true}); err != nil {
		if errors.Is(err, errTooLarge) {
			return refusal(wire.ErrTooLarge, "the message is too large to send through the hub")
		}
		return refusal(wire.ErrOffline, "this machine is not connected to the hub")
	}
	select {
	case res := <-reply:
		switch {
		case res.Error == wire.ErrUnreachable:
			return refusal(res.Error, req.To+" is not connected to the hub")
		case res.Error != "":
			return refusal(res.Error, res.Message)
		}
		return wire.Response{OK: true, Body: res.Body}
	case <-time.After(timeout):
		return refusal(wire.ErrTimeout, fmt.Sprintf("%s did not answer within %s", req.To, timeout))
	case <-ctx.Done():
		return refusal(wire.ErrOffline, "the daemon is stopping")
	}
}

// listener is a program on this machine receiving messages for one name.
type listener struct {
	conn net.Conn
	wmu  sync.Mutex

	mu sync.Mutex
	// Deliveries the program has yet to answer, with who asked.
	waiting map[string]string
	// Set once the program has gone, when nothing more is delivered.
	closed bool
}

// deliver writes a message to the program, and reports whether it got there.
func (l *listener) deliver(from string, msg wire.Sealed) bool {
	body := msg.Body
	if len(body) == 0 {
		body = json.RawMessage("null")
	}
	l.mu.Lock()
	closed := l.closed
	if msg.Reply && !closed {
		l.waiting[msg.ID] = from
	}
	l.mu.Unlock()
	if closed {
		return false
	}
	return l.write(wire.Delivery{ID: msg.ID, From: from, Name: msg.Name, Body: body, WantsReply: msg.Reply}, msg.ID)
}

// write sends the program one line, and reports whether it got there. id is
// the delivery's, if the line is one, to take back should it not arrive.
func (l *listener) write(line any, id string) bool {
	l.wmu.Lock()
	l.conn.SetWriteDeadline(time.Now().Add(deliverTimeout))
	err := wire.WriteLine(l.conn, line)
	l.wmu.Unlock()
	if err != nil {
		// Before the close, so that listen does not answer it as well.
		l.take(id)
		// Ends the read in listen, which takes the listener off its name.
		l.conn.Close()
		return false
	}
	return true
}

func (l *listener) take(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	from, ok := l.waiting[id]
	delete(l.waiting, id)
	return from, ok
}

// listen serves one program for as long as its connection stays open: each
// message for the name goes out as a line, and each line that comes back is
// the reply to one of them.
func (d *Daemon) listen(c net.Conn, lines *bufio.Scanner, req wire.Request) {
	if req.Name == "" {
		wire.WriteLine(c, refusal(wire.ErrBadRequest, "a name to listen on is required"))
		return
	}
	l := &listener{conn: c, waiting: map[string]string{}}
	// Held until the program has its answer, so no message is written to it
	// ahead of that.
	l.wmu.Lock()
	d.mu.Lock()
	_, taken := d.listeners[req.Name]
	if !taken {
		d.listeners[req.Name] = l
	}
	// Messages could not reach the program before now, so it hears that
	// the hub is there, as it would have on the hub coming back. If the
	// hub has yet to list the devices, it hears when it does.
	connected := d.hub == wire.HubConnected
	d.mu.Unlock()
	if taken {
		l.wmu.Unlock()
		wire.WriteLine(c, refusal(wire.ErrNameTaken, fmt.Sprintf("another app on this machine is already listening on %q", req.Name)))
		return
	}
	defer func() {
		d.mu.Lock()
		delete(d.listeners, req.Name)
		d.mu.Unlock()
		l.mu.Lock()
		unanswered := l.waiting
		l.waiting, l.closed = map[string]string{}, true
		l.mu.Unlock()
		for id, from := range unanswered {
			d.respond(from, id, nil, wire.ErrNoReply, fmt.Sprintf("the listener on %s went away before answering", d.cfg.Name))
		}
	}()

	c.SetWriteDeadline(time.Now().Add(deliverTimeout))
	err := wire.WriteLine(c, wire.Response{OK: true})
	if err == nil && connected {
		err = wire.WriteLine(c, hubBack)
	}
	l.wmu.Unlock()
	if err != nil {
		return
	}
	for lines.Scan() {
		var reply wire.Reply
		if json.Unmarshal(lines.Bytes(), &reply) != nil {
			continue
		}
		from, ok := l.take(reply.ID)
		if !ok {
			continue
		}
		if len(reply.Body) > wire.MaxBody {
			d.respond(from, reply.ID, nil, wire.ErrTooLarge, "the reply was over the size limit")
			continue
		}
		d.respond(from, reply.ID, reply.Body, reply.Error, reply.Message)
	}
}
