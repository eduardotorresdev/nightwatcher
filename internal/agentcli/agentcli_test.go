package agentcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

type fakeRunner struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	timedOut bool
	err      error

	spec  Spec
	calls int
	fed   int
}

func (f *fakeRunner) Run(_ context.Context, spec Spec) (Outcome, error) {
	f.calls++
	f.spec = spec
	f.fed = 0
	if f.err != nil {
		return Outcome{}, f.err
	}
	// Feed the stream the way a real process does, so the perimeter inspection is
	// exercised and an abort actually stops the reading.
	if spec.OnLine != nil {
		sc := bufio.NewScanner(bytes.NewReader(f.stdout))
		for sc.Scan() {
			f.fed++
			if err := spec.OnLine(sc.Bytes()); err != nil {
				break
			}
		}
	}
	return Outcome{
		Stdout:   f.stdout,
		Stderr:   f.stderr,
		ExitCode: f.exitCode,
		TimedOut: f.timedOut,
		Duration: 3 * time.Second,
	}, nil
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func deepRequest() domain.AgentRequest {
	return domain.AgentRequest{
		Kind:    domain.SessionDiagnosis,
		Prompt:  "prompt de diagnóstico",
		WorkDir: "/data/clones/acme",
		Timeout: 20 * time.Minute,
	}
}

// The whole classification lives or dies here: what the session printed decides what
// the failure issue will say, and `subtype: "success"` is a lie on a 401.
func TestSessionOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		fixture    string // empty means the process printed nothing at all
		stderr     string
		exit       int
		timedOut   bool
		want       domain.AgentErrorKind // empty means the session succeeded
		wantRetry  bool                  // what the worker will do about it
		wantDetail string                // what the failure issue has to be able to show
	}{
		{name: "sucesso", fixture: "sucesso.jsonl"},
		// The headline trap: is_error is the signal, subtype says "success" anyway.
		{name: "401 com subtype success", fixture: "auth_401.jsonl", exit: 1, want: domain.AgentErrAuth},
		{name: "limite de uso pelo api_retry", fixture: "limite_de_uso.jsonl", exit: 1, want: domain.AgentErrRateLimit, wantRetry: true},
		{name: "serviço sobrecarregado", fixture: "sobrecarga.jsonl", exit: 1, want: domain.AgentErrUnavailable, wantRetry: true},
		{name: "SIGTERM do teto de tempo", fixture: "interrompida.jsonl", exit: 143, timedOut: true, want: domain.AgentErrTimeout, wantRetry: true},
		{name: "teto de turnos", fixture: "teto_de_turnos.jsonl", exit: 1, want: domain.AgentErrTimeout, wantRetry: true},
		// A broken reply is never retried: repeating it produces the same broken reply.
		{name: "stream truncado", fixture: "truncada.jsonl", exit: 1, want: domain.AgentErrMalformed},
		{name: "lixo na saída", fixture: "lixo.txt", stderr: "error: unknown option '--tools'", exit: 2, want: domain.AgentErrMalformed, wantDetail: "unknown option"},
		// Nothing printed at all is infrastructure, not a reply — and it has to be
		// worth waiting out, or a docker daemon restart marks Ocorrências unanalysed.
		{name: "o container nem subiu", stderr: "docker: Cannot connect to the Docker daemon at unix:///var/run/docker.sock.", exit: 125, want: domain.AgentErrUnavailable, wantRetry: true, wantDetail: "Cannot connect to the Docker daemon"},
		{name: "MCP vazado do repositório", fixture: "mcp_vazado.jsonl", want: domain.AgentErrPerimeter},
		{name: "autenticou por API key", fixture: "api_key_sequestrada.jsonl", want: domain.AgentErrPerimeter},
		{name: "sessão não se declarou", fixture: "sem_init.jsonl", want: domain.AgentErrPerimeter},
		// A session that failed has no answer to protect: the reason it failed is
		// worth more on the issue than the observation that it never declared itself.
		{name: "morreu antes de se declarar", fixture: "sem_init_401.jsonl", exit: 1, want: domain.AgentErrAuth, wantDetail: "401"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout []byte
			if tc.fixture != "" {
				stdout = fixture(t, tc.fixture)
			}
			f := &fakeRunner{
				stdout:   stdout,
				stderr:   []byte(tc.stderr),
				exitCode: tc.exit,
				timedOut: tc.timedOut,
			}
			a := New(Options{Runner: f, Token: "tok", Clock: &fakeClock{now: time.Unix(0, 0)}})

			res, err := a.Run(context.Background(), deepRequest())

			if tc.want == "" {
				if err != nil {
					t.Fatalf("esperava sucesso, veio: %v", err)
				}
				if !strings.Contains(res.Text, "causa_provavel") {
					t.Errorf("texto da resposta perdido: %q", res.Text)
				}
				if res.CostUSD != 0.42 || res.NumTurns != 4 {
					t.Errorf("custo/turnos: %v / %d", res.CostUSD, res.NumTurns)
				}
				return
			}

			if err == nil {
				t.Fatalf("esperava falha %q, veio sucesso com %q", tc.want, res.Text)
			}
			var ae *domain.AgentError
			if !errors.As(err, &ae) {
				t.Fatalf("erro não é *domain.AgentError: %T", err)
			}
			if ae.Kind != tc.want {
				t.Errorf("classificou como %q, esperava %q (detalhe: %s)", ae.Kind, tc.want, ae.Detail)
			}
			// The classification only matters through this: whether the Ocorrência is
			// tried again or goes straight to a failure issue.
			if got := ae.Kind.Retryable(); got != tc.wantRetry {
				t.Errorf("retentável = %v, esperava %v (%q)", got, tc.wantRetry, ae.Kind)
			}
			if ae.Detail == "" {
				t.Error("falha sem detalhe: a issue de falha precisa mostrar o motivo")
			}
			if tc.wantDetail != "" && !strings.Contains(ae.Detail, tc.wantDetail) {
				t.Errorf("detalhe %q não contém %q", ae.Detail, tc.wantDetail)
			}
			if res.Text != "" {
				t.Errorf("falha devolveu texto: %q", res.Text)
			}
		})
	}
}

// A session that loaded something it should not have is stopped at the init event,
// before the untrusted text ever gets answered.
func TestPerimeterViolationStopsTheSessionAtTheInitEvent(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "mcp_vazado.jsonl")}
	a := New(Options{Runner: f, Token: "tok"})

	_, err := a.Run(context.Background(), deepRequest())
	if domain.ClassifyAgentError(err) != domain.AgentErrPerimeter {
		t.Fatalf("classificou como %q", domain.ClassifyAgentError(err))
	}
	if f.fed != 1 {
		t.Errorf("a sessão leu %d linhas; deveria ter parado na primeira", f.fed)
	}
	if domain.ClassifyAgentError(err).Retryable() {
		t.Error("violação de perímetro não pode ser retentada")
	}
}

// The CLI never says when the subscription resets, so the nightwatcher stops
// dispatching on its own instead of spending the next Ocorrência finding the same wall.
func TestUsageLimitOpensTheBreakerUntilTheCooldownPasses(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	f := &fakeRunner{stdout: fixture(t, "limite_de_uso.jsonl"), exitCode: 1}
	a := New(Options{Runner: f, Token: "tok", Clock: clock, RateLimitCooldown: time.Hour})

	if _, err := a.Run(context.Background(), deepRequest()); domain.ClassifyAgentError(err) != domain.AgentErrRateLimit {
		t.Fatalf("primeira sessão: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("chamadas: %d", f.calls)
	}

	_, err := a.Run(context.Background(), deepRequest())
	if domain.ClassifyAgentError(err) != domain.AgentErrRateLimit {
		t.Errorf("com o disjuntor aberto esperava %q, veio %v", domain.AgentErrRateLimit, err)
	}
	if f.calls != 1 {
		t.Errorf("o disjuntor deixou passar: %d chamadas", f.calls)
	}
	if !domain.ClassifyAgentError(err).Retryable() {
		t.Error("limite de uso tem que valer a pena esperar")
	}

	clock.now = clock.now.Add(61 * time.Minute)
	if _, err := a.Run(context.Background(), deepRequest()); err == nil {
		t.Fatal("esperava a falha da própria sessão")
	}
	if f.calls != 2 {
		t.Errorf("passado o resfriamento a sessão deveria voltar a ser despachada: %d chamadas", f.calls)
	}
}

// --allowed-tools waives the confirmation prompt and restricts nothing. Using it as
// the allowlist is the mistake the PRD corrected, and it is invisible until a session
// runs a command nobody sanctioned.
func TestArgvRestrictsTheToolUniverseWithTools(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "sucesso.jsonl")}
	a := New(Options{Runner: f, Token: "tok", SystemPrompt: "regras", MaxTurns: 25, MaxBudgetUSD: 2})

	if _, err := a.Run(context.Background(), deepRequest()); err != nil {
		t.Fatalf("run: %v", err)
	}
	args := f.spec.Args

	if v, ok := flagValue(args, "--tools"); !ok || v != "Read,Grep,Glob" {
		t.Errorf("--tools = %q (presente: %v)", v, ok)
	}
	for _, forbidden := range []string{
		"--allowed-tools", "--allowedTools",
		"--bare",
		"--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
	} {
		if hasArg(args, forbidden) {
			t.Errorf("argv contém %s", forbidden)
		}
	}
	if v, _ := flagValue(args, "--output-format"); v != "stream-json" {
		t.Errorf("--output-format = %q; o api_retry só existe no stream", v)
	}
	if v, _ := flagValue(args, "--permission-mode"); v != permissionMode {
		t.Errorf("--permission-mode = %q", v)
	}
	if v, ok := flagValue(args, "--setting-sources"); !ok || v != "" {
		t.Errorf("--setting-sources = %q (presente: %v)", v, ok)
	}
	for _, want := range []string{"--print", "--verbose", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"} {
		if !hasArg(args, want) {
			t.Errorf("argv não contém %s", want)
		}
	}
	if v, _ := flagValue(args, "--disallowed-tools"); !strings.Contains(v, "Bash") || !strings.Contains(v, "mcp__*") {
		t.Errorf("--disallowed-tools = %q", v)
	}
	if v, _ := flagValue(args, "--settings"); !json.Valid([]byte(v)) {
		t.Errorf("--settings não é JSON válido: %s", v)
	}
	if v, _ := flagValue(args, "--append-system-prompt"); v != "regras" {
		t.Errorf("--append-system-prompt = %q", v)
	}
	// The ceilings the CLI itself understands. There is no timeout flag, so
	// these are the only ones it enforces from the inside.
	if v, ok := flagValue(args, "--max-turns"); !ok || v != "25" {
		t.Errorf("--max-turns = %q (presente: %v)", v, ok)
	}
	if v, ok := flagValue(args, "--max-budget-usd"); !ok || v != "2.00" {
		t.Errorf("--max-budget-usd = %q (presente: %v)", v, ok)
	}

	// The prompt is written by an end user: it goes on stdin, where `ps` cannot read
	// it and where its length is not an argv limit.
	if f.spec.Stdin != "prompt de diagnóstico" {
		t.Errorf("prompt não foi para o stdin: %q", f.spec.Stdin)
	}
	if strings.Contains(strings.Join(args, " "), "prompt de diagnóstico") {
		t.Error("o prompt vazou para o argv")
	}
}

// The short session mounts nothing and gets no tools at all: dúvida and ruído have to
// be cheap to discard.
func TestTriageSessionGetsNoToolsAndNoRepository(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "triagem.jsonl")}
	a := New(Options{Runner: f, Token: "tok", MaxTurns: 25})

	// The fixture declares `tools: []`, which is what the Triagem asked for: an empty
	// universe has to be accepted by the perimeter check, not read as a violation.
	res, err := a.Run(context.Background(), domain.AgentRequest{
		Kind:    domain.SessionTriage,
		Prompt:  "triagem",
		Timeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("a triagem sem ferramenta nenhuma falhou: %v", err)
	}
	if !strings.Contains(res.Text, "defeito") {
		t.Errorf("resposta da triagem perdida: %q", res.Text)
	}

	if v, ok := flagValue(f.spec.Args, "--tools"); !ok || v != "" {
		t.Errorf(`--tools = %q, esperava ""`, v)
	}
	if f.spec.WorkDir != "" {
		t.Errorf("a triagem recebeu diretório de trabalho: %q", f.spec.WorkDir)
	}
	if v, _ := flagValue(f.spec.Args, "--max-turns"); v != "3" {
		t.Errorf("--max-turns = %q; a sessão curta não tem ferramenta para gastar turno", v)
	}
	if f.spec.Timeout != 2*time.Minute {
		t.Errorf("teto de tempo = %s", f.spec.Timeout)
	}
}

// ANTHROPIC_API_KEY outranks the subscription token and bills per token, silently.
// The environment is built from scratch precisely so a stray variable on the host
// cannot hijack the credential.
func TestEnvIsBuiltFromScratchAndNeverCarriesTheAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-cobraria-por-token")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "outro-que-tambem-ganha")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")

	f := &fakeRunner{stdout: fixture(t, "sucesso.jsonl")}
	a := New(Options{Runner: f, Token: "token-da-assinatura"})
	if _, err := a.Run(context.Background(), deepRequest()); err != nil {
		t.Fatalf("run: %v", err)
	}

	allowed := map[string]bool{"HOME": true, "PATH": true, "CLAUDE_CODE_OAUTH_TOKEN": true}
	for k := range f.spec.Env {
		if !allowed[k] {
			t.Errorf("variável inesperada no ambiente da sessão: %s", k)
		}
	}
	if f.spec.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "token-da-assinatura" {
		t.Errorf("token da assinatura não chegou: %q", f.spec.Env["CLAUDE_CODE_OAUTH_TOKEN"])
	}
}

// A daemon shutting down mid-session is not a malformed reply. Getting this wrong
// turns every deploy into a batch of unanalysed issues.
func TestCancelledSessionIsRetryable(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "truncada.jsonl")}
	a := New(Options{Runner: f, Token: "tok"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.Run(ctx, deepRequest())
	kind := domain.ClassifyAgentError(err)
	if !kind.Retryable() {
		t.Errorf("cancelamento classificado como %q, que não é retentável", kind)
	}
}

// A CLI that is not installed, or a docker daemon that is down, is the service being
// out — worth waiting for, not worth turning into a failure issue on the first try.
func TestRunnerThatCannotStartIsUnavailable(t *testing.T) {
	f := &fakeRunner{err: errors.New("exec: \"claude\": executable file not found in $PATH")}
	a := New(Options{Runner: f, Token: "tok"})

	_, err := a.Run(context.Background(), deepRequest())
	if kind := domain.ClassifyAgentError(err); kind != domain.AgentErrUnavailable {
		t.Errorf("classificou como %q", kind)
	}
	if !domain.ClassifyAgentError(err).Retryable() {
		t.Error("serviço fora do ar vale a pena esperar")
	}
}

func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
