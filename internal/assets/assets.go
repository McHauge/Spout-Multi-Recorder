// Package assets embeds static resources such as the application icon.
// Regenerate the icon with: go run ./tools/icongen
package assets

import (
	_ "embed"

	"fyne.io/fyne/v2"
)

//go:embed icon.png
var iconPNG []byte

// Icon is the application icon used for the window and taskbar.
var Icon = fyne.NewStaticResource("icon.png", iconPNG)
