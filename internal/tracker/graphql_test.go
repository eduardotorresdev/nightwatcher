package tracker_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// O estouro de cota do Linear chega como HTTP 400, não 429. Classificar por status
// faria o daemon desistir de uma chamada que só precisava esperar — e um 400 comum,
// que nunca vai melhorar, seria repetido à toa.
func TestRetryableSeparatesRateLimitFromAPlainBadRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *tracker.APIError
		want bool
	}{
		{"cota estourada chega como 400 com RATELIMITED", &tracker.APIError{StatusCode: 400, Code: "RATELIMITED"}, true},
		{"400 comum é erro nosso, repetir não muda nada", &tracker.APIError{StatusCode: 400, Code: "BAD_USER_INPUT"}, false},
		{"400 sem código nenhum também não", &tracker.APIError{StatusCode: 400}, false},
		{"404 não melhora com o tempo", &tracker.APIError{StatusCode: 404}, false},
		{"502 é o Linear fora do ar", &tracker.APIError{StatusCode: 502}, true},
		{"429, se um dia vier, também", &tracker.APIError{StatusCode: 429}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Retryable(); got != tc.want {
				t.Errorf("Retryable() = %v, quero %v", got, tc.want)
			}
			if got := tracker.IsRetryable(error(tc.err)); got != tc.want {
				t.Errorf("IsRetryable() = %v, quero %v", got, tc.want)
			}
		})
	}
}

func TestRateLimitIsWaitedOutUsingTheResetHeader(t *testing.T) {
	clock := newFakeClock()
	f := newFakeLinear(t)
	reset := clock.Now().Add(2 * time.Second)

	var calls int
	f.on("CreateComment", func(w http.ResponseWriter, _ gqlRequest) {
		calls++
		if calls == 1 {
			writeRateLimited(w, reset)
			return
		}
		writeData(w, `{"commentCreate":{"success":true,"comment":{"id":"c1","url":"u"}}}`)
	})

	if err := newClient(t, f, clock).Comment(context.Background(), "issue-uuid", "corpo"); err != nil {
		t.Fatalf("a chamada tinha que sobreviver à cota: %v", err)
	}
	if calls != 2 {
		t.Errorf("chamadas = %d, quero 2", calls)
	}
	// Não há Retry-After nas respostas do Linear; a espera sai do header de reset.
	if naps := clock.naps(); len(naps) != 1 || naps[0] != 2*time.Second {
		t.Errorf("esperas = %v, quero [2s]", naps)
	}
}

func TestAPlainBadRequestIsNotRepeated(t *testing.T) {
	f := newFakeLinear(t)
	f.on("CreateComment", func(w http.ResponseWriter, _ gqlRequest) {
		writeBadRequest(w, "comment body is empty")
	})

	err := newClient(t, f, newFakeClock()).Comment(context.Background(), "issue-uuid", "")
	if err == nil {
		t.Fatal("quero erro")
	}
	if tracker.IsRetryable(err) {
		t.Error("400 comum não é retentável")
	}
	if f.count() != 1 {
		t.Errorf("requisições = %d, quero 1", f.count())
	}
}

// A cota do Linear reseta por hora. Dormir a janela inteira dentro da chamada
// prenderia o worker; o erro sobe com o tempo de espera para quem sabe reenfileirar.
func TestALongRateLimitWindowIsHandedBackInsteadOfSlept(t *testing.T) {
	clock := newFakeClock()
	f := newFakeLinear(t)
	f.on("CreateComment", func(w http.ResponseWriter, _ gqlRequest) {
		writeRateLimited(w, clock.Now().Add(time.Hour))
	})

	err := newClient(t, f, clock).Comment(context.Background(), "issue-uuid", "corpo")
	if err == nil {
		t.Fatal("quero erro")
	}
	if !tracker.IsRetryable(err) {
		t.Error("continua sendo um erro retentável, só não aqui dentro")
	}
	if got := tracker.RetryAfter(err); got < 55*time.Minute {
		t.Errorf("RetryAfter = %v, quero perto de 1h", got)
	}
	if naps := clock.naps(); len(naps) != 0 {
		t.Errorf("não deveria ter dormido, dormiu %v", naps)
	}
	if f.count() != 1 {
		t.Errorf("requisições = %d, quero 1", f.count())
	}
}

// A chave pessoal vai crua no header. O prefixo `Bearer` só existe para OAuth, e
// mandá-lo devolve 401 sem nenhuma pista de por quê.
func TestThePersonalKeyGoesWithoutTheBearerPrefix(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("CreateComment", `{"commentCreate":{"success":true,"comment":{"id":"c1","url":"u"}}}`)

	if err := newClient(t, f, newFakeClock()).Comment(context.Background(), "i", "corpo"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := f.only("CreateComment").Header.Get("Authorization"); got != "chave-pessoal" {
		t.Errorf("Authorization = %q, quero a chave crua", got)
	}
}

// O token da conta de serviço vale 30 dias e NÃO tem refresh token: renovar é pedir
// outro. Um 401 no meio da madrugada tem que resolver sozinho.
func TestTheServiceAccountTokenIsRenewedAfterAnUnauthorized(t *testing.T) {
	oauth := newFakeOAuth(t, 30*24*time.Hour)
	f := newFakeLinear(t)

	var calls int
	f.on("CreateComment", func(w http.ResponseWriter, _ gqlRequest) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"message":"Authentication required"}]}`)
			return
		}
		writeData(w, `{"commentCreate":{"success":true,"comment":{"id":"c1","url":"u"}}}`)
	})

	c := newAppClient(t, f, oauth, newFakeClock())
	if err := c.Comment(context.Background(), "issue-uuid", "corpo"); err != nil {
		t.Fatalf("o 401 tinha que ser resolvido com token novo: %v", err)
	}
	if oauth.count() != 2 {
		t.Errorf("pedidos de token = %d, quero 2", oauth.count())
	}
	if got := f.got("CreateComment")[1].Header.Get("Authorization"); got != "Bearer token-2" {
		t.Errorf("a segunda tentativa foi com %q", got)
	}
}

func TestTheServiceAccountTokenIsRenewedBeforeItExpires(t *testing.T) {
	clock := newFakeClock()
	oauth := newFakeOAuth(t, 48*time.Hour)
	f := newFakeLinear(t)
	f.onData("CreateComment", `{"commentCreate":{"success":true,"comment":{"id":"c1","url":"u"}}}`)

	c := newAppClient(t, f, oauth, clock)
	if err := c.Comment(context.Background(), "i", "corpo"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	clock.advance(25 * time.Hour)
	if err := c.Comment(context.Background(), "i", "corpo"); err != nil {
		t.Fatalf("Comment: %v", err)
	}

	if oauth.count() != 2 {
		t.Errorf("pedidos de token = %d, quero 2 (renovação proativa)", oauth.count())
	}
	form := oauth.forms()[0]
	if form.Get("grant_type") != "client_credentials" {
		t.Errorf("grant_type = %q", form.Get("grant_type"))
	}
	// actor=app é o que faz o token ser da aplicação, e não de uma pessoa.
	if form.Get("actor") != "app" {
		t.Errorf("actor = %q, quero app", form.Get("actor"))
	}
	if form.Get("scope") == "" {
		t.Error("scope é obrigatório no client_credentials do Linear")
	}
}

func TestNewRefusesToStartWithoutCredentialsOrTeam(t *testing.T) {
	if _, err := tracker.New(tracker.Options{TeamID: "t"}); err == nil {
		t.Error("sem credencial do Linear o Client não pode existir")
	}
	if _, err := tracker.New(tracker.Options{Secrets: config.Secrets{LinearAPIKey: "k"}}); err == nil {
		t.Error("sem o time do Destino o Client não pode existir")
	}
}

// fakeOAuth é o endpoint de token do Linear.
type fakeOAuth struct {
	server    *httptest.Server
	expiresIn time.Duration

	mu    sync.Mutex
	calls int
	seen  []url.Values
}

func newFakeOAuth(t *testing.T, expiresIn time.Duration) *fakeOAuth {
	t.Helper()
	o := &fakeOAuth{expiresIn: expiresIn}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("form ilegível: %v", err)
		}
		o.mu.Lock()
		o.calls++
		n := o.calls
		o.seen = append(o.seen, r.PostForm)
		o.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":%d,"scope":"read write"}`,
			n, int64(o.expiresIn.Seconds()))
	}))
	t.Cleanup(o.server.Close)
	return o
}

func (o *fakeOAuth) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func (o *fakeOAuth) forms() []url.Values {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]url.Values(nil), o.seen...)
}

func newAppClient(t *testing.T, f *fakeLinear, o *fakeOAuth, clock domain.Clock) *tracker.Client {
	t.Helper()
	c, err := tracker.New(tracker.Options{
		Secrets: config.Secrets{
			LinearClientID:     "client-id",
			LinearClientSecret: "client-secret",
		},
		TeamID:        "team-uuid",
		Endpoint:      f.server.URL,
		TokenEndpoint: o.server.URL,
		Clock:         clock,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("montando o client: %v", err)
	}
	return c
}
