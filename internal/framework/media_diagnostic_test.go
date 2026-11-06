package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
)

func TestMediaDiagnosticIsBoundedAndContainsNoMediaOrCredentials(t *testing.T) {
	events, err := activity.Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	s := &Service{config: FileConfig{Role: RoleZone, InstanceID: "zone-1"}, activity: events}
	d := &mediaLinkDiagnostic{s: s, peer: "application-1", generation: 4}
	d.beginFrame(dispatch.Frame{CallID: "diagnostic-call", SourceID: 10020, TalkgroupID: 1902, Sequence: 7, PCM: bytes.Repeat([]byte{0x49}, 320), Encoded: []byte("not-retained"), MessageIndicator: []byte("not-retained")}, 325*time.Millisecond)
	d.endFrame(141 * time.Millisecond)
	if len(d.last.PCM) != 0 || len(d.last.Encoded) != 0 || len(d.last.MessageIndicator) != 0 {
		t.Fatal("diagnostic retained audio or crypto framing")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.fail("route_write", errors.New("delayed media")) }()
	}
	wg.Wait()
	if err = events.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := events.Events(activity.Query{Audience: "diagnostic"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("disconnect produced more than one observation: %d %v", len(rows), err)
	}
	row := rows[0]
	if row.Type != "media_link" || row.CallID != "diagnostic-call" || row.RadioID != 10020 || row.Details["stage"] != "route_write" || row.Details["maximum_processing_ms"] != float64(141) || row.Details["last_frame_age_ms"] != float64(325) {
		t.Fatalf("missing diagnostic cause/timing: %#v", row)
	}
	raw, _ := json.Marshal(row)
	for _, field := range []string{`"pcm"`, `"encoded"`, `"message_indicator"`, `"token"`, "not-retained", "SUlJSUlJ"} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatal("transport diagnostic exposed media or credential fields", field)
		}
	}
}

// Send directly to the authenticated link to isolate the receiver's
// rejection, independent of sender-side freshness admission checks.

func TestMediaDiagnosticDoesNotWarnForIntentionalCloseOrShutdown(t *testing.T) {
	events, err := activity.Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{config: FileConfig{Role: RoleApplication, InstanceID: "application-1"}, activity: events, operationCtx: ctx}
	intentional := &mediaLinkDiagnostic{s: s}
	intentional.suppress()
	intentional.fail("write", errors.New("connection closed for failback"))
	peerClose := &mediaLinkDiagnostic{s: s}
	peerClose.fail("read", errors.New("peer closed its connection"))
	cancel()
	shutdown := &mediaLinkDiagnostic{s: s}
	shutdown.fail("heartbeat", errors.New("service is stopping"))
	if err = events.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := events.Events(activity.Query{Audience: "diagnostic"})
	if err != nil || len(rows) != 1 || rows[0].Severity != "info" || rows[0].Details["stage"] != "read" {
		t.Fatalf("normal close/shutdown generated warning noise: %#v %v", rows, err)
	}
}
