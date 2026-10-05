// Package daemon is the one long-running process on a machine. It keeps a
// connection to the hub, seals and opens the messages that cross it, and
// serves the programs on this machine over a local socket.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/dittofleet/crosstalk/internal/config"
	"github.com/dittofleet/crosstalk/internal/secret"
	"github.com/dittofleet/crosstalk/internal/wire"
)

const (
	// DefaultTimeout is how long a send waits for its reply.
	DefaultTimeout = 10 * time.Second
	maxTimeout     = 10 * time.Minute

	dialTimeout  = 15 * time.Second
	writeTimeout = 10 * time.Second
	// A message is only accepted this close to when it was sent, so the hub
	// cannot hold one back and deliver it later.
	freshness = 2 * time.Minute

	firstRetry = time.Second
	maxRetry   = 30 * time.Second
	// A connection that lasted this long was a good one, so the next
	// failure starts from firstRetry again.
	stableAfter = 30 * time.Second
	// For a refusal that only the user can fix, like a name another machine
	// holds: worth a look now and then, not a tight loop.
	parkedRetry = time.Minute
	// Undocking or joining a network changes addresses several times in
	// quick succession. One reconnect, once they have settled, is enough.
	settle = time.Second
)

// pace is how often a daemon checks on its connection. It is a value on
// the daemon, not constants, so that tests can run one fast.
type pace struct {
	// The hub answers a ping without waking, so these cost nothing there.
	pingEvery time.Duration
	// With a ping answered every pingEvery, this much silence means two
	// went unanswered and the connection is dead, whatever the socket
	// still claims.
	quietLimit time.Duration
	// How often to look for the machine having slept or moved network.
	// Neither look touches the network.
	watchEvery time.Duration
	// A gap this long between two looks means the machine was asleep.
	sleptAfter time.Duration
	// This machine's addresses, as one string to compare.
	localAddrs func() (string, error)
}

var defaultPace = pace{
	pingEvery:  15 * time.Second,
	quietLimit: 35 * time.Second,
	watchEvery: 2 * time.Second,
	sleptAfter: 10 * time.Second,
	localAddrs: interfaceAddrs,
}

// What Dial and Probe return when the hub turns a device away.
var (
	ErrUnauthorized  = errors.New("the hub rejected the key")
	ErrNameTaken     = errors.New("another machine already uses this name on the hub")
	ErrNotConfigured = errors.New("the hub has no HUB_TOKEN set")
	ErrRefused       = errors.New("the hub refused this device (a bad name, or a hub that needs a newer crosstalk)")
)

type Daemon struct {
	cfg  *config.Config
	keys *secret.Keys
	log  *log.Logger
	pace pace

	// One writer on the hub connection at a time.
	wmu sync.Mutex

	// Holds one value when the connection should be remade at once.
	nudge chan string

	mu      sync.Mutex
	conn    *websocket.Conn // nil while not connected
	hub     string
	devices []wire.Device
	// Whether the hub has sent its list of devices on this connection.
	// Until it has, it may yet turn this machine away, and every other
	// machine looks offline.
	listed bool
	// Programs on this machine listening on a name.
	listeners map[string]*listener
	// Sends from this machine that are waiting for a reply, by message id.
	pending map[string]chan wire.Sealed
	// Messages already taken in, so one replayed by the hub is dropped, and
	// when seen was last cleared of ids too old to matter.
	seen  map[string]time.Time
	swept time.Time
	// Local connections, closed when the daemon stops.
	locals map[net.Conn]struct{}
	// Posted and synced values, and the programs watching them.
	store    *store
	watchers map[*watcher]struct{}
	// Posted values other devices took back lately, by device and name.
	taken map[string]takenBack
}

func New(cfg *config.Config) (*Daemon, error) {
	keys, err := secret.Derive(cfg.Key)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		cfg:       cfg,
		keys:      keys,
		log:       log.New(os.Stderr, "", log.LstdFlags),
		pace:      defaultPace,
		hub:       wire.HubConnecting,
		nudge:     make(chan string, 1),
		listeners: map[string]*listener{},
		pending:   map[string]chan wire.Sealed{},
		seen:      map[string]time.Time{},
		locals:    map[net.Conn]struct{}{},
		store:     newStore(""),
		watchers:  map[*watcher]struct{}{},
		taken:     map[string]takenBack{},
	}, nil
}

// Run serves until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := listenLocal(config.SocketPath())
	if err != nil {
		return err
	}
	if d.store, err = loadStore(config.ValuesPath()); err != nil {
		ln.Close()
		return err
	}
	go d.serveLocal(ctx, ln)
	d.log.Printf("%s: serving %s", d.cfg.Name, config.SocketPath())

	d.hubLoop(ctx)

	ln.Close()
	os.Remove(config.SocketPath())
	d.mu.Lock()
	for c := range d.locals {
		c.Close()
	}
	d.mu.Unlock()
	return nil
}

// Dial opens a connection to the hub.
func Dial(ctx context.Context, cfg *config.Config, keys *secret.Keys) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, cfg.ConnectURL(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + keys.HubToken}},
	})
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return nil, ErrUnauthorized
			case http.StatusServiceUnavailable:
				return nil, ErrNotConfigured
			case http.StatusBadRequest:
				return nil, ErrRefused
			}
		}
		return nil, err
	}
	c.SetReadLimit(wire.MaxFrame)
	return c, nil
}

// Probe connects once and returns the hub's device list, to check a hub
// address, key and name before they are saved.
func Probe(ctx context.Context, cfg *config.Config, keys *secret.Keys) ([]wire.Device, error) {
	c, err := Dial(ctx, cfg, keys)
	if err != nil {
		return nil, err
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return nil, closeReason(err)
		}
		var frame wire.FromHub
		if json.Unmarshal(data, &frame) == nil && frame.T == "devices" {
			c.Close(websocket.StatusNormalClosure, "")
			return frame.Devices, nil
		}
	}
}

func closeReason(err error) error {
	if websocket.CloseStatus(err) == wire.CloseNameTaken {
		return ErrNameTaken
	}
	return err
}

// hubLoop keeps one connection to the hub for as long as the daemon runs,
// reconnecting after sleep, network changes and hub restarts.
func (d *Daemon) hubLoop(ctx context.Context) {
	go d.watch(ctx)
	retry := firstRetry
	for ctx.Err() == nil {
		started := time.Now()
		err := d.session(ctx)
		if ctx.Err() != nil {
			return
		}
		select {
		case why := <-d.nudge:
			// The connection was closed on purpose, so there is nothing
			// to back off from.
			d.log.Printf("hub: reconnecting, %s", why)
			retry = firstRetry
			continue
		default:
		}

		state, wait := wire.HubConnecting, retry
		switch {
		case errors.Is(err, ErrNameTaken):
			state, wait = wire.HubNameTaken, parkedRetry
		case errors.Is(err, ErrUnauthorized):
			state, wait = wire.HubUnauthorized, parkedRetry
		case time.Since(started) > stableAfter:
			retry, wait = firstRetry, firstRetry
		}
		retry = min(retry*2, maxRetry)
		d.mu.Lock()
		d.hub = state
		d.mu.Unlock()
		d.log.Printf("hub: %v, trying again in %s", err, wait.Round(time.Second))

		// Spread out, so machines that lost the hub together do not all
		// come back in the same instant.
		wait += time.Duration(mrand.Int64N(int64(wait)/4 + 1))
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		case why := <-d.nudge:
			d.log.Printf("hub: trying now, %s", why)
			retry = firstRetry
		}
	}
}

// watch notices the two things that leave a connection looking open while
// nothing can cross it: the machine sleeping, and the machine moving to
// another network. Either way the connection is remade at once, where
// waiting for pings to go unanswered would take most of a minute.
func (d *Daemon) watch(ctx context.Context) {
	tick := time.NewTicker(d.pace.watchEvery)
	defer tick.Stop()
	// Wall-clock time, with the monotonic reading stripped: the monotonic
	// clock stops while the machine sleeps, which is exactly the gap this
	// is looking for.
	last := time.Now().Round(0)
	addrs, _ := d.pace.localAddrs()
	var changed time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now().Round(0)
		gap := now.Sub(last)
		last = now
		d.expire()
		if gap > d.pace.sleptAfter {
			changed = time.Time{}
			if current, err := d.pace.localAddrs(); err == nil {
				addrs = current
			}
			d.reconnect(fmt.Sprintf("this machine was asleep for %s", gap.Round(time.Second)))
			continue
		}
		// A look that fails says nothing about the network, so it is
		// not taken for a move to one with no addresses.
		if current, err := d.pace.localAddrs(); err == nil && current != addrs {
			addrs, changed = current, now
		}
		if !changed.IsZero() && now.Sub(changed) >= settle {
			changed = time.Time{}
			d.reconnect("this machine's network changed")
		}
	}
}

// reconnect drops the connection, if there is one, and has hubLoop make a
// new one without waiting.
func (d *Daemon) reconnect(why string) {
	select {
	case d.nudge <- why:
	default:
	}
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c != nil {
		c.CloseNow()
	}
}

// interfaceAddrs lists this machine's addresses that say which network it
// is on.
func interfaceAddrs() (string, error) {
	all, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	return routable(all), nil
}

// routable keeps the addresses that can reach past this machine's own
// link, private ones included, as one string to compare. Loopback never
// changes. Link-local addresses come and go by themselves: macOS adds and
// drops one on llw0, the interface for AirDrop and Continuity, every few
// minutes, and none of them can reach the hub.
func routable(all []net.Addr) string {
	var addrs []string
	for _, addr := range all {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || !ipnet.IP.IsGlobalUnicast() {
			continue
		}
		addrs = append(addrs, ipnet.String())
	}
	slices.Sort(addrs)
	return strings.Join(addrs, " ")
}

// session runs one connection until it ends, and says why it did.
func (d *Daemon) session(ctx context.Context) error {
	c, err := Dial(ctx, d.cfg, d.keys)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var lastHeard atomic.Int64
	lastHeard.Store(time.Now().UnixNano())
	go d.keepAlive(ctx, c, &lastHeard)

	d.mu.Lock()
	d.conn, d.hub = c, wire.HubConnected
	d.mu.Unlock()
	defer d.dropped(c)

	for {
		kind, data, err := c.Read(ctx)
		if err != nil {
			return closeReason(err)
		}
		lastHeard.Store(time.Now().UnixNano())
		if kind != websocket.MessageText || string(data) == "pong" {
			continue
		}
		d.fromHub(data)
	}
}

// keepAlive pings the hub and closes the connection when it has gone
// quiet, so session returns and a fresh one is made. This is what catches a
// connection that died for a reason watch cannot see, like the hub's side
// of it going away without a word.
func (d *Daemon) keepAlive(ctx context.Context, c *websocket.Conn, lastHeard *atomic.Int64) {
	tick := time.NewTicker(d.pace.pingEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		quiet := time.Since(time.Unix(0, lastHeard.Load())) > d.pace.quietLimit
		if quiet || d.write(c, []byte("ping")) != nil {
			c.CloseNow()
			return
		}
	}
}

// dropped clears what belonged to a connection that has ended.
func (d *Daemon) dropped(c *websocket.Conn) {
	d.mu.Lock()
	if d.conn == c {
		d.conn, d.hub, d.listed = nil, wire.HubConnecting, false
	}
	for i := range d.devices {
		d.devices[i].Online = false
	}
	waiting := d.pending
	d.pending = map[string]chan wire.Sealed{}
	d.mu.Unlock()
	// Their replies would come over the connection that is gone.
	for _, ch := range waiting {
		ch <- wire.Sealed{Error: wire.ErrOffline, Message: "lost the connection to the hub while waiting for the reply"}
	}
}

func (d *Daemon) write(c *websocket.Conn, data []byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, data)
}

var (
	errOffline  = errors.New("not connected to the hub")
	errTooLarge = errors.New("too large for the hub")
)

func (d *Daemon) toHub(frame wire.ToHub) error {
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c == nil {
		return errOffline
	}
	data, err := wire.Marshal(frame)
	if err != nil {
		return err
	}
	// The hub would refuse it, and the other daemon could not read it.
	if len(data) > wire.MaxFrame {
		return errTooLarge
	}
	return d.write(c, data)
}

// post seals a message for another device and hands it to the hub. One
// with no id is given one.
func (d *Daemon) post(to string, msg wire.Sealed) error {
	if msg.ID == "" {
		msg.ID = rand.Text()
	}
	msg.TS = time.Now().UnixMilli()
	plain, err := wire.Marshal(msg)
	if err != nil {
		return err
	}
	box, err := d.keys.Seal(plain, d.cfg.Name, to)
	if err != nil {
		return err
	}
	frame := wire.ToHub{T: "msg", To: to, Box: box}
	// Only a request has someone waiting to hear that it went nowhere.
	if msg.K == "req" {
		frame.Ref = msg.ID
	}
	return d.toHub(frame)
}

// respond answers a request from another device. It is best effort: if the
// reply cannot be sent, the sender's own timeout tells them.
func (d *Daemon) respond(to, id string, body json.RawMessage, code, message string) {
	_ = d.post(to, wire.Sealed{K: "res", ID: id, Body: body, Error: code, Message: message})
}

func (d *Daemon) fromHub(data []byte) {
	var frame wire.FromHub
	if json.Unmarshal(data, &frame) != nil {
		return
	}
	switch frame.T {
	case "devices":
		for i := range frame.Devices {
			frame.Devices[i].Self = frame.Devices[i].Name == d.cfg.Name
		}
		d.mu.Lock()
		was := map[string]bool{}
		for _, device := range d.devices {
			was[device.Name] = device.Online
		}
		d.devices = frame.Devices
		d.forgetRemoved()
		// The connection is ready for an app to catch up on what it
		// missed while there was none.
		var listeners []*listener
		if !d.listed {
			d.listed = true
			listeners = slices.Collect(maps.Values(d.listeners))
		}
		d.mu.Unlock()
		// A device that has just connected may have missed changes, and
		// made some. After this daemon reconnects, that is every device. It
		// is asked for its summary in return, because a device that comes
		// back before the hub saw it go never looks new to the others.
		for _, device := range frame.Devices {
			if device.Online && !device.Self && !was[device.Name] {
				d.sendHave(device.Name, true)
			}
		}
		for _, l := range listeners {
			l.write(hubBack)
		}
	case "nack":
		if ch := d.takePending(frame.Ref); ch != nil {
			ch <- wire.Sealed{Error: wire.ErrUnreachable}
		}
	case "msg":
		d.received(frame.From, frame.Box)
	}
}

func (d *Daemon) received(from, box string) {
	plain, err := d.keys.Open(box, from, d.cfg.Name)
	if err != nil {
		d.log.Printf("dropped a message from %s: %v", from, err)
		return
	}
	var msg wire.Sealed
	if json.Unmarshal(plain, &msg) != nil || msg.ID == "" {
		return
	}
	if age := time.Since(time.UnixMilli(msg.TS)); age > freshness || age < -freshness {
		d.log.Printf("dropped a message from %s sent %s ago: check that both machines' clocks are right", from, age.Round(time.Second))
		return
	}
	// A reply is taken once already, by the send waiting for it, and has the
	// id of the request it answers.
	if msg.K != "res" && d.alreadySeen(msg.ID) {
		return
	}
	switch msg.K {
	case "put":
		if msg.Put != nil {
			d.applyPut(from, *msg.Put)
		}
	case "have":
		if msg.Have != nil {
			d.applyHave(from, *msg.Have)
			if msg.Reply {
				d.sendHave(from, false)
			}
		}
	case "want":
		if msg.Want != nil {
			d.applyWant(from, *msg.Want)
		}
	case "res":
		if ch := d.takePending(msg.ID); ch != nil {
			ch <- msg
		}
	case "req":
		d.deliver(from, msg)
	}
}

// deliver hands a request to the program listening on its name.
func (d *Daemon) deliver(from string, msg wire.Sealed) {
	d.mu.Lock()
	l := d.listeners[msg.Name]
	d.mu.Unlock()
	if l != nil && l.deliver(from, msg) {
		return
	}
	if msg.Reply {
		d.respond(from, msg.ID, nil, wire.ErrNoListener,
			fmt.Sprintf("nothing on %s is listening on %q", d.cfg.Name, msg.Name))
	}
}

func (d *Daemon) takePending(id string) chan wire.Sealed {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch := d.pending[id]
	delete(d.pending, id)
	return ch
}

// alreadySeen records a message id, and reports whether it was there
// already. Ids are only kept for as long as a replay could still pass the
// freshness check.
func (d *Daemon) alreadySeen(id string) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.swept) > freshness {
		d.swept = now
		for old, at := range d.seen {
			if now.Sub(at) > 2*freshness {
				delete(d.seen, old)
			}
		}
	}
	if _, ok := d.seen[id]; ok {
		return true
	}
	d.seen[id] = now
	return false
}
