package frame_test

import (
	"testing"

	"github.com/McHauge/Spout-Multi-Recorder/internal/frame"
)

func TestFit(t *testing.T) {
	var b frame.Buffer
	// 4x4 source, all pixels 0xAA
	src := make([]byte, 4*4*4)
	for i := range src {
		src[i] = 0xAA
	}
	b.Store(src, 4, 4, 87, true)

	// same size
	dst := make([]byte, 4*4*4)
	if !b.Snapshot(dst, 4, 4) {
		t.Fatal("snapshot failed")
	}
	if dst[0] != 0xAA {
		t.Fatal("copy wrong")
	}

	// smaller source centered into 8x8: corners stay 0, center is 0xAA
	dst8 := make([]byte, 8*8*4)
	if !b.Snapshot(dst8, 8, 8) {
		t.Fatal("snapshot 8x8 failed")
	}
	if dst8[0] != 0 {
		t.Fatal("border should be untouched")
	}
	c := (8*4 + 4) * 4 // pixel (4,4) is inside the centered 4x4 area (rows 2-5, cols 2-5)
	if dst8[c] != 0xAA {
		t.Fatalf("center should be copied, got %x", dst8[c])
	}
	// crop: 4x4 into 2x2
	dst2 := make([]byte, 2*2*4)
	if !b.Snapshot(dst2, 2, 2) || dst2[0] != 0xAA {
		t.Fatal("crop failed")
	}
	// disconnected
	b.SetConnected(false)
	if b.Snapshot(dst, 4, 4) {
		t.Fatal("should report disconnected")
	}
	// preview
	b.SetConnected(true)
	img := b.Preview(2)
	if img == nil || img.Bounds().Dx() != 2 || img.Bounds().Dy() != 2 {
		t.Fatal("preview size wrong")
	}
	if img.Pix[3] != 255 {
		t.Fatal("preview alpha")
	}
}
