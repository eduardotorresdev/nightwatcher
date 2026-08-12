package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Códigos de erro que o pacote reconhece. RATELIMITED é o único vindo literalmente
// do Linear; os outros dois são internos, para o chamador não precisar distinguir
// "a rede caiu" de "a API respondeu" quando a decisão é a mesma.
const (
	codeRateLimited = "RATELIMITED"
	codeTransport   = "TRANSPORTE"
	codeAuth        = "AUTENTICACAO"
	codeNotFound    = "NAO_ENCONTRADO"
)

// headerRequestsReset traz o instante em que a cota de requisições volta, em epoch
// UTC de MILISSEGUNDOS. Não existe Retry-After nas respostas do Linear.
const headerRequestsReset = "X-RateLimit-Requests-Reset"

// APIError é uma falha da API do Linear já classificada.
type APIError struct {
	// Op é a operação em linguagem humana, para a mensagem dizer o que quebrou.
	Op         string
	StatusCode int
	// Code é extensions.code do primeiro erro GraphQL, quando há um.
	Code    string
	Message string
	// RetryAfter é quanto falta para a cota voltar, derivado do header de reset.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("linear")
	if e.Op != "" {
		b.WriteString(" ")
		b.WriteString(e.Op)
	}
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.StatusCode)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " [%s]", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Retryable diz se repetir a chamada tem chance de dar outro resultado.
//
// A armadilha que este método existe para evitar: o estouro de cota do Linear chega
// como HTTP **400**, não 429, e só se distingue de um 400 comum por
// `errors[].extensions.code == "RATELIMITED"`. Um cliente que classifique por status
// trata cota estourada como erro permanente e desiste em silêncio.
func (e *APIError) Retryable() bool {
	switch e.Code {
	case codeRateLimited, codeTransport:
		return true
	}
	if e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500 {
		return true
	}
	return false
}

// IsRetryable é a mesma pergunta para um erro qualquer da cadeia.
func IsRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	return false
}

// RetryAfter devolve quanto esperar antes de tentar de novo, ou 0 se não se sabe.
func RetryAfter(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// do executa uma operação GraphQL, decodificando `data` em out.
//
// Cuida de duas coisas que todo chamador precisaria repetir: renovar o token uma vez
// diante de um 401 (não há refresh token — a renovação é pedir outro), e esperar a
// janela de cota quando a espera é curta.
func (c *Client) do(ctx context.Context, op, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("tracker: %s: montando requisição: %w", op, err)
	}

	authRetried := false
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, op, payload, out)
		if err == nil {
			return nil
		}

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return err
		}

		// 401: o token de 30 dias morreu antes da hora (secret rotacionado, app
		// reconfigurado). Uma segunda tentativa com token novo, e só uma.
		if apiErr.Code == codeAuth && apiErr.StatusCode == http.StatusUnauthorized &&
			c.auth.renewable() && !authRetried {
			authRetried = true
			c.auth.expire()
			continue
		}

		if !apiErr.Retryable() || attempt >= c.retries {
			return err
		}

		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = c.baseBackoff * time.Duration(1<<attempt)
		}
		if wait > c.maxBackoff {
			// A cota do Linear reseta por hora. Dormir a janela inteira aqui
			// prenderia o worker; o erro sobe com RetryAfter para quem sabe
			// reenfileirar decidir.
			return err
		}
		if err := c.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *Client) once(ctx context.Context, op string, payload []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("tracker: %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := c.auth.authorize(ctx, req); err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Erro de rede é indistinguível de indisponibilidade momentânea: retentável.
		return &APIError{Op: op, Code: codeTransport, Message: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &APIError{Op: op, Code: codeTransport, Message: err.Error()}
	}

	var parsed gqlResponse
	decodeErr := json.Unmarshal(body, &parsed)

	if len(parsed.Errors) > 0 {
		first := parsed.Errors[0]
		apiErr := &APIError{
			Op:         op,
			StatusCode: resp.StatusCode,
			Code:       first.Extensions.Code,
			Message:    first.Message,
		}
		if apiErr.Code == codeRateLimited {
			apiErr.RetryAfter = resetDelay(resp.Header.Get(headerRequestsReset), c.clock.Now())
		}
		if resp.StatusCode == http.StatusUnauthorized {
			apiErr.Code = codeAuth
		}
		return apiErr
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &APIError{Op: op, StatusCode: resp.StatusCode, Code: codeAuth,
			Message: truncate(strings.TrimSpace(string(body)), 300)}
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{Op: op, StatusCode: resp.StatusCode,
			Message: truncate(strings.TrimSpace(string(body)), 300)}
	}
	if decodeErr != nil {
		return fmt.Errorf("tracker: %s: resposta ilegível: %w", op, decodeErr)
	}
	if out == nil {
		return nil
	}
	if len(parsed.Data) == 0 || string(parsed.Data) == "null" {
		return fmt.Errorf("tracker: %s: o Linear respondeu 200 sem dados", op)
	}
	if err := json.Unmarshal(parsed.Data, out); err != nil {
		return fmt.Errorf("tracker: %s: resposta inesperada: %w", op, err)
	}
	return nil
}

// resetDelay converte o header de reset (epoch UTC em milissegundos) em espera.
func resetDelay(header string, now time.Time) time.Duration {
	ms, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	d := time.UnixMilli(ms).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
