package routing

import (
	"context"
	"testing"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestRFReservationBindsActualIdentityForEachSourceEpoch(t *testing.T) {
	var received dispatch.Frame
	r := newRouter(t, Options{Publish: func(_ Request, f dispatch.Frame) { received = f }})
	q := request("rf-all-start", 1001)
	q.SourceKind, q.Secure, q.EncryptionPending = "rf", true, true
	g, err := r.Begin(context.Background(), "site", q)
	if err != nil {
		t.Fatal(err)
	}
	f := frame(g)
	f.PCM = nil
	f.Encoded = make([]byte, 11)
	f.P25 = &dispatch.P25VoiceMetadata{Type: p25rtp.FR1}
	f.Encrypted = true
	f.ServiceOptions = 0x44
	f.AlgorithmID, f.KeyID, f.SourceEpoch = 129, 29, 1
	if err = r.Write(context.Background(), "site", g, f); err != nil {
		t.Fatal(err)
	}
	if received.AlgorithmID != 129 || received.KeyID != 29 {
		t.Fatal("actual RF key identity was changed")
	}
	f.KeyID = 30
	if err = r.Write(context.Background(), "site", g, f); err == nil {
		t.Fatal("key changed inside one source epoch")
	}
	f.SourceEpoch = 2
	if err = r.Write(context.Background(), "site", g, f); err == nil {
		t.Fatal("new epoch replaced an unfinished burst")
	}
	if err = r.EndSpurt(context.Background(), "site", g); err != nil {
		t.Fatal(err)
	}
	f.SourceEpoch = 1
	if err = r.Write(context.Background(), "site", g, f); err == nil {
		t.Fatal("ended source epoch reopened with another key")
	}
	f.SourceEpoch = 2
	if err = r.Write(context.Background(), "site", g, f); err != nil {
		t.Fatal("next burst could not bind its actual key", err)
	}
	f.SourceEpoch = 1
	f.KeyID = 29
	if err = r.Write(context.Background(), "site", g, f); err == nil {
		t.Fatal("old source epoch replayed after a later burst")
	}
}

func TestApplicationCannotReserveUnknownEncryptionIdentity(t *testing.T) {
	r := newRouter(t, Options{})
	q := request("application-pending", 1001)
	q.SourceKind, q.Secure, q.EncryptionPending = "application", true, true
	if _, err := r.Begin(context.Background(), "dispatch", q); err == nil {
		t.Fatal("application acquired pending RF exception")
	}
	q.SourceKind = "rf"
	q.AlgorithmID = 132
	if _, err := r.Begin(context.Background(), "site", q); err == nil {
		t.Fatal("pending RF reservation carried an invented partial key identity")
	}
}
