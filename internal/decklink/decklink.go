// Package decklink captures Blackmagic DeckLink SDI/HDMI input (video + up to
// 16 channels of embedded audio) via the DeckLink COM API through a small cgo
// C++ shim (see dl_shim.cpp). The interface declarations in dl_com.h are a
// minimal subset generated from the installed driver's type library, so no
// Blackmagic SDK download is required to build.
package decklink

/*
#cgo CXXFLAGS: -O2 -I${SRCDIR} -DNDEBUG -std=c++17
#cgo LDFLAGS: -lstdc++ -lole32 -loleaut32 -luuid
#include <stdlib.h>
#include "dl_shim.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Result flags from Capture.Latest.
const (
	flagConnected = 1
	flagNewFrame  = 4
)

// Available reports whether the Blackmagic Desktop Video driver is installed.
func Available() error {
	if C.dl_available() == 0 {
		return errors.New("decklink: Blackmagic Desktop Video driver not found")
	}
	return nil
}

// Devices returns the display names of the connected DeckLink devices.
func Devices() ([]string, error) {
	n := int(C.dl_device_count())
	if n < 0 {
		return nil, errors.New("decklink: device enumeration failed")
	}
	out := make([]string, 0, n)
	buf := make([]byte, 256)
	for i := 0; i < n; i++ {
		l := int(C.dl_device_name(C.int(i), (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
		if l <= 0 {
			continue
		}
		out = append(out, string(buf[:l]))
	}
	return out, nil
}

// Capture is one open DeckLink input. Use from a single goroutine at a time.
type Capture struct {
	h   unsafe.Pointer
	buf []byte
}

// Open opens the input of the device with the given display name.
func Open(name string) (*Capture, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	h := C.dl_open(cname)
	if h == nil {
		return nil, errors.New("decklink: failed to open device")
	}
	return &Capture{h: h}, nil
}

// Close releases the device.
func (c *Capture) Close() {
	if c.h != nil {
		C.dl_close(c.h)
		c.h = nil
	}
}

// Frame is the result of Latest. Pixels is BGRA, valid only until the next call.
type Frame struct {
	Pixels    []byte
	Width     int
	Height    int
	Connected bool
	NewFrame  bool
}

// Latest returns the newest video frame plus the current signal state. It never
// blocks.
func (c *Capture) Latest() Frame {
	w := int(C.dl_width(c.h))
	h := int(C.dl_height(c.h))
	need := w * h * 4
	if need > 0 && len(c.buf) < need {
		c.buf = make([]byte, need)
	}
	var p *C.uchar
	if len(c.buf) > 0 {
		p = (*C.uchar)(unsafe.Pointer(&c.buf[0]))
	}
	flags := int(C.dl_video_latest(c.h, p, C.int(len(c.buf))))
	f := Frame{
		Width:     w,
		Height:    h,
		Connected: flags&flagConnected != 0,
		NewFrame:  flags&flagNewFrame != 0,
	}
	if f.NewFrame {
		f.Pixels = c.buf[:need]
	}
	return f
}

// ReadAudio drains up to len(dst) bytes of interleaved s16le 48kHz audio into
// dst, returning the number of bytes written and the channel count.
func (c *Capture) ReadAudio(dst []byte) (n, channels int) {
	if len(dst) == 0 {
		return 0, int(C.dl_audio_channels(c.h))
	}
	var ch C.int
	n = int(C.dl_audio_read(c.h, (*C.uchar)(unsafe.Pointer(&dst[0])), C.int(len(dst)), &ch))
	return n, int(ch)
}
