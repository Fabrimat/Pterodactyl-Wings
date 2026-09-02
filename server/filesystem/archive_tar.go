package filesystem

import (
	"archive/tar"
	"context"
	"io"
	"strings"

	"emperror.dev/errors"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pterodactyl/wings/internal/ufs"
)

// StreamTar streams the archive to the given writer as an uncompressed tar.
//
// Stream always wraps its output in gzip, even with the compression level set
// to none, so it cannot be used where a plain tar is required. The walk setup
// below is a copy of Stream's rather than a shared helper the two both call,
// because this file has to keep merging cleanly from upstream and a new file
// cannot conflict. The duplication is the price of that.
//
// This goes through the same unixFS sandbox as Stream does, which is the whole
// point of building the tar here instead of letting an external tool walk the
// server's data directory.
func (a *Archive) StreamTar(ctx context.Context, w io.Writer) error {
	if a.Filesystem == nil {
		return errors.New("filesystem: archive.Filesystem is unset")
	}

	// The base directory may come with a prefixed `/`, strip it to prevent
	// problems.
	a.BaseDirectory = strings.TrimPrefix(a.BaseDirectory, "/")

	if filesLen := len(a.Files); filesLen > 0 {
		files := make([]string, filesLen)
		for i, f := range a.Files {
			if !strings.HasPrefix(f, a.Filesystem.Path()) {
				files[i] = f
				continue
			}
			files[i] = strings.TrimPrefix(strings.TrimPrefix(f, a.Filesystem.Path()), "/")
		}
		a.Files = files
	}

	// Create a new tar writer directly around the given writer. Compression is
	// the caller's business here.
	tw := tar.NewWriter(w)
	defer tw.Close()

	a.w = NewTarProgress(tw, a.Progress)

	fs := a.Filesystem.unixFS

	// If we're specifically looking for only certain files, or have requested
	// that certain files be ignored we'll update the callback function to reflect
	// that request.
	var callback walkFunc
	if len(a.Files) == 0 && len(a.Ignore) > 0 {
		i := ignore.CompileIgnoreLines(strings.Split(a.Ignore, "\n")...)
		callback = a.callback(func(_ int, _, relative string, _ ufs.DirEntry) error {
			if i.MatchesPath(relative) {
				return SkipThis
			}
			return nil
		})
	} else if len(a.Files) > 0 {
		callback = a.withFilesCallback()
	} else {
		callback = a.callback()
	}

	// Open the base directory we were provided.
	dirfd, name, closeFd, err := fs.SafePath(a.BaseDirectory)
	defer closeFd()
	if err != nil {
		return err
	}

	// Recursively walk the base directory.
	return fs.WalkDirat(dirfd, name, func(dirfd int, name, relative string, d ufs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return callback(dirfd, name, relative, d)
		}
	})
}
