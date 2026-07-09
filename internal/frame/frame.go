// Package frame provides a thread-safe latest-frame mailbox shared between
// the Spout capture loop, the recorder and the UI preview.
package frame

import (
	"image"
	"sync"
)

// Buffer holds the most recent frame of one channel.
type Buffer struct {
	mu        sync.RWMutex
	pix       []byte
	w, h      int
	format    uint32
	connected bool
	seq       uint64
}

// Store copies pix (w*h*4 bytes) into the mailbox.
func (b *Buffer) Store(pix []byte, w, h int, format uint32, connected bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	need := w * h * 4
	if need > 0 && len(pix) >= need {
		if cap(b.pix) < need {
			b.pix = make([]byte, need)
		}
		b.pix = b.pix[:need]
		copy(b.pix, pix[:need])
		b.w, b.h = w, h
	}
	b.format = format
	b.connected = connected
	b.seq++
}

// SetConnected updates only the connection state (keeps last pixels).
func (b *Buffer) SetConnected(c bool) {
	b.mu.Lock()
	b.connected = c
	b.seq++
	b.mu.Unlock()
}

// Dims returns current dimensions, format and connection state.
func (b *Buffer) Dims() (w, h int, format uint32, connected bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.w, b.h, b.format, b.connected
}

// Seq returns a counter incremented on every Store.
func (b *Buffer) Seq() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.seq
}

// Snapshot copies the current frame centered into dst which has dimensions
// dw x dh (dst len must be dw*dh*4). If the source is smaller, the border is
// left as-is (callers zero dst once); if larger, it is center-cropped.
// Returns false when the channel is currently disconnected (dst not written).
func (b *Buffer) Snapshot(dst []byte, dw, dh int) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.connected || b.w == 0 || b.h == 0 {
		return false
	}
	sw, sh := b.w, b.h
	if sw == dw && sh == dh {
		copy(dst, b.pix)
		return true
	}
	// center fit/crop
	cw, chh := min(sw, dw), min(sh, dh)
	sx, sy := (sw-cw)/2, (sh-chh)/2
	dx, dy := (dw-cw)/2, (dh-chh)/2
	for y := 0; y < chh; y++ {
		srow := ((sy+y)*sw + sx) * 4
		drow := ((dy+y)*dw + dx) * 4
		copy(dst[drow:drow+cw*4], b.pix[srow:srow+cw*4])
	}
	return true
}

// Preview renders a downscaled RGBA preview at most maxW wide (nearest
// neighbour). Returns nil when no frame is available.
func (b *Buffer) Preview(maxW int) *image.NRGBA {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.w == 0 || b.h == 0 || len(b.pix) < b.w*b.h*4 {
		return nil
	}
	pw := b.w
	ph := b.h
	if pw > maxW {
		ph = ph * maxW / pw
		pw = maxW
	}
	if pw < 1 || ph < 1 {
		return nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, pw, ph))
	swapRB := b.format != 28 // everything except RGBA8 treated as BGRA
	for y := 0; y < ph; y++ {
		sy := y * b.h / ph
		srow := sy * b.w * 4
		drow := y * img.Stride
		for x := 0; x < pw; x++ {
			s := srow + (x*b.w/pw)*4
			d := drow + x*4
			if swapRB {
				img.Pix[d+0] = b.pix[s+2]
				img.Pix[d+1] = b.pix[s+1]
				img.Pix[d+2] = b.pix[s+0]
			} else {
				img.Pix[d+0] = b.pix[s+0]
				img.Pix[d+1] = b.pix[s+1]
				img.Pix[d+2] = b.pix[s+2]
			}
			img.Pix[d+3] = 255
		}
	}
	return img
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
