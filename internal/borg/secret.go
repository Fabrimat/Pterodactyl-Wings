package borg

import "strings"

// redacted is what a Secret renders as no matter how it is formatted.
const redacted = "(redacted)"

// Secret is a string that never renders its own value. The repository
// passphrase and the SSH private key both reach this node on every backup
// operation and neither is ever allowed to reach a log line or an error, so the
// type refuses to print itself rather than relying on every call site to
// remember that.
//
// Decoding is deliberately left to the default decoder so that the wire object
// unmarshals without any extra handling.
type Secret string

// String implements fmt.Stringer, covering %s, %v and %+v.
func (s Secret) String() string {
	return redacted
}

// GoString implements fmt.GoStringer. Without it a %#v of any struct holding a
// Secret would print the raw value.
func (s Secret) GoString() string {
	return redacted
}

// MarshalJSON keeps the value out of anything that serialises the struct it
// belongs to.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// Reveal returns the underlying value. It is the only way to read a Secret and
// exists so that grepping for it turns up every place a secret is used.
func (s Secret) Reveal() string {
	return string(s)
}

// Scrub replaces every occurrence of the given secrets in s with the redaction
// placeholder. Output captured from borg is put through this before it is
// wrapped in an error or logged, since borg is happy to echo a passphrase back
// in a complaint about it.
func Scrub(s string, secrets ...Secret) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret.Reveal(), redacted)
	}
	return s
}
