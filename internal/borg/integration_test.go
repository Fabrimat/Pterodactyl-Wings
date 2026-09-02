//go:build borg_integration

package borg

// This file drives a real borg binary end to end. It is excluded from the
// normal build and from a plain "go test ./..." by the build tag above, since
// every other test in this package proves the adapter's own logic against a
// fake process and never needs borg installed to run. This is the one place
// that assumption gets checked against the real thing: whether borg actually
// accepts BORG_NEW_PASSPHRASE non-interactively, what its "already exists" and
// "does not exist" messages really look like, and what the undocumented shape
// of "info --json" actually is. See .github/workflows/borg.yaml for the job
// that installs borg and runs this with go test -tags borg_integration.
//
// The whole test is hermetic: the repository and borg's own base directory
// both live under t.TempDir(), the repository is local (no ssh://), and
// nothing is read from or written to a path outside that temp dir.

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// tarEntry is one thing to put in the tar this test round-trips through borg.
type tarEntry struct {
	name string
	mode int64
	dir  bool
	body []byte
}

func TestBorgRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("borg"); err != nil {
		t.Skip("borg is not installed on this machine; skipping the real borg integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// --- version gate -------------------------------------------------
	bin, err := LookPath()
	if err != nil {
		t.Fatalf("LookPath() = %v, want the borg found by exec.LookPath above", err)
	}
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		t.Fatalf("borg --version failed: %v", err)
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		t.Fatalf("ParseVersion(%q) returned an error: %v", out, err)
	}
	if v.Major != 1 || v.Minor < 2 {
		t.Fatalf("borg version = %s, want >= 1.2", v)
	}
	if v.Major >= 2 {
		t.Fatalf("borg version = %s, want < 2.0", v)
	}
	if !v.Supported() {
		t.Fatalf("Version(%s).Supported() = false, want true", v)
	}
	if err := RequireVersion(ctx); err != nil {
		t.Fatalf("RequireVersion() = %v, want the installed borg accepted", err)
	}

	// --- setup ----------------------------------------------------------
	root := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")
	archive := "roundtrip"
	target := repoPath + "::" + archive
	common := CommonOptions{LockWait: 30}

	runner := NewRunner(root, Repository{
		Path:       repoPath,
		Passphrase: Secret("borg integration test passphrase"),
	})

	// --- init -------------------------------------------------------------
	initCmd := Cmd{
		Sub:           "init",
		Common:        common,
		Options:       []string{"--encryption", "repokey-blake2"},
		Positionals:   []string{repoPath},
		NewPassphrase: true,
	}
	if _, err := runner.Run(ctx, initCmd); err != nil {
		t.Fatalf("init: %v", err)
	}

	// --- idempotent re-init -------------------------------------------
	// Two concurrent backups of the same server both try to init; the loser
	// has to be able to treat this as success. This is the only place that
	// tolerance is checked against borg's real exit code and message rather
	// than a canned stderr string.
	if _, err := runner.Run(ctx, initCmd); err != nil && !IsRepositoryExistsError(err) {
		t.Fatalf("second init: err = %v, want IsRepositoryExistsError to recognize it", err)
	}

	// --- import-tar -----------------------------------------------------
	entries := []tarEntry{
		{name: "nested/", mode: 0o755, dir: true},
		{name: "nested/dir/", mode: 0o755, dir: true},
		{name: "file-a.txt", mode: 0o644, body: []byte("contents of the first file")},
		{name: "nested/dir/file-b.txt", mode: 0o644, body: []byte("contents of the second file, not the same as the first")},
		{name: "empty.txt", mode: 0o644, body: []byte{}},
		{name: "script.sh", mode: 0o755, body: []byte("#!/bin/sh\necho hello\n")},
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var wantSize int64
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
			wantSize += int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", e.name, err)
		}
		if !e.dir {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("writing tar body for %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar writer: %v", err)
	}

	if _, err := runner.Run(ctx, Cmd{
		Sub:         "import-tar",
		Common:      common,
		Positionals: []string{target, "-"},
		Stdin:       bytes.NewReader(buf.Bytes()),
	}); err != nil {
		t.Fatalf("import-tar: %v", err)
	}

	// --- info --json ------------------------------------------------------
	// The JSON shape here is undocumented, so this is the one place a borg
	// upgrade that renames or restructures these fields would be caught. If
	// this disagrees with ParseArchiveInfo, that is a real finding, not a
	// reason to relax the assertion below.
	res, err := runner.Run(ctx, Cmd{
		Sub:         "info",
		Common:      common,
		Options:     []string{"--json"},
		Positionals: []string{target},
	})
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	info, err := ParseArchiveInfo(res.Stdout)
	if err != nil {
		t.Fatalf("ParseArchiveInfo(%s) returned an error: %v", res.Stdout, err)
	}
	if info.ID == "" {
		t.Errorf("info.ID is empty, want the archive id")
	}
	if info.OriginalSize != wantSize {
		t.Errorf("info.OriginalSize = %d, want %d (stdout: %s)", info.OriginalSize, wantSize, res.Stdout)
	}

	// --- export-tar -------------------------------------------------------
	var exported bytes.Buffer
	if _, err := runner.Run(ctx, Cmd{
		Sub:         "export-tar",
		Common:      common,
		Positionals: []string{target, "-"},
		Stdout:      &exported,
	}); err != nil {
		t.Fatalf("export-tar: %v", err)
	}

	got := map[string][]byte{}
	tr := tar.NewReader(&exported)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading the exported tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s from the exported tar: %v", hdr.Name, err)
		}
		got[hdr.Name] = body
	}
	for _, e := range entries {
		if e.dir {
			continue
		}
		body, ok := got[e.name]
		if !ok {
			t.Errorf("export-tar: %s is missing from the exported archive", e.name)
			continue
		}
		if !bytes.Equal(body, e.body) {
			t.Errorf("export-tar: %s = %q, want %q", e.name, body, e.body)
		}
	}

	// --- export-tar consumed the way Restore consumes it -------------------
	// The buffer above proves the content is right, but Run only returns
	// once the buffer holds everything, so that read never exercises the
	// actual restore shape. Restore instead pipes export-tar's stdout to a
	// tar.Reader that stops right after the end-of-archive marker, leaving
	// whatever borg wrote past it still unread on the pipe. That gap is
	// exactly what reached production: a restore that copied every byte
	// correctly and then failed because export-tar was still writing into a
	// pipe whose read end had already closed. This drives that same shape
	// against a real borg process rather than a stream this test built by
	// hand, so a future export-tar whose trailing bytes take a different
	// shape fails CI instead of a live restore.
	pr, pw := io.Pipe()
	runErr := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, Cmd{
			Sub:         "export-tar",
			Common:      common,
			Positionals: []string{target, "-"},
			Stdout:      pw,
		})
		_ = pw.CloseWithError(err)
		runErr <- err
	}()

	pipeTr := tar.NewReader(pr)
	var readErr error
	for {
		if _, err := pipeTr.Next(); err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
		if _, err := io.Copy(io.Discard, pipeTr); err != nil {
			readErr = err
			break
		}
	}
	// The fix under test: draining the rest of the pipe before closing it is
	// what lets export-tar reach EOF on its own and exit cleanly.
	readErr = DrainOnSuccess(readErr, pr)
	_ = pr.CloseWithError(readErr)
	if readErr != nil {
		t.Fatalf("reading export-tar through a pipe: %v", readErr)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("export-tar via pipe: %v", err)
	}

	// --- delete -------------------------------------------------------
	if _, err := runner.Run(ctx, Cmd{
		Sub:         "delete",
		Common:      common,
		Positionals: []string{target},
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := runner.Run(ctx, Cmd{
		Sub:         "info",
		Common:      common,
		Options:     []string{"--json"},
		Positionals: []string{target},
	}); err == nil {
		t.Errorf("info on a deleted archive succeeded, want it to report the archive missing")
	} else if !IsNotFoundError(err) {
		t.Errorf("info on a deleted archive: err = %v, want IsNotFoundError to recognize it", err)
	}

	// Deleting an already gone archive is the outcome the caller wanted, the
	// same way removing a missing local file is.
	if _, err := runner.Run(ctx, Cmd{
		Sub:         "delete",
		Common:      common,
		Positionals: []string{target},
	}); err != nil && !IsNotFoundError(err) {
		t.Errorf("second delete: err = %v, want nil or IsNotFoundError", err)
	}

	// --- compact ------------------------------------------------------
	if _, err := runner.Run(ctx, Cmd{
		Sub:         "compact",
		Common:      common,
		Positionals: []string{repoPath},
	}); err != nil {
		t.Errorf("compact: %v", err)
	}
}

// TestBorgRejectsAWrongPassphrase pins the message IsPassphraseError matches.
// The passphrase is derived from the panel's secret and never stored, so a
// changed secret reaches the node as an intact repository that stops opening.
// The predicate exists to say that by name instead of blaming a corrupt
// repository, and it can only stay right if borg's real wording is checked:
// a reworded message in a future borg would leave the predicate compiling,
// the unit test passing against its own canned string, and the operator back
// to reading the wrong diagnosis.
func TestBorgRejectsAWrongPassphrase(t *testing.T) {
	if _, err := exec.LookPath("borg"); err != nil {
		t.Skip("borg is not installed on this machine; skipping the real borg integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")
	common := CommonOptions{LockWait: 30}

	created := NewRunner(root, Repository{
		Path:       repoPath,
		Passphrase: Secret("the passphrase the repository was created with"),
	})
	if _, err := created.Run(ctx, Cmd{
		Sub:           "init",
		Common:        common,
		Options:       []string{"--encryption", "repokey-blake2"},
		Positionals:   []string{repoPath},
		NewPassphrase: true,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// A different secret derives a different passphrase for the same server.
	rotated := NewRunner(root, Repository{
		Path:       repoPath,
		Passphrase: Secret("the passphrase a changed panel secret would derive"),
	})
	_, err := rotated.Run(ctx, Cmd{
		Sub:         "info",
		Common:      common,
		Options:     []string{"--json"},
		Positionals: []string{repoPath},
	})
	if err == nil {
		t.Fatal("info with the wrong passphrase succeeded, want it rejected")
	}
	if !IsPassphraseError(err) {
		t.Errorf("IsPassphraseError() = false for a real wrong passphrase: %v", err)
	}
	// The two are distinct diagnoses and the wrong one sends an operator
	// looking for damage that is not there.
	if IsNotFoundError(err) {
		t.Errorf("IsNotFoundError() = true for a wrong passphrase: %v", err)
	}
}
