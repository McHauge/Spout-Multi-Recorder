// icongen renders the application icon (internal/assets/icon.png).
// Design: three cascading video frames (the multi-stream grid) with a
// record dot, on a dark rounded tile.
//
// Regenerate with:  go run ./tools/icongen
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"os"

	xdraw "golang.org/x/image/draw"
)

const (
	out   = "internal/assets/icon.png"
	size  = 256
	scale = 4 // supersampling factor for antialiasing
	s     = size * scale
)

type rrect struct {
	x0, y0, x1, y1, r float64
	col               color.NRGBA
}

type circle struct {
	cx, cy, r float64
	col       color.NRGBA
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (rr rrect) hit(x, y float64) bool {
	cx := clamp(x, rr.x0+rr.r, rr.x1-rr.r)
	cy := clamp(y, rr.y0+rr.r, rr.y1-rr.r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= rr.r*rr.r
}

func (c circle) hit(x, y float64) bool {
	dx, dy := x-c.cx, y-c.cy
	return dx*dx+dy*dy <= c.r*c.r
}

func main() {
	// Geometry in 256-space, scaled up when rendering.
	bg := rrect{6, 6, 250, 250, 58, color.NRGBA{0x14, 0x1a, 0x26, 0xff}}
	frames := []rrect{
		{92, 40, 226, 132, 14, color.NRGBA{0x36, 0x44, 0x62, 0xff}},  // back
		{62, 76, 202, 172, 14, color.NRGBA{0x4a, 0x64, 0x94, 0xff}},  // middle
		{32, 112, 178, 214, 14, color.NRGBA{0x6d, 0x9c, 0xe8, 0xff}}, // front
	}
	ringCol := color.NRGBA{0xf5, 0xf7, 0xfa, 0xff}
	dotCol := color.NRGBA{0xe5, 0x48, 0x4d, 0xff}
	ring := circle{196, 186, 42, ringCol}
	dot := circle{196, 186, 30, dotCol}

	big := image.NewNRGBA(image.Rect(0, 0, s, s))
	for py := 0; py < s; py++ {
		for px := 0; px < s; px++ {
			x := (float64(px) + 0.5) / scale
			y := (float64(py) + 0.5) / scale
			var c color.NRGBA
			switch {
			case dot.hit(x, y):
				c = dot.col
			case ring.hit(x, y):
				c = ring.col
			default:
				c = color.NRGBA{} // transparent
				if bg.hit(x, y) {
					c = bg.col
					for _, f := range frames {
						if f.hit(x, y) {
							c = f.col
						}
					}
				}
			}
			big.SetNRGBA(px, py, c)
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(img, img.Bounds(), big, big.Bounds(), xdraw.Over, nil)

	f, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%dx%d)", out, size, size)
}
