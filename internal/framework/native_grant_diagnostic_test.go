package framework

import (
	"context"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/siteaudio"
)

func TestNativeGrantDiagnosticIgnoresBlockedSourceSQLAndFullCaptureQueue(t *testing.T) {
	c := integrationNewCluster(t)
	site := c.site.service
	blockHistoryWriter(t, site)
	for i := 0; i < 1200; i++ {
		site.emit(activity.Event{Type: "saturated-source-observation"})
	}
	before := site.activity.IngestionHealth()
	if before.QueueDropped == 0 || before.QueueDepth != before.QueueCapacity {
		t.Fatal("fixture did not saturate blocked source", before)
	}
	// Also reserve the configuration writer. Diagnostics must use its existing
	// process-local ring rather than either database.
	writer, err := site.Store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	baseline, _ := site.Store.Events(100)
	started := time.Now().UTC()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, outcome := range []string{"granted", "failed"} {
			site.siteAudioObservation(siteaudio.Event{At: started.Add(12345 * time.Microsecond), Kind: "rf.grant", Outcome: outcome, RequestStartedAt: started, DurationMS: 12.345, CallID: "current-native-grant", AllocationID: "allocation-1", RadioID: 10002, TalkgroupID: 1001})
		}
		site.siteAudioObservation(siteaudio.Event{At: started, Kind: "rf.release", Outcome: "released", CallID: "current-native-grant"})
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("native grant observation waited for SQL or history capacity")
	}
	rows, err := site.Store.Events(100)
	if err != nil || len(rows) != len(baseline)+2 {
		t.Fatal("current native grant diagnostics unavailable or unrelated observation copied", rows, err)
	}
	want := []string{
		"Native voice grant failed after 12.345 ms.",
		"Native voice grant acknowledged in 12.345 ms.",
	}
	for i, event := range rows[:2] {
		if event.Kind != "rf.grant" || event.Message != want[i] || strings.Contains(event.Message, "airtime") {
			t.Fatal("native diagnostic lost result, measured duration or boundary", event)
		}
	}
	if after := site.activity.IngestionHealth(); after.QueueDropped < before.QueueDropped+3 {
		t.Fatal("canonical capture bypassed or waited for the full source queue", after)
	}
}
