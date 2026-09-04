package borg

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"emperror.dev/errors"
)

const (
	// defaultLockWait is used when the configuration carries no usable value.
	// Borg's own default is one second, which is nowhere near enough for a
	// repository two backups of the same server may be contending for.
	defaultLockWait = 600

	// stderrLimit caps how much of borg's stderr is kept for diagnostics. A
	// long backup produces a lot of progress output and only the tail of it
	// says anything about the failure.
	stderrLimit = 4 * 1024
)

// CommonOptions are the borg options that apply to every subcommand rather
// than to a specific one.
type CommonOptions struct {
	// LockWait is the number of seconds to wait for the repository lock before
	// giving up. A timeout is a failed backup, never a retry.
	LockWait int

	// UploadRatelimit caps data sent to a remote repository in KiB/s. Zero
	// disables the cap and the option is left off entirely.
	UploadRatelimit int
}

func (o CommonOptions) args() []string {
	wait := o.LockWait
	if wait <= 0 {
		wait = defaultLockWait
	}
	args := []string{"--lock-wait", strconv.Itoa(wait)}
	if o.UploadRatelimit > 0 {
		args = append(args, "--upload-ratelimit", strconv.Itoa(o.UploadRatelimit))
	}
	return args
}

// Cmd describes a single borg invocation.
type Cmd struct {
	// Sub is the borg subcommand, for example "import-tar" or "info".
	Sub string

	// Common holds the options that are passed no matter the subcommand.
	Common CommonOptions

	// Options are the subcommand's own options.
	Options []string

	// Positionals are the repository, archive and file arguments.
	Positionals []string

	// Stdin is fed to borg. Leaving it nil connects the null device, so a
	// prompt borg was not expected to need fails instead of hanging.
	Stdin io.Reader

	// Stdout receives borg's output. When nil it is captured into the Result.
	Stdout io.Writer

	// NewPassphrase also exports the passphrase as the answer to a prompt for a
	// new one. Only "init" asks for that.
	NewPassphrase bool
}

// Args returns the argument vector for this command. Borg accepts options to
// the left or the right of the positional arguments but never between them, so
// every option is emitted ahead of the first positional and the positionals
// stay a contiguous run at the end.
func (c Cmd) Args() []string {
	common := c.Common.args()
	args := make([]string, 0, 1+len(common)+len(c.Options)+len(c.Positionals))
	args = append(args, c.Sub)
	args = append(args, common...)
	args = append(args, c.Options...)
	return append(args, c.Positionals...)
}

// ExitStatus is a classification of how a borg process ended.
type ExitStatus int

const (
	// ExitSuccess is borg's exit code 0.
	ExitSuccess ExitStatus = iota

	// ExitWarning is borg's exit code 1. It is routine against a running game
	// server, most often "file changed while we backed it up", and the archive
	// is still committed, so it counts as a success.
	ExitWarning

	// ExitError is any exit code of 2 or above, which borg uses for real
	// failures.
	ExitError

	// ExitSignal is a process that was killed rather than one that chose its
	// own exit code.
	ExitSignal
)

// Classify maps a process exit code onto an ExitStatus.
func Classify(code int) ExitStatus {
	switch {
	case code == 0:
		return ExitSuccess
	case code == 1:
		return ExitWarning
	// A process killed by a signal has no exit code of its own: os/exec
	// reports -1 for it, while borg reports 128+N when it handles the signal
	// and exits on it itself.
	case code < 0 || code >= 128:
		return ExitSignal
	default:
		return ExitError
	}
}

// OK reports whether the archive borg was asked to produce can be trusted.
func (s ExitStatus) OK() bool {
	return s == ExitSuccess || s == ExitWarning
}

// CommandError is returned when a borg invocation fails. The captured stderr is
// scrubbed and truncated as the error is built, so nothing downstream can leak
// a secret by wrapping or logging it.
type CommandError struct {
	// Sub is the borg subcommand that failed.
	Sub string

	// Code is the raw process exit code.
	Code int

	// Status is the classification of Code.
	Status ExitStatus

	stderr string
}

// newCommandError builds the error for a failed invocation. The scrub runs
// against the whole buffer before it is truncated: cutting first could leave
// half of a secret behind where the replacement no longer matches it.
func newCommandError(sub string, code int, stderr []byte, secrets ...Secret) *CommandError {
	return &CommandError{
		Sub:    sub,
		Code:   code,
		Status: Classify(code),
		stderr: tail(Scrub(string(stderr), secrets...)),
	}
}

// Stderr returns the scrubbed tail of borg's standard error.
func (e *CommandError) Stderr() string {
	return e.stderr
}

func (e *CommandError) Error() string {
	if e.Status == ExitSignal {
		return fmt.Sprintf("borg: %s was killed by a signal (exit code %d): %s", e.Sub, e.Code, e.stderr)
	}
	return fmt.Sprintf("borg: %s failed with exit code %d: %s", e.Sub, e.Code, e.stderr)
}

// IsRepositoryExistsError reports whether err is borg declining to initialise a
// repository because there is already one at that location. Two concurrent
// backups of the same server both try to init, so the loser has to treat this
// as a success.
func IsRepositoryExistsError(err error) bool {
	return stderrContains(err, "already exists")
}

// IsPassphraseError reports whether err is borg refusing to open a repository
// because the passphrase does not match the one the repository was created
// with. The passphrase is derived from the panel's secret rather than stored,
// so this is what a changed secret looks like from the node: an intact
// repository that no longer opens.
func IsPassphraseError(err error) bool {
	return stderrContains(err, "passphrase supplied in borg_passphrase")
}

// IsNotFoundError reports whether err is borg failing because the repository or
// the archive it was pointed at is not there. Deleting an archive that is
// already gone is a success, the same way os.ErrNotExist is tolerated when
// removing a local backup.
func IsNotFoundError(err error) bool {
	return stderrContains(err, "does not exist", "is not a valid repository")
}

// stderrContains matches against the lower cased stderr, so every needle has
// to be lower case itself or it silently never matches.
func stderrContains(err error, needles ...string) bool {
	var cerr *CommandError
	if err == nil || !errors.As(err, &cerr) {
		return false
	}
	s := strings.ToLower(cerr.Stderr())
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// IsLocalRepository reports whether the repository is a path on this node.
// Borg reads a bare path and a file:// URL from the local filesystem, and
// reaches an ssh:// URL or an scp style host prefix over the network.
func IsLocalRepository(repository string) bool {
	if strings.HasPrefix(repository, "file://") {
		return true
	}
	if strings.Contains(repository, "://") {
		return false
	}
	if i := strings.Index(repository, ":"); i > 0 && !strings.Contains(repository[:i], "/") {
		return false
	}
	return true
}

func tail(s string) string {
	if len(s) > stderrLimit {
		s = s[len(s)-stderrLimit:]
	}
	return strings.TrimSpace(s)
}
