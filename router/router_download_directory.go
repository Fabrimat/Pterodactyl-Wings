package router

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/filesystem"
)

// streamDirectoryDownload writes a directory out to the client as a
// tar.gz, compressing it straight into the response as it goes rather than
// staging it to disk first. The error handling here follows
// streamBorgBackupDownload: a stream that fails after it has already written
// something has no response left to turn into an error, only a status line
// that already went out with a promise the body cannot keep.
func streamDirectoryDownload(c *gin.Context, s *server.Server, p string, st filesystem.Stat) {
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(st.Name()+".tar.gz"))
	// The archive's size is not known ahead of the stream, so there is no
	// Content-Length to set here; the response is chunked instead.
	if err := s.Filesystem().StreamDirectory(c.Request.Context(), p, c.Writer); err != nil {
		if !c.Writer.Written() {
			// The headers are still sitting in the map, and gin only fills in
			// a Content-Type that is absent, so an error rendered over the top
			// of these would reach the browser as a tar.gz attachment and be
			// saved as one holding a JSON error.
			c.Header("Content-Type", "")
			c.Header("Content-Disposition", "")
			middleware.CaptureAndAbort(c, err)
			return
		}
		// Past the first byte the status line has already gone, so there is no
		// response left to turn into an error and logging it is all that is
		// left to do.
		middleware.ExtractLogger(c).WithField("path", p).WithField("error", err).Error("failed to stream directory download to client")
	}
}
