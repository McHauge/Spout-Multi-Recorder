// Package ui implements the Fyne desktop interface: a grid of live previews,
// a master VU meter and the recording controls.
package ui

import (
	"encoding/json"
	"fmt"
	"image"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/McHauge/Spout-Multi-Recorder/internal/assets"
	"github.com/McHauge/Spout-Multi-Recorder/internal/audio"
	"github.com/McHauge/Spout-Multi-Recorder/internal/engine"
	"github.com/McHauge/Spout-Multi-Recorder/internal/recorder"
)

// Config is persisted between runs.
type Config struct {
	AudioDevice   string `json:"audio_device"`
	AudioLoopback bool   `json:"audio_loopback"`
	Codec         string `json:"codec"`
	FPS           int    `json:"fps"`
	OutDir        string `json:"out_dir"`
	MaxChannels   int    `json:"max_channels"`
	AutoRecord    bool   `json:"auto_record"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	dir = filepath.Join(dir, "SpoutMultiRecorder")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "config.json")
}

// LoadConfig reads the saved configuration (with defaults).
func LoadConfig() Config {
	cfg := Config{Codec: "h264", FPS: 30, MaxChannels: 8}
	home, _ := os.UserHomeDir()
	cfg.OutDir = filepath.Join(home, "Videos", "SpoutRecordings")
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}
	if cfg.MaxChannels <= 0 {
		cfg.MaxChannels = 8
	}
	return cfg
}

func (c Config) save() {
	if b, err := json.MarshalIndent(c, "", "  "); err == nil {
		_ = os.WriteFile(configPath(), b, 0o644)
	}
}

// App wires the engine, audio and widgets together.
type App struct {
	fapp fyne.App
	win  fyne.Window
	eng  *engine.Engine
	aud  *audio.Engine
	cfg  Config

	ffmpegPath string
	ffmpegErr  error

	vu        *VUMeter
	recordBtn *widget.Button
	elapsed   *widget.Label
	statusBar *widget.Label
	grid      *fyne.Container
	scroll    *container.Scroll
	emptyBox  fyne.CanvasObject

	audioSel  *widget.Select
	codecSel  *widget.Select
	fpsSel    *widget.Select
	maxChSel  *widget.Select
	autoChk   *widget.Check
	outEntry  *widget.Entry
	browseBtn *widget.Button

	devices  []audio.Device
	cards    map[string]*channelCard
	recStart time.Time
	stopping bool
}

type channelCard struct {
	ch      *engine.Channel
	img     *canvas.Image
	check   *widget.Check
	status  *widget.Label
	obj     fyne.CanvasObject
	lastSeq uint64
}

// Run builds the window and blocks until the app exits.
func Run(eng *engine.Engine, aud *audio.Engine) {
	a := &App{
		fapp:  app.NewWithID("com.mchauge.spoutmultirecorder"),
		eng:   eng,
		aud:   aud,
		cfg:   LoadConfig(),
		cards: map[string]*channelCard{},
	}
	a.fapp.SetIcon(assets.Icon)
	eng.SetMaxChannels(a.cfg.MaxChannels)
	eng.SetAutoRecord(a.cfg.AutoRecord)
	a.ffmpegPath, a.ffmpegErr = recorder.FindFFmpeg()
	if a.ffmpegErr == nil {
		recorder.ProbeEncoders(a.ffmpegPath)
	}

	a.win = a.fapp.NewWindow("Spout Multi Recorder")
	a.win.Resize(fyne.NewSize(1100, 720))
	eng.OnChange = func() {
		fyne.Do(a.rebuildGrid)
	}
	a.buildUI()
	a.startTickers()

	a.win.SetOnClosed(func() {
		a.cfg.save()
		eng.Close()
		aud.Close()
	})
	a.win.ShowAndRun()
}

func (a *App) buildUI() {
	// --- record / elapsed
	a.elapsed = widget.NewLabel("")
	a.recordBtn = widget.NewButtonWithIcon("Record", theme.MediaRecordIcon(), a.toggleRecord)
	a.recordBtn.Importance = widget.HighImportance

	// --- audio device
	a.refreshDevices()
	opts := []string{"No audio"}
	sel := "No audio"
	for _, d := range a.devices {
		opts = append(opts, d.Label())
		if d.Name == a.cfg.AudioDevice && d.Loopback == a.cfg.AudioLoopback {
			sel = d.Label()
		}
	}
	a.audioSel = widget.NewSelect(opts, a.onAudioSelected)
	a.audioSel.SetSelected(sel)

	// --- codec / fps / channels
	codecOpts := make([]string, len(recorder.Codecs))
	codecSel := recorder.Codecs[0].Label
	for i, c := range recorder.Codecs {
		codecOpts[i] = c.Label
		if c.ID == a.cfg.Codec {
			codecSel = c.Label
		}
	}
	a.codecSel = widget.NewSelect(codecOpts, func(s string) {
		for _, c := range recorder.Codecs {
			if c.Label == s {
				a.cfg.Codec = c.ID
			}
		}
		a.cfg.save()
	})
	a.codecSel.SetSelected(codecSel)

	a.fpsSel = widget.NewSelect([]string{"24", "25", "30", "50", "60"}, func(s string) {
		a.cfg.FPS, _ = strconv.Atoi(s)
		a.cfg.save()
	})
	a.fpsSel.SetSelected(strconv.Itoa(a.cfg.FPS))

	var chOpts []string
	for i := 1; i <= 16; i++ {
		chOpts = append(chOpts, strconv.Itoa(i))
	}
	a.maxChSel = widget.NewSelect(chOpts, func(s string) {
		a.cfg.MaxChannels, _ = strconv.Atoi(s)
		a.eng.SetMaxChannels(a.cfg.MaxChannels)
		a.cfg.save()
	})
	a.maxChSel.SetSelected(strconv.Itoa(a.cfg.MaxChannels))

	a.autoChk = widget.NewCheck("Auto-record new senders", func(v bool) {
		a.cfg.AutoRecord = v
		a.eng.SetAutoRecord(v)
		a.cfg.save()
	})
	a.autoChk.SetChecked(a.cfg.AutoRecord)

	// --- output folder
	a.outEntry = widget.NewEntry()
	a.outEntry.SetText(a.cfg.OutDir)
	a.outEntry.OnChanged = func(s string) { a.cfg.OutDir = s; a.cfg.save() }
	a.browseBtn = widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		d := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
			if err == nil && u != nil {
				a.outEntry.SetText(u.Path())
			}
		}, a.win)
		d.Show()
	})

	// --- VU meter
	a.vu = NewVUMeter()

	// --- status bar
	a.statusBar = widget.NewLabel("")
	a.statusBar.Truncation = fyne.TextTruncateEllipsis
	if a.ffmpegErr != nil {
		a.statusBar.SetText("⚠ " + a.ffmpegErr.Error())
	} else {
		a.statusBar.SetText("FFmpeg: " + a.ffmpegPath)
	}

	// --- layout
	row1 := container.NewHBox(
		a.recordBtn, a.elapsed,
		layout.NewSpacer(),
		widget.NewLabel("Audio:"), a.audioSel,
	)
	row2 := container.NewHBox(
		widget.NewLabel("Codec:"), a.codecSel,
		widget.NewLabel("FPS:"), a.fpsSel,
		widget.NewLabel("Max channels:"), a.maxChSel,
		a.autoChk,
	)
	row3 := container.NewBorder(nil, nil, widget.NewLabel("Save to:"), a.browseBtn, a.outEntry)

	a.grid = container.NewGridWrap(fyne.NewSize(324, 262))
	a.scroll = container.NewVScroll(a.grid)
	emptyLbl := widget.NewLabel("Waiting for Spout senders…\nStart any Spout-enabled app and it will appear here.")
	emptyLbl.Alignment = fyne.TextAlignCenter
	a.emptyBox = container.NewCenter(emptyLbl)
	a.rebuildGrid()

	top := container.NewVBox(row1, row2, row3, a.vu)
	body := container.NewStack(a.scroll, a.emptyBox)
	content := container.NewBorder(top, a.statusBar, nil, nil, body)
	a.win.SetContent(content)
}

func (a *App) refreshDevices() {
	devs, err := a.aud.Devices()
	if err != nil {
		log.Printf("audio devices: %v", err)
	}
	a.devices = devs
}

func (a *App) onAudioSelected(sel string) {
	if sel == "No audio" {
		a.aud.StopCapture()
		a.cfg.AudioDevice = ""
		a.cfg.save()
		return
	}
	for _, d := range a.devices {
		if d.Label() == sel {
			if err := a.aud.StartCapture(d); err != nil {
				dialog.ShowError(err, a.win)
				a.audioSel.SetSelected("No audio")
				return
			}
			a.cfg.AudioDevice = d.Name
			a.cfg.AudioLoopback = d.Loopback
			a.cfg.save()
			return
		}
	}
}

func blackImage() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 9))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	return img
}

func (a *App) rebuildGrid() {
	chans := a.eng.Channels()
	a.grid.RemoveAll()

	live := map[string]bool{}
	for _, ch := range chans {
		live[ch.Name] = true
		card, ok := a.cards[ch.Name]
		if !ok {
			card = a.newCard(ch)
			a.cards[ch.Name] = card
		}
		a.grid.Add(card.obj)
	}
	for name := range a.cards {
		if !live[name] {
			delete(a.cards, name)
		}
	}
	if len(chans) == 0 {
		a.emptyBox.Show()
		a.scroll.Hide()
	} else {
		a.emptyBox.Hide()
		a.scroll.Show()
	}
	a.grid.Refresh()
}

func (a *App) newCard(ch *engine.Channel) *channelCard {
	img := canvas.NewImageFromImage(blackImage())
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(308, 173))

	check := widget.NewCheck("record", func(v bool) { ch.SetArmed(v) })
	check.SetChecked(ch.Armed())
	// While recording, arming only matters when auto-record can pick it up.
	if a.eng.Recording() && !a.cfg.AutoRecord {
		check.Disable()
	}
	status := widget.NewLabel("⚫ waiting")

	name := widget.NewLabelWithStyle(ch.Name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	name.Truncation = fyne.TextTruncateEllipsis

	box := container.NewVBox(name, img, container.NewHBox(check, layout.NewSpacer(), status))
	return &channelCard{ch: ch, img: img, check: check, status: status, obj: container.NewPadded(box)}
}

func (a *App) startTickers() {
	// VU meter, 20 Hz
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			l, r := a.aud.Levels()
			fyne.Do(func() { a.vu.SetLevels(l, r) })
		}
	}()

	// Previews, ~7 fps
	go func() {
		t := time.NewTicker(150 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			type upd struct {
				card *channelCard
				img  image.Image
			}
			var updates []upd
			for _, card := range a.snapshotCards() {
				seq := card.ch.Buf.Seq()
				if seq == card.lastSeq {
					continue
				}
				card.lastSeq = seq
				if !card.ch.Online() {
					updates = append(updates, upd{card, blackImage()})
					continue
				}
				if p := card.ch.Buf.Preview(320); p != nil {
					updates = append(updates, upd{card, p})
				}
			}
			if len(updates) == 0 {
				continue
			}
			fyne.Do(func() {
				for _, u := range updates {
					u.card.img.Image = u.img
					u.card.img.Refresh()
				}
			})
		}
	}()

	// Status labels + elapsed, 2 Hz
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			cards := a.snapshotCards()
			recording := a.eng.Recording()
			fyne.Do(func() {
				for _, card := range cards {
					card.status.SetText(a.cardStatus(card))
				}
				if recording {
					d := time.Since(a.recStart).Round(time.Second)
					a.elapsed.SetText(fmt.Sprintf("● %s", d))
				} else {
					a.elapsed.SetText("")
				}
			})
		}
	}()
}

func (a *App) snapshotCards() []*channelCard {
	out := make([]*channelCard, 0, len(a.cards))
	fyne.DoAndWait(func() {
		for _, c := range a.cards {
			out = append(out, c)
		}
	})
	return out
}

func (a *App) cardStatus(card *channelCard) string {
	rec := card.ch.Recorder()
	w, h, _, _ := card.ch.Buf.Dims()
	online := card.ch.Online()
	switch {
	case rec != nil && rec.Err() != nil:
		return "⚠ error"
	case rec != nil && !online:
		return fmt.Sprintf("🔴 REC (black) %d f", rec.Frames())
	case rec != nil:
		return fmt.Sprintf("🔴 REC %d f", rec.Frames())
	case online:
		return fmt.Sprintf("🟢 %d×%d", w, h)
	case w > 0:
		return "⚫ offline"
	default:
		return "⚫ waiting"
	}
}

func (a *App) setControlsEnabled(enabled bool) {
	ws := []fyne.Disableable{a.audioSel, a.codecSel, a.fpsSel, a.maxChSel, a.outEntry, a.browseBtn}
	for _, w := range ws {
		if enabled {
			w.Enable()
		} else {
			w.Disable()
		}
	}
	for _, c := range a.cards {
		if enabled || a.cfg.AutoRecord {
			c.check.Enable()
		} else {
			c.check.Disable()
		}
	}
}

func (a *App) toggleRecord() {
	if a.stopping {
		return
	}
	if a.eng.Recording() {
		a.stopping = true
		a.recordBtn.Disable()
		a.recordBtn.SetText("Stopping…")
		go func() {
			a.eng.StopRecording()
			fyne.Do(func() {
				a.stopping = false
				a.recordBtn.SetText("Record")
				a.recordBtn.SetIcon(theme.MediaRecordIcon())
				a.recordBtn.Enable()
				a.setControlsEnabled(true)
				a.statusBar.SetText("Recording saved to " + a.cfg.OutDir)
			})
		}()
		return
	}

	if a.ffmpegErr != nil {
		dialog.ShowError(a.ffmpegErr, a.win)
		return
	}
	if err := os.MkdirAll(a.cfg.OutDir, 0o755); err != nil {
		dialog.ShowError(fmt.Errorf("cannot create output folder: %w", err), a.win)
		return
	}

	var audioEng *audio.Engine
	if a.aud.Running() {
		audioEng = a.aud
	}
	set := recorder.Settings{
		FFmpegPath: a.ffmpegPath,
		OutDir:     a.cfg.OutDir,
		FPS:        a.cfg.FPS,
		Codec:      recorder.CodecByID(a.cfg.Codec),
		Audio:      audioEng,
	}
	n, err := a.eng.StartRecording(set)
	if err != nil {
		dialog.ShowError(err, a.win)
		return
	}
	a.recStart = time.Now()
	a.recordBtn.SetText("Stop")
	a.recordBtn.SetIcon(theme.MediaStopIcon())
	a.setControlsEnabled(false)
	if n == 0 {
		a.statusBar.SetText("Recording armed — waiting for senders (auto-record)…")
	} else {
		a.statusBar.SetText(fmt.Sprintf("Recording %d channel(s)…", n))
	}
}
