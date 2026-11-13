//go:build unix

package bench

import (
	"os/exec"
	"syscall"
)

// killGroup makes cmd its own process group, and canceling its context
// kill the whole group: a wrapper like turbostat and the command it
// runs.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
