package agentcli

import (
	"context"
	"strings"
	"testing"
)

func sessionSpec() Spec {
	return Spec{
		Args:    []string{"--print", "--tools", "Read,Grep,Glob"},
		Env:     map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "segredo-da-assinatura", "PATH": "/usr/local/bin:/usr/bin", "HOME": "/home/nightwatcher-host"},
		WorkDir: "/data/clones/acme",
		Stdin:   "prompt",
	}
}

// The perimeter is the container, so these flags are the actual containment and not
// hygiene: unprivileged user, read-only root, writable tmpfs at HOME, the clone
// mounted read-only, and egress restricted by the network.
func TestDockerCommandRaisesThePerimeter(t *testing.T) {
	args, _ := DefaultDockerOptions().command(sessionSpec())
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--read-only",
		"--user 10001:10001",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--network nightwatcher-agent",
		"--tmpfs /tmp:rw,mode=1777,size=128m",
		"--volume /data/clones/acme:/workspace:ro",
		"--workdir /workspace",
		"--env HOME=/tmp",
		"--rm",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("o perímetro não contém %q\nargv: %s", want, joined)
		}
	}

	// The image's own ENTRYPOINT is the nightwatcher binary; without this the
	// container would raise the daemon's CLI instead of Claude.
	if !strings.Contains(joined, "--entrypoint "+DefaultBinary) {
		t.Errorf("faltou --entrypoint: %s", joined)
	}
}

// A name-only --env makes docker read the value from the client's own environment.
// Passing --env NAME=value instead would put the subscription token in argv, where
// `ps` shows it to every process on the host.
func TestSubscriptionTokenNeverReachesTheArgv(t *testing.T) {
	args, clientEnv := DefaultDockerOptions().command(sessionSpec())
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "segredo-da-assinatura") {
		t.Fatalf("o token foi parar no argv, visível em `ps`:\n%s", joined)
	}
	if !strings.Contains(joined, "--env CLAUDE_CODE_OAUTH_TOKEN ") && !strings.HasSuffix(joined, "--env CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("o token não foi repassado por nome: %s", joined)
	}
	if clientEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "segredo-da-assinatura" {
		t.Errorf("o cliente docker não tem o valor para repassar: %#v", clientEnv)
	}
	// The host's HOME is meaningless inside the container, and the only writable
	// place in there is the tmpfs.
	if strings.Contains(joined, "/home/nightwatcher-host") {
		t.Errorf("o HOME do host vazou para o container: %s", joined)
	}
}

// The CLI's own arguments come after the image, or docker eats them.
func TestCLIArgsComeAfterTheImage(t *testing.T) {
	o := DefaultDockerOptions()
	args, _ := o.command(sessionSpec())

	image, printFlag := -1, -1
	for i, a := range args {
		switch a {
		case o.Image:
			image = i
		case "--print":
			printFlag = i
		}
	}
	if image < 0 || printFlag < 0 || printFlag < image {
		t.Fatalf("imagem em %d, --print em %d: %v", image, printFlag, args)
	}
}

// The Triagem mounts no repository at all, so there is nothing to bind and the working
// directory is the only writable place there is.
func TestTriageContainerMountsNoRepository(t *testing.T) {
	spec := sessionSpec()
	spec.WorkDir = ""
	args, _ := DefaultDockerOptions().command(spec)
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--volume") {
		t.Errorf("a triagem montou alguma coisa: %s", joined)
	}
	if !strings.Contains(joined, "--workdir /tmp") {
		t.Errorf("diretório de trabalho errado: %s", joined)
	}
}

// A tmpfs hides everything under its mount point. The Dockerfile installs the CLI in
// the image user's home, so mounting the writable HOME there would raise a container
// with no `claude` in it — every Análise dying at container creation, with a docker
// error that never mentions the mount that caused it.
func TestWritableMountsNeverHideTheCLI(t *testing.T) {
	args, _ := DefaultDockerOptions().command(sessionSpec())

	for i, a := range args {
		var dest string
		switch a {
		case "--tmpfs": // <destino>:<opções>
			dest = strings.SplitN(args[i+1], ":", 2)[0]
		case "--volume": // <origem>:<destino>:<opções>
			dest = strings.SplitN(args[i+1], ":", 3)[1]
		default:
			continue
		}
		if covers(dest, cliInstallDir) {
			t.Errorf("%s %s cobre %s: o container subiria sem o CLI", a, dest, cliInstallDir)
		}
	}
}

// And a caller who configures the old, broken HOME finds out from the nightwatcher
// instead of from a container that will not start.
func TestHomeOverTheCLIInstallIsRefused(t *testing.T) {
	r := DockerRunner{Opts: DockerOptions{Home: "/home/nightwatcher"}}

	_, err := r.Run(context.Background(), sessionSpec())
	if err == nil {
		t.Fatal("um HOME por cima da instalação do CLI foi aceito")
	}
	if !strings.Contains(err.Error(), cliInstallDir) {
		t.Errorf("o erro não diz o que foi coberto: %v", err)
	}
}

// Both writable directories are mounted, and neither twice — docker refuses a repeated
// mount point, and HOME defaults to /tmp.
func TestWritableMountsAreNotDuplicated(t *testing.T) {
	o := DefaultDockerOptions()
	o.Home = "/session"
	args, _ := o.command(sessionSpec())

	seen := map[string]int{}
	for i, a := range args {
		if a == "--tmpfs" {
			seen[strings.SplitN(args[i+1], ":", 2)[0]]++
		}
	}
	if seen["/session"] != 1 || seen["/tmp"] != 1 {
		t.Errorf("montagens graváveis: %v", seen)
	}

	args, _ = DefaultDockerOptions().command(sessionSpec())
	n := 0
	for _, a := range args {
		if a == "--tmpfs" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("HOME é /tmp, esperava um único --tmpfs, veio %d: %v", n, args)
	}
}

// The probe that decides whether an MCP server is usable runs in the Análise's own
// perimeter, because the daemon and the ephemeral container sit on different networks
// by design — and the credential it carries must not reach the argv any more than the
// subscription token does.
func TestSidecarRunsInTheAgentNetworkWithoutLeakingTheCredential(t *testing.T) {
	o := DefaultDockerOptions()

	args, clientEnv := o.SidecarCommand(NightwatcherBinary(),
		[]string{"mcp", "probe", "--url", "https://mcp.exemplo/mcp"},
		[]string{"NW_MCP_EXEMPLO"})

	line := strings.Join(args, " ")
	for _, want := range []string{
		"--network nightwatcher-agent",
		"--read-only",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--entrypoint " + NightwatcherBinary(),
		"mcp probe",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("argv não contém %q:\n%s", want, line)
		}
	}
	// By name. `--env NW_MCP_EXEMPLO=valor` would put the credential where `ps` shows
	// it to every account on the host.
	if !strings.Contains(line, "--env NW_MCP_EXEMPLO ") {
		t.Errorf("a variável não foi passada por nome:\n%s", line)
	}
	if strings.Contains(line, "NW_MCP_EXEMPLO=") {
		t.Errorf("a variável foi passada com valor no argv:\n%s", line)
	}
	if clientEnv == nil {
		t.Error("o ambiente do cliente docker veio nil")
	}
}
