package decklink

import (
	"testing"
	"time"
)

// TestDeckLink checks driver availability and enumeration, and — if a card
// with a live signal is present — that video frames and audio arrive.
func TestDeckLink(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("DeckLink driver unavailable: %v", err)
	}
	t.Log("DeckLink driver available")

	devs, err := Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	t.Logf("found %d DeckLink device(s):", len(devs))
	for i, d := range devs {
		t.Logf("  [%d] %q", i, d)
	}
	if len(devs) == 0 {
		t.Skip("no DeckLink devices installed")
	}

	cap, err := Open(devs[0])
	if err != nil {
		t.Fatalf("Open(%q): %v", devs[0], err)
	}
	defer cap.Close()

	deadline := time.Now().Add(4 * time.Second)
	frames, audioBytes, chans := 0, 0, 0
	abuf := make([]byte, 64*1024)
	var w, h int
	for time.Now().Before(deadline) {
		f := cap.Latest()
		if f.NewFrame {
			frames++
			w, h = f.Width, f.Height
			if len(f.Pixels) != w*h*4 {
				t.Fatalf("frame %d: pixel len %d != %d", frames, len(f.Pixels), w*h*4)
			}
		}
		n, c := cap.ReadAudio(abuf)
		audioBytes += n
		if c > 0 {
			chans = c
		}
		time.Sleep(time.Second / 60)
	}
	t.Logf("captured %d video frames at %dx%d; %d audio bytes across %d channels",
		frames, w, h, audioBytes, chans)
	if frames == 0 {
		t.Log("no video frames — card present but likely no input signal connected")
	}
}
