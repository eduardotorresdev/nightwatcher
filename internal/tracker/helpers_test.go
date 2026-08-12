package tracker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// gqlRequest é uma chamada que o Linear falso recebeu.
type gqlRequest struct {
	Op     string
	Query  string
	Vars   map[string]any
	Header http.Header
}

// input devolve o mapa `input` das variáveis — a forma que quase toda mutation usa.
func (r gqlRequest) input(t *testing.T) map[string]any {
	t.Helper()
	in, ok := r.Vars["input"].(map[string]any)
	if !ok {
		t.Fatalf("%s: variáveis sem `input`: %v", r.Op, r.Vars)
	}
	return in
}

type responder func(w http.ResponseWriter, req gqlRequest)

// fakeLinear é a API do Linear em memória, roteando por operação. As respostas têm a
// forma gravada na pesquisa, e não uma forma inventada.
type fakeLinear struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []gqlRequest
	handlers map[string]responder
}

func newFakeLinear(t *testing.T) *fakeLinear {
	t.Helper()
	f := &fakeLinear{t: t, handlers: map[string]responder{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		f.t.Errorf("corpo da requisição não é JSON: %v", err)
	}
	req := gqlRequest{Op: opName(parsed.Query), Query: parsed.Query, Vars: parsed.Variables, Header: r.Header.Clone()}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	h, ok := f.handlers[req.Op]
	f.mu.Unlock()

	if !ok {
		f.t.Errorf("operação inesperada %q", req.Op)
		writeData(w, `{}`)
		return
	}
	h(w, req)
}

func (f *fakeLinear) on(op string, r responder) { f.handlers[op] = r }

// onData responde sempre o mesmo `data`.
func (f *fakeLinear) onData(op, data string) {
	f.on(op, func(w http.ResponseWriter, _ gqlRequest) { writeData(w, data) })
}

func (f *fakeLinear) got(op string) []gqlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gqlRequest
	for _, r := range f.requests {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

// only exige exatamente uma chamada da operação e devolve.
func (f *fakeLinear) only(op string) gqlRequest {
	f.t.Helper()
	got := f.got(op)
	if len(got) != 1 {
		f.t.Fatalf("esperava 1 chamada de %s, houve %d", op, len(got))
	}
	return got[0]
}

func (f *fakeLinear) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// opName extrai "IssueCreate" de "mutation IssueCreate($input: ...)".
func opName(query string) string {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) < 2 {
		return ""
	}
	name := fields[1]
	if i := strings.IndexAny(name, "({"); i >= 0 {
		name = name[:i]
	}
	return name
}

func writeData(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"data": %s}`, data)
}

// writeRateLimited reproduz o estouro de cota como o Linear o entrega: HTTP 400 com
// extensions.code, e o instante de reset num header em epoch de milissegundos.
func writeRateLimited(w http.ResponseWriter, reset time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Requests-Reset", fmt.Sprintf("%d", reset.UnixMilli()))
	w.WriteHeader(http.StatusBadRequest)
	io.WriteString(w, `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`)
}

// writeBadRequest é um 400 comum: erro do cliente, sem código de cota.
func writeBadRequest(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `{"errors":[{"message":%q,"extensions":{"code":"BAD_USER_INPUT"}}]}`, message)
}

// fakeClock adianta o relógio em vez de esperar: o backoff da cota do Linear é medido
// em minutos e nenhum teste pode pagar por isso.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) naps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// newClient monta um Client apontado para o Linear falso, autenticado por chave
// pessoal e com o time já resolvido (o UUID do time não é o assunto de nenhum teste).
func newClient(t *testing.T, f *fakeLinear, clock *fakeClock) *tracker.Client {
	t.Helper()
	c, err := tracker.New(tracker.Options{
		Secrets:  config.Secrets{LinearAPIKey: "chave-pessoal"},
		TeamID:   "team-uuid",
		Endpoint: f.server.URL,
		Clock:    clock,
		// O client avisa no log quando um enfeite da issue falha; num teste isso é
		// só ruído.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("montando o client: %v", err)
	}
	return c
}

// writeBadRequestWithCode é o 400 com um código específico — usado para a entidade
// que não existe.
func writeBadRequestWithCode(w http.ResponseWriter, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `{"errors":[{"message":%q,"extensions":{"code":%q}}]}`, message, code)
}

// newClientWith monta um Client apontado para o Linear falso deixando o teste mexer
// nas Options — credencial, time por chave, autoria.
func newClientWith(t *testing.T, f *fakeLinear, clock domain.Clock, tweak func(*tracker.Options)) *tracker.Client {
	t.Helper()
	opts := tracker.Options{
		Secrets:  config.Secrets{LinearAPIKey: "chave-pessoal"},
		TeamID:   "team-uuid",
		Endpoint: f.server.URL,
		Clock:    clock,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(&opts)
	}
	c, err := tracker.New(opts)
	if err != nil {
		t.Fatalf("montando o client: %v", err)
	}
	return c
}

// isUUIDv4 confere a forma da chave de idempotência: sem ela o Linear gera o id no
// servidor e a repetição da chamada vira uma segunda issue.
func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		case 14:
			if r != '4' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}
