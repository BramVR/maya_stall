//go:build windows

package cli

import (
	"os"
	"syscall"
)

const fileAttributeReparsePoint = 0x400

func isReparsePoint(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&fileAttributeReparsePoint != 0
}
