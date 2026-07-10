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

	"golang.org/x/sys/windows"

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
	"github.com/McHauge/Spout-Multi-Recorder/internal/ndi"
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
	// SessionFolders puts each recording session into its own timestamped
	// subfolder of OutDir so files that belong together stay together.
	SessionFolders bool `json:"session_folders"`
	// Timecode embeds the wall-clock time-of-day as each file's start
	// timecode, for timecode sync in DaVinci Resolve etc.
	Timecode bool `json:"timecode"`
	// ResolveProject writes .drp, .xml and .fcpxml project files next to
	// the recordings for direct import into DaVinci Resolve.
	ResolveProject bool     `json:"resolve_project"`
	NDISources     []string `json:"ndi_sources"`
	// Per-NDI-source master-audio preference (default false = native NDI audio).
	NDIReplaceAudio map[string]bool `json:"ndi_replace_audio,omitempty"`
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
	cfg := Config{Codec: "h264", FPS: 30, MaxChannels: 8,
		SessionFolders: true, Timecode: true, ResolveProject: true}
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

	audioSel   *widget.Select
	codecSel   *widget.Select
	fpsSel     *widget.Select
	maxChSel   *widget.Select
	autoChk    *widget.Check
	sessionChk *widget.Check
	tcChk      *widget.Check
	resolveChk *widget.Check
	outEntry   *widget.Entry
	browseBtn  *widget.Button

	devices  []audio.Device
	cards    map[string]*channelCard
	recStart time.Time
	stopping bool
	gridWide bool
	version  string
}

type channelCard struct {
	ch        *engine.Channel
	img       *canvas.Image
	check     *widget.Check
	audio     *widget.Check
	audioInfo *widget.Label
	meter     *MultiVU
	status    *widget.Label
	obj       fyne.CanvasObject
	lastSeq   uint64
}

// Run builds the window and blocks until the app exits.
func Run(eng *engine.Engine, aud *audio.Engine, version string) {
	a := &App{
		version: version,
		fapp:    app.NewWithID("com.mchauge.spoutmultirecorder"),
		eng:     eng,
		aud:     aud,
		cfg:     LoadConfig(),
		cards:   map[string]*channelCard{},
	}
	a.fapp.SetIcon(assets.Icon)
	eng.SetMaxChannels(a.cfg.MaxChannels)
	eng.SetAutoRecord(a.cfg.AutoRecord)
	for _, n := range a.cfg.NDISources {
		eng.AddNDI(n, a.cfg.NDIReplaceAudio[n])
	}
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
	})
	a.win.ShowAndRun()

	// The window is gone — tear down with a hard deadline. Both the NDI and
	// WASAPI runtimes can hang during teardown, and even ExitProcess (what
	// os.Exit calls) runs DLL detach handlers that can block forever. The
	// watchdog therefore uses TerminateProcess, which skips DLL teardown and
	// cannot be blocked. 30 s leaves room for an active recording session to
	// finalise its files (recorder.Stop waits up to 15 s per FFmpeg process).
	log.Printf("shutting down")
	go func() {
		time.Sleep(30 * time.Second)
		log.Printf("shutdown watchdog: teardown stuck after 30s, terminating process")
		_ = windows.TerminateProcess(windows.CurrentProcess(), 0)
	}()
	eng.Close() // bounded: waits at most ~5 s for capture loops
	log.Printf("engine closed")
	aud.Close()
	log.Printf("audio closed, exiting")
	os.Exit(0)
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

	a.autoChk = widget.NewCheck("Auto-record new Spout senders", func(v bool) {
		a.cfg.AutoRecord = v
		a.eng.SetAutoRecord(v)
		a.cfg.save()
	})
	a.autoChk.SetChecked(a.cfg.AutoRecord)

	// --- session / Resolve options
	a.sessionChk = widget.NewCheck("Folder per recording", func(v bool) {
		a.cfg.SessionFolders = v
		a.cfg.save()
	})
	a.sessionChk.SetChecked(a.cfg.SessionFolders)
	a.tcChk = widget.NewCheck("Embed timecode", func(v bool) {
		a.cfg.Timecode = v
		a.cfg.save()
	})
	a.tcChk.SetChecked(a.cfg.Timecode)
	a.resolveChk = widget.NewCheck("Resolve project files (.drp/.xml/.fcpxml)", func(v bool) {
		a.cfg.ResolveProject = v
		a.cfg.save()
	})
	a.resolveChk.SetChecked(a.cfg.ResolveProject)

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
	addNDIBtn := widget.NewButtonWithIcon("Add NDI", theme.ContentAddIcon(), a.addNDISource)
	row2 := container.NewHBox(
		widget.NewLabel("Codec:"), a.codecSel,
		widget.NewLabel("FPS:"), a.fpsSel,
		widget.NewLabel("Max channels:"), a.maxChSel,
		a.autoChk,
		layout.NewSpacer(),
		addNDIBtn,
	)
	row3 := container.NewBorder(nil, nil, widget.NewLabel("Save to:"), a.browseBtn, a.outEntry)
	row4 := container.NewHBox(a.sessionChk, a.tcChk, a.resolveChk, layout.NewSpacer())

	a.grid = container.NewGridWrap(fyne.NewSize(324, 300))
	a.scroll = container.NewVScroll(a.grid)
	emptyLbl := widget.NewLabel("Waiting for Spout senders…\nStart any Spout-enabled app and it will appear here.")
	emptyLbl.Alignment = fyne.TextAlignCenter
	a.emptyBox = container.NewCenter(emptyLbl)
	a.rebuildGrid()

	top := container.NewVBox(row1, row2, row3, row4, a.vu)
	body := container.NewStack(a.scroll, a.emptyBox)
	verText := a.version
	if verText != "" && verText[0] >= '0' && verText[0] <= '9' {
		verText = "v" + verText
	}
	verLbl := widget.NewLabel(verText)
	bottom := container.NewBorder(nil, nil, nil, verLbl, a.statusBar)
	content := container.NewBorder(top, bottom, nil, nil, body)
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
	img.SetMinSize(fyne.NewSize(278, 156))
	meter := NewMultiVU()

	check := widget.NewCheck("record", func(v bool) { ch.SetArmed(v) })
	check.SetChecked(ch.Armed())

	// "master audio": mux the master device into this channel's file. When
	// off, NDI channels record their native audio; Spout channels get none.
	audioChk := widget.NewCheck("master audio", func(v bool) {
		ch.SetReplaceAudio(v)
		if ch.NDI {
			if a.cfg.NDIReplaceAudio == nil {
				a.cfg.NDIReplaceAudio = map[string]bool{}
			}
			a.cfg.NDIReplaceAudio[ch.DisplayName()] = v
			a.cfg.save()
		}
	})
	audioChk.SetChecked(ch.ReplaceAudio())

	// While recording, changes only matter when auto-record can pick them up.
	if a.eng.Recording() && !a.cfg.AutoRecord {
		check.Disable()
		audioChk.Disable()
	}
	status := widget.NewLabel("⚫ waiting")

	title := ch.DisplayName()
	if ch.NDI {
		title = "NDI • " + title
	}
	name := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	name.Truncation = fyne.TextTruncateEllipsis

	var header fyne.CanvasObject = name
	if ch.NDI {
		rm := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() { a.removeNDISource(ch) })
		header = container.NewBorder(nil, nil, nil, rm, name)
	}

	audioInfo := widget.NewLabel("")

	preview := container.NewHBox(layout.NewSpacer(), img, meter, layout.NewSpacer())
	box := container.NewVBox(header, preview,
		container.NewHBox(check, layout.NewSpacer(), status),
		container.NewHBox(audioChk, layout.NewSpacer(), audioInfo))
	card := &channelCard{ch: ch, img: img, check: check, audio: audioChk, audioInfo: audioInfo, meter: meter, status: status, obj: container.NewPadded(box)}
	audioInfo.SetText(a.cardAudioInfo(card))
	return card
}

// cardAudioInfo describes the audio the next (or current) recording of this
// channel gets, e.g. "2ch aac", "16ch opus" or "no audio".
func (a *App) cardAudioInfo(card *channelCard) string {
	codec := recorder.CodecByID(a.cfg.Codec)
	var ch int
	switch {
	case card.ch.ReplaceAudio():
		if !a.aud.Running() {
			return "no audio"
		}
		ch = 2
	case card.ch.NDI:
		ch = card.ch.NativeAudioChannels()
		if ch <= 0 {
			ch = 2
		}
	default:
		return "no audio"
	}
	name, maxCh := codec.AudioInfo()
	if maxCh > 0 && ch > maxCh {
		ch = maxCh
	}
	return fmt.Sprintf("%dch %s", ch, name)
}

// addNDISource browses the network and lets the user pick a source to add.
func (a *App) addNDISource() {
	if err := ndi.Available(); err != nil {
		dialog.ShowError(err, a.win)
		return
	}
	searching := dialog.NewCustomWithoutButtons("Searching for NDI sources…",
		widget.NewProgressBarInfinite(), a.win)
	searching.Show()

	go func() {
		srcs, err := ndi.FindSources(3 * time.Second)
		fyne.Do(func() {
			searching.Hide()
			if err != nil {
				dialog.ShowError(err, a.win)
				return
			}
			// Filter out sources that are already added.
			existing := map[string]bool{}
			for _, ch := range a.eng.Channels() {
				if ch.NDI {
					existing[ch.DisplayName()] = true
				}
			}
			var names []string
			for _, s := range srcs {
				if !existing[s.Name] {
					names = append(names, s.Name)
				}
			}
			if len(names) == 0 {
				dialog.ShowInformation("Add NDI source",
					"No (new) NDI sources found on the network.\nBoth full NDI and NDI|HX sources are supported.", a.win)
				return
			}

			selected := -1
			list := widget.NewList(
				func() int { return len(names) },
				func() fyne.CanvasObject { return widget.NewLabel("source name placeholder") },
				func(i widget.ListItemID, o fyne.CanvasObject) { o.(*widget.Label).SetText(names[i]) },
			)
			list.OnSelected = func(id widget.ListItemID) { selected = id }
			d := dialog.NewCustomConfirm("Add NDI source", "Add", "Cancel",
				container.NewVScroll(list), func(ok bool) {
					if !ok || selected < 0 {
						return
					}
					name := names[selected]
					a.eng.AddNDI(name, a.cfg.NDIReplaceAudio[name])
					found := false
					for _, n := range a.cfg.NDISources {
						if n == name {
							found = true
						}
					}
					if !found {
						a.cfg.NDISources = append(a.cfg.NDISources, name)
						a.cfg.save()
					}
				}, a.win)
			d.Resize(fyne.NewSize(520, 400))
			d.Show()
		})
	}()
}

// removeNDISource removes an NDI channel and forgets it in the config.
func (a *App) removeNDISource(ch *engine.Channel) {
	if err := a.eng.RemoveChannel(ch.Name); err != nil {
		dialog.ShowError(err, a.win)
		return
	}
	name := ch.DisplayName()
	out := a.cfg.NDISources[:0]
	for _, n := range a.cfg.NDISources {
		if n != name {
			out = append(out, n)
		}
	}
	a.cfg.NDISources = out
	delete(a.cfg.NDIReplaceAudio, name)
	a.cfg.save()
}

func (a *App) startTickers() {
	// VU meters (master bar + per-preview channel meters), 20 Hz
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			l, r := a.aud.Levels()
			masterOn := a.aud.Running()
			type lv struct {
				card   *channelCard
				levels []float64
			}
			cards := a.snapshotCards()
			ups := make([]lv, 0, len(cards))
			for _, card := range cards {
				var levels []float64
				switch {
				case card.ch.ReplaceAudio():
					if masterOn {
						levels = []float64{l, r}
					}
				case card.ch.NDI:
					levels = card.ch.AudioLevels()
				}
				ups = append(ups, lv{card, levels})
			}
			anyWide := false
			for _, u := range ups {
				if len(u.levels) > 4 {
					anyWide = true
					break
				}
			}
			fyne.Do(func() {
				a.vu.SetLevels(l, r)
				for _, u := range ups {
					u.card.meter.SetLevels(u.levels)
				}
				// The preview image stays constant; when any channel needs
				// the wide (8/16ch) meter, widen the grid columns instead.
				if anyWide != a.gridWide {
					a.gridWide = anyWide
					w := float32(324)
					if anyWide {
						w = 352
					}
					a.grid.Layout = layout.NewGridWrapLayout(fyne.NewSize(w, 300))
					a.grid.Refresh()
				}
			})
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
					card.audioInfo.SetText(a.cardAudioInfo(card))
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
		if b := rec.Behind(); b > 2*int64(a.cfg.FPS) {
			return fmt.Sprintf("🔴 REC %d f ⚠ %ds behind", rec.Frames(), b/int64(a.cfg.FPS))
		}
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
	ws := []fyne.Disableable{a.audioSel, a.codecSel, a.fpsSel, a.maxChSel, a.outEntry, a.browseBtn,
		a.sessionChk, a.tcChk, a.resolveChk}
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
			c.audio.Enable()
		} else {
			c.check.Disable()
			c.audio.Disable()
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
				dir := a.eng.SessionDir()
				if dir == "" {
					dir = a.cfg.OutDir
				}
				a.statusBar.SetText("Recording saved to " + dir)
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
		FFmpegPath:     a.ffmpegPath,
		OutDir:         a.cfg.OutDir,
		FPS:            a.cfg.FPS,
		Codec:          recorder.CodecByID(a.cfg.Codec),
		Audio:          audioEng,
		Timecode:       a.cfg.Timecode,
		SessionFolders: a.cfg.SessionFolders,
		ResolveProject: a.cfg.ResolveProject,
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
