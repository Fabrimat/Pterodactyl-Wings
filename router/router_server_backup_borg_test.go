package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/backup"
)

func TestRequireBorgConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if requireBorgConfiguration(c, nil) {
		t.Fatal("expected a missing borg configuration to be rejected")
	}
	if c.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected status %d for a missing borg configuration, got %d", http.StatusBadRequest, c.Writer.Status())
	}

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	backupID := "11111111-1111-1111-1111-111111111111"
	if !requireBorgConfiguration(c2, &remote.BorgConfiguration{Repository: "/srv/borg/server", Archive: backupID}) {
		t.Fatal("expected a complete borg configuration to be accepted")
	}

	w3 := httptest.NewRecorder()
	c3, _ := gin.CreateTestContext(w3)
	if requireBorgConfiguration(c3, &remote.BorgConfiguration{Archive: backupID}) {
		t.Fatal("expected a borg configuration with no repository to be rejected")
	}
	if c3.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected status %d for a borg configuration with no repository, got %d", http.StatusBadRequest, c3.Writer.Status())
	}

	w4 := httptest.NewRecorder()
	c4, _ := gin.CreateTestContext(w4)
	if requireBorgConfiguration(c4, &remote.BorgConfiguration{Repository: "/srv/borg/server"}) {
		t.Fatal("expected a borg configuration with no archive to be rejected")
	}
	if c4.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected status %d for a borg configuration with no archive, got %d", http.StatusBadRequest, c4.Writer.Status())
	}
}

func TestPostServerBackupRejectsMissingBorgConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	backupID := "11111111-1111-1111-1111-111111111111"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/servers/server/backup", strings.NewReader(fmt.Sprintf(`{"adapter":"borg","uuid":%q}`, backupID)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "server", Value: "server"}}

	client := backupTestRemoteClient{}
	s, err := wserver.New(client)
	if err != nil {
		t.Fatal(err)
	}
	s.Config().Uuid = "server"
	s.Environment = backupTestEnvironment{}
	defer s.CtxCancel()

	c.Set("server", s)
	c.Set("api_client", client)
	c.Set("logger", log.WithField("test", t.Name()))

	postServerBackup(c)

	if c.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected a backup request with adapter \"borg\" and no borg field to be rejected, got status %d body %s", c.Writer.Status(), w.Body.String())
	}
}

// TestDeleteServerBackupEmptyBodyBehavesAsBefore guards against the optional
// body bind added for the borg branch breaking the wings/s3 delete path,
// which historically receives no body at all.
func TestDeleteServerBackupEmptyBodyBehavesAsBefore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	backupID := "11111111-1111-1111-1111-111111111111"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/servers/server/backup/"+backupID, nil)
	c.Params = gin.Params{
		{Key: "server", Value: "server"},
		{Key: "backup", Value: backupID},
	}
	c.Set("api_client", backupTestRemoteClient{})

	deleteServerBackup(c)

	if c.Writer.Status() != http.StatusNotFound {
		t.Fatalf("expected an empty-body delete for a backup missing on disk to 404 exactly as before, got status %d body %s", c.Writer.Status(), w.Body.String())
	}
}

// TestDeleteServerBackupBorgRejectsIncompleteConfigurationBeforeAck confirms
// an incomplete borg configuration is rejected synchronously, before the 204
// is sent, rather than acknowledging a delete that was never actionable and
// only reporting that in a background log line the panel never sees.
func TestDeleteServerBackupBorgRejectsIncompleteConfigurationBeforeAck(t *testing.T) {
	gin.SetMode(gin.TestMode)

	backupID := "11111111-1111-1111-1111-111111111111"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/servers/server/backup/"+backupID, strings.NewReader(`{"borg":{"archive":"`+backupID+`"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{
		{Key: "server", Value: "server"},
		{Key: "backup", Value: backupID},
	}
	c.Set("api_client", backupTestRemoteClient{})
	c.Set("logger", log.WithField("test", t.Name()))

	deleteServerBackup(c)

	if c.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected a borg delete with no repository to be rejected before ack, got status %d body %s", c.Writer.Status(), w.Body.String())
	}
}

// TestRestoreServerBackupBorgDoesNotPublishCompletionOnFailure guards against
// a failed borg restore being reported to the panel as a success. The
// repository given here does not exist, so RestoreBackup is guaranteed to
// fail regardless of whether a borg binary happens to be on this machine.
func TestRestoreServerBackupBorgDoesNotPublishCompletionOnFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	client := backupTestRemoteClient{restoreStatus: make(chan string, 1)}
	s, err := wserver.New(client)
	if err != nil {
		t.Fatal(err)
	}
	s.Config().Uuid = "server"
	s.Environment = backupTestEnvironment{}
	defer s.CtxCancel()

	s.SetRestoring(true)

	sink := make(chan []byte, 16)
	s.Events().On(sink)
	defer s.Events().Off(sink)

	backupID := "11111111-1111-1111-1111-111111111111"
	cfg := &remote.BorgConfiguration{
		Repository: filepath.Join(t.TempDir(), "does-not-exist"),
		Archive:    backupID,
	}
	b := backup.NewBorg(client, backupID, "", cfg)

	restoreServerBackupBorg(s, b, log.WithField("test", t.Name()))

	deadline := time.Now().Add(10 * time.Second)
	for s.IsRestoring() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.IsRestoring() {
		t.Fatal("expected SetRestoring(false) to run after a failed restore")
	}

	var sawFailureMessage, sawCompletion bool
	for drained := false; !drained; {
		select {
		case raw := <-sink:
			ev := events.MustDecode(raw)
			switch ev.Topic {
			case wserver.BackupRestoreCompletedEvent:
				sawCompletion = true
			case wserver.DaemonMessageEvent:
				if msg, ok := ev.Data.(string); ok && strings.Contains(strings.ToLower(msg), "failed") {
					sawFailureMessage = true
				}
			}
		default:
			drained = true
		}
	}

	if sawCompletion {
		t.Fatal("expected a failed borg restore not to publish the completion event")
	}
	if !sawFailureMessage {
		t.Fatal("expected a failed borg restore to publish a daemon message stating the failure")
	}
}

// borgDownloadStub stands in for the borg adapter on the download path. The
// real one needs a borg binary and a populated repository behind it, neither
// of which a unit test has, and the behaviour under test here is the order the
// router does things in rather than anything borg does.
type borgDownloadStub struct {
	exists    bool
	existsErr error
	exported  bool
}

func (s *borgDownloadStub) ArchiveExists(context.Context) (bool, error) {
	return s.exists, s.existsErr
}

func (s *borgDownloadStub) ExportTar(_ context.Context, w io.Writer) error {
	s.exported = true
	_, err := w.Write([]byte("tar"))
	return err
}

func newBorgDownloadContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/download/backup", nil)
	c.Set("logger", log.WithField("test", t.Name()))
	return c, w
}

// TestStreamBorgBackupDownloadRejectsMissingArchive covers the ordering that
// makes a failed download reportable at all. Borg writes nothing when the
// archive is not there, so a check made after the attachment headers went out
// leaves the client holding a 200 and an empty tar that looks exactly like a
// backup of an empty server.
func TestStreamBorgBackupDownloadRejectsMissingArchive(t *testing.T) {
	backupID := "11111111-1111-1111-1111-111111111111"
	c, w := newBorgDownloadContext(t)

	b := &borgDownloadStub{}
	streamBorgBackupDownload(c, b, backupID)

	if c.Writer.Status() != http.StatusNotFound {
		t.Fatalf("expected status %d for a backup that is not in the repository, got %d", http.StatusNotFound, c.Writer.Status())
	}
	if b.exported {
		t.Fatal("expected the export not to run for a backup that is not in the repository")
	}
	if v := w.Header().Get("Content-Disposition"); v != "" {
		t.Fatalf("expected no attachment header on a rejected download, got %q", v)
	}
	if v := w.Header().Get("Content-Type"); strings.Contains(v, "x-tar") {
		t.Fatalf("expected no tar content type on a rejected download, got %q", v)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected a JSON error body, got %q: %v", w.Body.String(), err)
	}
	if body.Error == "" {
		t.Fatalf("expected the JSON error body to carry a message, got %q", w.Body.String())
	}
}

// TestStreamBorgBackupDownloadReportsAnUnreadableRepository separates the two
// failures the check can run into. A repository that cannot be reached or
// unlocked is not an archive that is gone, and answering 404 to it would tell
// the panel to drop a backup that is still sitting in the repository. The
// status itself is left to the CaptureErrors middleware, so what is asserted
// here is that the error was handed to it rather than turned into a 404.
func TestStreamBorgBackupDownloadReportsAnUnreadableRepository(t *testing.T) {
	backupID := "11111111-1111-1111-1111-111111111111"
	c, w := newBorgDownloadContext(t)

	b := &borgDownloadStub{existsErr: errors.New("borg: could not open the repository")}
	streamBorgBackupDownload(c, b, backupID)

	if !c.IsAborted() {
		t.Fatal("expected an unreadable repository to abort the request")
	}
	if len(c.Errors) == 0 {
		t.Fatal("expected an unreadable repository to be reported through the error middleware rather than swallowed")
	}
	if c.Writer.Status() == http.StatusNotFound {
		t.Fatal("expected an unreadable repository not to be reported as a missing backup")
	}
	if b.exported {
		t.Fatal("expected the export not to run when the repository could not be read")
	}
	if v := w.Header().Get("Content-Disposition"); v != "" {
		t.Fatalf("expected no attachment header on a rejected download, got %q", v)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("expected no body to be written for an unreadable repository, got %q", w.Body.String())
	}
}

// TestStreamBorgBackupDownloadStreamsAnExistingArchive is the other half of
// the guard above: the new check must not stand in the way of a download that
// was always going to work.
func TestStreamBorgBackupDownloadStreamsAnExistingArchive(t *testing.T) {
	backupID := "11111111-1111-1111-1111-111111111111"
	c, w := newBorgDownloadContext(t)

	b := &borgDownloadStub{exists: true}
	streamBorgBackupDownload(c, b, backupID)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("expected status %d for an archive that is in the repository, got %d body %s", http.StatusOK, c.Writer.Status(), w.Body.String())
	}
	if !b.exported {
		t.Fatal("expected the export to run for an archive that is in the repository")
	}
	if v := w.Header().Get("Content-Disposition"); !strings.Contains(v, backupID+".tar") {
		t.Fatalf("expected the attachment header to name the backup, got %q", v)
	}
	if w.Body.String() != "tar" {
		t.Fatalf("expected the exported archive to reach the client, got %q", w.Body.String())
	}
}
