package server

import (
	"time"

	"github.com/pterodactyl/wings/internal/progress"
	"github.com/pterodactyl/wings/server/backup"
)

// backupProgressInterval is how often a running backup reports how far it has
// got. Every client watching the server's socket receives a copy of every
// event, so this is a deliberate trade of resolution against turning an hour
// long backup into a flood of frames nobody can see the difference between.
const backupProgressInterval = 2 * time.Second

// trackBackupProgress attaches a progress tracker to the backup and starts
// publishing what it records over the server's websocket. It returns the
// function that stops the reporting again, which the caller has to call on
// both the success and the failure path.
//
// The total is the server's cached disk usage rather than a fresh walk of the
// data directory: the archive's real size is not known until it has been
// written, and walking every file again just to draw a bar would cost more
// than the bar is worth. That value can be stale, and it is zero on a server
// whose usage has not been measured yet, which is passed through as it is - a
// zero total is the consumer's signal that the size is unknown, which is a
// truthful answer where a made up one would not be.
//
// The stop function publishes one last event and does not return until the
// reporting goroutine has finished with it. That ordering is the whole point:
// the completion event the caller publishes afterwards is always the last
// thing a client sees for this backup, and a client is never left looking at a
// bar frozen wherever the previous tick happened to land.
//
// This lives here rather than in an adapter because every adapter streams the
// same tar through filesystem.Archive, so the local, s3 and borg backups all
// report the same way without any of them knowing about the socket.
func (s *Server) trackBackupProgress(b backup.BackupInterface, total int64) func() {
	// A negative reading would wrap into an enormous total and draw a bar that
	// never moves, so it is flattened into the same "unknown" a zero means.
	p := progress.NewProgress(uint64(max(total, 0)))
	b.SetProgress(p)

	publish := func() {
		s.Events().Publish(BackupProgressEvent+":"+b.Identifier(), map[string]interface{}{
			"uuid":          b.Identifier(),
			"bytes_written": p.Written(),
			"bytes_total":   p.Total(),
		})
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)

		ticker := time.NewTicker(backupProgressInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				// The final event reports where the archive actually ended up
				// rather than wherever the last tick caught it.
				publish()
				return
			case <-s.Context().Done():
				// The server is being torn down, taking the sockets this was
				// being written for with it. Returning without the final event
				// keeps this from pushing into a bus that is going away.
				return
			case <-ticker.C:
				publish()
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}
