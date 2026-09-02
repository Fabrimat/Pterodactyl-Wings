package borg

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A realistic passphrase from the wire contract: 64 hex characters.
const testPassphrase = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSecretIsNeverFormattedInTheClear(t *testing.T) {
	s := Secret(testPassphrase)
	cfg := struct {
		Repository string `json:"repository"`
		Passphrase Secret `json:"passphrase"`
	}{Repository: "ssh://borg@host:22/./repo", Passphrase: s}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal returned an error: %v", err)
	}

	cases := map[string]string{
		`%s`:                           fmt.Sprintf("%s", s),
		`%v`:                           fmt.Sprintf("%v", s),
		`%+v`:                          fmt.Sprintf("%+v", s),
		`%#v`:                          fmt.Sprintf("%#v", s),
		`%v of a struct`:               fmt.Sprintf("%v", cfg),
		`%+v of a struct`:              fmt.Sprintf("%+v", cfg),
		`%#v of a struct`:              fmt.Sprintf("%#v", cfg),
		`json.Marshal`:                 string(encoded),
		`error built from borg stderr`: newCommandError("create", 2, []byte("passphrase supplied in BORG_PASSPHRASE is incorrect: "+testPassphrase), s).Error(),
	}
	for name, out := range cases {
		if strings.Contains(out, testPassphrase) {
			t.Errorf("%s leaked the secret: %s", name, out)
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s did not contain the redaction placeholder: %s", name, out)
		}
	}
}

func TestSecretRevealReturnsTheRawValue(t *testing.T) {
	if got := Secret(testPassphrase).Reveal(); got != testPassphrase {
		t.Errorf("Reveal() = %q, want the raw value", got)
	}
}

func TestSecretUnmarshalsWithTheDefaultDecoder(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{name: "a string", body: `{"passphrase":"hunter2"}`, want: "hunter2"},
		{name: "null", body: `{"passphrase":null}`, want: ""},
		{name: "an absent key", body: `{}`, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var v struct {
				Passphrase Secret `json:"passphrase"`
			}
			if err := json.Unmarshal([]byte(tt.body), &v); err != nil {
				t.Fatalf("json.Unmarshal returned an error: %v", err)
			}
			if got := v.Passphrase.Reveal(); got != tt.want {
				t.Errorf("Passphrase = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScrub(t *testing.T) {
	for _, tt := range []struct {
		name    string
		in      string
		secrets []Secret
		want    string
	}{
		{
			name:    "replaces every occurrence",
			in:      "abc and abc again",
			secrets: []Secret{"abc"},
			want:    redacted + " and " + redacted + " again",
		},
		{
			name:    "replaces more than one secret",
			in:      "key=aaa pass=bbb",
			secrets: []Secret{"aaa", "bbb"},
			want:    "key=" + redacted + " pass=" + redacted,
		},
		{
			name:    "ignores empty secrets so the output is not shredded",
			in:      "nothing to hide",
			secrets: []Secret{""},
			want:    "nothing to hide",
		},
		{
			name:    "leaves text without a secret alone",
			in:      "Repository /srv/borg does not exist",
			secrets: []Secret{testPassphrase},
			want:    "Repository /srv/borg does not exist",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Scrub(tt.in, tt.secrets...); got != tt.want {
				t.Errorf("Scrub() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScrubRunsBeforeStderrIsTruncated(t *testing.T) {
	// Place the secret so that truncating the buffer first would cut it in
	// half, leaving a fragment behind that the replacement no longer matches.
	// Scrubbing has to happen against the whole buffer for that reason.
	stderr := []byte(strings.Repeat("x", 100) + testPassphrase + strings.Repeat("y", 4060))
	got := newCommandError("create", 2, stderr, Secret(testPassphrase)).Error()
	for i := 0; i+16 <= len(testPassphrase); i++ {
		if strings.Contains(got, testPassphrase[i:i+16]) {
			t.Fatalf("a fragment of the secret survived truncation: %q", testPassphrase[i:i+16])
		}
	}
}
