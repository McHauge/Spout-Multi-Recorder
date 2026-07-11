// Spout Multi Recorder - records all Spout video streams on a PC to disk,
// with one shared master audio track embedded in every file.
package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/McHauge/Spout-Multi-Recorder/internal/audio"
	"github.com/McHauge/Spout-Multi-Recorder/internal/engine"
	"github.com/McHauge/Spout-Multi-Recorder/internal/ui"
)

// version is stamped by the build (-ldflags "-X main.version=..."):
// the tag version on releases, `git describe` output from build.ps1.
var version = "dev"

// buildVersion falls back to the VCS revision Go embeds in every binary
// built from a git checkout, so even a plain `go build` is identifiable.
func buildVersion() string {
	if version != "dev" && version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "+dirty"
				}
			}
		}
		if rev != "" {
			if len(rev) > 7 {
				rev = rev[:7]
			}
			return "dev-" + rev + dirty
		}
	}
	return version
}

func main() {
	setupLogging()

	aud, err := audio.NewEngine()
	if err != nil {
		log.Fatalf("audio init: %v", err)
	}

	cfg := ui.LoadConfig()
	eng := engine.New(cfg.MaxChannels, aud)

	ui.Run(eng, aud, buildVersion())
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
