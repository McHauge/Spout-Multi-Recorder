package ui

import (
	"image/color"
	"math"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/widget"
)

// VUMeter is a stereo horizontal peak meter with peak-hold markers.
type VUMeter struct {
	widget.BaseWidget
	levelL, levelR float64 // 0..1 linear
	peakL, peakR   float64
}

// NewVUMeter creates the meter.
func NewVUMeter() *VUMeter {
	v := &VUMeter{}
	v.ExtendBaseWidget(v)
	return v
}

// SetLevels updates the meter with linear peak levels (0..1).
// Must be called on the Fyne thread (via fyne.Do).
func (v *VUMeter) SetLevels(l, r float64) {
	// decay peak-hold slowly
	v.peakL = math.Max(l, v.peakL*0.96)
	v.peakR = math.Max(r, v.peakR*0.96)
	v.levelL = l
	v.levelR = r
	v.Refresh()
}

// scale converts a linear level to meter position (dB scale, -60..0).
func scale(lin float64) float64 {
	if lin <= 0.001 {
		return 0
	}
	db := 20 * math.Log10(lin)
	if db < -60 {
		return 0
	}
	return (db + 60) / 60
}

func levelColor(pos float64) color.Color {
	switch {
	case pos > 0.92: // > ~ -5 dB
		return color.NRGBA{R: 0xe5, G: 0x39, B: 0x35, A: 0xff}
	case pos > 0.75: // > ~ -15 dB
		return color.NRGBA{R: 0xfb, G: 0xc0, B: 0x2d, A: 0xff}
	default:
		return color.NRGBA{R: 0x43, G: 0xa0, B: 0x47, A: 0xff}
	}
}

// CreateRenderer implements fyne.Widget.
func (v *VUMeter) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.NRGBA{R: 0x20, G: 0x20, B: 0x20, A: 0xff})
	barL := canvas.NewRectangle(levelColor(0))
	barR := canvas.NewRectangle(levelColor(0))
	holdL := canvas.NewRectangle(color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xc0})
	holdR := canvas.NewRectangle(color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xc0})
	return &vuRenderer{v: v, bg: bg, barL: barL, barR: barR, holdL: holdL, holdR: holdR}
}

type vuRenderer struct {
	v            *VUMeter
	bg           *canvas.Rectangle
	barL, barR   *canvas.Rectangle
	holdL, holdR *canvas.Rectangle
}

func (r *vuRenderer) MinSize() fyne.Size { return fyne.NewSize(120, 22) }

func (r *vuRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	r.bg.Move(fyne.NewPos(0, 0))
	r.place(size)
}

func (r *vuRenderer) place(size fyne.Size) {
	gap := float32(2)
	barH := (size.Height - gap*3) / 2
	w := size.Width - gap*2

	posL := float32(scale(r.v.levelL))
	posR := float32(scale(r.v.levelR))
	r.barL.FillColor = levelColor(scale(r.v.levelL))
	r.barR.FillColor = levelColor(scale(r.v.levelR))
	r.barL.Move(fyne.NewPos(gap, gap))
	r.barL.Resize(fyne.NewSize(w*posL, barH))
	r.barR.Move(fyne.NewPos(gap, gap*2+barH))
	r.barR.Resize(fyne.NewSize(w*posR, barH))

	hL := float32(scale(r.v.peakL))
	hR := float32(scale(r.v.peakR))
	r.holdL.Move(fyne.NewPos(gap+w*hL-1, gap))
	r.holdL.Resize(fyne.NewSize(2, barH))
	r.holdR.Move(fyne.NewPos(gap+w*hR-1, gap*2+barH))
	r.holdR.Resize(fyne.NewSize(2, barH))
}

func (r *vuRenderer) Refresh() {
	r.place(r.v.Size())
	canvas.Refresh(r.v)
}

func (r *vuRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.bg, r.barL, r.barR, r.holdL, r.holdR}
}

func (r *vuRenderer) Destroy() {}
