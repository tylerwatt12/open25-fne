package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
	"open25.local/open25-fne/internal/routing"
	"open25.local/open25-fne/internal/rtpvoice"
	"open25.local/open25-fne/internal/sipvoice"
)

// This uses the actual source voiceCall actor, SIP/TCP SourceReady and UDP wire
// boundary. Remote allocation/authorization is a fixed peer, not native/RF or
// full service-admission qualification.
func TestIndependentVoiceFirstWordAgesWhileSourceReadyIsPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	localPair, err := rtpvoice.Listen(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	defer localPair.Close()
	remotePair, err := rtpvoice.Listen(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := rtpvoice.New(remotePair, rtpvoice.Options{Remote: localPair.LocalRTP(), LocalRole: p25rtp.GroupServing, RemoteRole: p25rtp.GroupHome})
	if err != nil {
		remotePair.Close()
		t.Fatal(err)
	}
	defer remote.Close()
	entered, permit := make(chan struct{}), make(chan struct{})
	start := func(id string, onSource func(context.Context, *sipvoice.Session, sipvoice.Source) error) (*sipvoice.Node, sipvoice.Peer) {
		t.Helper()
		n, err := sipvoice.New(sipvoice.Options{
			PeerID: id, BearerToken: "review-" + id, AdvertiseHost: "127.0.0.1",
			Authenticate: func(_ context.Context, auth sipvoice.AuthRequest) error {
				if auth.BearerToken != "review-"+auth.PeerID {
					return sipvoice.ErrUnauthorized
				}
				return nil
			},
			OnInvite: func(_ context.Context, session *sipvoice.Session) (sipvoice.Answer, error) {
				if session.Call().ID != "independent-source-ready-age" || session.Call().GroupID != 1001 {
					return sipvoice.Answer{}, sipvoice.ErrRejected
				}
				return sipvoice.Answer{Endpoints: sipvoice.Endpoints{RTP: remotePair.LocalRTP(), RTCP: remotePair.LocalRTCP()}, ResponseContext: json.RawMessage(`{"test":"source-age"}`), Release: func() {}}, nil
			},
			OnSource: onSource, OnRenew: func(context.Context, *sipvoice.Session) error { return nil },
			OnEnd: func(*sipvoice.Session, sipvoice.EndReason) {},
		})
		if err != nil {
			t.Fatal(err)
		}
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			n.Close()
			t.Fatal(err)
		}
		uri, err := sipvoice.PeerURI(id, listener.Addr().String())
		if err != nil {
			listener.Close()
			n.Close()
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- n.ServeTCP(listener) }()
		t.Cleanup(func() {
			_ = n.Close()
			select {
			case err := <-served:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(time.Second):
				t.Error("SIP fixture did not close")
			}
		})
		return n, sipvoice.Peer{ID: id, URI: uri}
	}
	caller, _ := start("source-age-home", nil)
	_, recipient := start("source-age-serving", func(ctx context.Context, _ *sipvoice.Session, source sipvoice.Source) error {
		if source.Epoch != 1 || source.TSN != 1 || source.MFID != 0 || source.Service != 4 {
			return sipvoice.ErrRejected
		}
		close(entered)
		select {
		case <-permit:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	var session *sipvoice.Session
	for {
		session, err = caller.Invite(ctx, recipient, sipvoice.Call{ID: "independent-source-ready-age", GroupID: 1001, Source: sipvoice.SourceRF, Authorization: json.RawMessage(`{"test":"source-age"}`)}, sipvoice.Endpoints{RTP: localPair.LocalRTP(), RTCP: localPair.LocalRTCP()})
		if !errors.Is(err, sipvoice.ErrClosed) {
			break
		}
		// Before the contact is published, ErrClosed sends no INVITE.
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{operationCtx: ctx}
	service.directory.value.Store(&directorySnapshot{System: SystemConfig{WACN: 1, SystemID: 1}})
	runtime := &voiceRuntime{s: service, calls: map[*voiceCall]bool{}}
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "independent-source-ready-age", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf"}
	call := runtime.newCall(recipient.ID, PeerCredential{Role: RoleSite}, q, dispatch.Grant{ID: q.ID, Talkgroups: q.Talkgroups}, false, true, p25rtp.GroupHome, localPair)
	call.sip = session
	if err := runtime.add(call); err != nil {
		t.Fatal(err)
	}
	go call.run()
	t.Cleanup(func() {
		call.cancel()
		select {
		case <-call.done:
		case <-time.After(time.Second):
			t.Error("source actor did not close")
		}
	})
	frame := sipTestFrame(q.ID, q.RadioID, 1001, 1, 0)
	if err := call.Write(frame); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first frame never reached pending SourceReady")
	}
	// A fresh frame's real SIP transaction consumes the pre-ready age budget.
	if remaining := time.Until(frame.CapturedAt.Add(300 * time.Millisecond)); remaining > 0 {
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(permit)
	read, stop := context.WithTimeout(ctx, 600*time.Millisecond)
	defer stop()
	for {
		packet, err := remote.Receive(read)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			break
		}
		if packet.Voice != nil {
			t.Fatalf("first source word crossed UDP after SourceReady consumed its 240ms budget; actual age %s", time.Since(frame.CapturedAt))
		}
	}
	select {
	case <-call.ctx.Done():
	default:
		t.Fatal("source-ready age overrun did not retire the allocation")
	}
}
