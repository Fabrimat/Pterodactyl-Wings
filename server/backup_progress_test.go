package server

import (
	"runtime"
	"testing"
	"time"

	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/server/backup"
)

const backupProgressTestUUID = "11111111-1111-1111-1111-111111111111"

// waitForGoroutines polls until the goroutine count has come back down to the
// baseline. It is a poll rather than a single reading because a goroutine that
// has run its last deferred call still counts for a moment afterwards, and a
// leak fails this by never coming back down rather than by timing.
func waitForGoroutines(t *testing.T, baseline int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected the progress reporter to have exited, still at %d goroutines against a baseline of %d", runtime.NumGoroutine(), baseline)
}

// drainEvents returns everything the bus has queued up for the sink without
// waiting for anything more to arrive.
func drainEvents(sink chan []byte) []events.Event {
	var out []events.Event
	for {
		select {
		case raw := <-sink:
			out = append(out, events.MustDecode(raw))
		default:
			return out
		}
	}
}

// TestTrackBackupProgressPublishesFinalStateOnStop covers the two things the
// reporter has to get right for the completion event to mean anything: the
// stop function is what publishes the last set of numbers, and it does not
// return until the goroutine behind it has gone. A reporter that outlived the
// backup would keep publishing after a client has already been told the backup
// is over.
func TestTrackBackupProgressPublishesFinalStateOnStop(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.CtxCancel()

	sink := make(chan []byte, 8)
	s.Events().On(sink)
	defer s.Events().Off(sink)

	baseline := runtime.NumGoroutine()

	b := backup.NewLocal(nil, backupProgressTestUUID, "")
	stop := s.trackBackupProgress(b, 4096)
	stop()

	waitForGoroutines(t, baseline)

	published := drainEvents(sink)
	if len(published) != 1 {
		t.Fatalf("expected exactly one progress event to be published by the stop, got %d", len(published))
	}
	if published[0].Topic != BackupProgressEvent {
		t.Fatalf("expected the event to be published as %q, got %q", BackupProgressEvent, published[0].Topic)
	}

	data, ok := published[0].Data.(map[string]interface{})
	if !ok {
		t.Fatalf("expected the event payload to be an object, got %T", published[0].Data)
	}
	if data["uuid"] != backupProgressTestUUID {
		t.Fatalf("expected the payload to name the backup, got %v", data["uuid"])
	}
	if data["bytes_written"] != float64(0) {
		t.Fatalf("expected no bytes written for a backup that never ran, got %v", data["bytes_written"])
	}
	if data["bytes_total"] != float64(4096) {
		t.Fatalf("expected the total handed to the reporter to be passed through, got %v", data["bytes_total"])
	}

	// Nothing may follow the final event, since the completion event the caller
	// publishes next has to be the last word on this backup.
	time.Sleep(50 * time.Millisecond)
	if extra := drainEvents(sink); len(extra) != 0 {
		t.Fatalf("expected nothing to be published after the stop returned, got %d events", len(extra))
	}
}

// TestTrackBackupProgressStopsWithTheServerContext covers the other way out of
// the reporting goroutine. A server being torn down takes the sockets this was
// written for with it, so the reporter leaves without a final event, and the
// stop function still has to return rather than wait on a goroutine that has
// already gone.
func TestTrackBackupProgressStopsWithTheServerContext(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	sink := make(chan []byte, 8)
	s.Events().On(sink)
	defer s.Events().Off(sink)

	baseline := runtime.NumGoroutine()

	b := backup.NewLocal(nil, backupProgressTestUUID, "")
	stop := s.trackBackupProgress(b, 0)
	s.CtxCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		stop()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("expected the stop to return once the server context was canceled")
	}

	waitForGoroutines(t, baseline)
}
