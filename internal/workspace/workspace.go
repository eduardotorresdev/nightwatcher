// Package workspace delivers the Projeto's directory ready for READING, and nothing
// more. It keeps one clone per repository under a root on the persistent volume,
// brings it to the configured branch before every Análise — so the Diagnóstico never
// points at code that has already changed — and hands the path to the session, which
// mounts it read-only.
//
// That the nightwatcher never creates a branch, a commit or a pull request is made
// structurally true here rather than merely intended:
//
//   - run() refuses any git subcommand outside a fixed allowlist, and every entry in
//     that allowlist can only write locally;
//   - the credential never reaches the clone — it travels in the environment of our
//     own git process, so .git/config on the volume holds no way to authenticate a
//     write;
//   - the clone's remote.origin.pushurl is set to a protocol git cannot speak, so a
//     push attempted from inside that directory dies before any network I/O.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// DefaultCommandTimeout caps a single git invocation. The first clone of a large
// repository on a VPS with two vCPUs is the slow case; everything after it is a
// depth-1 fetch. The ceiling exists so a hung transfer fails the Análise instead of
// occupying a worker forever.
const DefaultCommandTimeout = 15 * time.Minute

// blockedPushURL is deliberately a protocol git does not implement. It is written
// into every clone so that `git push` from that directory fails at URL parsing,
// before resolving a host or opening a socket.
const blockedPushURL = "no-push://nightwatcher.invalido/somente-leitura"

// tempClonePrefix marks the directory a clone is assembled in before it is renamed
// into place. The rename is what makes a clone appear whole or not at all.
const tempClonePrefix = ".clone-"

// lockSuffix names the file that carries the on-disk half of the per-repository
// lock, next to the clone and never inside it — recreate() deletes the clone
// directory, and a lock nobody can hold is not a lock.
const lockSuffix = ".lock"

// lockPoll is how often a Prepare waiting on another process retries. flock has no
// context-aware form, so the wait is a poll.
const lockPoll = 200 * time.Millisecond

// maxErrOutput caps how much of git's stderr travels inside an error. Enough to
// diagnose, not enough to bury the message.
const maxErrOutput = 4000

// readOnlyGitCommands is the whole set of git subcommands this adapter may invoke.
// It is an allowlist and not a denylist on purpose: keeping "never writes to the
// remote" true while other people edit this file means making the wrong call
// impossible here, not merely absent. Every command below writes at most to the
// local clone.
var readOnlyGitCommands = map[string]bool{
	"cat-file":  true,
	"checkout":  true,
	"clean":     true,
	"clone":     true,
	"config":    true,
	"fetch":     true,
	"rev-parse": true,
}

// Git is the domain.Workspace backed by the git binary.
type Git struct {
	root     string
	token    string
	gitBin   string
	timeout  time.Duration
	lockWait time.Duration

	mu    sync.Mutex
	repos map[string]*repoState
}

var _ domain.Workspace = (*Git)(nil)

// repoState is the per-repository bookkeeping. Its mutex is what stops two fetches
// from landing in one directory, which is how a clone gets corrupted.
type repoState struct {
	mu      sync.Mutex
	readers int
	// branch is the branch the clone currently holds. It exists so that a Prepare
	// which cannot update the clone — because another Análise is reading it — can
	// tell "already at the branch you asked for" from "at somebody else's branch",
	// instead of handing out a tree from the wrong branch.
	branch string
}

// Option tunes the adapter. There is one on purpose: everything else is a decision,
// not a setting.
type Option func(*Git)

// WithCommandTimeout overrides the per-invocation ceiling.
func WithCommandTimeout(d time.Duration) Option {
	return func(g *Git) {
		if d > 0 {
			g.timeout = d
		}
	}
}

// New prepares the clone root. token is the global GitHub credential, read only; an
// empty token is valid and means the remotes are reachable without one.
func New(root, token string, opts ...Option) (*Git, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("diretório raiz dos clones não configurado")
	}
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git não encontrado no PATH: %w", err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolvendo raiz dos clones %s: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("criando raiz dos clones %s: %w", abs, err)
	}
	g := &Git{
		root:    abs,
		token:   token,
		gitBin:  bin,
		timeout: DefaultCommandTimeout,
		repos:   make(map[string]*repoState),
	}
	for _, o := range opts {
		o(g)
	}
	// Waiting on another process is bounded by the same ceiling as the transfer it
	// is waiting for: whatever holds the lock cannot outlive its own timeout.
	if g.lockWait <= 0 {
		g.lockWait = g.timeout
	}
	g.sweepAbandonedClones()
	return g, nil
}

// sweepAbandonedClones removes the temporary directories of clones that were killed
// before the rename that installs them — a SIGKILLed process leaves one behind, and
// on a repository the size of acme those add up until the volume is full. Only the
// old ones go: the daemon and the ephemeral `dokku run` containers share this volume,
// and a clone in progress somewhere else must not be swept out from under it. Best
// effort — a full disk is a problem for later, a failed startup is a problem now.
func (g *Git) sweepAbandonedClones() {
	entries, err := os.ReadDir(g.root)
	if err != nil {
		return
	}
	limite := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), tempClonePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(limite) {
			continue
		}
		os.RemoveAll(filepath.Join(g.root, e.Name()))
	}
}

// target is everything one Prepare needs to know, resolved once.
type target struct {
	remote string
	branch string
	dir    string
}

// Prepare returns the Projeto's directory at the configured branch. The release
// function must be called when the Análise is done; it is safe to call more than
// once, and is nil whenever the error is not.
//
// It fails rather than improvise when the clone cannot be brought to the configured
// branch right now — another process is updating it, or another Análise is reading
// it on a different branch. Both clear on their own, so the retry is the answer;
// handing over a tree from the wrong branch would not be.
func (g *Git) Prepare(ctx context.Context, p domain.Project) (string, func(), error) {
	remote, err := remoteURL(p.Repo)
	if err != nil {
		return "", nil, fmt.Errorf("projeto %s: %w", p.Name, err)
	}
	branch := strings.TrimSpace(p.Branch)
	if !validBranch(branch) {
		return "", nil, fmt.Errorf("projeto %s: branch %q inválida", p.Name, p.Branch)
	}
	t := target{remote: remote, branch: branch, dir: filepath.Join(g.root, cloneDir(remote))}

	// Keyed by repository, as is the clone directory: two Projetos on the same repo
	// share both, and sharing the lock is exactly what keeps their fetches apart.
	st := g.state(t.dir)

	st.mu.Lock()
	// Another Análise is reading this clone on a different branch — two Projetos can
	// share a repository and configure different branches. Moving the clone would
	// yank the tree out from under that Análise; leaving it would hand this one code
	// from a branch it did not ask for, and a Diagnóstico written against the wrong
	// branch is wrong in a way nobody can see. So: neither. The Análise fails, is
	// retried, and by then the reader is usually gone.
	if st.readers > 0 && st.branch != t.branch {
		st.mu.Unlock()
		return "", nil, fmt.Errorf("projeto %s: clone de %s está na branch %q para outra Análise em andamento; %q fica para a retentativa",
			p.Name, t.remote, st.branch, t.branch)
	}
	// The mutex above only binds this process, and the volume is not this process's
	// alone: `dokku run` containers mount it to reprocess and to simulate, and they
	// run their own Prepare against the same directory. The file lock is the half of
	// the per-repository lock that those processes can see.
	unlock, err := g.lockRepo(ctx, t.dir)
	if err != nil {
		st.mu.Unlock()
		return "", nil, fmt.Errorf("projeto %s: %w", p.Name, err)
	}
	switch sound := g.sound(ctx, t); {
	case !sound:
		// A directory that is not a healthy repository is re-created even while
		// another Análise is reading it: that Análise is already looking at a broken
		// tree, so replacing it can only help. Recovering from a corrupted or
		// half-finished clone without anyone logging into the VPS is the point.
		err = g.recreate(ctx, t)
	case st.readers == 0:
		if uerr := g.update(ctx, t); uerr != nil {
			// A killed fetch leaves a lock file, an interrupted checkout leaves a
			// half-written index, a truncated pack leaves objects that resolve and
			// then do not read. Diagnosing which one it was on the VPS is worth less
			// than throwing the directory away.
			if rerr := g.recreate(ctx, t); rerr != nil {
				err = errors.Join(uerr, rerr)
			}
		}
	default:
		// Another Análise of this same Projeto is reading the clone right now, and
		// it is on the branch this one wants. Fetching under it would pull files out
		// from beneath a session that is mid-read, which is worse than the staleness
		// — and that clone was brought to the branch when that Análise started,
		// minutes ago.
	}
	// The lock covers the whole update and nothing after it: the Análises that follow
	// only read, and they read concurrently by design.
	unlock()
	if err != nil {
		st.mu.Unlock()
		return "", nil, err
	}
	st.branch = t.branch
	st.readers++
	st.mu.Unlock()

	// The lock is released before the Análise begins, not after: serialising the
	// updates is required, serialising the Análises is not.
	var once sync.Once
	release := func() {
		once.Do(func() {
			st.mu.Lock()
			if st.readers > 0 {
				st.readers--
			}
			st.mu.Unlock()
		})
	}
	return t.dir, release, nil
}

// lockRepo takes the per-repository file lock and returns the function that drops
// it. flock is released by the kernel when the file descriptor closes, so a process
// killed mid-update leaves no stale lock behind — which matters, because the process
// most likely to be killed here is the one holding it.
func (g *Git) lockRepo(ctx context.Context, dir string) (func(), error) {
	f, err := os.OpenFile(dir+lockSuffix, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("abrindo a trava do clone: %w", err)
	}
	deadline := time.Now().Add(g.lockWait)
	for {
		switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
		case err == nil:
			return func() { f.Close() }, nil
		case !errors.Is(err, syscall.EWOULDBLOCK):
			f.Close()
			return nil, fmt.Errorf("travando o clone: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("clone ocupado por outro processo há mais de %s", g.lockWait)
		}
		t := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, fmt.Errorf("esperando a trava do clone: %w", ctx.Err())
		case <-t.C:
		}
	}
}

func (g *Git) state(key string) *repoState {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.repos[key]
	if st == nil {
		st = &repoState{}
		g.repos[key] = st
	}
	return st
}

// sound reports whether dir is a git repository this adapter can still update.
func (g *Git) sound(ctx context.Context, t target) bool {
	// The plain stat comes first because git's repository discovery walks upwards:
	// on an empty or half-deleted directory git would cheerfully answer about a
	// repository somewhere above it, and we would then update the wrong tree.
	fi, err := os.Stat(filepath.Join(t.dir, ".git"))
	if err != nil || !fi.IsDir() {
		return false
	}
	// Peeling HEAD to its tree AND finding that tree in the object database, in one
	// call: a name that resolves is not the same as an object that reads, and a
	// transfer killed mid-write leaves a repository that answers rev-parse and then
	// cannot produce a single file.
	if _, err := g.run(ctx, t.remote, t.dir, "cat-file", "-e", "HEAD^{tree}"); err != nil {
		return false
	}
	// A Projeto whose repository was reconfigured gets a new clone rather than a
	// fetch from the old remote.
	origin, err := g.run(ctx, t.remote, t.dir, "config", "--get", "remote.origin.url")
	return err == nil && strings.TrimSpace(origin) == t.remote
}

// update brings an existing clone to the tip of the configured branch.
func (g *Git) update(ctx context.Context, t target) error {
	spec := "+refs/heads/" + t.branch + ":refs/remotes/origin/" + t.branch
	if _, err := g.run(ctx, t.remote, t.dir, "fetch", "--depth", "1", "--no-tags", "--force", "origin", spec); err != nil {
		return err
	}
	// --force -B rather than a plain reset: it follows a force-push on the branch,
	// it repairs a checkout left half-done, and it moves the clone when the Projeto's
	// branch is reconfigured — all without another full transfer.
	if _, err := g.run(ctx, t.remote, t.dir, "checkout", "--force", "-B", t.branch, "refs/remotes/origin/"+t.branch); err != nil {
		return err
	}
	// The session must see the branch and only the branch. Nothing should be writing
	// here — the mount is read-only — but a clone interrupted halfway can leave files
	// that belong to no commit, and a Diagnóstico citing one of them is a wrong
	// answer nobody can reproduce.
	_, err := g.run(ctx, t.remote, t.dir, "clean", "-ffdx")
	return err
}

// recreate throws the directory away and clones again, installing the result with a
// rename so a failure halfway leaves the previous clone untouched.
func (g *Git) recreate(ctx context.Context, t target) error {
	tmp, err := os.MkdirTemp(g.root, tempClonePrefix)
	if err != nil {
		return fmt.Errorf("criando diretório temporário do clone: %w", err)
	}
	// A no-op once the rename below has succeeded.
	defer os.RemoveAll(tmp)

	// Shallow, single branch, no tags: the Diagnóstico reads current code and never
	// needs history, and this is the smallest thing that lands on a VPS with two
	// vCPUs and ~60 GB free. Deliberately NOT a partial clone (--filter): a partial
	// clone leaves a promisor remote behind, and then reading a file whose blob was
	// never fetched becomes a network call — from inside a container that has no
	// egress and a read-only mount. A missing object has to be impossible, not lazy.
	if _, err := g.run(ctx, t.remote, "", "clone", "--depth", "1", "--single-branch",
		"--no-tags", "--branch", t.branch, "--", t.remote, tmp); err != nil {
		return err
	}
	// Written into every clone, including the ones nobody will ever push from.
	if _, err := g.run(ctx, t.remote, tmp, "config", "remote.origin.pushurl", blockedPushURL); err != nil {
		return err
	}
	if err := os.RemoveAll(t.dir); err != nil {
		return fmt.Errorf("descartando clone anterior em %s: %w", t.dir, err)
	}
	if err := os.Rename(tmp, t.dir); err != nil {
		return fmt.Errorf("instalando clone em %s: %w", t.dir, err)
	}
	return nil
}

// run invokes git. dir empty means no -C, which only the clone needs.
func (g *Git) run(ctx context.Context, remote, dir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("invocação de git sem subcomando")
	}
	if !readOnlyGitCommands[args[0]] {
		return "", fmt.Errorf("subcomando git não permitido: %q — o nightwatcher só lê o repositório", args[0])
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}

	cmd := exec.CommandContext(ctx, g.gitBin, full...)
	cmd.Dir = g.root
	cmd.Env = g.env(remote)
	cmd.Stdin = nil
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	// SIGTERM before SIGKILL: git removes its lock files on SIGTERM, and a SIGKILLed
	// fetch is precisely the half-finished state this adapter then has to recover
	// from with a full re-clone.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Run(); err != nil {
		switch {
		// The caller giving up is not a timeout of ours. Telling them the clone took
		// fifteen minutes when the daemon was shutting down is a motivo that sends
		// whoever reads the failed Análise looking for a slow network.
		case parent.Err() != nil:
			return "", fmt.Errorf("git %s interrompido: %w", args[0], parent.Err())
		case ctx.Err() != nil:
			return "", fmt.Errorf("git %s excedeu o tempo limite de %s", args[0], g.timeout)
		}
		return "", fmt.Errorf("git %s falhou: %w: %s", args[0], err, g.redact(tail(errOut.String())))
	}
	return out.String(), nil
}

// env builds the environment from scratch instead of inheriting it. A gitconfig on
// the machine can rewrite URLs with insteadOf, install a credential helper or point
// core.hooksPath somewhere; none of that may decide what the nightwatcher fetches.
func (g *Git) env(remote string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"PATH=" + path,
		// HOME points at the clone root so that no ~/.gitconfig and no
		// ~/.git-credentials of the host account are in reach.
		"HOME=" + g.root,
		"LC_ALL=C",
		// A daemon must never sit waiting for a username at a terminal that is not
		// there; a bad credential has to fail, not hang.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}

	// The token goes in through GIT_CONFIG_VALUE_n and never through `git -c` or the
	// remote URL: /proc/<pid>/cmdline is world-readable, so anything on the command
	// line is visible to every account on the VPS, while /proc/<pid>/environ is
	// readable only by the process owner. Keeping it out of the URL also keeps it out
	// of .git/config, where it would sit in plain text on the persistent volume and
	// in every backup of it.
	cfg := [][2]string{
		// Empty value resets the helper chain: nothing may supply a stored credential
		// and nothing may write ours down.
		{"credential.helper", ""},
	}
	if h := g.authHeader(remote); h != "" {
		// Scoped to the origin so the header is not offered to some other host.
		cfg = append(cfg, [2]string{"http." + originPrefix(remote) + ".extraHeader", h})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]),
		)
	}
	return env
}

// authHeader is the HTTP Basic header GitHub accepts for a token over HTTPS. The
// username x-access-token is the documented one for installation tokens and is
// ignored for personal tokens, so one form covers both.
//
// The credential is offered to github.com and to nothing else. A Projeto's Repo is
// typed by an operator and passed through verbatim when it is already a URL, so
// "any https host" would mean a typo — githubb.com, or a host somebody else
// controls — receiving the credential that opens every repository we can read.
func (g *Git) authHeader(remote string) string {
	if g.token == "" {
		return ""
	}
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return ""
	}
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+g.token))
}

// redact keeps the credential out of anything that can be logged or stored on an
// issue. git has no reason to echo it, which is exactly why the day it does would be
// the day nobody is checking.
func (g *Git) redact(s string) string {
	if g.token == "" {
		return s
	}
	s = strings.ReplaceAll(s, g.token, "***")
	return strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+g.token)), "***")
}

func originPrefix(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/"
}

// remoteURL turns the Projeto's `owner/repo` into the URL git is given. Always
// HTTPS, never SSH — the credential is a token — and never carrying the credential
// itself.
func remoteURL(repo string) (string, error) {
	r := strings.TrimSpace(repo)
	if r == "" {
		return "", errors.New("repositório não configurado")
	}
	// A full URL or an absolute path is passed through, which is what a remote that
	// is not github.com and what the fixture repository of the tests need.
	if strings.Contains(r, "://") || strings.HasPrefix(r, "/") {
		return r, nil
	}
	parts := strings.Split(strings.TrimSuffix(r, ".git"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		strings.HasPrefix(r, "-") || strings.Contains(r, "@") || strings.Contains(r, "..") {
		return "", fmt.Errorf("repositório %q não está no formato owner/repo", repo)
	}
	return "https://github.com/" + parts[0] + "/" + parts[1] + ".git", nil
}

// validBranch is narrow on purpose: the branch is interpolated into git arguments,
// and a value starting with a dash would be read as a flag instead of a ref.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.Contains(b, "..") {
		return false
	}
	for _, r := range b {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/':
		default:
			return false
		}
	}
	return true
}

// cloneDir names the directory for a repository: readable enough to recognise on the
// volume, hashed so two remotes can never collide. The branch is not part of the
// name — one repository, one clone, brought to whichever branch is configured.
func cloneDir(remote string) string {
	sum := sha256.Sum256([]byte(remote))
	return slug(remote) + "-" + hex.EncodeToString(sum[:6])
}

func slug(s string) string {
	s = strings.TrimSuffix(s, ".git")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	// The tail is the informative end: owner and repository, not the scheme.
	if len(out) > 40 {
		out = out[len(out)-40:]
	}
	// Trimmed after the cut, not before: a remote whose tail happens to read
	// ".clone-…" would otherwise produce a clone directory the sweeper mistakes for
	// an abandoned temporary one and deletes out from under a live Análise.
	out = strings.Trim(out, "-.")
	if out == "" {
		out = "repo"
	}
	return out
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrOutput {
		s = s[len(s)-maxErrOutput:]
	}
	return s
}
