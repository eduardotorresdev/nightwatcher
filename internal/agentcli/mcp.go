package agentcli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// Servidores MCP remotos na invocação do CLI.
//
// Duas coisas aqui não são óbvias e custam uma sessão inteira para descobrir:
//
//  1. `--tools` governa SÓ o conjunto embutido ("from the built-in set", diz o próprio
//     --help). Ferramenta de MCP não passa por ele. Quem a autoriza sob
//     `--permission-mode dontAsk` é `--allowed-tools` — o mesmo flag que, para
//     ferramenta embutida, não restringe nada. Os dois fatos convivem, e confundi-los
//     nas duas direções é um erro diferente: usar --allowed-tools como allowlist de
//     ferramenta embutida não protege, e não usá-lo para MCP não libera.
//
//  2. `--disallowed-tools` vence `--allowed-tools`. Enquanto `mcp__*` estiver na regra
//     de deny, nenhum servidor funciona, e funciona em SILÊNCIO: a sessão sobe, o
//     servidor conecta, o init declara as ferramentas, e toda chamada é negada. Por
//     isso denyList precisa entender o curinga em vez de comparar string exata.
//
// `--strict-mcp-config` permanece em todas as invocações, com ou sem servidor: é ele
// que impede o `.mcp.json` do repositório clonado de entrar junto, que é conteúdo que
// ninguém aqui controla.

// mcpConfig is the shape --mcp-config reads.
type mcpConfig struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// buildMCPConfig renders the servers as the JSON handed to --mcp-config.
//
// The header value is written as ${VARIAVEL}, never as the credential: the CLI expands
// it from its own environment, and that environment reaches the container by NAME
// (`docker run --env FOO`), so the secret never appears in an argv that /proc makes
// world-readable. It is the same reasoning that keeps the subscription token out of
// the docker command line.
func buildMCPConfig(servers []domain.MCPServer) (string, error) {
	cfg := mcpConfig{MCPServers: make(map[string]mcpServerConfig, len(servers))}
	for _, s := range servers {
		if err := s.Validate(); err != nil {
			return "", err
		}
		if _, dup := cfg.MCPServers[s.Name]; dup {
			return "", fmt.Errorf("servidor MCP %s declarado duas vezes na mesma sessão", s.Name)
		}
		entry := mcpServerConfig{Type: string(s.Transport), URL: s.URL}
		if len(s.Headers) > 0 {
			entry.Headers = make(map[string]string, len(s.Headers))
			for _, h := range s.Headers {
				entry.Headers[h.Name] = h.Prefix + "${" + h.ValueEnv + "}"
			}
		}
		cfg.MCPServers[s.Name] = entry
	}
	// Marshal of a map sorts the keys, so the argv of two identical Análises is
	// identical too — which is what makes an argv assertion in a test worth writing.
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("montando a configuração MCP: %w", err)
	}
	return string(b), nil
}

// mcpAllowedTools names each server as a whole (mcp__<nome>), and not each tool. The
// tool names live on the server and change without asking us; enumerating them here
// would mean an Análise silently losing a capability the day the server adds one.
// The boundary that matters is which SERVER may be reached, and that is this list.
func mcpAllowedTools(servers []domain.MCPServer) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.ToolPrefix())
	}
	sort.Strings(out)
	return out
}

// mcpEnv collects the environment variables the config references, so the Runner can
// pass them by name. Only the ones actually referenced: the child's environment is
// built from scratch, and this keeps that true.
func mcpEnv(servers []domain.MCPServer, lookup func(string) string) map[string]string {
	env := map[string]string{}
	for _, s := range servers {
		for _, h := range s.Headers {
			env[h.ValueEnv] = lookup(h.ValueEnv)
		}
	}
	return env
}

// MissingMCPEnv names the variables a set of servers needs and the environment does
// not have. An empty variable is a header sent empty, which the server answers with a
// 401 that reads like a revoked credential — so it is caught here, by name.
func MissingMCPEnv(servers []domain.MCPServer, lookup func(string) string) []string {
	var missing []string
	seen := map[string]bool{}
	for _, s := range servers {
		for _, h := range s.Headers {
			if seen[h.ValueEnv] {
				continue
			}
			seen[h.ValueEnv] = true
			if strings.TrimSpace(lookup(h.ValueEnv)) == "" {
				missing = append(missing, h.ValueEnv)
			}
		}
	}
	sort.Strings(missing)
	return missing
}
