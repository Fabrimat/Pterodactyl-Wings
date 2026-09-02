package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
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
