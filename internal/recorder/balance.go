package recorder

import (
	"github.com/McHauge/Spout-Multi-Recorder/internal/hwstat"
)

// PlanInput describes one channel to be placed onto an encoder. W/H/FPS drive
// the estimated per-channel encoder cost (0 dims fall back to the baseline).
type PlanInput struct {
	Channel string
	W, H    int
	FPS     int
}

// BalanceCfg tunes how channels are spread across encoders.
type BalanceCfg struct {
	// FillWeight is the projected-load fraction (0..1) an encoder is filled to
	// before channels spill to the next one in Order. Default 0.80.
	FillWeight float64
	// Order is the encoder preference, highest first. CPU should be last and is
	// always usable (no cap). Default NVENC → AMD → Intel → CPU.
	Order []hwstat.Vendor
	// CostBaseline is the estimated encoder load (%) of one 1080p30 channel.
	CostBaseline float64
}

// DefaultBalanceCfg returns the shipping defaults.
func DefaultBalanceCfg() BalanceCfg {
	return BalanceCfg{
		FillWeight:   0.80,
		Order:        []hwstat.Vendor{hwstat.VendorNVENC, hwstat.VendorAMD, hwstat.VendorIntel, hwstat.VendorCPU},
		CostBaseline: 18.0,
	}
}

// baseline1080p30 is the pixel-rate a channel is measured against for cost.
const baseline1080p30 = 1920.0 * 1080.0 * 30.0

// EstimatedCost is one channel's estimated share (%) of a single encoder.
func (cfg BalanceCfg) EstimatedCost(w, h, fps int) float64 {
	base := cfg.CostBaseline
	if base <= 0 {
		base = 18.0
	}
	if w <= 0 || h <= 0 || fps <= 0 {
		return base
	}
	return base * (float64(w) * float64(h) * float64(fps) / baseline1080p30)
}

// Assign places each channel onto an encoder. It walks channels in the given
// order and, for each, picks the highest-preference available encoder whose
// projected load — the live measured load plus the estimated cost of channels
// already assigned to it this run — stays under FillWeight. When every GPU
// encoder is at or above the weight it spills to the next, and finally to CPU
// (which has no cap). The result is a channel→vendor map, fixed for the session.
func Assign(inputs []PlanInput, avail map[hwstat.Vendor]bool, live hwstat.Load, cfg BalanceCfg) map[string]hwstat.Vendor {
	if cfg.FillWeight <= 0 {
		cfg.FillWeight = 0.80
	}
	if len(cfg.Order) == 0 {
		cfg.Order = DefaultBalanceCfg().Order
	}
	threshold := cfg.FillWeight * 100
	accumulated := map[hwstat.Vendor]float64{}
	out := make(map[string]hwstat.Vendor, len(inputs))

	for _, in := range inputs {
		cost := cfg.EstimatedCost(in.W, in.H, in.FPS)
		placed := hwstat.VendorCPU
		for _, v := range cfg.Order {
			if v == hwstat.VendorCPU {
				// CPU is the always-available fallback; take it unconditionally.
				placed = v
				break
			}
			if !avail[v] {
				continue
			}
			projected := live.Of(v) + accumulated[v] + cost
			if projected < threshold {
				placed = v
				break
			}
		}
		accumulated[placed] += cost
		out[in.Channel] = placed
	}
	return out
}
