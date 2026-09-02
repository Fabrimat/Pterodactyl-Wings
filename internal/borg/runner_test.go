package borg

import (
	"strings"
	"testing"
)

func TestBuildEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root", "BORG_PASSPHRASE=stale", "BORG_REPO=/somewhere/else"}
	env := buildEnv(base, Secret("hunter2"), "/var/lib/pterodactyl/borg/cache", "ssh -i /key", false)

	for _, want := range []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"BORG_PASSPHRASE=hunter2",
		"BORG_BASE_DIR=/var/lib/pterodactyl/borg/cache",
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
	got := sshCommand("/var/lib/pterodactyl/borg/ssh/1/id", "/var/lib/pterodactyl/borg/ssh/1/known_hosts")
	for _, want := range []string{
		"-i '/var/lib/pterodactyl/borg/ssh/1/id'",
		"-o BatchMode=yes",
		"-o IdentitiesOnly=yes",
		"-o StrictHostKeyChecking=yes",
		"-o UserKnownHostsFile='/var/lib/pterodactyl/borg/ssh/1/known_hosts'",
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
