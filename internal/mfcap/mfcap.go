// Package mfcap captures UVC/webcam video via Media Foundation through a small
// cgo C++ shim (see mfcap_shim.cpp). Frames are delivered as top-down BGRA,
// matching the pixel format the rest of the pipeline expects.
package mfcap

/*
#cgo CXXFLAGS: -O2 -I${SRCDIR} -DNDEBUG -std=c++17
#cgo LDFLAGS: -lstdc++ -lmfplat -lmf -lmfreadwrite -lmfuuid -lole32 -luuid
#include <stdlib.h>
#include "mfcap_shim.h"
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
	flagLost      = 8
)

// Available reports whether Media Foundation could be initialised. On Windows N
// editions without the Media Feature Pack this fails.
func Available() error {
	if C.mfcap_available() == 0 {
		return errors.New("webcam: Media Foundation unavailable (install the Media Feature Pack on Windows N)")
	}
	return nil
}

// Device is an enumerated webcam.
type Device struct {
	Name string // friendly name for the UI
	Link string // stable symbolic link used to (re)open the device
}

// Devices enumerates the connected video capture devices.
func Devices() ([]Device, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	n := int(C.mfcap_enum())
	if n < 0 {
		return nil, errors.New("webcam: device enumeration failed")
	}
	out := make([]Device, 0, n)
	buf := make([]byte, 1024)
	for i := 0; i < n; i++ {
		name := readStr(func(p *C.char, l C.int) C.int {
			return C.mfcap_device_name(C.int(i), p, l)
		}, buf)
		link := readStr(func(p *C.char, l C.int) C.int {
			return C.mfcap_device_link(C.int(i), p, l)
		}, buf)
		if link == "" {
			continue
		}
		out = append(out, Device{Name: name, Link: link})
	}
	return out, nil
}

func readStr(fn func(*C.char, C.int) C.int, buf []byte) string {
	n := int(fn((*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}

// Mode is a supported webcam video mode. The zero Mode means "auto".
type Mode struct {
	W, H     int
	FPSx1000 int
}

// FPS returns the frame rate as a float (e.g. 29.97).
func (m Mode) FPS() float64 { return float64(m.FPSx1000) / 1000 }

// Modes lists the distinct video modes offered by the device (largest first).
func Modes(link string) ([]Mode, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	clink := C.CString(link)
	defer C.free(unsafe.Pointer(clink))
	n := int(C.mfcap_enum_modes(clink))
	if n < 0 {
		return nil, errors.New("webcam: mode enumeration failed")
	}
	out := make([]Mode, 0, n)
	for i := 0; i < n; i++ {
		var w, h, fps C.uint
		if C.mfcap_mode(C.int(i), &w, &h, &fps) == 1 {
			out = append(out, Mode{W: int(w), H: int(h), FPSx1000: int(fps)})
		}
	}
	return out, nil
}

// Capture is one open webcam. It must be used from a single goroutine at a
// time (Latest reads a lock-protected mailbox; Close tears it down).
type Capture struct {
	h   unsafe.Pointer
	buf []byte
}

// Open opens the webcam with the given symbolic link. The zero Mode auto-picks
// the best mode; if only mode.FPSx1000 is set, the highest resolution reaching
// that frame rate is chosen; a fully specified mode is used as-is.
func Open(link string, mode Mode) (*Capture, error) {
	clink := C.CString(link)
	defer C.free(unsafe.Pointer(clink))
	h := C.mfcap_open(clink, C.uint(mode.W), C.uint(mode.H), C.uint(mode.FPSx1000))
	if h == nil {
		return nil, errors.New("webcam: failed to open device")
	}
	return &Capture{h: h}, nil
}

// Close releases the device.
func (c *Capture) Close() {
	if c.h != nil {
		C.mfcap_close(c.h)
		c.h = nil
	}
}

// Frame is the result of Latest. Pixels is top-down BGRA, valid only until the
// next Latest call.
type Frame struct {
	Pixels    []byte
	Width     int
	Height    int
	Connected bool
	NewFrame  bool
	Lost      bool
}

// Latest returns the newest frame (copying it into an internal buffer) plus the
// current connection state. It never blocks.
func (c *Capture) Latest() Frame {
	w := int(C.mfcap_width(c.h))
	h := int(C.mfcap_height(c.h))
	need := w * h * 4
	if need > 0 && len(c.buf) < need {
		c.buf = make([]byte, need)
	}
	var p *C.uchar
	if len(c.buf) > 0 {
		p = (*C.uchar)(unsafe.Pointer(&c.buf[0]))
	}
	flags := int(C.mfcap_latest(c.h, p, C.int(len(c.buf))))
	f := Frame{
		Width:     w,
		Height:    h,
		Connected: flags&flagConnected != 0,
		NewFrame:  flags&flagNewFrame != 0,
		Lost:      flags&flagLost != 0,
	}
	if f.NewFrame {
		f.Pixels = c.buf[:need]
	}
	return f
}

// FPSx1000 returns the negotiated frame rate times 1000 (0 if unknown).
func (c *Capture) FPSx1000() int { return int(C.mfcap_fps_x1000(c.h)) }
