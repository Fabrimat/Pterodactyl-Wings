package borg

import (
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		want   string
		err    bool
	}{
		{name: "the usual output", output: "borg 1.2.8\n", want: "1.2.8"},
		{name: "no trailing newline", output: "borg 1.4.0", want: "1.4.0"},
		{name: "no patch component", output: "borg 1.2", want: "1.2.0"},
		{name: "a prerelease suffix", output: "borg 2.0.0b8\n", want: "2.0.0"},
		{name: "an empty output", output: "", err: true},
		{name: "no version at all", output: "borg\n", err: true},
		{name: "garbage", output: "command not found\n", err: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := ParseVersion(tt.output)
			if tt.err {
				if err == nil {
					t.Fatalf("ParseVersion(%q) = %v, want an error", tt.output, v)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVersion(%q) returned an error: %v", tt.output, err)
			}
			if v.String() != tt.want {
				t.Errorf("ParseVersion(%q) = %s, want %s", tt.output, v, tt.want)
			}
		})
	}
}

func TestVersionSupported(t *testing.T) {
	for _, tt := range []struct {
		output string
		want   bool
	}{
		{output: "borg 1.2.0", want: true},
		{output: "borg 1.2.8", want: true},
		{output: "borg 1.4.1", want: true},
		{output: "borg 1.1.18", want: false},
		{output: "borg 1.0.9", want: false},
		// Borg 2.0 renamed init to repo-create and changed the encryption mode
		// names, so its dialect is a different adapter.
		{output: "borg 2.0.0", want: false},
		{output: "borg 2.1.0", want: false},
		{output: "borg 0.29.0", want: false},
	} {
		t.Run(tt.output, func(t *testing.T) {
			v, err := ParseVersion(tt.output)
			if err != nil {
				t.Fatalf("ParseVersion(%q) returned an error: %v", tt.output, err)
			}
			if got := v.Supported(); got != tt.want {
				t.Errorf("Version(%s).Supported() = %v, want %v", v, got, tt.want)
			}
		})
	}
}

func TestUnsupportedVersionErrorIsActionable(t *testing.T) {
	err := unsupportedVersionError(Version{Major: 1, Minor: 1, Patch: 18})
	for _, want := range []string{"1.1.18", "1.2", "2.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to mention %q", err.Error(), want)
		}
	}
}
