# Spout Multi Recorder

Records **all [Spout](https://spout.zeal.co/) video streams** on a PC — plus any **[NDI](https://ndi.video)® sources** on the network — to disk simultaneously, each stream to its own file, with flexible per-channel audio.

Written in Go, with the [Spout2 SDK](https://github.com/leadedge/Spout2) (SpoutDX, vendored under `internal/spout/`) via cgo, WASAPI audio capture via [malgo](https://github.com/gen2brain/malgo), FFmpeg for encoding, and a [Fyne](https://fyne.io) desktop UI.

![Spout Multi Recorder](docs/screenshot.png)

## Features

- Records any number of channels at once, each at its native resolution, to `<sender>_<timestamp>.<ext>`. Live preview cards with per-channel VU meters.
- Robust to sources dropping out: recording continues with **black frames** (and silence) at constant framerate, and picks the stream back up when the source returns — even at a new resolution (frames are centered/cropped).
- **Auto-record**: while a session is running, new Spout senders are automatically armed and get their own file. You can even hit Record with zero senders and let them join as they start up.
- Settings persist between runs (`%APPDATA%\SpoutMultiRecorder\config.json`, log next to it).

### Video sources

- **Spout**: every sender on the machine is discovered automatically and appears as a preview card within a second (up to **Max channels** are auto-armed).
- **NDI** — full-bandwidth NDI *and* NDI|HX: added manually via the **Add NDI** button, which lists the sources discovered on the network. Added sources are remembered across restarts and removable via the 🗑 button on the card. Receivers connect by source name, so they survive the sending application restarting (reconnects automatically, including on a new address). Requires the [NDI runtime](https://ndi.video/tools/) on the machine; HX decoding is handled by the runtime.

### Audio

- One **master audio source** — any input device or speaker loopback (🔊 "what you hear") — selectable in the toolbar, with a stereo VU meter.
- **Per-channel audio choice** via the **master audio** checkbox on each card:
  - *Checked* (default for Spout, which carries no audio): the master source is muxed into that channel's file.
  - *Unchecked* (default for NDI): NDI channels record their **native embedded NDI audio**; Spout channels record without an audio track.
- **Multichannel NDI audio**: the source's channel count is preserved — 2/4/8/16 channels (capped at 16), converted to 48 kHz s16 and silence-padded during dropouts so files never desync. The channel count locks when a recording starts; if the source changes mid-recording the stream is adapted (extra channels dropped, missing ones silent).
- Each card shows exactly what will be recorded (e.g. `2ch aac`, `8ch opus`, `16ch pcm`, `no audio`) plus a live per-channel VU meter beside the preview — one bar per audio channel for native NDI audio, a vertical mirror of the master meter when master audio is selected. No meter = no audio track.

### Codecs (selectable in the UI)

| Preset | Container | Audio | Notes |
|---|---|---|---|
| H.264 / HEVC (auto hardware) | MP4 | AAC | NVENC → QuickSync → AMF, x264/x265 fallback. AAC caps at 8 audio channels (auto-downmix). |
| H.264 / HEVC + Opus | MKV | Opus | Same video encoders; Opus carries the full channel count (~64 kbit/s per channel). |
| ProRes 422 HQ, DNxHR HQ, MJPEG | MOV | PCM | Editing-friendly; PCM keeps all channels uncompressed. |

## Runtime requirements

- Windows 10/11 x64, DirectX 11 capable GPU.
- `ffmpeg.exe` — next to `SpoutMultiRecorder.exe` or on `PATH` (e.g. `winget install ffmpeg`, or the [gyan.dev](https://www.gyan.dev/ffmpeg/builds/) "essentials" build).
- For NDI: the [NDI runtime](https://ndi.video/tools/) (bundled with NDI Tools). Optional — without it, only the NDI features are unavailable.

## Using it

1. Start the app. Running Spout senders appear automatically; add NDI sources with **Add NDI**.
2. Pick the **Audio** master source (🔊 = speaker loopback, 🎤 = input) — the VU meter confirms signal — and set the per-card **master audio** checkboxes as needed.
3. Pick **Codec**, **FPS**, **Max channels** and the output folder.
4. Tick **record** on the channels you want, press **Record**, later **Stop**. Every armed channel becomes its own file.

Good test senders: the *Spout Demo Sender* from the Spout distribution, OBS (Spout2 plugin or NDI output), Resolume, TouchDesigner.

## Building

1. Install [Go](https://go.dev/dl/) 1.26 or later (see `go.mod`).
2. Install a MinGW-w64 C/C++ toolchain (needed by cgo for the Spout SDK and audio):
   - Easiest: [MSYS2](https://www.msys2.org), then in the MSYS2 shell: `pacman -S mingw-w64-ucrt-x86_64-gcc`
   - Add `C:\msys64\ucrt64\bin` to your `PATH`.
3. Build:

```powershell
.\build.ps1
```

or manually:

```powershell
$env:CGO_ENABLED="1"; go build -ldflags "-H windowsgui -s -w" -o dist\SpoutMultiRecorder.exe .
```

Note for LLVM/clang MinGW toolchains (llvm-mingw): the cgo link flags reference `-lstdc++`; either use a GCC-based MinGW build, or copy `libc++.a` to `libstdc++.a` in the toolchain's `lib` directory.

## Releases (CI)

Releases are built by [GoReleaser](https://goreleaser.com) via GitHub Actions (`.github/workflows/release.yml`): cross-compiled from Ubuntu with the [llvm-mingw](https://github.com/mstorsjo/llvm-mingw) toolchain, statically linked, and published as a zip with checksums.

To cut a release:

```bash
git tag v1.0.0
git push origin v1.0.0
```

Every push/PR also runs a cross-build + tests via `.github/workflows/ci.yml`. For a local dry run (Linux/WSL with llvm-mingw on PATH): `goreleaser release --snapshot --clean`.

## App icon

The icon is generated by `go run ./tools/icongen` (writes `internal/assets/icon.png`, embedded as the Fyne window/taskbar icon). The exe icon, version properties (company, copyright, product name — see `winres/winres.json`) and application manifest come from a `.syso` resource that GoReleaser generates at release time, stamped with the tag version (see `.goreleaser.yaml`). It is not committed; for a local build with the icon/metadata, generate it once:

```bash
go install github.com/tc-hib/go-winres@latest
go-winres make --in winres/winres.json --arch amd64
```

## Notes & limitations

- Float-format Spout senders (e.g. RGBA16F/RGBA32F) are rare and not converted; 8-bit BGRA/RGBA senders (the default) are fully supported.
- If a source changes resolution mid-recording the file keeps its original size; frames are centered (padded/cropped), since a video file cannot change resolution mid-stream.
- Audio/video both start when you press Record and stay aligned; extremely long sessions may accumulate a small drift (audio is clocked by the source, video by the wall clock). NDI audio at rates other than 48 kHz is resampled with a simple nearest-sample pass.
- The Spout2 SDK sources in `internal/spout/` are BSD-licensed by Lynn Jarvis — see `internal/spout/SPOUT_LICENSE.txt`.
- NDI® is a registered trademark of Vizrt NDI AB. The app loads the NDI runtime DLL dynamically at runtime (nothing NDI-related is compiled in or redistributed); without the runtime installed, NDI features are simply unavailable.
