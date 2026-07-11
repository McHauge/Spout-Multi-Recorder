package mfcap

import (
	"strings"
	"testing"
	"time"
)

// TestEnumerateAndCapture lists webcams and, if any are present, opens the
// first and confirms frames arrive. It is skipped when no camera is attached.
func TestEnumerateAndCapture(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("Media Foundation unavailable: %v", err)
	}
	devs, err := Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	t.Logf("found %d webcam(s):", len(devs))
	for i, d := range devs {
		t.Logf("  [%d] %q link=%q", i, d.Name, d.Link)
	}
	if len(devs) == 0 {
		t.Skip("no webcam attached")
	}

	// Try each device; NDI virtual cameras with no upstream source enumerate
	// but never deliver frames. Pass if at least one real device delivers.
	delivered := 0
	for _, d := range devs {
		frames, w, h := grab(t, d, 2*time.Second)
		t.Logf("%-28q -> %d frames at %dx%d", d.Name, frames, w, h)
		if frames > 0 {
			delivered++
		}
	}
	if delivered == 0 {
		t.Fatalf("no device delivered any frames")
	}
}

// TestModes enumerates a physical webcam's modes and checks that opening with a
// target FPS picks a resolution that can sustain it.
func TestModes(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("Media Foundation unavailable: %v", err)
	}
	devs, _ := Devices()
	var dev *Device
	for i := range devs {
		if !strings.HasPrefix(devs[i].Name, "NDI ") {
			dev = &devs[i]
			break
		}
	}
	if dev == nil {
		t.Skip("no physical webcam attached")
	}

	modes, err := Modes(dev.Link)
	if err != nil {
		t.Fatalf("Modes: %v", err)
	}
	t.Logf("%q offers %d modes:", dev.Name, len(modes))
	best30W, best30H := 0, 0
	for _, m := range modes {
		t.Logf("  %dx%d @ %.2f fps", m.W, m.H, m.FPS())
		if m.FPSx1000+500 >= 30000 && m.W*m.H > best30W*best30H {
			best30W, best30H = m.W, m.H
		}
	}
	if len(modes) == 0 {
		t.Fatal("no modes reported")
	}

	// Open targeting 30fps and confirm the negotiated mode reaches it.
	c, err := Open(dev.Link, Mode{FPSx1000: 30000})
	if err != nil {
		t.Fatalf("Open(30fps): %v", err)
	}
	defer c.Close()
	deadline := time.Now().Add(2 * time.Second)
	var w, h int
	for time.Now().Before(deadline) {
		f := c.Latest()
		if f.NewFrame {
			w, h = f.Width, f.Height
			break
		}
		time.Sleep(time.Second / 60)
	}
	t.Logf("target 30fps → opened %dx%d @ %.2f fps (highest-30fps mode is %dx%d)",
		w, h, float64(c.FPSx1000())/1000, best30W, best30H)
	if best30W > 0 && c.FPSx1000()+500 < 30000 {
		t.Fatalf("targeted 30fps but negotiated %.2f", float64(c.FPSx1000())/1000)
	}
}

func grab(t *testing.T, d Device, dur time.Duration) (frames, w, h int) {
	cap, err := Open(d.Link, Mode{})
	if err != nil {
		t.Logf("Open(%q): %v", d.Name, err)
		return 0, 0, 0
	}
	defer cap.Close()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		f := cap.Latest()
		if f.Lost {
			break
		}
		if f.NewFrame {
			frames++
			w, h = f.Width, f.Height
			if len(f.Pixels) != w*h*4 {
				t.Fatalf("frame %d: pixel len %d != %d", frames, len(f.Pixels), w*h*4)
			}
		}
		time.Sleep(time.Second / 60)
	}
	return frames, w, h
}
