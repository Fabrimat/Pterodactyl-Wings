package router

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/system"
)

func TestPostUpdateConfigurationRotatesCredentials(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "old-id"
	cfg.AuthenticationToken = "old-token"
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	credentials := make(chan [2]string, 1)
	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: credentials})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequest("POST", "/api/update", strings.NewReader(`{"token_id":"new-id","token":"new-token"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected successful update, got status %d", recorder.Code)
	}
	updated := config.Get()
	if updated.Token.ID != "new-id" || updated.Token.Token != "new-token" {
		t.Fatalf("unexpected resolved credentials: %#v", updated.Token)
	}
	select {
	case got := <-credentials:
		if got != [2]string{"new-id", "new-token"} {
			t.Fatalf("unexpected client credentials: %#v", got)
		}
	default:
		t.Fatal("expected client credentials to be rotated")
	}
}

func assertHasFeatures(t *testing.T, body []byte) {
	t.Helper()

	var decoded struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}

	for _, want := range system.Features {
		found := false
		for _, got := range decoded.Features {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected features %v to contain %q, response was %s", decoded.Features, want, body)
		}
	}
}

// The Panel calls this endpoint with no query string, so this is the shape
// it actually receives.
func TestGetSystemInformationTrimmedResponseIncludesFeatures(t *testing.T) {
	info := &system.Information{
		Version:  "test",
		Features: system.Features,
	}

	body, err := json.Marshal(newTrimmedSystemInformation(info))
	if err != nil {
		t.Fatal(err)
	}
	assertHasFeatures(t, body)
}

// The full shape returned by ?v=2 must carry the same features array.
func TestGetSystemInformationFullResponseIncludesFeatures(t *testing.T) {
	info := &system.Information{
		Version:  "test",
		Features: system.Features,
	}

	body, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	assertHasFeatures(t, body)
}
