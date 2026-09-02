package borg

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"emperror.dev/errors"
)

// Version is a borg release, parsed out of "borg --version".
type Version struct {
	Major int
	Minor int
	Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Supported reports whether this adapter speaks the version's dialect. The
// floor is 1.2, which is where "import-tar" arrived and where
// "--remote-ratelimit" became "--upload-ratelimit". The ceiling is 2.0, which
// renamed "init" to "repo-create" and changed the encryption mode names.
func (v Version) Supported() bool {
	return v.Major == 1 && v.Minor >= 2
}

// ParseVersion pulls the version out of borg's own version output, which looks
// like "borg 1.2.8".
func ParseVersion(output string) (Version, error) {
	for _, field := range strings.Fields(output) {
		if v, ok := parseVersionField(field); ok {
			return v, nil
		}
	}
	return Version{}, errors.Errorf("borg: could not determine the borg version from %q", tail(output))
}

func parseVersionField(field string) (Version, bool) {
	parts := strings.SplitN(field, ".", 3)
	if len(parts) < 2 {
		return Version{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Version{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Version{}, false
	}
	var patch int
	if len(parts) == 3 {
		// A prerelease looks like "2.0.0b8", so read the leading digits and
		// drop whatever follows them.
		digits := parts[2]
		for i, r := range digits {
			if r < '0' || r > '9' {
				digits = digits[:i]
				break
			}
		}
		patch, _ = strconv.Atoi(digits)
	}
	return Version{Major: major, Minor: minor, Patch: patch}, true
}

func unsupportedVersionError(v Version) error {
	return errors.Errorf("borg: found borg %s on this node, but the backup adapter requires borg >= 1.2 and < 2.0", v)
}

var (
	versionMu sync.Mutex
	version   Version
)

// RequireVersion checks that a usable borg binary is installed. The probe runs
// once per process and only a success is cached, so a version check that
// failed because its context was cancelled does not poison every later backup.
func RequireVersion(ctx context.Context) error {
	versionMu.Lock()
	defer versionMu.Unlock()
	if version.Supported() {
		return nil
	}
	bin, err := LookPath()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return errors.Wrap(err, "borg: could not run borg --version")
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		return err
	}
	if !v.Supported() {
		return unsupportedVersionError(v)
	}
	version = v
	return nil
}

// LookPath resolves the borg binary so that a node without borg installed
// fails with something an administrator can act on rather than with whatever
// exec reports at the point of use.
func LookPath() (string, error) {
	bin, err := exec.LookPath("borg")
	if err != nil {
		return "", errors.New("borg: the borg binary was not found in PATH; install borg 1.2 or newer on this node to use the borg backup adapter")
	}
	return bin, nil
}
