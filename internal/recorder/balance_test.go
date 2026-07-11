package recorder

import (
	"testing"

	"github.com/McHauge/Spout-Multi-Recorder/internal/hwstat"
)

// inputs makes n identical 1080p30 channels named ch0..ch(n-1).
func inputs(n int) []PlanInput {
	out := make([]PlanInput, n)
	for i := range out {
		out[i] = PlanInput{Channel: "ch" + string(rune('0'+i)), W: 1920, H: 1080, FPS: 30}
	}
	return out
}

func countByVendor(m map[string]hwstat.Vendor) map[hwstat.Vendor]int {
	c := map[hwstat.Vendor]int{}
	for _, v := range m {
		c[v]++
	}
	return c
}

func TestAssignNVENCOnly(t *testing.T) {
	// Only NVENC available: with cost 18 and weight 80%, four channels fit
	// (18,36,54,72 all < 80); the fifth (90) spills to CPU.
	avail := map[hwstat.Vendor]bool{hwstat.VendorNVENC: true, hwstat.VendorCPU: true}
	got := Assign(inputs(5), avail, hwstat.Load{}, DefaultBalanceCfg())
	by := countByVendor(got)
	if by[hwstat.VendorNVENC] != 4 {
		t.Errorf("NVENC = %d, want 4", by[hwstat.VendorNVENC])
	}
	if by[hwstat.VendorCPU] != 1 {
		t.Errorf("CPU = %d, want 1", by[hwstat.VendorCPU])
	}
}

func TestAssignSpillsToAMD(t *testing.T) {
	// NVENC + AMD available: first four fill NVENC, the next spill to AMD.
	avail := map[hwstat.Vendor]bool{hwstat.VendorNVENC: true, hwstat.VendorAMD: true, hwstat.VendorCPU: true}
	got := Assign(inputs(6), avail, hwstat.Load{}, DefaultBalanceCfg())
	by := countByVendor(got)
	if by[hwstat.VendorNVENC] != 4 {
		t.Errorf("NVENC = %d, want 4", by[hwstat.VendorNVENC])
	}
	if by[hwstat.VendorAMD] != 2 {
		t.Errorf("AMD = %d, want 2", by[hwstat.VendorAMD])
	}
	if by[hwstat.VendorCPU] != 0 {
		t.Errorf("CPU = %d, want 0", by[hwstat.VendorCPU])
	}
}

func TestAssignNoGPUAllCPU(t *testing.T) {
	avail := map[hwstat.Vendor]bool{hwstat.VendorCPU: true}
	got := Assign(inputs(3), avail, hwstat.Load{}, DefaultBalanceCfg())
	if by := countByVendor(got); by[hwstat.VendorCPU] != 3 {
		t.Errorf("CPU = %d, want 3", by[hwstat.VendorCPU])
	}
}

func TestAssignLivePreload(t *testing.T) {
	// NVENC already 72% busy: the first 1080p30 channel (cost 18 → 90) exceeds
	// the 80% weight, so it spills straight to AMD.
	avail := map[hwstat.Vendor]bool{hwstat.VendorNVENC: true, hwstat.VendorAMD: true, hwstat.VendorCPU: true}
	got := Assign(inputs(1), avail, hwstat.Load{NVENC: 72}, DefaultBalanceCfg())
	if got["ch0"] != hwstat.VendorAMD {
		t.Errorf("ch0 = %s, want amd", got["ch0"])
	}
}

func TestEstimatedCostScales(t *testing.T) {
	cfg := DefaultBalanceCfg()
	base := cfg.EstimatedCost(1920, 1080, 30)
	if base < 17.9 || base > 18.1 {
		t.Errorf("1080p30 cost = %.2f, want ~18", base)
	}
	// 4K30 is 4× the pixels → ~4× the cost.
	uhd := cfg.EstimatedCost(3840, 2160, 30)
	if uhd < 71 || uhd > 73 {
		t.Errorf("4K30 cost = %.2f, want ~72", uhd)
	}
	// Zero dims fall back to the baseline.
	if z := cfg.EstimatedCost(0, 0, 0); z != cfg.CostBaseline {
		t.Errorf("zero-dim cost = %.2f, want %.2f", z, cfg.CostBaseline)
	}
}
