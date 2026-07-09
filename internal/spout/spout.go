// Package spout wraps the SpoutDX receiver (vendored Spout2 SDK sources,
// see SPOUT_LICENSE.txt) with a small cgo shim.
package spout

/*
#cgo CXXFLAGS: -O2 -mssse3 -msse4.1 -I${SRCDIR} -DNDEBUG
#cgo LDFLAGS: -lstdc++ -ld3d11 -ldxgi -lole32 -loleaut32 -luuid -luser32 -lgdi32 -ladvapi32 -lshell32 -lshlwapi -lversion -lpsapi -lcomctl32 -lcomdlg32 -lwinmm -lsetupapi -limm32
#include <stdlib.h>
#include "smr_shim.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Pixel format constants (DXGI_FORMAT values used by Spout senders).
const (
	FormatBGRA8 = 87 // DXGI_FORMAT_B8G8R8A8_UNORM (Spout default)
	FormatRGBA8 = 28 // DXGI_FORMAT_R8G8B8A8_UNORM
)

// Receive result flags.
const (
	Connected = 1
	Updated   = 2
	NewFrame  = 4
)

// Receiver receives frames from a single named Spout sender.
// A Receiver must only be used from one goroutine at a time.
type Receiver struct {
	h   unsafe.Pointer
	buf []byte
	w   int
	h2  int
}

// NewReceiver creates a receiver bound to sendername.
func NewReceiver(sendername string) (*Receiver, error) {
	cname := C.CString(sendername)
	defer C.free(unsafe.Pointer(cname))
	h := C.smr_create(cname)
	if h == nil {
		return nil, errors.New("spout: failed to create receiver (DirectX11 init failed)")
	}
	return &Receiver{h: h}, nil
}

// Close releases the receiver.
func (r *Receiver) Close() {
	if r.h != nil {
		C.smr_destroy(r.h)
		r.h = nil
	}
}

// Frame is the result of a Receive call. Pixels is only valid until the next
// Receive call and is either BGRA or RGBA depending on Format.
type Frame struct {
	Pixels    []byte
	Width     int
	Height    int
	Format    uint32 // DXGI format
	Connected bool
	NewFrame  bool
}

// Receive polls the sender. It transparently handles connect, reconnect and
// size changes. When Connected is false, the sender is currently gone
// (Pixels holds the last received frame, if any).
func (r *Receiver) Receive(invert bool) Frame {
	inv := C.int(0)
	if invert {
		inv = 1
	}
	var p *C.uchar
	if len(r.buf) > 0 {
		p = (*C.uchar)(unsafe.Pointer(&r.buf[0]))
	}
	flags := int(C.smr_receive(r.h, p, inv))

	if flags&Updated != 0 {
		// Sender appeared or changed size: resize our buffer, frame data
		// arrives on the next call.
		r.w = int(C.smr_width(r.h))
		r.h2 = int(C.smr_height(r.h))
		need := r.w * r.h2 * 4
		if need > 0 && len(r.buf) != need {
			r.buf = make([]byte, need)
		}
	}

	return Frame{
		Pixels:    r.buf,
		Width:     r.w,
		Height:    r.h2,
		Format:    uint32(C.smr_format(r.h)),
		Connected: flags&Connected != 0,
		NewFrame:  flags&NewFrame != 0,
	}
}

// SenderFPS returns the frame rate reported by the sender, or 0.
func (r *Receiver) SenderFPS() float64 {
	return float64(C.smr_sender_fps_x1000(r.h)) / 1000.0
}

// ListSenders returns the names of all active Spout senders.
func ListSenders() []string {
	n := int(C.smr_sender_count())
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	buf := make([]byte, 256)
	for i := 0; i < n; i++ {
		if C.smr_sender_name(C.int(i), (*C.char)(unsafe.Pointer(&buf[0])), 256) == 1 {
			// find NUL
			l := 0
			for l < len(buf) && buf[l] != 0 {
				l++
			}
			if l > 0 {
				out = append(out, string(buf[:l]))
			}
		}
	}
	return out
}
