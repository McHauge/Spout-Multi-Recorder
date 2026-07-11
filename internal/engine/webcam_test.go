package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/McHauge/Spout-Multi-Recorder/internal/mfcap"
)

// TestWebcamChannel drives a real webcam through the engine and confirms frames
// reach the channel's frame buffer and the channel reports online. Skipped when
// no physical webcam is attached.
func TestWebcamChannel(t *testing.T) {
	if err := mfcap.Available(); err != nil {
		t.Skipf("Media Foundation unavailable: %v", err)
	}
	devs, err := mfcap.Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	// Prefer a physical device; NDI virtual cameras enumerate but rarely feed.
	var dev *mfcap.Device
	for i := range devs {
		if !strings.HasPrefix(devs[i].Name, "NDI ") {
			dev = &devs[i]
			break
		}
	}
	if dev == nil {
		t.Skip("no physical webcam attached")
	}
	t.Logf("using webcam %q", dev.Name)

	e := New(8, nil)
	defer e.Close()
	e.AddWebcam(dev.Name, dev.Link, "", false, mfcap.Mode{}, true)

	var ch *Channel
	for _, c := range e.Channels() {
		if c.Kind == KindWebcam {
			ch = c
		}
	}
	if ch == nil {
		t.Fatal("webcam channel not created")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w, h, _, connected := ch.Buf.Dims(); connected && w > 0 && h > 0 {
			t.Logf("online: %dx%d, seq=%d", w, h, ch.Buf.Seq())
			if !ch.Online() {
				t.Fatal("frames flowing but Online() is false")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("webcam channel never went online within 5s")
}
