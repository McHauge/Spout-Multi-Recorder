//go:build windows

package webstream

import "syscall"

// sysProcAttr hides the console window of spawned FFmpeg processes.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
