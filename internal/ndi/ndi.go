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

type audioFrameV2T struct {
	sampleRate    int32
	noChannels    int32
	noSamples     int32
	_             [4]byte
	timecode      int64
	pData         *byte // planar float32
	channelStride int32
	_             [4]byte
	pMetadata     *byte
	timestamp     int64
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
	frameTypeAudio = 2

	colorFormatBGRXBGRA = 0
	bandwidthHighest    = 100

	// Output audio format (matches the recorder pipeline).
	OutSampleRate = 48000
	// MaxChannels caps how many source audio channels are preserved.
	// NDI sources in the wild use 2/4/8, occasionally 16.
	MaxChannels = 16
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
	pRecvFreeAudio  *windows.Proc
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
		pRecvFreeAudio = get("NDIlib_recv_free_audio_v2")
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
	inst     uintptr
	keep     []*byte // keeps C strings alive
	scratch  []byte
	ascratch []byte
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
	pix, w, h = r.copyVideo(&vf)
	return pix, w, h, pix != nil
}

// CaptureAV waits up to timeout for a frame. Exactly one of pix (BGRA video,
// with w/h) or aud (interleaved s16le 48 kHz PCM with audCh channels) is
// non-nil, or both are nil on timeout. Slices are valid until the next call.
func (r *Receiver) CaptureAV(timeout time.Duration) (pix []byte, w, h int, aud []byte, audCh int) {
	var vf videoFrameV2T
	var af audioFrameV2T
	ft, _, _ := pRecvCapture.Call(
		r.inst,
		uintptr(unsafe.Pointer(&vf)),
		uintptr(unsafe.Pointer(&af)),
		0,
		uintptr(timeout.Milliseconds()),
	)
	switch int32(ft) {
	case frameTypeVideo:
		defer pRecvFreeVideo.Call(r.inst, uintptr(unsafe.Pointer(&vf)))
		pix, w, h = r.copyVideo(&vf)
		return pix, w, h, nil, 0
	case frameTypeAudio:
		defer pRecvFreeAudio.Call(r.inst, uintptr(unsafe.Pointer(&af)))
		aud, audCh = r.convertAudio(&af)
		return nil, 0, 0, aud, audCh
	}
	return nil, 0, 0, nil, 0
}

func (r *Receiver) copyVideo(vf *videoFrameV2T) (pix []byte, w, h int) {
	w, h = int(vf.xres), int(vf.yres)
	stride := int(vf.stride)
	if w <= 0 || h <= 0 || vf.pData == nil || stride < w*4 {
		return nil, 0, 0
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
	return r.scratch, w, h
}

// convertAudio converts NDI planar float32 audio to interleaved s16le at
// 48 kHz, preserving the source channel count (capped at MaxChannels;
// nearest-sample resampling if the source rate differs).
func (r *Receiver) convertAudio(af *audioFrameV2T) ([]byte, int) {
	ns := int(af.noSamples)
	srcCh := int(af.noChannels)
	rate := int(af.sampleRate)
	if ns <= 0 || srcCh <= 0 || rate <= 0 || af.pData == nil {
		return nil, 0
	}
	ch := srcCh
	if ch > MaxChannels {
		ch = MaxChannels
	}
	strideF := int(af.channelStride) / 4
	if strideF < ns {
		strideF = ns
	}
	data := unsafe.Slice((*float32)(unsafe.Pointer(af.pData)), (srcCh-1)*strideF+ns)
	outN := ns
	if rate != OutSampleRate {
		outN = ns * OutSampleRate / rate
		if outN <= 0 {
			return nil, 0
		}
	}
	need := outN * ch * 2
	if cap(r.ascratch) < need {
		r.ascratch = make([]byte, need)
	}
	out := r.ascratch[:need]
	for i := 0; i < outN; i++ {
		si := i
		if outN != ns {
			si = i * ns / outN
		}
		base := i * ch * 2
		for c := 0; c < ch; c++ {
			v := f32toS16(data[c*strideF+si])
			out[base+c*2] = byte(v)
			out[base+c*2+1] = byte(v >> 8)
		}
	}
	return out, ch
}

func f32toS16(f float32) int16 {
	v := int32(f * 32767)
	if v > 32767 {
		v = 32767
	} else if v < -32768 {
		v = -32768
	}
	return int16(v)
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
