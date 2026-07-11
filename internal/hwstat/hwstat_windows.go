//go:build windows

package hwstat

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sampleState holds per-sampler OS handles, the previous CPU snapshot (so CPU
// load can be derived from deltas), and the GPU LUID→vendor map used to
// attribute encode-engine counters to the right vendor.
type sampleState struct {
	// CPU delta tracking (100-ns FILETIME ticks).
	prevIdle, prevKernel, prevUser uint64
	havePrevCPU                    bool

	// PDH GPU Engine (video-encode) query, opened once and reused.
	pdhQuery   uintptr
	pdhCounter uintptr
	pdhOK      bool

	// Maps an adapter LUID key ("high_low", 8 hex each) to its encoder vendor.
	luidVendor map[string]Vendor
}

func newSampleState() *sampleState {
	s := &sampleState{luidVendor: buildLuidVendor()}
	s.openPDH()
	return s
}

func (s *sampleState) close() {
	if s.pdhQuery != 0 {
		pdhCloseQuery(s.pdhQuery)
		s.pdhQuery = 0
	}
}

// sample builds one utilization snapshot: real per-vendor video-encode
// utilization from the Windows GPU Engine counters (attributed by adapter
// LUID) plus system-wide CPU load. Numbers match Task Manager's per-engine
// "Video Encode" / "Video Codec Engine" graphs.
func sample(s *sampleState) Load {
	cpu := s.cpuPct()
	enc := s.gpuEncodeByVendor()
	return Load{
		NVENC: enc[VendorNVENC],
		AMD:   enc[VendorAMD],
		Intel: enc[VendorIntel],
		CPU:   clampPct(cpu),
	}
}

// --- GPU enumeration via DXGI ----------------------------------------------

var (
	moddxgi               = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory = moddxgi.NewProc("CreateDXGIFactory")
)

// IID_IDXGIFactory {7b7166ec-21c7-44ae-b21a-c9ae321ae369}
var iidIDXGIFactory = windows.GUID{
	Data1: 0x7b7166ec, Data2: 0x21c7, Data3: 0x44ae,
	Data4: [8]byte{0xb2, 0x1a, 0xc9, 0xae, 0x32, 0x1a, 0xe3, 0x69},
}

// dxgiAdapterDesc mirrors DXGI_ADAPTER_DESC; VendorId and AdapterLuid are used.
type dxgiAdapterDesc struct {
	Description           [128]uint16
	VendorId              uint32
	DeviceId              uint32
	SubSysId              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	AdapterLuid           windows.LUID
}

// PCI vendor IDs for the encoder-capable GPU makers.
const (
	vendorNVIDIA = 0x10DE
	vendorAMD    = 0x1002
	vendorIntel  = 0x8086
)

// comVtbl is a COM object's method table; 16 slots cover every interface we
// call. comObject is the interface pointer itself (first word is the vtable).
type comVtbl struct{ m [16]uintptr }
type comObject struct{ vtbl *comVtbl }

// comCall invokes the vtable method at idx on a COM interface pointer.
func comCall(this unsafe.Pointer, idx int, args ...uintptr) uintptr {
	obj := (*comObject)(this)
	ret, _, _ := syscall.SyscallN(obj.vtbl.m[idx], append([]uintptr{uintptr(this)}, args...)...)
	return ret
}

type adapterInfo struct {
	vendor Vendor
	luid   string // "high_low", 8 lowercase hex digits each
}

// luidKey formats a LUID the same way GPU Engine counter instances encode it.
func luidKey(high uint32, low uint32) string {
	return fmt.Sprintf("%08x_%08x", high, low)
}

// enumAdapters lists the encoder-capable display adapters (NVIDIA/AMD/Intel)
// with their LUIDs. The second result is false when DXGI is unavailable, so
// callers can fail open. IDXGIFactory::EnumAdapters is vtable slot 7,
// IDXGIAdapter::GetDesc is slot 8, IUnknown::Release is slot 2.
func enumAdapters() ([]adapterInfo, bool) {
	if err := procCreateDXGIFactory.Find(); err != nil {
		return nil, false
	}
	var factory unsafe.Pointer
	r, _, _ := procCreateDXGIFactory.Call(
		uintptr(unsafe.Pointer(&iidIDXGIFactory)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if r != 0 || factory == nil {
		return nil, false
	}
	defer comCall(factory, 2) // Release factory

	var out []adapterInfo
	for i := 0; ; i++ {
		var adapter unsafe.Pointer
		if comCall(factory, 7, uintptr(uint32(i)), uintptr(unsafe.Pointer(&adapter))) != 0 || adapter == nil {
			break // DXGI_ERROR_NOT_FOUND: no more adapters
		}
		var desc dxgiAdapterDesc
		if comCall(adapter, 8, uintptr(unsafe.Pointer(&desc))) == 0 {
			var v Vendor
			switch desc.VendorId {
			case vendorNVIDIA:
				v = VendorNVENC
			case vendorAMD:
				v = VendorAMD
			case vendorIntel:
				v = VendorIntel
			}
			if v != "" {
				out = append(out, adapterInfo{
					vendor: v,
					luid:   luidKey(uint32(desc.AdapterLuid.HighPart), desc.AdapterLuid.LowPart),
				})
			}
		}
		comCall(adapter, 2) // Release adapter
	}
	return out, true
}

// detectGPUVendors reports which encoder vendors are physically installed.
// Returns nil if DXGI is unavailable so callers fail open.
func detectGPUVendors() map[Vendor]bool {
	adapters, ok := enumAdapters()
	if !ok {
		return nil
	}
	present := map[Vendor]bool{}
	for _, a := range adapters {
		present[a.vendor] = true
	}
	return present
}

// buildLuidVendor maps each adapter LUID to its vendor for counter attribution.
func buildLuidVendor() map[string]Vendor {
	adapters, ok := enumAdapters()
	if !ok {
		return nil
	}
	m := make(map[string]Vendor, len(adapters))
	for _, a := range adapters {
		m[a.luid] = a.vendor
	}
	return m
}

// --- CPU via GetSystemTimes ------------------------------------------------

var (
	modkernel32        = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes = modkernel32.NewProc("GetSystemTimes")
)

func ftToU64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func (s *sampleState) cpuPct() float64 {
	var idle, kernel, user windows.Filetime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return 0
	}
	i, k, u := ftToU64(idle), ftToU64(kernel), ftToU64(user)
	defer func() { s.prevIdle, s.prevKernel, s.prevUser, s.havePrevCPU = i, k, u, true }()
	if !s.havePrevCPU {
		return 0
	}
	// Kernel time includes idle time; busy = (kernel+user) - idle.
	total := (k - s.prevKernel) + (u - s.prevUser)
	if total == 0 {
		return 0
	}
	idleDelta := i - s.prevIdle
	busy := float64(total-idleDelta) / float64(total)
	return busy * 100
}

// --- Per-vendor GPU video-encode via PDH -----------------------------------

var (
	modpdh                        = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW             = modpdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW     = modpdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData       = modpdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCntrArrayW = modpdh.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery             = modpdh.NewProc("PdhCloseQuery")
)

const (
	pdhFmtDouble = 0x00000200
	pdhMoreData  = 0x800007D2
	// All GPU engine instances; we filter to the encode-capable engines in
	// code. Vendors name them differently — NVIDIA/Intel expose "VideoEncode",
	// AMD a unified "VideoCodec" engine — so a fixed engtype wildcard misses
	// some hardware.
	gpuEncodeCounterPath = `\GPU Engine(*)\Utilization Percentage`
)

// isEncodeEngine reports whether a GPU engine type does video encoding. Matches
// NVIDIA/Intel "VideoEncode" and AMD's combined "VideoCodec" engine; excludes
// decode, 3D, copy, etc.
func isEncodeEngine(engtype string) bool {
	e := strings.ToLower(engtype)
	return strings.Contains(e, "encode") || strings.Contains(e, "codec")
}

// pdhFmtCounterValue mirrors PDH_FMT_COUNTERVALUE (union sized to 8 bytes).
type pdhFmtCounterValue struct {
	CStatus     uint32
	_           uint32 // padding to 8-byte align the value
	DoubleValue float64
}

// pdhFmtCounterValueItemW mirrors PDH_FMT_COUNTERVALUE_ITEM_W.
type pdhFmtCounterValueItemW struct {
	SzName   *uint16
	FmtValue pdhFmtCounterValue
}

func pdhCloseQuery(q uintptr) { procPdhCloseQuery.Call(q) }

func (s *sampleState) openPDH() {
	var query uintptr
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&query))); r != 0 {
		return
	}
	path, err := windows.UTF16PtrFromString(gpuEncodeCounterPath)
	if err != nil {
		pdhCloseQuery(query)
		return
	}
	var counter uintptr
	if r, _, _ := procPdhAddEnglishCounterW.Call(
		query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter)),
	); r != 0 {
		pdhCloseQuery(query)
		return
	}
	// Prime: utilization needs two collections to yield a value.
	procPdhCollectQueryData.Call(query)
	s.pdhQuery, s.pdhCounter, s.pdhOK = query, counter, true
}

// parseGPUInstance extracts the adapter LUID key, engine index and engine type
// from a GPU Engine counter instance name, e.g.
// "pid_9552_luid_0x00000000_0x0000C4E7_phys_0_eng_3_engtype_VideoEncode".
func parseGPUInstance(name string) (luid, eng, engtype string, ok bool) {
	parts := strings.Split(name, "_")
	var high, low string
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "luid":
			if i+2 < len(parts) {
				high, low = parts[i+1], parts[i+2]
			}
		case "eng":
			if i+1 < len(parts) {
				eng = parts[i+1]
			}
		case "engtype":
			if i+1 < len(parts) {
				engtype = strings.Join(parts[i+1:], "_")
			}
		}
	}
	if high == "" || low == "" {
		return "", "", "", false
	}
	return luidKey(hex32(high), hex32(low)), eng, engtype, true
}

func hex32(s string) uint32 {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	v, _ := strconv.ParseUint(s, 16, 64)
	return uint32(v)
}

// gpuEncodeByVendor returns the current video-encode utilization per vendor.
// Instances are attributed to a vendor by adapter LUID, summed per engine node
// (matching what one Task Manager engine graph shows), and the busiest node is
// reported per vendor — summing across nodes would overcount.
func (s *sampleState) gpuEncodeByVendor() map[Vendor]float64 {
	out := map[Vendor]float64{}
	if !s.pdhOK {
		return out
	}
	if r, _, _ := procPdhCollectQueryData.Call(s.pdhQuery); r != 0 {
		return out
	}
	// First call sizes the buffer (expects PDH_MORE_DATA).
	var bufSize, itemCount uint32
	r, _, _ := procPdhGetFormattedCntrArrayW.Call(
		s.pdhCounter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&bufSize)), uintptr(unsafe.Pointer(&itemCount)), 0,
	)
	if r != pdhMoreData || bufSize == 0 || itemCount == 0 {
		return out
	}
	buf := make([]byte, bufSize)
	if r, _, _ = procPdhGetFormattedCntrArrayW.Call(
		s.pdhCounter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&bufSize)), uintptr(unsafe.Pointer(&itemCount)),
		uintptr(unsafe.Pointer(&buf[0])),
	); r != 0 {
		return out
	}
	items := unsafe.Slice((*pdhFmtCounterValueItemW)(unsafe.Pointer(&buf[0])), itemCount)

	// Sum process utilization per (vendor, engine node), keeping only the
	// encode-capable engines.
	nodes := map[Vendor]map[string]float64{}
	for i := range items {
		if items[i].FmtValue.CStatus > 1 { // keep VALID_DATA / NEW_DATA only
			continue
		}
		luid, eng, engtype, ok := parseGPUInstance(windows.UTF16PtrToString(items[i].SzName))
		if !ok || !isEncodeEngine(engtype) {
			continue
		}
		v, found := s.luidVendor[luid]
		if !found {
			continue
		}
		if nodes[v] == nil {
			nodes[v] = map[string]float64{}
		}
		nodes[v][engtype+"#"+eng] += items[i].FmtValue.DoubleValue
	}
	// Report the busiest engine node per vendor.
	for v, byEng := range nodes {
		var maxNode float64
		for _, val := range byEng {
			if val > maxNode {
				maxNode = val
			}
		}
		out[v] = clampPct(maxNode)
	}
	return out
}
