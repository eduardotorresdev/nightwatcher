package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
)

func TestProjectAddWithEveryFlagCreatesTheProjetoAndItsFontes(t *testing.T) {
	db := tempDB(t)

	out, err := runCLI(t, "",
		"--db", db, "project", "add",
		"--nome", "acme",
		"--repo", "acme/acme",
		"--branch", "main",
		"--linear-project", "11112222-3333-4444-5555-666677778888",
		"--fonte", "acme-web:web",
		"--fonte", "acme-api:api",
	)
	if err != nil {
		t.Fatalf("project add devia ter funcionado: %v\n%s", err, out)
	}

	// The rows are what the daemon will read on its next queue cycle.
	st := openStore(t, db)
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("listando projetos: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("esperava 1 Projeto, vieram %d", len(projects))
	}
	p := projects[0]
	if p.Name != "acme" || p.Repo != "acme/acme" || p.Branch != "main" {
		t.Errorf("Projeto gravado errado: %+v", p)
	}
	if len(p.Sources) != 2 {
		t.Fatalf("esperava 2 Fontes, vieram %d: %+v", len(p.Sources), p.Sources)
	}
	labels := map[string]string{}
	for _, src := range p.Sources {
		labels[src.SentrySlug] = src.Label
	}
	if labels["acme-web"] != "web" || labels["acme-api"] != "api" {
		t.Errorf("os rótulos das Fontes não sobreviveram: %v", labels)
	}

	// And `project list` shows them with their rótulos, which is what makes "me
	// mostra tudo que veio da API" a filter and not a text search.
	listed, err := runCLI(t, "", "--db", db, "project", "list")
	if err != nil {
		t.Fatalf("project list: %v\n%s", err, listed)
	}
	line := lineWith(t, listed, "acme")
	for _, want := range []string{"acme/acme", "main", "acme-web (web)", "acme-api (api)"} {
		mustContain(t, line, want)
	}
}

// withTerminal makes the commands believe a human is watching, which is what `dokku
// run` actually gives them: it allocates a TTY unless it is asked for `-d`. A test
// cannot attach a real terminal to its own process, so the check itself is replaced.
func withTerminal(t *testing.T) {
	t.Helper()
	prev := interactive
	interactive = func(*cobra.Command) bool { return true }
	t.Cleanup(func() { interactive = prev })
}

func TestProjectAddWithEveryFlagAsksNothingEvenWithATerminalAttached(t *testing.T) {
	db := tempDB(t)
	withTerminal(t)

	// A GitHub that knows no repository, so a run that reached it would fail loudly —
	// and a counter, because the point is that it is never reached at all.
	var reached atomic.Int64
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	}))
	t.Cleanup(github.Close)

	// Empty stdin: any question at all ends the command with "a entrada acabou", which
	// is exactly the failure a scripted `dokku run ... project add` would hit.
	out, err := runCLI(t, "",
		"--db", db, "--"+flagGitHubAPI, github.URL,
		"project", "add",
		"--nome", "acme",
		"--repo", "acme/acme",
		"--branch", "main",
		"--linear-project", "11112222-3333-4444-5555-666677778888",
		"--fonte", "acme-web:web",
	)
	if err != nil {
		t.Fatalf("com todas as flags preenchidas o comando não pode perguntar nada: %v\n%s", err, out)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("o comando alcançou o GitHub %d vez(es); com --branch dada não há nada a descobrir", n)
	}

	st := openStore(t, db)
	p, err := st.ProjectByName(context.Background(), "acme")
	if err != nil {
		t.Fatalf("o Projeto não foi registrado: %v", err)
	}
	if p.Branch != "main" || len(p.Sources) != 1 || p.Sources[0].Label != "web" {
		t.Errorf("Projeto gravado errado: %+v", p)
	}
}

func TestProjectAddWithATerminalStillGuidesWhenAFlagIsMissing(t *testing.T) {
	db := tempDB(t)
	withTerminal(t)

	github := fakeGitHub(t, map[string]string{"acme/acme": "producao"})
	linear := fakeLinear(t, "0f1e2d3c-4b5a-4968-8776-655443332211", []linearProject{
		{ID: "aaaa1111-2222-3333-4444-555566667777", Name: "Acme Business System"},
	})
	t.Setenv(config.EnvGitHubToken, "ghp_token")
	t.Setenv(config.EnvLinearAPIKey, "lin_api_chave")
	t.Setenv(config.EnvSentryToken, "")

	// Everything but the Destino, so the terminal is there to be used for the one
	// answer the command line did not carry; the two empty lines after it close the
	// list of Fontes, which the guided flow always offers to extend, and decline the
	// servidores MCP it always offers to add.
	answers := "1\n\n\n"
	args := append([]string{"--db", db}, endpointFlags(github, nil, linear)...)
	args = append(args, "project", "add",
		"--nome", "acme", "--repo", "acme/acme", "--branch", "main",
		"--fonte", "acme-web:web")

	out, err := runCLI(t, answers, args...)
	if err != nil {
		t.Fatalf("com terminal e uma flag faltando o comando devia ter perguntado: %v\n%s", err, out)
	}
	mustContain(t, out, "Acme Business System")

	st := openStore(t, db)
	p, err := st.ProjectByName(context.Background(), "acme")
	if err != nil {
		t.Fatalf("o Projeto não foi registrado: %v", err)
	}
	if p.LinearProjectID != "aaaa1111-2222-3333-4444-555566667777" {
		t.Errorf("Destino = %q, esperava o Project escolhido pelo número 1", p.LinearProjectID)
	}
}

func TestProjectAddWithoutTerminalAndWithoutFlagsFailsSayingWhatWasMissingAndWritesNothing(t *testing.T) {
	db := tempDB(t)

	out, err := runCLI(t, "", "--db", db, "project", "add")
	if err == nil {
		t.Fatalf("sem terminal e sem flags o comando tinha que falhar; saída:\n%s", out)
	}

	message := err.Error() + out
	for _, flag := range []string{"--nome", "--repo", "--linear-project", "--fonte"} {
		mustContain(t, message, flag)
	}

	st := openStore(t, db)
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("listando projetos: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("um comando recusado não pode ter gravado nada, mas há %d Projeto(s)", len(projects))
	}
}

func TestProjectAddRefusesAFonteThatIsNotSlugAndRotulo(t *testing.T) {
	db := tempDB(t)

	out, err := runCLI(t, "",
		"--db", db, "project", "add",
		"--nome", "acme",
		"--repo", "acme/acme",
		"--branch", "main",
		"--linear-project", "algum-project",
		"--fonte", "acme-web",
	)
	if err == nil {
		t.Fatalf("uma Fonte sem rótulo tinha que ser recusada; saída:\n%s", out)
	}
	mustContain(t, err.Error(), "slug:característica")
}

func TestGuidedProjectAddValidatesEachAnswerLiveBeforeAcceptingIt(t *testing.T) {
	db := tempDB(t)
	const teamID = "0f1e2d3c-4b5a-4968-8776-655443332211"

	github := fakeGitHub(t, map[string]string{"acme/acme": "producao"})
	sentry := fakeSentry(t, "acme", "acme-web")
	linear := fakeLinear(t, teamID, []linearProject{
		{ID: "aaaa1111-2222-3333-4444-555566667777", Name: "Acme Business System"},
		{ID: "bbbb1111-2222-3333-4444-555566667777", Name: "Outro produto"},
	})

	t.Setenv(config.EnvSentryToken, "sentry-token")
	t.Setenv(config.EnvGitHubToken, "ghp_token")
	t.Setenv(config.EnvLinearAPIKey, "lin_api_chave")

	// A mistyped repository first, then the right one: the rejection has to land on
	// the next question and not on the first webhook.
	answers := strings.Join([]string{
		"acme",
		"acme/acme-que-nao-existe",
		"acme/acme",
		"", // aceita a branch default descoberta no GitHub
		"1",
		"acme", // organização do Sentry
		"acme-web:web",
		"", // encerra as Fontes
		"", // nenhum servidor MCP
	}, "\n") + "\n"

	args := append([]string{"--db", db}, endpointFlags(github, sentry, linear)...)
	args = append(args, "project", "add", "--"+flagInteractive)

	out, err := runCLI(t, answers, args...)
	if err != nil {
		t.Fatalf("o modo guiado devia ter terminado: %v\n%s", err, out)
	}
	mustContain(t, out, "não existe")
	mustContain(t, out, "Acme Business System")

	st := openStore(t, db)
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("listando projetos: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("esperava 1 Projeto, vieram %d", len(projects))
	}
	p := projects[0]
	if p.Branch != "producao" {
		t.Errorf("branch = %q, esperava a default descoberta no GitHub (producao)", p.Branch)
	}
	if p.LinearProjectID != "aaaa1111-2222-3333-4444-555566667777" {
		t.Errorf("Destino = %q, esperava o Project escolhido pelo número 1", p.LinearProjectID)
	}
	if len(p.Sources) != 1 || p.Sources[0].SentrySlug != "acme-web" || p.Sources[0].Label != "web" {
		t.Errorf("Fontes gravadas erradas: %+v", p.Sources)
	}

	// The organisation answered during the guided flow stays configured, so the next
	// run and every `config validate` stop asking for it.
	org, _, err := st.Setting(context.Background(), settingSentryOrg)
	if err != nil {
		t.Fatalf("lendo a organização: %v", err)
	}
	if org != "acme" {
		t.Errorf("sentry_org = %q, esperava acme", org)
	}
}

func TestProjectAddDiscoversTheDefaultBranchWhenTheFlagIsOmitted(t *testing.T) {
	db := tempDB(t)
	github := fakeGitHub(t, map[string]string{"acme/acme": "producao"})
	t.Setenv(config.EnvGitHubToken, "ghp_token")

	args := append([]string{"--db", db}, endpointFlags(github, nil, nil)...)
	args = append(args,
		"project", "add",
		"--nome", "acme",
		"--repo", "acme/acme",
		"--linear-project", "11112222-3333-4444-5555-666677778888",
		"--fonte", "acme-web:web",
	)

	out, err := runCLI(t, "", args...)
	if err != nil {
		t.Fatalf("project add sem --branch devia ter funcionado: %v\n%s", err, out)
	}

	st := openStore(t, db)
	p, err := st.ProjectByName(context.Background(), "acme")
	if err != nil {
		t.Fatalf("lendo o Projeto: %v", err)
	}
	if p.Branch != "producao" {
		t.Errorf("branch = %q, esperava producao", p.Branch)
	}
}

func TestProjectListInJSONCarriesTheSameProjetosAndRotulos(t *testing.T) {
	db := tempDB(t)
	seedProject(t, db, projectFixture("acme", "acme/acme", "acme-web", "web"))

	out, err := runCLI(t, "", "--db", db, "project", "list", "--json")
	if err != nil {
		t.Fatalf("project list --json: %v\n%s", err, out)
	}

	var rows []projectJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("a saída não é JSON válido: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].Nome != "acme" {
		t.Fatalf("JSON inesperado: %+v", rows)
	}
	if len(rows[0].Fontes) != 1 || rows[0].Fontes[0].Rotulo != "web" {
		t.Errorf("o rótulo não sobreviveu ao JSON: %+v", rows[0].Fontes)
	}
}
