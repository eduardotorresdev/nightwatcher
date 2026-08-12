package agentcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DefaultBinary is the Claude Code CLI. It is a native binary and does not invoke
// Node, so the runtime image only needs the binary itself.
const DefaultBinary = "claude"

const (
	// terminateGrace is how long the CLI has to finish after SIGTERM before it is
	// killed outright. It only needs enough time to flush the final `result` line —
	// which is the entire reason the ceiling is a SIGTERM and not a SIGKILL.
	terminateGrace = 10 * time.Second

	// maxLine caps one stream-json line. A Read tool result is a whole file, so the
	// 64 KiB default of bufio.Scanner would turn a large file into a truncated
	// stream and a perfectly good session into a malformed one.
	maxLine = 16 << 20
)

// errAborted is what cancels the process when the perimeter inspection rejects the
// session. It is distinct from the deadline so an abort is never reported as a
// timeout.
var errAborted = errors.New("sessão abortada")

// Runner raises the process that carries the Claude CLI and hands back everything it
// printed. It is the seam between the perimeter and the parsing: the
// container implementation is what production uses, and DockerRunner is the only
// place in this package that knows the word "docker".
//
// The direct implementation exists because it is what a developer runs locally and
// what makes the whole parsing side testable without a container.
type Runner interface {
	Run(ctx context.Context, spec Spec) (Outcome, error)
}

// Spec is one CLI invocation, described without saying how it will be contained.
type Spec struct {
	// Args is the CLI's own argv, after the binary name.
	Args []string

	// Env is the COMPLETE environment of the process, built from scratch. A Runner
	// may add the writable HOME its perimeter provides, and nothing else.
	Env map[string]string

	// WorkDir is the host path of the read-only clone. Empty for the Triagem, which
	// mounts no repository at all.
	WorkDir string

	// Stdin carries the prompt. It goes on stdin rather than in argv because the
	// text is written by an end user: argv is visible to every process on the box
	// through `ps`, and has a length limit a stack trace can reach.
	Stdin string

	// Timeout is the wall-clock ceiling. The CLI has no timeout flag, so
	// this is where the ceiling actually lives.
	Timeout time.Duration

	// OnLine, when set, receives each stdout line as it arrives; returning an error
	// terminates the session. This is what lets the perimeter check kill a run that
	// loaded an MCP server out of the cloned repository instead of merely noticing it
	// afterwards. The slice is only valid for the duration of the call.
	OnLine func(line []byte) error
}

// Outcome is everything the process left behind. A non-zero exit is NOT reported as
// an error: the CLI prints its failures on stdout as a well-formed result event, so
// the classification lives in the parser and not in the exit status.
type Outcome struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int

	// TimedOut says the ceiling fired and the process got a SIGTERM.
	TimedOut bool
	// Aborted says OnLine rejected the session.
	Aborted bool
	// ReadErr is a failure reading stdout — in practice a line past maxLine, which
	// leaves the stream truncated.
	ReadErr  error
	Duration time.Duration
}

// ExecRunner runs the CLI directly, with no container around it. This is the local
// development path and the test path. Production uses DockerRunner, because the CLI
// flags are hygiene and the containment is the container.
type ExecRunner struct {
	// Binary defaults to DefaultBinary.
	Binary string
}

func (r ExecRunner) Run(ctx context.Context, spec Spec) (Outcome, error) {
	env := cloneEnv(spec.Env)
	if env["HOME"] == "" {
		// The CLI writes .claude.json while it runs, so a read-only HOME breaks it
		// outright. A throwaway directory per invocation also means no
		// session state carries from one Ocorrência to the next — and the sessions
		// hold text written by end users.
		dir, err := os.MkdirTemp("", "nightwatcher-agent-")
		if err != nil {
			return Outcome{}, fmt.Errorf("não consegui criar um HOME gravável para a sessão: %w", err)
		}
		defer os.RemoveAll(dir)
		env["HOME"] = dir
	}
	bin := r.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	return runProcess(ctx, bin, spec.Args, env, spec.WorkDir, spec)
}

// runProcess is the one place a child process is created, shared by both runners.
// The binary, argv, environment and directory come as parameters because they are the
// four things the container translates; the Spec is along for what it does not touch —
// the prompt on stdin, the ceiling, and the line-by-line inspection.
func runProcess(ctx context.Context, bin string, args []string, env map[string]string, dir string, spec Spec) (Outcome, error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	// A separate cancellation layer so an abort by OnLine is distinguishable from
	// the deadline: both kill the process, but only one of them is a timeout.
	runCtx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = dir
	cmd.Env = envSlice(env)
	cmd.Stdin = strings.NewReader(spec.Stdin)

	// SIGTERM, not SIGKILL. exec.CommandContext defaults to Kill, which would throw
	// away the final `result` line — and that line is exactly what turns an
	// interruption into a recorded reason: it comes back well formed, with
	// terminal_reason "aborted_streaming". WaitDelay escalates to a kill if
	// the CLI ignores the signal.
	cmd.Cancel = func() error { return terminateGently(cmd) }
	cmd.WaitDelay = terminateGrace
	// Own process group, so a signal aimed at the daemon's terminal does not reach
	// the session sideways.
	setProcessGroup(cmd)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{}, fmt.Errorf("não consegui abrir a saída do CLI: %w", err)
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Outcome{}, fmt.Errorf("não consegui iniciar %s: %w", bin, err)
	}

	var stdoutBuf bytes.Buffer
	aborted := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		stdoutBuf.Write(line)
		stdoutBuf.WriteByte('\n')
		if spec.OnLine == nil || aborted {
			continue
		}
		if err := spec.OnLine(line); err != nil {
			aborted = true
			abort(errAborted)
		}
	}
	readErr := sc.Err()
	if readErr != nil {
		// Drain the rest, otherwise a process blocked writing into a full pipe would
		// never exit and Wait would never return.
		_, _ = io.Copy(io.Discard, stdout)
	}
	// A non-zero exit is expected on every failed run, so Wait's error is not the
	// signal here; the ProcessState is.
	_ = cmd.Wait()

	out := Outcome{
		Stdout:   stdoutBuf.Bytes(),
		Stderr:   stderrBuf.Bytes(),
		ExitCode: -1,
		Aborted:  aborted,
		TimedOut: errors.Is(context.Cause(runCtx), context.DeadlineExceeded),
		ReadErr:  readErr,
		Duration: time.Since(started),
	}
	if st := cmd.ProcessState; st != nil {
		out.ExitCode = st.ExitCode()
	}
	return out, nil
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// envSlice renders the environment deterministically, so two identical sessions
// produce identical process descriptions.
func envSlice(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
