package borg

import "io"

// DrainOnSuccess drains r to EOF when err is nil and returns whatever that
// drain produces instead of the nil it was called with. A tar stream ends
// with a two block end-of-archive marker and is then padded out to a block
// factor; Go's tar reader stops right after the marker and never reads that
// padding. A caller that closes its side of a pipe the moment its reader is
// satisfied therefore closes it while the other end is still writing that
// padding, so the write fails with a closed pipe and an otherwise successful
// operation is reported as an error. Draining first lets the writer reach
// EOF on its own and exit cleanly before the pipe is closed.
//
// err is returned unchanged when it is already non-nil: a reader that failed
// partway through has nothing left it can trust to read, and draining past a
// real failure would risk turning it into a false success.
func DrainOnSuccess(err error, r io.Reader) error {
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, r)
	return err
}
