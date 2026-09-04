package filesystem

import (
	"context"
	"io"
	"path"
)

// StreamDirectory writes a directory out to w as a tar.gz, generated on the
// fly. It reuses the same Archive that CompressFiles builds for the "create
// archive" button, with the directory being downloaded standing in as the
// single entry of an allow-list rooted at its parent. That is the mechanism
// CompressFiles already documents for archiving a folder out of a larger
// directory, so a folder pulled down through this path has the identical
// layout to one archived first and downloaded second.
//
// Nothing is written to disk here. Stream is handed w directly, so the
// gzip and tar writers produce bytes straight into whatever is consuming
// this call; there is no intermediate file to create or clean up.
func (fs *Filesystem) StreamDirectory(ctx context.Context, p string, w io.Writer) error {
	// Anchor before cleaning. path.Clean on its own maps ".", "" and "./" to
	// ".", which would fall into the else branch below with BaseDirectory:
	// "." and Files: ["."], a filter nothing walked ever matches, silently
	// producing an empty archive. Prefixing with "/" first sends all three of
	// those, along with "/" itself, to the same cleaned root: "/". Absolute
	// and relative real paths are unaffected: "/foo/" and "foo" both still
	// clean down to "/foo".
	p = path.Clean("/" + p)

	a := &Archive{Filesystem: fs}
	if p == "/" {
		// path.Base("/") is "/" itself, which would end up in Files and match
		// nothing when walked, quietly producing an empty archive instead of
		// one holding the whole server. Archive.Stream treats an empty Files
		// slice as "everything under BaseDirectory", so the root is archived
		// by leaving Files unset and pointing BaseDirectory at it directly.
		a.BaseDirectory = "/"
	} else {
		a.BaseDirectory = path.Dir(p)
		a.Files = []string{path.Base(p)}
	}

	return a.Stream(ctx, w)
}
