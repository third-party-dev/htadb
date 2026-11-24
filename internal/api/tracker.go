package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"htadb/internal/adb"
)

// Tracker keeps one host:track-devices-l stream open and fans device events
// out to subscribers. It reconnects with backoff when the adb server goes
// away.
type Tracker struct {
	adb *adb.Client
	log *slog.Logger

	mu      sync.Mutex
	devices []adb.Device
	ok      bool // last stream healthy
	subs    map[chan []adb.DeviceEvent]bool
	hooks   []func(adb.DeviceEvent)
}

// NewTracker creates a tracker; call Run to start it.
func NewTracker(c *adb.Client, log *slog.Logger) *Tracker {
	return &Tracker{adb: c, log: log, subs: map[chan []adb.DeviceEvent]bool{}}
}

// OnEvent registers a callback for every event (e.g. mirroring's fallback
// memory, cleared on reconnect).
func (t *Tracker) OnEvent(f func(adb.DeviceEvent)) {
	t.mu.Lock()
	t.hooks = append(t.hooks, f)
	t.mu.Unlock()
}

// Run tracks until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		err := t.adb.TrackDevices(ctx, func(ds []adb.Device) error {
			backoff = 500 * time.Millisecond
			t.update(ds, true)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		t.log.Debug("device tracking interrupted", "err", err)
		t.update(nil, false)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (t *Tracker) update(ds []adb.Device, ok bool) {
	t.mu.Lock()
	ev := adb.DiffDevices(t.devices, ds)
	t.devices = ds
	t.ok = ok
	hooks := append([]func(adb.DeviceEvent){}, t.hooks...)
	var subs []chan []adb.DeviceEvent
	for ch := range t.subs {
		subs = append(subs, ch)
	}
	t.mu.Unlock()
	if len(ev) == 0 {
		return
	}
	for _, e := range ev {
		for _, h := range hooks {
			h(e)
		}
	}
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // slow subscriber: it will resync from the snapshot
		}
	}
}

// Snapshot returns the current list and whether tracking is healthy.
func (t *Tracker) Snapshot() ([]adb.Device, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]adb.Device(nil), t.devices...), t.ok
}

// Subscribe returns a channel of event batches; call the cancel func when
// done.
func (t *Tracker) Subscribe() (<-chan []adb.DeviceEvent, func()) {
	ch := make(chan []adb.DeviceEvent, 64)
	t.mu.Lock()
	t.subs[ch] = true
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		delete(t.subs, ch)
		t.mu.Unlock()
	}
}
