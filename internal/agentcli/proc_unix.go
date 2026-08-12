//go:build unix

package agentcli

import (
	"os/exec"
	"syscall"
)

// terminateGently asks the CLI to stop the way the documented interruption path
// expects: SIGTERM aborts the turn, runs SessionEnd hooks, prints the final `result`
// line and exits 143.
func terminateGently(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGTERM)
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
