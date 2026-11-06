package framework

import (
	"context"
	"fmt"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
	"os"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sipRFCall(t *testing.T, c *mediaCluster, id string, secure, emergency bool, pending ...bool) (*voiceCall, routing.Request) {
	t.Helper()
	source := c.site.service
	_, generation := source.mediaClient.Link()
	endpoint, err := source.mediaClient.endpointAt(generation)
	if err != nil {
		t.Fatal(err)
	}
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: id, RadioID: 10001, Talkgroups: []int{1001}, Secure: secure, Emergency: emergency}, SourceKind: "rf", OriginSite: "site-1"}
	if secure {
		q.AlgorithmID, q.KeyID = 0x84, 7
		if len(pending) > 0 && pending[0] {
			q.EncryptionPending = true
			q.AlgorithmID, q.KeyID = 0, 0
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := source.voice.Load().begin(ctx, endpoint, q, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = call.End(ctx)
	})
	return call, q
}
func sipRFFrame(call *voiceCall, q routing.Request, epoch, ordinal uint64) dispatch.Frame {
	f := sipTestFrame(call.grant.ID, q.RadioID, 1001, epoch, ordinal)
	f.ServiceOptions = 0x18
	if q.Secure {
		f.ServiceOptions |= 0x40
	}
	if q.Emergency {
		f.ServiceOptions |= 0x80
	}
	f.Emergency = q.Emergency
	f.Encrypted = q.Secure
	if q.Secure {
		f.AlgorithmID, f.KeyID = 0x84, 7
		if f.P25.Header != nil {
			f.P25.Header.AlgorithmID, f.P25.Header.KeyID = 0x84, 7
		}
	}
	return f
}
func sipWriteBurst(t *testing.T, call *voiceCall, q routing.Request, epoch uint64, words int) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < words; i++ {
		f := sipRFFrame(call, q, epoch, uint64(i))
		if err := call.Write(f); err != nil {
			t.Fatalf("epoch%d word%d: %v", epoch, i, err)
		}
		if i+1 < words {
			<-ticker.C
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := call.EndSpurt(ctx); err != nil {
		call.mu.Lock()
		t.Logf("source stats %+v reason%s SIP%s", call.lastStats, call.lastReason, call.sip.Reason())
		call.mu.Unlock()
		t.Fatal("spurt release", err)
	}
}
func sipActualFrames(t *testing.T, c *mediaCluster, site string, count int) []dispatch.Frame {
	t.Helper()
	out := []dispatch.Frame{}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for len(out) < count {
		select {
		case f := <-c.codecs[site].frames:
			if !f.End {
				out = append(out, f)
			}
		case <-timer.C:
			indices := []uint8{}
			for _, f := range out {
				indices = append(indices, f.VoiceIndex)
			}
			t.Logf("received indices %v", indices)
			for _, node := range []*integrationService{c.site, c.zone, c.otherSite} {
				node.service.voice.Load().mu.Lock()
				calls := []*voiceCall{}
				for call := range node.service.voice.Load().calls {
					calls = append(calls, call)
				}
				node.service.voice.Load().mu.Unlock()
				for _, call := range calls {
					call.mu.Lock()
					stats := call.lastStats
					reason := call.lastReason
					if call.active != nil {
						stats = call.active.engine.Stats()
					}
					t.Logf("%s source%v call%s stats%+v reason%s", node.service.Config().Role, call.source, call.grant.ID, stats, reason)
					call.mu.Unlock()
				}
			}
			t.Fatalf("%s received%d/%d actual words", site, len(out), count)
		}
	}
	return out
}
func TestSIPRTPServiceRFReservationAndSuccessiveOpaqueBursts(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint("encrypted=", secure), func(t *testing.T) {
			c.affiliations(t, true)
			t.Cleanup(func() {
				if t.Failed() {
					for _, node := range []*integrationService{c.site, c.zone, c.otherSite} {
						_ = node.service.activity.Flush(context.Background())
						events, _ := node.service.activity.Events(activity.Query{Audience: "diagnostic"})
						for _, e := range events {
							t.Logf("%s diagnostic %s %+v", node.service.Config().Role, e.Message, e.Details)
						}
					}
				}
			})
			for _, node := range []*integrationService{c.site, c.otherSite, c.zone} {
				v := *node.service.view()
				v.System.Talkgroups = slices.Clone(v.System.Talkgroups)
				for i := range v.System.Talkgroups {
					if v.System.Talkgroups[i].TGID == 1001 {
						if secure {
							v.System.Talkgroups[i].AdmissionMode = "both"
							v.System.Talkgroups[i].TransmitMode = "selectable"
							v.System.Talkgroups[i].AlgorithmID = 129
							v.System.Talkgroups[i].KeyID = 29
						} else {
							v.System.Talkgroups[i].AdmissionMode = "clear"
							v.System.Talkgroups[i].TransmitMode = "clear"
							v.System.Talkgroups[i].AlgorithmID = 0
							v.System.Talkgroups[i].KeyID = 0
						}
					}
				}
				node.service.directory.value.Store(&v)
			}
			call, q := sipRFCall(t, c, fmt.Sprint("real-sip-udp-", secure), secure, secure, secure)
			time.Sleep(700 * time.Millisecond)
			if err := call.Renew(); err != nil {
				t.Fatal("reservation expired before RF voice", err)
			}
			call.mu.Lock()
			started := call.active != nil
			call.mu.Unlock()
			if started {
				t.Fatal("reservation synthesized source-ready media")
			}
			sipWriteBurst(t, call, q, 1, 18)
			first := sipActualFrames(t, c, "site-2", 18)
			for i, got := range first {
				want := sipRFFrame(call, q, 1, uint64(i))
				if !reflect.DeepEqual(got.Encoded, want.Encoded) || got.P25 == nil || got.P25.Type != want.P25.Type || got.P25.Quality != want.P25.Quality || !reflect.DeepEqual(got.P25.LSD, want.P25.LSD) || !reflect.DeepEqual(got.P25.ES, want.P25.ES) || got.ManufacturerID != want.ManufacturerID || got.ServiceOptions != want.ServiceOptions || got.Emergency != want.Emergency || got.Encrypted != want.Encrypted || got.AlgorithmID != want.AlgorithmID || got.KeyID != want.KeyID {
					t.Fatalf("source word%d changed: %+v", i, got)
				}
				if i == 0 && (!reflect.DeepEqual(got.P25.Header, want.P25.Header) || got.ManufacturerID == got.P25.Header.MFID) {
					t.Fatal("raw Header/MFID source distinction changed")
				}
			}
			time.Sleep(1100 * time.Millisecond)
			if err := call.Renew(); err != nil {
				t.Fatal("normal pause revoked allocation", err)
			}
			if len(c.zone.service.broker.router.Active()) != 1 {
				t.Fatal("spurt End released allocation")
			}
			sipWriteBurst(t, call, q, 2, 18)
			second := sipActualFrames(t, c, "site-2", 18)
			if first[0].CallID == second[0].CallID || first[0].SourceEpoch != 1 || second[0].SourceEpoch != 2 {
				t.Fatal("native media owner reused across spurts")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := call.End(ctx); err != nil {
				t.Fatal(err)
			}
			mediaWait(t, time.Second, func() bool {
				return len(c.zone.service.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
			}, "SIP allocation/native release")
		})
	}
}

func TestSIPRTPServiceReportAgeDoesNotWithdrawLiveAllocation(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	call, q := sipRFCall(t, c, "stale-management-live-call", false, false)
	v := *c.zone.service.view()
	v.Reports = slices.Clone(v.Reports)
	for i := range v.Reports {
		v.Reports[i].ReceivedAt = time.Now().Add(-time.Hour)
	}
	c.zone.service.directory.value.Store(&v)
	sipWriteBurst(t, call, q, 1, 18)
	frames := sipActualFrames(t, c, "site-2", 18)
	if len(frames) != 18 {
		t.Fatal("healthy allocation followed management age")
	}
	if len(c.zone.service.broker.router.Active()) != 1 {
		t.Fatal("management report age revoked live SIP allocation")
	}
	// The accepted source observation may age past its admission window while
	// the SIP lease and actual native recipients remain healthy.
	timer := time.NewTimer(time.Until(call.request.RFSource.ObservedAt.Add(4100 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-call.done:
		t.Fatal("source observation age revoked the live call")
	case <-timer.C:
	}
	if err := call.Renew(); err != nil {
		t.Fatal("healthy call stopped renewing", err)
	}
	sipWriteBurst(t, call, q, 2, 18)
	sipActualFrames(t, c, "site-2", 18)
}

func TestSIPRTPPendingRFIdentityPreservesHeaderlessCiphertext(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	for _, node := range []*integrationService{c.site, c.zone, c.otherSite} {
		v := *node.service.view()
		v.System.Talkgroups = slices.Clone(v.System.Talkgroups)
		for i := range v.System.Talkgroups {
			if v.System.Talkgroups[i].TGID == 1001 {
				v.System.Talkgroups[i].AdmissionMode = "both"
				v.System.Talkgroups[i].AlgorithmID, v.System.Talkgroups[i].KeyID = 129, 29
			}
		}
		node.service.directory.value.Store(&v)
	}
	call, q := sipRFCall(t, c, "headerless-pending-rf", true, false, true)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for i := 0; i < 9; i++ {
		f := sipRFFrame(call, q, 1, uint64(i))
		f.P25.Header = nil
		if err := call.Write(f); err != nil {
			t.Fatal(err)
		}
		<-tick.C
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := call.EndSpurt(ctx); err != nil {
		t.Fatal(err)
	}
	for _, f := range sipActualFrames(t, c, "site-2", 9) {
		if !f.Encrypted || f.AlgorithmID != 132 || f.KeyID != 7 || f.P25.Header != nil {
			t.Fatal("opaque headerless receive identity changed", f)
		}
	}
}

func TestSIPRTPServiceSourcePermissionAndRevocation(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	source := c.site.service
	_, generation := source.mediaClient.Link()
	endpoint, err := source.mediaClient.endpointAt(generation)
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"site-2", ""} {
		q := routing.Request{CallRequest: dispatch.CallRequest{ID: "forged-" + origin, RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: origin}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = source.voice.Load().begin(ctx, endpoint, q, false)
		cancel()
		if err == nil {
			t.Fatal("forged RF origin admitted")
		}
	}
	call, q := sipRFCall(t, c, "current-radio-policy", false, false)
	f := sipRFFrame(call, q, 1, 0)
	forged := f
	forged.SourceID++
	if err := call.Write(forged); err == nil {
		t.Fatal("media replaced admitted source")
	}
	v := *c.zone.service.view()
	v.System.Radios = slices.Clone(v.System.Radios)
	for i := range v.System.Radios {
		if v.System.Radios[i].RID == 10001 {
			v.System.Radios[i].Enabled = false
		}
	}
	c.zone.service.directory.value.Store(&v)
	select {
	case <-call.done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked radio retained SIP allocation")
	}
	if len(c.zone.service.broker.router.Active()) != 0 {
		t.Fatal("revocation left router floor")
	}
}
func TestGeneratedHeaderUsesSiteNACWithoutChangingObservedRF(t *testing.T) {
	s := &Service{config: FileConfig{Role: RoleSite, InstanceID: "site-1"}}
	s.directory.value.Store(&directorySnapshot{Zone: &ZoneConfig{Sites: []SiteIdentity{{ID: "site-1", NAC: 0x293}}}})
	f := sipTestFrame("generated", 10002, 1001, 1, 0)
	f.SourceKind = "application"
	f.P25.Header.NID = 0
	original := f.P25.Header
	if err := s.stampGeneratedVoiceHeader(&f); err != nil {
		t.Fatal(err)
	}
	if f.P25.Header.NID != 0x2930 || original.NID != 0 || f.P25.Header.MFID != original.MFID || f.P25.Header.MI != original.MI {
		t.Fatal("generated header stamping changed source fields or aliases")
	}
	observed := sipTestFrame("observed", 10001, 1001, 1, 0)
	observed.SourceKind = "rf"
	observed.P25.Header.NID = 0x1230
	if err := s.stampGeneratedVoiceHeader(&observed); err != nil || observed.P25.Header.NID != 0x1230 {
		t.Fatal("observed RF NID was inferred/overwritten")
	}
}

func TestSIPRTPServiceNativeCodecAndBrowserAudio(t *testing.T) {
	binary := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binary == "" {
		t.Skip("set reviewed native packet codec executable")
	}
	c := newMediaCluster(t)
	c.affiliations(t, true)
	codec, err := dispatch.NewPacketCodec(dispatch.PacketCodecOptions{Binary: binary, KeyFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = codec.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c.dispatch.service.crypto.mu.Lock()
	c.dispatch.service.crypto.codec = codec
	c.dispatch.service.crypto.mu.Unlock()
	t.Cleanup(func() { _ = codec.Close() })
	listener := newMediaBrowser(t, c.dispatch, c.users[1])
	source := newMediaBrowser(t, c.dispatch, c.users[0])
	if err = source.send(map[string]any{"type": "ptt_start", "request_id": "sip-native-browser", "talkgroups": []int{1001}}); err != nil {
		t.Fatal(err)
	}
	source.waitState(t, func(s dispatch.State) bool { return s.TX.State == "transmitting" }, "SIP/RF allocation")
	source.microphone(t, 31)
	if err = source.send(map[string]any{"type": "ptt_stop", "request_id": "sip-native-browser"}); err != nil {
		t.Fatal(err)
	}
	source.waitState(t, func(s dispatch.State) bool { return s.TX.State == "idle" }, "SIP/native tail release")
	received := []browserMediaAudio{}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for len(received) < 31 {
		select {
		case audio := <-listener.audio:
			if audio.stream == "tg-1001" {
				if len(audio.payload) != dispatch.FrameSamples {
					t.Fatal("invalid browser audio block")
				}
				if len(received) > 0 {
					last := received[len(received)-1]
					if audio.sequence != last.sequence+1 || audio.timestamp != last.timestamp+dispatch.FrameSamples {
						t.Fatal("browser RTP sequence/timestamp gap")
					}
				}
				received = append(received, audio)
			}
		case err := <-listener.errors:
			t.Fatal(err)
		case <-timer.C:
			t.Fatalf("browser received%d/31 native-decoded blocks", len(received))
		}
	}
	for _, site := range []string{"site-1", "site-2"} {
		frames := sipActualFrames(t, c, site, 36)
		if len(frames) != 36 {
			t.Fatal("native encoder tail was lost")
		}
		if frames[0].P25.Header == nil || frames[0].P25.Header.NID == 0 {
			t.Fatal("virtual console NID was not stamped for Site")
		}
	}
	mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) == 0 }, "all SIP allocations retired")
	t.Log("Actual Mac native IMBE encode/decode, SIP/TCP, UDP P25 RTP, two Site endpoints and browser Pion audio passed; RF/native Site boundary remains a fake.")
}

func TestRemoteByeDrainsAlreadyReceivedVoice(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	call, q := sipRFCall(t, c, "bye-drain", false, false)
	for i := 0; i < 9; i++ {
		if err := call.Write(sipRFFrame(call, q, 1, uint64(i))); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mediaWait(t, time.Second, func() bool {
		v := c.zone.service.voice.Load()
		v.mu.Lock()
		defer v.mu.Unlock()
		for incoming := range v.calls {
			incoming.mu.Lock()
			b := incoming.active
			incoming.mu.Unlock()
			if !incoming.source && b != nil && b.engine.Stats().ReceivedFrames == 9 {
				return true
			}
		}
		return false
	}, "nine admitted RTP words")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := call.sip.End(ctx); err != nil {
		t.Fatal(err)
	}
	if frames := sipActualFrames(t, c, "site-2", 9); len(frames) != 9 {
		t.Fatal("BYE discarded admitted voice")
	}
}

func nativeGrantCount(c *mediaCluster, site string) int {
	n := c.natives[site]
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, path := range n.paths {
		if path == "/p25/dispatch/grant" {
			count++
		}
	}
	return count
}
func TestSIPRTPServiceControllerRestartFallbackAndNoReplay(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	call, q := sipRFCall(t, c, "before-controller-restart", false, false)
	for i := 0; i < 3; i++ {
		if err := call.Write(sipRFFrame(call, q, 1, uint64(i))); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	oldGeneration := c.zone.service.voice.Load().endpoint.Generation
	before := nativeGrantCount(c, "site-2")
	c.zone.service.voice.Load().Close()
	select {
	case <-call.done:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("dead controller retained SIP owner")
	}
	mediaWait(t, 3*time.Second, func() bool { return c.dispatch.service.mediaClient.URL() == c.site.server.URL }, "authenticated local Site fallback")
	if status := c.dispatch.service.mediaClient.ConnectionStatus(); status.State != "Site Trunking" {
		t.Fatal("fallback not reported honestly", status)
	}
	if err := call.Write(sipRFFrame(call, q, 1, 3)); err == nil {
		t.Fatal("old call crossed controller loss")
	}
	if err := c.zone.service.openVoice(); err != nil {
		t.Fatal(err)
	}
	if c.zone.service.voice.Load().endpoint.Generation == oldGeneration {
		t.Fatal("restart reused generation")
	}
	mediaWait(t, 12*time.Second, func() bool {
		return c.dispatch.service.mediaClient.URL() == c.zone.server.URL && c.site.service.mediaClient.URL() == c.zone.server.URL
	}, "primary return between calls")
	if nativeGrantCount(c, "site-2") != before || len(c.zone.service.broker.router.Active()) != 0 {
		t.Fatal("restart/failback replayed old call")
	}
	c.affiliations(t, true)
	next, nq := sipRFCall(t, c, "after-controller-restart", false, false)
	sipWriteBurst(t, next, nq, 1, 9)
	if nativeGrantCount(c, "site-2") != before+1 {
		t.Fatal("fresh call did not obtain one new receiving allocation")
	}
}

func TestSIPRTPDurableRFSourceRetainsNewerOwnerAfterCallEndAndRestart(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	// The subscriber really moved to the second native Site table. Leave the
	// management reports unchanged to isolate the admitted SIP observation.
	runtime := c.radios["site-2"]
	runtime.mu.Lock()
	runtime.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	runtime.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	source := c.otherSite.service
	_, generation := source.mediaClient.Link()
	endpoint, err := source.mediaClient.endpointAt(generation)
	if err != nil {
		t.Fatal(err)
	}
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "new-roaming-owner", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-2"}
	call, err := source.voice.Load().begin(ctx, endpoint, q, false)
	if err != nil {
		t.Fatal("current native roam rejected", err)
	}
	if err := call.End(ctx); err != nil {
		t.Fatal(err)
	}
	mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) == 0 }, "roamed call release")
	zone := c.zone.service
	path := zone.rfAuthority.path
	zone.rfAuthority.close()
	zone.rfAuthority, err = openRFSourceAuthority(path, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	if owner, ok := zone.rfAuthority.lookup(10001); !ok || owner.SiteID != "site-2" {
		t.Fatal("new owner was not durable", owner, ok)
	}
	source = c.site.service
	_, generation = source.mediaClient.Link()
	endpoint, err = source.mediaClient.endpointAt(generation)
	if err != nil {
		t.Fatal(err)
	}
	q.ID, q.OriginSite = "older-owner-after-roamed-call", "site-1"
	if old, err := source.voice.Load().begin(ctx, endpoint, q, false); err == nil {
		_ = old.End(ctx)
		t.Fatal("ending the new owner's call revived the older Site observation")
	}
}

func TestSIPRTPDurableRFSourceRevocation(t *testing.T) {
	for _, kind := range []string{"group permission", "source Site removed", "source Site group policy", "inhibit", "roam", "null affiliation", "deregistration", "native restart", "native departure"} {
		t.Run(kind, func(t *testing.T) {
			c := newMediaCluster(t)
			c.affiliations(t, true)
			call, q := sipRFCall(t, c, "durable-source-revocation", false, false)
			sipWriteBurst(t, call, q, 1, 9)
			sipActualFrames(t, c, "site-2", 9)
			s := c.zone.service
			view := *s.view()
			observed := call.request
			update := rfAuthorityUpdate{SiteID: "site-1", NativeStartedAt: observed.RFSource.NativeStartedAt, ObservedAt: time.Now().UTC()}
			switch kind {
			case "group permission":
				view.System.Talkgroups = slices.Clone(view.System.Talkgroups)
				for i := range view.System.Talkgroups {
					if view.System.Talkgroups[i].TGID == 1001 {
						view.System.Talkgroups[i].TransmitAccess = "selected"
						view.System.Talkgroups[i].TransmitRadioIDs = []int{10003}
					}
				}
			case "source Site removed":
				zone := *view.Zone
				zone.Sites = slices.DeleteFunc(slices.Clone(zone.Sites), func(site SiteIdentity) bool { return site.ID == "site-1" })
				view.Zone = &zone
			case "source Site group policy":
				view.System.Talkgroups = slices.Clone(view.System.Talkgroups)
				for i := range view.System.Talkgroups {
					if view.System.Talkgroups[i].TGID == 1001 {
						view.System.Talkgroups[i].SiteAccess = "selected"
						view.System.Talkgroups[i].SiteIDs = []string{"site-2"}
					}
				}
			case "inhibit":
				s.inhibition.Store(inhibitionSnapshot(radioInhibitionPolicy{Desired: []int{10001}}, nil))
			case "roam":
				update.SiteID = "site-2"
				update.NativeStartedAt = c.radios["site-2"].SourceState().NativeStartedAt
				update.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
			case "null affiliation":
				update.NullAffiliations = []radio.Registration{{RID: 10001, At: time.Now().UTC()}}
			case "deregistration":
				update.Terminals = []radio.Registration{{RID: 10001, At: time.Now().UTC()}}
			case "native restart":
				update.NativeStartedAt = time.Now().UTC().Add(-time.Millisecond)
			case "native departure":
				update.Withdrawals = []rfAuthorityWithdrawal{{RadioID: 10001, GroupID: 1001, AffiliatedAt: observed.RFSource.AffiliatedAt, NativeStartedAt: observed.RFSource.NativeStartedAt}}
			}
			if kind != "group permission" && kind != "source Site removed" && kind != "source Site group policy" && kind != "inhibit" {
				update.ObservedAt = time.Now().UTC()
				if err := s.rfAuthority.apply(context.Background(), update, s.rfAuthorityScope()); err != nil {
					t.Fatal(err)
				}
			}
			s.directory.value.Store(&view)
			select {
			case <-call.done:
			case <-time.After(2 * time.Second):
				t.Fatal("revoked physical source retained call")
			}
			if call.Renew() == nil || len(s.broker.router.Active()) != 0 {
				t.Fatal("revocation retained lease or floor")
			}
		})
	}
}

func TestSIPRTPUnchangedSourceDoesNotWaitForUnrelatedAuthorityWrite(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	zone := c.zone.service
	view := *zone.view()
	view.System.Radios = slices.Clone(view.System.Radios)
	view.System.Radios = append(view.System.Radios, Radio{RID: 10005, Alias: "Independent source", Kind: "subscriber", Enabled: true})
	zone.directory.value.Store(&view)
	authority := zone.rfAuthority
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var writes atomic.Int32
	authority.write = func(path string, value any) error {
		writes.Add(1)
		once.Do(func() { close(entered) })
		<-release
		return writeRFAuthority(path, value)
	}
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	nativeRun := c.radios["site-2"].SourceState().NativeStartedAt
	observed := time.Now().UTC()
	update := rfAuthorityUpdate{SiteID: "site-2", NativeStartedAt: nativeRun, ObservedAt: observed, Registrations: []radio.Registration{{RID: 10005, At: observed}}}
	done := make(chan error, 1)
	go func() { done <- authority.apply(context.Background(), update, zone.rfAuthorityScope()) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("unrelated fact rejected before writer", err)
	case <-time.After(time.Second):
		t.Fatal("writer not blocked")
	}
	call, q := sipRFCall(t, c, "independent-durable-source-first", false, false)
	sipWriteBurst(t, call, q, 1, 9)
	sipActualFrames(t, c, "site-2", 9)
	if err := call.Renew(); err != nil {
		t.Fatal("live renewal waited on authority writer", err)
	}
	sipWriteBurst(t, call, q, 2, 9)
	sipActualFrames(t, c, "site-2", 9)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := call.End(ctx); err != nil {
		t.Fatal(err)
	}
	mediaWait(t, time.Second, func() bool {
		return len(zone.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
	}, "previous source release")
	call, q = sipRFCall(t, c, "independent-durable-source-second", false, false)
	sipWriteBurst(t, call, q, 1, 9)
	sipActualFrames(t, c, "site-2", 9)
	if writes.Load() != 1 {
		t.Fatal("unchanged calls or RTP attempted authority persistence", writes.Load())
	}
	select {
	case err := <-done:
		t.Fatal("blocked unrelated write completed unexpectedly", err)
	default:
	}
	unblock.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("released authority commit did not finish")
	}
}

func TestSIPRTPConflictingSourceIsDeniedAfterDurableFence(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	call, q := sipRFCall(t, c, "accepted-A-before-tie", false, false)
	sipWriteBurst(t, call, q, 1, 9)
	sipActualFrames(t, c, "site-2", 9)
	original := call.request.RFSource.AffiliatedAt
	runtime := c.radios["site-2"]
	runtime.mu.Lock()
	runtime.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: original}}
	runtime.mu.Unlock()
	source := c.otherSite.service
	_, generation := source.mediaClient.Link()
	endpoint, err := source.mediaClient.endpointAt(generation)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conflict := routing.Request{CallRequest: dispatch.CallRequest{ID: "valid-B-conflict", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-2"}
	if denied, e := source.voice.Load().begin(ctx, endpoint, conflict, false); e == nil {
		_ = denied.End(ctx)
		t.Fatal("conflicting call was granted")
	}
	zone := c.zone.service
	if owner, ok := zone.rfAuthority.lookup(10001); ok {
		t.Fatal("denied valid conflicting evidence left first owner authorized", owner)
	}
	fact := zone.rfAuthority.value.Load().document.Units[10001].Affiliation
	if !fact.Ambiguous || !fact.SiteAmbiguous || !fact.At.Equal(original) {
		t.Fatal("conflict did not retain original semantic clock", fact)
	}
	select {
	case <-call.done:
	case <-time.After(2 * time.Second):
		t.Fatal("first call survived accepted conflict")
	}
	path := zone.rfAuthority.path
	zone.rfAuthority.close()
	zone.rfAuthority, err = openRFSourceAuthority(path, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	if owner, ok := zone.rfAuthority.lookup(10001); ok {
		t.Fatal("reopen resurrected first owner", owner)
	}
	source = c.site.service
	_, generation = source.mediaClient.Link()
	endpoint, err = source.mediaClient.endpointAt(generation)
	if err != nil {
		t.Fatal(err)
	}
	q.ID = "fresh-A-observation-after-tie-reopen"
	if denied, e := source.voice.Load().begin(ctx, endpoint, q, false); e == nil {
		_ = denied.End(ctx)
		t.Fatal("fresh receipt time erased durable tie")
	}
}
