package recorder

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/McHauge/Spout-Multi-Recorder/internal/hwstat"
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
	{ID: "av1", Label: "AV1 (auto hardware)", Ext: "mp4"},
	{ID: "h264_mkv", Label: "H.264 + Opus (MKV, multichannel)", Ext: "mkv"},
	{ID: "hevc_mkv", Label: "HEVC + Opus (MKV, multichannel)", Ext: "mkv"},
	{ID: "av1_mkv", Label: "AV1 + Opus (MKV, multichannel)", Ext: "mkv"},
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

// baseID maps the MKV presets onto the mp4 video-encoder family so they reuse
// the same encoder selection.
func baseID(id string) string {
	switch id {
	case "h264_mkv":
		return "h264"
	case "hevc_mkv":
		return "hevc"
	case "av1_mkv":
		return "av1"
	}
	return id
}

// codecFamily returns "h264", "hevc" or "av1" for presets that have hardware
// encoders, or "" for software-only presets (h264_sw, prores, dnxhr, mjpeg).
func codecFamily(c Codec) string {
	switch baseID(c.ID) {
	case "h264", "hevc", "av1":
		return baseID(c.ID)
	}
	return ""
}

// AvailableVendors reports which encoder backends can serve this codec. A
// hardware vendor counts only when the ffmpeg build has its encoder *and* a
// matching GPU is physically installed (an ffmpeg build can list e.g. QSV on a
// machine with no Intel GPU). CPU is always available; software-only presets
// return CPU alone. Used by the load balancer to decide where channels can go.
func AvailableVendors(c Codec) map[hwstat.Vendor]bool {
	av := map[hwstat.Vendor]bool{hwstat.VendorCPU: true}
	fam := codecFamily(c)
	if fam == "" {
		return av
	}
	av[hwstat.VendorNVENC] = hasEncoder(fam+"_nvenc") && hwstat.PresentGPU(hwstat.VendorNVENC)
	av[hwstat.VendorIntel] = hasEncoder(fam+"_qsv") && hwstat.PresentGPU(hwstat.VendorIntel)
	av[hwstat.VendorAMD] = hasEncoder(fam+"_amf") && hwstat.PresentGPU(hwstat.VendorAMD)
	return av
}

// videoArgs returns the FFmpeg output arguments (video + audio codecs) for a
// codec preset, using the encoder backend chosen by the load balancer. If the
// requested hardware encoder is unavailable it falls back to software.
// audioCh is the input audio channel count; AAC (mp4) is limited to 8
// channels, so higher counts are downmixed there (PCM in MOV keeps all).
func videoArgs(c Codec, vendor hwstat.Vendor, withAudio bool, audioCh int) []string {
	v := encoderArgs(c, vendor)

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

// swArgs returns the software encoder for a codec family.
func swArgs(fam string) []string {
	switch fam {
	case "hevc":
		return []string{"-c:v", "libx265", "-preset", "fast", "-crf", "23", "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
	case "av1":
		return []string{"-c:v", "libsvtav1", "-preset", "8", "-crf", "30", "-pix_fmt", "yuv420p"}
	default:
		return []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p"}
	}
}

// encoderArgs returns the -c:v arguments for a preset using the requested
// vendor. Non-hardware presets ignore vendor; hardware presets fall back to
// software when the requested encoder isn't in this ffmpeg build.
func encoderArgs(c Codec, vendor hwstat.Vendor) []string {
	switch baseID(c.ID) {
	case "h264":
		return h264Args(vendor)
	case "hevc":
		return hevcArgs(vendor)
	case "av1":
		return av1Args(vendor)
	case "prores":
		return []string{"-c:v", "prores_ks", "-profile:v", "3", "-vendor", "apl0", "-pix_fmt", "yuv422p10le"}
	case "dnxhr":
		return []string{"-c:v", "dnxhd", "-profile:v", "dnxhr_hq", "-pix_fmt", "yuv422p"}
	case "mjpeg":
		return []string{"-c:v", "mjpeg", "-q:v", "3", "-pix_fmt", "yuvj422p"}
	default: // h264_sw and anything unknown
		return swArgs("h264")
	}
}

func h264Args(vendor hwstat.Vendor) []string {
	switch vendor {
	case hwstat.VendorNVENC:
		if hasEncoder("h264_nvenc") {
			return []string{"-c:v", "h264_nvenc", "-preset", "p5", "-rc", "vbr", "-cq", "21", "-b:v", "0", "-pix_fmt", "yuv420p"}
		}
	case hwstat.VendorIntel:
		if hasEncoder("h264_qsv") {
			return []string{"-c:v", "h264_qsv", "-global_quality", "21", "-pix_fmt", "nv12"}
		}
	case hwstat.VendorAMD:
		if hasEncoder("h264_amf") {
			return []string{"-c:v", "h264_amf", "-quality", "quality", "-rc", "cqp", "-qp_i", "21", "-qp_p", "23", "-pix_fmt", "yuv420p"}
		}
	}
	return swArgs("h264")
}

func hevcArgs(vendor hwstat.Vendor) []string {
	switch vendor {
	case hwstat.VendorNVENC:
		if hasEncoder("hevc_nvenc") {
			return []string{"-c:v", "hevc_nvenc", "-preset", "p5", "-rc", "vbr", "-cq", "23", "-b:v", "0", "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
		}
	case hwstat.VendorIntel:
		if hasEncoder("hevc_qsv") {
			return []string{"-c:v", "hevc_qsv", "-global_quality", "23", "-pix_fmt", "nv12", "-tag:v", "hvc1"}
		}
	case hwstat.VendorAMD:
		if hasEncoder("hevc_amf") {
			return []string{"-c:v", "hevc_amf", "-quality", "quality", "-rc", "cqp", "-qp_i", "23", "-qp_p", "25", "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
		}
	}
	return swArgs("hevc")
}

func av1Args(vendor hwstat.Vendor) []string {
	switch vendor {
	case hwstat.VendorNVENC:
		if hasEncoder("av1_nvenc") {
			return []string{"-c:v", "av1_nvenc", "-preset", "p5", "-rc", "vbr", "-cq", "27", "-b:v", "0", "-pix_fmt", "yuv420p"}
		}
	case hwstat.VendorIntel:
		if hasEncoder("av1_qsv") {
			return []string{"-c:v", "av1_qsv", "-global_quality", "27", "-pix_fmt", "nv12"}
		}
	case hwstat.VendorAMD:
		if hasEncoder("av1_amf") {
			return []string{"-c:v", "av1_amf", "-quality", "quality", "-rc", "cqp", "-qp_i", "27", "-qp_p", "29", "-pix_fmt", "yuv420p"}
		}
	}
	return swArgs("av1")
}
