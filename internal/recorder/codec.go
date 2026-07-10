package recorder

import (
	"fmt"
	"os/exec"
	"strings"
)

// Codec is a user-selectable encoding preset.
type Codec struct {
	ID    string
	Label string
	Ext   string // container extension without dot
}

// Codecs available in the UI, in display order.
var Codecs = []Codec{
	{ID: "h264", Label: "H.264 (auto hardware)", Ext: "mp4"},
	{ID: "h264_sw", Label: "H.264 (software x264)", Ext: "mp4"},
	{ID: "hevc", Label: "HEVC / H.265 (auto hardware)", Ext: "mp4"},
	{ID: "h264_mkv", Label: "H.264 + Opus (MKV, multichannel)", Ext: "mkv"},
	{ID: "hevc_mkv", Label: "HEVC + Opus (MKV, multichannel)", Ext: "mkv"},
	{ID: "prores", Label: "ProRes 422 HQ (editing)", Ext: "mov"},
	{ID: "dnxhr", Label: "DNxHR HQ (editing)", Ext: "mov"},
	{ID: "mjpeg", Label: "MJPEG (low CPU, big files)", Ext: "mov"},
}

// AudioInfo returns the audio codec short name and its channel limit
// (0 = no practical limit) for this preset's container.
func (c Codec) AudioInfo() (name string, maxCh int) {
	switch c.Ext {
	case "mov":
		return "pcm", 0
	case "mkv":
		return "opus", 0 // mapping family 255 carries up to 255 channels
	default:
		return "aac", 8
	}
}

// CodecByID returns the codec with the given ID, defaulting to h264.
func CodecByID(id string) Codec {
	for _, c := range Codecs {
		if c.ID == id {
			return c
		}
	}
	return Codecs[0]
}

// encoderProbe caches which encoders this ffmpeg build supports.
var encoderProbe map[string]bool

// ProbeEncoders queries `ffmpeg -encoders` once and caches the result.
func ProbeEncoders(ffmpegPath string) {
	encoderProbe = map[string]bool{}
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && len(fields[0]) >= 1 && strings.ContainsAny(fields[0][:1], "VAS") {
			encoderProbe[fields[1]] = true
		}
	}
}

func hasEncoder(name string) bool {
	if encoderProbe == nil {
		return false
	}
	return encoderProbe[name]
}

// videoArgs returns the FFmpeg output arguments (video + audio codecs) for a
// codec preset. Hardware encoders are picked in order NVENC > QSV > AMF.
// audioCh is the input audio channel count; AAC (mp4) is limited to 8
// channels, so higher counts are downmixed there (PCM in MOV keeps all).
func videoArgs(c Codec, withAudio bool, audioCh int) []string {
	var v []string
	// MKV presets reuse the mp4 video encoder selection.
	id := c.ID
	switch id {
	case "h264_mkv":
		id = "h264"
	case "hevc_mkv":
		id = "hevc"
	}
	switch id {
	case "h264":
		switch {
		case hasEncoder("h264_nvenc"):
			v = []string{"-c:v", "h264_nvenc", "-preset", "p5", "-rc", "vbr", "-cq", "21", "-b:v", "0", "-pix_fmt", "yuv420p"}
		case hasEncoder("h264_qsv"):
			v = []string{"-c:v", "h264_qsv", "-global_quality", "21", "-pix_fmt", "nv12"}
		case hasEncoder("h264_amf"):
			v = []string{"-c:v", "h264_amf", "-quality", "quality", "-rc", "cqp", "-qp_i", "21", "-qp_p", "23", "-pix_fmt", "yuv420p"}
		default:
			v = []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p"}
		}
	case "h264_sw":
		v = []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p"}
	case "hevc":
		switch {
		case hasEncoder("hevc_nvenc"):
			v = []string{"-c:v", "hevc_nvenc", "-preset", "p5", "-rc", "vbr", "-cq", "23", "-b:v", "0", "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
		case hasEncoder("hevc_qsv"):
			v = []string{"-c:v", "hevc_qsv", "-global_quality", "23", "-pix_fmt", "nv12", "-tag:v", "hvc1"}
		case hasEncoder("hevc_amf"):
			v = []string{"-c:v", "hevc_amf", "-quality", "quality", "-rc", "cqp", "-qp_i", "23", "-qp_p", "25", "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
		default:
			v = []string{"-c:v", "libx265", "-preset", "fast", "-crf", "23", "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
		}
	case "prores":
		v = []string{"-c:v", "prores_ks", "-profile:v", "3", "-vendor", "apl0", "-pix_fmt", "yuv422p10le"}
	case "dnxhr":
		v = []string{"-c:v", "dnxhd", "-profile:v", "dnxhr_hq", "-pix_fmt", "yuv422p"}
	case "mjpeg":
		v = []string{"-c:v", "mjpeg", "-q:v", "3", "-pix_fmt", "yuvj422p"}
	default:
		v = []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p"}
	}

	if withAudio {
		switch c.Ext {
		case "mov":
			v = append(v, "-c:a", "pcm_s16le")
		case "mkv":
			// ~64 kbit/s per channel; discrete mapping beyond 8 channels.
			v = append(v, "-c:a", "libopus", "-b:a", fmt.Sprintf("%dk", 64*max(audioCh, 1)))
			if audioCh > 8 {
				v = append(v, "-mapping_family", "255")
			}
		default:
			v = append(v, "-c:a", "aac", "-b:a", "192k")
			if audioCh > 8 {
				v = append(v, "-ac", "8") // AAC caps at 8 channels
			}
		}
	}
	return v
}
