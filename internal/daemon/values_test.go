package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dittofleet/crosstalk/internal/wire"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// read returns what a machine holds of a device's posted value, or "" when
// it holds none.
func (m *machine) read(device, name string) string {
	m.t.Helper()
	res := m.ask(wire.Request{Op: "read", To: device, Name: name})
	if !res.OK {
		if res.Error != wire.ErrNoValue {
			m.t.Fatalf("read = %+v", res)
		}
		return ""
	}
	return string(res.Entries[0].Value)
}

func (m *machine) get(name string) string {
	m.t.Helper()
	res := m.ask(wire.Request{Op: "get", Name: name})
	if !res.OK {
		return ""
	}
	return string(res.Entries[0].Value)
}

func (m *machine) do(req wire.Request) wire.Response {
	m.t.Helper()
	res := m.ask(req)
	if !res.OK {
		m.t.Fatalf("%s %s = %+v", req.Op, req.Name, res)
	}
	return res
}

func TestAPostedValueReachesTheOtherMachines(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]

	if res := lychee.do(wire.Request{Op: "post", Name: "lookout:disk", Body: raw(`{"free":12}`)}); !*res.Changed {
		t.Fatal("a first post changed nothing")
	}
	eventually(t, "the value arriving", func() bool { return macbook.read("lychee", "lookout:disk") == `{"free":12}` })
	if lychee.read("lychee", "lookout:disk") != `{"free":12}` {
		t.Error("the machine that posted cannot read its own value")
	}

	// The same value again is not a change.
	if res := lychee.do(wire.Request{Op: "post", Name: "lookout:disk", Body: raw(`{"free":12}`)}); *res.Changed {
		t.Error("posting the same value counted as a change")
	}

	// Each machine has its own value under the name.
	macbook.do(wire.Request{Op: "post", Name: "lookout:disk", Body: raw(`{"free":3}`)})
	eventually(t, "both values on one machine", func() bool {
		res := lychee.do(wire.Request{Op: "read", To: "*", Name: "lookout:disk"})
		return len(res.Entries) == 2 && res.Entries[0].Device == "lychee" && string(res.Entries[1].Value) == `{"free":3}`
	})

	lychee.do(wire.Request{Op: "unpost", Name: "lookout:disk"})
	eventually(t, "the value going", func() bool { return macbook.read("lychee", "lookout:disk") == "" })
}

func TestAMachineThatWasAwayCatchesUp(t *testing.T) {
	f := newFleet(t)
	lychee := f.add("lychee")
	lychee.do(wire.Request{Op: "post", Name: "status", Body: raw(`"first"`)})
	lychee.do(wire.Request{Op: "set", Name: "theme", Body: raw(`"dark"`)})
	lychee.do(wire.Request{Op: "set", Name: "old", Body: raw(`1`)})

	// It learns what was there before it joined.
	macbook := f.add("macbook")
	eventually(t, "the posted value", func() bool { return macbook.read("lychee", "status") == `"first"` })
	eventually(t, "the synced values", func() bool { return macbook.get("theme") == `"dark"` && macbook.get("old") == `1` })

	// And what changed while it was away: a new value, a value taken back,
	// a setting changed and a setting removed, on both sides.
	macbook.stop()
	eventually(t, "macbook going away", func() bool {
		for _, device := range lychee.ask(wire.Request{Op: "devices"}).Devices {
			if device.Name == "macbook" {
				return !device.Online
			}
		}
		return false
	})
	if macbook.read("lychee", "status") != `"first"` {
		t.Error("a machine that is away cannot read what it last saw")
	}
	lychee.do(wire.Request{Op: "unpost", Name: "status"})
	lychee.do(wire.Request{Op: "post", Name: "report", Body: raw(`{"n":2}`)})
	lychee.do(wire.Request{Op: "unset", Name: "old"})
	lychee.do(wire.Request{Op: "set", Name: "theme", Body: raw(`"light"`)})

	// The same machine, back: the same store, a new connection.
	back := f.add("macbook2")
	back.daemon.mu.Lock()
	back.daemon.store = macbook.daemon.store
	back.daemon.mu.Unlock()
	back.do(wire.Request{Op: "set", Name: "volume", Body: raw(`7`)})
	back.daemon.reconnect("to exchange summaries again")

	eventually(t, "the new posted value", func() bool { return back.read("lychee", "report") == `{"n":2}` })
	eventually(t, "the taken back value going", func() bool { return back.read("lychee", "status") == "" })
	eventually(t, "the changed setting", func() bool { return back.get("theme") == `"light"` })
	eventually(t, "the removed setting going", func() bool { return back.get("old") == "" })
	eventually(t, "its own change reaching the other", func() bool { return lychee.get("volume") == `7` })
}

// A machine that reconnects before the hub noticed it was gone never looks
// new to the others, so what it missed comes from asking for their summary.
func TestAQuickReconnectStillCatchesUp(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]
	// A change that was pushed to macbook's dead connection, and lost.
	lychee.daemon.applyPut("asleep", wire.Put{Kind: wire.Synced, Name: "theme", Value: raw(`"dark"`), Version: 1 << 50, By: "asleep"})

	macbook.daemon.sendHave("lychee", true)
	eventually(t, "the missed change", func() bool { return macbook.get("theme") == `"dark"` })
}

func TestTheLatestWriteToASyncedValueWins(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]

	lychee.do(wire.Request{Op: "set", Name: "theme", Body: raw(`"dark"`)})
	eventually(t, "the first write", func() bool { return macbook.get("theme") == `"dark"` })
	macbook.do(wire.Request{Op: "set", Name: "theme", Body: raw(`"light"`)})
	eventually(t, "the later write", func() bool { return lychee.get("theme") == `"light"` })

	// A write that lost cannot come back: applying it again changes nothing.
	lychee.daemon.applyPut("macbook", wire.Put{Kind: wire.Synced, Name: "theme", Value: raw(`"stale"`), Version: 1, By: "macbook"})
	if lychee.get("theme") != `"light"` {
		t.Error("an older write replaced a newer one")
	}

	// Two writes with the same version settle the same way on both sides.
	a := wire.Put{Kind: wire.Synced, Name: "tie", Value: raw(`"a"`), Version: 5, By: "lychee"}
	b := wire.Put{Kind: wire.Synced, Name: "tie", Value: raw(`"b"`), Version: 5, By: "macbook"}
	lychee.daemon.applyPut("lychee", a)
	lychee.daemon.applyPut("macbook", b)
	macbook.daemon.applyPut("macbook", b)
	macbook.daemon.applyPut("lychee", a)
	if lychee.get("tie") != macbook.get("tie") {
		t.Errorf("a tie settled as %s on one machine and %s on the other", lychee.get("tie"), macbook.get("tie"))
	}

	macbook.do(wire.Request{Op: "unset", Name: "theme"})
	eventually(t, "the value going everywhere", func() bool { return lychee.get("theme") == "" })
	if res := lychee.ask(wire.Request{Op: "get", Name: "theme"}); res.Error != wire.ErrNoValue {
		t.Errorf("get of a removed value = %+v", res)
	}
}

func TestOnlyTheOwnerChangesAPostedValue(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]
	lychee.do(wire.Request{Op: "post", Name: "status", Body: raw(`"mine"`)})
	eventually(t, "the value arriving", func() bool { return macbook.read("lychee", "status") == `"mine"` })

	// A put is stored under whoever sent it, so another machine posting
	// under the same name adds its own value and leaves lychee's alone.
	macbook.daemon.applyPut("asleep", wire.Put{Kind: wire.Posted, Name: "status", Value: raw(`"theirs"`), Version: 1 << 60})
	if macbook.read("lychee", "status") != `"mine"` {
		t.Error("one machine changed another's posted value")
	}
}

func TestWatchPrintsWhatIsThereAndThenEachChange(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]
	lychee.do(wire.Request{Op: "post", Name: "status", Body: raw(`1`)})
	eventually(t, "the value arriving", func() bool { return macbook.read("lychee", "status") == `1` })

	_, lines := macbook.dial(wire.Request{Op: "watch", Name: "status"})
	next := func() wire.Entry {
		t.Helper()
		if !lines.Scan() {
			t.Fatal("the watch ended")
		}
		var e wire.Entry
		json.Unmarshal(lines.Bytes(), &e)
		return e
	}
	if !lines.Scan() || !strings.Contains(lines.Text(), `"ok":true`) {
		t.Fatalf("watch: %s", lines.Text())
	}
	if e := next(); e.Kind != wire.Posted || e.Device != "lychee" || string(e.Value) != `1` {
		t.Fatalf("what was there = %+v", e)
	}

	lychee.do(wire.Request{Op: "post", Name: "status", Body: raw(`2`)})
	if e := next(); e.Device != "lychee" || string(e.Value) != `2` || e.Deleted {
		t.Fatalf("a change = %+v", e)
	}
	// Another name is none of this watch's business.
	lychee.do(wire.Request{Op: "post", Name: "other", Body: raw(`0`)})
	macbook.do(wire.Request{Op: "set", Name: "status", Body: raw(`"shared"`)})
	if e := next(); e.Kind != wire.Synced || e.Device != "macbook" || string(e.Value) != `"shared"` {
		t.Fatalf("a synced change = %+v", e)
	}
	lychee.do(wire.Request{Op: "unpost", Name: "status"})
	if e := next(); e.Kind != wire.Posted || !e.Deleted || string(e.Value) != `null` {
		t.Fatalf("a value going = %+v", e)
	}
}

func TestAFreshReadAsksTheMachineToPostAgain(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]
	lychee.do(wire.Request{Op: "post", Name: "report", Body: raw(`"old"`)})
	eventually(t, "the value arriving", func() bool { return macbook.read("lychee", "report") == `"old"` })

	// With nothing listening, the stored value comes back, marked.
	res := macbook.do(wire.Request{Op: "read", To: "lychee", Name: "report", Fresh: true})
	if string(res.Entries[0].Value) != `"old"` || *res.Entries[0].Fresh {
		t.Fatalf("a fresh read with nobody to ask = %+v", res.Entries[0])
	}

	lychee.listen("report", func(msg wire.Delivery) wire.Reply {
		if string(msg.Body) != wire.RefreshBody {
			t.Errorf("asked with %s", msg.Body)
		}
		lychee.do(wire.Request{Op: "post", Name: "report", Body: raw(`"new"`)})
		return wire.Reply{}
	})
	res = macbook.do(wire.Request{Op: "read", To: "lychee", Name: "report", Fresh: true})
	if string(res.Entries[0].Value) != `"new"` || !*res.Entries[0].Fresh {
		t.Fatalf("a fresh read = %+v", res.Entries[0])
	}
	// Posting the same value again changes nothing, and is still an answer.
	res = macbook.do(wire.Request{Op: "read", To: "lychee", Name: "report", Fresh: true})
	if string(res.Entries[0].Value) != `"new"` || !*res.Entries[0].Fresh {
		t.Fatalf("a fresh read of an unchanged value = %+v", res.Entries[0])
	}
}

// A summary says what its device posted as of its clock. A value posted
// after that can arrive first, and must not be dropped for missing from it.
func TestASummaryOnlyDropsWhatItKnewAbout(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]
	lychee.do(wire.Request{Op: "post", Name: "status", Body: raw(`1`)})
	eventually(t, "the value arriving", func() bool { return macbook.read("lychee", "status") == `1` })

	macbook.daemon.mu.Lock()
	version := macbook.daemon.store.Posted["lychee"]["status"].Version
	macbook.daemon.mu.Unlock()
	macbook.daemon.applyHave("lychee", wire.Summary{Clock: version - 1})
	if macbook.read("lychee", "status") != `1` {
		t.Fatal("a summary older than the value dropped it")
	}
	macbook.daemon.applyHave("lychee", wire.Summary{Clock: version})
	if macbook.read("lychee", "status") != "" {
		t.Fatal("a summary without the value kept it")
	}
}

func TestAnExpiredValueGoes(t *testing.T) {
	_, machines := start(t, func(p *pace) { p.watchEvery = 5 * 1e6 })
	lychee, macbook := machines["lychee"], machines["macbook"]
	lychee.do(wire.Request{Op: "post", Name: "report", Body: raw(`1`), Expires: 0.2})
	eventually(t, "the value arriving", func() bool { return macbook.read("lychee", "report") == `1` })
	eventually(t, "the value expiring where it was read", func() bool { return macbook.read("lychee", "report") == "" })
	eventually(t, "the value expiring where it was posted", func() bool { return lychee.read("lychee", "report") == "" })
}

func TestARemovedMachinesValuesGo(t *testing.T) {
	f := newFleet(t)
	lychee, macbook := f.add("lychee"), f.add("macbook")
	macbook.do(wire.Request{Op: "post", Name: "status", Body: raw(`1`)})
	eventually(t, "the value arriving", func() bool { return lychee.read("macbook", "status") == `1` })

	macbook.stop()
	f.hub.mu.Lock()
	delete(f.hub.known, "macbook")
	f.hub.mu.Unlock()
	f.hub.announce()
	eventually(t, "the removed machine's value going", func() bool { return lychee.read("macbook", "status") == "" })
}

func TestValuesSurviveARestart(t *testing.T) {
	f := newFleet(t)
	lychee := f.add("lychee")
	lychee.do(wire.Request{Op: "post", Name: "status", Body: raw(`{"a":1}`)})
	lychee.do(wire.Request{Op: "set", Name: "theme", Body: raw(`"dark"`)})
	lychee.do(wire.Request{Op: "set", Name: "gone", Body: raw(`1`)})
	lychee.do(wire.Request{Op: "unset", Name: "gone"})

	again, err := loadStore(lychee.daemon.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Posted["lychee"]["status"].Value) != `{"a":1}` || string(again.Synced["theme"].Value) != `"dark"` {
		t.Fatalf("reloaded %+v", again)
	}
	if !again.Synced["gone"].Deleted || again.Clock != lychee.daemon.store.Clock {
		t.Error("a removal or the clock was not kept")
	}
}

func TestATakenBackValueStaysGone(t *testing.T) {
	_, machines := start(t)
	macbook := machines["macbook"]

	// A post and the taking back of it, sent close together, can arrive
	// the wrong way round. The older post must not bring the value back.
	post := wire.Put{Kind: wire.Posted, Name: "status", Value: raw(`1`), Version: 10}
	gone := wire.Put{Kind: wire.Posted, Name: "status", Version: 11, Deleted: true}
	macbook.daemon.applyPut("lychee", gone)
	macbook.daemon.applyPut("lychee", post)
	if macbook.read("lychee", "status") != "" {
		t.Error("an older post brought back a value that was taken back")
	}
	// The same after it was held here, and for the post delivered again.
	macbook.daemon.applyPut("lychee", wire.Put{Kind: wire.Posted, Name: "other", Value: raw(`1`), Version: 20})
	macbook.daemon.applyPut("lychee", wire.Put{Kind: wire.Posted, Name: "other", Version: 21, Deleted: true})
	macbook.daemon.applyPut("lychee", wire.Put{Kind: wire.Posted, Name: "other", Value: raw(`1`), Version: 20})
	if macbook.read("lychee", "other") != "" {
		t.Error("a post delivered again brought back a value that was taken back")
	}
	// A post made after the taking back is a new value.
	macbook.daemon.applyPut("lychee", wire.Put{Kind: wire.Posted, Name: "status", Value: raw(`2`), Version: 12})
	if macbook.read("lychee", "status") != `2` {
		t.Error("a value could not be posted again after being taken back")
	}
}

func TestReadingEveryMachinesLargeValue(t *testing.T) {
	_, machines := start(t)
	lychee, macbook := machines["lychee"], machines["macbook"]
	// Each within the limit, together well past one frame.
	big := raw(`"` + strings.Repeat("x", wire.MaxBody-2) + `"`)
	for _, device := range []string{"a", "b", "c"} {
		macbook.daemon.applyPut(device, wire.Put{Kind: wire.Posted, Name: "big", Value: big, Version: 1})
	}
	lychee.do(wire.Request{Op: "post", Name: "big", Body: big})
	eventually(t, "the value arriving", func() bool { return macbook.read("lychee", "big") != "" })
	if res := macbook.do(wire.Request{Op: "read", To: "*", Name: "big"}); len(res.Entries) != 4 {
		t.Fatalf("read %d of 4 large values", len(res.Entries))
	}
}
