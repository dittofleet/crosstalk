package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/dittofleet/crosstalk/internal/wire"
)

// record is one value as the daemon keeps it.
type record struct {
	Value   json.RawMessage `json:"value,omitempty"`
	Version int64           `json:"version"`
	At      int64           `json:"at"`
	Expires int64           `json:"expires,omitempty"`
	// Deleted marks a synced value that was unset. It is kept, so that a
	// device which was away cannot bring the value back.
	Deleted bool   `json:"deleted,omitempty"`
	By      string `json:"by,omitempty"`
}

func (r *record) expired(now int64) bool {
	return r.Expires != 0 && now >= r.Expires
}

func (r *record) stamp() wire.Stamp {
	return wire.Stamp{Version: r.Version, By: r.By}
}

// newer reports whether a write stamped a replaces one stamped b.
func newer(a, b wire.Stamp) bool {
	return a.Version > b.Version || (a.Version == b.Version && a.By > b.By)
}

// store is every value this daemon holds: its own, and the last it saw of
// every other device's. Only the latest of each, with no history. The whole
// of it is one file, rewritten on each change.
type store struct {
	path string

	// Clock is the highest version this device has written or seen. Each
	// write takes a higher one, so versions keep growing even when the
	// system clock is set back.
	Clock int64 `json:"clock"`
	// Posted values, by the device that posted them, then by name.
	Posted map[string]map[string]*record `json:"posted"`
	Synced map[string]*record            `json:"synced"`
}

// newStore makes an empty store. With no path it is never written to disk.
func newStore(path string) *store {
	return &store{path: path, Posted: map[string]map[string]*record{}, Synced: map[string]*record{}}
}

func loadStore(path string) (*store, error) {
	s := newStore(path)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		// Everything in it can be had again from the other devices, or
		// posted again. Starting empty beats not starting.
		aside := path + ".unreadable"
		os.Rename(path, aside)
		fmt.Fprintf(os.Stderr, "crosstalk: %s could not be read and was moved to %s: %v\n", path, aside, err)
		return newStore(path), nil
	}
	if s.Posted == nil {
		s.Posted = map[string]map[string]*record{}
	}
	if s.Synced == nil {
		s.Synced = map[string]*record{}
	}
	return s, nil
}

func (s *store) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// tick returns the version for a new write: later than anything this device
// has written or seen, and later than the write it replaces.
func (s *store) tick(replaces int64) int64 {
	s.Clock = max(s.Clock+1, replaces+1, time.Now().UnixMilli())
	return s.Clock
}

func (s *store) posted(device string) map[string]*record {
	if s.Posted[device] == nil {
		s.Posted[device] = map[string]*record{}
	}
	return s.Posted[device]
}

func (s *store) dropPosted(device, name string) {
	delete(s.Posted[device], name)
	if len(s.Posted[device]) == 0 {
		delete(s.Posted, device)
	}
}

func entry(kind, name, device string, r *record) wire.Entry {
	e := wire.Entry{Kind: kind, Name: name, Device: device, Value: r.Value, Deleted: r.Deleted,
		At: time.UnixMilli(r.At).UTC().Format(time.RFC3339)}
	if len(e.Value) == 0 {
		e.Value = json.RawMessage("null")
	}
	return e
}

func put(kind, name string, r *record) wire.Put {
	return wire.Put{Kind: kind, Name: name, Value: r.Value, Version: r.Version, At: r.At,
		Expires: r.Expires, Deleted: r.Deleted, By: r.By}
}

func fromPut(p wire.Put) *record {
	return &record{Value: p.Value, Version: p.Version, At: p.At, Expires: p.Expires, Deleted: p.Deleted, By: p.By}
}
