//go:build !unix

package agentcli

import "os/exec"

// Outside unix there is no SIGTERM worth sending, so the ceiling degrades to a kill
// and the final `result` line is lost. The nightwatcher runs on Linux; this exists so
// the package still builds elsewhere.
func terminateGently(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func setProcessGroup(*exec.Cmd) {}
