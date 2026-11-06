package framework

import (
	"context"
	"slices"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
)

// The HTTP, service credentials, media sockets, source authorization and
// history forwarding are real. Only physical radio/codec endpoints are fixtures.
func TestAuthenticatedWideAreaCallKeepsOneHistoryIdentityAcrossSiteRejoin(t *testing.T) {
	c := newMediaCluster(t)
	requireMediaPacketCodec(t, c)
	c.affiliations(t, true)
	router := &networkRouter{service: c.dispatch.service}
	p := dispatch.Principal{UserID: c.users[0].ID, RadioID: 10002, ReceiveGroups: []int{1001}, TransmitGroups: []int{1001}, CanTransmit: true}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := router.ClaimAudio(ctx, p, "history-source"); err != nil {
		t.Fatal(err)
	}
	defer router.ReleaseAudio(context.Background(), p, "history-source")
	grant, err := router.Begin(ctx, dispatch.CallRequest{ID: "one-wide-area-conversation", LogicalCallID: "forged-other-conversation", SessionID: "history-source", UserID: p.UserID, RadioID: p.RadioID, Talkgroups: []int{1001}})
	if err != nil {
		t.Fatal(err)
	}
	renewCtx, stopRenew := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	renewError := make(chan error, 1)
	go func() {
		defer close(renewDone)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-tick.C:
				if err := router.Renew(renewCtx, grant); err != nil {
					if renewCtx.Err() == nil {
						renewError <- err
					}
					return
				}
			}
		}
	}()
	defer func() { stopRenew(); <-renewDone }()
	defer router.End(context.Background(), grant)

	allocation := func(node *integrationService) string {
		active := node.service.broker.router.Active()
		if len(active) != 1 {
			t.Fatalf("expected one receiving allocation at %s: %#v", node.service.Config().InstanceID, active)
		}
		if active[0].LogicalCallID != grant.ID || active[0].ID == grant.ID {
			t.Fatalf("conversation identity replaced the one-use allocation: %#v", active[0])
		}
		return active[0].ID
	}
	firstSite := allocation(c.site)
	oldAllocation := allocation(c.otherSite)
	if firstSite == oldAllocation {
		t.Fatal("receiving Sites reused an allocation identity")
	}
	writeCtx, stopWrite := context.WithCancel(ctx)
	writeDone := make(chan struct{})
	writeError := make(chan error, 1)
	go func() {
		defer close(writeDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for sequence := uint64(1); ; sequence++ {
			if err := router.Write(writeCtx, grant, dispatch.Frame{CallID: grant.ID, SourceID: p.RadioID, Sequence: sequence, PCM: make([]byte, dispatch.FrameBytes), CapturedAt: time.Now()}); err != nil {
				if writeCtx.Err() == nil {
					writeError <- err
				}
				return
			}
			select {
			case <-writeCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { stopWrite(); <-writeDone }()
	mediaWait(t, time.Second, func() bool { return len(c.codecs["site-1"].frames) > 0 && len(c.codecs["site-2"].frames) > 0 }, "both native delivery endpoints")
	// Withdraw Site 2's RF interest, while Site 1 and the source call continue.
	c.radios["site-2"].mu.Lock()
	saved := slices.Clone(c.radios["site-2"].status.Affiliations)
	c.radios["site-2"].status.Affiliations = nil
	c.radios["site-2"].mu.Unlock()
	run := c.radios["site-2"].SourceState().NativeStartedAt
	historyPublishInterest(t, c, rfAuthorityWithdrawal{RadioID: saved[0].RID, GroupID: saved[0].TGID, AffiliatedAt: saved[0].At, NativeStartedAt: run})
	mediaWait(t, 2*time.Second, func() bool { return len(c.otherSite.service.broker.router.Active()) == 0 }, "Site leaves the conversation")
	previous := saved[0].At
	saved[0].At = time.Now().UTC()
	if !saved[0].At.After(previous) {
		t.Fatal("rejoining radio must supply a newer affiliation")
	}
	c.radios["site-2"].mu.Lock()
	c.radios["site-2"].status.Affiliations = saved
	c.radios["site-2"].mu.Unlock()
	historyPublishInterest(t, c)
	mediaWait(t, 2*time.Second, func() bool { return len(c.otherSite.service.broker.router.Active()) == 1 }, "Site rejoins the same conversation")
	if again := allocation(c.otherSite); again == oldAllocation || again == firstSite {
		t.Fatal("rejoining Site reused an ended allocation identity")
	}
	stopWrite()
	<-writeDone
	select {
	case err := <-writeError:
		t.Fatal("source media failed", err)
	default:
	}
	stopRenew()
	<-renewDone
	if err = router.End(ctx, grant); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-renewError:
		t.Fatal("source renewal failed", err)
	default:
	}
	mediaWait(t, 2*time.Second, func() bool {
		return len(c.zone.service.broker.router.Active()) == 0 && len(c.site.service.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
	}, "remote SIP release and native tail completion")
	historyAssertOneCentralCall(t, c, grant.ID, "application")
}

func historyPublishInterest(t *testing.T, c *mediaCluster, withdrawals ...rfAuthorityWithdrawal) {
	t.Helper()
	reports := []SiteReport{}
	for _, node := range []*integrationService{c.site, c.otherSite} {
		runtime := c.radios[node.service.Config().InstanceID]
		runtime.mu.Lock()
		status := runtime.status
		status.Affiliations = slices.Clone(status.Affiliations)
		runtime.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		update := rfUpdateFromSource(runtime.SourceState())
		if node == c.otherSite {
			update.Withdrawals = withdrawals
		}
		err := c.zone.service.rfAuthority.apply(ctx, update, c.zone.service.rfAuthorityScope())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		reports = append(reports, SiteReport{ID: node.service.Config().InstanceID, ReceivedAt: time.Now(), Radio: &status})
	}
	if err := c.zone.service.writeCache("site_reports", reports); err != nil {
		t.Fatal(err)
	}
	c.zone.service.refreshDirectory()
}

func historyAssertOneCentralCall(t *testing.T, c *mediaCluster, id, kind string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for _, node := range []*integrationService{c.site, c.otherSite, c.dispatch} {
		if err := node.service.activity.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		node.service.uploadActivity(ctx)
	}
	if err := c.zone.service.activity.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	c.zone.service.uploadActivity(ctx)
	if err := c.system.service.activity.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := c.system.service.activity.CallHistory(activity.Query{})
	if err != nil || len(page.Calls) != 1 || page.Calls[0].ID != id || page.Calls[0].SourceKind != kind || page.Calls[0].EndedAt == nil {
		t.Fatalf("a wide-area conversation became extra central calls: %#v %v", page, err)
	}
	sites := map[string]bool{}
	for _, d := range page.Calls[0].Destinations {
		if d.SiteID != "" {
			sites[d.SiteID] = true
		}
		if d.EndedAt == nil {
			t.Fatal("an ended receiving allocation left its destination active")
		}
	}
	if len(sites) != 2 || !sites["site-1"] || !sites["site-2"] {
		t.Fatalf("central history lost actual Site destinations: %#v", page)
	}
	stats, err := c.system.service.activity.Statistics(activity.Query{})
	if err != nil || stats.Calls != 1 || stats.SiteDeliveries != 2 || stats.CompletedCalls != 1 {
		t.Fatalf("rejoin inflated conversation or Site counts: %#v %v", stats, err)
	}
}

func TestRFSourceAndReceivingSiteKeepTheSameConversationIdentity(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	source := c.site.service
	frame := sipTestFrame("one-rf-conversation", 10001, 1001, 1, 0)
	source.codecFrame(frame)
	mediaWait(t, 3*time.Second, func() bool {
		source.broker.mu.Lock()
		defer source.broker.mu.Unlock()
		stream := source.broker.rfCalls[frame.CallID]
		return stream != nil && stream.ready && stream.remote.ID != ""
	}, "authenticated RF source forwarding")
	active := c.otherSite.service.broker.router.Active()
	if len(active) != 1 || active[0].ID == frame.CallID || active[0].LogicalCallID != frame.CallID || active[0].SourceKind != "rf" {
		t.Fatalf("receiving Site lost RF conversation lineage: %#v", active)
	}
	frame = sipTestFrame(frame.CallID, 10001, 1001, 1, 1)
	source.codecFrame(frame)
	mediaWait(t, time.Second, func() bool { return len(c.codecs["site-2"].frames) > 0 }, "RF source delivered to other Site")
	frame.End, frame.Encoded, frame.P25 = true, nil, nil
	source.codecFrame(frame)
	mediaWait(t, 2*time.Second, func() bool {
		return len(source.broker.router.Active()) == 0 && len(c.zone.service.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
	}, "RF conversation release")
	historyAssertOneCentralCall(t, c, frame.CallID, "rf")
}
