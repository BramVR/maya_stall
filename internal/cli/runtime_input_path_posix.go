//go:build !windows

package cli

import "os"

func isReparsePoint(os.FileInfo) bool { return false }
