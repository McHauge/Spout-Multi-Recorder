package ui

import (
	"image/color"
	"math"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/widget"
)

// MultiVU is a compact vertical peak meter with one bar per audio channel,
// shown beside each preview. Hidden when the channel records no audio.
type MultiVU struct {
	widget.BaseWidget
	levels []float64 // 0..1 linear, one per channel
	peaks  []float64
}

// NewMultiVU creates the meter (hidden until levels arrive).
func NewMultiVU() *MultiVU {
	m := &MultiVU{}
	m.ExtendBaseWidget(m)
	m.Hide()
	return m
}

// SetLevels updates the meter with linear peak levels (one per channel).
// nil/empty hides the meter. Must be called on the Fyne thread.
func (m *MultiVU) SetLevels(levels []float64) {
	if len(levels) == 0 {
		if m.Visible() {
			m.Hide()
		}
		return
	}
	if len(m.levels) != len(levels) {
		m.levels = make([]float64, len(levels))
		m.peaks = make([]float64, len(levels))
	}
	copy(m.levels, levels)
	for i, l := range levels {
		m.peaks[i] = math.Max(l, m.peaks[i]*0.94)
	}
	if !m.Visible() {
		m.Show()
	}
	m.Refresh()
}

// CreateRenderer implements fyne.Widget.
func (m *MultiVU) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.NRGBA{R: 0x20, G: 0x20, B: 0x20, A: 0xff})
	return &multiVURenderer{m: m, bg: bg}
}

type multiVURenderer struct {
	m     *MultiVU
	bg    *canvas.Rectangle
	bars  []*canvas.Rectangle
	holds []*canvas.Rectangle
}

func (r *multiVURenderer) ensure(n int) {
	for len(r.bars) < n {
		r.bars = append(r.bars, canvas.NewRectangle(levelColor(0)))
		r.holds = append(r.holds, canvas.NewRectangle(color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xc0}))
	}
	if len(r.bars) > n {
		r.bars = r.bars[:n]
		r.holds = r.holds[:n]
	}
}

func (r *multiVURenderer) MinSize() fyne.Size {
	// Double width when there are many bars (8/16 channels).
	if len(r.m.levels) > 4 {
		return fyne.NewSize(52, 60)
	}
	return fyne.NewSize(26, 60)
}

func (r *multiVURenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	r.bg.Move(fyne.NewPos(0, 0))
	r.place(size)
}

func (r *multiVURenderer) place(size fyne.Size) {
	n := len(r.m.levels)
	r.ensure(n)
	if n == 0 {
		return
	}
	gap := float32(1)
	barW := (size.Width - gap*float32(n+1)) / float32(n)
	if barW < 1 {
		barW = 1
	}
	h := size.Height - gap*2
	for i := 0; i < n; i++ {
		x := gap + float32(i)*(barW+gap)
		pos := float32(scale(r.m.levels[i]))
		bh := h * pos
		r.bars[i].FillColor = levelColor(scale(r.m.levels[i]))
		r.bars[i].Move(fyne.NewPos(x, gap+h-bh))
		r.bars[i].Resize(fyne.NewSize(barW, bh))

		hp := float32(scale(r.m.peaks[i]))
		r.holds[i].Move(fyne.NewPos(x, gap+h-h*hp-1))
		r.holds[i].Resize(fyne.NewSize(barW, 2))
	}
}

func (r *multiVURenderer) Refresh() {
	r.place(r.m.Size())
	canvas.Refresh(r.m)
}

func (r *multiVURenderer) Objects() []fyne.CanvasObject {
	out := make([]fyne.CanvasObject, 0, 1+len(r.bars)*2)
	out = append(out, r.bg)
	for i := range r.bars {
		out = append(out, r.bars[i], r.holds[i])
	}
	return out
}

func (r *multiVURenderer) Destroy() {}
