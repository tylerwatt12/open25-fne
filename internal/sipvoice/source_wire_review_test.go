package sipvoice

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestIndependentSourceWireRejectsMalformedAndForeignControls(t *testing.T) {
	a := startTestNode(t, "wire-source", nil)
	var calls atomic.Int32
	b := startTestNode(t, "wire-recipient", func(o *Options) {
		o.OnSource = func(context.Context, *Session, Source) error { calls.Add(1); return nil }
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("wire-source-validation"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	await(t, incoming.Ready())
	valid := `{"epoch":1,"tsn":1,"mfid":0,"service":4,"algorithm_id":128,"key_id":0}`
	tests := []struct {
		name, body string
		mutate     func(*sip.Request)
		code       int
	}{
		{"missing-field", `{"epoch":1,"tsn":1,"mfid":0}`, nil, 400},
		{"missing-algorithm", `{"epoch":1,"tsn":1,"mfid":0,"service":4,"key_id":0}`, nil, 400},
		{"missing-key", `{"epoch":1,"tsn":1,"mfid":0,"service":4,"algorithm_id":128}`, nil, 400},
		{"unknown-field", `{"epoch":1,"tsn":1,"mfid":0,"service":4,"extra":1}`, nil, 400},
		{"trailing-object", valid + `{}`, nil, 400}, {"broken-json", `{`, nil, 400},
		{"overflow", `{"epoch":1,"tsn":1,"mfid":256,"service":4}`, nil, 400},
		{"zero-epoch", strings.Replace(valid, `"epoch":1`, `"epoch":0`, 1), nil, 488},
		{"zero-tsn", strings.Replace(valid, `"tsn":1`, `"tsn":0`, 1), nil, 488},
		{"large-tsn", strings.Replace(valid, `"tsn":1`, `"tsn":128`, 1), nil, 488},
		{"unsupported-mfid", strings.Replace(valid, `"mfid":0`, `"mfid":2`, 1), nil, 488},
		{"oversized", strings.Repeat(" ", 129), nil, 415},
		{"body-type", valid, func(r *sip.Request) { r.ReplaceHeader(sip.NewHeader("Content-Type", "text/plain")) }, 415},
		{"disposition", valid, func(r *sip.Request) { r.RemoveHeader("Content-Disposition") }, 415},
		{"package", valid, func(r *sip.Request) { r.ReplaceHeader(sip.NewHeader("Info-Package", "unknown-package")) }, 469},
		{"foreign-peer", valid, func(r *sip.Request) { r.ReplaceHeader(sip.NewHeader("X-TrunkSystem-Peer", "other-peer")) }, 481},
		{"foreign-credential", valid, func(r *sip.Request) { r.ReplaceHeader(sip.NewHeader("Authorization", "Bearer invalid")) }, 481},
		{"wrong-tag", valid, func(r *sip.Request) { r.To().Params.Add("tag", "foreign-tag") }, 481},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := reviewInfo(t, s, sourcePackage, tc.body)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			if got := reviewInfoResponse(t, s, r).StatusCode; got != tc.code {
				t.Fatalf("status%d want%d", got, tc.code)
			}
			if calls.Load() != 0 {
				t.Fatal("rejected control reached source callback")
			}
		})
	}
	first := reviewInfo(t, s, sourcePackage, valid)
	if got := reviewInfoResponse(t, s, first).StatusCode; got != 200 {
		t.Fatal("valid after rejects", got)
	}
	if got := reviewInfoResponse(t, s, first.Clone()).StatusCode; got != 200 {
		t.Fatal("exact replay", got)
	}
	changed := first.Clone()
	changed.SetBody([]byte(strings.Replace(valid, `"service":4`, `"service":5`, 1)))
	if got := reviewInfoResponse(t, s, changed).StatusCode; got != 481 {
		t.Fatal("same CSeq rebound source", got)
	}
	repeat := reviewInfo(t, s, sourcePackage, valid)
	if got := reviewInfoResponse(t, s, repeat).StatusCode; got != 200 {
		t.Fatal("semantic repeat", got)
	}
	next := repeat.Clone()
	next.SetBody([]byte(`{"epoch":2,"tsn":3,"mfid":0,"service":4,"algorithm_id":128,"key_id":0}`))
	if got := reviewInfoResponse(t, s, next).StatusCode; got != 481 {
		t.Fatal("fresh repeated source did not consume CSeq", got)
	}
	if calls.Load() != 1 {
		t.Fatal("repeat invoked source callback", calls.Load())
	}
	if err = s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, incoming.Done())
	if got := reviewInfoResponse(t, s, first.Clone()).StatusCode; got != 481 {
		t.Fatal("closed source accepted", got)
	}
}
func TestIndependentRenewWireRejectsForeignBodyAndRole(t *testing.T) {
	a := startTestNode(t, "renew-wire-source", nil)
	var calls atomic.Int32
	b := startTestNode(t, "renew-wire-recipient", func(o *Options) { o.OnRenew = func(context.Context, *Session) error { calls.Add(1); return nil } })
	s, err := a.n.Invite(t.Context(), b.peer, testCall("renew-wire"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	await(t, incoming.Ready())
	for _, tc := range []struct {
		name   string
		mutate func(*sip.Request)
		code   int
	}{
		{"body", func(r *sip.Request) { r.SetBody([]byte(`{}`)) }, 415},
		{"type", func(r *sip.Request) { r.AppendHeader(sip.NewHeader("Content-Type", "application/json")) }, 415},
		{"disposition", func(r *sip.Request) { r.RemoveHeader("Content-Disposition") }, 415},
		{"peer", func(r *sip.Request) { r.ReplaceHeader(sip.NewHeader("X-TrunkSystem-Peer", "other")) }, 481},
		{"credential", func(r *sip.Request) { r.ReplaceHeader(sip.NewHeader("Authorization", "Bearer invalid")) }, 481},
		{"stale", func(r *sip.Request) { r.CSeq().SeqNo = s.request.CSeq().SeqNo }, 481},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reviewInfo(t, s, renewPackage, "")
			tc.mutate(r)
			if got := reviewInfoResponse(t, s, r).StatusCode; got != tc.code {
				t.Fatalf("status%d want%d", got, tc.code)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid renew reached callback")
	}
	if err = s.Renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("valid renew missing")
	}
	reverse := reviewInfo(t, incoming, renewPackage, "")
	if got := reviewInfoResponse(t, incoming, reverse).StatusCode; got != 481 {
		t.Fatal("callee sent caller-only renewal", got)
	}
	if err = s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, incoming.Done())
}

func TestIndependentColonEnrolledIDsUseCanonicalSIPUser(t *testing.T) {
	var raw sip.Uri
	if err := sip.ParseUri("sip:site:site-1@127.0.0.1:5060;transport=tcp", &raw); err != nil {
		t.Fatal(err)
	}
	if raw.User != "site" || raw.Password != "site-1" {
		t.Fatalf("upstream raw-colon semantics changed: %#v", raw)
	}
	for _, id := range []string{"zone:zone-1", "site:site-1", "dispatch:dispatch-1", "literal%3A:ID@?"} {
		uri, err := PeerURI(id, "127.0.0.1:5060")
		if err != nil {
			t.Fatal(err)
		}
		var parsed sip.Uri
		if err = sip.ParseUri(uri, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.User != peerUser(id) || parsed.Password != "" || !tcpURI(parsed) {
			t.Fatalf("identity not preserved in user field: %s %#v", uri, parsed)
		}
	}
	if uri, _ := PeerURI("site:site-1", "127.0.0.1:5060"); uri != "sip:site%3Asite-1@127.0.0.1:5060;transport=tcp" {
		t.Fatal("noncanonical URI", uri)
	}
	for _, address := range []string{"127.0.0.1:0", "255.255.255.255:5060", "0.0.0.0:5060", "[::1]:5060", "host.test:5060"} {
		if _, err := PeerURI("site:site-1", address); err == nil {
			t.Fatal("invalid endpoint accepted", address)
		}
	}
	a := startTestNode(t, "site:site-1", nil)
	b := startTestNode(t, "zone:zone-1", func(o *Options) {
		o.OnRenew = func(context.Context, *Session) error { return nil }
		o.OnSource = func(context.Context, *Session, Source) error { return nil }
	})
	var err error
	a.peer.URI, err = PeerURI(a.peer.ID, strings.TrimSuffix(strings.TrimPrefix(a.peer.URI, "sip:"+a.peer.ID+"@"), ";transport=tcp"))
	if err != nil {
		t.Fatal(err)
	}
	b.peer.URI, err = PeerURI(b.peer.ID, strings.TrimSuffix(strings.TrimPrefix(b.peer.URI, "sip:"+b.peer.ID+"@"), ";transport=tcp"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.n.Invite(t.Context(), b.peer, testCall("colon-enrollment"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	if s.PeerID() != b.peer.ID || incoming.PeerID() != a.peer.ID {
		t.Fatal("credential/header identity was encoded or renamed")
	}
	if err = s.Renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.SourceReady(t.Context(), Source{Epoch: 1, TSN: 1, MFID: 1, Service: 0x44}); err != nil {
		t.Fatal(err)
	}
	if err = s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, incoming.Done())
}
