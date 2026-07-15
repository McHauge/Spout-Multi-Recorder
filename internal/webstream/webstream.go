// Package webstream pulls network video streams — RTMP, RTSP (incl. the
// rtspt:// TCP convention), HLS/HTTP, SRT, UDP and anything else the local
// FFmpeg build speaks — by running one FFmpeg decode process per stream:
// video arrives as raw BGRA frames on stdout, audio as interleaved s16le
// 48 kHz PCM over a loopback TCP connection. The stream is probed first
// (ffprobe when available, otherwise FFmpeg's stream banner) to learn the
// frame geometry and audio channel count.
package webstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MaxChannels caps how many source audio channels are preserved (matches the
// rest of the audio pipeline).
const MaxChannels = 16

// Info describes a probed stream.
type Info struct {
	W, H    int
	AudioCh int // 0 = no audio stream
}

// InputArgs translates a user-entered URL into the FFmpeg input URL and any
// extra input options. rtspt:// (RTSP-over-TCP, VLC convention) becomes
// rtsp:// with -rtsp_transport tcp; every other URL passes through unchanged.
func InputArgs(raw string) (string, []string) {
	if len(raw) >= 8 && strings.EqualFold(raw[:8], "rtspt://") {
		return "rtsp://" + raw[8:], []string{"-rtsp_transport", "tcp"}
	}
	return raw, nil
}

// ValidURL reports whether raw looks like a stream URL FFmpeg could open.
func ValidURL(raw string) bool {
	i := strings.Index(raw, "://")
	return i > 0 && len(raw) > i+3
}

// DeriveName builds a short human-readable channel name from a stream URL,
// e.g. "rtsp://user:pw@cam.local:554/live/ch0" -> "cam.local live ch0".
func DeriveName(raw string) string {
	norm, _ := InputArgs(raw)
	u, err := url.Parse(norm)
	if err != nil || u.Host == "" {
		s := raw
		if i := strings.Index(s, "://"); i >= 0 {
			s = s[i+3:]
		}
		return strings.TrimSpace(strings.NewReplacer("/", " ", "?", " ").Replace(s))
	}
	name := u.Hostname()
	if p := strings.Trim(u.Path, "/"); p != "" {
		name += " " + strings.ReplaceAll(p, "/", " ")
	}
	return strings.TrimSpace(name)
}

// ffprobePath returns the ffprobe binary matching ffmpegPath, or "".
func ffprobePath(ffmpegPath string) string {
	dir, file := filepath.Split(ffmpegPath)
	probe := "ffprobe"
	if strings.HasSuffix(strings.ToLower(file), ".exe") {
		probe += ".exe"
	}
	if dir != "" {
		p := filepath.Join(dir, probe)
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath(probe); err == nil {
		return p
	}
	return ""
}

// Probe determines the stream's video geometry and audio channel count.
// It prefers ffprobe (clean JSON) and falls back to parsing the FFmpeg
// stream banner. ctx bounds the network wait.
func Probe(ctx context.Context, ffmpegPath, rawURL string) (Info, error) {
	target, in := InputArgs(rawURL)
	if fp := ffprobePath(ffmpegPath); fp != "" {
		args := append([]string{"-v", "error", "-print_format", "json", "-show_streams"}, in...)
		args = append(args, target)
		cmd := exec.CommandContext(ctx, fp, args...)
		cmd.SysProcAttr = sysProcAttr()
		out, err := cmd.Output()
		if err == nil {
			if info, perr := parseProbeJSON(out); perr == nil {
				return info, nil
			}
		}
		// fall through to the ffmpeg banner on any ffprobe failure
	}
	args := append([]string{"-hide_banner", "-nostats"}, in...)
	args = append(args, "-i", target, "-frames:v", "1", "-f", "null", "-")
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.SysProcAttr = sysProcAttr()
	stderr, _ := cmd.CombinedOutput() // the banner is on stderr; exit code may be non-zero
	info, err := parseBanner(string(stderr))
	if err != nil {
		return Info{}, fmt.Errorf("probe %s: %w", rawURL, err)
	}
	return info, nil
}

func parseProbeJSON(b []byte) (Info, error) {
	var doc struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Channels  int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return Info{}, err
	}
	var info Info
	for _, s := range doc.Streams {
		switch s.CodecType {
		case "video":
			if info.W == 0 && s.Width > 0 && s.Height > 0 {
				info.W, info.H = s.Width, s.Height
			}
		case "audio":
			if info.AudioCh == 0 && s.Channels > 0 {
				info.AudioCh = s.Channels
			}
		}
	}
	if info.W == 0 {
		return Info{}, fmt.Errorf("no video stream found")
	}
	if info.AudioCh > MaxChannels {
		info.AudioCh = MaxChannels
	}
	return info, nil
}

var (
	bannerVideoRe = regexp.MustCompile(`Stream #\d+:\d+.*: Video: .*?(\d{2,5})x(\d{2,5})`)
	bannerAudioRe = regexp.MustCompile(`Stream #\d+:\d+.*: Audio: .*? Hz, ([^,\r\n]+)`)
	chCountRe     = regexp.MustCompile(`^(\d+) channels`)
	chDotRe       = regexp.MustCompile(`^(\d+)\.(\d+)$`)
)

// parseBanner extracts stream info from FFmpeg's input dump on stderr.
func parseBanner(s string) (Info, error) {
	var info Info
	if m := bannerVideoRe.FindStringSubmatch(s); m != nil {
		info.W, _ = strconv.Atoi(m[1])
		info.H, _ = strconv.Atoi(m[2])
	}
	if info.W == 0 || info.H == 0 {
		return Info{}, fmt.Errorf("no video stream found in FFmpeg output")
	}
	if m := bannerAudioRe.FindStringSubmatch(s); m != nil {
		info.AudioCh = chFromLayout(strings.TrimSpace(m[1]))
	}
	if info.AudioCh > MaxChannels {
		info.AudioCh = MaxChannels
	}
	return info, nil
}

// chFromLayout maps an FFmpeg channel-layout token ("stereo", "mono", "5.1",
// "5.1(side)", "7.1", "quad", "N channels", ...) to a channel count.
func chFromLayout(layout string) int {
	if i := strings.IndexByte(layout, '('); i >= 0 {
		layout = layout[:i]
	}
	layout = strings.TrimSpace(layout)
	switch layout {
	case "mono":
		return 1
	case "stereo", "downmix":
		return 2
	case "quad":
		return 4
	}
	if m := chCountRe.FindStringSubmatch(layout); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if m := chDotRe.FindStringSubmatch(layout); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		return a + b
	}
	return 2
}
