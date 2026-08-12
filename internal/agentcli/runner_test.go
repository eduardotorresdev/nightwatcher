package agentcli

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("os testes de processo usam /bin/sh")
	}
}

// The CLI writes .claude.json while it runs, so a HOME that is missing or read-only
// breaks the session outright. The runner is what guarantees there is a writable one.
func TestExecRunnerProvidesAWritableHome(t *testing.T) {
	skipWithoutShell(t)
	r := ExecRunner{Binary: "/bin/sh"}

	out, err := r.Run(context.Background(), Spec{
		Args:    []string{"-c", `test -w "$HOME" && echo gravavel && echo "$HOME"`},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(out.Stdout), "gravavel") {
		t.Fatalf("HOME não era gravável; saída: %q, stderr: %q", out.Stdout, out.Stderr)
	}
	if out.ExitCode != 0 {
		t.Errorf("saída %d", out.ExitCode)
	}
}

// The money bug, at the level where it would actually happen: the child process must
// not see ANTHROPIC_API_KEY even when the daemon's own environment has one.
func TestExecRunnerDoesNotInheritTheParentEnvironment(t *testing.T) {
	skipWithoutShell(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-cobraria-por-token")
	r := ExecRunner{Binary: "/bin/sh"}

	out, err := r.Run(context.Background(), Spec{
		Args:    []string{"-c", "env"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin", "CLAUDE_CODE_OAUTH_TOKEN": "token-da-assinatura"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(string(out.Stdout), "ANTHROPIC_API_KEY") {
		t.Fatalf("a chave de API vazou para a sessão:\n%s", out.Stdout)
	}
	if !strings.Contains(string(out.Stdout), "CLAUDE_CODE_OAUTH_TOKEN=token-da-assinatura") {
		t.Errorf("o token da assinatura não chegou:\n%s", out.Stdout)
	}
}

// The ceiling is a SIGTERM and not a SIGKILL, and the difference is visible: the CLI
// still flushes its final `result` line on the way out, which is what turns an
// interruption into a recorded reason instead of a silence.
func TestCeilingTerminatesGentlySoTheFinalLineSurvives(t *testing.T) {
	skipWithoutShell(t)
	// The background sleep gets its own stdout: otherwise it would keep the pipe open
	// after the shell exits and the read would hang until it finished.
	const script = `
trap 'echo "{\"type\":\"result\",\"terminal_reason\":\"aborted_streaming\"}"; exit 143' TERM
echo '{"type":"system","subtype":"init"}'
sleep 30 >/dev/null 2>&1 &
wait`

	r := ExecRunner{Binary: "/bin/sh"}
	started := time.Now()
	out, err := r.Run(context.Background(), Spec{
		Args:    []string{"-c", script},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("o teto não interrompeu nada: %s", elapsed)
	}
	if !out.TimedOut {
		t.Error("o teto de tempo não foi registrado")
	}
	if out.ExitCode != 143 {
		t.Errorf("saída %d; 143 é o que prova o SIGTERM — com SIGKILL o trap nem roda", out.ExitCode)
	}
	if !strings.Contains(string(out.Stdout), "aborted_streaming") {
		t.Errorf("a última linha do stream se perdeu:\n%s", out.Stdout)
	}
}

// OnLine is what makes the perimeter inspection a gate: rejecting the first line has
// to end the session, not merely be noted afterwards.
func TestOnLineErrorAbortsTheSession(t *testing.T) {
	skipWithoutShell(t)
	const script = `
echo '{"type":"system","subtype":"init"}'
sleep 30 >/dev/null 2>&1 &
wait`

	r := ExecRunner{Binary: "/bin/sh"}
	seen := 0
	started := time.Now()
	out, err := r.Run(context.Background(), Spec{
		Args:    []string{"-c", script},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 30 * time.Second,
		OnLine: func([]byte) error {
			seen++
			return errors.New("perímetro violado")
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("a sessão não foi abortada: %s", elapsed)
	}
	if seen != 1 {
		t.Errorf("linhas entregues depois do aborto: %d", seen)
	}
	if !out.Aborted {
		t.Error("o aborto não foi registrado")
	}
	if out.TimedOut {
		t.Error("um aborto foi reportado como estouro de tempo")
	}
}

// The CLI prints its failures on stdout and exits non-zero. That is not an error from
// the runner's point of view — the reason is in the stream, and throwing it away
// would leave the failure issue with nothing to say.
func TestNonZeroExitIsNotARunnerError(t *testing.T) {
	skipWithoutShell(t)
	r := ExecRunner{Binary: "/bin/sh"}

	out, err := r.Run(context.Background(), Spec{
		Args:    []string{"-c", `echo '{"type":"result","is_error":true}'; exit 1`},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("saída não-zero virou erro do runner: %v", err)
	}
	if out.ExitCode != 1 {
		t.Errorf("saída %d", out.ExitCode)
	}
	if !strings.Contains(string(out.Stdout), "is_error") {
		t.Errorf("stdout perdido: %q", out.Stdout)
	}
}

func TestRunnerReportsABinaryThatDoesNotExist(t *testing.T) {
	r := ExecRunner{Binary: "/nao/existe/claude"}
	if _, err := r.Run(context.Background(), Spec{Timeout: time.Second}); err == nil {
		t.Fatal("esperava erro ao iniciar")
	}
}
