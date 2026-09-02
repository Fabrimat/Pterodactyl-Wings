package borg

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCmdArgsKeepsThePositionalsContiguousAtTheEnd(t *testing.T) {
	// Borg accepts options on either side of the positional arguments but never
	// between them, so a trailing "-" must stay glued to the archive.
	c := Cmd{
		Sub:         "import-tar",
		Common:      CommonOptions{LockWait: 600, UploadRatelimit: 512},
		Options:     []string{"--compression", "zstd,3", "--checkpoint-interval", "1800"},
		Positionals: []string{"/srv/borg/repo::backup", "-"},
	}
	args := c.Args()
	want := []string{
		"import-tar",
		"--lock-wait", "600",
		"--upload-ratelimit", "512",
		"--compression", "zstd,3",
		"--checkpoint-interval", "1800",
		"/srv/borg/repo::backup", "-",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("Args() = %q, want %q", args, want)
	}
	tail := args[len(args)-len(c.Positionals):]
	if !reflect.DeepEqual(tail, c.Positionals) {
		t.Errorf("positionals are not the final arguments: got tail %q, want %q", tail, c.Positionals)
	}
}

func TestCmdArgsAlwaysPassesLockWait(t *testing.T) {
	// Borg's own default is a single second, so leaving the flag off silently
	// discards the value the panel sent.
	for _, tt := range []struct {
		name string
		cmd  Cmd
		want string
	}{
		{name: "a value from the panel", cmd: Cmd{Sub: "info", Common: CommonOptions{LockWait: 900}}, want: "900"},
		{name: "a zero value", cmd: Cmd{Sub: "info"}, want: "600"},
		{name: "a negative value", cmd: Cmd{Sub: "compact", Common: CommonOptions{LockWait: -5}}, want: "600"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.cmd.Args()
			i := indexOf(args, "--lock-wait")
			if i < 0 {
				t.Fatalf("Args() = %q, --lock-wait is missing", args)
			}
			if args[i+1] != tt.want {
				t.Errorf("--lock-wait = %q, want %q", args[i+1], tt.want)
			}
		})
	}
}

func TestCmdArgsOnlyPassesUploadRatelimitWhenItIsSet(t *testing.T) {
	for _, tt := range []struct {
		name  string
		limit int
		want  bool
	}{
		{name: "disabled", limit: 0, want: false},
		{name: "negative", limit: -1, want: false},
		{name: "enabled", limit: 1, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := Cmd{Sub: "create", Common: CommonOptions{UploadRatelimit: tt.limit}}.Args()
			if got := indexOf(args, "--upload-ratelimit") >= 0; got != tt.want {
				t.Errorf("--upload-ratelimit present = %v, want %v (args %q)", got, tt.want, args)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	for _, tt := range []struct {
		name string
		code int
		want ExitStatus
		ok   bool
	}{
		{name: "success", code: 0, want: ExitSuccess, ok: true},
		// Exit 1 is routine on a live game server: "file changed while we
		// backed it up". Failing on it would fail almost every backup.
		{name: "warning", code: 1, want: ExitWarning, ok: true},
		{name: "generic error", code: 2, want: ExitError, ok: false},
		{name: "specific error", code: 73, want: ExitError, ok: false},
		{name: "borg reporting a signal", code: 137, want: ExitSignal, ok: false},
		{name: "os/exec reporting a signal", code: -1, want: ExitSignal, ok: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.code)
			if got != tt.want {
				t.Errorf("Classify(%d) = %v, want %v", tt.code, got, tt.want)
			}
			if got.OK() != tt.ok {
				t.Errorf("Classify(%d).OK() = %v, want %v", tt.code, got.OK(), tt.ok)
			}
		})
	}
}

func TestCommandErrorNamesASignalDeathDistinctly(t *testing.T) {
	killed := newCommandError("import-tar", 137, []byte("terminated")).Error()
	if !strings.Contains(killed, "signal") {
		t.Errorf("Error() = %q, want it to mention a signal", killed)
	}
	failed := newCommandError("import-tar", 2, []byte("terminated")).Error()
	if strings.Contains(failed, "signal") {
		t.Errorf("Error() = %q, a plain failure should not mention a signal", failed)
	}
}

func TestIsRepositoryExistsError(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "an unrelated error", err: errors.New("boom"), want: false},
		{
			name: "borg refusing to reinitialise",
			err:  newCommandError("init", 2, []byte("Repository /srv/borg/repo already exists.")),
			want: true,
		},
		{
			name: "some other borg failure",
			err:  newCommandError("init", 2, []byte("Connection closed by remote host")),
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRepositoryExistsError(tt.err); got != tt.want {
				t.Errorf("IsRepositoryExistsError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsNotFoundError(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "a missing archive",
			err:  newCommandError("delete", 2, []byte("Archive 5f1e.. does not exist")),
			want: true,
		},
		{
			name: "a missing repository",
			err:  newCommandError("info", 2, []byte("Repository /srv/borg/repo does not exist.")),
			want: true,
		},
		{
			name: "a repository that is not a repository",
			err:  newCommandError("info", 2, []byte("/srv/borg/repo is not a valid repository. Check repo config.")),
			want: true,
		},
		{
			name: "an unrelated failure",
			err:  newCommandError("delete", 2, []byte("Failed to create/acquire the lock")),
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFoundError(tt.err); got != tt.want {
				t.Errorf("IsNotFoundError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsLocalRepository(t *testing.T) {
	for _, tt := range []struct {
		repository string
		want       bool
	}{
		{repository: "/srv/borg/6f2c", want: true},
		{repository: "./relative/repo", want: true},
		{repository: "file:///srv/borg/6f2c", want: true},
		{repository: "ssh://borg@backup.example.com:22/./pterodactyl/6f2c", want: false},
		{repository: "borg@backup.example.com:pterodactyl/6f2c", want: false},
		{repository: "", want: true},
	} {
		t.Run(tt.repository, func(t *testing.T) {
			if got := IsLocalRepository(tt.repository); got != tt.want {
				t.Errorf("IsLocalRepository(%q) = %v, want %v", tt.repository, got, tt.want)
			}
		})
	}
}

func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}
