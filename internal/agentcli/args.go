package agentcli

import (
	"strconv"
	"strings"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// permissionMode auto-denies anything not pre-approved instead of blocking on a
// confirmation prompt. There is no TTY here to answer one, and a daemon that hangs
// waiting for a person is the predecessor's silent death by another route.
const permissionMode = "dontAsk"

// DeepTools is the universe of the long session: read a file, search by content,
// search by name. Nothing that runs, writes or reaches the network.
var DeepTools = []string{"Read", "Grep", "Glob"}

// dangerousTools is denied by name on top of the universe. A pure name in
// --disallowed-tools removes the tool from the model's context entirely, so this is
// defence in depth over --tools, not a substitute for it. Only names that certainly
// exist are listed: a rule for a tool that does not exist buys nothing and invites a
// startup warning.
//
// `mcp__*` stays the default and is lifted only for the servers a session actually
// declared — see denyList. A session with no server keeps the blanket deny, which is
// what makes an MCP appearing out of the cloned repository useless as well as fatal.
var dangerousTools = []string{
	"Bash", "Write", "Edit", "NotebookEdit",
	"WebFetch", "WebSearch", "Task", "TodoWrite", "SlashCommand",
	"mcp__*",
}

// mcpWildcard is the deny rule that has to come off before any MCP server can be
// used, because --disallowed-tools outranks --allowed-tools.
const mcpWildcard = "mcp__*"

// secretDenySettings keeps the read tools away from the obvious credentials in the
// clone. Path rules are only consulted for Read(...) and Edit(...) — the CLI accepts
// a Write(path) or Glob(path) rule and then never asks it anything — so everything
// here is written as Read.
const secretDenySettings = `{"permissions":{"deny":[` +
	`"Read(./.env)","Read(./.env.*)","Read(**/.env)","Read(**/*.pem)",` +
	`"Read(**/id_rsa*)","Read(**/.git/config)","Read(**/.npmrc)"]}}`

// maxTurnsTriage caps the short session. It has no tools at all, so it answers in one
// turn; the slack is for a retry inside the CLI, not room to wander.
const maxTurnsTriage = 3

// buildArgs assembles the CLI invocation. Every flag here is one fact verified against
// the CLI itself, and most of them are a bug that would otherwise only show up in
// production.
func buildArgs(req domain.AgentRequest, tools []string, o Options) ([]string, error) {
	mcpJSON := ""
	if len(req.MCPServers) > 0 {
		var err error
		if mcpJSON, err = buildMCPConfig(req.MCPServers); err != nil {
			return nil, err
		}
	}

	args := []string{
		"--print",

		// stream-json, not json: the system/api_retry event is the only structured
		// classifier for a usage limit, and it only exists in the stream. --verbose is
		// not optional, stream-json refuses to run without it.
		"--output-format", "stream-json",
		"--verbose",

		// --tools is the flag that restricts which tools EXIST. --allowed-tools merely
		// waives the confirmation prompt and restricts nothing at all — using it as
		// the allowlist is the mistake the PRD corrected. An empty value disables
		// every tool, which is exactly what the Triagem wants.
		"--tools", strings.Join(tools, ","),
		"--disallowed-tools", strings.Join(denyList(tools, req.MCPServers), ","),
		"--permission-mode", permissionMode,

		// --bare would be the natural isolation switch, and it is out: it does not
		// read CLAUDE_CODE_OAUTH_TOKEN, which is the whole authentication. These three
		// are the verified substitute — no host settings, no MCP servers, no slash
		// commands from the cloned repository.
		//
		// --strict-mcp-config is passed with and WITHOUT servers of our own: it means
		// "only what --mcp-config says", so with no --mcp-config it means none at all,
		// and with one it means ours and not the clone's.
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disable-slash-commands",

		// The session holds text written by an end user. None of it survives on disk.
		"--no-session-persistence",

		"--settings", secretDenySettings,
	}

	if mcpJSON != "" {
		// --allowed-tools, which for a built-in tool restricts nothing, is what
		// AUTHORISES an MCP tool under `dontAsk` — where anything not pre-approved is
		// auto-denied. Without it the servers connect and every call is refused.
		args = append(args,
			"--mcp-config", mcpJSON,
			"--allowed-tools", strings.Join(mcpAllowedTools(req.MCPServers), ","),
		)
	}

	if turns := maxTurns(req.Kind, o); turns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(turns))
	}
	if o.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(o.MaxBudgetUSD, 'f', 2, 64))
	}
	if o.SystemPrompt != "" {
		// The untrusted text arrives in the user turn; the rule about it belongs
		// somewhere the user turn cannot argue with.
		args = append(args, "--append-system-prompt", o.SystemPrompt)
	}
	return args, nil
}

func maxTurns(kind domain.SessionKind, o Options) int {
	if kind == domain.SessionTriage {
		if o.MaxTurns > 0 && o.MaxTurns < maxTurnsTriage {
			return o.MaxTurns
		}
		return maxTurnsTriage
	}
	return o.MaxTurns
}

// denyList is the dangerous set minus whatever this session legitimately asked for,
// so the deny rules never contradict the universe.
//
// The MCP wildcard is the one entry that is not matched by an exact name, and getting
// that wrong fails in the worst possible way: `--disallowed-tools` outranks
// `--allowed-tools`, so a session that declared a server, connected to it and listed
// its tools in the init event would have every single call denied — a capability that
// is configured, reported as present, and silently inert. So the rule comes off as a
// whole the moment this session has a server of its own; what bounds it then is
// `--allowed-tools`, which names each declared server and nothing else.
func denyList(tools []string, servers []domain.MCPServer) []string {
	allowed := make(map[string]bool, len(tools))
	for _, t := range tools {
		allowed[strings.TrimSpace(t)] = true
	}
	out := make([]string, 0, len(dangerousTools))
	for _, t := range dangerousTools {
		if allowed[t] {
			continue
		}
		if t == mcpWildcard && len(servers) > 0 {
			continue
		}
		out = append(out, t)
	}
	return out
}

// effectiveTools is the universe for this session. A nil Tools means "the usual for
// this kind"; an empty non-nil slice means "none", which is the Triagem's contract.
func effectiveTools(req domain.AgentRequest) []string {
	if req.Tools != nil {
		return req.Tools
	}
	if req.Kind == domain.SessionTriage {
		return nil
	}
	return DeepTools
}

// buildEnv builds the child's environment from scratch, and never from os.Environ().
//
// ANTHROPIC_API_KEY outranks CLAUDE_CODE_OAUTH_TOKEN in the CLI's credential
// precedence, and in --print mode it is used without a prompt. One stray variable
// inherited from the host would silently move the whole operation to per-token API
// billing while everything kept working. Inheriting is a money bug, not a style
// question.
func buildEnv(req domain.AgentRequest, o Options, lookup func(string) string) map[string]string {
	env := map[string]string{}
	if o.Token != "" {
		env["CLAUDE_CODE_OAUTH_TOKEN"] = o.Token
	}
	// The MCP credentials, and only the ones the config actually references. They
	// travel as values here and as NAMES on the docker command line, which is what
	// keeps them out of an argv that /proc shows to every account on the host.
	for k, v := range mcpEnv(req.MCPServers, lookup) {
		env[k] = v
	}
	// PATH is the one deliberate inheritance: without it the binary cannot be found.
	if o.Path != "" {
		env["PATH"] = o.Path
	}
	// HOME has to be writable — the CLI creates .claude.json during the run. When it
	// is empty the Runner supplies one from its own perimeter: a tmpfs in the
	// container, a throwaway directory locally.
	if o.Home != "" {
		env["HOME"] = o.Home
	}
	return env
}
