package engine

import (
	"log"
	"time"

	"github.com/McHauge/Spout-Multi-Recorder/internal/audio"
	"github.com/McHauge/Spout-Multi-Recorder/internal/mfcap"
	"github.com/McHauge/Spout-Multi-Recorder/internal/spout"
)

// captureWebcam opens the webcam (by symbolic link), publishes BGRA frames to
// the mailbox and, if configured, captures its microphone into the audio pump.
// It reopens the device after a loss (e.g. USB unplug/replug) until stopped.
func (c *Channel) captureWebcam() {
	defer close(c.done)
	go c.pump.run(c.stop)

	tick := time.NewTicker(time.Second / 60)
	defer tick.Stop()

	for {
		select {
		case <-c.stop:
			return
		default:
		}

		cam, err := mfcap.Open(c.deviceID, c.camMode)
		if err != nil {
			log.Printf("channel %s: open webcam: %v", c.Name, err)
			c.online.Store(false)
			c.Buf.SetConnected(false)
			if !c.sleepStop(3 * time.Second) {
				return
			}
			continue
		}
		aux := c.startMic()
		lost := c.runWebcam(cam, tick)
		cam.Close()
		if aux != nil {
			aux.Close()
		}
		c.online.Store(false)
		c.Buf.SetConnected(false)
		if !lost {
			return // channel stopped
		}
		if !c.sleepStop(2 * time.Second) {
			return
		}
	}
}

// runWebcam polls the open camera until the channel stops or the device is
// lost. It returns true when the device was lost (caller should reopen).
func (c *Channel) runWebcam(cam *mfcap.Capture, tick *time.Ticker) bool {
	for {
		select {
		case <-c.stop:
			return false
		case <-tick.C:
		}
		f := cam.Latest()
		if f.Lost {
			log.Printf("channel %s: webcam lost, reopening", c.Name)
			return true
		}
		if f.NewFrame && f.Width > 0 {
			c.online.Store(true)
			c.Buf.Store(f.Pixels, f.Width, f.Height, spout.FormatBGRA8, true)
		} else if !f.Connected {
			c.online.Store(false)
			c.Buf.SetConnected(false)
		}
	}
}

// startMic opens the channel's microphone endpoint (if any) and feeds it into
// the audio pump. Best-effort: a missing/busy mic just yields no native audio.
func (c *Channel) startMic() *audio.AuxCapture {
	if c.audioDev == "" || c.eng == nil || c.eng.aud == nil {
		return nil
	}
	aux, err := c.eng.aud.OpenAux(c.audioDev, c.audioLoopback, func(pcm []byte, ch int) {
		c.pump.push(pcm, ch)
	})
	if err != nil {
		log.Printf("channel %s: mic %q: %v", c.Name, c.audioDev, err)
		return nil
	}
	return aux
}
