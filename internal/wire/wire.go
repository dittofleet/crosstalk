// Package wire holds the shapes crosstalk passes around: frames to and from
// the hub, the message sealed inside them, and the lines of JSON the daemon
// speaks on its local socket.
package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
)

// HubProtocol is sent on connecting. The hub turns away one it does not know.
const HubProtocol = "1"

// MaxBody is the largest body a message can carry, in bytes of JSON.
const MaxBody = 256 * 1024

// MaxFrame bounds one frame on the hub connection or one line on the local
// socket: a full body, sealed and encoded, with room for the envelope.
const MaxFrame = 512 * 1024

// CloseNameTaken is the code the hub closes a connection with when another
// machine already holds the name.
const CloseNameTaken = 4001

// The daemon's connection to the hub, as `crosstalk devices` reports it.
const (
	HubConnecting   = "connecting"
	HubConnected    = "connected"
	HubNameTaken    = "name-taken"
	HubUnauthorized = "unauthorized"
)

var deviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidDevice reports whether name can be a device's name.
func ValidDevice(name string) bool { return deviceName.MatchString(name) }

// ToHub is a frame a daemon sends the hub.
type ToHub struct {
	T    string `json:"t"` // "msg" or "remove"
	To   string `json:"to,omitempty"`
	Ref  string `json:"ref,omitempty"`
	Box  string `json:"box,omitempty"`
	Name string `json:"name,omitempty"`
}

// FromHub is a frame the hub sends a daemon.
type FromHub struct {
	T       string   `json:"t"` // "msg", "nack" or "devices"
	From    string   `json:"from,omitempty"`
	Box     string   `json:"box,omitempty"`
	Ref     string   `json:"ref,omitempty"`
	Devices []Device `json:"devices,omitempty"`
}

// Device is one machine on the hub's list.
type Device struct {
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	LastSeen int64  `json:"lastSeen"` // unix milliseconds
	Self     bool   `json:"self,omitempty"`
}

// Sealed is what travels inside a box, between two daemons.
type Sealed struct {
	// "req" and "res" carry a message and its reply. "put" carries one
	// value, "have" says which values a daemon holds, and "want" asks for
	// some of them. Reply is set on a req that waits for its res, and on a
	// have that asks for one back.
	K  string `json:"k"`
	ID string `json:"id"`
	// TS is when it was sent, in unix milliseconds. A daemon drops one that
	// is too old, so the hub cannot hold a message back and deliver it later.
	TS      int64           `json:"ts"`
	Name    string          `json:"name,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Reply   bool            `json:"reply,omitempty"`
	Error   string          `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
	Put     *Put            `json:"put,omitempty"`
	Have    *Summary        `json:"have,omitempty"`
	Want    *Summary        `json:"want,omitempty"`
}

// The two kinds of value. A posted value belongs to the device that posted
// it, and only that device changes it. A synced value is one value shared by
// every device, and the latest write wins.
const (
	Posted = "posted"
	Synced = "synced"
)

// Put is one value on its way to another daemon. A posted one is always the
// sender's own.
type Put struct {
	Kind  string          `json:"kind"`
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value,omitempty"`
	// Version orders the writes to a value. It only ever grows.
	Version int64 `json:"version"`
	// At is when it was written, Expires when it stops counting, both in
	// unix milliseconds. Expires is 0 for a value that stays.
	At      int64 `json:"at"`
	Expires int64 `json:"expires,omitempty"`
	Deleted bool  `json:"deleted,omitempty"`
	// By is the device that wrote a synced value.
	By string `json:"by,omitempty"`
}

// Stamp is what orders two writes to a synced value: the higher version,
// and between equal versions the later device name.
type Stamp struct {
	Version int64  `json:"version"`
	By      string `json:"by,omitempty"`
}

// Summary names values by kind. As a "have" it lists the sender's own posted
// values and every synced value it holds, with their versions, and its clock
// when it made the list. As a "want" it lists the ones to send back, and the
// versions mean nothing.
type Summary struct {
	Posted map[string]int64 `json:"posted,omitempty"`
	Synced map[string]Stamp `json:"synced,omitempty"`
	Clock  int64            `json:"clock,omitempty"`
}

// RefreshBody is the message `read --fresh` sends to the listener on a
// value's name, asking it to post again.
const RefreshBody = `{"refresh":true}`

// Errors a request to the daemon can end with.
const (
	// This machine's daemon is not connected to the hub.
	ErrOffline = "offline"
	// The other machine is not connected to the hub.
	ErrUnreachable = "unreachable"
	// The other machine is connected, but nothing there listens on the name.
	ErrNoListener = "no-listener"
	// The listener went away or answered with nothing usable.
	ErrNoReply    = "no-reply"
	ErrTimeout    = "timeout"
	ErrTooLarge   = "too-large"
	ErrBadRequest = "bad-request"
	// Another program on this machine already listens on the name.
	ErrNameTaken = "name-taken"
	// No value by that name: nothing posted by the device, or nothing set.
	ErrNoValue = "no-value"
)

// Request is the first line a program writes to the daemon's socket.
type Request struct {
	// Messages: "send", "listen". Posted values: "post", "unpost", "read".
	// Synced values: "set", "unset", "get". Both: "watch". The list of
	// machines: "devices", "remove".
	Op string `json:"op"`
	// To is a device name. "*" is every other connected device for a send,
	// which then waits for no reply, and every device for a read.
	To   string          `json:"to,omitempty"`
	Name string          `json:"name,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
	// Timeout is how long a send, or a fresh read, waits, in seconds.
	Timeout float64 `json:"timeout,omitempty"`
	// Expires is how long a posted value counts for, in seconds. 0 is for
	// as long as it is not replaced.
	Expires float64 `json:"expires,omitempty"`
	// Fresh has a read ask the device to post again first.
	Fresh bool `json:"fresh,omitempty"`
}

// Response is the daemon's answer to a Request.
type Response struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Sent    []string        `json:"sent,omitempty"`
	Hub     string          `json:"hub,omitempty"`
	Devices []Device        `json:"devices,omitempty"`
	// Changed is false for a post or set of the value already there, which
	// is not written again or sent anywhere.
	Changed *bool   `json:"changed,omitempty"`
	Entries []Entry `json:"entries,omitempty"`
}

// Entry is one value as a program sees it: in the answer to a read or get,
// and as each line of a watch.
type Entry struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Device is whose value a posted one is, and who last wrote a synced one.
	Device string          `json:"device"`
	Value  json.RawMessage `json:"value"`
	// At is when it was written, by the writer's clock.
	At string `json:"at"`
	// Deleted marks, in a watch, a value that has gone.
	Deleted bool `json:"deleted,omitempty"`
	// Fresh is set on a fresh read: whether the device posted again.
	Fresh *bool `json:"fresh,omitempty"`
}

// Delivery is a line the daemon writes to a listener for each message.
type Delivery struct {
	ID         string          `json:"id"`
	From       string          `json:"from"`
	Name       string          `json:"name"`
	Body       json.RawMessage `json:"body"`
	WantsReply bool            `json:"wantsReply"`
}

// Notice is a line the daemon writes to a listener about itself rather than
// a message. Its one key, "crosstalk", is never a Delivery's, and is kept
// for these.
type Notice struct {
	Crosstalk Event `json:"crosstalk"`
}

// Event is what a Notice says. Hub is HubConnected each time the daemon
// connects to the hub: messages sent while it was not connected never
// arrive, so a listener may want to catch up on what it missed.
type Event struct {
	Hub string `json:"hub,omitempty"`
}

// Reply is a line a listener writes back to answer a Delivery.
type Reply struct {
	ID      string          `json:"id"`
	Body    json.RawMessage `json:"body,omitempty"`
	Error   string          `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
}

// Marshal is json.Marshal without the escaping of <, > and & meant for
// HTML, which would let a body under MaxBody grow past MaxFrame.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// WriteLine writes v as one line of JSON, as both ends of the local socket
// speak it.
func WriteLine(w io.Writer, v any) error {
	return encoder(w).Encode(v)
}

func encoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// MaxLine bounds one line on the local socket. It is far above MaxFrame
// because the answer to a read of every device's value is one line holding
// all of them, each of which may be MaxBody.
const MaxLine = 32 * 1024 * 1024

// NewScanner reads lines of JSON of up to MaxLine bytes.
func NewScanner(r io.Reader) *bufio.Scanner {
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 64*1024), MaxLine)
	return lines
}
