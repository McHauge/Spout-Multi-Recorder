// Package audio captures one master audio source (microphone/line input or
// speaker loopback) via WASAPI and fans the PCM data out to any number of
// subscribers (one per running recorder), while tracking peak levels for the
// UI VU meter.
package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/gen2brain/malgo"
)

const (
	SampleRate = 48000
	Channels   = 2
	// BytesPerSecond of the s16le stream fed to FFmpeg.
	BytesPerSecond = SampleRate * Channels * 2
)

// Device describes a selectable audio source.
type Device struct {
	Name     string
	Loopback bool // true = a playback device captured via WASAPI loopback
	id       malgo.DeviceID
}

// Label returns the UI label for the device.
func (d Device) Label() string {
	if d.Loopback {
		return "🔊 " + d.Name + " (what you hear)"
	}
	return "🎤 " + d.Name
}

// Engine owns the malgo context and the currently running capture device.
type Engine struct {
	ctx *malgo.AllocatedContext

	mu     sync.Mutex
	device *malgo.Device
	subs   map[int]chan []byte
	nextID int

	// VU levels, stored as atomic uint32 of level*1e6 (0..1e6).
	peakL atomic.Uint32
	peakR atomic.Uint32
}

// NewEngine initialises WASAPI.
func NewEngine() (*Engine, error) {
	ctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendWasapi}, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("audio: init context: %w", err)
	}
	return &Engine{ctx: ctx, subs: map[int]chan []byte{}}, nil
}

// Close stops capture and frees the context.
func (e *Engine) Close() {
	e.StopCapture()
	if e.ctx != nil {
		_ = e.ctx.Uninit()
		e.ctx.Free()
		e.ctx = nil
	}
}

// Devices lists capture devices and playback devices (as loopback sources).
func (e *Engine) Devices() ([]Device, error) {
	var out []Device
	caps, err := e.ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, fmt.Errorf("audio: list capture devices: %w", err)
	}
	for _, d := range caps {
		out = append(out, Device{Name: d.Name(), Loopback: false, id: d.ID})
	}
	plays, err := e.ctx.Devices(malgo.Playback)
	if err != nil {
		return nil, fmt.Errorf("audio: list playback devices: %w", err)
	}
	for _, d := range plays {
		out = append(out, Device{Name: d.Name(), Loopback: true, id: d.ID})
	}
	return out, nil
}

// StartCapture starts (or switches) the master capture to dev.
func (e *Engine) StartCapture(dev Device) error {
	e.StopCapture()

	devType := malgo.Capture
	if dev.Loopback {
		devType = malgo.Loopback
	}
	cfg := malgo.DefaultDeviceConfig(devType)
	cfg.SampleRate = SampleRate
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = Channels
	// For loopback, the capture device ID must be the *playback* device ID.
	id := dev.id // copy: pointer must stay valid while device runs
	cfg.Capture.DeviceID = id.Pointer()

	onRecv := func(_, pInput []byte, _ uint32) {
		e.updateLevels(pInput)
		// Copy: malgo reuses the buffer.
		chunk := make([]byte, len(pInput))
		copy(chunk, pInput)
		e.mu.Lock()
		for _, ch := range e.subs {
			select {
			case ch <- chunk:
			default: // subscriber too slow: drop rather than block capture
			}
		}
		e.mu.Unlock()
	}

	d, err := malgo.InitDevice(e.ctx.Context, cfg, malgo.DeviceCallbacks{Data: onRecv})
	if err != nil {
		return fmt.Errorf("audio: init device %q: %w", dev.Name, err)
	}
	if err := d.Start(); err != nil {
		d.Uninit()
		return fmt.Errorf("audio: start device %q: %w", dev.Name, err)
	}
	e.mu.Lock()
	e.device = d
	e.mu.Unlock()
	return nil
}

// StopCapture stops the master capture (subscribers stop receiving data).
func (e *Engine) StopCapture() {
	e.mu.Lock()
	d := e.device
	e.device = nil
	e.mu.Unlock()
	if d != nil {
		d.Uninit()
	}
	e.peakL.Store(0)
	e.peakR.Store(0)
}

// Running reports whether a capture device is active.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.device != nil
}

// Subscribe returns a channel receiving raw s16le 48kHz stereo chunks and an
// unsubscribe function. Slow consumers miss chunks instead of blocking capture.
func (e *Engine) Subscribe() (<-chan []byte, func()) {
	e.mu.Lock()
	id := e.nextID
	e.nextID++
	ch := make(chan []byte, 256) // ~ several seconds of headroom
	e.subs[id] = ch
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		if c, ok := e.subs[id]; ok {
			delete(e.subs, id)
			close(c)
		}
		e.mu.Unlock()
	}
}

// Levels returns the current peak levels (0..1) for left and right.
func (e *Engine) Levels() (l, r float64) {
	return float64(e.peakL.Load()) / 1e6, float64(e.peakR.Load()) / 1e6
}

func (e *Engine) updateLevels(pcm []byte) {
	var pl, pr int32
	n := len(pcm) / 4 // frames (2ch * 2 bytes)
	for i := 0; i < n; i++ {
		l := int32(int16(binary.LittleEndian.Uint16(pcm[i*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(pcm[i*4+2:])))
		if l < 0 {
			l = -l
		}
		if r < 0 {
			r = -r
		}
		if l > pl {
			pl = l
		}
		if r > pr {
			pr = r
		}
	}
	e.peakL.Store(uint32(math.Round(float64(pl) / 32767.0 * 1e6)))
	e.peakR.Store(uint32(math.Round(float64(pr) / 32767.0 * 1e6)))
}
