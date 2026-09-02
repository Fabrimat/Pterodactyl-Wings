package borg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root", "BORG_PASSPHRASE=stale", "BORG_REPO=/somewhere/else"}
	env := buildEnv(base, Secret("hunter2"), "/var/lib/pterodactyl/.borg/cache", "ssh -i /key", false)

	for _, want := range []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"BORG_PASSPHRASE=hunter2",
		"BORG_BASE_DIR=/var/lib/pterodactyl/.borg/cache",
		"BORG_RSH=ssh -i /key",
		// Without these borg can stop and ask a question. During import-tar its
		// stdin is the tar stream, so a prompt would eat archive data.
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
	} {
		if indexOf(env, want) < 0 {
			t.Errorf("buildEnv() = %q, want it to contain %q", env, want)
		}
	}
	if indexOf(env, "BORG_PASSPHRASE=stale") >= 0 || indexOf(env, "BORG_REPO=/somewhere/else") >= 0 {
		t.Errorf("buildEnv() = %q, want the node's own BORG_ variables dropped", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "BORG_NEW_PASSPHRASE=") {
			t.Errorf("buildEnv() = %q, BORG_NEW_PASSPHRASE belongs to init only", env)
		}
	}
}

func TestBuildEnvAnswersTheInitPassphrasePrompt(t *testing.T) {
	// borg init asks for a new passphrase rather than an existing one and will
	// block on the prompt if only BORG_PASSPHRASE is set.
	env := buildEnv(nil, Secret("hunter2"), "/cache", "", true)
	if indexOf(env, "BORG_NEW_PASSPHRASE=hunter2") < 0 {
		t.Errorf("buildEnv() = %q, want BORG_NEW_PASSPHRASE to be set", env)
	}
}

func TestBuildEnvOmitsRshWhenThereIsNoSshCommand(t *testing.T) {
	for _, e := range buildEnv(nil, "", "/cache", "", false) {
		if strings.HasPrefix(e, "BORG_RSH=") {
			t.Errorf("buildEnv() set %q with no ssh command", e)
		}
	}
}

func TestSSHCommand(t *testing.T) {
	got := sshCommand("/var/lib/pterodactyl/.borg/ssh/1/id", "/var/lib/pterodactyl/.borg/ssh/1/known_hosts")
	for _, want := range []string{
		"-i '/var/lib/pterodactyl/.borg/ssh/1/id'",
		"-o BatchMode=yes",
		"-o IdentitiesOnly=yes",
		"-o StrictHostKeyChecking=yes",
		"-o UserKnownHostsFile='/var/lib/pterodactyl/.borg/ssh/1/known_hosts'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sshCommand() = %q, want it to contain %q", got, want)
		}
	}
	// This connection carries the repository passphrase and every byte of a
	// server's data, so a host key may never be trusted on first sight.
	if strings.Contains(got, "accept-new") {
		t.Errorf("sshCommand() = %q, host key checking must stay strict", got)
	}
}

func TestSSHCommandFallsBackToTheSystemKnownHosts(t *testing.T) {
	got := sshCommand("/key", "")
	if strings.Contains(got, "UserKnownHostsFile") {
		t.Errorf("sshCommand() = %q, want no known hosts override", got)
	}
	if !strings.Contains(got, "-o StrictHostKeyChecking=yes") {
		t.Errorf("sshCommand() = %q, want strict host key checking", got)
	}
}

func TestSSHCommandWithoutAKey(t *testing.T) {
	got := sshCommand("", "/known_hosts")
	if strings.Contains(got, "-i") {
		t.Errorf("sshCommand() = %q, want no identity file", got)
	}
	if !strings.Contains(got, "UserKnownHostsFile='/known_hosts'") {
		t.Errorf("sshCommand() = %q, want the known hosts override", got)
	}
}

func TestShellQuote(t *testing.T) {
	// borg splits BORG_RSH with shell lexing, so a path with a space in it has
	// to survive the round trip.
	for _, tt := range []struct {
		in   string
		want string
	}{
		{in: "/var/lib/pterodactyl/id", want: `'/var/lib/pterodactyl/id'`},
		{in: "/opt/my node/id", want: `'/opt/my node/id'`},
		{in: `/o'dd/id`, want: `'/o'\''dd/id'`},
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := shellQuote(tt.in); got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestKeyMaterialGetsATrailingNewline(t *testing.T) {
	// ssh rejects a key file whose last line is not terminated.
	if got := withNewline("-----BEGIN-----\nkey\n-----END-----"); !strings.HasSuffix(got, "\n") {
		t.Errorf("withNewline() = %q, want a trailing newline", got)
	}
	if got := withNewline("already\n"); got != "already\n" {
		t.Errorf("withNewline() = %q, want the input unchanged", got)
	}
}

// posixTempRoot returns a fresh temporary directory in the POSIX-absolute
// form cleanRoot requires, which a real path on this OS never is. The \\?\
// prefix is Windows' own literal-path escape, so the result still resolves to
// the real temporary directory - it just also satisfies the "wings node is
// always Linux" shape cleanRoot checks for.
func posixTempRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		return dir
	}
	// A wings node is always Linux, so cleanRoot requires a POSIX absolute
	// path. Windows' extended-length prefix keeps that shape while still
	// resolving to the real temporary directory, which lets the test write
	// through it on a native checkout.
	return "//?/" + filepath.ToSlash(dir)
}

func TestSSHEnvironmentSkipsLocalRepositories(t *testing.T) {
	for _, tt := range []struct {
		name      string
		path      string
		wantRSH   bool
		wantFiles bool
	}{
		{name: "a local path", path: "/srv/backups/repo", wantRSH: false, wantFiles: false},
		{name: "a file url", path: "file:///srv/backups/repo", wantRSH: false, wantFiles: false},
		{name: "an ssh repository", path: "ssh://borg@example.com/./repo", wantRSH: true, wantFiles: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := posixTempRoot(t)
			r := NewRunner(root, Repository{
				Path:       tt.path,
				SSHKey:     Secret("-----BEGIN-----\nkey\n-----END-----"),
				KnownHosts: "example.com ssh-ed25519 AAAA",
			})

			rsh, cleanup, err := r.sshEnvironment()
			defer cleanup()
			if err != nil {
				t.Fatalf("sshEnvironment() returned an error: %v", err)
			}

			if hasRSH := rsh != ""; hasRSH != tt.wantRSH {
				t.Errorf("sshEnvironment() BORG_RSH = %q, want non-empty: %v", rsh, tt.wantRSH)
			}

			// The point of the guard is that a local repository never gets an
			// ssh directory at all, so this checks the filesystem rather than
			// just the returned values: a regression that still wrote the key
			// but happened to return an empty BORG_RSH would slip past that
			// alone.
			sshParent := filepath.Join(root, ".borg", "ssh")
			wroteFiles := false
			if entries, err := os.ReadDir(sshParent); err == nil {
				for _, e := range entries {
					sub, err := os.ReadDir(filepath.Join(sshParent, e.Name()))
					if err == nil && len(sub) > 0 {
						wroteFiles = true
					}
				}
			}
			if wroteFiles != tt.wantFiles {
				t.Errorf("sshEnvironment() wrote files under %s = %v, want %v", sshParent, wroteFiles, tt.wantFiles)
			}
		})
	}
}

func TestCleanRoot(t *testing.T) {
	for _, tt := range []struct {
		name string
		root string
		want string
	}{
		{name: "an absolute path", root: "/var/lib/pterodactyl", want: "/var/lib/pterodactyl"},
		// A trailing separator is a plausible thing to write in the config
		// file and means exactly the same directory, so it is normalised
		// rather than rejected.
		{name: "a trailing separator", root: "/var/lib/pterodactyl/", want: "/var/lib/pterodactyl"},
		{name: "a doubled separator", root: "/var/lib//pterodactyl", want: "/var/lib/pterodactyl"},
		{name: "a single dot element", root: "/var/lib/./pterodactyl", want: "/var/lib/pterodactyl"},
		{name: "the filesystem root", root: "/", want: "/"},

		// An SSH private key is written under this directory, so anything that
		// would move it somewhere unintended fails instead.
		{name: "empty", root: "", want: ""},
		{name: "relative", root: "pterodactyl", want: ""},
		{name: "explicitly relative", root: "./pterodactyl", want: ""},
		{name: "whitespace", root: " ", want: ""},
		{name: "traversal in the middle", root: "/var/lib/../etc", want: ""},
		{name: "traversal at the end", root: "/var/lib/pterodactyl/..", want: ""},
		{name: "only a traversal", root: "..", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanRoot(tt.root)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("cleanRoot(%q) = %q, want an error", tt.root, got)
				}
				// The operator has to be told which setting to go and fix.
				if !strings.Contains(err.Error(), "root_directory") {
					t.Errorf("cleanRoot(%q) error = %q, want it to name root_directory", tt.root, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("cleanRoot(%q) returned an error: %v", tt.root, err)
			}
			// Compared as slashes so the expectations hold on any platform the
			// package's tests run on.
			if slashed := filepath.ToSlash(got); slashed != tt.want {
				t.Errorf("cleanRoot(%q) = %q, want %q", tt.root, slashed, tt.want)
			}
		})
	}
}
