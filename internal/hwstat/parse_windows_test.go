//go:build windows

package hwstat

import "testing"

func TestParseGPUInstance(t *testing.T) {
	cases := []struct {
		name        string
		wantLuid    string
		wantEng     string
		wantEngtype string
		wantOK      bool
	}{
		{
			name:        "pid_9552_luid_0x00000000_0x0000C4E7_phys_0_eng_3_engtype_VideoEncode",
			wantLuid:    "00000000_0000c4e7",
			wantEng:     "3",
			wantEngtype: "VideoEncode",
			wantOK:      true,
		},
		{
			name:        "pid_1_luid_0x00000001_0xDEADBEEF_phys_0_eng_0_engtype_VideoCodec",
			wantLuid:    "00000001_deadbeef",
			wantEng:     "0",
			wantEngtype: "VideoCodec",
			wantOK:      true,
		},
		{name: "garbage_instance_name", wantOK: false},
	}
	for _, c := range cases {
		luid, eng, engtype, ok := parseGPUInstance(c.name)
		if ok != c.wantOK {
			t.Errorf("%q: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if luid != c.wantLuid || eng != c.wantEng || engtype != c.wantEngtype {
			t.Errorf("%q: got luid=%q eng=%q engtype=%q, want luid=%q eng=%q engtype=%q",
				c.name, luid, eng, engtype, c.wantLuid, c.wantEng, c.wantEngtype)
		}
	}
}

func TestIsEncodeEngine(t *testing.T) {
	encode := []string{"VideoEncode", "VideoCodec", "videocodec"}
	for _, e := range encode {
		if !isEncodeEngine(e) {
			t.Errorf("isEncodeEngine(%q) = false, want true", e)
		}
	}
	notEncode := []string{"VideoDecode", "3D", "Copy", "VideoProcessing", "Compute_0"}
	for _, e := range notEncode {
		if isEncodeEngine(e) {
			t.Errorf("isEncodeEngine(%q) = true, want false", e)
		}
	}
}

// luidKey must format the same way for DXGI adapter LUIDs and counter instance
// names, or attribution silently fails.
func TestLuidKeyMatchesInstance(t *testing.T) {
	// DXGI side: HighPart=0, LowPart=0xC4E7.
	fromAdapter := luidKey(0, 0xC4E7)
	// Counter side: same LUID parsed out of an instance name.
	fromInstance, _, _, ok := parseGPUInstance("pid_1_luid_0x00000000_0x0000C4E7_phys_0_eng_0_engtype_VideoEncode")
	if !ok || fromAdapter != fromInstance {
		t.Errorf("adapter key %q != instance key %q (ok=%v)", fromAdapter, fromInstance, ok)
	}
}
