package engine

import (
	"log"
	"time"

	"github.com/McHauge/Spout-Multi-Recorder/internal/decklink"
	"github.com/McHauge/Spout-Multi-Recorder/internal/spout"
)

// captureDeckLink opens a DeckLink input, publishes BGRA frames to the mailbox
// and pushes embedded SDI audio into the pump. It reopens after an open failure
// (e.g. the card was briefly busy); loss of input signal is handled in-place by
// the SDK's format detection, so the device stays open.
func (c *Channel) captureDeckLink() {
	defer close(c.done)
	go c.pump.run(c.stop)

	tick := time.NewTicker(time.Second / 60)
	defer tick.Stop()
	abuf := make([]byte, 64*1024)

	for {
		select {
		case <-c.stop:
			return
		default:
		}

		cap, err := decklink.Open(c.deviceID)
		if err != nil {
			log.Printf("channel %s: open decklink: %v", c.Name, err)
			c.online.Store(false)
			c.Buf.SetConnected(false)
			if !c.sleepStop(5 * time.Second) {
				return
			}
			continue
		}
		stopped := c.runDeckLink(cap, tick, abuf)
		cap.Close()
		c.online.Store(false)
		c.Buf.SetConnected(false)
		if stopped {
			return // channel stopped
		}
		if !c.sleepStop(2 * time.Second) {
			return
		}
	}
}

// runDeckLink polls an open DeckLink input until the channel stops. It returns
// true when the channel was stopped (never reopens on its own).
func (c *Channel) runDeckLink(cap *decklink.Capture, tick *time.Ticker, abuf []byte) bool {
	for {
		select {
		case <-c.stop:
			return true
		case <-tick.C:
		}
		f := cap.Latest()
		if f.NewFrame && f.Width > 0 {
			c.online.Store(true)
			c.Buf.Store(f.Pixels, f.Width, f.Height, spout.FormatBGRA8, true)
		} else if !f.Connected {
			c.online.Store(false)
			c.Buf.SetConnected(false)
		}
		if n, ch := cap.ReadAudio(abuf); n > 0 && ch > 0 {
			c.pump.push(abuf[:n], ch)
		}
	}
}
