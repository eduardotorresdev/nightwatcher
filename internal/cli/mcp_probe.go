package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// O teste de conectividade de um servidor MCP.
//
// `project add` confere cada resposta ao vivo — o repositório no GitHub, os Projects
// do Linear, cada Fonte no Sentry — para que um erro de digitação apareça na pergunta
// seguinte e não no primeiro webhook. `mcp add` faz o mesmo, e tem um motivo a mais:
// um MCP que não responde não quebra nada visivelmente. A Análise roda, o Diagnóstico
// sai, e ninguém descobre que ele foi escrito sem a fonte que o operador acreditava
// ter configurado.

// mcpProtocolVersion is the version this probe negotiates. It is a date string by
// specification, and an older server answering with a different one is fine — what is
// being proved here is that the endpoint speaks MCP and accepts the credential, not
// that it agrees on a version.
const mcpProtocolVersion = "2025-06-18"

// mcpProbeTimeout is short on purpose: this runs while somebody watches a terminal,
// and a probe that hangs is a probe that gets skipped next time.
const mcpProbeTimeout = 20 * time.Second

// mcpProbeResult is what the probe learned, for printing.
type mcpProbeResult struct {
	ServerName    string
	ServerVersion string
}

// probeMCP proves the server is there, speaks MCP and accepts the credential.
//
// lookup resolves the header variables. It is injected rather than read from the
// process so the same function serves the probe running here and the one running
// inside the Análise's container.
func probeMCP(ctx context.Context, client *http.Client, m domain.MCPServer, lookup func(string) string) (mcpProbeResult, error) {
	if err := m.Validate(); err != nil {
		return mcpProbeResult{}, err
	}
	if missing := missingEnv(m, lookup); len(missing) > 0 {
		return mcpProbeResult{}, &mcpEnvError{Vars: missing}
	}
	if m.Transport == domain.MCPTransportSSE {
		return probeMCPSSE(ctx, client, m, lookup)
	}
	return probeMCPHTTP(ctx, client, m, lookup)
}

// mcpEnvError is the missing-credential case, kept as a type so `mcp add` can print
// the fix instead of the failure.
type mcpEnvError struct{ Vars []string }

func (e *mcpEnvError) Error() string {
	return "variável de ambiente não definida: " + strings.Join(e.Vars, ", ")
}

// mcpOAuthError is a server that wants an interactive OAuth flow. It gets its own type
// because it is the one failure where retrying, fixing the token or opening the
// firewall are all wrong answers: the nightwatcher cannot do this at all.
type mcpOAuthError struct{ Challenge string }

func (e *mcpOAuthError) Error() string {
	return "o servidor exige OAuth interativo (" + e.Challenge + ")"
}

func missingEnv(m domain.MCPServer, lookup func(string) string) []string {
	var out []string
	for _, h := range m.Headers {
		if strings.TrimSpace(lookup(h.ValueEnv)) == "" {
			out = append(out, h.ValueEnv)
		}
	}
	return out
}

func mcpHeaders(m domain.MCPServer, lookup func(string) string) http.Header {
	h := http.Header{}
	for _, hd := range m.Headers {
		h.Set(hd.Name, hd.Value(lookup))
	}
	return h
}

// probeMCPHTTP sends the `initialize` handshake, which is the first message of every
// MCP session and therefore exactly what the Análise's own session will send.
func probeMCPHTTP(ctx context.Context, client *http.Client, m domain.MCPServer, lookup func(string) string) (mcpProbeResult, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "nightwatcher", "version": Version},
		},
	})
	if err != nil {
		return mcpProbeResult{}, fmt.Errorf("montando o handshake MCP: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
	if err != nil {
		return mcpProbeResult{}, fmt.Errorf("montando requisição para %s: %w", m.URL, err)
	}
	for k, vs := range mcpHeaders(m, lookup) {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	// Both, in this order: the Streamable HTTP transport lets the server answer either
	// with a plain JSON body or with a one-message SSE stream, and a server that only
	// does the second refuses a request that did not say it would accept it.
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return mcpProbeResult{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return mcpProbeResult{}, fmt.Errorf("lendo a resposta de %s: %w", m.URL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if c := resp.Header.Get("WWW-Authenticate"); strings.Contains(strings.ToLower(c), "bearer") &&
				strings.Contains(strings.ToLower(c), "resource_metadata") {
				return mcpProbeResult{}, &mcpOAuthError{Challenge: c}
			}
		}
		return mcpProbeResult{}, &httpError{Status: resp.StatusCode, Endpoint: m.URL, Body: string(raw)}
	}

	payload := raw
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		if payload, err = firstSSEData(raw); err != nil {
			return mcpProbeResult{}, err
		}
	}

	var reply struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &reply); err != nil {
		return mcpProbeResult{}, fmt.Errorf("%s respondeu 200 mas não em JSON-RPC — a URL aponta para um servidor MCP?", m.URL)
	}
	if reply.Error != nil {
		return mcpProbeResult{}, fmt.Errorf("o servidor recusou o handshake MCP: %s (código %d)", reply.Error.Message, reply.Error.Code)
	}
	// A 200 with neither result nor error is not a working server: it is something
	// else entirely answering on that URL, and accepting it here would store a
	// configuration that fails only at the first Diagnóstico.
	if reply.Result == nil {
		return mcpProbeResult{}, fmt.Errorf("%s respondeu sem `result` — a URL aponta para um servidor MCP?", m.URL)
	}
	return mcpProbeResult{
		ServerName:    reply.Result.ServerInfo.Name,
		ServerVersion: reply.Result.ServerInfo.Version,
	}, nil
}

// probeMCPSSE covers the older transport, where the session starts with a GET that
// stays open. Nothing is negotiated here: what is proved is that the endpoint answers,
// accepts the credential and really opens an event stream.
func probeMCPSSE(ctx context.Context, client *http.Client, m domain.MCPServer, lookup func(string) string) (mcpProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return mcpProbeResult{}, fmt.Errorf("montando requisição para %s: %w", m.URL, err)
	}
	for k, vs := range mcpHeaders(m, lookup) {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return mcpProbeResult{}, err
	}
	// The stream is meant to stay open; we close it as soon as the headers prove the
	// point, which is why the body is never read to the end.
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if c := resp.Header.Get("WWW-Authenticate"); strings.Contains(strings.ToLower(c), "resource_metadata") {
				return mcpProbeResult{}, &mcpOAuthError{Challenge: c}
			}
		}
		return mcpProbeResult{}, &httpError{Status: resp.StatusCode, Endpoint: m.URL, Body: string(raw)}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		return mcpProbeResult{}, fmt.Errorf("%s respondeu %s em vez de text/event-stream — o transporte é sse mesmo?", m.URL, ct)
	}
	return mcpProbeResult{}, nil
}

// firstSSEData pulls the first `data:` payload out of an event stream. The initialize
// reply is a single message, so the first one is the whole answer.
func firstSSEData(raw []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			return []byte(strings.TrimSpace(data)), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("lendo o event stream: %w", err)
	}
	return nil, errors.New("o event stream não trouxe nenhuma mensagem `data:`")
}
