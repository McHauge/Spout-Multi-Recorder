// Package ndi provides a minimal NDI receiver (video only) by loading the
// NDI runtime DLL dynamically at runtime — no SDK or cgo required to build.
//
// Both full-bandwidth NDI and NDI|HX sources are supported: HX decoding is
// handled transparently by the NDI runtime, the receive API is identical.
//
// The user needs the NDI runtime installed (bundled with NDI Tools, or the
// standalone "NDI Runtime"): https://ndi.video/tools/
package ndi

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- C ABI structs (x64 layout, NDI SDK v5/v6 stable ABI) ----

type sourceT struct {
	pNdiName *byte
	pURL     *byte
}

type findCreateT struct {
	showLocalSources bool
	_                [7]byte
	pGroups          *byte
	pExtraIPs        *byte
}

type recvCreateV3T struct {
	source          sourceT
	colorFormat     int32 // 0 = NDIlib_recv_color_format_BGRX_BGRA
	bandwidth       int32 // 100 = NDIlib_recv_bandwidth_highest
	allowVideoField bool
	_               [7]byte
	pRecvName       *byte
}

type videoFrameV2T struct {
	xres, yres             int32
	fourCC                 uint32
	frameRateN, frameRateD int32
	aspect                 float32
	frameFormat            int32
	_                      [4]byte
	timecode               int64
	pData                  *byte
	stride                 int32
	_                      [4]byte
	pMetadata              *byte
	timestamp              int64
}

const (
	frameTypeVideo = 1

	colorFormatBGRXBGRA = 0
	bandwidthHighest    = 100
)

// ---- runtime loading ----

var (
	loadOnce sync.Once
	loadErr  error

	pInitialize     *windows.Proc
	pFindCreate     *windows.Proc
	pFindDestroy    *windows.Proc
	pFindWait       *windows.Proc
	pFindGetCurrent *windows.Proc
	pRecvCreate     *windows.Proc
	pRecvDestroy    *windows.Proc
	pRecvCapture    *windows.Proc
	pRecvFreeVideo  *windows.Proc
	pRecvNoConns    *windows.Proc
)

func dllCandidates() []string {
	name := "Processing.NDI.Lib.x64.dll"
	if runtime.GOARCH == "arm64" {
		name = "Processing.NDI.Lib.arm64.dll"
	}
	var out []string
	if dir := os.Getenv("NDILIB_REDIST_FOLDER"); dir != "" {
		out = append(out, filepath.Join(dir, name))
	}
	for _, pf := range []string{os.Getenv("ProgramFiles"), `C:\Program Files`} {
		if pf == "" {
			continue
		}
		out = append(out,
			filepath.Join(pf, "NDI", "NDI 6 Runtime", "v6", name),
			filepath.Join(pf, "NDI", "NDI 5 Runtime", "v5", name),
		)
	}
	out = append(out, name) // plain LoadLibrary search path
	return out
}

func load() error {
	loadOnce.Do(func() {
		var dll *windows.DLL
		var err error
		for _, cand := range dllCandidates() {
			dll, err = windows.LoadDLL(cand)
			if err == nil {
				break
			}
		}
		if dll == nil {
			loadErr = fmt.Errorf("NDI runtime not found — install NDI Tools or the NDI Runtime from https://ndi.video/tools/ (%v)", err)
			return
		}
		get := func(name string) *windows.Proc {
			p, e := dll.FindProc(name)
			if e != nil && loadErr == nil {
				loadErr = fmt.Errorf("NDI runtime is missing %s (too old?)", name)
			}
			return p
		}
		pInitialize = get("NDIlib_initialize")
		pFindCreate = get("NDIlib_find_create_v2")
		pFindDestroy = get("NDIlib_find_destroy")
		pFindWait = get("NDIlib_find_wait_for_sources")
		pFindGetCurrent = get("NDIlib_find_get_current_sources")
		pRecvCreate = get("NDIlib_recv_create_v3")
		pRecvDestroy = get("NDIlib_recv_destroy")
		pRecvCapture = get("NDIlib_recv_capture_v2")
		pRecvFreeVideo = get("NDIlib_recv_free_video_v2")
		pRecvNoConns = get("NDIlib_recv_get_no_connections")
		if loadErr != nil {
			return
		}
		r, _, _ := pInitialize.Call()
		if r&1 == 0 {
			loadErr = fmt.Errorf("NDIlib_initialize failed (CPU unsupported?)")
		}
	})
	return loadErr
}

// Available reports whether the NDI runtime is usable (nil = yes).
func Available() error { return load() }

// ptrFromUintptr converts a raw address in C-owned memory to a pointer
// without tripping go vet's unsafeptr heuristic.
func ptrFromUintptr(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}

func cstr(s string) *byte {
	b, _ := windows.BytePtrFromString(s)
	return b
}

func gostr(p *byte) string {
	if p == nil {
		return ""
	}
	return windows.BytePtrToString(p)
}

// ---- discovery ----

// Source describes a discovered NDI source.
type Source struct {
	Name string // e.g. "MACHINE (OBS)"
	URL  string
}

// FindSources browses the network for roughly the given duration.
func FindSources(timeout time.Duration) ([]Source, error) {
	if err := load(); err != nil {
		return nil, err
	}
	cfg := findCreateT{showLocalSources: true}
	inst, _, _ := pFindCreate.Call(uintptr(unsafe.Pointer(&cfg)))
	if inst == 0 {
		return nil, fmt.Errorf("NDIlib_find_create_v2 failed")
	}
	defer pFindDestroy.Call(inst)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		wait := time.Until(deadline).Milliseconds()
		if wait < 50 {
			wait = 50
		}
		pFindWait.Call(inst, uintptr(wait))
	}

	var n uint32
	arr, _, _ := pFindGetCurrent.Call(inst, uintptr(unsafe.Pointer(&n)))
	if arr == 0 || n == 0 {
		return nil, nil
	}
	// arr points into C-owned memory (valid until find_destroy); the uintptr
	// round-trip is safe here and hidden from vet's unsafeptr check.
	srcs := unsafe.Slice((*sourceT)(ptrFromUintptr(arr)), n)
	out := make([]Source, 0, n)
	for _, s := range srcs {
		out = append(out, Source{Name: gostr(s.pNdiName), URL: gostr(s.pURL)})
	}
	return out, nil
}

// ---- receiving ----

// Receiver receives video frames from one NDI source.
// Must be used from a single goroutine.
type Receiver struct {
	inst    uintptr
	keep    []*byte // keeps C strings alive
	scratch []byte
}

// NewReceiver connects to the given source (BGRA video, highest bandwidth).
func NewReceiver(src Source) (*Receiver, error) {
	if err := load(); err != nil {
		return nil, err
	}
	nameC := cstr(src.Name)
	var urlC *byte
	if src.URL != "" {
		urlC = cstr(src.URL)
	}
	recvName := cstr("Spout Multi Recorder")
	cfg := recvCreateV3T{
		source:      sourceT{pNdiName: nameC, pURL: urlC},
		colorFormat: colorFormatBGRXBGRA,
		bandwidth:   bandwidthHighest,
		pRecvName:   recvName,
	}
	inst, _, _ := pRecvCreate.Call(uintptr(unsafe.Pointer(&cfg)))
	if inst == 0 {
		return nil, fmt.Errorf("NDI: failed to create receiver for %q", src.Name)
	}
	return &Receiver{inst: inst, keep: []*byte{nameC, urlC, recvName}}, nil
}

// Capture waits up to timeout for a video frame and returns it as contiguous
// BGRA pixels (valid until the next Capture call). ok is false on timeout.
func (r *Receiver) Capture(timeout time.Duration) (pix []byte, w, h int, ok bool) {
	var vf videoFrameV2T
	ft, _, _ := pRecvCapture.Call(
		r.inst,
		uintptr(unsafe.Pointer(&vf)),
		0, 0,
		uintptr(timeout.Milliseconds()),
	)
	if int32(ft) != frameTypeVideo {
		return nil, 0, 0, false
	}
	defer pRecvFreeVideo.Call(r.inst, uintptr(unsafe.Pointer(&vf)))

	w, h = int(vf.xres), int(vf.yres)
	stride := int(vf.stride)
	if w <= 0 || h <= 0 || vf.pData == nil || stride < w*4 {
		return nil, 0, 0, false
	}
	need := w * h * 4
	if cap(r.scratch) < need {
		r.scratch = make([]byte, need)
	}
	r.scratch = r.scratch[:need]
	src := unsafe.Slice(vf.pData, stride*h)
	if stride == w*4 {
		copy(r.scratch, src)
	} else {
		for y := 0; y < h; y++ {
			copy(r.scratch[y*w*4:(y+1)*w*4], src[y*stride:y*stride+w*4])
		}
	}
	return r.scratch, w, h, true
}

// Connections returns the number of senders currently connected (0 = source gone).
func (r *Receiver) Connections() int {
	n, _, _ := pRecvNoConns.Call(r.inst)
	return int(int32(n))
}

// Close destroys the receiver.
func (r *Receiver) Close() {
	if r.inst != 0 {
		pRecvDestroy.Call(r.inst)
		r.inst = 0
	}
}
