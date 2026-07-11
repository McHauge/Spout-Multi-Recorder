//go:build windows

package hwstat

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sampleState holds per-sampler OS handles and the previous CPU snapshot so
// utilization can be derived from deltas between ticks.
type sampleState struct {
	// CPU delta tracking (100-ns FILETIME ticks).
	prevIdle, prevKernel, prevUser uint64
	havePrevCPU                    bool

	// PDH GPU Engine (video-encode) query, opened once and reused.
	pdhQuery   uintptr
	pdhCounter uintptr
	pdhOK      bool

	// nvidia-smi discovery (looked up lazily, cached).
	nvOnce sync.Once
	nvPath string
}

func newSampleState() *sampleState {
	s := &sampleState{}
	s.openPDH()
	return s
}

func (s *sampleState) close() {
	if s.pdhQuery != 0 {
		pdhCloseQuery(s.pdhQuery)
		s.pdhQuery = 0
	}
}

// sample builds one utilization snapshot. NVENC comes from nvidia-smi; the
// non-NVIDIA GPU video-encode load (Intel/AMD) is the total measured encode
// utilization minus NVENC, reported on whichever of those vendors is present
// (the UI hides absent ones). CPU is the system-wide busy percentage.
func sample(s *sampleState) Load {
	cpu := s.cpuPct()
	nv := s.nvEncoderPct()
	gpuTotal := s.gpuEncodePct()
	other := gpuTotal - nv
	if other < 0 {
		other = 0
	}
	return Load{
		NVENC: clampPct(nv),
		AMD:   clampPct(other),
		Intel: clampPct(other),
		CPU:   clampPct(cpu),
	}
}

// --- GPU presence via DXGI adapter enumeration -----------------------------

var (
	moddxgi               = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory = moddxgi.NewProc("CreateDXGIFactory")
)

// IID_IDXGIFactory {7b7166ec-21c7-44ae-b21a-c9ae321ae369}
var iidIDXGIFactory = windows.GUID{
	Data1: 0x7b7166ec, Data2: 0x21c7, Data3: 0x44ae,
	Data4: [8]byte{0xb2, 0x1a, 0xc9, 0xae, 0x32, 0x1a, 0xe3, 0x69},
}

// dxgiAdapterDesc mirrors DXGI_ADAPTER_DESC; only VendorId is used.
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

// detectGPUVendors enumerates the installed display adapters and reports which
// encoder vendors are physically present. Returns nil if DXGI is unavailable so
// callers fail open. IDXGIFactory::EnumAdapters is vtable slot 7,
// IDXGIAdapter::GetDesc is slot 8, IUnknown::Release is slot 2.
func detectGPUVendors() map[Vendor]bool {
	if err := procCreateDXGIFactory.Find(); err != nil {
		return nil
	}
	var factory unsafe.Pointer
	r, _, _ := procCreateDXGIFactory.Call(
		uintptr(unsafe.Pointer(&iidIDXGIFactory)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if r != 0 || factory == nil {
		return nil
	}
	defer comCall(factory, 2) // Release factory

	present := map[Vendor]bool{}
	for i := 0; ; i++ {
		var adapter unsafe.Pointer
		if comCall(factory, 7, uintptr(uint32(i)), uintptr(unsafe.Pointer(&adapter))) != 0 || adapter == nil {
			break // DXGI_ERROR_NOT_FOUND: no more adapters
		}
		var desc dxgiAdapterDesc
		if comCall(adapter, 8, uintptr(unsafe.Pointer(&desc))) == 0 {
			switch desc.VendorId {
			case vendorNVIDIA:
				present[VendorNVENC] = true
			case vendorAMD:
				present[VendorAMD] = true
			case vendorIntel:
				present[VendorIntel] = true
			}
		}
		comCall(adapter, 2) // Release adapter
	}
	return present
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

// --- NVENC via nvidia-smi --------------------------------------------------

func (s *sampleState) nvEncoderPct() float64 {
	s.nvOnce.Do(func() {
		if p, err := exec.LookPath("nvidia-smi"); err == nil {
			s.nvPath = p
		}
	})
	if s.nvPath == "" {
		return 0
	}
	cmd := exec.Command(s.nvPath,
		"--query-gpu=utilization.encoder", "--format=csv,noheader,nounits")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	// One line per GPU; take the busiest encoder.
	var maxPct float64
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.TrimSpace(line)
		if f == "" {
			continue
		}
		if v, err := strconv.ParseFloat(f, 64); err == nil && v > maxPct {
			maxPct = v
		}
	}
	return maxPct
}

// --- Total GPU video-encode via PDH ---------------------------------------

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
	// Sum utilization across every GPU video-encode engine instance.
	gpuEncodeCounterPath = `\GPU Engine(*engtype_VideoEncode)\Utilization Percentage`
)

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

func (s *sampleState) gpuEncodePct() float64 {
	if !s.pdhOK {
		return 0
	}
	if r, _, _ := procPdhCollectQueryData.Call(s.pdhQuery); r != 0 {
		return 0
	}
	// First call sizes the buffer (expects PDH_MORE_DATA).
	var bufSize, itemCount uint32
	r, _, _ := procPdhGetFormattedCntrArrayW.Call(
		s.pdhCounter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&bufSize)), uintptr(unsafe.Pointer(&itemCount)), 0,
	)
	if r != pdhMoreData || bufSize == 0 || itemCount == 0 {
		return 0
	}
	buf := make([]byte, bufSize)
	r, _, _ = procPdhGetFormattedCntrArrayW.Call(
		s.pdhCounter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&bufSize)), uintptr(unsafe.Pointer(&itemCount)),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if r != 0 {
		return 0
	}
	items := unsafe.Slice((*pdhFmtCounterValueItemW)(unsafe.Pointer(&buf[0])), itemCount)
	var total float64
	for i := range items {
		// CStatus 0 (VALID_DATA) or 1 (NEW_DATA) are usable.
		if items[i].FmtValue.CStatus <= 1 {
			total += items[i].FmtValue.DoubleValue
		}
	}
	return total
}
