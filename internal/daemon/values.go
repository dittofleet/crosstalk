package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/dittofleet/crosstalk/internal/wire"
)

// How many changes a watcher may fall behind before it is dropped.
const watchBacklog = 256

// takenBack is the version a posted value was taken back at, and when this
// daemon heard.
type takenBack struct {
	version int64
	at      time.Time
}

// A watcher is a program on this machine following the values under a name.
type watcher struct {
	name string
	// Closed when the watcher falls too far behind to catch up.
	changes chan wire.Entry
}

// notify tells the watchers of a name about a change. Called with d.mu held.
func (d *Daemon) notify(e wire.Entry) {
	for w := range d.watchers {
		if w.name != e.Name {
			continue
		}
		select {
		case w.changes <- e:
		default:
			delete(d.watchers, w)
			close(w.changes)
		}
	}
}

// persist writes the store. Called with d.mu held. A failure is logged and
// the value still counts for as long as the daemon runs.
func (d *Daemon) persist() {
	if err := d.store.save(); err != nil {
		d.log.Printf("could not save values: %v", err)
	}
}

// drop takes away a device's posted value and tells its watchers, with at
// as when it went. Called with d.mu held. The caller persists.
func (d *Daemon) drop(device, name string, at int64) {
	r := d.store.Posted[device][name]
	if r == nil {
		return
	}
	d.store.dropPosted(device, name)
	d.notify(entry(wire.Posted, name, device, &record{Version: r.Version, At: at, Deleted: true}))
}

// postedEntries lists the posted values under a name that have not expired,
// by device, from one device or with to "*" from all. Called with d.mu held.
func (d *Daemon) postedEntries(name, to string) []wire.Entry {
	now := time.Now().UnixMilli()
	var entries []wire.Entry
	for device, values := range d.store.Posted {
		r := values[name]
		if r == nil || r.expired(now) || (to != "*" && to != device) {
			continue
		}
		entries = append(entries, entry(wire.Posted, name, device, r))
	}
	slices.SortFunc(entries, func(a, b wire.Entry) int { return strings.Compare(a.Device, b.Device) })
	return entries
}

// others lists the other devices connected now. Called with d.mu held.
func (d *Daemon) others() []string {
	var names []string
	for _, device := range d.devices {
		if device.Online && !device.Self {
			names = append(names, device.Name)
		}
	}
	return names
}

// push sends a value to other devices. One that is away misses it, and gets
// it from the exchange of summaries when it next connects.
func (d *Daemon) push(to []string, p wire.Put) {
	for _, device := range to {
		_ = d.post(device, wire.Sealed{K: "put", Put: &p})
	}
}

// checkBody refuses a request with no name, or with a body, called what in
// the refusal, that is missing, too large or not JSON.
func checkBody(req wire.Request, needBody bool, what string) *wire.Response {
	var res wire.Response
	switch {
	case req.Name == "":
		res = refusal(wire.ErrBadRequest, "a name is required")
	case needBody && len(req.Body) == 0:
		res = refusal(wire.ErrBadRequest, "a "+what+" is required")
	case len(req.Body) > wire.MaxBody:
		res = refusal(wire.ErrTooLarge, fmt.Sprintf("the %s is over the %d KB limit", what, wire.MaxBody/1024))
	case len(req.Body) > 0 && !json.Valid(req.Body):
		res = refusal(wire.ErrBadRequest, "the "+what+" is not JSON")
	default:
		return nil
	}
	return &res
}

func changed(yes bool) wire.Response {
	return wire.Response{OK: true, Changed: &yes}
}

// opPost stores this device's value for a name and sends it to the others.
func (d *Daemon) opPost(req wire.Request) wire.Response {
	if bad := checkBody(req, true, "value"); bad != nil {
		return *bad
	}
	now := time.Now().UnixMilli()
	d.mu.Lock()
	mine := d.store.posted(d.cfg.Name)
	prev := mine[req.Name]
	// Posting what is already there is the common case for a program that
	// reports on a timer, and is not worth a write or a message. One with
	// an expiry is posted again all the same, to move the expiry on.
	if prev != nil && req.Expires == 0 && prev.Expires == 0 && bytes.Equal(prev.Value, req.Body) {
		d.mu.Unlock()
		return changed(false)
	}
	r := &record{Value: req.Body, At: now}
	if prev != nil {
		r.Version = prev.Version
	}
	r.Version = d.store.tick(r.Version)
	if req.Expires > 0 {
		r.Expires = now + int64(req.Expires*1000)
	}
	mine[req.Name] = r
	d.persist()
	d.notify(entry(wire.Posted, req.Name, d.cfg.Name, r))
	to := d.others()
	d.mu.Unlock()
	d.push(to, put(wire.Posted, req.Name, r))
	return changed(true)
}

// opUnpost takes back this device's value for a name.
func (d *Daemon) opUnpost(req wire.Request) wire.Response {
	if bad := checkBody(req, false, "value"); bad != nil {
		return *bad
	}
	d.mu.Lock()
	prev := d.store.Posted[d.cfg.Name][req.Name]
	if prev == nil {
		d.mu.Unlock()
		return changed(false)
	}
	gone := &record{Version: d.store.tick(prev.Version), At: time.Now().UnixMilli(), Deleted: true}
	d.store.dropPosted(d.cfg.Name, req.Name)
	d.persist()
	d.notify(entry(wire.Posted, req.Name, d.cfg.Name, gone))
	to := d.others()
	d.mu.Unlock()
	d.push(to, put(wire.Posted, req.Name, gone))
	return changed(true)
}

// opRead answers with the value one device posted, or with every device's.
// It answers from what this daemon holds, so it works while the other
// device is away. A fresh read first asks the device to post again.
func (d *Daemon) opRead(ctx context.Context, req wire.Request) wire.Response {
	if bad := checkBody(req, false, "value"); bad != nil {
		return *bad
	}
	if req.To == "" {
		return refusal(wire.ErrBadRequest, "a device to read from is required")
	}
	var fresh *bool
	if req.Fresh {
		if req.To == "*" {
			return refusal(wire.ErrBadRequest, "a fresh read is from one device")
		}
		// Whoever listens on the value's name is who posts it. Their post
		// travels ahead of their reply, so it is here once the reply is.
		// The answer is what says the value is current: a post of the
		// value already there sends nothing, and a post that happens to
		// arrive while this waits may have nothing to do with it. Any
		// failure leaves the stored value, marked as not fresh.
		res := d.send(ctx, wire.Request{To: req.To, Name: req.Name, Body: json.RawMessage(wire.RefreshBody), Timeout: req.Timeout})
		fresh = &res.OK
	}

	d.mu.Lock()
	entries := d.postedEntries(req.Name, req.To)
	d.mu.Unlock()
	if req.To != "*" && len(entries) == 0 {
		return refusal(wire.ErrNoValue, fmt.Sprintf("%s has posted nothing on %q", req.To, req.Name))
	}
	for i := range entries {
		entries[i].Fresh = fresh
	}
	return wire.Response{OK: true, Entries: entries}
}

// opSet writes a synced value, or with unset marks it as gone.
func (d *Daemon) opSet(req wire.Request, unset bool) wire.Response {
	if bad := checkBody(req, !unset, "value"); bad != nil {
		return *bad
	}
	d.mu.Lock()
	prev := d.store.Synced[req.Name]
	same := prev != nil && prev.Deleted == unset && (unset || bytes.Equal(prev.Value, req.Body))
	if same || (prev == nil && unset) {
		d.mu.Unlock()
		return changed(false)
	}
	r := &record{At: time.Now().UnixMilli(), By: d.cfg.Name, Deleted: unset}
	if !unset {
		r.Value = req.Body
	}
	if prev != nil {
		r.Version = prev.Version
	}
	r.Version = d.store.tick(r.Version)
	d.store.Synced[req.Name] = r
	d.persist()
	d.notify(entry(wire.Synced, req.Name, r.By, r))
	to := d.others()
	d.mu.Unlock()
	d.push(to, put(wire.Synced, req.Name, r))
	return changed(true)
}

// opGet answers with a synced value, from this daemon's own copy.
func (d *Daemon) opGet(req wire.Request) wire.Response {
	if bad := checkBody(req, false, "value"); bad != nil {
		return *bad
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.store.Synced[req.Name]
	if r == nil || r.Deleted {
		return refusal(wire.ErrNoValue, fmt.Sprintf("%q is not set", req.Name))
	}
	return wire.Response{OK: true, Entries: []wire.Entry{entry(wire.Synced, req.Name, r.By, r)}}
}

// opWatch writes the values under a name as they stand, then each change
// as it happens, for as long as the program stays connected.
func (d *Daemon) opWatch(c net.Conn, lines *bufio.Scanner, req wire.Request) {
	if bad := checkBody(req, false, "value"); bad != nil {
		wire.WriteLine(c, *bad)
		return
	}
	w := &watcher{name: req.Name, changes: make(chan wire.Entry, watchBacklog)}
	d.mu.Lock()
	current := d.postedEntries(req.Name, "*")
	if r := d.store.Synced[req.Name]; r != nil && !r.Deleted {
		current = append(current, entry(wire.Synced, req.Name, r.By, r))
	}
	// Registered in the same breath as the snapshot, so no change falls
	// between the two.
	d.watchers[w] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.watchers, w)
		d.mu.Unlock()
	}()

	// The program says nothing more. Its end of the connection closing is
	// how the daemon learns it has gone.
	gone := make(chan struct{})
	go func() {
		for lines.Scan() {
		}
		close(gone)
	}()

	if wire.WriteLine(c, wire.Response{OK: true}) != nil {
		return
	}
	for _, e := range current {
		if wire.WriteLine(c, e) != nil {
			return
		}
	}
	for {
		select {
		case <-gone:
			return
		case e, ok := <-w.changes:
			if !ok {
				return
			}
			c.SetWriteDeadline(time.Now().Add(deliverTimeout))
			if wire.WriteLine(c, e) != nil {
				return
			}
		}
	}
}

// applyPut takes in a value another device sent.
func (d *Daemon) applyPut(from string, p wire.Put) {
	if p.Name == "" || len(p.Value) > wire.MaxBody {
		return
	}
	r := fromPut(p)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.store.Clock = max(d.store.Clock, p.Version)
	switch p.Kind {
	case wire.Posted:
		// A posted value is whoever sent it's own. The hub vouches for
		// nothing here: the box only opens as coming from that device.
		prev := d.store.Posted[from][p.Name]
		if prev != nil && p.Version <= prev.Version {
			return
		}
		// A value that was taken back leaves nothing to compare a version
		// with, so the version it went at is kept for a while. Without it,
		// an older post that arrives after the taking back, as two sent
		// close together can, would bring the value back.
		mark := from + "\n" + p.Name
		if went, ok := d.taken[mark]; ok && p.Version <= went.version {
			return
		}
		if p.Deleted {
			d.taken[mark] = takenBack{version: p.Version, at: time.Now()}
		}
		if p.Deleted || r.expired(time.Now().UnixMilli()) {
			if prev == nil {
				return
			}
			d.store.dropPosted(from, p.Name)
			r.Deleted = true
		} else {
			r.By = ""
			d.store.posted(from)[p.Name] = r
		}
		d.persist()
		d.notify(entry(wire.Posted, p.Name, from, r))
	case wire.Synced:
		if prev := d.store.Synced[p.Name]; prev != nil && !newer(r.stamp(), prev.stamp()) {
			return
		}
		r.Expires = 0
		d.store.Synced[p.Name] = r
		d.persist()
		d.notify(entry(wire.Synced, p.Name, r.By, r))
	}
}

// sendHave tells a device which values this one holds, so it can ask for
// what it lacks, and with reply asks it to do the same.
func (d *Daemon) sendHave(to string, reply bool) {
	now := time.Now().UnixMilli()
	have := wire.Summary{Posted: map[string]int64{}, Synced: map[string]wire.Stamp{}}
	d.mu.Lock()
	have.Clock = d.store.Clock
	for name, r := range d.store.Posted[d.cfg.Name] {
		if !r.expired(now) {
			have.Posted[name] = r.Version
		}
	}
	for name, r := range d.store.Synced {
		have.Synced[name] = r.stamp()
	}
	d.mu.Unlock()
	_ = d.post(to, wire.Sealed{K: "have", Have: &have, Reply: reply})
}

// applyHave compares another device's summary with what is held here, drops
// what that device no longer posts, and asks for what is newer there.
func (d *Daemon) applyHave(from string, have wire.Summary) {
	want := wire.Summary{Posted: map[string]int64{}, Synced: map[string]wire.Stamp{}}
	d.mu.Lock()
	held := d.store.Posted[from]
	dropped := false
	for name, r := range held {
		// A value posted after the summary was made can arrive ahead of it,
		// and is newer than its clock. One from a daemon that sends no
		// clock is judged by the summary alone.
		if _, still := have.Posted[name]; !still && (have.Clock == 0 || r.Version <= have.Clock) {
			d.drop(from, name, time.Now().UnixMilli())
			dropped = true
		}
	}
	if dropped {
		d.persist()
	}
	for name, version := range have.Posted {
		if r := held[name]; r == nil || version > r.Version {
			want.Posted[name] = version
		}
	}
	for name, stamp := range have.Synced {
		if r := d.store.Synced[name]; r == nil || newer(stamp, r.stamp()) {
			want.Synced[name] = stamp
		}
	}
	d.mu.Unlock()
	if len(want.Posted)+len(want.Synced) > 0 {
		_ = d.post(from, wire.Sealed{K: "want", Want: &want})
	}
}

// applyWant sends another device the values it asked for.
func (d *Daemon) applyWant(from string, want wire.Summary) {
	now := time.Now().UnixMilli()
	var puts []wire.Put
	d.mu.Lock()
	for name := range want.Posted {
		if r := d.store.Posted[d.cfg.Name][name]; r != nil && !r.expired(now) {
			puts = append(puts, put(wire.Posted, name, r))
		}
	}
	for name := range want.Synced {
		if r := d.store.Synced[name]; r != nil {
			puts = append(puts, put(wire.Synced, name, r))
		}
	}
	d.mu.Unlock()
	for _, p := range puts {
		d.push([]string{from}, p)
	}
}

// expire drops posted values whose time is up, and tells their watchers.
func (d *Daemon) expire() {
	now := time.Now().UnixMilli()
	d.mu.Lock()
	defer d.mu.Unlock()
	// Kept for as long as an older post could still be accepted.
	for mark, went := range d.taken {
		if time.Since(went.at) > 2*freshness {
			delete(d.taken, mark)
		}
	}
	dropped := false
	for device, values := range d.store.Posted {
		for name, r := range values {
			if r.expired(now) {
				d.drop(device, name, r.Expires)
				dropped = true
			}
		}
	}
	if dropped {
		d.persist()
	}
}

// forgetRemoved drops the posted values of devices that are no longer on
// the hub's list. Called with d.mu held, after d.devices is replaced.
func (d *Daemon) forgetRemoved() {
	listed := map[string]bool{d.cfg.Name: true}
	for _, device := range d.devices {
		listed[device.Name] = true
	}
	dropped := false
	for device, values := range d.store.Posted {
		if listed[device] {
			continue
		}
		for name := range values {
			d.drop(device, name, time.Now().UnixMilli())
		}
		delete(d.store.Posted, device)
		dropped = true
	}
	if dropped {
		d.persist()
	}
}
