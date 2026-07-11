package ui

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

// pathStatus is a compact, clickable footer indicator (e.g. "ffmpeg loaded").
// Hovering reveals the full path inline; clicking copies it to the clipboard.
// The reveal is inline (the label swaps its own text) rather than a floating
// popup, so nothing ever covers the cursor and there is no hover flicker.
type pathStatus struct {
	widget.BaseWidget
	label    *widget.Label
	base     string // resting text
	full     string // full path (revealed on hover, copied on click)
	clip     fyne.Clipboard
	align    fyne.TextAlign
	truncate bool // ellipsis-clip long text (for width-constrained placements)
	hovering bool
	flashing bool // showing the transient "copied" confirmation
}

func newPathStatus(clip fyne.Clipboard) *pathStatus {
	p := &pathStatus{clip: clip, align: fyne.TextAlignCenter}
	p.ExtendBaseWidget(p)
	return p
}

// setAlign overrides the label alignment. Call before the widget is rendered.
func (p *pathStatus) setAlign(a fyne.TextAlign) { p.align = a }

// setTruncate enables ellipsis clipping so long text doesn't force the window
// wider. Only needed in width-constrained placements (e.g. a border centre).
// Call before the widget is rendered.
func (p *pathStatus) setTruncate(t bool) { p.truncate = t }

// set updates the resting text and the full path revealed on hover/click.
func (p *pathStatus) set(text, full string) {
	p.base, p.full = text, full
	p.render()
}

func (p *pathStatus) CreateRenderer() fyne.WidgetRenderer {
	p.label = widget.NewLabel(p.base)
	p.label.Alignment = p.align
	if p.truncate {
		p.label.Truncation = fyne.TextTruncateEllipsis
	}
	return widget.NewSimpleRenderer(p.label)
}

// render sets the label text for the current state.
func (p *pathStatus) render() {
	if p.label == nil || p.flashing {
		return
	}
	if p.hovering && p.full != "" {
		p.label.SetText(p.full)
	} else {
		p.label.SetText(p.base)
	}
}

// Tapped copies the path and briefly confirms.
func (p *pathStatus) Tapped(_ *fyne.PointEvent) {
	if p.full == "" || p.clip == nil {
		return
	}
	p.clip.SetContent(p.full)
	p.flashing = true
	p.label.SetText("✓ path copied")
	go func() {
		time.Sleep(1500 * time.Millisecond)
		fyne.Do(func() {
			p.flashing = false
			p.render()
		})
	}()
}

// Cursor shows a pointer to signal the indicator is clickable.
func (p *pathStatus) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (p *pathStatus) MouseIn(*desktop.MouseEvent) {
	p.hovering = true
	p.render()
}

func (p *pathStatus) MouseMoved(*desktop.MouseEvent) {}

func (p *pathStatus) MouseOut() {
	p.hovering = false
	p.render()
}
