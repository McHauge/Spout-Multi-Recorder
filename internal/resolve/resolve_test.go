package resolve

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimecode(t *testing.T) {
	cases := []struct {
		frames int64
		fps    int
		want   string
	}{
		{0, 30, "00:00:00:00"},
		{29, 30, "00:00:00:29"},
		{30, 30, "00:00:01:00"},
		{int64((13*3600+45+0)*30) + 0, 30, "13:00:45:00"},
		{int64(24*3600*25) + 5, 25, "00:00:00:05"}, // wraps at 24h
	}
	for _, c := range cases {
		if got := Timecode(c.frames, c.fps); got != c.want {
			t.Errorf("Timecode(%d, %d) = %q, want %q", c.frames, c.fps, got, c.want)
		}
	}
}

func TestFramesSinceMidnightRoundTrip(t *testing.T) {
	loc := time.Local
	tm := time.Date(2026, 7, 10, 13, 45, 14, int(23*time.Second/30)+1, loc)
	f := FramesSinceMidnight(tm, 30)
	if got, want := Timecode(f, 30), "13:45:14:23"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func testClips() []Clip {
	return []Clip{
		{Name: "OBS Studio", Path: `D:\rec\2026\OBS_Studio_2026-07-10.mp4`, W: 1920, H: 1080,
			StartFrames: 30 * (13*3600 + 45*60 + 14), DurFrames: 30 * 60, AudioCh: 2},
		{Name: "Resolume & Co", Path: `D:\rec\2026\Resolume_2026-07-10.mp4`, W: 1280, H: 720,
			StartFrames: 30*(13*3600+45*60+14) + 45, DurFrames: 30*60 - 45, AudioCh: 0},
	}
}

func TestWriteDRP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.drp")
	if err := WriteDRP(path, "session", 30, testClips()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (header + stop event), got %d", len(lines))
	}

	var hdr map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header not valid JSON: %v", err)
	}
	if hdr["masterTimecode"] != "13:45:14:00" {
		t.Errorf("masterTimecode = %v", hdr["masterTimecode"])
	}
	if hdr["videoMode"] != "1080p30" {
		t.Errorf("videoMode = %v", hdr["videoMode"])
	}
	srcs := hdr["sources"].([]any)
	if len(srcs) != 2+5 { // black + 2 clips + bars/color1/color2/media player
		t.Fatalf("got %d sources", len(srcs))
	}
	cam1 := srcs[1].(map[string]any)
	if cam1["file"] != "OBS_Studio_2026-07-10.mp4" {
		t.Errorf("cam1 file = %v", cam1["file"])
	}
	if cam1["startTimecode"] != "13:45:14:00" {
		t.Errorf("cam1 startTimecode = %v", cam1["startTimecode"])
	}
	cam2 := srcs[2].(map[string]any)
	if cam2["startTimecode"] != "13:45:15:15" {
		t.Errorf("cam2 startTimecode = %v", cam2["startTimecode"])
	}
	if cam2["_index_"].(float64) != 2 {
		t.Errorf("cam2 index = %v", cam2["_index_"])
	}

	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatalf("event not valid JSON: %v", err)
	}
	if ev["masterTimecode"] != "13:46:14:00" { // start + 60 s
		t.Errorf("stop event timecode = %v", ev["masterTimecode"])
	}
	meb := ev["mixEffectBlocks"].([]any)[0].(map[string]any)
	if meb["onAir"] != false {
		t.Errorf("stop event onAir = %v", meb["onAir"])
	}
	if _, ok := meb["source"]; ok {
		t.Error("stop event should not contain a source switch")
	}
}

func TestWriteFCPXML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.fcpxml")
	if err := WriteFCPXML(path, "session <&>", 30, testClips()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Must be well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("XML not well-formed: %v", err)
		}
	}

	s := string(data)
	for _, want := range []string{
		`<fcpxml version="1.8">`,
		`frameDuration="1/30s" width="1920" height="1080"`,
		`frameDuration="1/30s" width="1280" height="720"`,
		`audioChannels="2"`,
		`src="file:///D:/rec/2026/OBS_Studio_2026-07-10.mp4"`,
		`<multicam format="r1" tcStart="0s" tcFormat="NDF">`,
		`offset="0s"`,     // first angle at session start
		`offset="45/30s"`, // second angle 45 frames later
		`<mc-clip ref="mc1"`,
		`duration="1800/30s"`, // 60 s session
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(s, "session <&>") {
		t.Error("project name not XML-escaped")
	}
}

func TestWriteProject(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProject(dir, "2026-07-10_13-45-14", 30, testClips()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"2026-07-10_13-45-14_ATEM_Project.drp", "2026-07-10_13-45-14_MultiCam_Timeline.xml", "2026-07-10_13-45-14_MultiCam_Clip.fcpxml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if err := WriteProject(dir, "empty", 30, nil); err == nil {
		t.Error("want error for empty clip list")
	}
}

func TestWriteXMEML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.xml")
	if err := WriteXMEML(path, "session", 30, testClips()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("XML not well-formed: %v", err)
		}
	}

	s := string(data)
	if n := strings.Count(s, "<track>"); n != 3 { // 2 video + 1 audio (clip 2 has no audio)
		t.Errorf("got %d tracks, want 3", n)
	}
	for _, want := range []string{
		`<string>13:45:14:00</string>`, // sequence + first file timecode
		`<string>13:45:15:15</string>`, // second file timecode
		`<start>0</start>`,
		`<start>45</start>`,
		`<pathurl>file://localhost/D:/rec/2026/OBS_Studio_2026-07-10.mp4</pathurl>`,
		`<channelcount>2</channelcount>`,
		`<width>1280</width>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q", want)
		}
	}
}
