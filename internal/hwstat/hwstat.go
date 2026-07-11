// Package hwstat samples the current video-encode utilization of each hardware
// encoder vendor (NVIDIA/AMD/Intel) plus overall CPU load, so the recorder can
// spread channels across encoders and the UI can show live per-vendor usage.
//
// All numbers are a percentage in [0,100]. Real measurement is Windows-only
// (see hwstat_windows.go); on other platforms the sampler reports zeros. Where
// a source is unavailable (e.g. no NVIDIA GPU), that vendor's value stays 0 and
// callers blend in an estimate derived from the assigned channel count.
package hwstat

import (
	"sync"
	"time"
)

// Vendor identifies a hardware-encoder backend (or the CPU/software encoder).
type Vendor string

const (
	VendorNVENC Vendor = "nvenc" // NVIDIA NVENC
	VendorAMD   Vendor = "amd"   // AMD AMF / VCN
	VendorIntel Vendor = "intel" // Intel Quick Sync (QSV)
	VendorCPU   Vendor = "cpu"   // software encode (libx264/x265/svt-av1)
)

// Label returns a short human-facing name for the footer/badge.
func (v Vendor) Label() string {
	switch v {
	case VendorNVENC:
		return "NVENC"
	case VendorAMD:
		return "AMD"
	case VendorIntel:
		return "Intel"
	case VendorCPU:
		return "CPU"
	}
	return string(v)
}

// Load is a single utilization snapshot, one percentage per vendor.
type Load struct {
	NVENC float64
	AMD   float64
	Intel float64
	CPU   float64
}

// Of returns the load for one vendor.
func (l Load) Of(v Vendor) float64 {
	switch v {
	case VendorNVENC:
		return l.NVENC
	case VendorAMD:
		return l.AMD
	case VendorIntel:
		return l.Intel
	case VendorCPU:
		return l.CPU
	}
	return 0
}

// Sampler polls hardware-encode utilization on a background ticker and caches
// the latest snapshot for cheap concurrent reads.
type Sampler struct {
	mu     sync.RWMutex
	last   Load
	stopCh chan struct{}
	done   chan struct{}
	state  *sampleState // platform sampler state (nil until Start)
}

// New creates an unstarted sampler.
func New() *Sampler { return &Sampler{} }

var (
	gpuOnce    sync.Once
	gpuPresent map[Vendor]bool // nil = detection unavailable (fail-open)
)

// PresentGPU reports whether a physical GPU from the given vendor is installed.
// It enumerates the real display adapters once and caches the result. When
// detection is unavailable (non-Windows, or the query failed) it returns true
// so callers don't wrongly hide an encoder. Only NVENC/AMD/Intel are checked;
// any other vendor (including CPU) returns true.
func PresentGPU(v Vendor) bool {
	switch v {
	case VendorNVENC, VendorAMD, VendorIntel:
	default:
		return true
	}
	gpuOnce.Do(func() { gpuPresent = detectGPUVendors() })
	if gpuPresent == nil {
		return true
	}
	return gpuPresent[v]
}

// Start begins background sampling at ~1 Hz. Safe to call once; further calls
// are no-ops while running.
func (s *Sampler) Start() {
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return
	}
	s.stopCh = make(chan struct{})
	s.done = make(chan struct{})
	s.state = newSampleState()
	stop := s.stopCh
	done := s.done
	s.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		// Prime immediately so the first read isn't a full tick late.
		s.set(sample(s.state))
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.set(sample(s.state))
			}
		}
	}()
}

// Stop ends background sampling and releases platform resources.
func (s *Sampler) Stop() {
	s.mu.Lock()
	stop, done, st := s.stopCh, s.done, s.state
	s.stopCh, s.done, s.state = nil, nil, nil
	s.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
	if st != nil {
		st.close()
	}
}

// Load returns the most recent snapshot (zero value before the first sample).
func (s *Sampler) Load() Load {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

func (s *Sampler) set(l Load) {
	s.mu.Lock()
	s.last = l
	s.mu.Unlock()
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
