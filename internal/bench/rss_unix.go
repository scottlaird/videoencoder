//go:build unix

package bench

import (
	"os"
	"runtime"
	"syscall"
)

func maxRSSKB(ps *os.ProcessState) int64 {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	// Darwin reports bytes; Linux and the BSDs report kilobytes.
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return int64(ru.Maxrss) / 1024
	}
	return int64(ru.Maxrss)
}
