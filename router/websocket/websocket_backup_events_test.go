package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gorilla/websocket"

	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
)

const (
	backupEventTestServerUUID = "11111111-1111-1111-1111-111111111111"
	backupEventTestUserUUID   = "22222222-2222-2222-2222-222222222222"
	backupEventTestBackupUUID = "33333333-3333-3333-3333-333333333333"
)

// newBackupEventHandler returns a handler whose JWT carries exactly the
// permissions given, wired to a real socket so the test reads what the
// permission gate actually let onto the wire rather than what it was asked to
// send. The returned connection is the client end of that socket.
func newBackupEventHandler(t *testing.T, permissions ...string) (*Handler, *websocket.Conn) {
	t.Helper()

	s, err := server.New(nil)
	if err != nil {
		t.Fatalf("could not create the test server: %v", err)
	}
	t.Cleanup(s.CtxCancel)
	s.Config().Uuid = backupEventTestServerUUID

	conn, client := newTestSocket(t)

	return &Handler{
		Connection: conn,
		server:     s,
		limiter:    NewLimiter(),
		jwt: &tokens.WebsocketPayload{
			Payload: jwt.Payload{
				ExpirationTime: jwt.NumericDate(time.Now().Add(time.Hour)),
				// NumericDate rounds down to a whole second, so an issue time
				// of "now" lands before the boot time the denylist measures
				// against and the token comes back revoked. A second out keeps
				// the rounding on the right side of it.
				IssuedAt: jwt.NumericDate(time.Now().Add(time.Second)),
			},
			Scoped:      tokens.Scoped{Scope: string(tokens.Websocket)},
			UserUUID:    backupEventTestUserUUID,
			ServerUUID:  backupEventTestServerUUID,
			Permissions: permissions,
		},
	}, client
}

// newTestSocket returns both ends of a websocket connection. The handler under
// test writes to the first, the test reads from the second.
func newTestSocket(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	upgrader := websocket.Upgrader{}
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted <- c
	}))
	t.Cleanup(srv.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("could not dial the test socket: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case conn := <-accepted:
		t.Cleanup(func() { _ = conn.Close() })
		return conn, client
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the test socket to be accepted")
		return nil, nil
	}
}

// readEvent returns the name of the next event to arrive on the socket.
func readEvent(t *testing.T, client *websocket.Conn) string {
	t.Helper()

	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	var m Message
	if err := client.ReadJSON(&m); err != nil {
		t.Fatalf("could not read from the test socket: %v", err)
	}
	return string(m.Event)
}

// TestSendJsonGatesBackupEvents walks the events that carry something about a
// backup through the permission gate. They are checked as a set rather than
// one at a time because an ungated one hands a subuser who was never given
// backup.read a running commentary on the server's backups.
func TestSendJsonGatesBackupEvents(t *testing.T) {
	for _, event := range []string{
		server.BackupCompletedEvent + ":" + backupEventTestBackupUUID,
		server.BackupProgressEvent + ":" + backupEventTestBackupUUID,
	} {
		t.Run(event, func(t *testing.T) {
			t.Run("withheld without the permission", func(t *testing.T) {
				h, client := newBackupEventHandler(t, PermissionConnect)

				if err := h.SendJson(Message{Event: Event(event)}); err != nil {
					t.Fatalf("SendJson returned an error: %v", err)
				}
				// An ungated event sent straight after is the marker: if the
				// backup event was withheld this is the first thing on the
				// wire, so nothing has to wait out a timeout to find out.
				if err := h.SendJson(Message{Event: Event(server.StatusEvent)}); err != nil {
					t.Fatalf("SendJson returned an error for the marker event: %v", err)
				}

				if got := readEvent(t, client); got != server.StatusEvent {
					t.Fatalf("expected %q to be withheld from a JWT without %q, got %q over the socket", event, PermissionReceiveBackups, got)
				}
			})

			t.Run("delivered with the permission", func(t *testing.T) {
				h, client := newBackupEventHandler(t, PermissionConnect, PermissionReceiveBackups)

				if err := h.SendJson(Message{Event: Event(event)}); err != nil {
					t.Fatalf("SendJson returned an error: %v", err)
				}

				if got := readEvent(t, client); got != event {
					t.Fatalf("expected %q to reach a JWT holding %q, got %q", event, PermissionReceiveBackups, got)
				}
			})
		})
	}
}

// TestSendJsonDeliversRestoreCompletionWithoutTheBackupPermission pins the one
// event held out of the gate above. It is published with an empty payload, so
// there is nothing in it to withhold, and it is what clears the restoring
// state the panel puts the whole server UI behind. A console or files subuser
// carries no backup.read and would sit on the restoring screen until they
// reloaded the page if this were gated with the rest of the namespace.
func TestSendJsonDeliversRestoreCompletionWithoutTheBackupPermission(t *testing.T) {
	h, client := newBackupEventHandler(t, PermissionConnect)

	if err := h.SendJson(Message{Event: Event(server.BackupRestoreCompletedEvent), Args: []string{""}}); err != nil {
		t.Fatalf("SendJson returned an error: %v", err)
	}

	if got := readEvent(t, client); got != server.BackupRestoreCompletedEvent {
		t.Fatalf("expected %q to reach a JWT without %q, got %q", server.BackupRestoreCompletedEvent, PermissionReceiveBackups, got)
	}
}
