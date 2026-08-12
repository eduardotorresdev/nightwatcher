package agentcli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// cliInstallDir is where the repository's Dockerfile puts the Claude CLI: the native
// installer writes into the image user's own home, and the image's PATH points there.
//
// It matters here because a tmpfs SHADOWS everything under its mount point. A writable
// HOME mounted at or above this directory hides the very binary `--entrypoint` names,
// and every container then dies at creation with "executable file not found in $PATH".
const cliInstallDir = "/home/nightwatcher/.local/bin"

// nightwatcherBinary is where the repository's Dockerfile puts the Go binary, and it
// is the image's own ENTRYPOINT. SidecarCommand names it explicitly for the same
// reason DockerOptions.Entrypoint exists: a `docker run` that does not say otherwise
// gets the daemon, not the command it meant to run.
const nightwatcherBinary = "/usr/local/bin/nightwatcher"

// NightwatcherBinary is that path, for the caller that needs to run this binary inside
// the Análise's perimeter.
func NightwatcherBinary() string { return nightwatcherBinary }

// containerTmp is always a tmpfs: the root filesystem is read-only and something has
// to be writable besides HOME.
const containerTmp = "/tmp"

// DockerRunner raises one ephemeral container per Análise and lets it die at the
// end. This is the perimeter: unprivileged user, read-only root filesystem with
// a writable tmpfs at HOME, the clone mounted read-only, egress restricted by the
// network, and an environment built from scratch. The CLI flags are hygiene; this is
// the containment.
type DockerRunner struct {
	Opts DockerOptions
}

// DockerOptions is the shape of that perimeter. The defaults match the image built by
// the repository's Dockerfile.
type DockerOptions struct {
	// Image carries the Go binary AND the Claude CLI — the same image the daemon
	// runs from.
	Image string

	// Docker is the client binary.
	Docker string

	// Entrypoint has to be given explicitly: the image's own ENTRYPOINT is the
	// nightwatcher binary, so without this the container would run the daemon's CLI
	// instead of Claude.
	Entrypoint string

	// User is the unprivileged uid:gid baked into the image.
	User string

	// Network is a docker network whose egress is restricted to the Anthropic
	// endpoint. It is always passed: a missing network makes docker fail loudly,
	// which is much better than a session running with unrestricted egress because a
	// flag was quietly omitted.
	Network string

	// Home is where the writable tmpfs is mounted inside the container. The CLI
	// writes .claude.json during the run, so a read-only filesystem alone breaks it.
	// It must NOT be the image user's home directory: that is where the
	// CLI itself is installed, and a tmpfs over it hides the binary. See
	// cliInstallDir.
	Home string

	// MountPoint is where the clone appears inside the container, read-only.
	MountPoint string

	// TmpfsSize bounds the writable scratch, so a session cannot fill the host's RAM.
	TmpfsSize string
	Memory    string
	PidsLimit int

	// ClientEnv is what the docker CLI itself needs — DOCKER_HOST, and HOME if the
	// daemon authenticates against a registry. It is NOT the session's environment.
	ClientEnv map[string]string

	// ExtraArgs are appended to `docker run` before the image, for whatever the
	// deployment needs and this struct did not predict.
	ExtraArgs []string
}

// DefaultDockerOptions matches the repository's Dockerfile.
//
// HOME is /tmp and not the image user's home on purpose: the Dockerfile installs the
// CLI into /home/nightwatcher/.local, so the writable tmpfs has to live somewhere that
// does not shadow it. /tmp exists in the base image, is already a tmpfs here, and is
// thrown away with the container.
func DefaultDockerOptions() DockerOptions {
	return DockerOptions{
		Image:      "nightwatcher",
		Docker:     "docker",
		Entrypoint: DefaultBinary,
		User:       "10001:10001",
		Network:    "nightwatcher-agent",
		Home:       containerTmp,
		MountPoint: "/workspace",
		TmpfsSize:  "128m",
		Memory:     "2g",
		PidsLimit:  256,
	}
}

func (o DockerOptions) withDefaults() DockerOptions {
	d := DefaultDockerOptions()
	if o.Image != "" {
		d.Image = o.Image
	}
	if o.Docker != "" {
		d.Docker = o.Docker
	}
	if o.Entrypoint != "" {
		d.Entrypoint = o.Entrypoint
	}
	if o.User != "" {
		d.User = o.User
	}
	if o.Network != "" {
		d.Network = o.Network
	}
	if o.Home != "" {
		d.Home = o.Home
	}
	if o.MountPoint != "" {
		d.MountPoint = o.MountPoint
	}
	if o.TmpfsSize != "" {
		d.TmpfsSize = o.TmpfsSize
	}
	if o.Memory != "" {
		d.Memory = o.Memory
	}
	if o.PidsLimit > 0 {
		d.PidsLimit = o.PidsLimit
	}
	d.ClientEnv = o.ClientEnv
	d.ExtraArgs = o.ExtraArgs
	return d
}

func (r DockerRunner) Run(ctx context.Context, spec Spec) (Outcome, error) {
	o := r.Opts.withDefaults()
	if err := o.validate(); err != nil {
		return Outcome{}, err
	}
	args, clientEnv := o.command(spec)
	// The clone is mounted, not entered: the docker client itself runs from wherever
	// the daemon is, so no working directory is handed down.
	return runProcess(ctx, o.Docker, args, clientEnv, "", spec)
}

// command builds the `docker run` argv and the environment of the docker client.
//
// The split matters: the session's variables are passed by NAME (`--env FOO`), so
// docker reads their values out of the client's own environment. Passing
// `--env FOO=value` instead would put the subscription token in argv, where `ps`
// shows it to every process on the host.
func (o DockerOptions) command(spec Spec) ([]string, map[string]string) {
	args := []string{
		"run", "--rm", "--interactive",
		"--user", o.User,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", o.Network,
		"--memory", o.Memory,
		"--pids-limit", strconv.Itoa(o.PidsLimit),
	}
	// mode=1777 because a tmpfs is created owned by root: without it the unprivileged
	// user cannot write, and the CLI cannot create .claude.json. The list is deduped
	// because HOME defaults to /tmp and docker refuses the same mount point twice.
	for _, dir := range writableDirs(o.Home) {
		args = append(args, "--tmpfs", dir+":rw,mode=1777,size="+o.TmpfsSize)
	}

	workdir := o.Home
	if spec.WorkDir != "" {
		args = append(args, "--volume", spec.WorkDir+":"+o.MountPoint+":ro")
		workdir = o.MountPoint
	}
	args = append(args, "--workdir", workdir)

	// HOME is the container's tmpfs whatever the caller had in mind: the host path is
	// meaningless in here, and this is the only writable place.
	args = append(args, "--env", "HOME="+o.Home)

	clientEnv := map[string]string{}
	for k, v := range o.ClientEnv {
		clientEnv[k] = v
	}
	if p, ok := spec.Env["PATH"]; ok && clientEnv["PATH"] == "" {
		clientEnv["PATH"] = p
	}
	for _, k := range sortedKeys(spec.Env) {
		switch k {
		case "HOME", "PATH":
			// HOME is set above; PATH belongs to the image, not to the daemon's host.
			continue
		}
		args = append(args, "--env", k)
		clientEnv[k] = spec.Env[k]
	}

	args = append(args, o.ExtraArgs...)
	args = append(args, "--entrypoint", o.Entrypoint, o.Image)
	return append(args, spec.Args...), clientEnv
}

// validate refuses a perimeter that would hide the CLI from itself. Getting this wrong
// does not degrade the session — it kills the container before it starts, on every
// single Análise, with a docker error that says nothing about the mount that caused it.
func (o DockerOptions) validate() error {
	for _, m := range []struct{ what, path string }{
		{"o HOME gravável", o.Home},
		{"o ponto de montagem do clone", o.MountPoint},
	} {
		if covers(m.path, cliInstallDir) {
			return fmt.Errorf(
				"%s (%s) cobre %s, onde o CLI está instalado: o container subiria sem o binário %s",
				m.what, m.path, cliInstallDir, DefaultBinary)
		}
	}
	return nil
}

// covers reports whether mounting at dir would shadow path.
func covers(dir, path string) bool {
	if dir == "" {
		return false
	}
	if dir == "/" {
		return true
	}
	dir = strings.TrimSuffix(dir, "/")
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// writableDirs is the set of tmpfs mount points, in a stable order and without
// duplicates.
func writableDirs(home string) []string {
	if home == "" || home == containerTmp {
		return []string{containerTmp}
	}
	return []string{home, containerTmp}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SidecarCommand builds a `docker run` argv that runs some other binary inside the
// SAME perimeter an Análise gets — above all the same network.
//
// It exists for one question that cannot be answered from the daemon's own process:
// "can the Análise reach this host?". The daemon and the ephemeral container sit on
// different networks by design — `nightwatcher-agent` restricts egress — so a probe
// that succeeds from here proves nothing about there. Running the probe in the real
// perimeter is the only honest answer, and this is the seam that allows it without
// the word "docker" escaping this file.
//
// envNames are passed by NAME, never as `--env K=V`: /proc/<pid>/cmdline is
// world-readable, and the whole point of the probe is that it carries a credential.
func (o DockerOptions) SidecarCommand(entrypoint string, args, envNames []string) ([]string, map[string]string) {
	d := o.withDefaults()

	cmd := []string{
		"run", "--rm",
		"--user", d.User,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", d.Network,
		"--memory", d.Memory,
		"--pids-limit", strconv.Itoa(d.PidsLimit),
	}
	for _, dir := range writableDirs(d.Home) {
		cmd = append(cmd, "--tmpfs", dir+":rw,mode=1777,size="+d.TmpfsSize)
	}
	cmd = append(cmd, "--workdir", d.Home, "--env", "HOME="+d.Home)

	clientEnv := map[string]string{}
	for k, v := range d.ClientEnv {
		clientEnv[k] = v
	}
	for _, name := range envNames {
		cmd = append(cmd, "--env", name)
	}

	cmd = append(cmd, d.ExtraArgs...)
	cmd = append(cmd, "--entrypoint", entrypoint, d.Image)
	return append(cmd, args...), clientEnv
}
