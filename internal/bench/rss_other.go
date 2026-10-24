//go:build !unix

package bench

import "os"

func maxRSSKB(*os.ProcessState) int64 { return 0 }
