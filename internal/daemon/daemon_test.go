package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dittofleet/crosstalk/internal/config"
	"github.com/dittofleet/crosstalk/internal/secret"
	"github.com/dittofleet/crosstalk/internal/wire"
)

// fakeHub does what the worker does, as far as a daemon can tell: it passes
// boxes between the devices connected to it and nacks one for a device that
// is not. It also keeps every box it passed, to play back later.
type fakeHub struct {
	token string

	mu     sync.Mutex
	conns  map[string]*websocket.Conn
	passed []wire.ToHub
	// Every device on the list, connected or not.
	known map[string]bool
	// How many times each device has connected.
	connects map[string]int
	// Set to stop answering pings, as a connection that has died without
	// closing would.
	silent bool
}

func (h *fakeHub) connected(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connects[name]
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.URL.Query().Get("name")
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(wire.MaxFrame)
	h.mu.Lock()
	h.conns[name] = c
	h.known[name] = true
	h.connects[name]++
	h.mu.Unlock()
	h.announce()
	defer func() {
		h.mu.Lock()
		delete(h.conns, name)
		h.mu.Unlock()
		h.announce()
	}()
	for {
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		if string(data) == "ping" {
			h.mu.Lock()
			silent := h.silent
			h.mu.Unlock()
			if !silent {
				c.Write(r.Context(), websocket.MessageText, []byte("pong"))
			}
			continue
		}
		var frame wire.ToHub
		if json.Unmarshal(data, &frame) != nil || frame.T != "msg" {
			continue
		}
		h.mu.Lock()
		h.passed = append(h.passed, frame)
		h.mu.Unlock()
		if !h.send(frame.To, wire.FromHub{T: "msg", From: name, Box: frame.Box}) {
			h.send(name, wire.FromHub{T: "nack", Ref: frame.Ref})
		}
	}
}

func (h *fakeHub) send(to string, frame wire.FromHub) bool {
	h.mu.Lock()
	c := h.conns[to]
	h.mu.Unlock()
	if c == nil {
		return false
	}
	data, _ := json.Marshal(frame)
	return c.Write(context.Background(), websocket.MessageText, data) == nil
}

func (h *fakeHub) announce() {
	h.mu.Lock()
	var devices []wire.Device
	for name := range h.known {
		devices = append(devices, wire.Device{Name: name, Online: h.conns[name] != nil})
	}
	h.mu.Unlock()
	for _, device := range devices {
		h.send(device.Name, wire.FromHub{T: "devices", Devices: devices})
	}
}

// machine is one daemon with its own config and socket, as on its own Mac.
type machine struct {
	t      *testing.T
	daemon *Daemon
	socket string
	// Stops the daemon's connection to the hub, as the machine going to
	// sleep would.
	stop func()
}

func (m *machine) dial(req wire.Request) (net.Conn, *bufio.Scanner) {
	m.t.Helper()
	c, err := net.Dial("unix", m.socket)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { c.Close() })
	wire.WriteLine(c, req)
	return c, wire.NewScanner(c)
}

func (m *machine) ask(req wire.Request) wire.Response {
	m.t.Helper()
	_, lines := m.dial(req)
	var res wire.Response
	if !lines.Scan() || json.Unmarshal(lines.Bytes(), &res) != nil {
		m.t.Fatalf("no answer to %+v", req)
	}
	return res
}

// listen answers every message on name with reply(body).
func (m *machine) listen(name string, reply func(wire.Delivery) wire.Reply) {
	m.t.Helper()
	c, lines := m.dial(wire.Request{Op: "listen", Name: name})
	if !lines.Scan() || !strings.Contains(lines.Text(), `"ok":true`) {
		m.t.Fatalf("listen %s: %s", name, lines.Text())
	}
	go func() {
		for lines.Scan() {
			var msg wire.Delivery
			json.Unmarshal(lines.Bytes(), &msg)
			r := reply(msg)
			r.ID = msg.ID
			wire.WriteLine(c, r)
		}
	}()
}

// fleet is a hub and the machines joined to it.
type fleet struct {
	t   *testing.T
	hub *fakeHub
	url string
	key string
}

func newFleet(t *testing.T) *fleet {
	t.Helper()
	key, _ := secret.NewKey()
	keys, _ := secret.Derive(key)
	hub := &fakeHub{token: keys.HubToken, conns: map[string]*websocket.Conn{}, connects: map[string]int{},
		// A device that has been seen and is away now.
		known: map[string]bool{"asleep": true}}
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	return &fleet{t: t, hub: hub, url: server.URL, key: key}
}

// add starts a machine's daemon and waits until it is connected. tune, if
// given, changes the daemon's pace before it starts.
func (f *fleet) add(name string, tune ...func(*pace)) *machine {
	f.t.Helper()
	// Short, because a socket path has little room.
	dir, err := os.MkdirTemp("/tmp", "ct")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { os.RemoveAll(dir) })
	d, err := New(&config.Config{Hub: f.url, Name: name, ID: name + "-0000000000000000", Key: f.key})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, tweak := range tune {
		tweak(&d.pace)
	}
	if d.store, err = loadStore(dir + "/values.json"); err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &machine{t: f.t, daemon: d, socket: dir + "/sock", stop: cancel}
	ln, err := listenLocal(m.socket)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		cancel()
		ln.Close()
	})
	go d.serveLocal(ctx, ln)
	go d.hubLoop(ctx)
	eventually(f.t, name+" connecting", func() bool {
		for _, device := range m.ask(wire.Request{Op: "devices"}).Devices {
			if device.Self && device.Online {
				return true
			}
		}
		return false
	})
	return m
}

// start runs a hub with two machines on it.
func start(t *testing.T, tune ...func(*pace)) (*fakeHub, map[string]*machine) {
	t.Helper()
	f := newFleet(t)
	machines := map[string]*machine{"lychee": f.add("lychee", tune...), "macbook": f.add("macbook", tune...)}
	// Until the first has heard about the second.
	eventually(t, "the machines seeing each other", func() bool {
		return len(machines["lychee"].ask(wire.Request{Op: "devices"}).Devices) == 3
	})
	return f.hub, machines
}

func TestSendReachesTheListenerAndBringsBackItsReply(t *testing.T) {
	_, machines := start(t)
	machines["lychee"].listen("tagteam", func(msg wire.Delivery) wire.Reply {
		if msg.From != "macbook" || msg.Name != "tagteam" || !msg.WantsReply {
			t.Errorf("delivered %+v", msg)
		}
		return wire.Reply{Body: json.RawMessage(`{"got":` + string(msg.Body) + `}`)}
	})

	res := machines["macbook"].ask(wire.Request{Op: "send", To: "lychee", Name: "tagteam", Body: json.RawMessage(`{"do":"refresh"}`)})
	if !res.OK || string(res.Body) != `{"got":{"do":"refresh"}}` {
		t.Fatalf("send = %+v", res)
	}
}

func TestSendSaysWhyItFailed(t *testing.T) {
	_, machines := start(t)
	machines["lychee"].listen("refuses", func(wire.Delivery) wire.Reply {
		return wire.Reply{Error: "not-now", Message: "busy"}
	})

	cases := []struct {
		req  wire.Request
		code string
	}{
		{wire.Request{Op: "send", To: "lychee", Name: "nobody"}, wire.ErrNoListener},
		{wire.Request{Op: "send", To: "asleep", Name: "tagteam"}, wire.ErrUnreachable},
		{wire.Request{Op: "send", To: "stranger", Name: "tagteam"}, wire.ErrUnreachable},
		{wire.Request{Op: "send", To: "lychee", Name: "refuses"}, "not-now"},
		{wire.Request{Op: "send", To: "lychee"}, wire.ErrBadRequest},
		{wire.Request{Op: "send", To: "lychee", Name: "x", Body: json.RawMessage(strings.Repeat("1", wire.MaxBody+1))}, wire.ErrTooLarge},
	}
	for _, c := range cases {
		if res := machines["macbook"].ask(c.req); res.OK || res.Error != c.code {
			t.Errorf("%s to %s/%s = %+v, want %s", c.req.Op, c.req.To, c.req.Name, res, c.code)
		}
	}
	if res := machines["lychee"].ask(wire.Request{Op: "listen", Name: "refuses"}); res.Error != wire.ErrNameTaken {
		t.Errorf("a second listener = %+v", res)
	}
}

func TestSendToAllGoesToEveryOtherConnectedDevice(t *testing.T) {
	_, machines := start(t)
	got := make(chan wire.Delivery, 1)
	machines["lychee"].listen("status", func(msg wire.Delivery) wire.Reply {
		got <- msg
		return wire.Reply{}
	})
	res := machines["macbook"].ask(wire.Request{Op: "send", To: "*", Name: "status", Body: json.RawMessage(`1`)})
	if !res.OK || len(res.Sent) != 1 || res.Sent[0] != "lychee" {
		t.Fatalf("send to all = %+v", res)
	}
	select {
	case msg := <-got:
		if msg.WantsReply {
			t.Error("a message to all asked for a reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never delivered")
	}
}

// The hub is not trusted with more than passing boxes along. Everything it
// could do with one it has already seen must come to nothing.
func TestAHubCannotReplayRedirectOrForge(t *testing.T) {
	hub, machines := start(t)
	deliveries := make(chan wire.Delivery, 8)
	for _, name := range []string{"lychee", "macbook"} {
		machines[name].listen("tagteam", func(msg wire.Delivery) wire.Reply {
			deliveries <- msg
			return wire.Reply{}
		})
	}
	if res := machines["macbook"].ask(wire.Request{Op: "send", To: "lychee", Name: "tagteam"}); !res.OK {
		t.Fatalf("send = %+v", res)
	}
	<-deliveries
	hub.mu.Lock()
	box := hub.passed[0].Box
	hub.mu.Unlock()

	// The same box again, the box handed to another device, and the box
	// passed off as coming from another device.
	hub.send("lychee", wire.FromHub{T: "msg", From: "macbook", Box: box})
	hub.send("macbook", wire.FromHub{T: "msg", From: "macbook", Box: box})
	hub.send("lychee", wire.FromHub{T: "msg", From: "asleep", Box: box})

	// A box held back until it is stale.
	keys := machines["macbook"].daemon.keys
	old, _ := json.Marshal(wire.Sealed{K: "req", ID: "held-back", Name: "tagteam", TS: time.Now().Add(-freshness - time.Minute).UnixMilli()})
	stale, _ := keys.Seal(old, "macbook", "lychee")
	hub.send("lychee", wire.FromHub{T: "msg", From: "macbook", Box: stale})

	// Something real after them, so there is an end to wait for.
	if res := machines["macbook"].ask(wire.Request{Op: "send", To: "lychee", Name: "tagteam", Body: json.RawMessage(`"real"`)}); !res.OK {
		t.Fatalf("send = %+v", res)
	}
	if msg := <-deliveries; string(msg.Body) != `"real"` {
		t.Fatalf("delivered %+v, which the hub made up", msg)
	}
}

// A value is taken in once too: replaying an old put must not bring back a
// value its device has since taken back.
func TestAHubCannotReplayAValue(t *testing.T) {
	hub, machines := start(t)
	keys := machines["lychee"].daemon.keys
	box := func(id string, p wire.Put) string {
		plain, _ := json.Marshal(wire.Sealed{K: "put", ID: id, TS: time.Now().UnixMilli(), Put: &p})
		sealed, _ := keys.Seal(plain, "lychee", "macbook")
		return sealed
	}
	posted := box("p1", wire.Put{Kind: wire.Posted, Name: "status", Value: json.RawMessage(`1`), Version: 1})
	hub.send("macbook", wire.FromHub{T: "msg", From: "lychee", Box: posted})
	eventually(t, "the value arriving", func() bool { return machines["macbook"].read("lychee", "status") == `1` })
	hub.send("macbook", wire.FromHub{T: "msg", From: "lychee", Box: box("p2", wire.Put{Kind: wire.Posted, Name: "status", Version: 2, Deleted: true})})
	eventually(t, "the value going", func() bool { return machines["macbook"].read("lychee", "status") == "" })

	hub.send("macbook", wire.FromHub{T: "msg", From: "lychee", Box: posted})
	// Something real after it, so there is an end to wait for.
	machines["lychee"].do(wire.Request{Op: "post", Name: "after", Body: json.RawMessage(`2`)})
	eventually(t, "the real value", func() bool { return machines["macbook"].read("lychee", "after") == `2` })
	if machines["macbook"].read("lychee", "status") != "" {
		t.Error("a replayed put brought back a value")
	}
}

// HTML-ish text is not escaped on the way, so it fits as well as any other.
func TestABodyAtTheLimitGetsThrough(t *testing.T) {
	_, machines := start(t)
	machines["lychee"].listen("big", func(msg wire.Delivery) wire.Reply {
		return wire.Reply{Body: json.RawMessage(strconv.Itoa(len(msg.Body)))}
	})
	body := `"` + strings.Repeat("<&>", (wire.MaxBody-2)/3) + `"`
	res := machines["macbook"].ask(wire.Request{Op: "send", To: "lychee", Name: "big", Body: json.RawMessage(body)})
	if !res.OK || string(res.Body) != strconv.Itoa(len(body)) {
		t.Fatalf("send = %+v", res)
	}
}

func TestAWrongKeyIsTurnedAway(t *testing.T) {
	hub, _ := start(t)
	server := httptest.NewServer(hub)
	defer server.Close()
	key, _ := secret.NewKey()
	keys, _ := secret.Derive(key)
	_, err := Probe(context.Background(), &config.Config{Hub: server.URL, Name: "intruder", ID: "intruder-00000000"}, keys)
	if err != ErrUnauthorized {
		t.Fatalf("Probe = %v", err)
	}
}

// eventually waits for something a daemon does in the background.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sendWorks(m *machine) bool {
	return m.ask(wire.Request{Op: "send", To: "lychee", Name: "tagteam"}).OK
}

func TestAConnectionThatGoesQuietIsRemade(t *testing.T) {
	quietLimit := 60 * time.Millisecond
	hub, machines := start(t, func(p *pace) { p.pingEvery, p.quietLimit = 10*time.Millisecond, quietLimit })
	machines["lychee"].listen("tagteam", func(wire.Delivery) wire.Reply { return wire.Reply{} })

	// While pings are answered the connection is left alone.
	time.Sleep(3 * quietLimit)
	if n := hub.connected("macbook"); n != 1 {
		t.Fatalf("connected %d times while the hub was answering", n)
	}

	hub.mu.Lock()
	hub.silent = true
	hub.mu.Unlock()
	eventually(t, "a second connection", func() bool { return hub.connected("macbook") >= 2 })
	hub.mu.Lock()
	hub.silent = false
	hub.mu.Unlock()
	eventually(t, "a send working again", func() bool { return sendWorks(machines["macbook"]) })
}

func TestMovingToAnotherNetworkRemakesTheConnection(t *testing.T) {
	var mu sync.Mutex
	addrs := "192.168.1.20/24"
	hub, machines := start(t, func(p *pace) {
		p.watchEvery = 5 * time.Millisecond
		p.localAddrs = func() string {
			mu.Lock()
			defer mu.Unlock()
			return addrs
		}
	})
	machines["lychee"].listen("tagteam", func(wire.Delivery) wire.Reply { return wire.Reply{} })

	mu.Lock()
	addrs = "10.0.0.7/8"
	mu.Unlock()
	// Not before the addresses have had time to settle.
	time.Sleep(settle / 2)
	if n := hub.connected("macbook"); n != 1 {
		t.Fatalf("reconnected %d times before the network settled", n-1)
	}
	eventually(t, "a second connection", func() bool { return hub.connected("macbook") >= 2 })
	eventually(t, "a send working again", func() bool { return sendWorks(machines["macbook"]) })
	if n := hub.connected("macbook"); n != 2 {
		t.Fatalf("connected %d times for one change of network", n)
	}
}
