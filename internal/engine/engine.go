// Package engine discovers Spout senders, runs one capture goroutine per
// sender, and coordinates recording sessions across all armed channels.
package engine

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/McHauge/Spout-Multi-Recorder/internal/frame"
	"github.com/McHauge/Spout-Multi-Recorder/internal/ndi"
	"github.com/McHauge/Spout-Multi-Recorder/internal/recorder"
	"github.com/McHauge/Spout-Multi-Recorder/internal/spout"
)

// NDIPrefix namespaces manually added NDI channels in the channel map.
const NDIPrefix = "ndi:"

// Channel is one Spout sender being monitored (and possibly recorded).
type Channel struct {
	Name string // Spout sender name, or "ndi:<source name>" for NDI
	NDI  bool
	Buf  *frame.Buffer

	armed        atomic.Bool
	online       atomic.Bool
	replaceAudio atomic.Bool  // record master device audio instead of source audio
	pump         *audioPump   // native audio fanout (NDI channels only)
	lastSeen     atomic.Int64 // unix nano of last time the sender name was listed

	mu  sync.Mutex
	rec *recorder.Recorder

	stop chan struct{}
	done chan struct{}
}

// Armed reports whether this channel will be included in recordings.
func (c *Channel) Armed() bool { return c.armed.Load() }

// SetArmed marks the channel for recording (no effect on a running session
// unless auto-record is enabled).
func (c *Channel) SetArmed(v bool) { c.armed.Store(v) }

// Online reports whether the sender currently exists.
func (c *Channel) Online() bool { return c.online.Load() }

// DisplayName is the channel name without the NDI namespace prefix.
func (c *Channel) DisplayName() string { return strings.TrimPrefix(c.Name, NDIPrefix) }

// NativeAudioChannels reports the channel count of the source's own audio
// stream (NDI only; 0 when unknown or not an NDI channel).
func (c *Channel) NativeAudioChannels() int {
	if c.pump == nil {
		return 0
	}
	return c.pump.Channels()
}

// AudioLevels returns per-channel peaks (0..1) of the source's native audio,
// or nil for channels without their own audio.
func (c *Channel) AudioLevels() []float64 {
	if c.pump == nil {
		return nil
	}
	return c.pump.Levels()
}

// ReplaceAudio reports whether this channel records the master audio device.
// When false, NDI channels record their native embedded audio instead and
// Spout channels (which carry no audio) record without an audio track.
func (c *Channel) ReplaceAudio() bool { return c.replaceAudio.Load() }

// SetReplaceAudio changes the audio preference (affects the next recording).
func (c *Channel) SetReplaceAudio(v bool) { c.replaceAudio.Store(v) }

// Recorder returns the active recorder, or nil.
func (c *Channel) Recorder() *recorder.Recorder {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rec
}

// sleepStop sleeps for d or until the channel is stopped (returns false).
func (c *Channel) sleepStop(d time.Duration) bool {
	select {
	case <-c.stop:
		return false
	case <-time.After(d):
		return true
	}
}

// captureNDI receives frames from a manually added NDI source. Both full
// NDI and NDI|HX sources work — the runtime decodes HX transparently.
func (c *Channel) captureNDI() {
	defer close(c.done)
	if err := ndi.Available(); err != nil {
		log.Printf("channel %s: %v", c.Name, err)
		<-c.stop
		return
	}
	name := c.DisplayName()
	go c.pump.run(c.stop)
	var rx *ndi.Receiver
	defer func() {
		if rx != nil {
			rx.Close()
		}
	}()
	var lastFrame time.Time
	var lastAny time.Time
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		if rx == nil {
			// Create the receiver with the source *name* only: the NDI
			// runtime then runs its own discovery inside the receiver and
			// (re)connects whenever the source appears — including after the
			// sending application restarts on a new URL/port.
			r, err := ndi.NewReceiver(ndi.Source{Name: name})
			if err != nil {
				log.Printf("channel %s: %v", c.Name, err)
				if !c.sleepStop(5 * time.Second) {
					return
				}
				continue
			}
			rx = r
			lastAny = time.Now()
			log.Printf("NDI receiver created for %q, waiting for source", name)
		}
		pix, w, h, aud, audCh := rx.CaptureAV(250 * time.Millisecond)
		switch {
		case pix != nil:
			c.online.Store(true)
			c.Buf.Store(pix, w, h, spout.FormatBGRA8, true)
			lastFrame = time.Now()
			lastAny = lastFrame
		case aud != nil:
			c.pump.push(aud, audCh)
			lastAny = time.Now()
		default:
			if rx.Connections() == 0 || time.Since(lastFrame) > 3*time.Second {
				c.online.Store(false)
				c.Buf.SetConnected(false)
			}
			// Safety net: the receiver's internal discovery normally handles
			// reconnects itself; if it somehow gets stuck, rebuild it after a
			// longer silence.
			if time.Since(lastAny) > 30*time.Second {
				log.Printf("NDI %q silent for 30s, rebuilding receiver", name)
				rx.Close()
				rx = nil
			}
		}
	}
}

// captureLoop polls the Spout receiver and publishes frames to the mailbox.
func (c *Channel) captureLoop() {
	defer close(c.done)
	rx, err := spout.NewReceiver(c.Name)
	if err != nil {
		log.Printf("channel %s: %v", c.Name, err)
		return
	}
	defer rx.Close()

	tick := time.NewTicker(time.Second / 60)
	defer tick.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
		}
		f := rx.Receive(false)
		c.online.Store(f.Connected)
		if f.Connected && f.NewFrame && f.Width > 0 {
			c.Buf.Store(f.Pixels, f.Width, f.Height, f.Format, true)
		} else if !f.Connected {
			c.Buf.SetConnected(false)
		}
	}
}

// Engine owns all channels.
type Engine struct {
	mu          sync.Mutex
	channels    map[string]*Channel
	order       []string
	recording   bool
	maxChannels int
	autoRecord  bool
	recSet      recorder.Settings
	stop        chan struct{}

	// OnChange is called (from the discovery goroutine) whenever the channel
	// list changes. The UI wraps this with fyne.Do.
	OnChange func()
}

// New creates the engine and starts sender discovery.
func New(maxChannels int) *Engine {
	e := &Engine{
		channels:    map[string]*Channel{},
		maxChannels: maxChannels,
		stop:        make(chan struct{}),
	}
	go e.discoveryLoop()
	return e
}

// SetMaxChannels limits how many channels are auto-armed.
func (e *Engine) SetMaxChannels(n int) {
	e.mu.Lock()
	e.maxChannels = n
	e.mu.Unlock()
}

// SetAutoRecord controls whether senders that appear (or deliver their first
// frame) during a recording session automatically get their own recorder.
func (e *Engine) SetAutoRecord(v bool) {
	e.mu.Lock()
	e.autoRecord = v
	e.mu.Unlock()
}

// Close stops discovery, capture loops and any recording.
func (e *Engine) Close() {
	close(e.stop)
	e.StopRecording()
	e.mu.Lock()
	chans := make([]*Channel, 0, len(e.channels))
	for _, c := range e.channels {
		chans = append(chans, c)
	}
	e.mu.Unlock()
	for _, c := range chans {
		close(c.stop)
		<-c.done
	}
}

func (e *Engine) discoveryLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
		}
		names := spout.ListSenders()
		now := time.Now().UnixNano()
		changed := false

		e.mu.Lock()
		seen := map[string]bool{}
		for _, n := range names {
			seen[n] = true
			if c, ok := e.channels[n]; ok {
				c.lastSeen.Store(now)
				continue
			}
			// New sender.
			c := &Channel{
				Name: n,
				Buf:  &frame.Buffer{},
				stop: make(chan struct{}),
				done: make(chan struct{}),
			}
			c.lastSeen.Store(now)
			c.replaceAudio.Store(true) // Spout has no audio: use master device
			armedCount := 0
			for _, name := range e.order {
				if e.channels[name].Armed() {
					armedCount++
				}
			}
			c.SetArmed(armedCount < e.maxChannels)
			e.channels[n] = c
			e.order = append(e.order, n)
			sort.Strings(e.order)
			go c.captureLoop()
			changed = true
			log.Printf("discovered sender %q", n)
		}

		// Remove channels whose sender has been gone for a while, but never
		// while recording (they keep producing black frames instead).
		// NDI channels are manual and only removed by the user.
		if !e.recording {
			for name, c := range e.channels {
				if seen[name] || c.NDI {
					continue
				}
				if now-c.lastSeen.Load() > int64(5*time.Second) {
					close(c.stop)
					delete(e.channels, name)
					for i, o := range e.order {
						if o == name {
							e.order = append(e.order[:i], e.order[i+1:]...)
							break
						}
					}
					changed = true
					log.Printf("sender %q gone, channel removed", name)
				}
			}
		}

		// Auto-record: while a session is running, start a recorder on any
		// armed channel that doesn't have one yet and has frames available
		// (covers both brand-new senders and ones that connected late).
		if e.recording && e.autoRecord {
			for _, n := range e.order {
				c := e.channels[n]
				if !c.Armed() {
					continue
				}
				c.mu.Lock()
				has := c.rec != nil
				c.mu.Unlock()
				if has {
					continue
				}
				w, h, _, _ := c.Buf.Dims()
				if w == 0 || h == 0 {
					continue
				}
				rec, err := recorder.Start(c.Name, c.Buf, e.recSet, e.audioSourceFor(c, e.recSet))
				if err != nil {
					log.Printf("auto-record %s: %v", n, err)
					continue
				}
				c.mu.Lock()
				c.rec = rec
				c.mu.Unlock()
				changed = true
				log.Printf("auto-record started for %q", n)
			}
		}
		e.mu.Unlock()

		if changed && e.OnChange != nil {
			e.OnChange()
		}
	}
}

// AddNDI adds a manually selected NDI source as a channel (idempotent).
// replaceAudio=false records the source's native NDI audio.
func (e *Engine) AddNDI(sourceName string, replaceAudio bool) {
	key := NDIPrefix + sourceName
	e.mu.Lock()
	if _, ok := e.channels[key]; ok {
		e.mu.Unlock()
		return
	}
	c := &Channel{
		Name: key,
		NDI:  true,
		Buf:  &frame.Buffer{},
		pump: newAudioPump(),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	c.lastSeen.Store(time.Now().UnixNano())
	c.SetArmed(true)
	c.replaceAudio.Store(replaceAudio)
	e.channels[key] = c
	e.order = append(e.order, key)
	sort.Strings(e.order)
	go c.captureNDI()
	e.mu.Unlock()
	log.Printf("added NDI source %q", sourceName)
	if e.OnChange != nil {
		e.OnChange()
	}
}

// RemoveChannel removes a (manually added) channel. It refuses while the
// channel is being recorded.
func (e *Engine) RemoveChannel(name string) error {
	e.mu.Lock()
	c, ok := e.channels[name]
	if !ok {
		e.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	recording := c.rec != nil
	c.mu.Unlock()
	if recording {
		e.mu.Unlock()
		return fmt.Errorf("%s is recording — stop the recording first", c.DisplayName())
	}
	close(c.stop)
	delete(e.channels, name)
	for i, o := range e.order {
		if o == name {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	e.mu.Unlock()
	log.Printf("removed channel %q", name)
	if e.OnChange != nil {
		e.OnChange()
	}
	return nil
}

// Channels returns the channels in stable (alphabetical) order.
func (e *Engine) Channels() []*Channel {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Channel, 0, len(e.order))
	for _, n := range e.order {
		out = append(out, e.channels[n])
	}
	return out
}

// Recording reports whether a session is active.
func (e *Engine) Recording() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recording
}

// StartRecording starts recorders on all armed channels that have received at
// least one frame. Returns the number started.
func (e *Engine) StartRecording(set recorder.Settings) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.recording {
		return 0, fmt.Errorf("already recording")
	}
	e.recSet = set
	started := 0
	var firstErr error
	for _, n := range e.order {
		c := e.channels[n]
		if !c.Armed() {
			continue
		}
		w, h, _, _ := c.Buf.Dims()
		if w == 0 || h == 0 {
			log.Printf("skipping %s: no frame received yet", n)
			continue
		}
		rec, err := recorder.Start(c.Name, c.Buf, set, e.audioSourceFor(c, set))
		if err != nil {
			log.Printf("start %s: %v", n, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c.mu.Lock()
		c.rec = rec
		c.mu.Unlock()
		started++
	}
	if started > 0 {
		e.recording = true
		return started, nil
	}
	if e.autoRecord && firstErr == nil {
		// Empty session: with auto-record enabled it is valid to start with
		// no senders and let channels join as they appear.
		e.recording = true
		return 0, nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no armed channels with frames to record")
	}
	return 0, firstErr
}

// audioSourceFor picks the audio for one channel's recording: the master
// device when the channel replaces audio (and a device is running), the
// channel's native NDI audio otherwise, or none.
func (e *Engine) audioSourceFor(c *Channel, set recorder.Settings) recorder.AudioSource {
	if c.ReplaceAudio() {
		if set.Audio != nil && set.Audio.Running() {
			return set.Audio
		}
		return nil
	}
	if c.NDI && c.pump != nil {
		return c.pump
	}
	return nil
}

// StopRecording stops all recorders (in parallel) and waits for the files to
// be finalised.
func (e *Engine) StopRecording() {
	e.mu.Lock()
	if !e.recording {
		e.mu.Unlock()
		return
	}
	var wg sync.WaitGroup
	for _, c := range e.channels {
		c.mu.Lock()
		rec := c.rec
		c.rec = nil
		c.mu.Unlock()
		if rec != nil {
			wg.Add(1)
			go func(r *recorder.Recorder) {
				defer wg.Done()
				r.Stop()
			}(rec)
		}
	}
	e.recording = false
	e.mu.Unlock()
	wg.Wait()
}
