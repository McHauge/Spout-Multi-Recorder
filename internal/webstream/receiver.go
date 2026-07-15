package webstream

import (
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// Receiver is one running FFmpeg decode process for a stream. Video frames
// are read with ReadFrame; audio (when the stream has any) is delivered by
// AudioLoop over a loopback TCP connection. Close kills the process and is
// safe to call from another goroutine to unblock ReadFrame.
type Receiver struct {
	info    Info
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	lis     net.Listener
	frame   []byte
	audioCh int

	mu     sync.Mutex
	closed bool
}

// Open probes nothing: it starts the decode process for a stream already
// described by info. A scale filter pins the frame geometry to info.W×info.H
// so adaptive streams (e.g. HLS variant switches) cannot change the raw
// frame size mid-stream.
func Open(ffmpegPath, rawURL string, info Info) (*Receiver, error) {
	if info.W <= 0 || info.H <= 0 {
		return nil, fmt.Errorf("webstream: invalid video size %dx%d", info.W, info.H)
	}
	target, in := InputArgs(rawURL)

	r := &Receiver{info: info, frame: make([]byte, info.W*info.H*4)}

	args := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	args = append(args, in...)
	args = append(args, "-i", target,
		"-map", "0:v:0",
		"-vf", fmt.Sprintf("scale=%d:%d", info.W, info.H),
		"-f", "rawvideo", "-pix_fmt", "bgra", "pipe:1",
	)
	if info.AudioCh > 0 {
		r.audioCh = info.AudioCh
		if r.audioCh > MaxChannels {
			r.audioCh = MaxChannels
		}
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("webstream: audio listener: %w", err)
		}
		r.lis = lis
		args = append(args,
			"-map", "0:a:0",
			"-f", "s16le", "-ar", "48000", "-ac", strconv.Itoa(r.audioCh),
			"tcp://"+lis.Addr().String(),
		)
	}

	cmd := exec.Command(ffmpegPath, args...)
	cmd.SysProcAttr = sysProcAttr()
	cmd.Stderr = log.Default().Writer()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		r.Close()
		return nil, fmt.Errorf("webstream: start ffmpeg: %w", err)
	}
	r.cmd = cmd
	r.stdout = stdout
	return r, nil
}

// Info returns the stream description the receiver was opened with.
func (r *Receiver) Info() Info { return r.info }

// ReadFrame blocks until the next full BGRA frame (W*H*4 bytes, buffer
// reused between calls) or returns an error when the stream/process ends.
func (r *Receiver) ReadFrame() ([]byte, error) {
	if _, err := io.ReadFull(r.stdout, r.frame); err != nil {
		return nil, err
	}
	return r.frame, nil
}

// AudioLoop accepts the decode process's audio connection and pushes fixed
// 20 ms PCM chunks (interleaved s16le 48 kHz) until the stream ends. Chunks
// stay sample-aligned so the channel interleave can never shift. No-op for
// streams without audio. Blocks; run it in its own goroutine.
func (r *Receiver) AudioLoop(push func(pcm []byte, ch int)) {
	if r.lis == nil {
		return
	}
	conn, err := r.lis.Accept()
	if err != nil {
		return // closed before FFmpeg connected
	}
	defer conn.Close()
	chunk := make([]byte, 48000/50*r.audioCh*2) // 20 ms
	for {
		if _, err := io.ReadFull(conn, chunk); err != nil {
			return
		}
		push(chunk, r.audioCh)
	}
}

// Close kills the decode process and releases the audio listener. Safe to
// call concurrently with ReadFrame/AudioLoop (unblocks both) and to call
// more than once.
func (r *Receiver) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()

	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	if r.stdout != nil {
		_ = r.stdout.Close()
	}
	if r.lis != nil {
		_ = r.lis.Close()
	}
	if r.cmd != nil {
		done := make(chan struct{})
		go func() { _ = r.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}
