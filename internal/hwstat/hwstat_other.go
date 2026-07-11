//go:build !windows

package hwstat

// sampleState is unused off Windows.
type sampleState struct{}

func newSampleState() *sampleState { return &sampleState{} }
func (s *sampleState) close()      {}

// sample reports no utilization; real measurement is Windows-only.
func sample(_ *sampleState) Load { return Load{} }

// detectGPUVendors is unavailable off Windows; nil means "unknown" so callers
// fail open and don't hide encoders.
func detectGPUVendors() map[Vendor]bool { return nil }
