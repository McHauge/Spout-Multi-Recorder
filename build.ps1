# Builds SpoutMultiRecorder.exe
# Requires: Go 1.22+, a MinGW-w64 gcc/g++ toolchain on PATH (e.g. via
# https://www.msys2.org : pacman -S mingw-w64-ucrt-x86_64-gcc, then add
# C:\msys64\ucrt64\bin to PATH), and ffmpeg.exe at runtime.

$ErrorActionPreference = "Stop"

$env:CGO_ENABLED = "1"
$env:GOOS = "windows"
$env:GOARCH = "amd64"

New-Item -ItemType Directory -Force -Path dist | Out-Null

$version = (git describe --tags --always 2>$null) -replace '^v', ''
if (-not $version) { $version = "dev" }

go build -ldflags "-H windowsgui -s -w -X main.version=$version -extldflags '-static'" -o dist\SpoutMultiRecorder.exe .

Write-Host "Built dist\SpoutMultiRecorder.exe"
Write-Host "Remember to place ffmpeg.exe next to it or on PATH."
