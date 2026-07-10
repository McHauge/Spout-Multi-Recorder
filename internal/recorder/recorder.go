// Package recorder runs one FFmpeg process per Spout stream: raw BGRA video
// is piped on stdin at a constant framerate (black frames while the sender is
// gone) and master audio arrives on a Windows named pipe.
package recorder

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/McHauge/Spout-Multi-Recorder/internal/audio"
	"github.com/McHauge/Spout-Multi-Recorder/internal/frame"
	"github.com/McHauge/Spout-Multi-Recorder/internal/resolve"
)

// Settings for one recording session (shared by all channels).
type Settings struct {
	FFmpegPath string
	OutDir     string
	FPS        int
	Codec      Codec
	Audio      *audio.Engine // master audio device (used per channel-preference)

	// Timecode embeds the wall-clock time-of-day as the file's start
	// timecode (a tmcd track in MP4/MOV, a TIMECODE tag in MKV), so files
	// from one session can be synced by timecode in an NLE.
	Timecode bool
	// SessionFolders groups each recording session into its own
	// subfolder of OutDir (handled by the engine).
	SessionFolders bool
	// ResolveProject writes DaVinci Resolve project files (.drp, .xml,
	// .fcpxml) next to the recordings when the session stops (handled by
	// the engine).
	ResolveProject bool
}

// AudioSource provides the interleaved s16le 48 kHz PCM stream muxed into a
// recording. Implemented by *audio.Engine (master device, stereo) and by the
// engine's NDI audio pump (native NDI audio, source channel count).
// Channels must be called after Subscribe (the format locks on subscription).
type AudioSource interface {
	Subscribe() (<-chan []byte, func())
	Channels() int
}

// Recorder records a single channel.
type Recorder struct {
	name        string
	buf         *frame.Buffer
	set         Settings
	w, h        int
	pixFmt      string
	file        string
	startTime   time.Time // wall-clock instant frame 0 corresponds to
	startFrames int64     // start timecode as frames since midnight
	audioCh     int       // audio channels in the file (0 = no audio track)
	cmd         *exec.Cmd
	stopCh      chan struct{}
	doneCh      chan struct{}
	frames      atomic.Int64
	errMu       sync.Mutex
	err         error
	unsub       func()
	pipeLis     interface{ Close() error }
}

// Info describes a (finished) recording for session exports.
type Info struct {
	Name        string // channel name as passed to Start
	File        string // output file path
	W, H, FPS   int
	StartFrames int64 // start timecode as frames since midnight
	Frames      int64 // video frames written
	AudioCh     int   // audio channels in the file (0 = none)
}

// Info returns the recording's metadata (safe after Stop; Frames is a live
// counter while recording).
func (r *Recorder) Info() Info {
	return Info{
		Name: r.name, File: r.file, W: r.w, H: r.h, FPS: r.set.FPS,
		StartFrames: r.startFrames, Frames: r.frames.Load(), AudioCh: r.audioCh,
	}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitize(s string) string {
	s = unsafeChars.ReplaceAllString(s, "_")
	if s == "" {
		s = "spout"
	}
	return s
}

var pipeCounter atomic.Int64

// Start begins recording the given channel. The video size is fixed at the
// channel's current dimensions for the duration of the recording; if the
// sender changes size mid-recording, frames are centered/cropped to fit.
// audioSrc selects this channel's audio (master device, NDI native audio,
// or nil for no audio track).
func Start(name string, buf *frame.Buffer, set Settings, audioSrc AudioSource) (*Recorder, error) {
	w, h, format, connected := buf.Dims()
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("%s: no frame received yet, cannot determine size", name)
	}
	_ = connected // recording starts even if currently disconnected (black)

	pixFmt := "bgra"
	if format == 28 { // DXGI_FORMAT_R8G8B8A8_UNORM
		pixFmt = "rgba"
	}
	// Most encoders want even dimensions.
	w &^= 1
	h &^= 1

	now := time.Now()
	ts := now.Format("2006-01-02_15-04-05")
	file := filepath.Join(set.OutDir, fmt.Sprintf("%s_%s.%s", sanitize(name), ts, set.Codec.Ext))

	r := &Recorder{
		name: name, buf: buf, set: set, w: w, h: h, pixFmt: pixFmt,
		file: file, startTime: now, startFrames: resolve.FramesSinceMidnight(now, set.FPS),
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}

	withAudio := audioSrc != nil
	var sub <-chan []byte
	audioCh := audio.Channels
	if withAudio {
		// Subscribe first: this locks the source's stream format so the
		// channel count passed to FFmpeg matches the delivered PCM.
		s, unsub := audioSrc.Subscribe()
		sub = s
		r.unsub = unsub
		audioCh = audioSrc.Channels()
		// Effective channel count in the file (AAC caps at 8).
		r.audioCh = audioCh
		if _, maxCh := set.Codec.AudioInfo(); maxCh > 0 && r.audioCh > maxCh {
			r.audioCh = maxCh
		}
	}

	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "rawvideo", "-pix_fmt", pixFmt,
		"-s", fmt.Sprintf("%dx%d", w, h),
		"-r", strconv.Itoa(set.FPS),
		"-thread_queue_size", "512",
		"-i", "pipe:0",
	}

	pipeName := ""
	if withAudio {
		pipeName = fmt.Sprintf(`\\.\pipe\smr_audio_%d_%d`, os.Getpid(), pipeCounter.Add(1))
		args = append(args,
			"-f", "s16le",
			"-ar", strconv.Itoa(audio.SampleRate),
			"-ac", strconv.Itoa(audioCh),
			"-thread_queue_size", "512",
			"-i", pipeName,
		)
	}

	args = append(args, videoArgs(set.Codec, withAudio, audioCh)...)
	if withAudio {
		// No -shortest: when the encoder lags behind real time, frames are
		// still queued at stop, and -shortest would cut the file at the
		// audio stream's end and discard that video backlog.
		args = append(args, "-map", "0:v", "-map", "1:a")
	}
	if set.Timecode {
		// Wall-clock time-of-day as start timecode: a tmcd track in
		// MP4/MOV, a TIMECODE stream tag in MKV. Set globally and on the
		// video stream so every muxer picks it up.
		tc := resolve.Timecode(r.startFrames, set.FPS)
		args = append(args, "-metadata", "timecode="+tc, "-metadata:s:v:0", "timecode="+tc)
	}
	args = append(args, file)

	// Audio pipe must be listening before FFmpeg starts.
	if withAudio {
		lis, err := winio.ListenPipe(pipeName, nil)
		if err != nil {
			r.cleanupAudio()
			return nil, fmt.Errorf("%s: create audio pipe: %w", name, err)
		}
		r.pipeLis = lis
		go func() {
			conn, err := lis.Accept()
			if err != nil {
				// listener closed before ffmpeg connected
				for range sub {
				}
				return
			}
			defer conn.Close()
			for chunk := range sub {
				if _, err := conn.Write(chunk); err != nil {
					return
				}
			}
		}()
	}

	cmd := exec.Command(set.FFmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.cleanupAudio()
		return nil, err
	}
	cmd.Stderr = log.Default().Writer()
	if err := cmd.Start(); err != nil {
		r.cleanupAudio()
		return nil, fmt.Errorf("%s: start ffmpeg: %w", name, err)
	}
	r.cmd = cmd
	log.Printf("recording %s -> %s (%dx%d %s @%dfps, audio=%v)", name, file, w, h, set.Codec.ID, set.FPS, withAudio)

	go r.videoLoop(stdin)
	return r, nil
}

func (r *Recorder) cleanupAudio() {
	if r.unsub != nil {
		r.unsub()
		r.unsub = nil
	}
	if r.pipeLis != nil {
		_ = r.pipeLis.Close()
		r.pipeLis = nil
	}
}

// videoLoop writes frames at an exact constant framerate derived from the
// wall clock, duplicating or padding with black as needed.
func (r *Recorder) videoLoop(stdin interface {
	Write([]byte) (int, error)
	Close() error
}) {
	defer close(r.doneCh)
	defer stdin.Close()

	size := r.w * r.h * 4
	buf := make([]byte, size) // current frame, fitted
	black := make([]byte, size)
	lastW, lastH := r.w, r.h

	// Pace from the instant captured in Start (which the embedded start
	// timecode refers to) so frame N corresponds to startTime + N/FPS; the
	// few frames "missed" while FFmpeg spawned are written immediately.
	start := r.startTime
	interval := time.Second / time.Duration(r.set.FPS)
	var written int64

	for {
		select {
		case <-r.stopCh:
			return
		default:
		}
		next := start.Add(time.Duration(written) * interval)
		if d := time.Until(next); d > 0 {
			select {
			case <-time.After(d):
			case <-r.stopCh:
				return
			}
		}

		// If the sender resolution changed, clear the canvas once so the
		// border around a smaller centered image is black, not stale pixels.
		if sw, sh, _, _ := r.buf.Dims(); sw != lastW || sh != lastH {
			lastW, lastH = sw, sh
			clear(buf)
		}
		ok := r.buf.Snapshot(buf, r.w, r.h)
		var out []byte
		if ok {
			out = buf
		} else {
			out = black
		}

		if _, err := stdin.Write(out); err != nil {
			r.setErr(fmt.Errorf("%s: ffmpeg closed the video pipe: %w", r.name, err))
			return
		}
		written++
		r.frames.Store(written)
	}
}

func (r *Recorder) setErr(err error) {
	r.errMu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.errMu.Unlock()
	log.Print(err)
}

// Err returns the first error encountered, if any.
func (r *Recorder) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

// Frames returns the number of video frames written so far.
func (r *Recorder) Frames() int64 { return r.frames.Load() }

// Behind reports how many frames the recording lags behind the wall clock.
// Near zero when the encoder keeps up; a growing value means FFmpeg cannot
// encode in real time (the file will be shorter than the session).
func (r *Recorder) Behind() int64 {
	expected := int64(time.Since(r.startTime) * time.Duration(r.set.FPS) / time.Second)
	if b := expected - r.frames.Load(); b > 0 {
		return b
	}
	return 0
}

// File returns the output file path.
func (r *Recorder) File() string { return r.file }

// Stop ends the recording and finalises the file.
func (r *Recorder) Stop() {
	close(r.stopCh)
	<-r.doneCh // video stdin closed
	r.cleanupAudio()

	if r.cmd != nil {
		done := make(chan error, 1)
		go func() { done <- r.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				r.setErr(fmt.Errorf("%s: ffmpeg exit: %w", r.name, err))
			}
		case <-time.After(15 * time.Second):
			_ = r.cmd.Process.Kill()
			<-done
			r.setErr(fmt.Errorf("%s: ffmpeg did not finish in time, killed", r.name))
		}
	}
	log.Printf("stopped %s (%d frames) -> %s", r.name, r.frames.Load(), r.file)
}

// FindFFmpeg locates ffmpeg.exe: next to the executable, then on PATH.
func FindFFmpeg() (string, error) {
	if exe, err := os.Executable(); err == nil {
		local := filepath.Join(filepath.Dir(exe), "ffmpeg.exe")
		if _, err := os.Stat(local); err == nil {
			return local, nil
		}
	}
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg.exe not found (put it next to the app or on PATH): %w", err)
	}
	return p, nil
}
