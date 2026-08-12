package workspace

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// tokenDeTeste nunca chega a autenticar nada — a fixture é local. Está aqui para que
// os testes possam provar que ele não aparece onde não deve.
const tokenDeTeste = "ghp_token_de_teste_que_nao_pode_vazar"

func TestPrepareEntregaOsArquivosDaBranchConfigurada(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)

	dir, release, err := ws.Prepare(context.Background(), projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer release()

	if got := leArquivo(t, dir, "app/handler.go"); got != "package app // main-1\n" {
		t.Errorf("app/handler.go = %q, esperado o conteúdo da branch main", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Errorf("README.md não veio no clone: %v", err)
	}
}

func TestPrepareEntregaABranchQueOProjetoConfigurou(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)
	ctx := context.Background()

	_, release, err := ws.Prepare(ctx, projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare main: %v", err)
	}
	release()

	dir, release, err := ws.Prepare(ctx, projeto(fx, "producao"))
	if err != nil {
		t.Fatalf("Prepare producao: %v", err)
	}
	defer release()

	if got := leArquivo(t, dir, "app/handler.go"); got != "package app // producao-1\n" {
		t.Errorf("app/handler.go = %q, esperado o conteúdo da branch producao", got)
	}
}

func TestPrepareAtualizaOCloneAntesDaAnalise(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)
	ctx := context.Background()

	dir, release, err := ws.Prepare(ctx, projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	release()

	// Uma marca dentro do .git sobrevive a uma atualização e some num re-clone. É o
	// que distingue os dois, e num repositório grande a diferença é entre segundos e
	// minutos de cada Análise.
	marca := filepath.Join(dir, ".git", "nightwatcher-marca")
	if err := os.WriteFile(marca, []byte("x"), 0o644); err != nil {
		t.Fatalf("gravando marca: %v", err)
	}

	commitNaFixture(t, fx, "main", "app/handler.go", "package app // main-2\n")

	dir2, release2, err := ws.Prepare(ctx, projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare depois do commit novo: %v", err)
	}
	defer release2()

	if dir2 != dir {
		t.Fatalf("Prepare devolveu outro diretório (%s, antes %s): o clone é por repositório", dir2, dir)
	}
	if got := leArquivo(t, dir2, "app/handler.go"); got != "package app // main-2\n" {
		t.Errorf("app/handler.go = %q, esperado o commit novo da branch", got)
	}
	if _, err := os.Stat(marca); err != nil {
		t.Errorf("o clone foi refeito em vez de atualizado: %v", err)
	}
}

func TestPrepareRecuperaCloneQuebradoSemIntervencao(t *testing.T) {
	requerGit(t)

	quebras := map[string]func(t *testing.T, dir string){
		"objetos apagados": func(t *testing.T, dir string) {
			rm(t, filepath.Join(dir, ".git", "objects"))
		},
		"HEAD corrompido": func(t *testing.T, dir string) {
			grava(t, filepath.Join(dir, ".git", "HEAD"), "lixo\n")
		},
		"clone pela metade": func(t *testing.T, dir string) {
			rm(t, filepath.Join(dir, ".git"))
		},
		"lock deixado por fetch morto": func(t *testing.T, dir string) {
			grava(t, filepath.Join(dir, ".git", "index.lock"), "")
		},
		"remoto reconfigurado": func(t *testing.T, dir string) {
			gitCru(t, dir, "config", "remote.origin.url", "file:///repositorio/que/nao/existe")
		},
	}

	for nome, quebra := range quebras {
		t.Run(nome, func(t *testing.T) {
			fx := repoFixture(t)
			ws := novoGit(t)
			ctx := context.Background()

			_, release, err := ws.Prepare(ctx, projeto(fx, "main"))
			if err != nil {
				t.Fatalf("Prepare inicial: %v", err)
			}
			release()

			quebra(t, filepath.Join(ws.root, cloneDir("file://"+fx)))

			dir, release2, err := ws.Prepare(ctx, projeto(fx, "main"))
			if err != nil {
				t.Fatalf("Prepare depois de %q: %v", nome, err)
			}
			defer release2()

			if got := leArquivo(t, dir, "app/handler.go"); got != "package app // main-1\n" {
				t.Errorf("app/handler.go = %q depois de %q", got, nome)
			}
		})
	}
}

func TestPreparesConcorrentesDoMesmoProjetoNaoQuebramAArvore(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)
	p := projeto(fx, "main")

	const analises, rodadas = 8, 3
	erros := make(chan error, analises*rodadas)
	var wg sync.WaitGroup
	for i := 0; i < analises; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rodadas; r++ {
				dir, release, err := ws.Prepare(context.Background(), p)
				if err != nil {
					erros <- fmt.Errorf("Prepare: %w", err)
					return
				}
				b, err := os.ReadFile(filepath.Join(dir, "app", "handler.go"))
				switch {
				case err != nil:
					erros <- fmt.Errorf("lendo app/handler.go: %w", err)
				case string(b) != "package app // main-1\n":
					erros <- fmt.Errorf("app/handler.go = %q", b)
				}
				release()
			}
		}()
	}
	wg.Wait()
	close(erros)
	for err := range erros {
		t.Error(err)
	}

	dir, release, err := ws.Prepare(context.Background(), p)
	if err != nil {
		t.Fatalf("Prepare depois da concorrência: %v", err)
	}
	defer release()
	if sujo := strings.TrimSpace(gitCru(t, dir, "status", "--porcelain")); sujo != "" {
		t.Errorf("árvore entregue com sobras:\n%s", sujo)
	}
	gitCru(t, dir, "fsck", "--no-progress", "--no-dangling")
}

// Dois Projetos podem apontar para o mesmo repositório em branches diferentes, e aí
// um clone só não serve os dois ao mesmo tempo. O desfecho aceitável é recusar; o
// inaceitável é entregar em silêncio a branch da outra Análise, porque o Diagnóstico
// sai errado sem que nada pareça errado.
func TestPrepareNaoEntregaOCloneNaBranchDeOutraAnalise(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)
	ctx := context.Background()

	dirProducao, releaseProducao, err := ws.Prepare(ctx, projeto(fx, "producao"))
	if err != nil {
		t.Fatalf("Prepare producao: %v", err)
	}
	defer releaseProducao()

	dir, release, err := ws.Prepare(ctx, projeto(fx, "main"))
	if err == nil {
		defer release()
		if got := leArquivo(t, dir, "app/handler.go"); got != "package app // main-1\n" {
			t.Errorf("quem pediu a branch main recebeu %q", got)
		}
	}

	// E a Análise que já estava lendo não pode ver o arquivo trocar debaixo dela.
	if got := leArquivo(t, dirProducao, "app/handler.go"); got != "package app // producao-1\n" {
		t.Errorf("a Análise de producao passou a ler %q", got)
	}
}

// O mesmo volume é montado pelo daemon e pelos contêineres efêmeros do `dokku run`,
// que reprocessam e simulam Ocorrências com o seu próprio Prepare. Um mutex de
// processo não enxerga isso; dois fetches no mesmo diretório é como o clone corrompe.
func TestPrepareNaoMexeNoCloneQueOutroProcessoEstaAtualizando(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)
	ctx := context.Background()

	dir, release, err := ws.Prepare(ctx, projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	release()

	// Outro descritor é o que o flock enxerga como outro dono, processo separado ou
	// não — é assim que este teste representa o contêiner do `dokku run`.
	outro, err := os.OpenFile(dir+lockSuffix, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("abrindo a trava do repositório: %v", err)
	}
	if err := syscall.Flock(int(outro.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("segurando a trava do repositório: %v", err)
	}

	commitNaFixture(t, fx, "main", "app/handler.go", "package app // main-2\n")
	ws.lockWait = 200 * time.Millisecond
	if _, _, err := ws.Prepare(ctx, projeto(fx, "main")); err == nil {
		t.Error("Prepare atualizou um clone que outro processo já estava mexendo")
	}
	if got := leArquivo(t, dir, "app/handler.go"); got != "package app // main-1\n" {
		t.Errorf("o clone mudou apesar da trava do outro processo: %q", got)
	}

	outro.Close()
	ws.lockWait = time.Minute
	dir2, release2, err := ws.Prepare(ctx, projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare com a trava já liberada: %v", err)
	}
	defer release2()
	if got := leArquivo(t, dir2, "app/handler.go"); got != "package app // main-2\n" {
		t.Errorf("app/handler.go = %q, esperado o commit novo depois da trava liberada", got)
	}
}

// Quem lê a Análise que falhou lê o motivo. Cancelamento reportado como estouro de
// tempo manda essa pessoa procurar rede lenta onde houve desligamento.
func TestPrepareInterrompidoNaoCulpaOTempoLimite(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := ws.Prepare(ctx, projeto(fx, "main"))
	if err == nil {
		t.Fatal("Prepare aceitou um contexto já cancelado")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("erro não identifica o cancelamento: %v", err)
	}
	if strings.Contains(err.Error(), "tempo limite") {
		t.Errorf("cancelamento reportado como estouro de tempo: %v", err)
	}
}

// A credencial não pode aparecer em argv: /proc/<pid>/cmdline é legível por qualquer
// conta da VPS e /proc/<pid>/environ não é. E ela é do GitHub — nenhum outro host a
// recebe, nem quando o Repo do Projeto foi digitado errado.
func TestCredencialViajaNoAmbienteESoParaOGitHub(t *testing.T) {
	cabecalho := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tokenDeTeste))

	casos := map[string]struct {
		repo      string
		autentica bool
	}{
		"github recebe a credencial":  {repo: "acme/acme", autentica: true},
		"host de fora não recebe":     {repo: "https://gitlab.example.com/acme/acme.git"},
		"host quase certo não recebe": {repo: "https://githubb.com/acme/acme.git"},
	}
	for nome, c := range casos {
		t.Run(nome, func(t *testing.T) {
			argv, ambiente, err := chamadaAoGit(t, c.repo)
			if err == nil {
				t.Fatal("esperado erro: o git de mentira sempre falha")
			}
			if strings.Contains(err.Error(), tokenDeTeste) || strings.Contains(err.Error(), cabecalho) {
				t.Errorf("o erro devolvido carrega a credencial: %v", err)
			}
			if !strings.Contains(argv, "clone") {
				t.Fatalf("o git de mentira não chegou a ser chamado para clonar:\n%s", argv)
			}
			if strings.Contains(argv, tokenDeTeste) || strings.Contains(argv, cabecalho) {
				t.Errorf("a credencial está na linha de comando do git:\n%s", argv)
			}
			switch noAmbiente := strings.Contains(ambiente, cabecalho); {
			case c.autentica && !noAmbiente:
				t.Error("a credencial não chegou ao git por caminho nenhum")
			case !c.autentica && noAmbiente:
				t.Errorf("a credencial do GitHub foi oferecida a %s", c.repo)
			}
			if !c.autentica && strings.Contains(ambiente, tokenDeTeste) {
				t.Errorf("o token cru foi para o ambiente de um git que fala com %s", c.repo)
			}
		})
	}
}

// Guarda o ponto onde a lista é aplicada, e não só o conteúdo dela: um dia alguém
// pode remover a consulta e a lista continuaria correta e inútil.
func TestGitNaoRodaSubcomandoQueEscreveNoRemoto(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)

	dir, release, err := ws.Prepare(context.Background(), projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer release()

	for _, args := range [][]string{
		{"push", "origin", "HEAD:refs/heads/tentativa"},
		{"commit", "--allow-empty", "-m", "tentativa"},
		{"tag", "tentativa"},
	} {
		if _, err := ws.run(context.Background(), "file://"+fx, dir, args...); err == nil {
			t.Errorf("git %s foi executado", args[0])
		}
	}
	if refs := gitCru(t, fx, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "tentativa") {
		t.Errorf("o remoto ganhou uma ref nova:\n%s", refs)
	}
}

func TestCloneEntregueNaoConsegueEscreverNoRemoto(t *testing.T) {
	requerGit(t)
	fx := repoFixture(t)
	ws := novoGit(t)

	dir, release, err := ws.Prepare(context.Background(), projeto(fx, "main"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer release()

	cmd := exec.Command("git", "-C", dir, "push", "origin", "HEAD:refs/heads/tentativa")
	cmd.Env = ambienteGit()
	saida, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("push a partir do clone entregue funcionou:\n%s", saida)
	}
	if refs := gitCru(t, fx, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "tentativa") {
		t.Errorf("o remoto ganhou uma ref nova:\n%s", refs)
	}

	// A credencial não pode ficar gravada no volume: o que está em .git/config está
	// no backup, e um clone sem credencial é um clone do qual não se escreve.
	cfg := leArquivo(t, dir, ".git/config")
	if strings.Contains(cfg, tokenDeTeste) {
		t.Errorf(".git/config guardou a credencial")
	}
}

// Guarda o mecanismo e não a implementação: no dia em que alguém acrescentar à lista
// um subcomando capaz de escrever no remoto, este teste cai.
func TestListaDeSubcomandosSoLeORemoto(t *testing.T) {
	for _, c := range []string{"push", "send-pack", "commit", "tag", "remote", "am", "apply", "gc"} {
		if readOnlyGitCommands[c] {
			t.Errorf("subcomando %q permitido: pode escrever onde o nightwatcher não escreve", c)
		}
	}
}

func TestPrepareRecusaConfiguracaoQueNaoDaParaClonar(t *testing.T) {
	requerGit(t)
	ws := novoGit(t)

	casos := map[string]domain.Project{
		"sem repositório":           {Name: "acme", Branch: "main"},
		"repo que não é owner/repo": {Name: "acme", Repo: "acme", Branch: "main"},
		"branch vazia":              {Name: "acme", Repo: "acme/acme", Branch: ""},
		"branch que parece flag":    {Name: "acme", Repo: "acme/acme", Branch: "--upload-pack=id"},
	}
	for nome, p := range casos {
		t.Run(nome, func(t *testing.T) {
			if _, _, err := ws.Prepare(context.Background(), p); err == nil {
				t.Errorf("Prepare aceitou %s", nome)
			}
		})
	}
}

func TestRemoteURL(t *testing.T) {
	casos := []struct {
		repo string
		want string
		erro bool
	}{
		{repo: "acme/acme", want: "https://github.com/acme/acme.git"},
		{repo: "  acme/acme.git  ", want: "https://github.com/acme/acme.git"},
		{repo: "https://github.com/acme/acme.git", want: "https://github.com/acme/acme.git"},
		{repo: "file:///srv/fixtures/acme", want: "file:///srv/fixtures/acme"},
		{repo: "/srv/fixtures/acme", want: "/srv/fixtures/acme"},
		{repo: "", erro: true},
		{repo: "acme", erro: true},
		{repo: "acme/acme/extra", erro: true},
		{repo: "git@github.com:acme/acme.git", erro: true},
		{repo: "acme/../../etc", erro: true},
	}
	for _, c := range casos {
		got, err := remoteURL(c.repo)
		if c.erro {
			if err == nil {
				t.Errorf("remoteURL(%q) = %q, esperado erro", c.repo, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("remoteURL(%q): %v", c.repo, err)
			continue
		}
		if got != c.want {
			t.Errorf("remoteURL(%q) = %q, esperado %q", c.repo, got, c.want)
		}
		if strings.Contains(got, "@") {
			t.Errorf("remoteURL(%q) = %q: a URL não pode carregar credencial", c.repo, got)
		}
	}
}

// --- fixture ---------------------------------------------------------------

func requerGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git não está no PATH")
	}
}

func novoGit(t *testing.T) *Git {
	t.Helper()
	g, err := New(t.TempDir(), tokenDeTeste)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func projeto(fixture, branch string) domain.Project {
	return domain.Project{ID: 1, Name: "acme", Repo: "file://" + fixture, Branch: branch}
}

// repoFixture monta um repositório de verdade com duas branches. Nada de rede.
func repoFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCru(t, dir, "init", "--quiet")
	// symbolic-ref em vez de --initial-branch: não depende da versão do git.
	gitCru(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	grava(t, filepath.Join(dir, "app", "handler.go"), "package app // main-1\n")
	grava(t, filepath.Join(dir, "README.md"), "acme\n")
	gitCru(t, dir, "add", "-A")
	gitCru(t, dir, "commit", "--quiet", "-m", "primeiro")

	gitCru(t, dir, "checkout", "--quiet", "-b", "producao")
	grava(t, filepath.Join(dir, "app", "handler.go"), "package app // producao-1\n")
	gitCru(t, dir, "commit", "--quiet", "-a", "-m", "producao")
	gitCru(t, dir, "checkout", "--quiet", "main")
	return dir
}

func commitNaFixture(t *testing.T, fx, branch, rel, conteudo string) {
	t.Helper()
	if atual := strings.TrimSpace(gitCru(t, fx, "rev-parse", "--abbrev-ref", "HEAD")); atual != branch {
		gitCru(t, fx, "checkout", "--quiet", branch)
	}
	grava(t, filepath.Join(fx, filepath.FromSlash(rel)), conteudo)
	gitCru(t, fx, "add", "-A")
	gitCru(t, fx, "commit", "--quiet", "-m", "atualiza "+rel)
}

// ambienteGit é montado do zero pelo mesmo motivo do adaptador: a configuração global
// de quem roda o teste não pode decidir o resultado.
func ambienteGit() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.TempDir(),
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=nightwatcher",
		"GIT_AUTHOR_EMAIL=nightwatcher@example.invalid",
		"GIT_COMMITTER_NAME=nightwatcher",
		"GIT_COMMITTER_EMAIL=nightwatcher@example.invalid",
	}
}

// chamadaAoGit põe na frente do PATH um "git" que só anota como foi chamado e falha,
// e devolve o argv e o ambiente que o adaptador realmente entregou ao processo. É a
// fronteira em que a pergunta "a credencial está na linha de comando?" tem resposta:
// olhar o código que monta os argumentos provaria o código, não o processo.
func chamadaAoGit(t *testing.T, repo string) (argv, ambiente string, err error) {
	t.Helper()
	shim := t.TempDir()
	logArgv := filepath.Join(shim, "argv")
	logAmbiente := filepath.Join(shim, "ambiente")
	grava(t, filepath.Join(shim, "git"), "#!/bin/sh\n"+
		"printf '%s\\n' \"$@\" >> '"+logArgv+"'\n"+
		"env >> '"+logAmbiente+"'\n"+
		"exit 1\n")
	if err := os.Chmod(filepath.Join(shim, "git"), 0o755); err != nil {
		t.Fatalf("tornando o git de mentira executável: %v", err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	ws, nerr := New(t.TempDir(), tokenDeTeste)
	if nerr != nil {
		t.Fatalf("New: %v", nerr)
	}
	_, _, err = ws.Prepare(context.Background(), domain.Project{Name: "acme", Repo: repo, Branch: "main"})
	return leSeExistir(t, logArgv), leSeExistir(t, logAmbiente), err
}

func leSeExistir(t *testing.T, caminho string) string {
	t.Helper()
	b, err := os.ReadFile(caminho)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("lendo %s: %v", caminho, err)
	}
	return string(b)
}

func gitCru(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = ambienteGit()
	saida, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s em %s: %v\n%s", strings.Join(args, " "), dir, err, saida)
	}
	return string(saida)
}

func grava(t *testing.T, caminho, conteudo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(caminho), 0o755); err != nil {
		t.Fatalf("criando %s: %v", filepath.Dir(caminho), err)
	}
	if err := os.WriteFile(caminho, []byte(conteudo), 0o644); err != nil {
		t.Fatalf("gravando %s: %v", caminho, err)
	}
}

func rm(t *testing.T, caminho string) {
	t.Helper()
	if err := os.RemoveAll(caminho); err != nil {
		t.Fatalf("removendo %s: %v", caminho, err)
	}
}

func leArquivo(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("lendo %s: %v", rel, err)
	}
	return string(b)
}

func TestNewLimpaCloneAbandonadoSemTocarNoQueEstaEmAndamento(t *testing.T) {
	requerGit(t)
	root := t.TempDir()

	abandonado := filepath.Join(root, tempClonePrefix+"morto")
	grava(t, filepath.Join(abandonado, "objects", "info", "packs"), "")
	antigo := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(abandonado, antigo, antigo); err != nil {
		t.Fatalf("envelhecendo o diretório: %v", err)
	}

	// Um clone que outro processo está montando agora mesmo: o volume é
	// compartilhado entre o daemon e os contêineres do `dokku run`.
	emAndamento := filepath.Join(root, tempClonePrefix+"vivo")
	grava(t, filepath.Join(emAndamento, "objects", "info", "packs"), "")

	if _, err := New(root, tokenDeTeste); err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := os.Stat(abandonado); !os.IsNotExist(err) {
		t.Errorf("o clone abandonado continua ocupando o volume (err=%v)", err)
	}
	if _, err := os.Stat(emAndamento); err != nil {
		t.Errorf("o clone em andamento de outro processo foi apagado: %v", err)
	}
}
