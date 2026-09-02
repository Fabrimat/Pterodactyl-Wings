package borg

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
)

// Repository is the location and the credentials for one borg repository. The
// passphrase and the SSH key arrive with every operation and are never
// persisted on the node, so this only ever lives for as long as the operation
// that carries it.
type Repository struct {
	// Path is the repository location, either a path on this node or an
	// ssh:// URL.
	Path string

	// Passphrase unlocks the repository.
	Passphrase Secret

	// SSHKey is the private key for a remote repository, in PEM form. Empty
	// leaves the connection to the node's own ssh configuration.
	SSHKey Secret

	// KnownHosts is the content of a known_hosts file used to verify the
	// repository host. Empty falls back to the system known_hosts.
	KnownHosts string
}

// Runner executes borg against a single repository.
type Runner struct {
	repo Repository

	// root is wings' own root directory. Borg's chunk cache and any temporary
	// key material live underneath it: the cache would otherwise land in
	// $HOME/.cache/borg, which depends on how the service was started and grows
	// without bound across every server on the node, and /tmp is not an option
	// for the key because a tmp reaper deleting it halfway through a long
	// backup would fail that backup.
	root string
}

// NewRunner returns a runner for the given repository. The root is wings' own
// root directory.
func NewRunner(root string, repo Repository) *Runner {
	return &Runner{repo: repo, root: root}
}

// Result is the outcome of a borg invocation.
type Result struct {
	// Stdout is borg's standard output, captured only when Cmd.Stdout was nil.
	Stdout []byte

	// Stderr is borg's standard error, scrubbed of secrets and truncated to
	// its tail.
	Stderr string

	// Status classifies how the process ended.
	Status ExitStatus

	// Code is the raw process exit code.
	Code int
}

// Run executes a borg command. An exit code of 1 is a warning rather than a
// failure - it is what a running game server produces when a file changes
// underneath the backup - so it comes back with a nil error and the warning in
// Result.Stderr for the caller to log.
func (r *Runner) Run(ctx context.Context, c Cmd) (*Result, error) {
	bin, err := LookPath()
	if err != nil {
		return nil, err
	}

	baseDir, err := r.dir("cache")
	if err != nil {
		return nil, err
	}

	rsh, cleanup, err := r.sshEnvironment()
	defer cleanup()
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, bin, c.Args()...)
	cmd.Env = buildEnv(os.Environ(), r.repo.Passphrase, baseDir, rsh, c.NewPassphrase)
	// A nil Stdin leaves os/exec to connect the null device, which is what we
	// want here: a prompt borg was not expected to need then fails immediately
	// instead of hanging forever on an answer nobody will give.
	cmd.Stdin = c.Stdin

	var stdout, stderr bytes.Buffer
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := &Result{Stdout: stdout.Bytes(), Stderr: r.clean(stderr.Bytes())}

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.Status = ExitSuccess
	case errors.As(runErr, &exitErr):
		res.Code = exitErr.ExitCode()
		res.Status = Classify(res.Code)
	default:
		// The process never started, or the copy feeding its stdin failed.
		// Neither of those reaches borg's own exit codes.
		return res, errors.Errorf("borg: %s could not be executed: %s", c.Sub, Scrub(runErr.Error(), r.secrets()...))
	}
	if !res.Status.OK() {
		return res, newCommandError(c.Sub, res.Code, stderr.Bytes(), r.secrets()...)
	}
	return res, nil
}

// clean makes captured output safe to put into an error or a log line.
func (r *Runner) clean(b []byte) string {
	return tail(Scrub(string(b), r.secrets()...))
}

func (r *Runner) secrets() []Secret {
	return []Secret{r.repo.Passphrase, r.repo.SSHKey}
}

// dir returns the path of one of borg's own directories underneath wings' root
// and creates it if it is not there yet. Every caller goes through here so that
// the root is checked before anything is written below it.
//
// Everything lives under a single hidden ".borg" parent rather than directly
// under root. A repository base is commonly configured as a path under root
// too, and a future reconciliation pass over that base would otherwise find a
// "cache" and an "ssh" entry sitting alongside the server UUIDs it expects.
// The leading dot keeps this out of that enumeration as a second layer, not
// the only one: cleanRoot below is what actually keeps the key material this
// writes somewhere the operator meant.
func (r *Runner) dir(name string) (string, error) {
	root, err := cleanRoot(r.root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, ".borg", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.Wrapf(err, "borg: could not create the %s directory", name)
	}
	return dir, nil
}

// cleanRoot normalises and checks wings' root directory. Every other component
// joined onto a path in this package is a constant, so the root is the only
// part worth checking, and it is worth checking: an SSH private key is written
// underneath it, and a root that is empty, relative or carrying traversal
// elements would put key material somewhere other than where the operator
// meant with nothing saying so.
//
// Absoluteness is checked against the slash form rather than with
// filepath.IsAbs so that it means the same thing wherever this package's tests
// run. A wings node is always Linux and root_directory is always a POSIX path.
func cleanRoot(root string) (string, error) {
	const fix = "set root_directory in wings' configuration to an absolute path"
	if root == "" {
		return "", errors.New("borg: the configured root_directory is empty; " + fix)
	}
	// Clean resolves a traversal away, so a ".." has to be caught in what was
	// configured rather than in the result.
	for _, part := range strings.Split(filepath.ToSlash(root), "/") {
		if part == ".." {
			return "", errors.Errorf("borg: the configured root_directory %q contains a %q element; %s", root, "..", fix)
		}
	}
	cleaned := filepath.Clean(root)
	if !strings.HasPrefix(filepath.ToSlash(cleaned), "/") {
		return "", errors.Errorf("borg: the configured root_directory %q is not absolute; %s", root, fix)
	}
	return cleaned, nil
}

// sshEnvironment writes out the key material for a remote repository and
// returns the BORG_RSH command that points at it, along with a cleanup
// function that removes it again. The cleanup function is always safe to call.
func (r *Runner) sshEnvironment() (string, func(), error) {
	noop := func() {}
	// A local repository has no remote end, so writing key material for it
	// would put a private key on disk where it cannot be used. The panel
	// already leaves the key fields empty for a local repository, which makes
	// this look like a duplicate of the check below it - it is not. This is
	// the only check that still holds if the panel ever sends a key it should
	// not, and because the panel filters the case first, nothing but
	// TestSSHEnvironmentSkipsLocalRepositories ever exercises it.
	if IsLocalRepository(r.repo.Path) {
		return "", noop, nil
	}
	if r.repo.SSHKey == "" && r.repo.KnownHosts == "" {
		return "", noop, nil
	}

	parent, err := r.dir("ssh")
	if err != nil {
		return "", noop, err
	}
	dir, err := os.MkdirTemp(parent, "")
	if err != nil {
		return "", noop, errors.Wrap(err, "borg: could not create the ssh key directory")
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	var keyPath, knownHostsPath string
	if r.repo.SSHKey != "" {
		keyPath = filepath.Join(dir, "id")
		if err := os.WriteFile(keyPath, []byte(withNewline(r.repo.SSHKey.Reveal())), 0o600); err != nil {
			cleanup()
			return "", noop, errors.Wrap(err, "borg: could not write the ssh private key")
		}
	}
	if r.repo.KnownHosts != "" {
		knownHostsPath = filepath.Join(dir, "known_hosts")
		if err := os.WriteFile(knownHostsPath, []byte(withNewline(r.repo.KnownHosts)), 0o600); err != nil {
			cleanup()
			return "", noop, errors.Wrap(err, "borg: could not write the ssh known hosts")
		}
	}
	return sshCommand(keyPath, knownHostsPath), cleanup, nil
}

// buildEnv assembles the environment for a borg child process. The secrets
// travel in the environment and never in the argument vector, where any
// unprivileged process on the node could read them out of /proc. Every BORG_
// variable the node's own environment happens to carry is dropped so that only
// the values assembled here reach borg.
func buildEnv(base []string, passphrase Secret, baseDir, rsh string, newPassphrase bool) []string {
	env := make([]string, 0, len(base)+6)
	for _, e := range base {
		if strings.HasPrefix(e, "BORG_") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"BORG_PASSPHRASE="+passphrase.Reveal(),
		"BORG_BASE_DIR="+baseDir,
		// Answer the questions borg would otherwise stop and ask. This is not
		// hygiene: during import-tar borg's stdin is the tar stream, so a
		// prompt would either consume archive bytes or deadlock.
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
	)
	if newPassphrase {
		// borg init asks for a new passphrase, not an existing one.
		env = append(env, "BORG_NEW_PASSPHRASE="+passphrase.Reveal())
	}
	if rsh != "" {
		env = append(env, "BORG_RSH="+rsh)
	}
	return env
}

// sshCommand builds the value for BORG_RSH. Host key checking stays strict:
// this connection carries the repository passphrase and every byte of a
// server's data, so accept-new is not an option on it. With no known hosts
// file of its own it falls back to the node's system known_hosts.
func sshCommand(keyPath, knownHostsPath string) string {
	args := []string{"ssh"}
	if keyPath != "" {
		args = append(args, "-i", shellQuote(keyPath))
	}
	args = append(args,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes",
	)
	if knownHostsPath != "" {
		args = append(args, "-o", "UserKnownHostsFile="+shellQuote(knownHostsPath))
	}
	return strings.Join(args, " ")
}

// shellQuote quotes a path for BORG_RSH, which borg splits with shell lexing.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// withNewline terminates key material, which ssh rejects without a final
// newline.
func withNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
