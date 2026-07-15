//go:build !windows

package webstream

import "syscall"

// sysProcAttr is a no-op off Windows (the package is only used on Windows;
// this keeps `go test` runnable on other platforms).
func sysProcAttr() *syscall.SysProcAttr { return nil }
