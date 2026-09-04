package router

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/backup"
)

// borgDeleteBackgroundTimeout bounds a borg archive delete that runs after the
// request that triggered it has already been acknowledged. The request
// context is gone the moment that ack is sent, and a delete plus the
// coalesced compact that follows it can run for minutes.
const borgDeleteBackgroundTimeout = 30 * time.Minute

// requireBorgConfiguration reports whether the request carried a usable borg
// configuration object, aborting with a 400 if it did not. This mirrors what
// BorgBackup.validate checks in package backup, which is unexported and
// reachable only by way of a real operation; the fields checked here are the
// same two it has no usable zero value for. Building the adapter with a
// missing or incomplete configuration and letting it fail later would only
// surface once the operation is already running in the background, where the
// caller sees nothing but an unexplained failure.
func requireBorgConfiguration(c *gin.Context, cfg *remote.BorgConfiguration) bool {
	if cfg != nil && cfg.Repository != "" && cfg.Archive != "" {
		return true
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"error": "The borg field is required when the backup adapter is set to borg.",
	})
	return false
}

// probeBorgRestoreArchive validates the borg configuration for a restore
// request and confirms the archive it names actually exists in the
// repository. This has to happen before the caller truncates the server's
// data directory: a lock timeout, an unreachable repository or a wrong
// passphrase must fail the request rather than empty the server out for a
// restore that was never going to succeed. The adapter is returned for reuse
// so the archive is only described once.
func probeBorgRestoreArchive(c *gin.Context, client remote.Client, uuid string, cfg *remote.BorgConfiguration) (*backup.BorgBackup, bool) {
	if !requireBorgConfiguration(c, cfg) {
		return nil, false
	}
	b := backup.NewBorg(client, uuid, "", cfg)
	exists, err := b.ArchiveExists(c.Request.Context())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Could not verify the borg archive before restoring: " + err.Error(),
		})
		return nil, false
	}
	if !exists {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "The requested backup archive was not found in the borg repository.",
		})
		return nil, false
	}
	return b, true
}

// restoreServerBackupBorg runs the borg branch of postServerRestoreBackup,
// mirroring the local adapter branch it sits next to. The adapter starts borg
// itself, so the reader handed to RestoreBackup is nil.
func restoreServerBackupBorg(s *server.Server, b *backup.BorgBackup, logger *log.Entry) {
	go func(s *server.Server, b *backup.BorgBackup, logger *log.Entry) {
		logger.Info("starting restoration process for server backup using borg driver")
		if err := s.RestoreBackup(b, nil); err != nil {
			logger.WithField("error", err).Error("failed to restore borg backup to server")
			s.Events().Publish(server.DaemonMessageEvent, "Server restoration from borg backup failed.")
			s.SetRestoring(false)
			return
		}
		s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from borg backup.")
		s.Events().Publish(server.BackupRestoreCompletedEvent, "")
		logger.Info("completed server restoration from borg backup")
		s.SetRestoring(false)
	}(s, b, logger)
}

// deleteServerBackupBorg runs the borg branch of deleteServerBackup. It has to
// run before backup.LocateLocal, since a borg backup has no local file and
// would just 404 there. The configuration is checked synchronously, before
// anything is acknowledged: it is a local, in-memory check, unlike the delete
// itself, so there is no reason to ack a request that is already known to be
// unactionable and then only report that in a background log line the panel
// has no way to see. A borg delete plus the compaction it queues can take
// minutes, well past the panel's Guzzle timeout on this call, so once the
// configuration is known good the response is sent immediately and the actual
// delete happens in the background against its own context rather than the
// now-dead request one.
func deleteServerBackupBorg(c *gin.Context, client remote.Client, uuid string, cfg *remote.BorgConfiguration, logger *log.Entry) {
	if !requireBorgConfiguration(c, cfg) {
		return
	}
	b := backup.NewBorg(client, uuid, "", cfg)
	c.Status(http.StatusNoContent)

	go func(b *backup.BorgBackup, uuid string, logger *log.Entry) {
		ctx, cancel := context.WithTimeout(context.Background(), borgDeleteBackgroundTimeout)
		defer cancel()
		if err := b.DeleteArchive(ctx); err != nil {
			// The panel has already dropped its row for this backup by the time
			// this runs, so there is no retry channel: a quiet failure here is an
			// unrecoverable storage leak.
			logger.WithField("backup", uuid).WithField("error", err).Error("failed to delete borg backup archive, it may be left behind in the repository")
		}
	}(b, uuid, logger)
}

// borgArchiveSource is the part of the borg adapter that the download path
// uses. The adapter is its only implementation; it is an interface so the
// ordering the download depends on - the archive is confirmed before a single
// header is written - can be covered without a borg binary and a populated
// repository on the machine running the tests.
type borgArchiveSource interface {
	ArchiveExists(ctx context.Context) (bool, error)
	ExportTar(ctx context.Context, w io.Writer) error
}

// getDownloadBackupBorg runs the borg fallback of getDownloadBackup. It is
// only reached once backup.LocateLocal has already reported that no local
// file exists for this backup, since S3 downloads never reach this endpoint
// at all - the panel presigns those directly.
func getDownloadBackupBorg(c *gin.Context, client remote.Client, uuid string) {
	cfg, err := client.GetBackupBorgConfiguration(c.Request.Context(), uuid)
	if err != nil {
		if stderrors.Is(err, remote.ErrBorgBackupNotFound) || stderrors.Is(err, remote.ErrBorgBackupInvalid) || stderrors.Is(err, remote.ErrBorgBackupForbidden) {
			// A node that does not own the server for this backup gets the exact
			// same response as one that does not exist, rather than a response
			// that confirms the backup is there but off limits to it.
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "The requested backup was not found on this server.",
			})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	streamBorgBackupDownload(c, backup.NewBorg(client, uuid, "", cfg), uuid)
}

// streamBorgBackupDownload writes a borg archive out to the client, confirming
// that it is actually in the repository before it commits to a response.
//
// That confirmation has to come first. Borg exits non-zero having written
// nothing when the archive is not there - a backup that is still running, one
// that failed, or one removed out of band - and a handler that has declared a
// tar attachment and then has nothing left to say is finished off as a 200
// with an empty body, which the caller cannot tell apart from a backup of a
// server with nothing on it.
func streamBorgBackupDownload(c *gin.Context, b borgArchiveSource, uuid string) {
	exists, err := b.ArchiveExists(c.Request.Context())
	if err != nil {
		// A repository that cannot be reached, unlocked or read is not the same
		// thing as an archive that is not in it, and answering 404 to it would
		// tell the panel a backup is gone while it is still sitting there.
		middleware.CaptureAndAbort(c, err)
		return
	}
	if !exists {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested backup was not found on this server.",
		})
		return
	}

	c.Header("Content-Type", "application/x-tar")
	c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(uuid+".tar"))
	// The archive's size is not known ahead of the stream, so there is no
	// Content-Length to set here; the response is chunked instead.
	if err := b.ExportTar(c.Request.Context(), c.Writer); err != nil {
		// Setting those two headers committed nothing: gin writes the status
		// line on the first write and not before, so what is still possible
		// here is decided by whether any of the archive got out, not by how
		// far along the handler is.
		//
		// Confirming the archive above does not close off a failure that
		// produces no bytes at all. ArchiveExists takes a shared lock on the
		// repository and a borg create takes an exclusive one, so a backup
		// starting in the window between the two makes the export block and
		// then give up on lock_wait with nothing written, on a repository that
		// is reachable and an archive that is there.
		if !c.Writer.Written() {
			// The headers are still sitting in the map, and gin only fills in
			// a Content-Type that is absent, so an error rendered over the top
			// of these would reach the browser as a tar attachment and be
			// saved as a .tar holding a JSON error.
			c.Header("Content-Type", "")
			c.Header("Content-Disposition", "")
			middleware.CaptureAndAbort(c, err)
			return
		}
		// Past the first byte the status line has already gone, so there is no
		// response left to turn into an error and logging it is all that is
		// left to do.
		middleware.ExtractLogger(c).WithField("backup", uuid).WithField("error", err).Error("failed to stream borg backup download to client")
	}
}
