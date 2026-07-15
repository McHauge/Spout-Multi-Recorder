package engine

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/McHauge/Spout-Multi-Recorder/internal/recorder"
	"github.com/McHauge/Spout-Multi-Recorder/internal/spout"
	"github.com/McHauge/Spout-Multi-Recorder/internal/webstream"
)

// AddWeb adds a network stream (RTMP, RTSP/rtspt, HLS, SRT, ...) as a channel
// (idempotent), keyed by the user-chosen name. url is the stream address.
// replaceAudio=false records the stream's own embedded audio when present.
func (e *Engine) AddWeb(name, url string, replaceAudio bool) {
	e.addManual(WebPrefix+name, KindWeb, replaceAudio, func(c *Channel) {
		c.deviceID = url
	}, (*Channel).captureWeb)
}

// captureWeb pulls a network stream through an FFmpeg decode process:
// probe → decode → publish frames, reconnecting with backoff when the stream
// drops. While the stream is down an active recording keeps writing black
// frames (and pump silence), like every other source.
func (c *Channel) captureWeb() {
	defer close(c.done)
	ffmpeg, err := recorder.FindFFmpeg()
	if err != nil {
		log.Printf("channel %s: %v", c.Name, err)
		<-c.stop
		return
	}
	go c.pump.run(c.stop)

	backoff := time.Second
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		gotFrames, stopped := c.runWebStream(ffmpeg)
		if stopped {
			return
		}
		c.online.Store(false)
		c.Buf.SetConnected(false)
		if gotFrames {
			backoff = time.Second // stream was up: retry quickly
		} else if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		if !c.sleepStop(backoff) {
			return
		}
	}
}

// runWebStream performs one probe+decode cycle. gotFrames reports whether any
// video arrived (resets the retry backoff); stopped reports that the channel
// was stopped and the caller should exit.
func (c *Channel) runWebStream(ffmpeg string) (gotFrames, stopped bool) {
	url := c.deviceID

	// Probe, aborting promptly when the channel stops meanwhile.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		select {
		case <-c.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	info, err := webstream.Probe(ctx, ffmpeg, url)
	if err != nil {
		select {
		case <-c.stop:
			return false, true
		default:
		}
		log.Printf("channel %s: %v", c.Name, err)
		return false, false
	}

	rx, err := webstream.Open(ffmpeg, url, info)
	if err != nil {
		log.Printf("channel %s: %v", c.Name, err)
		return false, false
	}
	log.Printf("web stream %q connected (%dx%d, %dch audio)", c.DisplayName(), info.W, info.H, info.AudioCh)

	go rx.AudioLoop(c.pump.push) // no-op for streams without audio

	// Watchdog: closes the receiver when the channel stops or the stream
	// stalls, which unblocks ReadFrame below. Also closes it on normal exit.
	var lastFrame atomic.Int64
	lastFrame.Store(time.Now().UnixNano())
	readerDone := make(chan struct{})
	defer close(readerDone)
	go func() {
		defer rx.Close()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-readerDone:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastFrame.Load())) > 20*time.Second {
					log.Printf("channel %s: stream stalled for 20s, reconnecting", c.Name)
					return
				}
			}
		}
	}()

	for {
		pix, err := rx.ReadFrame()
		if err != nil {
			select {
			case <-c.stop:
				return gotFrames, true
			default:
				return gotFrames, false
			}
		}
		gotFrames = true
		lastFrame.Store(time.Now().UnixNano())
		c.online.Store(true)
		c.Buf.Store(pix, info.W, info.H, spout.FormatBGRA8, true)
	}
}
