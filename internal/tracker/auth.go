package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// authorizer injeta o header Authorization. As duas formas de autenticar no Linear
// escrevem headers diferentes e têm ciclos de vida diferentes, e o resto do pacote
// não deve saber qual está em uso.
type authorizer interface {
	authorize(ctx context.Context, req *http.Request) error
	// expire descarta o token corrente. Chamado ao receber 401: como o grant
	// client_credentials NÃO devolve refresh token, renovar é pedir um token novo.
	expire()
	// renewable diz se vale a pena repetir a chamada depois de um 401.
	renewable() bool
	// actorApp diz se o token é da aplicação. Só nesse modo o Linear aceita
	// createAsUser/displayIconUrl.
	actorApp() bool
}

// personalKey é a chave pessoal — a primeira fase, para destravar
// desenvolvimento sem app OAuth no workspace.
type personalKey struct{ key string }

func (p personalKey) authorize(_ context.Context, req *http.Request) error {
	// A chave pessoal vai CRUA no header, sem o prefixo `Bearer`. Só o token de
	// OAuth o usa. Errar isso devolve 401 sem nenhuma pista de por quê.
	req.Header.Set("Authorization", p.key)
	return nil
}

func (personalKey) expire()         {}
func (personalKey) renewable() bool { return false }
func (personalKey) actorApp() bool  { return false }

// renewMargin é a antecedência com que o token da aplicação é renovado. O token vale
// 30 dias; renovar com um dia de folga significa que a renovação acontece numa
// chamada qualquer, e não na primeira Ocorrência depois de o token morrer.
const renewMargin = 24 * time.Hour

// appToken é a conta de serviço: OAuth client_credentials com actor=app. Não há
// refresh token, então a renovação é proativa (antes de expirar) e reativa (no 401).
type appToken struct {
	clientID     string
	clientSecret string
	scope        string
	endpoint     string
	http         *http.Client
	clock        domain.Clock

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func (a *appToken) renewable() bool { return true }
func (a *appToken) actorApp() bool  { return true }

func (a *appToken) expire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = ""
	a.expiresAt = time.Time{}
}

func (a *appToken) authorize(ctx context.Context, req *http.Request) error {
	tok, err := a.get(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func (a *appToken) get(ctx context.Context) (string, error) {
	// O lock cobre a chamada HTTP de propósito: o worker roda Análises em paralelo e
	// N renovações simultâneas gastariam N dos 1.000 tokens ativos que o app tem.
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.clock.Now().Before(a.expiresAt) {
		return a.token, nil
	}

	form := url.Values{
		"grant_type": {"client_credentials"},
		// scope é obrigatório no client_credentials do Linear.
		"scope": {a.scope},
		// actor=app emite o token como a aplicação, que é o modo documentado para
		// "agents and service accounts".
		"actor": {"app"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(a.clientID, a.clientSecret)

	resp, err := a.http.Do(req)
	if err != nil {
		return "", &APIError{Op: "oauth/token", Code: codeTransport, Message: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &APIError{Op: "oauth/token", Code: codeTransport, Message: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &APIError{
			Op:         "oauth/token",
			StatusCode: resp.StatusCode,
			Code:       codeAuth,
			Message:    truncate(strings.TrimSpace(string(body)), 300),
		}
	}

	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("tracker: resposta de token ilegível: %w", err)
	}
	if out.AccessToken == "" {
		return "", &APIError{Op: "oauth/token", StatusCode: resp.StatusCode, Code: codeAuth,
			Message: "resposta de token sem access_token"}
	}

	ttl := time.Duration(out.ExpiresIn) * time.Second
	now := a.clock.Now()
	switch {
	case ttl <= 0:
		// Sem expires_in não dá para renovar proativamente; trata como token curto
		// e deixa o 401 fazer o trabalho.
		a.expiresAt = now.Add(time.Hour)
	case ttl <= renewMargin:
		a.expiresAt = now.Add(ttl / 2)
	default:
		a.expiresAt = now.Add(ttl - renewMargin)
	}
	a.token = out.AccessToken
	return a.token, nil
}
