//go:build !unix

package bench

import "os/exec"

func killGroup(*exec.Cmd) {}
