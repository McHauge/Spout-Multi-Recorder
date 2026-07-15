package webstream

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestInputArgs(t *testing.T) {
	cases := []struct {
		in      string
		wantURL string
		wantTCP bool
	}{
		{"rtmp://live.example/app/key", "rtmp://live.example/app/key", false},
		{"rtsp://cam.local/ch0", "rtsp://cam.local/ch0", false},
		{"rtspt://cam.local/ch0", "rtsp://cam.local/ch0", true},
		{"RTSPT://cam.local/ch0", "rtsp://cam.local/ch0", true},
		{"srt://host:9000?mode=caller", "srt://host:9000?mode=caller", false},
		{"https://cdn.example/live/master.m3u8", "https://cdn.example/live/master.m3u8", false},
	}
	for _, c := range cases {
		u, args := InputArgs(c.in)
		if u != c.wantURL {
			t.Errorf("InputArgs(%q) url = %q, want %q", c.in, u, c.wantURL)
		}
		hasTCP := len(args) == 2 && args[0] == "-rtsp_transport" && args[1] == "tcp"
		if hasTCP != c.wantTCP {
			t.Errorf("InputArgs(%q) args = %v, wantTCP=%v", c.in, args, c.wantTCP)
		}
	}
}

func TestDeriveName(t *testing.T) {
	cases := map[string]string{
		"rtsp://user:pw@cam.local:554/live/ch0": "cam.local live ch0",
		"rtmp://live.twitch.tv/app/streamkey":   "live.twitch.tv app streamkey",
		"https://cdn.example/hls/master.m3u8":   "cdn.example hls master.m3u8",
		"srt://10.0.0.5:9000":                   "10.0.0.5",
		"rtspt://cam.local/ch1":                 "cam.local ch1",
	}
	for in, want := range cases {
		if got := DeriveName(in); got != want {
			t.Errorf("DeriveName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidURL(t *testing.T) {
	for _, ok := range []string{"rtmp://x/y", "srt://h:1", "https://a/b.m3u8"} {
		if !ValidURL(ok) {
			t.Errorf("ValidURL(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "not a url", "://x", "rtmp://"} {
		if ValidURL(bad) {
			t.Errorf("ValidURL(%q) = true", bad)
		}
	}
}

func TestChFromLayout(t *testing.T) {
	cases := map[string]int{
		"mono": 1, "stereo": 2, "quad": 4, "downmix": 2,
		"5.1": 6, "5.1(side)": 6, "7.1": 8, "2 channels": 2,
		"16 channels": 16, "surround": 2, // unknown -> 2
	}
	for in, want := range cases {
		if got := chFromLayout(in); got != want {
			t.Errorf("chFromLayout(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseBanner(t *testing.T) {
	banner := `Input #0, flv, from 'rtmp://x/live':
  Duration: N/A, start: 0.000000, bitrate: N/A
  Stream #0:0: Video: h264 (High), yuv420p(progressive), 1920x1080 [SAR 1:1 DAR 16:9], 30 fps, 30 tbr, 1k tbn
  Stream #0:1: Audio: aac (LC), 48000 Hz, 5.1(side), fltp
`
	info, err := parseBanner(banner)
	if err != nil {
		t.Fatal(err)
	}
	if info.W != 1920 || info.H != 1080 || info.AudioCh != 6 {
		t.Errorf("got %+v", info)
	}
	if _, err := parseBanner("no streams here"); err == nil {
		t.Error("want error for bannerless output")
	}
}

func TestParseProbeJSON(t *testing.T) {
	j := []byte(`{"streams":[
		{"codec_type":"audio","channels":2},
		{"codec_type":"video","width":1280,"height":720}
	]}`)
	info, err := parseProbeJSON(j)
	if err != nil {
		t.Fatal(err)
	}
	if info.W != 1280 || info.H != 720 || info.AudioCh != 2 {
		t.Errorf("got %+v", info)
	}
}

// Integration: generate a short test clip with ffmpeg, then probe and decode
// it through the real Receiver pipeline (a file path is a valid FFmpeg input
// "URL", exercising the same code paths as a network stream).
func TestProbeAndReceive(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	clip := filepath.Join(t.TempDir(), "test.mp4")
	gen := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest", clip)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate clip: %v: %s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := Probe(ctx, ffmpeg, clip)
	if err != nil {
		t.Fatal(err)
	}
	if info.W != 320 || info.H != 180 || info.AudioCh != 1 {
		t.Fatalf("probe: got %+v", info)
	}

	rx, err := Open(ffmpeg, clip, info)
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()

	var audioBytes int
	audioDone := make(chan struct{})
	go func() {
		rx.AudioLoop(func(pcm []byte, ch int) {
			if ch != 1 {
				t.Errorf("audio ch = %d, want 1", ch)
			}
			audioBytes += len(pcm)
		})
		close(audioDone)
	}()

	frames := 0
	nonBlack := false
	for {
		pix, err := rx.ReadFrame()
		if err != nil {
			break
		}
		frames++
		if !bytes.Equal(pix[:64], make([]byte, 64)) {
			nonBlack = true
		}
	}
	if frames < 25 || frames > 35 {
		t.Errorf("got %d frames, want ~30", frames)
	}
	if !nonBlack {
		t.Error("frames look empty")
	}
	select {
	case <-audioDone:
	case <-time.After(5 * time.Second):
		t.Fatal("audio loop did not finish")
	}
	// ~1 s of 48 kHz mono s16 = 96000 bytes (trailing partial chunk dropped).
	if audioBytes < 60000 || audioBytes > 120000 {
		t.Errorf("audio bytes = %d, want ~96000", audioBytes)
	}
}

func TestProbeBadURL(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Probe(ctx, ffmpeg, "/nonexistent/file.mp4"); err == nil {
		t.Error("want error for bad input")
	}
}
