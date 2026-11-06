package framework

import (
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
	"open25.local/open25-fne/internal/routing"
	"open25.local/open25-fne/internal/sipvoice"
	"testing"
	"time"
)

func TestReceiveEnvelopeRejectsNonRFOrInventedPendingIdentity(t *testing.T) {
	user := fixtureUser()
	s := &Service{config: FileConfig{Role: RoleApplication}, hub: &dispatch.Hub{}, mediaClient: &mediaClient{activeURL: "http://zone", endpoint: voiceEndpoint{PeerID: "zone"}}, audioLeases: map[int]audioIdentityLease{10002: {UserID: "user:" + user.ID, Expires: time.Now().Add(time.Minute)}}}
	s.directory.value.Store(&directorySnapshot{System: userFixtureSystem(), Users: map[string]userRecord{user.ID: {User: user}}})
	v := &voiceRuntime{s: s}
	e := voiceEnvelope{Request: routing.Request{CallRequest: dispatch.CallRequest{ID: "receive-pending", RadioID: 10001, Talkgroups: []int{1001}, Secure: true, EncryptionPending: true}, SourceKind: "rf"}, Receive: true, Origin: p25rtp.GroupHome}
	c := sipvoice.Call{ID: "receive-pending", GroupID: 1001, Source: sipvoice.SourceRF}
	peer := PeerCredential{Role: RoleZone, InstanceID: "zone-1"}
	if err := v.validate("zone", peer, c, e); err != nil {
		t.Fatal("valid RF receive reservation rejected", err)
	}
	for _, test := range []struct {
		name   string
		change func(*voiceEnvelope, *sipvoice.Call)
	}{
		{"application", func(e *voiceEnvelope, c *sipvoice.Call) {
			e.Request.SourceKind = "application"
			c.Source = sipvoice.SourceConsole
		}},
		{"clear", func(e *voiceEnvelope, _ *sipvoice.Call) { e.Request.Secure = false }},
		{"algorithm", func(e *voiceEnvelope, _ *sipvoice.Call) { e.Request.AlgorithmID = 132 }},
		{"key", func(e *voiceEnvelope, _ *sipvoice.Call) { e.Request.KeyID = 7 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad, call := e, c
			test.change(&bad, &call)
			if err := v.validate("zone", peer, call, bad); err == nil {
				t.Fatal("malformed receive-only pending identity admitted")
			}
		})
	}
}

func TestVoiceSourceBindsActualPendingIdentityAndFencesFixedApplications(t *testing.T) {
	q := routing.Request{CallRequest: dispatch.CallRequest{Secure: true, EncryptionPending: true}, SourceKind: "rf"}
	c := &voiceCall{request: q, origin: p25rtp.GroupHome}
	source := sipvoice.Source{Epoch: 1, TSN: 1, Service: 0x40, AlgorithmID: 129, KeyID: 29}
	if !c.sourceValid(source) {
		t.Fatal("actual RF identity was rejected")
	}
	source.AlgorithmID = 0
	if c.sourceValid(source) {
		t.Fatal("actual source still had an unknown algorithm")
	}
	source.AlgorithmID = 129
	c.request.EncryptionPending = false
	c.request.SourceKind = "application"
	c.request.AlgorithmID, c.request.KeyID = 132, 7
	if c.sourceValid(source) {
		t.Fatal("application burst replaced its fixed identity")
	}
}
