# Spout Multi Recorder

Records **all [Spout](https://spout.zeal.co/) video streams** on a PC to disk simultaneously, embedding **one shared master audio track** (any input device, or speaker loopback — "what you hear") into every file.

Written in Go, with the [Spout2 SDK](https://github.com/leadedge/Spout2) (SpoutDX, vendored under `internal/spout/`) via cgo, WASAPI audio capture via [malgo](https://github.com/gen2brain/malgo), FFmpeg for encoding, and a [Fyne](https://fyne.io) desktop UI.

## Features

- Auto-discovers every Spout sender on the machine; each gets a live preview card.
- Records any number of channels at once (cap selectable via **Max channels**).
- One master audio source — microphone/line input or speaker loopback — muxed into *all* recordings, with a stereo VU meter.
- Codec selectable in the UI: H.264 / HEVC (NVENC, QuickSync or AMF hardware encoding picked automatically, x264/x265 fallback), ProRes 422 HQ, DNxHR HQ, MJPEG.
- Robust to senders dropping out: recording simply continues with **black frames** at constant framerate, and picks the stream back up when the sender returns (even at a new resolution — frames are centered/cropped).
- **Auto-record new senders**: while a session is running, any sender that appears is automatically armed and gets its own recording file (you can even hit Record with zero senders and let them join as they start up).
- Each stream records at its native resolution. Files are named `<sender>_<timestamp>.<ext>`.

## Runtime requirements

- Windows 10/11 x64, DirectX 11 capable GPU.
- `ffmpeg.exe` — place it next to `SpoutMultiRecorder.exe` or anywhere on `PATH` (e.g. from <https://www.gyan.dev/ffmpeg/builds/>, the "essentials" build is fine).

## Building

1. Install [Go](https://go.dev/dl/) 1.22 or later.
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

## Using it

1. Start the app. Any running Spout senders appear as preview cards within a second (test with the *Spout Demo Sender* from the [Spout distribution](https://spout.zeal.co/), OBS with the Spout2 plugin, Resolume, TouchDesigner, …).
2. Pick the **Audio** source — entries marked 🔊 are speaker loopback ("what you hear"), 🎤 are inputs. The VU meter confirms signal.
3. Pick **Codec**, **FPS**, **Max channels** and the output folder.
4. Tick **record** on the channels you want (new senders auto-arm up to the max), press **Record**, later press **Stop**. Every armed channel becomes its own file with the same audio.

Settings persist in `%APPDATA%\SpoutMultiRecorder\config.json`; a log is written next to it.

## Notes & limitations

- Float-format senders (e.g. RGBA16F/RGBA32F) are rare and not converted; 8-bit BGRA/RGBA senders (the default) are fully supported.
- If a sender changes resolution mid-recording the file keeps its original size; frames are centered (padded/cropped), since a video file cannot change resolution mid-stream.
- Audio/video both start when you press Record and stay aligned; extremely long sessions may accumulate a small drift (audio is clocked by the sound device, video by the wall clock).
- The Spout2 SDK sources in `internal/spout/` are BSD-licensed by Lynn Jarvis — see `internal/spout/SPOUT_LICENSE.txt`.
