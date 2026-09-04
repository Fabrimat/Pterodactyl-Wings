package filesystem

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	. "github.com/franela/goblin"
)

func TestFilesystem_StreamDirectory(t *testing.T) {
	g := Goblin(t)
	fs, _ := NewFs()

	g.Describe("StreamDirectory", func() {
		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})

		g.It("archives a nested directory using its name as the prefix", func() {
			g.Assert(fs.CreateDirectory("test", "/")).IsNil()

			r := strings.NewReader("hello, world!\n")
			g.Assert(fs.Write("test/a.txt", r, r.Size(), 0o644)).IsNil()

			// Real requests carry an absolute file_path, so exercise the same
			// shape here rather than a relative one that would take a
			// different branch inside StreamDirectory.
			var buf bytes.Buffer
			g.Assert(fs.StreamDirectory(context.Background(), "/test", &buf)).IsNil()

			names, err := readTarNames(&buf)
			g.Assert(err).IsNil()
			g.Assert(slices.Contains(names, "test/a.txt")).IsTrue()
		})

		g.It("archives the whole server when the path is the root", func() {
			g.Assert(fs.CreateDirectory("test", "/")).IsNil()

			r := strings.NewReader("hello, world!\n")
			g.Assert(fs.Write("test/a.txt", r, r.Size(), 0o644)).IsNil()

			var buf bytes.Buffer
			g.Assert(fs.StreamDirectory(context.Background(), "/", &buf)).IsNil()

			names, err := readTarNames(&buf)
			g.Assert(err).IsNil()
			g.Assert(len(names) > 0).IsTrue()
		})

		g.It("archives the whole server when the path is a bare dot", func() {
			// Panel-side validation only requires `file` to be a non-empty
			// string, so "." reaches here as a real request would. Before the
			// path was anchored with a leading "/" ahead of path.Clean, this
			// normalised to "." instead of "/", took the non-root branch with
			// BaseDirectory: "." and Files: ["."], and produced a silently
			// empty archive instead of an error.
			g.Assert(fs.CreateDirectory("test", "/")).IsNil()

			r := strings.NewReader("hello, world!\n")
			g.Assert(fs.Write("test/a.txt", r, r.Size(), 0o644)).IsNil()

			var buf bytes.Buffer
			g.Assert(fs.StreamDirectory(context.Background(), ".", &buf)).IsNil()

			names, err := readTarNames(&buf)
			g.Assert(err).IsNil()
			g.Assert(slices.Contains(names, "test/a.txt")).IsTrue()
		})
	})
}

// readTarNames decompresses and reads back a gzip tar produced by
// StreamDirectory, returning the names of the entries it holds.
func readTarNames(r io.Reader) ([]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	var names []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
	return names, nil
}
