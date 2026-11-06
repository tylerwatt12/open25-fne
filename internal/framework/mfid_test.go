package framework

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
	"testing"
	"time"
)

func TestDispatchSnapshotsVoiceMFIDWithoutRewritingRFVoice(t *testing.T) {
	c := newMediaCluster(t)
	requireMediaPacketCodec(t, c)
	c.affiliations(t, true)
	setVoiceMFID := func(mfid int) {
		t.Helper()
		var system SystemConfig
		if err := c.system.service.owned(&system); err != nil {
			t.Fatal(err)
		}
		system.VoiceMFID = mfid
		if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
			t.Fatal(err)
		}
		// Replace the routing helper's abbreviated reports with real scoped
		// reports before the Zone submits its full central report.
		integrationSync(t, c.site)
		integrationSync(t, c.otherSite)
		integrationSyncAll(t, c.integrationCluster)
	}
	setVoiceMFID(1)
	router := &networkRouter{service: c.dispatch.service}
	user := c.users[0]
	principal := dispatch.Principal{UserID: user.ID, RadioID: 10002, CanTransmit: true, TransmitGroups: []int{1001}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := router.ClaimAudio(ctx, principal, "mfid-session"); err != nil {
		t.Fatal(err)
	}
	defer router.ReleaseAudio(context.Background(), principal, "mfid-session")
	grant, err := router.Begin(ctx, dispatch.CallRequest{ID: "mfid-admitted-one", SessionID: "mfid-session", UserID: user.ID, RadioID: 10002, Talkgroups: []int{1001}})
	if err != nil {
		t.Fatal(err)
	}
	if router.route(grant).VoiceMFID != 1 {
		t.Fatal("call did not snapshot its configured voice MFID")
	}
	setVoiceMFID(0)
	frame := dispatch.Frame{CallID: grant.ID, TalkgroupID: 1001, SourceID: 10002, Sequence: 1, PCM: make([]byte, dispatch.FrameBytes), CapturedAt: time.Now(), ManufacturerID: 0}
	if err = router.Write(ctx, grant, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-c.codecs["site-1"].frames:
		if received.ManufacturerID != 1 {
			t.Fatal("admitted voice MFID changed mid-call")
		}
	case <-time.After(time.Second):
		t.Fatal("voice frame not routed")
	}
	if err = router.End(ctx, grant); err != nil {
		t.Fatal(err)
	}
	mediaWait(t, 2*time.Second, func() bool {
		return len(c.zone.service.broker.router.Active()) == 0 && len(c.site.service.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
	}, "console allocation and native tails released")
	// An actual RF-originated MFID is retained, irrespective of the System's
	// setting for newly generated console voice.
	source := c.site.service
	rf := sipTestFrame("rf-mfid-one-use", 10001, 1001, 1, 0)
	rf.ManufacturerID = 1
	source.codecFrame(rf)
	mediaWait(t, 3*time.Second, func() bool {
		source.broker.mu.Lock()
		defer source.broker.mu.Unlock()
		stream := source.broker.rfCalls[rf.CallID]
		return stream != nil && stream.ready && stream.remote.ID != ""
	}, "RF MFID routing admission")
	rf = sipTestFrame(rf.CallID, 10001, 1001, 1, 1)
	rf.ManufacturerID = 1
	source.codecFrame(rf)
	// Site 2 may still contain the preceding console frame; use its source ID
	// to select the RF observation rather than manufacture a matching result.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case received := <-c.codecs["site-2"].frames:
			if received.SourceID == 10001 {
				if received.ManufacturerID != 1 {
					t.Fatal("RF MFID was rewritten to System voice MFID")
				}
				rf.End = true
				rf.Encoded, rf.P25 = nil, nil
				source.codecFrame(rf)
				return
			}
		case <-deadline.C:
			t.Fatal("RF source frame not routed")
		}
	}
}
