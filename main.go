// Spout Multi Recorder - records all Spout video streams on a PC to disk,
// with one shared master audio track embedded in every file.
package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/McHauge/Spout-Multi-Recorder/internal/audio"
	"github.com/McHauge/Spout-Multi-Recorder/internal/engine"
	"github.com/McHauge/Spout-Multi-Recorder/internal/ui"
)

func main() {
	setupLogging()

	aud, err := audio.NewEngine()
	if err != nil {
		log.Fatalf("audio init: %v", err)
	}

	cfg := ui.LoadConfig()
	eng := engine.New(cfg.MaxChannels)

	ui.Run(eng, aud)
}

func setupLogging() {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "SpoutMultiRecorder")
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
}
