package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pterodactyl/wings/config"
	wserver "github.com/pterodactyl/wings/server"
)

const nodeBackupTestToken = "node-backup-test-token"

// nodeBackupTestEngine builds the real router so that these tests exercise the
// registration and the middleware chain rather than a hand-assembled context.
// Driving the engine is the whole point here: the node-scoped delete route runs
// without ServerExists, so a test that set up its own context would prove
// nothing about what the handler is actually given at runtime.
func nodeBackupTestEngine(t *testing.T) http.Handler {
	t.Helper()
	config.Set(&config.Configuration{AuthenticationToken: nodeBackupTestToken})
	return Configure(wserver.NewEmptyManager(backupTestRemoteClient{}), backupTestRemoteClient{})
}

func nodeBackupTestDelete(t *testing.T, path, body string, authorize bool) *httptest.ResponseRecorder {
	t.Helper()
	// A nil io.Reader is what httptest.NewRequest wants for a request with no
	// body. A typed nil pointer would satisfy the interface and then be read.
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodDelete, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorize {
		req.Header.Set("Authorization", "Bearer "+nodeBackupTestToken)
	}

	w := httptest.NewRecorder()
	nodeBackupTestEngine(t).ServeHTTP(w, req)
	return w
}

// TestDeleteBackupNodeScopedRouteIsRegistered is the test this route exists
// for. Without the registration gin answers with its own bare 404, which is
// indistinguishable from the handler's own 404 by status alone, so the body is
// what is asserted here: a caller has to be able to tell "this node has no such
// backup file" apart from "this node has no such route".
func TestDeleteBackupNodeScopedRouteIsRegistered(t *testing.T) {
	w := nodeBackupTestDelete(t, "/api/backups/11111111-1111-1111-1111-111111111111", "", true)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected a delete for a backup missing on disk to 404, got status %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "The requested backup was not found on this server.") {
		t.Fatalf("expected the handler's own 404 body rather than gin's default, got %s", w.Body.String())
	}
}

// TestDeleteBackupNodeScopedRouteReachesBorgBranch covers the part of the
// handler that runs before a borg delete is acknowledged. It needs no borg
// binary and starts no background work, but it does reach the ExtractLogger
// call on the borg path, which panics if the logger is missing - and off the
// server-scoped route the logger can only have come from AttachRequestID.
func TestDeleteBackupNodeScopedRouteReachesBorgBranch(t *testing.T) {
	backupID := "11111111-1111-1111-1111-111111111111"
	w := nodeBackupTestDelete(t, "/api/backups/"+backupID, `{"borg":{"archive":"`+backupID+`"}}`, true)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected a borg delete with no repository to be rejected before ack, got status %d body %s", w.Code, w.Body.String())
	}
}

// TestDeleteBackupNodeScopedRouteRequiresAuthorization confirms the route
// landed inside the protected block. It is registered as a bare path on the
// engine rather than in a group, so nothing about its own line says whether the
// authorization middleware is in front of it.
func TestDeleteBackupNodeScopedRouteRequiresAuthorization(t *testing.T) {
	w := nodeBackupTestDelete(t, "/api/backups/11111111-1111-1111-1111-111111111111", "", false)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected an unauthenticated delete to be rejected, got status %d body %s", w.Code, w.Body.String())
	}
}
