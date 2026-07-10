package engine

import (
	"sync"
	"time"
)

const (
	pumpRate  = 48000
	pumpMaxCh = 16
)

// audioPump distributes an NDI channel's native audio to recorder
// subscribers. The channel count follows the source (up to pumpMaxCh) and is
// locked while a recording is subscribed; incoming frames with a different
// count are adapted (extra channels dropped, missing ones silent). Silence is
// emitted at wall-clock rate when the source stops delivering audio so the
// muxer never starves.
type audioPump struct {
	mu       sync.Mutex
	subs     map[int]chan []byte
	nextID   int
	srcCh    int // latest channel count seen from the source
	lockCh   int // format locked while subscribers exist
	written  int64
	start    time.Time
	lastReal time.Time
	levels   [pumpMaxCh]float64 // per-channel peak of the last real frame
}

func newAudioPump() *audioPump {
	return &audioPump{subs: map[int]chan []byte{}}
}

// Channels implements recorder.AudioSource: the channel count of the stream
// delivered to current subscribers (locked at Subscribe time).
func (p *audioPump) Channels() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lockCh > 0 {
		return p.lockCh
	}
	if p.srcCh > 0 {
		return p.srcCh
	}
	return 2
}

// Subscribe implements recorder.AudioSource. The first subscriber locks the
// stream format to the source's current channel count.
func (p *audioPump) Subscribe() (<-chan []byte, func()) {
	p.mu.Lock()
	if len(p.subs) == 0 {
		lc := p.srcCh
		if lc <= 0 {
			lc = 2
		}
		if lc > pumpMaxCh {
			lc = pumpMaxCh
		}
		p.lockCh = lc
		p.start = time.Now()
		p.written = 0
	}
	id := p.nextID
	p.nextID++
	ch := make(chan []byte, 256)
	p.subs[id] = ch
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		if c, ok := p.subs[id]; ok {
			delete(p.subs, id)
			close(c)
		}
		if len(p.subs) == 0 {
			p.lockCh = 0 // unlock: next recording follows the source again
		}
		p.mu.Unlock()
	}
}

// push distributes real audio (interleaved s16le 48 kHz with srcCh channels).
// b may be reused by the caller after return.
func (p *audioPump) push(b []byte, srcCh int) {
	if len(b) == 0 || srcCh <= 0 {
		return
	}
	p.mu.Lock()
	p.srcCh = srcCh
	p.updateLevels(b, srcCh)
	if len(p.subs) == 0 {
		p.lastReal = time.Now()
		p.mu.Unlock()
		return
	}
	lock := p.lockCh
	var chunk []byte
	if srcCh == lock {
		chunk = make([]byte, len(b))
		copy(chunk, b)
	} else {
		// Adapt frame-by-frame to the locked channel count.
		frames := len(b) / (srcCh * 2)
		chunk = make([]byte, frames*lock*2)
		n := srcCh
		if n > lock {
			n = lock
		}
		for f := 0; f < frames; f++ {
			copy(chunk[f*lock*2:f*lock*2+n*2], b[f*srcCh*2:f*srcCh*2+n*2])
		}
	}
	p.lastReal = time.Now()
	for _, ch := range p.subs {
		select {
		case ch <- chunk:
		default: // slow subscriber: drop
		}
	}
	p.written += int64(len(chunk))
	p.mu.Unlock()
}

// updateLevels records the per-channel peak of an interleaved s16le frame.
// Caller holds p.mu.
func (p *audioPump) updateLevels(b []byte, srcCh int) {
	n := srcCh
	if n > pumpMaxCh {
		n = pumpMaxCh
	}
	frames := len(b) / (srcCh * 2)
	for c := 0; c < n; c++ {
		var peak int32
		for f := 0; f < frames; f++ {
			i := (f*srcCh + c) * 2
			v := int32(int16(uint16(b[i]) | uint16(b[i+1])<<8))
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		p.levels[c] = float64(peak) / 32767.0
	}
}

// Levels returns the current per-channel peaks (0..1) of the source's audio,
// or nil when the source has never delivered audio. Zeros while audio is
// paused so meters fall to silence.
func (p *audioPump) Levels() []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.srcCh <= 0 {
		return nil
	}
	n := p.srcCh
	if n > pumpMaxCh {
		n = pumpMaxCh
	}
	out := make([]float64, n)
	if time.Since(p.lastReal) <= 400*time.Millisecond {
		copy(out, p.levels[:n])
	}
	return out
}

// run emits silence while no real audio is arriving, keeping the stream at
// wall-clock rate. Stops when stop is closed.
func (p *audioPump) run(stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	var silence []byte
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		p.mu.Lock()
		if len(p.subs) == 0 || p.lockCh == 0 {
			p.mu.Unlock()
			continue
		}
		bps := int64(pumpRate * 2 * p.lockCh)
		frameBytes := int64(p.lockCh * 2)
		expected := int64(time.Since(p.start)) * bps / int64(time.Second)
		if time.Since(p.lastReal) > 400*time.Millisecond && p.written < expected {
			need := expected - p.written
			if maxChunk := bps * 3 / 10; need > maxChunk {
				need = maxChunk // cap catch-up bursts
			}
			need -= need % frameBytes
			if need > 0 {
				if int64(len(silence)) < need {
					silence = make([]byte, need)
				}
				chunk := silence[:need]
				for _, ch := range p.subs {
					select {
					case ch <- chunk:
					default:
					}
				}
				p.written += need
			}
		}
		// While real audio flows its own clock rules; keep the budget from
		// drifting far so a later dropout pads a sensible amount.
		if diff := expected - p.written; diff > bps || diff < -bps {
			p.written = expected
		}
		p.mu.Unlock()
	}
}
