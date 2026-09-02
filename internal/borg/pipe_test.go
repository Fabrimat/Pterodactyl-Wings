package borg

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"
)

// tarWithTrailingPadding builds a real tar archive and then appends extra
// zero bytes after the writer's own end-of-archive marker. That mimics what
// borg's export-tar actually produces: a well formed archive followed by
// padding out to its block factor, which is more than the two zero blocks
// archive/tar itself writes on Close.
func tarWithTrailingPadding(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("contents of the only file in this archive")
	if err := tw.WriteHeader(&tar.Header{Name: "file.txt", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatalf("writing tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("writing tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}

	// GNU tar pads the archive out to a full record (traditionally 20 blocks,
	// 10240 bytes) past the end-of-archive marker. archive/tar's own Writer
	// does not do this, so it is added by hand to reproduce what a real
	// export-tar stream looks like on the wire.
	padding := make([]byte, 10240-buf.Len()%10240)
	buf.Write(padding)
	return buf.Bytes()
}

// TestDrainOnSuccessLetsTheWriterFinish reproduces the shape of a real borg
// restore: a writer keeps pushing bytes into a pipe after archive/tar's
// reader has already stopped at the end-of-archive marker, with the archive's
// trailing padding still unread. Without draining that padding before the
// read end is closed, the writer's next write fails with a closed pipe even
// though every file was extracted correctly - which is exactly the spurious
// failure seen on a real node. This test never touches a borg binary.
func TestDrainOnSuccessLetsTheWriterFinish(t *testing.T) {
	archive := tarWithTrailingPadding(t)

	pr, pw := io.Pipe()
	writeErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(pw, bytes.NewReader(archive))
		_ = pw.CloseWithError(err)
		writeErr <- err
	}()

	// Stand-in for archives.Tar{}.Extract: read every entry with the stdlib
	// tar reader and stop at the end-of-archive marker, leaving the trailing
	// padding above unread on the pipe.
	tr := tar.NewReader(pr)
	var extractErr error
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			extractErr = err
			break
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			extractErr = err
			break
		}
	}

	// This is the fix under test: draining before closing the read end lets
	// the writer above reach EOF on its own instead of hitting a closed pipe.
	err := DrainOnSuccess(extractErr, pr)
	_ = pr.CloseWithError(err)

	if err != nil {
		t.Fatalf("DrainOnSuccess() = %v, want nil", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("writer side error = %v, want nil (the padding should have been drained, not left to fail the writer)", err)
	}
}

// TestDrainOnSuccessLeavesARealFailureAlone checks the other half of the
// contract: a genuine extraction failure must not be papered over by
// draining past it, and the error it returns must be the original one.
func TestDrainOnSuccessLeavesARealFailureAlone(t *testing.T) {
	want := errors.New("boom")
	pr, pw := io.Pipe()
	_ = pw.CloseWithError(errors.New("writer already gave up"))

	if got := DrainOnSuccess(want, pr); got != want {
		t.Fatalf("DrainOnSuccess(%v, ...) = %v, want the original error returned unchanged", want, got)
	}
}
